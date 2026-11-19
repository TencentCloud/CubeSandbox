// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { fetch } from "undici";

import type { Sandbox } from "./sandbox.js";
import { createIdleTimeout } from "./stream.js";

export const ENVD_PORT = 49983;
export const CONNECT_PROTOCOL_VERSION = "1";
export const CONNECT_CONTENT_TYPE = "application/connect+json";
export const CONNECT_END_STREAM_FLAG = 0x02;
export const CONNECT_COMPRESSED_FLAG = 0x01;
export const MAX_CONNECT_ENVELOPE_SIZE = 64 * 1024 * 1024;
export const DEFAULT_ENVD_USER = "root";
const CONNECT_KNOWN_FLAGS = CONNECT_COMPRESSED_FLAG | CONNECT_END_STREAM_FLAG;
// Hints shared with the Go SDK (sdk/go/connect.go). The routing hint is for
// HTML pages; a body that is not Connect-framed can also be a compressed
// response, so its hint names that cause too.
const ROUTING_HINT =
  "check whether CUBE_PROXY_NODE_IP is configured or if the data-plane endpoint points to a web service";
const NOT_AN_ENVELOPE_HINT =
  "check whether CUBE_PROXY_NODE_IP is configured, the data-plane endpoint points to a web service, or the response was compressed";
// Bounds for reading an HTTP error page on a Connect stream request.
const MAX_ROUTING_PAGE_PEEK_BYTES = 4096;
const MAX_DIAGNOSTIC_LEN = 200;

interface HttpResponseLike {
  status: number;
  headers: { get(name: string): string | null };
}

function mediaType(resp: HttpResponseLike): string {
  return (resp.headers.get("content-type") ?? "").split(";", 1)[0].trim().toLowerCase();
}

/**
 * Diagnose an HTTP 200 ``text/html`` response on a Connect stream. A Connect
 * stream is never labelled ``text/html``; such a response means the request
 * reached a Web UI or portal page instead of CubeProxy. Returns ``null`` for
 * anything else and never reads the body. Mirrors the Go SDK.
 */
export function htmlResponseError(resp: HttpResponseLike): string | null {
  if (resp.status !== 200 || mediaType(resp) !== "text/html") {
    return null;
  }
  const contentType = resp.headers.get("content-type") ?? "";
  return `received HTML instead of a Connect response (content-type ${JSON.stringify(contentType)}); ${ROUTING_HINT}`;
}

/**
 * Whether an HTTP error body looks like a Web UI or gateway page. A JSON
 * error document keeps the server's message even when labelled text/html.
 */
function isRoutingPage(type: string, body: Buffer): boolean {
  const trimmed = body.toString("latin1").trim();
  if (trimmed.startsWith("{")) {
    return false;
  }
  const head = trimmed.slice(0, "<!doctype".length).toLowerCase();
  return type === "text/html" || head.startsWith("<!doctype") || head.startsWith("<html");
}

function routingPageHint(status: number): string {
  if (status === 401 || status === 403) {
    return "received an HTML/text page instead of a Connect stream; check credentials or whether a portal is intercepting the request";
  }
  if (status >= 500) {
    return "received an HTML error page instead of a Connect stream; the request may have reached CubeProxy but the upstream envd or sandbox node is unreachable";
  }
  return `response may be an HTML/text page instead of a Connect stream; ${ROUTING_HINT}`;
}

function truncateDiagnostic(message: string): string {
  const chars = Array.from(message);
  return chars.length <= MAX_DIAGNOSTIC_LEN
    ? message
    : `${chars.slice(0, MAX_DIAGNOSTIC_LEN).join("")}...`;
}

/**
 * Describe an HTTP error response to a Connect stream request: the trimmed
 * body text, as before, except that an HTML page (a misrouted request landing
 * on a Web UI, gateway or portal) is reported from a bounded prefix of its
 * body followed by the same hint the Go SDK gives.
 */
