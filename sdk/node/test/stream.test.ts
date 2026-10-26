// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";

import { Execution } from "../src/models.js";
import { parseLine, parseNdjsonStream } from "../src/stream.js";

function streamFromChunks(chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) {
        controller.enqueue(encoder.encode(chunk));
      }
      controller.close();
    },
  });
}

describe("parseLine", () => {
  it("parses a result event and captures the main result text", () => {
    const ex = new Execution();
    parseLine(ex, '{"type":"result","text":"2","is_main_result":true}');
    expect(ex.text).toBe("2");
    expect(ex.results).toHaveLength(1);
  });

  it("parses e2b-style rich result fields", () => {
    const ex = new Execution();
    parseLine(
      ex,
      '{"type":"result","json":{"a":1},"data":{"b":2},"chart":{"type":"bar"},"is_main_result":true}',
    );
    const result = ex.results[0];
    expect(result.json).toEqual({ a: 1 });
    expect(result.jsonData).toEqual({ a: 1 });
    expect(result.data).toEqual({ b: 2 });
    expect(result.chart).toEqual({ type: "bar" });
  });

  it("parses stdout events", () => {
    const ex = new Execution();
    parseLine(ex, '{"type":"stdout","text":"hello\\n","timestamp":"t1"}');
    expect(ex.logs.stdout).toEqual(["hello\n"]);
  });

  it("parses stderr events", () => {
    const ex = new Execution();
    parseLine(ex, '{"type":"stderr","text":"warn\\n","timestamp":"t1"}');
    expect(ex.logs.stderr).toEqual(["warn\n"]);
  });

  it("parses error events", () => {
    const ex = new Execution();
    parseLine(ex, '{"type":"error","name":"ValueError","value":"bad","traceback":["l1","l2"]}');
    expect(ex.error?.name).toBe("ValueError");
    expect(ex.error?.value).toBe("bad");
    expect(ex.error?.traceback).toBe("l1\nl2");
  });

  it("parses the execution count", () => {
    const ex = new Execution();
    parseLine(ex, '{"type":"number_of_executions","execution_count":5}');
    expect(ex.executionCount).toBe(5);
  });

  it("ignores malformed JSON", () => {
    const ex = new Execution();
    parseLine(ex, "not json at all");
    expect(ex.results).toEqual([]);
    expect(ex.error).toBeNull();
  });

  it("ignores empty lines", () => {
    const ex = new Execution();
    parseLine(ex, "");
    expect(ex.results).toEqual([]);
  });

  it("ignores unknown event types", () => {
    const ex = new Execution();
    parseLine(ex, '{"type":"unknown_event","data":"x"}');
    expect(ex.results).toEqual([]);
  });

  it("invokes the stdout callback", () => {
    const ex = new Execution();
    const seen: Array<[string, boolean]> = [];
    parseLine(ex, '{"type":"stdout","text":"hi\\n"}', {
      onStdout: (m) => seen.push([m.text, m.isStderr]),
    });
    expect(seen).toEqual([["hi\n", false]]);
  });

  it("invokes the stderr callback with isStderr=true", () => {
    const ex = new Execution();
    const seen: Array<[string, boolean]> = [];
    parseLine(ex, '{"type":"stderr","text":"warn\\n"}', {
      onStderr: (m) => seen.push([m.text, m.isStderr]),
    });
    expect(seen).toEqual([["warn\n", true]]);
  });

  it("invokes the result and error callbacks", () => {
    const ex = new Execution();
    const results: Array<string | undefined> = [];
    const errors: string[] = [];
    parseLine(ex, '{"type":"result","text":"42","is_main_result":true}', {
      onResult: (r) => results.push(r.text),
    });
    parseLine(ex, '{"type":"error","name":"Err","value":"v","traceback":[]}', {
      onError: (e) => errors.push(e.name),
    });
    expect(results).toEqual(["42"]);
    expect(errors).toEqual(["Err"]);
  });
});

describe("parseNdjsonStream", () => {
  it("assembles events across chunk boundaries", async () => {
    const ex = new Execution();
    const chunks = [
      '{"type":"stdout","text":"hel',
      'lo\\n"}\n{"type":"resul',
      't","text":"5","is_main_result":true}\n',
    ];
    await parseNdjsonStream(streamFromChunks(chunks), ex);
    expect(ex.logs.stdout).toEqual(["hello\n"]);
    expect(ex.text).toBe("5");
  });

  it("parses a trailing line that lacks a newline terminator", async () => {
    const ex = new Execution();
    await parseNdjsonStream(
      streamFromChunks(['{"type":"result","text":"9","is_main_result":true}']),
      ex,
    );
    expect(ex.text).toBe("9");
  });

  it("skips malformed lines but keeps valid ones", async () => {
    const ex = new Execution();
    const chunks = [
      '{"type":"stdout","text":"a\\n"}\n',
      "this is not json\n",
      '{"type":"stdout","text":"b\\n"}\n',
    ];
    await parseNdjsonStream(streamFromChunks(chunks), ex);
    expect(ex.logs.stdout).toEqual(["a\n", "b\n"]);
  });

  it("forwards callbacks while streaming", async () => {
    const ex = new Execution();
    const stdout: string[] = [];
    await parseNdjsonStream(
      streamFromChunks([
        '{"type":"stdout","text":"one\\n"}\n{"type":"stdout","text":"two\\n"}\n',
      ]),
      ex,
      { onStdout: (m) => stdout.push(m.text) },
    );
    expect(stdout).toEqual(["one\n", "two\n"]);
  });

  it("reassembles a single large line split across many chunks", async () => {
    const ex = new Execution();
    const big = "x".repeat(200_000);
    const line = `{"type":"result","text":"${big}","is_main_result":true}`;
    // Feed the line one byte-ish slice at a time, with the terminating newline
    // only in the final chunk — exercises the no-newline fast path.
    const chunks: string[] = [];
    for (let i = 0; i < line.length; i += 4096) {
      chunks.push(line.slice(i, i + 4096));
    }
    chunks.push("\n");
    await parseNdjsonStream(streamFromChunks(chunks), ex);
    expect(ex.text).toBe(big);
  });
});
