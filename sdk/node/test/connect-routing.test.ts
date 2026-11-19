// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Misrouted Connect streams: an HTML page (or another non-envelope body) must
// be reported as a routing problem, not as an oversized Connect message, and
// valid Connect streams must keep parsing.

import { createServer, type ServerResponse, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { gzipSync } from "node:zlib";

import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { encodeConnectEnvelope, readConnectFrames } from "../src/commands.js";
import { Config } from "../src/config.js";
import { CubeSandboxError, Sandbox } from "../src/index.js";

const SANDBOX_DATA = {
  sandboxID: "sb-routing",
  templateID: "tpl-test",
  domain: "cube.app",
  state: "running",
};
const HTML_PAGE = "<!DOCTYPE html><html><body>Cube Sandbox Web UI</body></html>";

interface Reply {
  status: number;
  body: Buffer | string;
  contentType?: string | null;
  /** Write the body a few bytes at a time so envelope headers span chunks. */
  chunked?: boolean;
}

let server: Server;
let port: number;
let reply: Reply = { status: 200, body: HTML_PAGE };

async function send(res: ServerResponse, r: Reply): Promise<void> {
  const headers = r.contentType ? { "content-type": r.contentType } : {};
  res.writeHead(r.status, headers);
  const body = Buffer.from(r.body);
  if (!r.chunked) {
    res.end(body);
    return;
  }
  for (let i = 0; i < body.length; i += 3) {
    res.write(body.subarray(i, i + 3));
    await new Promise((resolve) => setImmediate(resolve));
  }
  res.end();
}

beforeAll(async () => {
  server = createServer((req, res) => {
    req.resume();
    req.on("end", () => {
      if (req.url?.startsWith("/sandboxes")) {
        res.writeHead(201, { "content-type": "application/json" });
        res.end(JSON.stringify(SANDBOX_DATA));
        return;
      }
      void send(res, reply);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, resolve));
  port = (server.address() as AddressInfo).port;
});

afterAll(() => {
  server.close();
});

async function createSandbox(next: Reply): Promise<Sandbox> {
  reply = next;
  return Sandbox.create({
    config: new Config({
      apiUrl: `http://127.0.0.1:${port}`,
      templateId: "tpl-test",
      proxyNodeIp: "127.0.0.1",
      proxyPort: port,
      sandboxDomain: "cube.app",
    }),
  });
}

function html(contentType: string | null = "text/html; charset=utf-8"): Reply {
  return { status: 200, body: HTML_PAGE, contentType };
}

function frame(flags: number, payload: unknown): Buffer {
  return encodeConnectEnvelope(Buffer.from(JSON.stringify(payload)), flags);
}

function dataEvent(fields: Record<string, string>): Buffer {
  const data = Object.fromEntries(
    Object.entries(fields).map(([k, v]) => [k, Buffer.from(v).toString("base64")]),
  );
  return frame(0, { event: { data } });
}

function streamOf(data: Buffer): ReadableStream<Uint8Array> {
  return new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(new Uint8Array(data));
      controller.close();
    },
  });
}

type Target = "run" | "watch" | "create" | "connect";
const TARGETS: Target[] = ["run", "watch", "create", "connect"];

async function call(sb: Sandbox, target: Target): Promise<unknown> {
  switch (target) {
    case "run":
      return sb.commands.run("echo hi");
    case "watch":
      return sb.files.watchDir("/tmp");
    case "create":
      return sb.pty.create({ rows: 24, cols: 80 });
    case "connect":
      return sb.pty.connect(123);
  }
}

async function errorOf(sb: Sandbox, target: Target): Promise<Error> {
  const err = await call(sb, target).then(
    () => null,
    (e: unknown) => e,
  );
  expect(err).toBeInstanceOf(Error);
  return err as Error;
}

describe("readConnectFrames", () => {
  it.each([
    ["html", Buffer.from(HTML_PAGE)],
    ["bom", Buffer.concat([Buffer.from([0xef, 0xbb, 0xbf]), Buffer.from(HTML_PAGE)])],
    ["leading newline", Buffer.from(`\n${HTML_PAGE}`)],
    ["json", Buffer.from('{"error":{"code":"unavailable","message":"no upstream"}}')],
    ["gzip", gzipSync(Buffer.from("payload"))],
  ])("rejects a %s body before reading the length", async (_label, body) => {
    // "<!DOC" used to be read as flags 0x3c and a 558124867-byte length.
    const frames = readConnectFrames(streamOf(body));
    await expect(frames.next()).rejects.toThrow(
      `response is not a Connect envelope (first 5 bytes: ${body.subarray(0, 5).toString("hex")})`,
    );
  });

  it.each([0x00, 0x01, 0x02, 0x03])("accepts Connect flags %s", async (flags) => {
    const frames = readConnectFrames(streamOf(encodeConnectEnvelope(Buffer.from("{}"), flags)));
    const first = await frames.next();
    expect(first.value).toMatchObject({ flags });
  });
});

