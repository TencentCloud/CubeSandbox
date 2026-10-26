// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import {
  createServer,
  type IncomingHttpHeaders,
  type Server,
  type ServerResponse,
} from "node:http";
import type { AddressInfo } from "node:net";

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { Config } from "../src/config.js";
import { Sandbox, type PtyOutput } from "../src/index.js";

const SANDBOX_ID = "sb-pty-001";
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
  json?: unknown;
  buffer?: Buffer;
  headers?: Record<string, string>;
}

type Handler = (req: RecordedRequest) => MockResponse;

let server: Server;
let port: number;
let requests: RecordedRequest[] = [];
let handler: Handler = () => ({ status: 201, json: SANDBOX_DATA });

/** Build a single Connect protocol envelope: [flags:1][len:4 BE][payload]. */
function connectFrame(flags: number, payload: string): Buffer {
  const raw = Buffer.from(payload, "utf-8");
  const header = Buffer.alloc(5);
  header.writeUInt8(flags, 0);
  header.writeUInt32BE(raw.length, 1);
  return Buffer.concat([header, raw]);
}

/** Decode the JSON payload out of a data (flags=0) Connect envelope. */
function decodeConnectPayload(raw: Buffer): Record<string, any> {
  const flags = raw.readUInt8(0);
  const size = raw.readUInt32BE(1);
  expect(flags).toBe(0);
  expect(raw.length).toBe(5 + size);
  return JSON.parse(raw.slice(5, 5 + size).toString("utf-8"));
}

function b64(text: string): string {
  return Buffer.from(text, "utf-8").toString("base64");
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
  handler = () => ({ status: 201, json: SANDBOX_DATA });
  const sb = await Sandbox.create({ config: makeConfig() });
  requests = [];
  return sb;
}

