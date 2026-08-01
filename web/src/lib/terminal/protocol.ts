// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

export const TERMINAL_SUBPROTOCOL = 'cube-terminal.v1';
export const TERMINAL_GRANT_PREFIX = 'cube-grant.';

export const TERMINAL_CHANNEL = {
  stdin: 0x00,
  stdout: 0x01,
  stderr: 0x02,
  status: 0x03,
  resize: 0x04,
} as const;

export const MAX_TERMINAL_PAYLOAD_BYTES = 64 * 1024;
export const MAX_TERMINAL_STATUS_BYTES = 4 * 1024;

export const TERMINAL_ERROR_CODES = [
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
] as const;

export const TERMINAL_CLOSE_REASONS = [
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
] as const;

export const TERMINAL_REASON_CODES = [
  'UNAUTHORIZED',
  'FORBIDDEN',
  'TARGET_NOT_FOUND',
  'TARGET_NOT_RUNNING',
  'GRANT_INVALID',
  'LIMIT_EXCEEDED',
  'PROTOCOL_ERROR',
  'SHELL_NOT_FOUND',
  'SLOW_PRODUCER',
  'SLOW_CONSUMER',
  'IDLE_TIMEOUT',
  'MAX_LIFETIME',
  'SANDBOX_TRANSITION',
  'RUNTIME_EXITED',
  'SESSION_LOST',
  'INTERNAL',
  'SERVER_DRAINING',
  'USER_CLOSED',
] as const;

export type TerminalErrorCode = (typeof TERMINAL_ERROR_CODES)[number];
export type TerminalCloseReason = (typeof TERMINAL_CLOSE_REASONS)[number];
export type TerminalReasonCode = (typeof TERMINAL_REASON_CODES)[number];

export type TerminalServerFrame =
  | { type: 'stdout'; payload: Uint8Array }
  | {
      type: 'opened';
      sessionId: string;
      replay: { from: number; truncated: boolean };
    }
  | { type: 'exit'; exitCode: number }
  | { type: 'error'; code: TerminalErrorCode }
  | { type: 'close'; reason: TerminalCloseReason };

export type TerminalClientFrame =
  { type: 'stdin'; payload: Uint8Array } | { type: 'resize'; cols: number; rows: number };

export class TerminalProtocolError extends Error {
  constructor(
    readonly closeCode: 1008 | 1009,
    message = 'Invalid terminal protocol frame',
  ) {
    super(message);
    this.name = 'TerminalProtocolError';
  }
}

const errorCodes = new Set<string>(TERMINAL_ERROR_CODES);
const closeReasons = new Set<string>(TERMINAL_CLOSE_REASONS);
const textEncoder = new TextEncoder();

export function assertTerminalDimensions(cols: number, rows: number): void {
  if (
    !Number.isInteger(cols) ||
    !Number.isInteger(rows) ||
    cols < 1 ||
    rows < 1 ||
    cols > 1000 ||
    rows > 1000
  ) {
    throw new TerminalProtocolError(1008, 'Terminal dimensions are out of range');
  }
}

export function encodeResizeFrame(cols: number, rows: number): Uint8Array {
  assertTerminalDimensions(cols, rows);
  const payload = textEncoder.encode(JSON.stringify({ cols, rows }));
  if (payload.byteLength > MAX_TERMINAL_STATUS_BYTES) {
    throw new TerminalProtocolError(1008, 'Terminal resize payload is too large');
  }
  return withChannel(TERMINAL_CHANNEL.resize, payload);
}

export function encodeStdinFrames(data: string | Uint8Array): Uint8Array[] {
  const payload = typeof data === 'string' ? textEncoder.encode(data) : data;
  if (payload.byteLength === 0) {
    return [new Uint8Array([TERMINAL_CHANNEL.stdin])];
  }
  const frames: Uint8Array[] = [];
  for (let offset = 0; offset < payload.byteLength; offset += MAX_TERMINAL_PAYLOAD_BYTES) {
    frames.push(
      withChannel(
        TERMINAL_CHANNEL.stdin,
        payload.subarray(offset, offset + MAX_TERMINAL_PAYLOAD_BYTES),
      ),
    );
  }
  return frames;
}

export function decodeClientFrame(data: unknown): TerminalClientFrame {
  const frame = toBytes(data);
  if (frame.byteLength === 0) {
    throw new TerminalProtocolError(1008, 'Terminal frame is empty');
  }
  const payload = frame.subarray(1);
  if (payload.byteLength > MAX_TERMINAL_PAYLOAD_BYTES) {
    throw new TerminalProtocolError(1009, 'Terminal frame exceeds the payload limit');
  }
  switch (frame[0]) {
    case TERMINAL_CHANNEL.stdin:
      return { type: 'stdin', payload };
    case TERMINAL_CHANNEL.resize: {
      const value = decodeStrictJson(payload, MAX_TERMINAL_STATUS_BYTES);
      assertExactKeys(value, ['cols', 'rows']);
      const cols = value.cols;
      const rows = value.rows;
      if (typeof cols !== 'number' || typeof rows !== 'number') {
        throw new TerminalProtocolError(1008, 'Terminal resize schema is invalid');
      }
      assertTerminalDimensions(cols, rows);
      return { type: 'resize', cols, rows };
    }
    default:
      throw new TerminalProtocolError(1008, 'Terminal client channel is invalid');
  }
}

