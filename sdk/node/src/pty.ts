// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

/**
 * Pseudo-terminal (PTY) interface for CubeSandbox.
 *
 * Ports the Python SDK's ``sandbox.pty`` API (``cubesandbox._pty``) to Node.
 * It speaks envd's Connect-JSON RPC directly: streaming calls (``Start`` /
 * ``Connect``) use the framed ``application/connect+json`` wire format shared
 * with {@link Commands}, while selector calls (``SendSignal`` / ``SendInput`` /
 * ``Update``) use plain unary ``application/json`` POSTs. protobuf ``bytes``
 * fields are base64 strings on the JSON side.
 */

import { fetch } from "undici";

import {
  CONNECT_COMPRESSED_FLAG,
  CONNECT_CONTENT_TYPE,
  CONNECT_END_STREAM_FLAG,
  CONNECT_PROTOCOL_VERSION,
  DEFAULT_ENVD_USER,
  ENVD_PORT,
  encodeConnectEnvelope,
  exitCodeFromStatus,
  raiseConnectEndStream,
  readConnectFrames,
  userHeaders,
} from "./commands.js";
import { ApiError, CubeSandboxError } from "./exceptions.js";
import type { Sandbox } from "./sandbox.js";

/** Raw bytes streamed from the PTY master side. */
export type PtyOutput = Uint8Array;

/** Pseudo-terminal size. */
export interface PtySize {
  /** Number of rows. */
  rows: number;
  /** Number of columns. */
  cols: number;
}

/** Options for {@link Pty.create}. */
export interface PtyCreateOptions {
  /** Sandbox user for envd process auth. Defaults to ``"root"``. */
  user?: string;
  /** Working directory for the shell. */
  cwd?: string;
  /**
   * Extra environment variables. ``TERM``/``LANG``/``LC_ALL`` are seeded with
   * sensible defaults unless overridden here.
   */
  envs?: Record<string, string>;
  /**
   * Timeout in milliseconds, applied two ways to match the Python SDK:
   *  - sent to envd as ``Connect-Timeout-Ms`` (a hard wall-clock deadline), and
   *  - a client-side idle abort that resets on every chunk received.
   *
   * Default: 60000. Pass ``0`` to disable both.
   */
  timeoutMs?: number;
}

/** Options for {@link Pty.connect}. */
export interface PtyConnectOptions {
  /** See {@link PtyCreateOptions.timeoutMs}. Default: 60000. */
  timeoutMs?: number;
}

const SIGNAL_SIGKILL = "SIGNAL_SIGKILL";
const DEFAULT_PTY_TIMEOUT_MS = 60000;

/** Drives the client-side idle timeout and manual disconnect for a stream. */
interface StreamControl {
  /** Reset the idle timer (call on every received frame). */
  reset(): void;
  /** Cancel the idle timer without aborting the request. */
  clearIdle(): void;
  /** Abort the request; marks the stream as intentionally disconnected. */
  disconnect(): void;
  readonly signal: AbortSignal;
  readonly idleFired: boolean;
  readonly disconnected: boolean;
  readonly timeoutMs: number;
}

function createStreamControl(timeoutMs: number): StreamControl {
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  let idleFired = false;
  let disconnected = false;

  const arm = (): void => {
    if (timeoutMs <= 0) {
      return;
    }
    timer = setTimeout(() => {
      idleFired = true;
      controller.abort();
    }, timeoutMs);
    // Don't let a pending idle timer keep the event loop alive on its own,
    // matching ``createIdleTimeout`` in stream.ts.
    (timer as { unref?: () => void }).unref?.();
  };
  const clearTimer = (): void => {
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
  };

  arm();

  return {
    reset(): void {
      clearTimer();
      arm();
    },
    clearIdle(): void {
      clearTimer();
    },
    disconnect(): void {
      disconnected = true;
      clearTimer();
      if (!controller.signal.aborted) {
        controller.abort();
      }
    },
    signal: controller.signal,
    get idleFired() {
      return idleFired;
    },
    get disconnected() {
      return disconnected;
    },
    timeoutMs,
  };
}

/**
 * Handle to a running PTY.
 *
 * The handle is an async iterable that yields {@link PtyOutput} chunks until
 * the PTY process exits or {@link PtyHandle.disconnect} is called. Use
 * {@link PtyHandle.wait} to drain the stream with an optional callback, or read
 * chunks directly via ``for await`` when integrating with a UI / event loop.
 */