beforeAll(async () => {
  server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const url = new URL(req.url ?? "/", "http://localhost");
      const record: RecordedRequest = {
        method: req.method ?? "",
        pathname: url.pathname,
        headers: req.headers,
        body: Buffer.concat(chunks),
      };
      requests.push(record);
      const out = handler(record);
      const headers: Record<string, string> = { ...(out.headers ?? {}) };
      let payload: string | Buffer = "";
      if (out.buffer !== undefined) {
        payload = out.buffer;
      } else if (out.json !== undefined) {
        payload = JSON.stringify(out.json);
        headers["content-type"] ??= "application/json";
      }
      res.writeHead(out.status ?? 200, headers);
      res.end(payload);
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

describe("Pty.create", () => {
  it("streams output, resolves pid, and returns the exit code", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.method).toBe("POST");
      expect(req.pathname).toBe("/process.Process/Start");
      expect(req.headers.host).toBe(`49983-${SANDBOX_ID}.${DOMAIN}`);
      expect(req.headers["content-type"]).toBe("application/connect+json");
      expect(req.headers["connect-timeout-ms"]).toBe("60000");
      const payload = decodeConnectPayload(req.body);
      expect(payload.process.cmd).toBe("/bin/bash");
      expect(payload.process.args).toEqual(["-i", "-l"]);
      expect(payload.process.envs).toMatchObject({
        TERM: "xterm-256color",
        LANG: "C.UTF-8",
        LC_ALL: "C.UTF-8",
      });
      expect(payload.pty.size).toEqual({ rows: 24, cols: 80 });
      return {
        status: 200,
        buffer: Buffer.concat([
          connectFrame(0, '{"event":{"start":{"pid":4242}}}'),
          connectFrame(0, JSON.stringify({ event: { data: { pty: b64("hi> ") } } })),
          connectFrame(0, JSON.stringify({ event: { data: { pty: b64("done\n") } } })),
          connectFrame(0, '{"event":{"end":{"exitCode":0,"exited":true}}}'),
          connectFrame(0x02, "{}"),
        ]),
      };
    };

    const handle = await sb.pty.create({ rows: 24, cols: 80 });
    expect(handle.pid).toBe(4242);

    const collected: PtyOutput[] = [];
    const code = await handle.wait((chunk) => collected.push(chunk));
    expect(code).toBe(0);
    expect(handle.exitCode).toBe(0);
    expect(Buffer.concat(collected.map((c) => Buffer.from(c))).toString("utf-8")).toBe("hi> done\n");
    sb.close();
  });

  it("merges caller envs/cwd and lets the caller override TERM", async () => {
    const sb = await createSandbox();
    let seen: Record<string, any> = {};
    handler = (req) => {
      seen = decodeConnectPayload(req.body);
      return {
        status: 200,
        buffer: Buffer.concat([
          connectFrame(0, '{"event":{"start":{"pid":7}}}'),
          connectFrame(0, '{"event":{"end":{"exitCode":0,"exited":true}}}'),
          connectFrame(0x02, "{}"),
        ]),
      };
    };

    const handle = await sb.pty.create(
      { rows: 40, cols: 120 },
      { cwd: "/work", envs: { TERM: "dumb", FOO: "bar" } },
    );
    await handle.wait();
    expect(seen.process.cwd).toBe("/work");
    expect(seen.process.envs).toMatchObject({ TERM: "dumb", FOO: "bar", LANG: "C.UTF-8" });
    sb.close();
  });

  it("propagates an error delivered on the end-of-stream frame", async () => {
    const sb = await createSandbox();
    handler = () => ({
      status: 200,
      buffer: Buffer.concat([
        connectFrame(0, '{"event":{"start":{"pid":9}}}'),
        connectFrame(0x02, '{"error":{"code":"internal","message":"pty boom"}}'),
      ]),
    });
    const handle = await sb.pty.create({ rows: 24, cols: 80 });
    await expect(handle.wait()).rejects.toThrow(/pty boom/);
    sb.close();
  });

  it("raises when the stream closes before a start event", async () => {
    const sb = await createSandbox();
    handler = () => ({ status: 200, buffer: connectFrame(0x02, "{}") });
    await expect(sb.pty.create({ rows: 24, cols: 80 })).rejects.toThrow(
      /stream closed before start event/,
    );
    sb.close();
  });

  it("maps an HTTP error to ApiError", async () => {
    const sb = await createSandbox();
    handler = () => ({ status: 503, json: { message: "unavailable" } });
    await expect(sb.pty.create({ rows: 24, cols: 80 })).rejects.toThrow(/HTTP 503/);
    sb.close();
  });
});

describe("Pty.connect", () => {
  it("reattaches to an existing PTY by pid", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.pathname).toBe("/process.Process/Connect");
      const payload = decodeConnectPayload(req.body);
      expect(payload.process.pid).toBe(555);
      return {
        status: 200,
        buffer: Buffer.concat([
          connectFrame(0, '{"event":{"start":{"pid":555}}}'),
          connectFrame(0, '{"event":{"end":{"exit_code":3,"exited":true}}}'),
          connectFrame(0x02, "{}"),
        ]),
      };
    };
    const handle = await sb.pty.connect(555);
    expect(handle.pid).toBe(555);
    expect(await handle.wait()).toBe(3);
    sb.close();
  });
});

describe("Pty selector RPCs", () => {
  it("kill sends SIGKILL via unary SendSignal and returns true", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.pathname).toBe("/process.Process/SendSignal");
      expect(req.headers["content-type"]).toBe("application/json");
      expect(JSON.parse(req.body.toString("utf-8"))).toEqual({
        process: { pid: 12 },
        signal: "SIGNAL_SIGKILL",
      });
      return { status: 200, json: {} };
    };
    expect(await sb.pty.kill(12)).toBe(true);
    sb.close();
  });

  it("kill returns false on HTTP 404", async () => {
    const sb = await createSandbox();
    handler = () => ({ status: 404, json: { code: "not_found" } });
    expect(await sb.pty.kill(99)).toBe(false);
    sb.close();
  });

  it("kill returns false on a Connect not_found body", async () => {
    const sb = await createSandbox();
    handler = () => ({ status: 400, json: { code: "not_found", message: "gone" } });
    expect(await sb.pty.kill(99)).toBe(false);
    sb.close();
  });

  it("sendStdin base64-encodes input via unary SendInput", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.pathname).toBe("/process.Process/SendInput");
      expect(JSON.parse(req.body.toString("utf-8"))).toEqual({
        process: { pid: 5 },
        input: { pty: b64("ls -la\n") },
      });
      return { status: 200, json: {} };
    };
    await sb.pty.sendStdin(5, "ls -la\n");
    sb.close();
  });

  it("resize sends rows/cols via unary Update", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.pathname).toBe("/process.Process/Update");
      expect(JSON.parse(req.body.toString("utf-8"))).toEqual({
        process: { pid: 8 },
        pty: { size: { rows: 50, cols: 132 } },
      });
      return { status: 200, json: {} };
    };
    await sb.pty.resize(8, { rows: 50, cols: 132 });
    sb.close();
  });
});

