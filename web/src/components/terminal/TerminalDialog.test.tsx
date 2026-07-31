// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { StrictMode } from 'react';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { TerminalDialog, readTerminalFontSize } from './TerminalDialog';
import { TerminalEntry } from './TerminalEntry';
import { TERMINAL_CHANNEL, TERMINAL_SUBPROTOCOL, decodeClientFrame } from '@/lib/terminal/protocol';

const xtermState = vi.hoisted(() => ({
  terminals: [] as Array<{
    cols: number;
    rows: number;
    options: Record<string, unknown>;
    writes: string[];
    disposed: number;
    dataHandler?: (data: string) => void;
  }>,
  fits: [] as Array<{
    fitCount: number;
    disposed: number;
    terminal?: { cols: number; rows: number };
  }>,
  cols: 100,
  rows: 30,
}));

vi.mock('@xterm/xterm', () => {
  class Terminal {
    cols = 80;
    rows = 24;
    options: Record<string, unknown>;
    writes: string[] = [];
    disposed = 0;
    dataHandler?: (data: string) => void;

    constructor(options: Record<string, unknown>) {
      this.options = { ...options };
      xtermState.terminals.push(this);
    }

    loadAddon(addon: { activate?(terminal: Terminal): void }) {
      addon.activate?.(this);
    }

    open(host: HTMLElement) {
      Object.defineProperty(host, 'clientWidth', { configurable: true, value: 900 });
      Object.defineProperty(host, 'clientHeight', { configurable: true, value: 520 });
    }

    onData(handler: (data: string) => void) {
      this.dataHandler = handler;
      return { dispose: vi.fn(() => (this.dataHandler = undefined)) };
    }

    write(data: string) {
      this.writes.push(data);
    }

    dispose() {
      this.disposed++;
    }
  }
  return { Terminal };
});

vi.mock('@xterm/addon-fit', () => {
  class FitAddon {
    fitCount = 0;
    disposed = 0;
    terminal?: { cols: number; rows: number };

    constructor() {
      xtermState.fits.push(this);
    }

    activate(terminal: { cols: number; rows: number }) {
      this.terminal = terminal;
    }

    fit() {
      this.fitCount++;
      if (this.terminal) {
        this.terminal.cols = xtermState.cols;
        this.terminal.rows = xtermState.rows;
      }
    }

    dispose() {
      this.disposed++;
    }
  }
  return { FitAddon };
});

class FakeResizeObserver {
  static instances: FakeResizeObserver[] = [];
  readonly observe = vi.fn();
  readonly unobserve = vi.fn();
  readonly disconnect = vi.fn();

  constructor(private readonly callback: ResizeObserverCallback) {
    FakeResizeObserver.instances.push(this);
  }

  trigger() {
    this.callback([], this as unknown as ResizeObserver);
  }
}

interface GrantMetadata {
  sessionId: string;
  sandboxId: string;
  containerId: string;
}

class FakeBrowserSocket extends EventTarget {
  static instances: FakeBrowserSocket[] = [];
  static grants = new Map<string, GrantMetadata>();
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;
  readonly url: string;
  readonly protocols: string[];
  protocol = TERMINAL_SUBPROTOCOL;
  binaryType: BinaryType = 'blob';
  bufferedAmount = 0;
  readyState = FakeBrowserSocket.CONNECTING;
  sent: Uint8Array[] = [];
  closeCodes: number[] = [];
  metadata: GrantMetadata;

