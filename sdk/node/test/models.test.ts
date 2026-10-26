// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";

import {
  Execution,
  ExecutionError,
  Logs,
  OutputMessage,
  Result,
  SnapshotInfo,
} from "../src/models.js";

describe("Result", () => {
  it("defaults isMainResult to false", () => {
    expect(new Result({ text: "x" }).isMainResult).toBe(false);
  });

  it("parses is_main_result from the wire payload", () => {
    expect(new Result({ text: "42", is_main_result: true }).isMainResult).toBe(true);
  });

  it("exposes jsonData as an alias for json", () => {
    const result = new Result({ json: { a: 1 }, data: { b: 2 }, chart: { type: "bar" } });
    expect(result.json).toEqual({ a: 1 });
    expect(result.jsonData).toEqual({ a: 1 });
    expect(result.data).toEqual({ b: 2 });
    expect(result.chart).toEqual({ type: "bar" });
  });

  it("accepts the legacy json_data constructor key", () => {
    expect(new Result({ json_data: { a: 1 } }).json).toEqual({ a: 1 });
  });

  it("lists only the formats that are present", () => {
    const result = new Result({ json: { a: 1 }, data: { b: 2 }, chart: { type: "bar" } });
    expect(new Set(result.formats())).toEqual(new Set(["json", "data", "chart"]));
  });

  it("includes extra keys in formats()", () => {
    const result = new Result({ text: "hi", extra: { custom: "value" } });
    expect(result.formats()).toContain("text");
    expect(result.formats()).toContain("custom");
  });
});

describe("Execution", () => {
  it("returns the text of the main result", () => {
    const ex = new Execution();
    ex.results.push(new Result({ text: "side", is_main_result: false }));
    ex.results.push(new Result({ text: "42", is_main_result: true }));
    expect(ex.text).toBe("42");
  });

  it("returns undefined when there are no results", () => {
    expect(new Execution().text).toBeUndefined();
  });

  it("returns undefined when no result is the main result", () => {
    const ex = new Execution();
    ex.results.push(new Result({ text: "x", is_main_result: false }));
    expect(ex.text).toBeUndefined();
  });

  it("starts with empty logs and no error", () => {
    const ex = new Execution();
    expect(ex.logs.stdout).toEqual([]);
    expect(ex.logs.stderr).toEqual([]);
    expect(ex.error).toBeNull();
    expect(ex.executionCount).toBeNull();
  });
});

describe("ExecutionError", () => {
  it("joins a traceback array into a newline-separated string", () => {
    const err = new ExecutionError("ValueError", "bad", ["line1", "line2"]);
    expect(err.traceback).toBe("line1\nline2");
  });

  it("defaults the traceback to an empty string", () => {
    expect(new ExecutionError("e", "v").traceback).toBe("");
  });
});

describe("Logs", () => {
  it("serializes stdout and stderr via toJSON", () => {
    const logs = new Logs();
    logs.stdout.push("a");
    logs.stderr.push("b");
    expect(logs.toJSON()).toEqual({ stdout: ["a"], stderr: ["b"] });
  });
});

describe("OutputMessage", () => {
  it("exposes text and isStderr aliases", () => {
    const msg = new OutputMessage("hello\n", 123, true);
    expect(msg.line).toBe("hello\n");
    expect(msg.text).toBe("hello\n");
    expect(msg.error).toBe(true);
    expect(msg.isStderr).toBe(true);
    expect(String(msg)).toBe("hello\n");
  });

  it("defaults to a non-error stdout message", () => {
    const msg = new OutputMessage("out\n");
    expect(msg.isStderr).toBe(false);
  });
});

describe("SnapshotInfo", () => {
  it("parses snapshotID and names from a wire dict", () => {
    const info = SnapshotInfo.fromDict({ snapshotID: "snap-1", names: ["a", "b"] });
    expect(info.snapshotId).toBe("snap-1");
    expect(info.names).toEqual(["a", "b"]);
  });

  it("defaults names to an empty array", () => {
    const info = SnapshotInfo.fromDict({ snapshotID: "snap-2" });
    expect(info.snapshotId).toBe("snap-2");
    expect(info.names).toEqual([]);
  });
});
