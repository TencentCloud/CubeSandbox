// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { createServer, type IncomingHttpHeaders, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { Config } from "../src/config.js";
import type { WatchEvent } from "../src/filesystem.js";
import { Sandbox } from "../src/index.js";
import { stubCubeEnv } from "./_env.js";

stubCubeEnv();

const SANDBOX_ID = "sb-test-001";
const DOMAIN = "cube.app";
const SANDBOX_DATA = {
  sandboxID: SANDBOX_ID,
  templateID: "tpl-test",
  domain: DOMAIN,
  state: "running",
};

interface RecordedRequest {
  method: string;
  pathname: string;
  headers: IncomingHttpHeaders;
  body: Buffer;
}

interface MockResponse {
  status?: number;
  buffer?: Buffer;
}

type Handler = (req: RecordedRequest) => MockResponse;

let server: Server;
let port: number;
let requests: RecordedRequest[] = [];
let handler: Handler = () => ({ status: 201 });

/** Build a single Connect protocol envelope: [flags:1][len:4 BE][payload]. */
function connectFrame(flags: number, payload: string): Buffer {
  const raw = Buffer.from(payload, "utf-8");
  const header = Buffer.alloc(5);
  header.writeUInt8(flags, 0);
  header.writeUInt32BE(raw.length, 1);
  return Buffer.concat([header, raw]);
}

function makeConfig(): Config {
  return new Config({
    apiUrl: `http://127.0.0.1:${port}`,
    templateId: "tpl-test",
    proxyNodeIp: "127.0.0.1",
    proxyPort: port,
    sandboxDomain: DOMAIN,
  });
}

async function createSandbox(): Promise<Sandbox> {
  handler = () => ({ status: 201, buffer: Buffer.from(JSON.stringify(SANDBOX_DATA)) });
  const sb = await Sandbox.create({ config: makeConfig() });
  requests = [];
  return sb;
}

async function drain(watcher: AsyncIterable<WatchEvent>): Promise<WatchEvent[]> {
  const events: WatchEvent[] = [];
  for await (const ev of watcher) {
    events.push(ev);
  }
  return events;
}

beforeAll(async () => {
  server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const url = new URL(req.url ?? "/", "http://localhost");
      requests.push({
        method: req.method ?? "",
        pathname: url.pathname,
        headers: req.headers,
        body: Buffer.concat(chunks),
      });
      const out = handler(requests[requests.length - 1]);
      res.writeHead(out.status ?? 200);
      res.end(out.buffer ?? "");
    });
  });
  await new Promise<void>((resolve) => server.listen(0, resolve));
  port = (server.address() as AddressInfo).port;
});

afterAll(() => {
  server.close();
});

afterEach(() => {
  requests = [];
});

describe("Filesystem.watchDir / Watcher", () => {
  it("streams filesystem events until the end-of-stream frame", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.method).toBe("POST");
      expect(req.pathname).toBe("/filesystem.Filesystem/WatchDir");
      // The requested path is carried inside a Connect envelope.
      expect(req.body.subarray(5).toString()).toContain("/tmp");
      return {
        status: 200,
        buffer: Buffer.concat([
          connectFrame(0, JSON.stringify({ filesystem: { name: "a.txt", type: "create" } })),
          connectFrame(0, JSON.stringify({ filesystem: { name: "b.txt", type: "write" } })),
          connectFrame(0x02, "{}"),
        ]),
      };
    };

    const watcher = await sb.files.watchDir("/tmp");
    const events = await drain(watcher);
    expect(events).toEqual([
      { name: "a.txt", type: "create" },
      { name: "b.txt", type: "write" },
    ]);
    watcher.close();
    sb.close();
  });

  it("ignores frames without a filesystem payload", async () => {
    const sb = await createSandbox();
    handler = () => ({
      status: 200,
      buffer: Buffer.concat([
        connectFrame(0, JSON.stringify({ keepalive: true })),
        connectFrame(0, JSON.stringify({ filesystem: { name: "only.txt", type: "remove" } })),
        connectFrame(0x02, "{}"),
      ]),
    });
    const events = await drain(await sb.files.watchDir("/tmp"));
    expect(events).toEqual([{ name: "only.txt", type: "remove" }]);
    sb.close();
  });

  it("throws when the end-of-stream frame carries an error", async () => {
    const sb = await createSandbox();
    handler = () => ({
      status: 200,
      buffer: connectFrame(0x02, JSON.stringify({ error: { message: "watch boom" } })),
    });
    const watcher = await sb.files.watchDir("/tmp");
    await expect(drain(watcher)).rejects.toThrow(/watch boom/);
    sb.close();
  });

  it("rejects when WatchDir returns an error status", async () => {
    const sb = await createSandbox();
    handler = () => ({ status: 500 });
    await expect(sb.files.watchDir("/tmp")).rejects.toThrow(/WatchDir failed: HTTP 500/);
    sb.close();
  });

  it("close() is idempotent and safe to call before iterating", async () => {
    const sb = await createSandbox();
    handler = () => ({ status: 200, buffer: connectFrame(0x02, "{}") });
    const watcher = await sb.files.watchDir("/tmp");
    expect(() => {
      watcher.close();
      watcher.close();
    }).not.toThrow();
    sb.close();
  });
});
