// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { describe, expect, it } from 'vitest';
import {
  MAX_TERMINAL_PAYLOAD_BYTES,
  MAX_TERMINAL_STATUS_BYTES,
  TERMINAL_CHANNEL,
  TERMINAL_CLOSE_REASONS,
  TERMINAL_ERROR_CODES,
  TerminalProtocolError,
  decodeClientFrame,
  decodeServerFrame,
  encodeResizeFrame,
  encodeStdinFrames,
} from './protocol';

const encoder = new TextEncoder();

describe('cube-terminal.v1 protocol mirror', () => {
  it('mirrors the Go channel values and stable status sets', () => {
    expect(TERMINAL_CHANNEL).toEqual({ stdin: 0, stdout: 1, stderr: 2, status: 3, resize: 4 });
    expect(TERMINAL_ERROR_CODES).toEqual([
      'TARGET_NOT_FOUND',
      'TARGET_NOT_RUNNING',
      'LIMIT_EXCEEDED',
      'PROTOCOL_ERROR',
      'SHELL_NOT_FOUND',
      'SLOW_PRODUCER',
      'SLOW_CONSUMER',
      'SESSION_LOST',
      'INTERNAL',
      'SERVER_DRAINING',
      'SANDBOX_TRANSITION',
    ]);
    expect(TERMINAL_CLOSE_REASONS).toEqual([
      'USER_CLOSED',
      'IDLE_TIMEOUT',
      'MAX_LIFETIME',
      'SANDBOX_TRANSITION',
      'RUNTIME_EXITED',
      'SESSION_LOST',
      'PROTOCOL_ERROR',
      'SERVER_DRAINING',
      'SLOW_PRODUCER',
      'SLOW_CONSUMER',
      'INTERNAL',
    ]);
  });

  it('encodes stdin as UTF-8 and splits payloads at the 64 KiB boundary', () => {
    const unicode = encodeStdinFrames('中🙂');
    expect(unicode).toHaveLength(1);
    expect(unicode[0][0]).toBe(TERMINAL_CHANNEL.stdin);
    expect(new TextDecoder().decode(unicode[0].subarray(1))).toBe('中🙂');

    const payload = new Uint8Array(MAX_TERMINAL_PAYLOAD_BYTES * 2 + 7).fill(0x61);
    const frames = encodeStdinFrames(payload);
    expect(frames.map((frame) => frame.byteLength)).toEqual([
      MAX_TERMINAL_PAYLOAD_BYTES + 1,
      MAX_TERMINAL_PAYLOAD_BYTES + 1,
      8,
    ]);
    expect(frames.map((frame) => decodeClientFrame(frame).type)).toEqual([
      'stdin',
      'stdin',
      'stdin',
    ]);
  });

  it('accepts stdin/stdout at the payload limit and rejects oversized frames', () => {
    const stdin = frame(TERMINAL_CHANNEL.stdin, new Uint8Array(MAX_TERMINAL_PAYLOAD_BYTES));
    const stdout = frame(TERMINAL_CHANNEL.stdout, new Uint8Array(MAX_TERMINAL_PAYLOAD_BYTES));
    expect(decodeClientFrame(stdin)).toMatchObject({ type: 'stdin' });
    expect(decodeServerFrame(stdout)).toMatchObject({ type: 'stdout' });

    const oversizedClient = frame(
      TERMINAL_CHANNEL.stdin,
      new Uint8Array(MAX_TERMINAL_PAYLOAD_BYTES + 1),
    );
    const oversizedServer = frame(
      TERMINAL_CHANNEL.stdout,
      new Uint8Array(MAX_TERMINAL_PAYLOAD_BYTES + 1),
    );
    expectProtocolError(() => decodeClientFrame(oversizedClient), 1009);
    expectProtocolError(() => decodeServerFrame(oversizedServer), 1009);
  });

  it('strictly validates resize JSON and integer range 1..1000', () => {
    expect(decodeClientFrame(encodeResizeFrame(1, 1))).toEqual({
      type: 'resize',
      cols: 1,
      rows: 1,
    });
    expect(decodeClientFrame(encodeResizeFrame(1000, 1000))).toEqual({
      type: 'resize',
      cols: 1000,
      rows: 1000,
    });
    for (const json of [
      '{"cols":0,"rows":1}',
      '{"cols":1001,"rows":1}',
      '{"cols":1.5,"rows":24}',
      '{"cols":80,"rows":24,"extra":1}',
      '{"cols":80,"rows":24} {}',
    ]) {
      expectProtocolError(() => decodeClientFrame(statusLike(TERMINAL_CHANNEL.resize, json)), 1008);
    }
  });

  it('accepts the Go opened/exit/error/close vectors', () => {
    expect(
      decodeServerFrame(
        status('{"type":"opened","sessionId":"session-a","replay":{"from":7,"truncated":true}}'),
      ),
    ).toEqual({ type: 'opened', sessionId: 'session-a', replay: { from: 7, truncated: true } });
    expect(decodeServerFrame(status('{"type":"exit","exitCode":17}'))).toEqual({
      type: 'exit',
      exitCode: 17,
    });
    expect(decodeServerFrame(status('{"type":"error","code":"SLOW_PRODUCER"}'))).toEqual({
      type: 'error',
      code: 'SLOW_PRODUCER',
    });
    expect(decodeServerFrame(status('{"type":"close","reason":"SERVER_DRAINING"}'))).toEqual({
      type: 'close',
      reason: 'SERVER_DRAINING',
    });
  });

  it('rejects malformed and non-binary frames without exposing payload text', () => {
    for (const invalid of [
      new Uint8Array(),
      'text frame',
      new Uint8Array([0xff]),
      new Uint8Array([TERMINAL_CHANNEL.stderr]),
    ]) {
      expectProtocolError(() => decodeServerFrame(invalid), 1008);
    }
    expectProtocolError(() => decodeClientFrame(new Uint8Array([0xff])), 1008);
  });

  it('strictly rejects bad status schemas, unknown values, and multiple JSON values', () => {
    for (const json of [
      '{"type":"opened","sessionId":"session-a"}',
      '{"type":"exit","exitCode":0,"detail":"no"}',
      '{"type":"error","code":"details: secret"}',
      '{"type":"close","reason":"arbitrary"}',
      '{"type":"future"}',
      '{"type":"exit","exitCode":0} {}',
    ]) {
      expectProtocolError(() => decodeServerFrame(status(json)), 1008);
    }
  });

  it('rejects status payloads larger than 4 KiB', () => {
    const oversized = `{"type":"error","code":"${'A'.repeat(MAX_TERMINAL_STATUS_BYTES)}"}`;
    expectProtocolError(() => decodeServerFrame(status(oversized)), 1008);
  });
});

function status(json: string): Uint8Array {
  return statusLike(TERMINAL_CHANNEL.status, json);
}

function statusLike(channel: number, json: string): Uint8Array {
  return frame(channel, encoder.encode(json));
}

function frame(channel: number, payload = new Uint8Array()): Uint8Array {
  const value = new Uint8Array(payload.byteLength + 1);
  value[0] = channel;
  value.set(payload, 1);
  return value;
}

function expectProtocolError(run: () => unknown, closeCode: number): void {
  try {
    run();
    throw new Error('expected protocol error');
  } catch (error) {
    expect(error).toBeInstanceOf(TerminalProtocolError);
    expect((error as TerminalProtocolError).closeCode).toBe(closeCode);
    expect((error as Error).message).not.toContain('secret');
  }
}
