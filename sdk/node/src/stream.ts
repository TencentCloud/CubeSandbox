// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { Execution, ExecutionError, OutputMessage, Result } from "./models.js";

/** Streaming callbacks for {@link Sandbox.runCode}. */
export interface RunCodeCallbacks {
  onStdout?: (msg: OutputMessage) => void;
  onStderr?: (msg: OutputMessage) => void;
  onResult?: (result: Result) => void;
  onError?: (error: ExecutionError) => void;
}

/**
 * An idle (inactivity) timeout backed by an {@link AbortController}. The timer
 * is *reset* every time {@link IdleTimeout.reset} is called (i.e. on each chunk
 * received), so a long-running-but-active stream is never aborted — only a
 * stream that goes quiet for longer than ``timeoutMs`` is.
 *
 * This mirrors the Python SDK's httpx *read* timeout (as opposed to a hard
 * deadline over the whole request).
 */
export interface IdleTimeout {
  readonly signal: AbortSignal;
  /** Restart the inactivity timer (call whenever fresh bytes arrive). */
  reset(): void;
  /** Stop the timer for good (call when the stream is fully consumed). */
  clear(): void;
  /** True once the timer fired and aborted the signal. */
  readonly firedRef: { current: boolean };
}

/**
 * Create an {@link IdleTimeout}. When ``timeoutMs`` is falsy or non-positive the
 * returned handle is inert (never aborts) so callers can pass it unconditionally.
 */
export function createIdleTimeout(timeoutMs?: number): IdleTimeout {
  const controller = new AbortController();
  const firedRef = { current: false };

  if (!timeoutMs || timeoutMs <= 0) {
    return { signal: controller.signal, reset() {}, clear() {}, firedRef };
  }

  let timer: ReturnType<typeof setTimeout> | undefined;
  const arm = (): void => {
    timer = setTimeout(() => {
      firedRef.current = true;
      controller.abort();
    }, timeoutMs);
    // Don't let a pending idle timer keep the event loop alive on its own.
    (timer as { unref?: () => void }).unref?.();
  };
  arm();

  return {
    signal: controller.signal,
    reset(): void {
      if (timer !== undefined) {
        clearTimeout(timer);
      }
      arm();
    },
    clear(): void {
      if (timer !== undefined) {
        clearTimeout(timer);
        timer = undefined;
      }
    },
    firedRef,
  };
}

/**
 * Parse a single ndjson line emitted by the sandbox's envd process, mutating
 * ``execution`` in place and invoking the matching callback. Malformed lines
 * and unknown event types are silently skipped (mirrors the Python SDK).
 */
export function parseLine(
  execution: Execution,
  line: string,
  callbacks: RunCodeCallbacks = {},
): void {
  if (!line) {
    return;
  }

  let data: Record<string, any>;
  try {
    data = JSON.parse(line);
  } catch {
    return;
  }

  const eventType = data.type;

  switch (eventType) {
    case "result": {
      const result = new Result(data);
      execution.results.push(result);
      callbacks.onResult?.(result);
      break;
    }
    case "stdout": {
      const text = data.text ?? "";
      execution.logs.stdout.push(text);
      callbacks.onStdout?.(new OutputMessage(text, data.timestamp ?? ""));
      break;
    }
    case "stderr": {
      const text = data.text ?? "";
      execution.logs.stderr.push(text);
      callbacks.onStderr?.(new OutputMessage(text, data.timestamp ?? "", true));
      break;
    }
    case "error": {
      execution.error = new ExecutionError(
        data.name ?? "",
        data.value ?? "",
        data.traceback ?? [],
      );
      callbacks.onError?.(execution.error);
      break;
    }
    case "number_of_executions": {
      execution.executionCount = data.execution_count ?? null;
      break;
    }
    default:
      break;
  }
}

/**
 * Consume a web ``ReadableStream`` of ndjson bytes, feeding each complete line
 * to {@link parseLine}. Handles UTF-8 decoding and partial-line buffering.
 */
export async function parseNdjsonStream(
  body: ReadableStream<Uint8Array>,
  execution: Execution,
  callbacks: RunCodeCallbacks = {},
  onActivity?: () => void,
): Promise<void> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  // Accumulate decoded fragments without repeatedly reallocating a growing
  // string. We only join (and scan for newlines) when a fragment actually
  // contains one, so a large line split across many chunks costs O(n) total
  // instead of the O(n²) of `buffer += chunk` on every read.
  let pending: string[] = [];

  const flushCompleteLines = (): void => {
    let buf = pending.join("");
    let newlineIndex: number;
    while ((newlineIndex = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, newlineIndex).replace(/\r$/, "");
      buf = buf.slice(newlineIndex + 1);
      parseLine(execution, line, callbacks);
    }
    pending = buf ? [buf] : [];
  };

  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      onActivity?.();
      const text = decoder.decode(value, { stream: true });
      if (text.length === 0) {
        continue;
      }
      pending.push(text);
      if (text.indexOf("\n") >= 0) {
        flushCompleteLines();
      }
    }
  } finally {
    reader.releaseLock();
  }

  const flushed = decoder.decode();
  if (flushed) {
    pending.push(flushed);
  }
  const tail = pending.join("").replace(/\r$/, "");
  if (tail) {
    parseLine(execution, tail, callbacks);
  }
}
