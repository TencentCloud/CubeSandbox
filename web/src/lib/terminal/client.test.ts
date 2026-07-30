// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '@/lib/api';
import {
  TerminalClientError,
  TerminalSessionClient,
  normalizeTerminalWebSocketURL,
  requestTerminalConnection,
  type TerminalConnection,
  type TerminalGrantRequest,
  type TerminalSessionSnapshot,
} from './client';
import {
  MAX_TERMINAL_PAYLOAD_BYTES,
  TERMINAL_CHANNEL,
  TERMINAL_GRANT_PREFIX,
  TERMINAL_SUBPROTOCOL,
  decodeClientFrame,
} from './protocol';

const encoder = new TextEncoder();

describe('terminal connection security', () => {
  beforeEach(() => {
    localStorage.clear();
    sessionStorage.clear();
  });

  it('uses a same-origin URL and exactly two strict WebSocket subprotocols', async () => {
    const socket = new FakeWebSocket();
    const socketFactory = vi.fn(() => socket as unknown as WebSocket);
    const opsRequest = vi.fn(async () => grantResponse('fake-grant-value'));
    const connection = await requestTerminalConnection(
      openRequest(),
      new AbortController().signal,
      {
        origin: 'https://cube.example.test/dashboard',
        opsRequest,
        socketFactory,
      },
    );

    expect(opsRequest).toHaveBeenCalledWith(
      '/terminal/grants',
      expect.objectContaining({ method: 'POST' }),
    );
    expect(socketFactory).toHaveBeenCalledWith('wss://cube.example.test/opsapi/v1/terminal/ws', [
      TERMINAL_SUBPROTOCOL,
      `${TERMINAL_GRANT_PREFIX}fake-grant-value`,
    ]);
    expect(JSON.stringify(connection.metadata)).not.toContain('fake-grant-value');
    expect(connection.metadata.wsUrl).not.toContain('fake-grant-value');
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
  });

  it('rejects cross-origin, credentialed, query, and fragment WebSocket URLs', () => {
    for (const raw of [
      'wss://other.example.test/opsapi/v1/terminal/ws',
      'wss://user@cube.example.test/opsapi/v1/terminal/ws',
      '/opsapi/v1/terminal/ws?grant=secret',
      '/opsapi/v1/terminal/ws#secret',
    ]) {
      expect(() => normalizeTerminalWebSocketURL(raw, 'https://cube.example.test/')).toThrow(
        TerminalClientError,
      );
    }
  });
});

