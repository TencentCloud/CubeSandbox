// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

export const TERMINAL_PROTOCOL = 'cube-terminal.v1';
const GRANT_PROTOCOL_PREFIX = 'cube-terminal.grant.';

export type TerminalServerControl =
  | { v: 1; type: 'ready'; sessionId?: string }
  | {
      v: 1;
      type: 'error';
      code?: string;
      message?: string;
      retryable?: boolean;
    }
  | { v: 1; type: 'exit'; exitCode?: number; reason?: string };

export function terminalSubprotocols(grant: string): string[] {
  return [TERMINAL_PROTOCOL, `${GRANT_PROTOCOL_PREFIX}${grant}`];
}

export function buildTerminalWebSocketUrl(sandboxID: string, page: URL = new URL(location.href)) {
  const scheme = page.protocol === 'https:' ? 'wss:' : 'ws:';
  // CubeMaster sandbox IDs are canonical path-segment identifiers; embedded
  // slashes are not supported by the SDK routes or the nginx proxy.
  if (sandboxID.includes('/')) throw new Error('invalid sandbox ID');
  return `${scheme}//${page.host}/terminal/sandboxes/${encodeURIComponent(sandboxID)}`;
}

export function encodeResize(cols: number, rows: number): string {
  return JSON.stringify({ v: 1, type: 'resize', cols, rows });
}

export function encodeKeepalive(): string {
  return JSON.stringify({ v: 1, type: 'keepalive' });
}

export function encodeClose(): string {
  return JSON.stringify({ v: 1, type: 'close' });
}

export function reconcileTerminalContainer(current: string, available: string[]): string {
  return current && available.includes(current) ? current : (available[0] ?? '');
}

export function decodeServerControl(raw: string): TerminalServerControl {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    throw new Error('invalid terminal control');
  }
  if (!isRecord(value)) throw new Error('invalid terminal control');
  if (value.v !== 1) throw new Error('unsupported terminal protocol');
  if (value.type !== 'ready' && value.type !== 'error' && value.type !== 'exit') {
    throw new Error('invalid terminal control');
  }
  return value as TerminalServerControl;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