export function decodeServerFrame(data: unknown): TerminalServerFrame {
  const frame = toBytes(data);
  if (frame.byteLength === 0) {
    throw new TerminalProtocolError(1008, 'Terminal frame is empty');
  }
  const payload = frame.subarray(1);
  if (payload.byteLength > MAX_TERMINAL_PAYLOAD_BYTES) {
    throw new TerminalProtocolError(1009, 'Terminal frame exceeds the payload limit');
  }
  switch (frame[0]) {
    case TERMINAL_CHANNEL.stdout:
      return { type: 'stdout', payload };
    case TERMINAL_CHANNEL.status:
      return decodeStatus(payload);
    default:
      throw new TerminalProtocolError(1008, 'Terminal server channel is invalid');
  }
}

export function isTerminalReasonCode(value: unknown): value is TerminalReasonCode {
  return typeof value === 'string' && (TERMINAL_REASON_CODES as readonly string[]).includes(value);
}

function decodeStatus(payload: Uint8Array): Exclude<TerminalServerFrame, { type: 'stdout' }> {
  const value = decodeStrictJson(payload, MAX_TERMINAL_STATUS_BYTES);
  if (typeof value.type !== 'string') {
    throw new TerminalProtocolError(1008, 'Terminal status type is invalid');
  }
  switch (value.type) {
    case 'opened': {
      assertExactKeys(value, ['type', 'sessionId', 'replay']);
      if (typeof value.sessionId !== 'string' || value.sessionId.length === 0) {
        throw new TerminalProtocolError(1008, 'Terminal opened status is invalid');
      }
      const replay = asRecord(value.replay);
      assertExactKeys(replay, ['from', 'truncated']);
      if (
        typeof replay.from !== 'number' ||
        !Number.isSafeInteger(replay.from) ||
        replay.from < 0 ||
        typeof replay.truncated !== 'boolean'
      ) {
        throw new TerminalProtocolError(1008, 'Terminal replay status is invalid');
      }
      return {
        type: 'opened',
        sessionId: value.sessionId,
        replay: { from: replay.from, truncated: replay.truncated },
      };
    }
    case 'exit': {
      assertExactKeys(value, ['type', 'exitCode']);
      if (
        typeof value.exitCode !== 'number' ||
        !Number.isInteger(value.exitCode) ||
        value.exitCode < -2147483648 ||
        value.exitCode > 2147483647
      ) {
        throw new TerminalProtocolError(1008, 'Terminal exit status is invalid');
      }
      return { type: 'exit', exitCode: value.exitCode };
    }
    case 'error': {
      assertExactKeys(value, ['type', 'code']);
      if (typeof value.code !== 'string' || !errorCodes.has(value.code)) {
        throw new TerminalProtocolError(1008, 'Terminal error code is invalid');
      }
      return { type: 'error', code: value.code as TerminalErrorCode };
    }
    case 'close': {
      assertExactKeys(value, ['type', 'reason']);
      if (typeof value.reason !== 'string' || !closeReasons.has(value.reason)) {
        throw new TerminalProtocolError(1008, 'Terminal close reason is invalid');
      }
      return { type: 'close', reason: value.reason as TerminalCloseReason };
    }
    default:
      throw new TerminalProtocolError(1008, 'Terminal status type is unknown');
  }
}

function decodeStrictJson(payload: Uint8Array, limit: number): Record<string, unknown> {
  if (payload.byteLength === 0 || payload.byteLength > limit) {
    throw new TerminalProtocolError(1008, 'Terminal JSON payload size is invalid');
  }
  let decoded: string;
  try {
    decoded = new TextDecoder('utf-8', { fatal: true }).decode(payload);
  } catch {
    throw new TerminalProtocolError(1008, 'Terminal JSON payload is not UTF-8');
  }
  let value: unknown;
  try {
    value = JSON.parse(decoded);
  } catch {
    throw new TerminalProtocolError(1008, 'Terminal JSON payload is invalid');
  }
  return asRecord(value);
}

function asRecord(value: unknown): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new TerminalProtocolError(1008, 'Terminal JSON payload must be an object');
  }
  return value as Record<string, unknown>;
}

function assertExactKeys(value: Record<string, unknown>, expected: readonly string[]): void {
  const keys = Object.keys(value);
  if (
    keys.length !== expected.length ||
    expected.some((key) => !Object.prototype.hasOwnProperty.call(value, key))
  ) {
    throw new TerminalProtocolError(1008, 'Terminal JSON schema contains unknown fields');
  }
}

function toBytes(data: unknown): Uint8Array {
  if (data instanceof ArrayBuffer) {
    return new Uint8Array(data);
  }
  if (ArrayBuffer.isView(data)) {
    return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
  }
  throw new TerminalProtocolError(1008, 'Binary terminal frames are required');
}

function withChannel(channel: number, payload: Uint8Array): Uint8Array {
  const frame = new Uint8Array(payload.byteLength + 1);
  frame[0] = channel;
  frame.set(payload, 1);
  return frame;
}
