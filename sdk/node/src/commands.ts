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
    if (idleMs !== undefined) {
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
      const detail = (await resp.text().catch(() => "")).trim();
      const suffix = detail ? `: ${detail}` : "";
      throw new Error(`command failed: HTTP ${resp.status}${suffix}`);
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
    const stdout: string[] = [];
    const stderr: string[] = [];
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
        stdout.push(Buffer.from(data.stdout, "base64").toString("utf-8"));
      }
      if (data.stderr) {
        stderr.push(Buffer.from(data.stderr, "base64").toString("utf-8"));
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
    return { stdout: stdout.join(""), stderr: stderr.join(""), exitCode };
  }
}
