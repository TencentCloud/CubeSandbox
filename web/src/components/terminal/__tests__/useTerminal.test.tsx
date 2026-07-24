// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// Captured instances of the mocked xterm Terminal and WebSocket, so tests can
// drive frames/handlers and assert on calls. Classes live inside vi.hoisted
// because vi.mock factories are hoisted above top-level declarations.
const { terminals, sockets, MockTerminal, MockWebSocket } = vi.hoisted(() => {
  class MockTerminal {
    cols = 80;
    rows = 24;
    options: Record<string, unknown> = { fontSize: 13 };
    dataHandler: ((data: string) => void) | null = null;
    write = vi.fn();
    focus = vi.fn();
    paste = vi.fn();
    dispose = vi.fn();

    constructor() {
      terminals.push(this);
    }
    open() {}
    loadAddon() {}
    attachCustomKeyEventHandler() {}
    onData(cb: (data: string) => void) {
      this.dataHandler = cb;
      return { dispose() {} };
    }
    onResize() {
      return { dispose() {} };
    }
  }

  class MockWebSocket {
    static OPEN = 1;
    readonly url: string;
    readonly protocols?: string[];
    readyState = MockWebSocket.OPEN;
    binaryType = '';
    send = vi.fn();
    close = vi.fn();
    onopen: (() => void) | null = null;
    onmessage: ((event: { data: unknown }) => void) | null = null;
    onclose: (() => void) | null = null;
    onerror: (() => void) | null = null;

    constructor(url: string, protocols?: string[]) {
      this.url = url;
      this.protocols = protocols;
      sockets.push(this);
    }
  }

  const terminals: MockTerminal[] = [];
  const sockets: MockWebSocket[] = [];
  return { terminals, sockets, MockTerminal, MockWebSocket };
});

type MockTerminal = InstanceType<typeof MockTerminal>;
type MockWebSocket = InstanceType<typeof MockWebSocket>;

vi.mock('@xterm/xterm', () => ({ Terminal: MockTerminal }));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit() {}
  },
}));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: class {} }));
vi.mock('@/lib/api', () => ({ ensureFreshToken: vi.fn(async () => 'test-token') }));

vi.stubGlobal('WebSocket', MockWebSocket);

import { useTerminal } from '../useTerminal';

function mountTerminal(containerID?: string) {
  const view = renderHook(({ cid }) => useTerminal('sb-1', true, cid), {
    initialProps: { cid: containerID },
  });
  // The hook waits for its callback ref (Radix portal timing); simulate the
  // container element appearing.
  act(() => view.result.current.containerRef(document.createElement('div')));
  return view;
}

async function nextSocket(): Promise<MockWebSocket> {
  await waitFor(() => expect(sockets.length).toBeGreaterThan(0));
  return sockets[sockets.length - 1];
}

function lastTerminal(): MockTerminal {
  return terminals[terminals.length - 1];
}

beforeEach(() => {
  terminals.length = 0;
  sockets.length = 0;
});

afterEach(() => {
  vi.clearAllMocks();
});

describe('useTerminal', () => {
  it('connects to the terminal WS URL with cols/rows', async () => {
    mountTerminal();
    const socket = await nextSocket();
    const url = new URL(socket.url);
    expect(url.pathname).toBe('/cubeapi/v1/sandboxes/sb-1/terminal/ws');
    expect(url.searchParams.get('cols')).toBe('80');
    expect(url.searchParams.get('rows')).toBe('24');
    expect(url.searchParams.get('container')).toBeNull();
  });

  it('appends the container query param when a containerID is given', async () => {
    mountTerminal('ctr side');
    const socket = await nextSocket();
    expect(new URL(socket.url).searchParams.get('container')).toBe('ctr side');
  });

  it('offers the token via WebSocket subprotocols', async () => {
    mountTerminal();
    const socket = await nextSocket();
    expect(socket.protocols).toEqual(['cube-terminal', 'cube-terminal.test-token']);
  });

  it('reconnects against the new container when containerID changes', async () => {
    const view = mountTerminal('ctr-1');
    await nextSocket();
    view.rerender({ cid: 'ctr-2' });
    await waitFor(() => expect(sockets.length).toBe(2));
    expect(new URL(sockets[1].url).searchParams.get('container')).toBe('ctr-2');
  });

  it('writes decoded output frames and applies exit/error frames', async () => {
    const view = mountTerminal();
    const socket = await nextSocket();
    const term = lastTerminal();

    act(() => socket.onmessage?.({ data: JSON.stringify({ type: 'ready' }) }));
    expect(view.result.current.status).toBe('ready');
    expect(term.focus).toHaveBeenCalled();

    // base64('hi') — the server sends PTY bytes as base64.
    act(() => socket.onmessage?.({ data: JSON.stringify({ type: 'output', data: 'aGk=' }) }));
    expect(term.write).toHaveBeenCalledWith(new Uint8Array([104, 105]));

    act(() => socket.onmessage?.({ data: JSON.stringify({ type: 'exit', code: 3 }) }));
    expect(view.result.current.status).toBe('exited');
    expect(view.result.current.exitCode).toBe(3);

    act(() =>
      socket.onmessage?.({ data: JSON.stringify({ type: 'error', message: 'no terminal' }) }),
    );
    expect(view.result.current.status).toBe('error');
    expect(view.result.current.errorMessage).toBe('no terminal');
  });

  it('sends an initial resize on open and base64 input frames on term data', async () => {
    mountTerminal();
    const socket = await nextSocket();
    const term = lastTerminal();

    act(() => socket.onopen?.());
    expect(socket.send).toHaveBeenCalledWith(
      JSON.stringify({ type: 'resize', cols: 80, rows: 24 }),
    );

    act(() => term.dataHandler?.('a'));
    // base64('a') === 'YQ=='
    expect(socket.send).toHaveBeenCalledWith(JSON.stringify({ type: 'input', data: 'YQ==' }));
  });

  it('closes the socket and disposes the terminal on unmount', async () => {
    const view = mountTerminal();
    const socket = await nextSocket();
    const term = lastTerminal();

    view.unmount();
    expect(socket.close).toHaveBeenCalled();
    expect(term.dispose).toHaveBeenCalled();
  });
});