describe('TerminalSessionClient', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('treats a normal close as terminal and never reconnects', async () => {
    const harness = createHarness();
    harness.client.start();
    await flush();
    harness.sockets[0].open();
    harness.sockets[0].message(opened(0));
    await flush();
    harness.sockets[0].serverClose(1000);
    await vi.runAllTimersAsync();

    expect(harness.requests).toHaveLength(1);
    expect(lastSnapshot(harness).state).toMatchObject({ kind: 'closed', reason: 'USER_CLOSED' });
  });

  it('retries abnormal transport loss at 1s, 2s, and 4s with a real resume request', async () => {
    const harness = createHarness();
    await openConnected(harness);
    harness.sockets[0].message(stdout('ready'));
    await flush();
    harness.sockets[0].serverClose(1006);
    expect(lastSnapshot(harness).state).toEqual({ kind: 'detached', attempt: 1 });

    await vi.advanceTimersByTimeAsync(999);
    expect(harness.requests).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    await flush();
    expect(harness.requests[1]).toEqual({
      kind: 'resume',
      sandboxId: 'sandbox-a',
      containerId: 'container-a',
      sessionId: 'session-a',
      cols: 80,
      rows: 24,
      lastOffset: 5,
    });

    harness.sockets[1].serverClose(1006);
    expect(lastSnapshot(harness).state).toEqual({ kind: 'detached', attempt: 2 });
    await vi.advanceTimersByTimeAsync(1999);
    expect(harness.requests).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    await flush();
    harness.sockets[2].serverClose(1006);
    expect(lastSnapshot(harness).state).toEqual({ kind: 'detached', attempt: 3 });
    await vi.advanceTimersByTimeAsync(3999);
    expect(harness.requests).toHaveLength(3);
    await vi.advanceTimersByTimeAsync(1);
    await flush();
    harness.sockets[3].serverClose(1006);
    expect(lastSnapshot(harness).state).toMatchObject({ kind: 'closed', reason: 'SESSION_LOST' });
  });

  it('preserves the decoder, terminal output, and byte offset across resume', async () => {
    const harness = createHarness();
    await openConnected(harness);
    const euro = encoder.encode('€');
    harness.sockets[0].message(binary(TERMINAL_CHANNEL.stdout, euro.subarray(0, 2)));
    await flush();
    expect(harness.output).toBe('');
    expect(harness.client.snapshot().lastOffset).toBe(2);

    harness.sockets[0].serverClose(1006);
    await vi.advanceTimersByTimeAsync(1000);
    await flush();
    const oldSocket = harness.sockets[0];
    oldSocket.message(stdout('ignored-old-generation'));
    harness.sockets[1].open();
    harness.sockets[1].message(opened(2));
    harness.sockets[1].message(binary(TERMINAL_CHANNEL.stdout, euro.subarray(2)));
    await flush();

    expect(harness.output).toBe('€');
    expect(harness.client.snapshot().lastOffset).toBe(3);
    expect(harness.openedEvents).toContainEqual({ resumed: true, truncated: false, replayFrom: 2 });
  });

  it('surfaces truncated replay and advances offsets by UTF-8 bytes, not JS characters', async () => {
    const harness = createHarness();
    await openConnected(harness);
    harness.sockets[0].message(stdout('中'));
    await flush();
    expect(harness.client.snapshot().lastOffset).toBe(3);
    harness.sockets[0].serverClose(1006);
    await vi.advanceTimersByTimeAsync(1000);
    await flush();
    harness.sockets[1].open();
    harness.sockets[1].message(opened(9, true));
    harness.sockets[1].message(stdout('🙂'));
    await flush();
    expect(harness.client.snapshot().lastOffset).toBe(13);
    expect(harness.openedEvents.at(-1)).toEqual({ resumed: true, truncated: true, replayFrom: 9 });
  });

  it('stops retrying when resume returns SESSION_LOST', async () => {
    const harness = createHarness((request, index) => {
      if (index > 0) {
        throw new ApiError(409, 'SESSION_LOST', { error: 'SESSION_LOST' });
      }
      return connectionFor(new FakeWebSocket(), request);
    });
    await openConnected(harness);
    harness.sockets[0].serverClose(1006);
    await vi.advanceTimersByTimeAsync(1000);
    await flush();
    expect(lastSnapshot(harness).state).toMatchObject({
      kind: 'closed',
      reason: 'SESSION_LOST',
      canStartNewSession: true,
    });
    await vi.runAllTimersAsync();
    expect(harness.requests).toHaveLength(2);
  });

  it('debounces resize for 100ms and sends only the latest valid dimensions', async () => {
    const harness = createHarness();
    await openConnected(harness);
    harness.client.resize(100, 40);
    harness.client.resize(120, 50);
    await vi.advanceTimersByTimeAsync(99);
    expect(harness.sockets[0].sent).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(1);
    expect(harness.sockets[0].sent).toHaveLength(1);
    expect(decodeClientFrame(harness.sockets[0].sent[0])).toEqual({
      type: 'resize',
      cols: 120,
      rows: 50,
    });
  });

  it('never queues stdin while unavailable or beyond the bounded WebSocket buffer', async () => {
    const harness = createHarness();
    harness.client.start();
    await flush();
    expect(harness.client.sendInput('before-open')).toBe(false);
    await finishOpen(harness);
    harness.sockets[0].bufferedAmount = 256 * 1024;
    expect(harness.client.sendInput('blocked')).toBe(false);
    expect(harness.sockets[0].sent).toHaveLength(0);

    harness.sockets[0].bufferedAmount = 0;
    expect(harness.client.sendInput('a'.repeat(MAX_TERMINAL_PAYLOAD_BYTES * 2 + 3))).toBe(true);
    expect(harness.sockets[0].sent.map((value) => value.byteLength)).toEqual([
      MAX_TERMINAL_PAYLOAD_BYTES + 1,
      MAX_TERMINAL_PAYLOAD_BYTES + 1,
      4,
    ]);
  });

  it('handles bounded Blob messages and disposes grant, socket, listeners, and timers', async () => {
    const captured: { signal?: AbortSignal } = {};
    const pendingRequester = vi.fn(
      (_request: TerminalGrantRequest, signal: AbortSignal) =>
        new Promise<TerminalConnection>(() => {
          captured.signal = signal;
        }),
    );
    const pending = createHarness(undefined, pendingRequester);
    pending.client.start();
    await flush();
    pending.client.dispose();
    expect(captured.signal?.aborted).toBe(true);

    const harness = createHarness();
    await openConnected(harness, true);
    harness.client.resize(90, 30);
    harness.sockets[0].serverClose(1006);
    harness.client.dispose();
    await vi.runAllTimersAsync();
    expect(harness.requests).toHaveLength(1);
    expect(harness.sockets[0].removedListeners).toBeGreaterThanOrEqual(4);
  });

  it('prefers an authoritative application close reason received after exit', async () => {
    const harness = createHarness();
    await openConnected(harness);
    harness.sockets[0].message(status('{"type":"exit","exitCode":9}'));
    await flush();

    expect(lastSnapshot(harness).state).toEqual({ kind: 'connected' });
    expect(harness.sockets[0].closeCalls).toHaveLength(0);

    harness.sockets[0].message(status('{"type":"close","reason":"SANDBOX_TRANSITION"}'));
    await flush();

    expect(lastSnapshot(harness).state).toEqual({
      kind: 'closed',
      reason: 'SANDBOX_TRANSITION',
      exitCode: 9,
      canStartNewSession: true,
    });
    expect(harness.sockets[0].closeCalls).toEqual([1000]);
    expect(harness.sockets[0].removedListeners).toBeGreaterThanOrEqual(4);
    harness.sockets[0].serverClose(1006);
    await vi.runAllTimersAsync();
    expect(harness.requests).toHaveLength(1);
  });

  it('prefers a typed application error received after exit', async () => {
    const harness = createHarness();
    await openConnected(harness);
    harness.sockets[0].message(status('{"type":"exit","exitCode":9}'));
    harness.sockets[0].message(status('{"type":"error","code":"SERVER_DRAINING"}'));
    await flush();

    expect(lastSnapshot(harness).state).toEqual({
      kind: 'closed',
      reason: 'SERVER_DRAINING',
      exitCode: 9,
      canStartNewSession: true,
    });
    expect(harness.sockets[0].closeCalls).toEqual([1000]);
    await vi.runAllTimersAsync();
    expect(harness.requests).toHaveLength(1);
  });

  it('finishes as runtime exited when a normal transport close follows exit', async () => {
    const harness = createHarness();
    await openConnected(harness);
    harness.sockets[0].message(status('{"type":"exit","exitCode":7}'));
    await flush();
    harness.sockets[0].serverClose(1000);
    await vi.runAllTimersAsync();

    expect(harness.requests).toHaveLength(1);
    expect(lastSnapshot(harness).state).toEqual({
      kind: 'closed',
      reason: 'RUNTIME_EXITED',
      exitCode: 7,
      canStartNewSession: true,
    });
  });

  it('never reconnects when an abnormal transport close follows exit', async () => {
    const harness = createHarness();
    await openConnected(harness);
    harness.sockets[0].message(status('{"type":"exit","exitCode":137}'));
    await flush();
    harness.sockets[0].serverClose(1006);
    await vi.runAllTimersAsync();

    expect(harness.requests).toHaveLength(1);
    expect(lastSnapshot(harness).state).toEqual({
      kind: 'closed',
      reason: 'RUNTIME_EXITED',
      exitCode: 137,
      canStartNewSession: true,
    });
  });

  it('does not reconnect after an application close status without exit', async () => {
    const harness = createHarness();
    await openConnected(harness);
    harness.sockets[0].message(status('{"type":"close","reason":"SANDBOX_TRANSITION"}'));
    await flush();

    expect(harness.sockets[0].closeCalls).toEqual([1000]);
    expect(harness.sockets[0].removedListeners).toBeGreaterThanOrEqual(4);
    harness.sockets[0].serverClose(1006);
    await vi.runAllTimersAsync();
    expect(harness.requests).toHaveLength(1);
    expect(lastSnapshot(harness).state).toMatchObject({
      kind: 'closed',
      reason: 'SANDBOX_TRANSITION',
    });
  });
});

