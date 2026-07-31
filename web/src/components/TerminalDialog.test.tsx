// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { sandboxApi } from '@/api/client';
import { TerminalDialog } from './TerminalDialog';

const terminalMocks = vi.hoisted(() => {
  const instances: Array<{
    rows: number;
    cols: number;
    options: Record<string, unknown>;
    writes: string[];
    customKeyHandler?: (event: KeyboardEvent) => boolean;
    selection: string;
    emitData(data: string): void;
    paste(data: string): void;
    reset: ReturnType<typeof vi.fn>;
    focus: ReturnType<typeof vi.fn>;
    dispose: ReturnType<typeof vi.fn>;
  }> = [];

  const translate = (key: string, options?: Record<string, unknown>) => {
    const values: Record<string, string> = {
      'terminal.title': `Terminal ${options?.sandboxID ?? ''}`,
      'terminal.description': 'Interactive shell',
      'terminal.mainContainerOnly': 'Main container only',
      'terminal.resizeHint': 'Drag the lower-right corner to resize.',
      'terminal.empty': 'No terminal tabs are open.',
      'terminal.status.connecting': 'Connecting',
      'terminal.status.connected': 'Connected',
      'terminal.status.closed': 'Terminal closed',
      'terminal.status.connectionLost': 'Connection lost. Reconnect.',
      'terminal.status.connectionLostReason': `Connection lost: ${options?.reason}. Reconnect.`,
      'terminal.status.error': 'Terminal error',
      'terminal.status.protocolError': 'Protocol error',
      'terminal.status.exited': `Exited ${options?.code ?? 0}`,
      'terminal.status.sessionLimit': 'Maximum 5 terminal tabs',
      'terminal.status.clipboardError': 'Clipboard error',
      'terminal.status.newShell': 'Starting a new shell',
      'terminal.tabs.list': 'Terminal sessions',
      'terminal.tabs.label': `Shell ${options?.index ?? ''}`,
      'terminal.tabs.new': 'New terminal session',
      'terminal.tabs.close': `Close ${options?.label ?? ''}`,
      'terminal.actions.copy': 'Copy',
      'terminal.actions.paste': 'Paste',
      'terminal.actions.reconnect': 'Reconnect',
      'terminal.actions.close': 'Close terminal',
    };
    return values[key] ?? key;
  };

  return {
    instances,
    createTerminalSession: vi.fn(),
    translate,
    Terminal: class {
      rows = 24;
      cols = 80;
      options: Record<string, unknown>;
      writes: string[] = [];
      selection = '';
      customKeyHandler?: (event: KeyboardEvent) => boolean;
      private dataHandlers: Array<(data: string) => void> = [];

      constructor(options: Record<string, unknown>) {
        this.options = options;
        instances.push(this);
      }

      loadAddon() {}

      open(host: HTMLElement) {
        const element = document.createElement('div');
        element.className = 'xterm';
        host.appendChild(element);
      }

      attachCustomKeyEventHandler(handler: (event: KeyboardEvent) => boolean) {
        this.customKeyHandler = handler;
      }

      onData(handler: (data: string) => void) {
        this.dataHandlers.push(handler);
        return { dispose: vi.fn() };
      }

      emitData(data: string) {
        this.dataHandlers.forEach((handler) => handler(data));
      }

      paste(data: string) {
        this.emitData(data);
      }

      write(data: Uint8Array | string) {
        this.writes.push(typeof data === 'string' ? data : new TextDecoder().decode(data));
      }

      writeln(data: string) {
        this.writes.push(data);
      }

      getSelection() {
        return this.selection;
      }

      reset = vi.fn();
      focus = vi.fn();
      dispose = vi.fn();
    },
  };
});

vi.mock('@/api/client', () => ({
  sandboxApi: {
    createTerminalSession: terminalMocks.createTerminalSession,
  },
}));

vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: terminalMocks.translate,
  }),
}));

vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit = vi.fn();
  },
}));

vi.mock('@xterm/xterm', () => ({
  Terminal: terminalMocks.Terminal,
}));

class MockWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  static instances: MockWebSocket[] = [];

  binaryType = '';
  readyState = MockWebSocket.CONNECTING;
  sent: unknown[] = [];
  closed = false;
  onopen: (() => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;

  constructor(
    public url: string,
    public protocols?: string | string[],
  ) {
    MockWebSocket.instances.push(this);
  }

  send(data: unknown) {
    this.sent.push(data);
  }

  close(code = 1000, reason = '') {
    if (this.readyState === MockWebSocket.CLOSED) return;
    this.closed = true;
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.(closeEvent(code, reason, true));
  }

  open() {
    this.readyState = MockWebSocket.OPEN;
    this.onopen?.();
  }

  message(data: string | ArrayBuffer) {
    this.onmessage?.(new MessageEvent('message', { data }));
  }

  serverClose(code: number, reason = '') {
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.(closeEvent(code, reason, terminalClean(code)));
  }
}

