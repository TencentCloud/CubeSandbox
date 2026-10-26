// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// Idle- (read-) timeout behavior for runCode / commands.run: the timer resets
// on every chunk, so a slow-but-active stream survives, while a stalled stream
// aborts after `timeoutMs` of silence.

import { createServer, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { encodeConnectEnvelope } from "../src/commands.js";
import { Config } from "../src/config.js";
import { Sandbox } from "../src/index.js";

const SANDBOX_ID = "sb-timeout";
const DOMAIN = "cube.app";
const SANDBOX_DATA = { sandboxID: SANDBOX_ID, templateID: "tpl-t", domain: DOMAIN };

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

interface StreamPlan {
  chunks: string[];
  gapMs: number;
  stall: boolean;
}

let server: Server;
let port: number;
let plan: StreamPlan = { chunks: [], gapMs: 0, stall: false };
const openResponses = new Set<ServerResponse>();

function connectDataFrame(text: string): string {
  const payload = JSON.stringify({
    event: { data: { stdout: Buffer.from(text, "utf-8").toString("base64") } },
  });
  return encodeConnectEnvelope(Buffer.from(payload, "utf-8")).toString("binary");
}

function connectEndFrame(exitCode: number): string {
  const payload = JSON.stringify({ event: { end: { exitCode } } });
  return encodeConnectEnvelope(Buffer.from(payload, "utf-8")).toString("binary");
}

async function streamPlan(res: ServerResponse): Promise<void> {
  openResponses.add(res);
  res.writeHead(200);
  for (const c of plan.chunks) {
    if (res.writableEnded || res.destroyed) return;
    res.write(Buffer.from(c, "binary"));
    if (plan.gapMs > 0) await sleep(plan.gapMs);
  }
  if (!plan.stall && !res.destroyed) {
    res.end();
  }
  // When stalling we intentionally leave the response open; the client's idle
  // timeout should abort it. afterEach closes anything left dangling.
}

beforeAll(async () => {
  server = createServer((req, res) => {
    req.resume();
    req.on("end", () => {
      const path = (req.url ?? "").split("?")[0];
      if (path === "/sandboxes") {
        res.writeHead(201, { "content-type": "application/json" });
        res.end(JSON.stringify(SANDBOX_DATA));
        return;
      }
      // /execute (ndjson) and /process.Process/Start (connect frames) both
      // just replay the configured stream plan.
      void streamPlan(res);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  port = (server.address() as AddressInfo).port;
});

afterEach(() => {
  for (const res of openResponses) {
    if (!res.writableEnded) res.destroy();
  }
  openResponses.clear();
});

afterAll(() => {
  server.close();
});

function config(): Config {
  return new Config({
    apiUrl: `http://127.0.0.1:${port}`,
    templateId: "tpl-t",
    proxyNodeIp: "127.0.0.1",
    proxyPort: port,
    sandboxDomain: DOMAIN,
  });
}

async function makeSandbox(): Promise<Sandbox> {
  return Sandbox.create({ config: config() });
}

describe("runCode idle timeout", () => {
  it("does NOT abort a slow stream whose total time exceeds timeoutMs", async () => {
    const sb = await makeSandbox();
    // 5 chunks, 40ms apart → ~200ms total, but each gap (40ms) < 100ms idle.
    plan = {
      chunks: [
        '{"type":"stdout","text":"a\\n"}\n',
        '{"type":"stdout","text":"b\\n"}\n',
        '{"type":"stdout","text":"c\\n"}\n',
        '{"type":"stdout","text":"d\\n"}\n',
        '{"type":"result","text":"done","is_main_result":true}\n',
      ],
      gapMs: 40,
      stall: false,
    };
    const execution = await sb.runCode("slow()", { timeoutMs: 100 });
    expect(execution.text).toBe("done");
    expect(execution.logs.stdout).toEqual(["a\n", "b\n", "c\n", "d\n"]);
    sb.close();
  });

  it("aborts with an inactivity error when the stream stalls", async () => {
    const sb = await makeSandbox();
    plan = {
      chunks: ['{"type":"stdout","text":"hi\\n"}\n'],
      gapMs: 0,
      stall: true, // never ends → idle timer fires
    };
    await expect(sb.runCode("hang()", { timeoutMs: 60 })).rejects.toThrow(/inactivity/);
    sb.close();
  });
});

describe("commands.run idle timeout", () => {
  it("aborts with an inactivity error when the process stream stalls", async () => {
    const sb = await makeSandbox();
    plan = {
      chunks: [connectDataFrame("partial output")],
      gapMs: 0,
      stall: true,
    };
    await expect(sb.commands.run("hang", { timeoutMs: 60 })).rejects.toThrow(/inactivity/);
    sb.close();
  });

  it("completes a slow-but-active process stream past timeoutMs", async () => {
    const sb = await makeSandbox();
    plan = {
      chunks: [
        connectDataFrame("one\n"),
        connectDataFrame("two\n"),
        connectDataFrame("three\n"),
        connectEndFrame(0),
      ],
      gapMs: 40, // < 100ms idle, but ~160ms total
      stall: false,
    };
    const result = await sb.commands.run("slow", { timeoutMs: 100 });
    expect(result.exitCode).toBe(0);
    expect(result.stdout).toBe("one\ntwo\nthree\n");
    sb.close();
  });
});
