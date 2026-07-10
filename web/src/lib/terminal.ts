// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import type { SandboxContainer } from '@/api/client';

export function canOpenTerminal(state?: string | null): boolean {
  return state === 'running';
}

export function hasTerminalContainerSelector(containers: SandboxContainer[]): boolean {
  return containers.length > 1;
}

export function selectTerminalContainer(
  containers: SandboxContainer[],
  sandboxID: string,
  requested?: string,
): SandboxContainer | undefined {
  if (requested) {
    return containers.find((container) => container.containerID === requested);
  }
  return containers.find((container) => (
    container.type === 'sandbox' || container.containerID === sandboxID
  )) ?? containers[0];
}

export function terminalResizeMessage(cols: number, rows: number): string {
  return JSON.stringify({ type: 'resize', cols, rows });
}
