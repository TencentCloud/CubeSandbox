// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { describe, expect, it } from 'vitest';
import {
  buildTerminalWebSocketUrl,
  decodeServerControl,
  encodeClose,
  encodeKeepalive,
  encodeResize,
  terminalSubprotocols,
  reconcileTerminalContainer,
} from './terminalProtocol';

describe('terminal protocol', () => {
  it('drops a stale container when navigating to another sandbox', () => {
    expect(reconcileTerminalContainer('sandbox-a-main', ['sandbox-b-main', 'sandbox-b-sidecar'])).toBe(
      'sandbox-b-main',
    );
    expect(reconcileTerminalContainer('sandbox-b-sidecar', ['sandbox-b-main', 'sandbox-b-sidecar'])).toBe(
      'sandbox-b-sidecar',
    );
  });
  it('keeps the one-time grant out of the URL', () => {
    const url = buildTerminalWebSocketUrl(
      'sandbox-a-b',
      new URL('https://console.example/sandboxes/sandbox'),
    );
    expect(url).toBe('wss://console.example/sandboxes/sandbox-a-b/terminal');
    expect(url).not.toContain('grant');
    expect(terminalSubprotocols('secret')).toEqual([
      'cube-terminal.v1',
      'cube-terminal.grant.secret',
    ]);
    expect(() =>
      buildTerminalWebSocketUrl('sandbox/other', new URL('https://console.example/sandboxes/sandbox')),
    ).toThrow('invalid sandbox ID');
  });

  it('encodes versioned client controls', () => {
    expect(JSON.parse(encodeResize(120, 40))).toEqual({
      v: 1,
      type: 'resize',
      cols: 120,
      rows: 40,
    });
    expect(JSON.parse(encodeKeepalive())).toEqual({ v: 1, type: 'keepalive' });
    expect(JSON.parse(encodeClose())).toEqual({ v: 1, type: 'close' });
  });

  it('accepts known server controls and rejects malformed versions', () => {
    expect(decodeServerControl('{"v":1,"type":"ready","sessionId":"s-1"}')).toEqual({
      v: 1,
      type: 'ready',
      sessionId: 's-1',
    });
    expect(() => decodeServerControl('{"v":2,"type":"ready"}')).toThrow(
      'unsupported terminal protocol',
    );
    expect(() => decodeServerControl('{"v":1,"type":"mystery"}')).toThrow(
      'invalid terminal control',
    );
  });
});
