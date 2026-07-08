// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { createServer, type IncomingHttpHeaders, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { Config } from "../src/config.js";
import { Sandbox } from "../src/index.js";

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
function decodeConnectPayload(raw: Buffer): Record<string, unknown> {
  expect(raw.length).toBeGreaterThanOrEqual(5);
  const flags = raw.readUInt8(0);
  const size = raw.readUInt32BE(1);
  expect(flags).toBe(0);
  expect(raw.length).toBe(5 + size);
  return JSON.parse(raw.slice(5, 5 + size).toString("utf-8"));
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

describe("Commands.run", () => {
  it("decodes base64 stdout and reports a zero exit code", async () => {
    const sb = await createSandbox();
    const stdout = Buffer.from("hello\nworld\n").toString("base64");
    handler = (req) => {
      expect(req.method).toBe("POST");
      expect(req.pathname).toBe("/process.Process/Start");
      expect(req.headers.host).toBe(`49983-${SANDBOX_ID}.${DOMAIN}`);
      expect(req.headers.authorization).toBe("Basic cm9vdDo=");
      const payload = decodeConnectPayload(req.body) as {
        process: { cmd: string; args: string[]; envs: Record<string, string>; cwd?: string };
      };
      expect(payload.process.cmd).toBe("/bin/bash");
      expect(payload.process.args).toEqual(["-l", "-c", "echo hello"]);
      expect(payload.process.envs).toEqual({ A: "B" });
      expect(payload.process.cwd).toBe("/work");
      const body = Buffer.concat([
        connectFrame(0, '{"event":{"start":{"pid":123}}}'),
        connectFrame(0, JSON.stringify({ event: { data: { stdout } } })),
        connectFrame(0, '{"event":{"end":{"exitCode":0,"exited":true}}}'),
        connectFrame(0x02, "{}"),
      ]);
      return { status: 200, buffer: body };
    };

    const result = await sb.commands.run("echo hello", { cwd: "/work", envs: { A: "B" } });
    expect(result.stdout).toBe("hello\nworld\n");
    expect(result.stderr).toBe("");
    expect(result.exitCode).toBe(0);
    sb.close();
  });

  it("decodes base64 stderr", async () => {
    const sb = await createSandbox();
    const stderr = Buffer.from("warn\nerror\n").toString("base64");
    handler = () => ({
      status: 200,
      buffer: Buffer.concat([
        connectFrame(0, JSON.stringify({ event: { data: { stderr } } })),
        connectFrame(0, '{"event":{"end":{"exitCode":0,"exited":true}}}'),
        connectFrame(0x02, "{}"),
      ]),
    });

    const result = await sb.commands.run("echo warn >&2");
    expect(result.stdout).toBe("");
    expect(result.stderr).toBe("warn\nerror\n");
    expect(result.exitCode).toBe(0);
    sb.close();
  });

  it("reports a non-zero exit code", async () => {
    const sb = await createSandbox();
    handler = () => ({
      status: 200,
      buffer: Buffer.concat([
        connectFrame(0, '{"event":{"end":{"exitCode":1,"exited":true}}}'),
        connectFrame(0x02, "{}"),
      ]),
    });
    const result = await sb.commands.run("false");
    expect(result.exitCode).toBe(1);
    sb.close();
  });

  it("propagates an error delivered on the end-of-stream frame", async () => {
    const sb = await createSandbox();
    handler = () => ({
      status: 200,
      buffer: connectFrame(
        0x02,
        '{"error":{"code":"internal","message":"boom"}}',
      ),
    });
    await expect(sb.commands.run("boom")).rejects.toThrow(/boom/);
    sb.close();
  });
});