describe("valid Connect streams", () => {
  it.each([false, true])("commands.run parses a multi-frame stream (chunked: %s)", async (chunked) => {
    const sb = await createSandbox({
      status: 200,
      contentType: "application/connect+json",
      chunked,
      body: Buffer.concat([
        frame(0, { event: { start: { pid: 123 } } }),
        dataEvent({ stdout: "hello\n" }),
        dataEvent({ stderr: "warn\n" }),
        dataEvent({ stdout: "world\n" }),
        frame(0, { event: { end: { exitCode: 3, exited: true } } }),
        frame(0x02, {}),
      ]),
    });
    const result = await sb.commands.run("echo hello");
    expect(result).toMatchObject({ stdout: "hello\nworld\n", stderr: "warn\n", exitCode: 3 });
    sb.close();
  });

  it.each([
    ["create", false],
    ["create", true],
    ["connect", false],
    ["connect", true],
  ] as const)("pty.%s parses a stream (chunked: %s)", async (target, chunked) => {
    const sb = await createSandbox({
      status: 200,
      contentType: "application/connect+json",
      chunked,
      body: Buffer.concat([
        frame(0, { event: { start: { pid: 123 } } }),
        dataEvent({ pty: "$ " }),
        dataEvent({ pty: "ls\r\n" }),
        frame(0, { event: { end: { exitCode: 0, exited: true } } }),
        frame(0x02, {}),
      ]),
    });
    const handle =
      target === "create" ? await sb.pty.create({ rows: 24, cols: 80 }) : await sb.pty.connect(123);
    const output: Uint8Array[] = [];
    for await (const chunk of handle) {
      output.push(chunk);
    }
    expect(handle.pid).toBe(123);
    expect(Buffer.concat(output).toString()).toBe("$ ls\r\n");
    expect(handle.exitCode).toBe(0);
    sb.close();
  });
});

describe("HTTP 200 text/html responses", () => {
  it("commands.run reports the routing problem", async () => {
    const sb = await createSandbox(html());
    await expect(sb.commands.run("echo hi")).rejects.toThrow(
      /^received HTML instead of a Connect response \(content-type "text\/html; charset=utf-8"\); check whether CUBE_PROXY_NODE_IP is configured/,
    );
  });

  it("files.watchDir reports the routing problem", async () => {
    const sb = await createSandbox(html("text/html"));
    await expect(sb.files.watchDir("/tmp")).rejects.toThrow(
      /^WatchDir failed: received HTML instead of a Connect response/,
    );
  });

  it.each(["create", "connect"] as const)("pty.%s reports the routing problem", async (target) => {
    const sb = await createSandbox(html("text/html"));
    const err = await errorOf(sb, target);
    expect(err).toBeInstanceOf(CubeSandboxError);
    expect(err.message).toMatch(/failed: received HTML instead of a Connect response/);
  });
});

describe("non-envelope bodies without an HTML content type", () => {
  it.each([null, "application/connect+json"])(
    "commands.run reports a non-envelope response (content-type %s)",
    async (contentType) => {
      const sb = await createSandbox(html(contentType));
      const err = await errorOf(sb, "run");
      expect(err.message).toMatch(/not a Connect envelope/);
      expect(err.message).not.toMatch(/too large/);
    },
  );

  it("watcher iteration reports a non-envelope response", async () => {
    const sb = await createSandbox(html(null));
    const watcher = await sb.files.watchDir("/tmp");
    const drain = async () => {
      for await (const _ of watcher) {
        // no events expected
      }
    };
    await expect(drain()).rejects.toThrow(/not a Connect envelope/);
  });

  it.each(["create", "connect"] as const)("pty.%s reports a non-envelope response", async (target) => {
    const sb = await createSandbox(html(null));
    const err = await errorOf(sb, target);
    expect(err.message).toMatch(/not a Connect envelope/);
  });
});

describe("HTTP error pages", () => {
  const PAGE = "<html><body>404 Not Found</body></html>";

  describe.each(TARGETS)("%s", (target) => {
    it.each([
      [404, "CUBE_PROXY_NODE_IP"],
      [502, "upstream envd or sandbox node is unreachable"],
      [401, "check credentials"],
    ])("an HTML %s page carries the routing hint", async (status, hint) => {
      const sb = await createSandbox({ status, body: PAGE, contentType: "text/html" });
      const err = await errorOf(sb, target);
      expect(err.message).toContain(`HTTP ${status}: ${PAGE}; `);
      expect(err.message).toContain(hint);
    });

    it("an HTML page without a content type carries the routing hint", async () => {
      const sb = await createSandbox({ status: 404, body: "\n<!DOCTYPE html><html></html>" });
      const err = await errorOf(sb, target);
      expect(err.message).toContain("CUBE_PROXY_NODE_IP");
    });

    it("a large HTML page is truncated", async () => {
      const body = `<!DOCTYPE html>${"x".repeat(100_000)}`;
      const sb = await createSandbox({ status: 502, body, contentType: "text/html" });
      const err = await errorOf(sb, target);
      expect(err.message).not.toContain("x".repeat(300));
      expect(err.message).toContain("...; received an HTML error page");
    });

    it("a JSON error document keeps the server's message", async () => {
      const body = '{"code":"not_found","message":"process not found"}';
      const sb = await createSandbox({ status: 404, body, contentType: "text/html" });
      const err = await errorOf(sb, target);
      expect(err.message).toMatch(/HTTP 404: \{"code":"not_found","message":"process not found"\}$/);
    });

    it("a plain-text error has no routing hint", async () => {
      const sb = await createSandbox({ status: 500, body: "envd crashed", contentType: "text/plain" });
      const err = await errorOf(sb, target);
      expect(err.message).toMatch(/HTTP 500: envd crashed$/);
    });
  });
});