function closeEvent(code: number, reason: string, wasClean: boolean): CloseEvent {
  const event = new Event('close') as CloseEvent;
  Object.defineProperties(event, {
    code: { value: code },
    reason: { value: reason },
    wasClean: { value: wasClean },
  });
  return event;
}

function terminalClean(code: number): boolean {
  return code === 1000 || code === 1001;
}

function decodeBinary(value: unknown): string {
  return new TextDecoder().decode(value as Uint8Array);
}

function renderTerminal(overrides: Partial<React.ComponentProps<typeof TerminalDialog>> = {}) {
  const props = {
    open: true,
    onOpenChange: vi.fn(),
    sandboxID: 'sandbox-1',
    ...overrides,
  };
  return render(<TerminalDialog {...props} />);
}

beforeEach(() => {
  terminalMocks.instances.length = 0;
  MockWebSocket.instances.length = 0;
  terminalMocks.createTerminalSession.mockReset();
  let grantSequence = 0;
  terminalMocks.createTerminalSession.mockImplementation(async () => {
    grantSequence += 1;
    return {
      grant: `grant-${grantSequence}`,
      protocol: 'cube-terminal.v1',
      websocketURL: '/opsapi/v1/terminal/sandboxes/sandbox-1/ws',
    };
  });
  vi.stubGlobal('WebSocket', MockWebSocket);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('TerminalDialog', () => {
  it('allows the terminal window to resize within the viewport', () => {
    renderTerminal();

    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveAttribute('data-resizable', 'true');
    expect(dialog).toHaveClass('resize');
    expect(dialog).toHaveStyle({
      minWidth: 'min(28rem, calc(100vw - 2rem))',
      minHeight: 'min(22rem, calc(100vh - 2rem))',
      maxWidth: 'calc(100vw - 2rem)',
      maxHeight: 'calc(100vh - 2rem)',
    });
    expect(screen.getByText('Drag the lower-right corner to resize.')).toHaveClass('sr-only');
  });

  it('opens the first shell and bridges resize, input, and output', async () => {
    renderTerminal();

    await waitFor(() => expect(sandboxApi.createTerminalSession).toHaveBeenCalledWith('sandbox-1'));
    expect(screen.getByRole('tab', { name: 'Shell 1' })).toHaveAttribute('aria-selected', 'true');
    expect(terminalMocks.instances[0].options.scrollback).toBe(5000);

    const socket = MockWebSocket.instances[0];
    expect(socket.url).toBe('ws://localhost/opsapi/v1/terminal/sandboxes/sandbox-1/ws');
    expect(socket.protocols).toEqual(['cube-terminal.v1', 'cube-terminal.grant.grant-1']);
    socket.open();
    socket.message(JSON.stringify({ type: 'status', message: 'connected' }));

    await waitFor(() =>
      expect(socket.sent).toContainEqual(JSON.stringify({ type: 'resize', rows: 24, cols: 80 })),
    );
    expect(screen.getByText('Connected')).toBeInTheDocument();

    terminalMocks.instances[0].emitData('ls\r');
    expect(decodeBinary(socket.sent.at(-1))).toBe('ls\r');
    const output = new window.ArrayBuffer(14);
    new Uint8Array(output).set(new TextEncoder().encode('\u001b[32mgreen\u001b[0m'));
    socket.message(output);
    await waitFor(() =>
      expect(terminalMocks.instances[0].writes).toContain('\u001b[32mgreen\u001b[0m'),
    );
  });

  it('keeps up to five terminal tabs isolated and closes only one tab', async () => {
    renderTerminal();
    await waitFor(() => expect(sandboxApi.createTerminalSession).toHaveBeenCalledTimes(1));

    for (let count = 2; count <= 5; count += 1) {
      fireEvent.click(screen.getByTitle('New terminal session'));
      await waitFor(() => expect(sandboxApi.createTerminalSession).toHaveBeenCalledTimes(count));
    }
    expect(screen.getByTitle('Maximum 5 terminal tabs')).toBeDisabled();
    expect(MockWebSocket.instances).toHaveLength(5);

    MockWebSocket.instances.forEach((socket) => {
      socket.open();
      socket.message(JSON.stringify({ type: 'status' }));
    });
    terminalMocks.instances[0].emitData('first\r');
    terminalMocks.instances[4].emitData('fifth\r');
    expect(decodeBinary(MockWebSocket.instances[0].sent.at(-1))).toBe('first\r');
    expect(decodeBinary(MockWebSocket.instances[4].sent.at(-1))).toBe('fifth\r');

    fireEvent.click(screen.getByTitle('Close Shell 5'));
    await waitFor(() =>
      expect(screen.queryByRole('tab', { name: 'Shell 5' })).not.toBeInTheDocument(),
    );
    expect(MockWebSocket.instances[4].closed).toBe(true);
    expect(MockWebSocket.instances[0].closed).toBe(false);
    expect(screen.getByRole('tab', { name: 'Shell 4' })).toHaveAttribute('aria-selected', 'true');
  });

  it('waits for manual reconnect and obtains a fresh grant for a new shell', async () => {
    const onSessionActiveChange = vi.fn();
    renderTerminal({ onSessionActiveChange });
    await waitFor(() => expect(sandboxApi.createTerminalSession).toHaveBeenCalledTimes(1));
    const first = MockWebSocket.instances[0];
    first.open();
    first.message(JSON.stringify({ type: 'status' }));
    await waitFor(() => expect(screen.getByText('Connected')).toBeInTheDocument());

    first.serverClose(1011, 'upstream reset');
    await waitFor(() =>
      expect(screen.getByText('Connection lost: upstream reset. Reconnect.')).toBeInTheDocument(),
    );
    expect(sandboxApi.createTerminalSession).toHaveBeenCalledTimes(1);
    expect(onSessionActiveChange).toHaveBeenLastCalledWith(false);

    fireEvent.click(screen.getByRole('button', { name: 'Reconnect' }));
    await waitFor(() => expect(sandboxApi.createTerminalSession).toHaveBeenCalledTimes(2));
    expect(terminalMocks.instances[0].reset).toHaveBeenCalledTimes(1);
    expect(MockWebSocket.instances[1].protocols).toEqual([
      'cube-terminal.v1',
      'cube-terminal.grant.grant-2',
    ]);
    expect(onSessionActiveChange).toHaveBeenLastCalledWith(true);
  });

  it('supports explicit and Ctrl/Cmd+Shift clipboard operations', async () => {
    const clipboard = {
      readText: vi.fn().mockResolvedValue('pasted text'),
      writeText: vi.fn().mockResolvedValue(undefined),
    };
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: clipboard,
    });
    renderTerminal();
    await waitFor(() => expect(sandboxApi.createTerminalSession).toHaveBeenCalledTimes(1));
    const socket = MockWebSocket.instances[0];
    socket.open();
    socket.message(JSON.stringify({ type: 'status' }));
    await waitFor(() => expect(screen.getByText('Connected')).toBeInTheDocument());

    const terminal = terminalMocks.instances[0];
    terminal.selection = 'selected text';
    fireEvent.click(screen.getByRole('button', { name: 'Copy' }));
    await waitFor(() => expect(clipboard.writeText).toHaveBeenCalledWith('selected text'));
    fireEvent.click(screen.getByRole('button', { name: 'Paste' }));
    await waitFor(() => expect(clipboard.readText).toHaveBeenCalledTimes(1));
    expect(decodeBinary(socket.sent.at(-1))).toBe('pasted text');

    terminal.selection = 'shortcut selection';
    expect(
      terminal.customKeyHandler?.({
        key: 'c',
        ctrlKey: true,
        metaKey: false,
        shiftKey: true,
      } as KeyboardEvent),
    ).toBe(false);
    await waitFor(() => expect(clipboard.writeText).toHaveBeenCalledWith('shortcut selection'));

    expect(
      terminal.customKeyHandler?.({
        key: 'v',
        ctrlKey: false,
        metaKey: true,
        shiftKey: true,
      } as KeyboardEvent),
    ).toBe(false);
    await waitFor(() => expect(clipboard.readText).toHaveBeenCalledTimes(2));
  });

  it('shows protocol errors and cleans every connection on unmount', async () => {
    const rendered = renderTerminal();
    await waitFor(() => expect(sandboxApi.createTerminalSession).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByTitle('New terminal session'));
    await waitFor(() => expect(sandboxApi.createTerminalSession).toHaveBeenCalledTimes(2));
    MockWebSocket.instances.forEach((socket) => socket.open());
    MockWebSocket.instances[0].message('not-json');
    fireEvent.click(screen.getByRole('tab', { name: 'Shell 1' }));
    await waitFor(() => expect(screen.getByText('Protocol error')).toBeInTheDocument());

    rendered.unmount();
    expect(MockWebSocket.instances.every((socket) => socket.closed)).toBe(true);
    expect(
      terminalMocks.instances.every((terminal) => terminal.dispose.mock.calls.length === 1),
    ).toBe(true);
  });
});
