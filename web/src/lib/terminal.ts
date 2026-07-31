// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

export function terminalWebSocketURL(
  path: string,
  ticket: string,
  location = window.location,
): string {
  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = new URL(path, `${protocol}//${location.host}`);
  url.protocol = protocol;
  url.searchParams.set('ticket', ticket);
  return url.toString();
}

interface TerminalContainerAvailability {
  state: string;
  envdPort?: number;
}

export function terminalAvailable(
  state: string | undefined,
  containers?: TerminalContainerAvailability[],
): boolean {
  if (state !== 'running') return false;
  // List responses do not contain per-container metadata; the authenticated
  // ticket endpoint performs the authoritative target check in that view.
  if (containers === undefined) return true;
  return containers.some(
    (container) => container.state === 'running' && (container.envdPort ?? 0) > 0,
  );
}