export async function streamHttpErrorDetail(
  resp: HttpResponseLike & { body: ReadableStream<Uint8Array> | null },
): Promise<string> {
  if (!resp.body) {
    return "";
  }
  const reader = resp.body.getReader();
  const chunks: Buffer[] = [];
  let length = 0;
  try {
    while (length < MAX_ROUTING_PAGE_PEEK_BYTES) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      chunks.push(Buffer.from(value));
      length += value.length;
    }
    const peek = Buffer.concat(chunks);
    if (isRoutingPage(mediaType(resp), peek)) {
      await reader.cancel().catch(() => undefined);
      const text = peek.subarray(0, MAX_ROUTING_PAGE_PEEK_BYTES).toString("utf-8").trim();
      const hint = routingPageHint(resp.status);
      return text ? `${truncateDiagnostic(text)}; ${hint}` : hint;
    }
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      chunks.push(Buffer.from(value));
    }
  } catch {
    // Report whatever was read before the body failed.
  }
  return Buffer.concat(chunks).toString("utf-8").trim();
}

/** Result of running a shell command inside the sandbox. */
export interface CommandResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

/** Options for {@link Commands.run}. */
export interface CommandOptions {
  /**
   * Timeout in milliseconds. Applied two ways, matching the Python SDK:
   *  - a client-side idle (read) abort that resets on every chunk received, and
   *  - sent to envd as ``Connect-Timeout-Ms``, which envd enforces as a hard
   *    wall-clock deadline for the whole command.
   *
   * A command is therefore aborted once it either goes silent for longer than
   * this OR runs longer than this in total — whichever comes first. So a
   * long-running command is not kept alive just because it keeps producing
   * output; use a larger timeout (or none) for genuinely long jobs.
   * Default: no timeout.
   */
  timeoutMs?: number;
  /**
   * Alias for {@link CommandOptions.timeoutMs} (also milliseconds), matching
   * the Issue #760 / E2B shape. {@link CommandOptions.timeoutMs} wins when both
   * are set.
   */
  timeout?: number;
  cwd?: string;
  envs?: Record<string, string>;
  /** Alias for ``envs``, matching the E2B SDK command API. */
  env?: Record<string, string>;
  /** Sandbox user for envd process auth. Defaults to ``"root"``. */
  user?: string;
}

/** Encode a single Connect envelope: ``[flags:1][len:4 BE][payload]``. */
export function encodeConnectEnvelope(data: Buffer, flags = 0): Buffer {
  const header = Buffer.alloc(5);
  header.writeUInt8(flags, 0);
  header.writeUInt32BE(data.length, 1);
  return Buffer.concat([header, data]);
}

/** ``Authorization: Basic base64(user:)`` header for envd. */
export function basicAuthUser(user: string): string {
  const token = Buffer.from(`${user}:`).toString("base64");
  return `Basic ${token}`;
}

export function userHeaders(user: string | undefined): Record<string, string> {
  return user ? { Authorization: basicAuthUser(user) } : {};
}

/**
 * Consume a web ``ReadableStream`` of Connect-framed bytes, yielding each
 * complete frame as ``{ flags, payload }``. Mirrors the Python/Go SDKs.
 *
 * Incoming chunks are held in a list and only copied out once a full frame is
 * available, so each received byte is copied at most once. This avoids the
 * quadratic ``Buffer.concat`` cost when a single large frame arrives split
 * across many chunks.
 */
