// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useTerminalSocket } from './useTerminalSocket';

const createTerminalTicket = vi.fn();
vi.mock('@/api/client', () => ({
  sandboxApi: {
    createTerminalTicket: (id: string) => createTerminalTicket(id),
  },
}));

/** Minimal scriptable WebSocket stand-in. */
class MockWebSocket {
  static instances: MockWebSocket[] = [];
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  readyState = MockWebSocket.CONNECTING;
  sent: string[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;

  constructor(readonly url: string) {
    MockWebSocket.instances.push(this);
  }

  send(data: string) {
    this.sent.push(data);
  }

  close() {
    if (this.readyState === MockWebSocket.CLOSED) return;
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.();
  }

  // — test helpers —
  open() {
    this.readyState = MockWebSocket.OPEN;
    this.onopen?.();
  }

  emit(msg: unknown) {
    this.onmessage?.({ data: JSON.stringify(msg) });
  }

  /** Simulates the socket dropping without a clean close handshake. */
  drop() {
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.();
  }

  parsedSent() {
    return this.sent.map((s) => JSON.parse(s));
  }
}

const b64 = (s: string) => btoa(String.fromCharCode(...new TextEncoder().encode(s)));

function renderSocket(overrides: { onOutput?: (data: string) => void } = {}) {
  const onOutput = overrides.onOutput ?? vi.fn();
  const view = renderHook(() =>
    useTerminalSocket({
      sandboxID: 'sbx-1',
      enabled: true,
      onOutput,
      getSize: () => ({ cols: 100, rows: 30 }),
    }),
  );
  return { ...view, onOutput };
}

/** Waits for the hook's async ticket fetch to produce a socket. */
async function latestSocket(): Promise<MockWebSocket> {
  await waitFor(() => expect(MockWebSocket.instances.length).toBeGreaterThan(0));
  return MockWebSocket.instances[MockWebSocket.instances.length - 1];
}

describe('useTerminalSocket', () => {
  beforeEach(() => {
    MockWebSocket.instances = [];
    createTerminalTicket.mockReset();
    createTerminalTicket.mockResolvedValue({ ticket: 'tkt-1', wsPath: '/terminal/ws' });
    vi.stubGlobal('WebSocket', MockWebSocket);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('requests a ticket and connects with the current terminal size', async () => {
    const { result } = renderSocket();
    expect(result.current.status).toBe('connecting');

    const socket = await latestSocket();
    expect(createTerminalTicket).toHaveBeenCalledWith('sbx-1');
    const url = new URL(socket.url);
    expect(url.pathname).toBe('/opsapi/v1/terminal/ws');
    expect(url.searchParams.get('ticket')).toBe('tkt-1');
    expect(url.searchParams.get('cols')).toBe('100');
    expect(url.searchParams.get('rows')).toBe('30');
    // A fresh session must not ask to reattach to a PID.
    expect(url.searchParams.get('pid')).toBeNull();
  });

  it('reports connected once the backend sends ready', async () => {
    const { result } = renderSocket();
    const socket = await latestSocket();

    act(() => {
      socket.open();
      socket.emit({ type: 'ready', pid: 4242 });
    });

    expect(result.current.status).toBe('connected');
    expect(result.current.canReconnect).toBe(false);
  });

  it('decodes base64 output, including multi-byte characters', async () => {
    const onOutput = vi.fn();
    renderSocket({ onOutput });
    const socket = await latestSocket();

    act(() => {
      socket.open();
      socket.emit({ type: 'ready', pid: 1 });
      socket.emit({ type: 'output', data: b64('总用量 4\n') });
    });

    expect(onOutput).toHaveBeenCalledWith('总用量 4\n');
  });

  it('base64-encodes input and forwards resize frames', async () => {
    const { result } = renderSocket();
    const socket = await latestSocket();
    act(() => {
      socket.open();
      socket.emit({ type: 'ready', pid: 1 });
    });

    act(() => {
      result.current.sendInput('ls\n');
      result.current.sendResize(120, 40);
    });

    const frames = socket.parsedSent();
    expect(frames).toContainEqual({ type: 'input', data: b64('ls\n') });
    expect(frames).toContainEqual({ type: 'resize', cols: 120, rows: 40 });
  });

  it('offers a reconnect after an abnormal drop and reattaches by pid', async () => {
    const { result } = renderSocket();
    const first = await latestSocket();
    act(() => {
      first.open();
      first.emit({ type: 'ready', pid: 777 });
    });

    act(() => {
      first.drop();
    });
    expect(result.current.status).toBe('disconnected');
    expect(result.current.canReconnect).toBe(true);

    act(() => {
      result.current.reconnect();
    });
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(2));
    const second = MockWebSocket.instances[1];
    expect(new URL(second.url).searchParams.get('pid')).toBe('777');
  });

  it('does not offer a reconnect after the shell exits', async () => {
    const { result } = renderSocket();
    const socket = await latestSocket();
    act(() => {
      socket.open();
      socket.emit({ type: 'ready', pid: 5 });
      socket.emit({ type: 'exit', exitCode: 130 });
    });

    expect(result.current.status).toBe('exited');
    expect(result.current.exitCode).toBe(130);
    expect(result.current.canReconnect).toBe(false);

    // A close arriving after the exit must not downgrade the state.
    act(() => {
      socket.drop();
    });
    expect(result.current.status).toBe('exited');
    expect(result.current.canReconnect).toBe(false);
  });

  it('surfaces a backend error frame without offering a reconnect', async () => {
    const { result } = renderSocket();
    const socket = await latestSocket();
    act(() => {
      socket.open();
      socket.emit({ type: 'error', message: 'sandbox is not running' });
    });

    expect(result.current.status).toBe('error');
    expect(result.current.detail).toBe('sandbox is not running');
    expect(result.current.canReconnect).toBe(false);
  });

  it('reports an error when the ticket request fails', async () => {
    createTerminalTicket.mockRejectedValue(new Error('409 Conflict'));
    const { result } = renderSocket();

    await waitFor(() => expect(result.current.status).toBe('error'));
    expect(result.current.detail).toBe('409 Conflict');
    expect(MockWebSocket.instances).toHaveLength(0);
  });

  // Regression: an unmount while the ticket request is still in flight used to
  // leave the resolved attempt free to open a socket nobody owned, leaking a
  // live PTY inside the sandbox.
  it('abandons a connection whose ticket resolves after unmount', async () => {
    let releaseTicket: (v: unknown) => void = () => {};
    createTerminalTicket.mockReturnValue(
      new Promise((resolve) => {
        releaseTicket = resolve;
      }),
    );

    const { unmount } = renderSocket();
    act(() => {
      unmount();
    });
    await act(async () => {
      releaseTicket({ ticket: 'tkt-late', wsPath: '/terminal/ws' });
    });

    expect(MockWebSocket.instances).toHaveLength(0);
  });

  // Regression: a remount (React StrictMode, or a fast close/reopen) reset the
  // shared "closing" flag, so the first attempt connected too — two sockets,
  // two shells, one dialog.
  it('opens only one socket when the hook remounts during the ticket fetch', async () => {
    const pending: Array<(v: unknown) => void> = [];
    createTerminalTicket.mockImplementation(() => new Promise((resolve) => pending.push(resolve)));

    const first = renderSocket();
    act(() => {
      first.unmount();
    });
    const second = renderSocket();

    await act(async () => {
      pending.forEach((resolve, i) => resolve({ ticket: `tkt-${i}`, wsPath: '/terminal/ws' }));
    });

    expect(createTerminalTicket).toHaveBeenCalledTimes(2);
    expect(MockWebSocket.instances).toHaveLength(1);
    second.unmount();
  });

  it('sends an explicit close so the backend kills the shell on unmount', async () => {
    const { unmount } = renderSocket();
    const socket = await latestSocket();
    act(() => {
      socket.open();
      socket.emit({ type: 'ready', pid: 9 });
    });

    act(() => {
      unmount();
    });

    expect(socket.parsedSent()).toContainEqual({ type: 'close' });
    expect(socket.readyState).toBe(MockWebSocket.CLOSED);
  });
});
