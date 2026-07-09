// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { TerminalPanel } from './TerminalPanel';

const mocks = vi.hoisted(() => ({
  terminals: [] as Array<{
    rows: number;
    cols: number;
    writeln: ReturnType<typeof vi.fn>;
    write: ReturnType<typeof vi.fn>;
    emitData: (data: string) => void;
    emitResize: (size: { rows: number; cols: number }) => void;
  }>,
  sockets: [] as Array<MockWebSocket>,
  terminalUrl: vi.fn(() => 'ws://localhost/cubeapi/v1/sandboxes/sb-1/terminal'),
  t: (key: string) => key,
}));

class MockWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;

  readyState = MockWebSocket.CONNECTING;
  sent: string[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: (() => void) | null = null;

  constructor(public url: string) {
    mocks.sockets.push(this);
  }

  send(message: string) {
    this.sent.push(message);
  }

  close() {
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.();
  }

  open() {
    this.readyState = MockWebSocket.OPEN;
    this.onopen?.();
  }

  receive(data: unknown) {
    this.onmessage?.({ data: JSON.stringify(data) } as MessageEvent<string>);
  }
}

vi.mock('@xterm/xterm', () => {
  class Terminal {
    rows = 24;
    cols = 80;
    options: { fontSize?: number };
    loadAddon = vi.fn();
    open = vi.fn();
    focus = vi.fn();
    writeln = vi.fn();
    write = vi.fn();
    dispose = vi.fn();
    private dataHandler: ((data: string) => void) | null = null;
    private resizeHandler: ((size: { rows: number; cols: number }) => void) | null = null;

    constructor(options: { fontSize?: number }) {
      this.options = { ...options };
      mocks.terminals.push(this);
    }

    onData = vi.fn((handler: (data: string) => void) => {
      this.dataHandler = handler;
      return { dispose: vi.fn() };
    });

    onResize = vi.fn((handler: (size: { rows: number; cols: number }) => void) => {
      this.resizeHandler = handler;
      return { dispose: vi.fn() };
    });

    emitData(data: string) {
      this.dataHandler?.(data);
    }

    emitResize(size: { rows: number; cols: number }) {
      this.resizeHandler?.(size);
    }
  }

  return { Terminal };
});
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { fit = vi.fn(); } }));
vi.mock('react-i18next', () => ({ useTranslation: () => ({ t: mocks.t }) }));
vi.mock('@/api/client', () => ({ sandboxTerminalWebSocketUrl: mocks.terminalUrl }));

describe('TerminalPanel', () => {
  beforeEach(() => {
    mocks.terminals.length = 0;
    mocks.sockets.length = 0;
    mocks.terminalUrl.mockClear();
    vi.stubGlobal('WebSocket', MockWebSocket);
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
      callback(0);
      return 0;
    });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('opens a websocket, sends resize/input messages, and renders output', () => {
    render(<TerminalPanel sandboxID="sb-1" open onOpenChange={vi.fn()} />);

    expect(screen.getByText('title')).toBeInTheDocument();
    expect(mocks.terminalUrl).toHaveBeenCalledWith('sb-1', {
      rows: 24,
      cols: 80,
      container: undefined,
    });
    const socket = mocks.sockets[0];
    const terminal = mocks.terminals[0];
    expect(socket.url).toBe('ws://localhost/cubeapi/v1/sandboxes/sb-1/terminal');

    socket.open();
    terminal.emitData('ls\n');
    terminal.emitResize({ rows: 40, cols: 120 });
    socket.receive({ type: 'output', data: btoa('ok\n') });

    expect(socket.sent.map((message) => JSON.parse(message))).toEqual([
      { type: 'resize', rows: 24, cols: 80 },
      { type: 'input', data: 'ls\n' },
      { type: 'resize', rows: 40, cols: 120 },
    ]);
    expect(terminal.write).toHaveBeenCalledWith('ok\n');
  });

  it('lets users select a specific container for the terminal session', () => {
    render(
      <TerminalPanel
        sandboxID="sb-1"
        open
        onOpenChange={vi.fn()}
        containers={[
          { containerID: 'default', name: 'Default' },
          { containerID: 'worker-1', name: 'Worker' },
        ]}
      />,
    );

    fireEvent.change(screen.getByTitle('container'), { target: { value: 'worker-1' } });

    expect(mocks.terminalUrl).toHaveBeenLastCalledWith('sb-1', {
      rows: 24,
      cols: 80,
      container: 'worker-1',
    });
  });

  it('shows server close reasons without adding an abnormal reconnect prompt', () => {
    render(<TerminalPanel sandboxID="sb-1" open onOpenChange={vi.fn()} />);

    const socket = mocks.sockets[0];
    const terminal = mocks.terminals[0];

    socket.open();
    socket.receive({
      type: 'exit',
      message: 'terminal session idle timeout',
    });
    socket.close();

    expect(terminal.writeln).toHaveBeenCalledWith(
      '\r\nclosed: terminal session idle timeout',
    );
    expect(terminal.writeln).not.toHaveBeenCalledWith('\r\nstatus.reconnecting');
  });
});
