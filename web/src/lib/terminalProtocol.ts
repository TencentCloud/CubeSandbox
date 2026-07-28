// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

export type TerminalServerMessage =
  | { type: 'start'; execId?: string; sessionId?: string }
  | { type: 'output'; data?: string }
  | { type: 'exit'; exitCode?: number | null; error?: string | null }
  | { type: 'error'; message?: string }
  | { type: 'idleTimeout'; message?: string; idleTimeoutSeconds?: number }
  | { type: 'streamEnd' }
  | { type?: string; [key: string]: unknown };

export const TERMINAL_WEBSOCKET_PROTOCOL = 'cube-terminal-v1';

export function toTerminalWebSocketUrl(path: string, origin = window.location.origin): string {
  const url = new URL(path, origin);
  url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
  return url.toString();
}

export function terminalInputFrame(data: string): string {
  return JSON.stringify({ type: 'inputBase64', data: encodeUtf8Base64(data) });
}

export function terminalResizeFrame(rows: number, cols: number): string {
  return JSON.stringify({ type: 'resize', rows, cols });
}

export function parseTerminalServerMessage(raw: string): TerminalServerMessage {
  return JSON.parse(raw) as TerminalServerMessage;
}

export function encodeUtf8Base64(value: string): string {
  const bytes = new TextEncoder().encode(value);
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

export function decodeBase64Bytes(value: string): Uint8Array {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return bytes;
}