export class PtyHandle {
  private readonly _pid: number;
  private readonly _events: AsyncGenerator<Record<string, any>>;
  private readonly _control: StreamControl;
  private readonly _handleKill: () => Promise<boolean>;
  private readonly _handleSendStdin: (
    data: string | Uint8Array,
    requestTimeoutMs?: number,
  ) => Promise<void>;
  private readonly _handleResize: (
    size: PtySize,
    requestTimeoutMs?: number,
  ) => Promise<void>;

  private _exitCode: number | null = null;
  private _error: string | null = null;
  private _exited = false;

  constructor(params: {
    pid: number;
    events: AsyncGenerator<Record<string, any>>;
    control: StreamControl;
    handleKill: () => Promise<boolean>;
    handleSendStdin: (data: string | Uint8Array, requestTimeoutMs?: number) => Promise<void>;
    handleResize: (size: PtySize, requestTimeoutMs?: number) => Promise<void>;
  }) {
    this._pid = params.pid;
    this._events = params.events;
    this._control = params.control;
    this._handleKill = params.handleKill;
    this._handleSendStdin = params.handleSendStdin;
    this._handleResize = params.handleResize;
  }

  /** PTY process ID. */
  get pid(): number {
    return this._pid;
  }

  /** Exit code once the PTY process has finished, otherwise ``null``. */
  get exitCode(): number | null {
    return this._exitCode;
  }

  /** Error message reported by envd for the PTY, if any. */
  get error(): string | null {
    return this._error;
  }

  async *[Symbol.asyncIterator](): AsyncGenerator<PtyOutput> {
    try {
      for await (const event of this._events) {
        const data = event.data ?? {};
        if (data.pty) {
          yield Buffer.from(data.pty, "base64");
        }
        const end = event.end;
        if (end !== undefined && end !== null) {
          this._exitCode = extractExitCode(end);
          this._error = end.error || null;
          this._exited = true;
        }
      }
    } catch (err) {
      if (this._control.disconnected) {
        return;
      }
      if (this._control.idleFired) {
        throw new CubeSandboxError(
          `PTY stream timed out after ${this._control.timeoutMs}ms of inactivity`,
        );
      }
      throw err;
    } finally {
      this._control.clearIdle();
      try {
        await this._events.return(undefined);
      } catch {
        // best-effort cleanup
      }
    }
  }

  /**
   * Block until the PTY exits and return its exit code.
   *
   * @param onData Callback invoked with each PTY output chunk.
   * @throws {CubeSandboxError} If the stream ends without an end event or envd
   *   reports an error.
   */
  async wait(onData?: (chunk: PtyOutput) => void): Promise<number> {
    for await (const chunk of this) {
      onData?.(chunk);
    }
    if (!this._exited) {
      throw new CubeSandboxError("PTY stream ended without an end event");
    }
    if (this._error) {
      throw new CubeSandboxError(`PTY exited with error: ${this._error}`);
    }
    return this._exitCode ?? 0;
  }

  /**
   * Stop receiving events without killing the PTY. The process keeps running
   * inside the sandbox; reattach later via {@link Pty.connect}.
   */
  disconnect(): void {
    this._control.disconnect();
    void this._events.return(undefined).catch(() => {});
  }

  /**
   * Send ``SIGKILL`` to the PTY process.
   *
   * @returns ``true`` if killed, ``false`` if it could not be found.
   */
  kill(): Promise<boolean> {
    return this._handleKill();
  }

  /** Send input (bytes or a UTF-8 string) to the PTY master side. */
  sendStdin(data: string | Uint8Array, requestTimeoutMs?: number): Promise<void> {
    return this._handleSendStdin(data, requestTimeoutMs);
  }

  /** Resize the PTY window. */
  resize(size: PtySize, requestTimeoutMs?: number): Promise<void> {
    return this._handleResize(size, requestTimeoutMs);
  }
}

/**
 * Module for interacting with PTYs (pseudo-terminals) in the sandbox.
 *
 * Mirrors the Python/E2B ``sandbox.pty`` namespace: {@link Pty.create} starts a
 * new interactive shell, {@link Pty.connect} reattaches to an existing one, and
 * {@link Pty.kill}/{@link Pty.sendStdin}/{@link Pty.resize} give ad-hoc control
 * by PID without holding a {@link PtyHandle}.
 */
export class Pty {
  private readonly sandbox: Sandbox;

  constructor(sandbox: Sandbox) {
    this.sandbox = sandbox;
  }

  private url(method: string): string {
    return this.sandbox.dataUrl(ENVD_PORT, `/process.Process/${method}`);
  }