export async function* readConnectFrames(
  body: ReadableStream<Uint8Array>,
): AsyncGenerator<{ flags: number; payload: Buffer }> {
  const reader = body.getReader();
  const chunks: Buffer[] = [];
  let buffered = 0;

  // Peek the 5-byte header without consuming it. Requires ``buffered >= 5``.
  const peekHeader = (): { flags: number; size: number } => {
    const first = chunks[0];
    if (first.length >= 5) {
      return { flags: first.readUInt8(0), size: first.readUInt32BE(1) };
    }
    const head = Buffer.allocUnsafe(5);
    let filled = 0;
    for (const chunk of chunks) {
      const copyLen = Math.min(chunk.length, 5 - filled);
      chunk.copy(head, filled, 0, copyLen);
      filled += copyLen;
      if (filled === 5) {
        break;
      }
    }
    return { flags: head.readUInt8(0), size: head.readUInt32BE(1) };
  };

  // Remove and return the first ``n`` bytes. Requires ``buffered >= n``.
  const take = (n: number): Buffer => {
    const first = chunks[0];
    if (first.length >= n) {
      buffered -= n;
      if (first.length === n) {
        chunks.shift();
        return first;
      }
      chunks[0] = first.subarray(n);
      return first.subarray(0, n);
    }
    const out = Buffer.allocUnsafe(n);
    let filled = 0;
    while (filled < n) {
      const chunk = chunks[0];
      const need = n - filled;
      if (chunk.length <= need) {
        chunk.copy(out, filled);
        filled += chunk.length;
        chunks.shift();
      } else {
        chunk.copy(out, filled, 0, need);
        chunks[0] = chunk.subarray(need);
        filled = n;
      }
    }
    buffered -= n;
    return out;
  };

  try {
    for (;;) {
      while (buffered >= 5) {
        const { flags, size } = peekHeader();
        // A misrouted HTML, JSON or gzip body would otherwise be read as an
        // envelope ("<!DOC" -> flags 0x3c, length 558124867) and reported as
        // an oversized message. Reject unknown flags before the length.
        if (flags & ~CONNECT_KNOWN_FLAGS) {
          const head = take(5).toString("hex");
          throw new Error(
            `response is not a Connect envelope (first 5 bytes: ${head}); ${NOT_AN_ENVELOPE_HINT}`,
          );
        }
        if (size > MAX_CONNECT_ENVELOPE_SIZE) {
          throw new Error(`Connect stream message too large: ${size} bytes`);
        }
        if (buffered < 5 + size) {
          break;
        }
        const frame = take(5 + size);
        yield { flags, payload: frame.subarray(5, 5 + size) };
      }
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      if (value && value.length > 0) {
        const chunk = Buffer.from(value);
        chunks.push(chunk);
        buffered += chunk.length;
      }
    }
  } finally {
    reader.releaseLock();
  }
  if (buffered > 0) {
    throw new Error("Connect stream ended with a partial message");
  }
}

export function raiseConnectEndStream(payload: Buffer): void {
  if (payload.length === 0) {
    return;
  }
  let data: Record<string, any>;
  try {
    data = JSON.parse(payload.toString("utf-8"));
  } catch {
    return;
  }
  const error = data?.error;
  if (!error) {
    return;
  }
  const message = String(error.message || "Connect stream error").trim();
  if (error.code) {
    throw new Error(`${error.code}: ${message}`);
  }
  throw new Error(message);
}

export function exitCodeFromStatus(status: unknown): number | null {
  if (typeof status !== "string") {
    return null;
  }
  const exitMatch = status.match(/(?:exit status|exited with code)\s+(-?\d+)/);
  if (exitMatch) {
    return parseInt(exitMatch[1], 10);
  }
  const signalMatch = status.match(/(?:signal|terminated by signal)\s+(\d+)/);
  if (signalMatch) {
    return 128 + parseInt(signalMatch[1], 10);
  }
  if (status === "exited") {
    return 0;
  }
  return null;
}

export class Commands {
  private readonly sandbox: Sandbox;

  constructor(sandbox: Sandbox) {
    this.sandbox = sandbox;
  }

