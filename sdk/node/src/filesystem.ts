// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { fetch } from "undici";

import {
  CONNECT_CONTENT_TYPE,
  CONNECT_END_STREAM_FLAG,
  CONNECT_PROTOCOL_VERSION,
  DEFAULT_ENVD_USER,
  ENVD_PORT,
  encodeConnectEnvelope,
  readConnectFrames,
} from "./commands.js";
import { FilesystemNotFoundError, PartialWriteError } from "./exceptions.js";
import type { Sandbox } from "./sandbox.js";

/** A file or directory entry returned by list/stat/makeDir/rename. */
export type FileEntry = Record<string, any>;

/** A single file to write via {@link Filesystem.writeFiles}. */
export interface WriteEntry {
  path: string;
  data: string | Uint8Array;
}

/** A filesystem change event delivered by {@link Watcher}. */
export interface WatchEvent {
  name: string;
  type: string;
}

function toBuffer(data: string | Uint8Array): Buffer {
  return typeof data === "string" ? Buffer.from(data, "utf-8") : Buffer.from(data);
}

export class Filesystem {
  private readonly sandbox: Sandbox;

  constructor(sandbox: Sandbox) {
    this.sandbox = sandbox;
  }

  private baseHeaders(): Record<string, string> {
    const headers: Record<string, string> = { ...this.sandbox.trafficTokenHeaders() };
    const token = this.sandbox.envdAccessToken;
    if (token) {
      headers["X-Access-Token"] = token;
    }
    return headers;
  }

  private async rpc(method: string, payload: Record<string, unknown>): Promise<Record<string, any>> {
    const headers: Record<string, string> = {
      ...this.baseHeaders(),
      "Content-Type": "application/json",
      "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
    };
    const resp = await fetch(this.sandbox.dataUrl(ENVD_PORT, `/filesystem.Filesystem/${method}`), {
      method: "POST",
      headers,
      body: JSON.stringify(payload),
      dispatcher: this.sandbox.dataDispatcher,
    });

    const text = await resp.text();
    if (resp.status >= 400) {
      let body: Record<string, any> = {};
      try {
        body = text ? JSON.parse(text) : {};
      } catch {
        body = {};
      }
      const code = body.code ?? "";
      let message = body.message || body.detail || text || `HTTP ${resp.status}`;
      if (code) {
        message = `${code}: ${message}`;
      }
      if (resp.status === 404 || code === "not_found") {
        throw new FilesystemNotFoundError(
          `Filesystem ${method} failed: ${message}`,
          resp.status,
        );
      }
      throw new Error(`Filesystem ${method} failed: ${message}`);
    }
    if (!text) {
      return {};
    }
    return JSON.parse(text);
  }

  /** Read a file's contents through envd's HTTP file API. */
  async read(path: string, options: { user?: string } = {}): Promise<string> {
    const user = options.user || DEFAULT_ENVD_USER;
    const params = new URLSearchParams({ path, username: user });
    const resp = await fetch(this.sandbox.dataUrl(ENVD_PORT, `/files?${params.toString()}`), {
      method: "GET",
      headers: this.baseHeaders(),
      dispatcher: this.sandbox.dataDispatcher,
    });
    const text = await resp.text();
    if (resp.status !== 200) {
      let message = text || `HTTP ${resp.status}`;
      try {
        const body = JSON.parse(text);
        message = body.message || body.detail || message;
      } catch {
        // raw text
      }
      throw new Error(`Failed to read ${path}: ${message}`);
    }
    return text;
  }

  /** Write a file through envd's HTTP file API (octet-stream, multipart fallback). */
  async write(
    path: string,
    data: string | Uint8Array,
    options: { user?: string } = {},
  ): Promise<void> {
    const user = options.user || DEFAULT_ENVD_USER;
    const params = new URLSearchParams({ path, username: user });
    const url = this.sandbox.dataUrl(ENVD_PORT, `/files?${params.toString()}`);
    const body = toBuffer(data);

    let resp = await fetch(url, {
      method: "POST",
      headers: { ...this.baseHeaders(), "Content-Type": "application/octet-stream" },
      body,
      dispatcher: this.sandbox.dataDispatcher,
    });

    if (resp.status >= 400) {
      const form = new FormData();
      form.append("file", new Blob([body]), path);
      resp = await fetch(url, {
        method: "POST",
        headers: this.baseHeaders(),
        body: form,
        dispatcher: this.sandbox.dataDispatcher,
      });
    }

    if (resp.status >= 400) {
      const text = await resp.text().catch(() => "");
      let message = text || `HTTP ${resp.status}`;
      try {
        const payload = JSON.parse(text);
        message = payload.message || payload.detail || message;
      } catch {
        // raw text
      }
      throw new Error(`Failed to write ${path}: ${message}`);
    }
  }

