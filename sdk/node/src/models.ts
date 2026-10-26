// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

/** Collected stdout / stderr lines from a code execution. */
export class Logs {
  stdout: string[] = [];
  stderr: string[] = [];

  toJSON(): { stdout: string[]; stderr: string[] } {
    return { stdout: this.stdout, stderr: this.stderr };
  }
}

/** Exception information captured when an execution fails. */
export class ExecutionError {
  name: string;
  value: string;
  traceback: string;

  constructor(name: string, value: string, traceback: string | string[] = "") {
    this.name = name;
    this.value = value;
    this.traceback = Array.isArray(traceback) ? traceback.join("\n") : traceback || "";
  }

  toJSON(): { name: string; value: string; traceback: string } {
    return { name: this.name, value: this.value, traceback: this.traceback };
  }
}

const RESULT_FORMAT_KEYS = [
  "text",
  "html",
  "markdown",
  "svg",
  "png",
  "jpeg",
  "pdf",
  "latex",
  "json",
  "javascript",
  "data",
  "chart",
] as const;

/**
 * A single result event from an execution (e.g. the value of the final
 * expression, or a rich display output such as an image or chart).
 */
export class Result {
  text?: string;
  html?: string;
  markdown?: string;
  svg?: string;
  png?: string;
  jpeg?: string;
  pdf?: string;
  latex?: string;
  json?: Record<string, unknown>;
  javascript?: string;
  data?: Record<string, unknown>;
  chart?: unknown;
  isMainResult = false;
  extra?: Record<string, unknown>;

  constructor(raw: Record<string, any> = {}) {
    this.text = raw.text;
    this.html = raw.html;
    this.markdown = raw.markdown;
    this.svg = raw.svg;
    this.png = raw.png;
    this.jpeg = raw.jpeg;
    this.pdf = raw.pdf;
    this.latex = raw.latex;
    this.json = raw.json ?? raw.json_data;
    this.javascript = raw.javascript;
    this.data = raw.data;
    this.chart = raw.chart;
    this.isMainResult = Boolean(raw.is_main_result ?? raw.isMainResult ?? false);
    this.extra = raw.extra;
  }

  /** Backward-compatible alias for E2B's ``json`` field. */
  get jsonData(): Record<string, unknown> | undefined {
    return this.json;
  }

  /** Names of the formats present on this result. */
  formats(): string[] {
    const formats: string[] = [];
    for (const key of RESULT_FORMAT_KEYS) {
      if ((this as Record<string, unknown>)[key]) {
        formats.push(key);
      }
    }
    if (this.extra) {
      formats.push(...Object.keys(this.extra));
    }
    return formats;
  }
}

/** The full outcome of a {@link Sandbox.runCode} call. */
export class Execution {
  results: Result[] = [];
  logs: Logs = new Logs();
  error: ExecutionError | null = null;
  executionCount: number | null = null;

  /** Text of the main result (the last expression value), if any. */
  get text(): string | undefined {
    for (const r of this.results) {
      if (r.isMainResult) {
        return r.text;
      }
    }
    return undefined;
  }

  toJSON(): Record<string, unknown> {
    return {
      results: this.results.map((r) => {
        const item: Record<string, unknown> = {};
        const record = r as unknown as Record<string, unknown>;
        for (const key of r.formats()) {
          item[key] = record[key] ?? r.extra?.[key];
        }
        item.text = r.text;
        return item;
      }),
      logs: this.logs.toJSON(),
      error: this.error ? this.error.toJSON() : null,
    };
  }
}

/** A single stdout / stderr line delivered to a streaming callback. */
export class OutputMessage {
  line: string;
  timestamp: string | number;
  error: boolean;

  constructor(line = "", timestamp: string | number = "", error = false) {
    this.line = line;
    this.timestamp = timestamp;
    this.error = error;
  }

  /** Backward-compatible alias for E2B's ``line`` field. */
  get text(): string {
    return this.line;
  }

  /** Backward-compatible alias for E2B's ``error`` field. */
  get isStderr(): boolean {
    return this.error;
  }

  toString(): string {
    return this.line;
  }
}

/** Metadata returned by snapshot-related APIs. */
export class SnapshotInfo {
  snapshotId: string;
  names: string[];

  constructor(snapshotId: string, names: string[] = []) {
    this.snapshotId = snapshotId;
    this.names = names;
  }

  static fromDict(data: Record<string, any>): SnapshotInfo {
    return new SnapshotInfo(data.snapshotID ?? "", data.names ?? []);
  }
}