interface Harness {
  client: TerminalSessionClient;
  requests: TerminalGrantRequest[];
  sockets: FakeWebSocket[];
  snapshots: TerminalSessionSnapshot[];
  output: string;
  openedEvents: unknown[];
}

function createHarness(
  resolver?: (request: TerminalGrantRequest, index: number) => TerminalConnection,
  requesterOverride?: (
    request: TerminalGrantRequest,
    signal: AbortSignal,
  ) => Promise<TerminalConnection>,
): Harness {
  const requests: TerminalGrantRequest[] = [];
  const sockets: FakeWebSocket[] = [];
  const snapshots: TerminalSessionSnapshot[] = [];
  const harness = {
    client: null as unknown as TerminalSessionClient,
    requests,
    sockets,
    snapshots,
    output: '',
    openedEvents: [] as unknown[],
  };
  const requester =
    requesterOverride ??
    (async (request: TerminalGrantRequest) => {
      const index = requests.length;
      requests.push({ ...request });
      if (resolver) {
        const connection = resolver(request, index);
        sockets.push(connection.socket as unknown as FakeWebSocket);
        return connection;
      }
      const socket = new FakeWebSocket();
      sockets.push(socket);
      return connectionFor(socket, request);
    });
  harness.client = new TerminalSessionClient({
    sandboxId: 'sandbox-a',
    cols: 80,
    rows: 24,
    requester: async (request, signal) => {
      if (requesterOverride) requests.push({ ...request });
      return requester(request, signal);
    },
    onSnapshot: (snapshot) => snapshots.push(snapshot),
    onOutput: (data) => {
      harness.output += data;
    },
    onOpened: (event) => harness.openedEvents.push(event),
  });
  return harness;
}

