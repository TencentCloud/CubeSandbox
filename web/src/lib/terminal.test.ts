// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { describe, expect, it } from 'vitest';

import type { SandboxContainer } from '@/api/client';
import {
  canOpenTerminal,
  hasTerminalContainerSelector,
  selectTerminalContainer,
  terminalResizeMessage,
} from './terminal';

const containers: SandboxContainer[] = [
  { containerID: 'sidecar', name: 'sidecar', status: 1, type: 'sidecar' },
  { containerID: 'sandbox', name: 'main', status: 1, type: 'sandbox' },
];

describe('terminal UI behavior', () => {
  it('enables the terminal only for running sandboxes', () => {
    expect(canOpenTerminal('running')).toBe(true);
    expect(canOpenTerminal('paused')).toBe(false);
    expect(canOpenTerminal('pausing')).toBe(false);
    expect(canOpenTerminal('stopped')).toBe(false);
  });

  it('shows a selector and resolves the requested container for multi-container sandboxes', () => {
    expect(hasTerminalContainerSelector(containers)).toBe(true);
    expect(selectTerminalContainer(containers, 'sandbox')?.containerID).toBe('sandbox');
    expect(selectTerminalContainer(containers, 'sandbox', 'sidecar')?.containerID).toBe('sidecar');
    expect(selectTerminalContainer(containers, 'sandbox', 'unknown')).toBeUndefined();
  });

  it('serializes resize dimensions into the WebSocket control protocol', () => {
    expect(JSON.parse(terminalResizeMessage(132, 43))).toEqual({
      type: 'resize',
      cols: 132,
      rows: 43,
    });
  });
});