  /** Write multiple files. Returns the count; throws {@link PartialWriteError} on first failure. */
  async writeFiles(files: WriteEntry[], options: { user?: string } = {}): Promise<number> {
    for (let i = 0; i < files.length; i++) {
      const { path, data } = files[i];
      try {
        await this.write(path, data, options);
      } catch (err) {
        throw new PartialWriteError(
          `writeFiles failed at ${path} (${i + 1}/${files.length}): ${
            err instanceof Error ? err.message : String(err)
          }`,
          i,
        );
      }
    }
    return files.length;
  }

  /** List entries in a directory. */
  async list(path: string): Promise<FileEntry[]> {
    const result = await this.rpc("ListDir", { path });
    return result.entries ?? [];
  }

  /** Return metadata for a file or directory. */
  async stat(path: string): Promise<FileEntry> {
    const result = await this.rpc("Stat", { path });
    return result.entry ?? {};
  }

  /** Return true if the path exists inside the sandbox. */
  async exists(path: string): Promise<boolean> {
    try {
      await this.stat(path);
      return true;
    } catch (err) {
      if (err instanceof FilesystemNotFoundError) {
        return false;
      }
      throw err;
    }
  }

  /** Delete a file or directory inside the sandbox. */
  async remove(path: string): Promise<void> {
    await this.rpc("Remove", { path });
  }

  /** Move or rename a file or directory. */
  async rename(oldPath: string, newPath: string): Promise<FileEntry> {
    const result = await this.rpc("Move", { source: oldPath, destination: newPath });
    return result.entry ?? {};
  }

  /** Create a directory inside the sandbox. */
  async makeDir(path: string): Promise<FileEntry> {
    const result = await this.rpc("MakeDir", { path });
    return result.entry ?? {};
  }

  /** Watch a directory for filesystem changes. */
  async watchDir(path: string): Promise<Watcher> {
    const controller = new AbortController();
    const headers: Record<string, string> = {
      ...this.baseHeaders(),
      "Content-Type": CONNECT_CONTENT_TYPE,
      "Connect-Protocol-Version": CONNECT_PROTOCOL_VERSION,
    };
    const envelope = encodeConnectEnvelope(Buffer.from(JSON.stringify({ path }), "utf-8"));
    const resp = await fetch(
      this.sandbox.dataUrl(ENVD_PORT, "/filesystem.Filesystem/WatchDir"),
      {
        method: "POST",
        headers,
        body: envelope,
        dispatcher: this.sandbox.dataDispatcher,
        signal: controller.signal,
      },
    );
    if (resp.status >= 400) {
      controller.abort();
      throw new Error(`WatchDir failed: HTTP ${resp.status}`);
    }
    if (!resp.body) {
      controller.abort();
      throw new Error("WatchDir failed: empty response body");
    }
    return new Watcher(resp.body as ReadableStream<Uint8Array>, controller);
  }
}

/**
 * Iterates over filesystem change events from a streaming ``WatchDir``
 * response. Use it as an async iterator and call {@link Watcher.close} when
 * done:
 * ```ts
 * const watcher = await sb.files.watchDir("/tmp");
 * for await (const ev of watcher) {
 *   console.log(ev.name, ev.type);
 * }
 * ```
 */
export class Watcher {
  private readonly body: ReadableStream<Uint8Array>;
  private readonly controller: AbortController;
  private closed = false;

  constructor(body: ReadableStream<Uint8Array>, controller: AbortController) {
    this.body = body;
    this.controller = controller;
  }

  /** Terminate the watch stream and release resources. */
  close(): void {
    if (!this.closed) {
      this.closed = true;
      this.controller.abort();
    }
  }

  async *[Symbol.asyncIterator](): AsyncIterator<WatchEvent> {
    try {
      for await (const { flags, payload } of readConnectFrames(this.body)) {
        if (flags & CONNECT_END_STREAM_FLAG) {
          const data = payload.length ? JSON.parse(payload.toString("utf-8")) : {};
          if (data?.error) {
            throw new Error(data.error.message || "watch stream error");
          }
          return;
        }
        const data = JSON.parse(payload.toString("utf-8"));
        if (data.filesystem) {
          yield { name: data.filesystem.name ?? "", type: data.filesystem.type ?? "" };
        }
      }
    } catch (err) {
      if (this.closed || (err as Error)?.name === "AbortError") {
        return;
      }
      throw err;
    }
  }
}