  private buildHeaders(options: {
    streaming: boolean;
    user?: string;
    timeoutMs?: number;
  }): Record<string, string> {
    const headers: Record<string, string> = options.streaming
      ? {
          "Content-Type": CONNECT_CONTENT_TYPE,
          "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
          "Connect-Content-Encoding": "identity",
        }
      : {
          "Content-Type": "application/json",
          "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
        };
    if (options.timeoutMs !== undefined && options.timeoutMs > 0) {
      headers["Connect-Timeout-Ms"] = String(Math.trunc(options.timeoutMs));
    }
    const accessToken = this.sandbox.envdAccessToken;
    if (accessToken) {
      headers["X-Access-Token"] = accessToken;
    }
    Object.assign(headers, this.sandbox.trafficTokenHeaders());
    Object.assign(headers, userHeaders(options.user));
    return headers;
  }

  // ── Selector RPCs (unary application/json) ────────────────────────────────

  /**
   * Send ``SIGKILL`` to a PTY process.
   *
   * @returns ``true`` if killed, ``false`` if not found (e.g. already exited).
   */
  async kill(pid: number, requestTimeoutMs?: number): Promise<boolean> {
    const result = await this.unary(
      "SendSignal",
      { process: { pid }, signal: SIGNAL_SIGKILL },
      { requestTimeoutMs, allowNotFound: true },
    );
    return result !== null;
  }

  /** Send input (bytes or a UTF-8 string) to a PTY identified by *pid*. */
  async sendStdin(
    pid: number,
    data: string | Uint8Array,
    requestTimeoutMs?: number,
  ): Promise<void> {
    const bytes = typeof data === "string" ? Buffer.from(data, "utf-8") : Buffer.from(data);
    await this.unary(
      "SendInput",
      { process: { pid }, input: { pty: bytes.toString("base64") } },
      { requestTimeoutMs },
    );
  }

  /** Resize a running PTY. */
  async resize(pid: number, size: PtySize, requestTimeoutMs?: number): Promise<void> {
    await this.unary(
      "Update",
      { process: { pid }, pty: { size: { rows: size.rows, cols: size.cols } } },
      { requestTimeoutMs },
    );
  }

  private async unary(
    method: string,
    payload: Record<string, unknown>,
    options: { requestTimeoutMs?: number; allowNotFound?: boolean } = {},
  ): Promise<Record<string, any> | null> {
    const timeoutMs = options.requestTimeoutMs ?? this.sandbox.config.requestTimeoutMs;
    const headers = this.buildHeaders({ streaming: false, timeoutMs });

    let resp: Awaited<ReturnType<typeof fetch>>;
    try {
      resp = await fetch(this.url(method), {
        method: "POST",
        headers,
        body: JSON.stringify(payload),
        dispatcher: this.sandbox.dataDispatcher,
        signal: AbortSignal.timeout(timeoutMs),
      });
    } catch (err) {
      throw new CubeSandboxError(`${method} failed: ${(err as Error).message}`);
    }

    if (resp.status >= 400) {
      const body = await resp.text().catch(() => "");
      if (options.allowNotFound && isNotFound(resp.status, body)) {
        return null;
      }
      const detail = body.trim();
      const suffix = detail ? `: ${detail}` : "";
      throw new ApiError(`${method} failed: HTTP ${resp.status}${suffix}`, resp.status);
    }

    const raw = await resp.text();
    if (!raw) {
      return {};
    }
    try {
      return JSON.parse(raw);
    } catch (err) {
      throw new CubeSandboxError(`${method}: invalid JSON response: ${(err as Error).message}`);
    }
  }

  // ── Streaming RPCs (framed application/connect+json) ───────────────────────

  /** Start a new PTY running an interactive login bash shell. */
  create(size: PtySize, options: PtyCreateOptions = {}): Promise<PtyHandle> {
    const envs: Record<string, string> = {
      TERM: "xterm-256color",
      LANG: "C.UTF-8",
      LC_ALL: "C.UTF-8",
      ...(options.envs ?? {}),
    };
    const process: Record<string, unknown> = {
      cmd: "/bin/bash",
      args: ["-i", "-l"],
      envs,
    };
    if (options.cwd) {
      process.cwd = options.cwd;
    }
    const payload = { process, pty: { size: { rows: size.rows, cols: size.cols } } };

    return this.openStream("Start", payload, {
      user: options.user || DEFAULT_ENVD_USER,
      timeoutMs: options.timeoutMs ?? DEFAULT_PTY_TIMEOUT_MS,
    });
  }

