// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { describe, expect, it } from 'vitest';
import { terminalAvailable, terminalWebSocketURL } from './terminal';

describe('terminal transport helpers', () => {
  it('uses WSS on an HTTPS dashboard and URL-encodes the one-time ticket', () => {
    const url = terminalWebSocketURL(
      '/cubeapi/v1/sandboxes/sandbox-1/terminal/ws',
      'ticket with + symbols',
      { protocol: 'https:', host: 'cube.example:12088' } as Location,
    );
    expect(url).toBe(
      'wss://cube.example:12088/cubeapi/v1/sandboxes/sandbox-1/terminal/ws?ticket=ticket+with+%2B+symbols',
    );
  });

  it('only enables terminal access for running sandboxes', () => {
    expect(terminalAvailable('running')).toBe(true);
    expect(terminalAvailable('paused')).toBe(false);
    expect(terminalAvailable('exited')).toBe(false);
    expect(terminalAvailable(undefined)).toBe(false);
  });

  it('disables a running detail target without a terminal-capable container', () => {
    expect(terminalAvailable('running', [])).toBe(false);
    expect(terminalAvailable('running', [{ state: 'running', envdPort: 0 }])).toBe(false);
    expect(terminalAvailable('running', [{ state: 'running', envdPort: 49983 }])).toBe(true);
  });
});