  constructor(url: string, protocols: string | string[]) {
    super();
    this.url = url;
    this.protocols = typeof protocols === 'string' ? [protocols] : [...protocols];
    const grantProtocol = this.protocols.find((value) => value.startsWith('cube-grant.')) ?? '';
    const token = grantProtocol.slice('cube-grant.'.length);
    const metadata = FakeBrowserSocket.grants.get(token);
    if (!metadata) throw new Error('missing fake grant metadata');
    this.metadata = metadata;
    FakeBrowserSocket.instances.push(this);
    setTimeout(() => {
      if (this.readyState !== FakeBrowserSocket.CONNECTING) return;
      this.readyState = FakeBrowserSocket.OPEN;
      this.dispatchEvent(new Event('open'));
      this.message(
        status({
          type: 'opened',
          sessionId: this.metadata.sessionId,
          replay: { from: 0, truncated: false },
        }),
      );
    }, 0);
  }

  send(data: ArrayBuffer | ArrayBufferView) {
    const view =
      data instanceof ArrayBuffer
        ? new Uint8Array(data)
        : new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
    this.sent.push(new Uint8Array(view));
  }

  close(code = 1000) {
    this.closeCodes.push(code);
    this.readyState = FakeBrowserSocket.CLOSED;
  }

  message(data: ArrayBuffer | ArrayBufferView) {
    this.dispatchEvent(new MessageEvent('message', { data }));
  }

  serverStatus(value: Record<string, unknown>) {
    this.message(status(value));
  }
}

let rafCallbacks = new Map<number, FrameRequestCallback>();
let nextRaf = 1;
let grantCounter = 0;
let fullscreenElement: Element | null = null;
let grantContainers = defaultGrantContainers();