  /** Reattach to an already-running PTY. */
  connect(pid: number, options: PtyConnectOptions = {}): Promise<PtyHandle> {
    return this.openStream(
      "Connect",
      { process: { pid } },
      { timeoutMs: options.timeoutMs ?? DEFAULT_PTY_TIMEOUT_MS },
    );
  }

  private async openStream(
    method: string,
    payload: Record<string, unknown>,
    options: { user?: string; timeoutMs: number },
  ): Promise<PtyHandle> {
    const control = createStreamControl(options.timeoutMs);
    const headers = this.buildHeaders({
      streaming: true,
      user: options.user,
      timeoutMs: options.timeoutMs,
    });
    const body = encodeConnectEnvelope(Buffer.from(JSON.stringify(payload), "utf-8"));

    let resp: Awaited<ReturnType<typeof fetch>>;
    try {
      resp = await fetch(this.url(method), {
        method: "POST",
        headers,
        body,
        dispatcher: this.sandbox.dataDispatcher,
        signal: control.signal,
      });
    } catch (err) {
      control.clearIdle();
      if (control.idleFired) {
        throw new CubeSandboxError(`${method} timed out after ${options.timeoutMs}ms`);
      }
      throw new CubeSandboxError(`${method} failed: ${(err as Error).message}`);
    }

    if (resp.status >= 400) {
      control.clearIdle();
      const detail = (await resp.text().catch(() => "")).trim();
      const suffix = detail ? `: ${detail}` : "";
      throw new ApiError(`${method} failed: HTTP ${resp.status}${suffix}`, resp.status);
    }
    if (!resp.body) {
      control.clearIdle();
      throw new CubeSandboxError(`${method}: stream closed before start event`);
    }

    const events = iterConnectEvents(resp.body as ReadableStream<Uint8Array>, control);

    let pid: number;
    try {
      const first = await events.next();
      if (first.done) {
        throw new CubeSandboxError(`${method}: stream closed before start event`);
      }
      const start = first.value?.start;
      if (!start || start.pid === undefined || start.pid === null) {
        throw new CubeSandboxError(
          `${method}: expected start event, got ${JSON.stringify(first.value)}`,
        );
      }
      pid = Number(start.pid);
    } catch (err) {
      control.disconnect();
      try {
        await events.return(undefined);
      } catch {
        // best-effort cleanup
      }
      if (control.idleFired) {
        throw new CubeSandboxError(`${method} timed out after ${options.timeoutMs}ms`);
      }
      throw err;
    }

    return new PtyHandle({
      pid,
      events,
      control,
      handleKill: () => this.kill(pid),
      handleSendStdin: (data, rt) => this.sendStdin(pid, data, rt),
      handleResize: (size, rt) => this.resize(pid, size, rt),
    });
  }
}

/**
 * Yield the ``event`` field of each ProcessEvent JSON message from a
 * Connect-framed stream. Resets ``control``'s idle timer on every frame and
 * stops (raising on error) at the end-of-stream trailer.
 */
async function* iterConnectEvents(
  body: ReadableStream<Uint8Array>,
  control: StreamControl,
): AsyncGenerator<Record<string, any>> {
  for await (const { flags, payload } of readConnectFrames(body)) {
    control.reset();
    if (flags & CONNECT_COMPRESSED_FLAG) {
      throw new CubeSandboxError("unsupported compressed Connect stream message");
    }
    if (flags & CONNECT_END_STREAM_FLAG) {
      raiseConnectEndStream(payload);
      return;
    }
    const message = JSON.parse(payload.toString("utf-8"));
    const event = message.event;
    if (event !== undefined && event !== null) {
      yield event as Record<string, any>;
    }
  }
}

/** Best-effort exit-code extraction from an end event. */
function extractExitCode(end: Record<string, any>): number | null {
  if (end.exitCode !== undefined && end.exitCode !== null) {
    return Number(end.exitCode);
  }
  if (end.exit_code !== undefined && end.exit_code !== null) {
    return Number(end.exit_code);
  }
  const parsed = exitCodeFromStatus(end.status);
  if (parsed !== null) {
    return parsed;
  }
  if (end.exited) {
    return 0;
  }
  return null;
}

/** Detect Connect's ``not_found`` on a unary error response (HTTP 404 or body code). */
function isNotFound(status: number, body: string): boolean {
  if (status === 404) {
    return true;
  }
  if (!body) {
    return false;
  }
  try {
    const parsed = JSON.parse(body);
    const code = parsed?.code;
    return typeof code === "string" && code.toLowerCase() === "not_found";
  } catch {
    return false;
  }
}