async function openConnected(harness: Harness, blob = false): Promise<void> {
  harness.client.start();
  await flush();
  await finishOpen(harness, blob);
}

async function finishOpen(harness: Harness, blob = false): Promise<void> {
  harness.sockets[0].open();
  const message = opened(0);
  if (blob) {
    const copied = new Uint8Array(message.byteLength);
    copied.set(message);
    const value = new Blob([copied.buffer]);
    Object.defineProperty(value, 'arrayBuffer', {
      value: async () =>
        message.buffer.slice(message.byteOffset, message.byteOffset + message.byteLength),
    });
    harness.sockets[0].message(value);
  } else {
    harness.sockets[0].message(message);
  }
  await flush();
  expect(lastSnapshot(harness).state).toEqual({ kind: 'connected' });
}

function connectionFor(socket: FakeWebSocket, request: TerminalGrantRequest): TerminalConnection {
  return {
    socket: socket as unknown as WebSocket,
    metadata: {
      wsUrl: 'ws://cube.test/opsapi/v1/terminal/ws',
      sessionId: request.sessionId ?? 'session-a',
      sandboxId: 'sandbox-a',
      containerId: request.containerId ?? 'container-a',
      expiresAt: '2026-07-30T10:00:00Z',
      containers: [{ containerId: 'container-a', name: 'primary', type: 'sandbox', status: 1 }],
    },
  };
}

function lastSnapshot(harness: Harness): TerminalSessionSnapshot {
  const value = harness.snapshots.at(-1);
  if (!value) throw new Error('missing snapshot');
  return value;
}

function openRequest(): TerminalGrantRequest {
  return { kind: 'open', sandboxId: 'sandbox-a', cols: 80, rows: 24 };
}

function grantResponse(token: string) {
  return {
    token,
    wsUrl: '/opsapi/v1/terminal/ws',
    sessionId: 'session-a',
    sandboxId: 'sandbox-a',
    containerId: 'container-a',
    expiresAt: '2026-07-30T10:00:00Z',
    containers: [{ containerId: 'container-a', name: 'primary', type: 'sandbox', status: 1 }],
  };
}

function opened(from: number, truncated = false): Uint8Array {
  return status(
    JSON.stringify({
      type: 'opened',
      sessionId: 'session-a',
      replay: { from, truncated },
    }),
  );
}

function stdout(value: string): Uint8Array {
  return binary(TERMINAL_CHANNEL.stdout, encoder.encode(value));
}

function status(value: string): Uint8Array {
  return binary(TERMINAL_CHANNEL.status, encoder.encode(value));
}

function binary(channel: number, payload: Uint8Array): Uint8Array {
  const frame = new Uint8Array(payload.byteLength + 1);
  frame[0] = channel;
  frame.set(payload, 1);
  return frame;
}

async function flush(): Promise<void> {
  for (let index = 0; index < 12; index++) await Promise.resolve();
}

class FakeWebSocket extends EventTarget {
  binaryType: BinaryType = 'blob';
  bufferedAmount = 0;
  protocol = TERMINAL_SUBPROTOCOL;
  readyState = 0;
  sent: Uint8Array[] = [];
  closeCalls: number[] = [];
  removedListeners = 0;

  override removeEventListener(
    type: string,
    callback: EventListenerOrEventListenerObject | null,
    options?: EventListenerOptions | boolean,
  ): void {
    this.removedListeners++;
    super.removeEventListener(type, callback, options);
  }

  send(data: ArrayBufferView | ArrayBuffer): void {
    const bytes =
      data instanceof ArrayBuffer
        ? new Uint8Array(data)
        : new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
    this.sent.push(new Uint8Array(bytes));
  }

  close(code = 1000): void {
    this.closeCalls.push(code);
    this.readyState = 2;
  }

  open(): void {
    this.readyState = 1;
    this.dispatchEvent(new Event('open'));
  }

  message(data: unknown): void {
    this.dispatchEvent(new MessageEvent('message', { data }));
  }

  serverClose(code: number): void {
    this.readyState = 3;
    this.dispatchEvent(new CloseEvent('close', { code, wasClean: code === 1000 }));
  }
}