  /**
   * Run a shell command inside the sandbox through envd's process API.
   * Starts ``/bin/bash -l -c <cmd>`` and returns the collected stdout/stderr
   * plus the process exit code.
   */
  async run(cmd: string, options: CommandOptions = {}): Promise<CommandResult> {
    const envs = options.envs ?? options.env ?? {};
    const user = options.user || DEFAULT_ENVD_USER;
    const idleMs = options.timeoutMs ?? options.timeout;

    const process: Record<string, unknown> = {
      cmd: "/bin/bash",
      args: ["-l", "-c", cmd],
      envs,
    };
    if (options.cwd) {
      process.cwd = options.cwd;
    }
    const payload = { process, stdin: false };

    const headers: Record<string, string> = {
      "Content-Type": CONNECT_CONTENT_TYPE,
      "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
      "Connect-Content-Encoding": "identity",
      ...this.sandbox.trafficTokenHeaders(),
      ...userHeaders(user),
    };
    // Only a positive deadline is sent. envd reads Connect-Timeout-Ms as a hard
    // wall-clock deadline, so a zero or negative one has already passed and the
    // request never gets an answer — it hangs until the HTTP client gives up on
    // headers, and reports a transport error that says nothing about the cause.
    //
    // Both non-positive values arrive in ordinary use: `0` is how the e2b SDK
    // spells "no deadline", and NEVER_TIMEOUT is how this one does. `pty.ts`
    // and the Go SDK already guard this way.
    if (idleMs !== undefined && idleMs > 0) {
      headers["Connect-Timeout-Ms"] = String(Math.trunc(idleMs));
    }
    const accessToken = this.sandbox.envdAccessToken;
    if (accessToken) {
      headers["X-Access-Token"] = accessToken;
    }

    const url = this.sandbox.dataUrl(ENVD_PORT, "/process.Process/Start");
    const encoded = encodeConnectEnvelope(Buffer.from(JSON.stringify(payload), "utf-8"));

    const idle = createIdleTimeout(idleMs);
    let resp: Awaited<ReturnType<typeof fetch>>;
    try {
      resp = await fetch(url, {
        method: "POST",
        headers,
        body: encoded,
        dispatcher: this.sandbox.dataDispatcher,
        signal: idle.signal,
      });
    } catch (err) {
      idle.clear();
      if (idle.firedRef.current) {
        throw new Error(`command timed out after ${idleMs}ms of inactivity`);
      }
      throw err;
    }

    if (resp.status >= 400) {
      idle.clear();
      const detail = await streamHttpErrorDetail(resp);
      const suffix = detail ? `: ${detail}` : "";
      throw new Error(`command failed: HTTP ${resp.status}${suffix}`);
    }
    const htmlError = htmlResponseError(resp);
    if (htmlError) {
      idle.clear();
      await resp.body?.cancel().catch(() => undefined);
      throw new Error(htmlError);
    }
    if (!resp.body) {
      idle.clear();
      throw new Error("process stream ended without EndEvent");
    }

    try {
      return await this.collectProcessStream(
        resp.body as ReadableStream<Uint8Array>,
        idle.reset,
      );
    } catch (err) {
      if (idle.firedRef.current) {
        throw new Error(`command timed out after ${idleMs}ms of inactivity`);
      }
      throw err;
    } finally {
      idle.clear();
    }
  }

  private async collectProcessStream(
    body: ReadableStream<Uint8Array>,
    onActivity?: () => void,
  ): Promise<CommandResult> {
    const stdout: Buffer[] = [];
    const stderr: Buffer[] = [];
    let exitCode: number | null = null;

    for await (const { flags, payload } of readConnectFrames(body)) {
      onActivity?.();
      if (flags & CONNECT_COMPRESSED_FLAG) {
        throw new Error("unsupported compressed Connect stream message");
      }
      if (flags & CONNECT_END_STREAM_FLAG) {
        // End-of-stream trailer: throws if it carries an error, otherwise the
        // stream is finished — stop reading rather than processing any further
        // (a well-behaved envd sends nothing after EOS; a misbehaving one must
        // not be able to inject post-EOS frames).
        raiseConnectEndStream(payload);
        break;
      }

      const event = JSON.parse(payload.toString("utf-8")).event ?? {};
      const data = event.data ?? {};
      if (data.stdout) {
        stdout.push(Buffer.from(data.stdout, "base64"));
      }
      if (data.stderr) {
        stderr.push(Buffer.from(data.stderr, "base64"));
      }
      const end = event.end;
      if (end !== undefined && end !== null) {
        if (end.exitCode !== undefined) {
          exitCode = Number(end.exitCode);
        } else if (end.exit_code !== undefined) {
          exitCode = Number(end.exit_code);
        } else if (exitCodeFromStatus(end.status) !== null) {
          exitCode = exitCodeFromStatus(end.status);
        } else if (end.error) {
          throw new Error(`process failed: ${end.error}`);
        } else if (end.exited) {
          exitCode = 0;
        } else {
          throw new Error("process EndEvent missing exit code");
        }
      }
    }

    if (exitCode === null) {
      throw new Error("process stream ended without EndEvent");
    }
    return {
      stdout: Buffer.concat(stdout).toString("utf-8"),
      stderr: Buffer.concat(stderr).toString("utf-8"),
      exitCode,
    };
  }
}