describe('TerminalEntry and TerminalDialog', () => {
  beforeEach(async () => {
    vi.useFakeTimers();
    localStorage.clear();
    FakeResizeObserver.instances = [];
    FakeBrowserSocket.instances = [];
    FakeBrowserSocket.grants.clear();
    xtermState.terminals = [];
    xtermState.fits = [];
    xtermState.cols = 100;
    xtermState.rows = 30;
    rafCallbacks = new Map();
    nextRaf = 1;
    grantCounter = 0;
    fullscreenElement = null;
    grantContainers = defaultGrantContainers();
    vi.stubGlobal('ResizeObserver', FakeResizeObserver);
    vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => {
      const id = nextRaf++;
      rafCallbacks.set(id, callback);
      return id;
    });
    vi.stubGlobal('cancelAnimationFrame', (id: number) => rafCallbacks.delete(id));
    vi.stubGlobal('WebSocket', FakeBrowserSocket);
    vi.stubGlobal('fetch', vi.fn(fakeGrantFetch));
    await act(async () => {
      const i18n = (await import('@/i18n')).default;
      await i18n.changeLanguage('en');
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
    delete (HTMLElement.prototype as Partial<HTMLElement>).requestFullscreen;
    Object.defineProperty(document, 'fullscreenElement', { configurable: true, value: null });
    Object.defineProperty(document, 'fullscreenEnabled', { configurable: true, value: false });
    delete (document as Partial<Document>).exitFullscreen;
  });

  it('renders a running-only accessible entry and disabled tooltip text', () => {
    const { rerender } = render(
      <TerminalEntry sandboxId="sandbox-a" state="paused" display="label" />,
    );
    const disabled = screen.getByRole('button', { name: 'Open terminal' });
    expect(disabled).toBeDisabled();
    expect(disabled).toHaveAttribute(
      'title',
      'Terminal is available only while the sandbox is running.',
    );

    rerender(<TerminalEntry sandboxId="sandbox-a" state="running" display="label" />);
    expect(screen.getByRole('button', { name: 'Open terminal' })).toBeEnabled();
  });

  it('waits for visible rAF fit before requesting a non-zero open grant', async () => {
    render(<TerminalDialog open onOpenChange={vi.fn()} sandboxId="sandbox-a" />);
    expect(fetch).not.toHaveBeenCalled();
    await flushRaf();
    await flushAsync();
    expect(fetch).toHaveBeenCalledTimes(1);
    const request = requestBody(0);
    expect(request).toMatchObject({ kind: 'open', sandboxId: 'sandbox-a', cols: 100, rows: 30 });
    expect(request.cols).toBeGreaterThan(0);
    expect(request.rows).toBeGreaterThan(0);
    await openPendingSockets();
    expect(screen.getByText('Connected')).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'primary' })).toBeInTheDocument();
    expect(screen.getByText('session-1')).toBeInTheDocument();
  });

  it('is StrictMode-safe and does not create duplicate grants or input handlers', async () => {
    const view = render(
      <StrictMode>
        <TerminalDialog open onOpenChange={vi.fn()} sandboxId="sandbox-a" />
      </StrictMode>,
    );
    await flushRaf();
    await flushAsync();
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(xtermState.terminals.filter((terminal) => terminal.disposed === 0)).toHaveLength(1);
    await openPendingSockets();
    view.unmount();
    expect(FakeBrowserSocket.instances[0].closeCodes).toContain(1000);
    const terminalObservers = FakeResizeObserver.instances.filter((observer) =>
      observer.observe.mock.calls.some(
        ([element]) =>
          element instanceof HTMLElement &&
          element.getAttribute('aria-label') === 'Interactive terminal output',
      ),
    );
    expect(terminalObservers).not.toHaveLength(0);
    expect(terminalObservers.every((observer) => observer.disconnect.mock.calls.length === 1)).toBe(
      true,
    );
    expect(xtermState.fits.every((fit) => fit.disposed === 1)).toBe(true);
  });

  it('opens a container menu from the tab bar, disables unavailable containers, and isolates tab close', async () => {
    render(<TerminalDialog open onOpenChange={vi.fn()} sandboxId="sandbox-a" />);
    await connectInitialDialog();

    fireEvent.keyDown(screen.getByRole('button', { name: 'New session' }), { key: 'ArrowDown' });
    const stopped = screen.getByRole('menuitem', { name: 'stopped not running' });
    expect(stopped).toHaveAttribute('data-disabled', '');
    fireEvent.click(screen.getByRole('menuitem', { name: 'worker' }));
    await flushRaf();
    await flushAsync();
    await openPendingSockets();

    expect(fetch).toHaveBeenCalledTimes(2);
    expect(requestBody(1)).toMatchObject({ kind: 'open', containerId: 'container-worker' });
    expect(screen.getAllByRole('tab')).toHaveLength(2);
    expect(FakeBrowserSocket.instances).toHaveLength(2);

    fireEvent.click(screen.getByRole('button', { name: 'Close primary' }));
    expect(FakeBrowserSocket.instances[0].closeCodes).toContain(1000);
    expect(FakeBrowserSocket.instances[1].closeCodes).toHaveLength(0);
    expect(screen.getAllByRole('tab')).toHaveLength(1);
  });

  it('starts a new tab directly when the sandbox has one container', async () => {
    grantContainers = [defaultGrantContainers()[0]];
    render(<TerminalDialog open onOpenChange={vi.fn()} sandboxId="sandbox-a" />);
    await connectInitialDialog();

    fireEvent.click(screen.getByRole('button', { name: 'New session' }));
    await flushRaf();
    await flushAsync();
    await openPendingSockets();

    expect(fetch).toHaveBeenCalledTimes(2);
    expect(requestBody(1)).toMatchObject({ kind: 'open', containerId: 'container-primary' });
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
    expect(screen.getAllByRole('tab')).toHaveLength(2);
  });

  it('disables a new session when every discovered container is unavailable', async () => {
    grantContainers = defaultGrantContainers().map((container) => ({ ...container, status: 0 }));
    render(<TerminalDialog open onOpenChange={vi.fn()} sandboxId="sandbox-a" />);
    await connectInitialDialog();

    expect(screen.getByRole('button', { name: 'New session' })).toBeDisabled();
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it('clears stale container choices when a later grant reports no containers', async () => {
    render(<TerminalDialog open onOpenChange={vi.fn()} sandboxId="sandbox-a" />);
    await connectInitialDialog();

    grantContainers = [];
    fireEvent.keyDown(screen.getByRole('button', { name: 'New session' }), { key: 'ArrowDown' });
    fireEvent.click(screen.getByRole('menuitem', { name: 'worker' }));
    await flushRaf();
    await flushAsync();
    await openPendingSockets();

    expect(screen.getByRole('button', { name: 'New session' })).toBeDisabled();
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  it('disables a closed tab retry after the selected target becomes unavailable', async () => {
    render(<TerminalDialog open onOpenChange={vi.fn()} sandboxId="sandbox-a" />);
    await connectInitialDialog();

    grantContainers = [];
    fireEvent.keyDown(screen.getByRole('button', { name: 'New session' }), { key: 'ArrowDown' });
    fireEvent.click(screen.getByRole('menuitem', { name: 'worker' }));
    await flushRaf();
    await flushAsync();
    await openPendingSockets();

    FakeBrowserSocket.instances[1].serverStatus({ type: 'error', code: 'SESSION_LOST' });
    await flushAsync();
    const retry = screen.getByRole('button', { name: 'Start new session' });
    expect(retry).toBeDisabled();
    fireEvent.click(retry);
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it('disables the empty-state action when no target remains available', async () => {
    render(<TerminalDialog open onOpenChange={vi.fn()} sandboxId="sandbox-a" />);
    await connectInitialDialog();

    grantContainers = [];
    fireEvent.keyDown(screen.getByRole('button', { name: 'New session' }), { key: 'ArrowDown' });
    fireEvent.click(screen.getByRole('menuitem', { name: 'worker' }));
    await flushRaf();
    await flushAsync();
    await openPendingSockets();

    const closeTab = () =>
      screen
        .getAllByRole('button')
        .find(
          (button) =>
            button.getAttribute('aria-label')?.startsWith('Close ') &&
            button.getAttribute('aria-label') !== 'Close terminal',
        );
    fireEvent.click(closeTab()!);
    fireEvent.click(closeTab()!);
    const emptyStateAction = screen.getByRole('button', { name: 'Start new session' });
    expect(emptyStateAction).toBeDisabled();
    fireEvent.click(emptyStateAction);
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it('persists font size 12..20, drives fullscreen fit, and reports fullscreen rejection', async () => {
    localStorage.setItem('cube.terminal.fontSize', 'invalid');
    expect(readTerminalFontSize()).toBe(14);
    render(<TerminalDialog open onOpenChange={vi.fn()} sandboxId="sandbox-a" />);
    await connectInitialDialog();

    fireEvent.click(screen.getByRole('button', { name: 'Increase font size' }));
    expect(localStorage.getItem('cube.terminal.fontSize')).toBe('15');
    expect(xtermState.terminals.at(-1)?.options.fontSize).toBe(15);
    fireEvent.click(screen.getByRole('button', { name: 'Decrease font size' }));
    expect(localStorage.getItem('cube.terminal.fontSize')).toBe('14');

    installFullscreenMocks();
    const fitBefore = xtermState.fits.at(-1)?.fitCount ?? 0;
    fireEvent.click(screen.getByRole('button', { name: 'Enter fullscreen' }));
    await flushAsync();
    await flushRaf();
    expect(fullscreenElement).not.toBeNull();
    expect((xtermState.fits.at(-1)?.fitCount ?? 0) > fitBefore).toBe(true);
    expect(screen.getByRole('button', { name: 'Exit fullscreen' })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Exit fullscreen' }));
    await flushAsync();
    expect(screen.getByRole('button', { name: 'Enter fullscreen' })).toBeInTheDocument();
    (HTMLElement.prototype.requestFullscreen as ReturnType<typeof vi.fn>).mockRejectedValueOnce(
      new DOMException('denied'),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Enter fullscreen' }));
    await flushAsync();
    expect(screen.getAllByText('The fullscreen request was denied.')).toHaveLength(2);
  });

  it('fits ResizeObserver changes with a 100ms debounce and shows translated terminal reasons', async () => {
    render(<TerminalDialog open onOpenChange={vi.fn()} sandboxId="sandbox-a" />);
    await connectInitialDialog();
    const socket = FakeBrowserSocket.instances[0];
    xtermState.cols = 120;
    xtermState.rows = 50;
    FakeResizeObserver.instances.at(-1)?.trigger();
    await flushRaf();
    await vi.advanceTimersByTimeAsync(99);
    expect(socket.sent).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1);
    expect(decodeClientFrame(socket.sent[0])).toEqual({ type: 'resize', cols: 120, rows: 50 });

    socket.serverStatus({ type: 'error', code: 'SESSION_LOST' });
    await flushAsync();
    expect(screen.getByText('The previous shell can no longer be resumed.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Start new session' })).toBeInTheDocument();
  });
});

async function fakeGrantFetch(_input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
  const request = JSON.parse(String(init?.body)) as Record<string, unknown>;
  const number = ++grantCounter;
  const token = `fake-grant-${number}`;
  const sessionId = typeof request.sessionId === 'string' ? request.sessionId : `session-${number}`;
  const containerId =
    typeof request.containerId === 'string' ? request.containerId : 'container-primary';
  const metadata = { sessionId, sandboxId: 'sandbox-a', containerId };
  FakeBrowserSocket.grants.set(token, metadata);
  return new Response(
    JSON.stringify({
      token,
      wsUrl: '/opsapi/v1/terminal/ws',
      ...metadata,
      expiresAt: '2026-07-30T10:30:00Z',
      containers: grantContainers,
    }),
    { status: 201, headers: { 'Content-Type': 'application/json' } },
  );
}

function defaultGrantContainers() {
  return [
    { containerId: 'container-primary', name: 'primary', type: 'sandbox', status: 1 },
    { containerId: 'container-worker', name: 'worker', type: 'service', status: 1 },
    { containerId: 'container-stopped', name: 'stopped', type: 'service', status: 0 },
  ];
}

function requestBody(call: number): Record<string, unknown> {
  const fetchMock = vi.mocked(fetch);
  return JSON.parse(String(fetchMock.mock.calls[call][1]?.body)) as Record<string, unknown>;
}

async function connectInitialDialog(): Promise<void> {
  await flushRaf();
  await flushAsync();
  await openPendingSockets();
}

async function openPendingSockets(): Promise<void> {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  await flushAsync();
}

async function flushRaf(): Promise<void> {
  const pending = [...rafCallbacks.entries()];
  rafCallbacks.clear();
  await act(async () => {
    for (const [, callback] of pending) callback(performance.now());
  });
}

async function flushAsync(): Promise<void> {
  await act(async () => {
    for (let index = 0; index < 12; index++) await Promise.resolve();
  });
}

function status(value: Record<string, unknown>): Uint8Array {
  const payload = new TextEncoder().encode(JSON.stringify(value));
  const frame = new Uint8Array(payload.byteLength + 1);
  frame[0] = TERMINAL_CHANNEL.status;
  frame.set(payload, 1);
  return frame;
}

function installFullscreenMocks(): void {
  Object.defineProperty(document, 'fullscreenEnabled', { configurable: true, value: true });
  Object.defineProperty(document, 'fullscreenElement', {
    configurable: true,
    get: () => fullscreenElement,
  });
  Object.defineProperty(HTMLElement.prototype, 'requestFullscreen', {
    configurable: true,
    value: vi.fn(async function (this: HTMLElement) {
      fullscreenElement = this;
      document.dispatchEvent(new Event('fullscreenchange'));
    }),
  });
  Object.defineProperty(document, 'exitFullscreen', {
    configurable: true,
    value: vi.fn(async () => {
      fullscreenElement = null;
      document.dispatchEvent(new Event('fullscreenchange'));
    }),
  });
}
