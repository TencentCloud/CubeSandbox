// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";

import {
  MAX_CONNECT_ENVELOPE_SIZE,
  encodeConnectEnvelope,
  readConnectFrames,
} from "../src/commands.js";

/** A ReadableStream that emits ``data`` in fixed-size slices. */
function streamOf(data: Buffer, chunkSize: number): ReadableStream<Uint8Array> {
  let offset = 0;
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (offset >= data.length) {
        controller.close();
        return;
      }
      const end = Math.min(offset + chunkSize, data.length);
      controller.enqueue(new Uint8Array(data.subarray(offset, end)));
      offset = end;
    },
  });
}

async function collect(
  stream: ReadableStream<Uint8Array>,
): Promise<{ flags: number; payload: Buffer }[]> {
  const frames: { flags: number; payload: Buffer }[] = [];
  for await (const frame of readConnectFrames(stream)) {
    // Copy the payload out: it may be a view into an internal chunk.
    frames.push({ flags: frame.flags, payload: Buffer.from(frame.payload) });
  }
  return frames;
}

describe("readConnectFrames", () => {
  it("reassembles frames when headers and payloads are split across chunks", async () => {
    const data = Buffer.concat([
      encodeConnectEnvelope(Buffer.from("first"), 0),
      encodeConnectEnvelope(Buffer.from("second"), 0),
      encodeConnectEnvelope(Buffer.alloc(0), 0x02),
    ]);

    // One byte at a time forces every header/payload boundary to span chunks.
    const frames = await collect(streamOf(data, 1));

    expect(frames).toHaveLength(3);
    expect(frames[0]).toEqual({ flags: 0, payload: Buffer.from("first") });
    expect(frames[1]).toEqual({ flags: 0, payload: Buffer.from("second") });
    expect(frames[2].flags).toBe(0x02);
    expect(frames[2].payload).toHaveLength(0);
  });

  it("reassembles a single large frame delivered in many small chunks", async () => {
    const big = Buffer.alloc(256 * 1024);
    for (let i = 0; i < big.length; i++) {
      big[i] = i % 251;
    }
    const data = encodeConnectEnvelope(big, 0);

    const frames = await collect(streamOf(data, 1024));

    expect(frames).toHaveLength(1);
    expect(frames[0].flags).toBe(0);
    expect(frames[0].payload.equals(big)).toBe(true);
  });

  it("handles multiple whole frames arriving in a single chunk", async () => {
    const data = Buffer.concat([
      encodeConnectEnvelope(Buffer.from("a"), 0),
      encodeConnectEnvelope(Buffer.from("bb"), 0),
      encodeConnectEnvelope(Buffer.from("ccc"), 0),
    ]);

    const frames = await collect(streamOf(data, data.length));

    expect(frames.map((f) => f.payload.toString())).toEqual(["a", "bb", "ccc"]);
  });

  it("rejects a frame whose declared size exceeds the limit", async () => {
    const header = Buffer.alloc(5);
    header.writeUInt8(0, 0);
    header.writeUInt32BE(MAX_CONNECT_ENVELOPE_SIZE + 1, 1);

    await expect(collect(streamOf(header, 5))).rejects.toThrow(/too large/);
  });

  it("rejects a stream that ends mid-frame", async () => {
    const header = Buffer.alloc(5);
    header.writeUInt8(0, 0);
    header.writeUInt32BE(10, 1); // claims 10 payload bytes...
    const data = Buffer.concat([header, Buffer.from("abc")]); // ...but only 3 arrive

    await expect(collect(streamOf(data, 2))).rejects.toThrow(/partial message/);
  });
});