describe("PtyHandle control delegates to the resolved pid", () => {
  it("routes kill/sendStdin/resize to the started pid", async () => {
    const sb = await createSandbox();
    let phase: "start" | "control" = "start";
    const controlPaths: string[] = [];
    const controlPids: number[] = [];
    handler = (req) => {
      if (phase === "start") {
        phase = "control";
        return {
          status: 200,
          buffer: Buffer.concat([
            connectFrame(0, '{"event":{"start":{"pid":314}}}'),
            connectFrame(0, JSON.stringify({ event: { data: { pty: b64("x") } } })),
            connectFrame(0, '{"event":{"end":{"exitCode":0,"exited":true}}}'),
            connectFrame(0x02, "{}"),
          ]),
        };
      }
      controlPaths.push(req.pathname);
      controlPids.push(JSON.parse(req.body.toString("utf-8")).process.pid);
      return { status: 200, json: {} };
    };

    const handle = await sb.pty.create({ rows: 24, cols: 80 });
    await handle.sendStdin(Buffer.from("echo\n"));
    await handle.resize({ rows: 10, cols: 10 });
    expect(await handle.kill()).toBe(true);

    expect(controlPaths).toEqual([
      "/process.Process/SendInput",
      "/process.Process/Update",
      "/process.Process/SendSignal",
    ]);
    expect(controlPids).toEqual([314, 314, 314]);
    sb.close();
  });
});

describe("PtyHandle.disconnect", () => {
  it("stops iterating gracefully without an end event", async () => {
    // A dedicated server that emits start + one data frame then holds the
    // socket open, so we can exercise disconnect() mid-stream.
    let heldRes: ServerResponse | undefined;
    const streamServer = createServer((req, res) => {
      req.on("data", () => {});
      req.on("end", () => {
        if (req.url === "/sandboxes") {
          res.writeHead(201, { "content-type": "application/json" });
          res.end(JSON.stringify(SANDBOX_DATA));
          return;
        }
        res.writeHead(200);
        res.write(connectFrame(0, '{"event":{"start":{"pid":21}}}'));
        res.write(connectFrame(0, JSON.stringify({ event: { data: { pty: b64("prompt$ ") } } })));
        heldRes = res; // never ended — held open until we disconnect
      });
    });
    await new Promise<void>((resolve) => streamServer.listen(0, resolve));
    const sp = (streamServer.address() as AddressInfo).port;

    const sb = await Sandbox.create({
      config: new Config({
        apiUrl: `http://127.0.0.1:${sp}`,
        templateId: "tpl-test",
        proxyNodeIp: "127.0.0.1",
        proxyPort: sp,
        sandboxDomain: DOMAIN,
      }),
    });

    try {
      const handle = await sb.pty.create({ rows: 24, cols: 80 });
      const it = handle[Symbol.asyncIterator]();
      const first = await it.next();
      expect(first.done).toBe(false);
      expect(Buffer.from(first.value as PtyOutput).toString("utf-8")).toBe("prompt$ ");

      handle.disconnect();
      const next = await it.next();
      expect(next.done).toBe(true);
    } finally {
      heldRes?.destroy();
      sb.close();
      await new Promise<void>((resolve) => streamServer.close(() => resolve()));
    }
  });
});
