// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { ApiError, ops } from '@/lib/api';
import {
  MAX_TERMINAL_PAYLOAD_BYTES,
  TERMINAL_GRANT_PREFIX,
  TERMINAL_SUBPROTOCOL,
  assertTerminalDimensions,
  decodeServerFrame,
  encodeResizeFrame,
  encodeStdinFrames,
  isTerminalReasonCode,
  type TerminalReasonCode,
} from './protocol';

const RETRY_DELAYS_MS = [1000, 2000, 4000] as const;
const MAX_OUTBOUND_BUFFERED_BYTES = 256 * 1024;
const MAX_INBOUND_PENDING_BYTES = 256 * 1024;

const WS_CONNECTING = 0;
const WS_OPEN = 1;

export interface TerminalContainer {
  containerId: string;
  name?: string;
  type?: string;
  status: number;
}

export interface TerminalGrantRequest {
  kind: 'open' | 'resume';
  sandboxId: string;
  containerId?: string;
  sessionId?: string;
  cols: number;
  rows: number;
  lastOffset?: number;
}

export interface TerminalGrantMetadata {
  wsUrl: string;
  sessionId: string;
  sandboxId: string;
  containerId: string;
  expiresAt: string;
  containers: TerminalContainer[];
}

interface TerminalGrantResponse extends TerminalGrantMetadata {
  token: string;
}

export interface TerminalConnection {
  socket: WebSocket;
  metadata: TerminalGrantMetadata;
}

export type TerminalConnectionState =
  | { kind: 'connecting' }
  | { kind: 'connected' }
  | { kind: 'detached'; attempt: 1 | 2 | 3 }
  | {
      kind: 'closed';
      reason: TerminalReasonCode;
      exitCode?: number;
      canStartNewSession: boolean;
    };

export interface TerminalSessionSnapshot {
  state: TerminalConnectionState;
  metadata: TerminalGrantMetadata | null;
  lastOffset: number;
}

export interface TerminalOpenedEvent {
  resumed: boolean;
  truncated: boolean;
  replayFrom: number;
}

export class TerminalClientError extends Error {
  constructor(readonly code: TerminalReasonCode) {
    super(code);
    this.name = 'TerminalClientError';
  }
}

export interface TerminalConnectionDependencies {
  origin?: string;
  socketFactory?: (url: string, protocols: string[]) => WebSocket;
  opsRequest?: (path: string, init: RequestInit) => Promise<unknown>;
}

export async function requestTerminalConnection(
  request: TerminalGrantRequest,
  signal: AbortSignal,
  dependencies: TerminalConnectionDependencies = {},
): Promise<TerminalConnection> {
  const response = parseGrantResponse(
    await (dependencies.opsRequest ?? ops)('/terminal/grants', {
      method: 'POST',
      body: JSON.stringify(request),
      signal,
    }),
  );
  const url = normalizeTerminalWebSocketURL(
    response.wsUrl,
    dependencies.origin ?? globalThis.location?.href,
  );
  let token = response.token;
  response.token = '';
  const protocols = [TERMINAL_SUBPROTOCOL, `${TERMINAL_GRANT_PREFIX}${token}`];
  let socket: WebSocket;
  try {
    socket = (dependencies.socketFactory ?? ((target, values) => new WebSocket(target, values)))(
      url,
      protocols,
    );
  } catch {
    throw new TerminalClientError('PROTOCOL_ERROR');
  } finally {
    token = '';
  }
  socket.binaryType = 'arraybuffer';
  return {
    socket,
    metadata: {
      wsUrl: url,
      sessionId: response.sessionId,
      sandboxId: response.sandboxId,
      containerId: response.containerId,
      expiresAt: response.expiresAt,
      containers: response.containers,
    },
  };
}

export function normalizeTerminalWebSocketURL(raw: string, pageLocation?: string): string {
  if (!pageLocation) throw new TerminalClientError('PROTOCOL_ERROR');
  let page: URL;
  let target: URL;
  try {
    page = new URL(pageLocation);
    target = new URL(raw, page);
  } catch {
    throw new TerminalClientError('PROTOCOL_ERROR');
  }
  if (
    (page.protocol !== 'http:' && page.protocol !== 'https:') ||
    !['http:', 'https:', 'ws:', 'wss:'].includes(target.protocol) ||
    target.host !== page.host ||
    target.username !== '' ||
    target.password !== '' ||
    target.search !== '' ||
    target.hash !== ''
  ) {
    throw new TerminalClientError('PROTOCOL_ERROR');
  }
  target.protocol = page.protocol === 'https:' ? 'wss:' : 'ws:';
  return target.href;
}

type TimeoutHandle = ReturnType<typeof globalThis.setTimeout>;

interface TerminalTimers {
  setTimeout(callback: () => void, delay: number): TimeoutHandle;
  clearTimeout(handle: TimeoutHandle): void;
}

type ConnectionRequester = (
  request: TerminalGrantRequest,
  signal: AbortSignal,
) => Promise<TerminalConnection>;

export interface TerminalSessionClientOptions {
  sandboxId: string;
  containerId?: string;
  cols: number;
  rows: number;
  requester?: ConnectionRequester;
  timers?: TerminalTimers;
  onSnapshot(snapshot: TerminalSessionSnapshot): void;
  onOutput(data: string): void;
  onOpened?(event: TerminalOpenedEvent): void;
}

interface SocketBinding {
  socket: WebSocket;
  generation: number;
  kind: 'open' | 'resume';
  opened: boolean;
  handlers: {
    open: EventListener;
    message: EventListener;
    close: EventListener;
    error: EventListener;
  };
}

export class TerminalSessionClient {
  private readonly requester: ConnectionRequester;
  private readonly timers: TerminalTimers;
  private readonly sandboxId: string;
  private readonly requestedContainerId?: string;
  private readonly onSnapshot: TerminalSessionClientOptions['onSnapshot'];
  private readonly onOutput: TerminalSessionClientOptions['onOutput'];
  private readonly onOpened?: TerminalSessionClientOptions['onOpened'];
  private state: TerminalConnectionState = { kind: 'connecting' };
  private metadata: TerminalGrantMetadata | null = null;
  private cols: number;
  private rows: number;
  private lastOffset = 0;
  private retryAttempt = 0;
  private generation = 0;
  private started = false;
  private disposed = false;
  private final = false;
  private exitCode: number | undefined;
  private grantAbort: AbortController | null = null;
  private retryTimer: TimeoutHandle | null = null;
  private resizeTimer: TimeoutHandle | null = null;
  private binding: SocketBinding | null = null;
  private decoder = new TextDecoder();
  private inboundTail: Promise<void> = Promise.resolve();
  private inboundPendingBytes = 0;

  constructor(options: TerminalSessionClientOptions) {
    assertTerminalDimensions(options.cols, options.rows);
    this.sandboxId = options.sandboxId;
    this.requestedContainerId = options.containerId;
    this.cols = options.cols;
    this.rows = options.rows;
    this.requester =
      options.requester ?? ((request, signal) => requestTerminalConnection(request, signal));
    this.timers =
      options.timers ??
      ({
        setTimeout: (callback, delay) => globalThis.setTimeout(callback, delay),
        clearTimeout: (handle) => globalThis.clearTimeout(handle),
      } satisfies TerminalTimers);
    this.onSnapshot = options.onSnapshot;
    this.onOutput = options.onOutput;
    this.onOpened = options.onOpened;
  }

  start(): void {
    if (this.started || this.disposed) return;
    this.started = true;
    this.emitSnapshot();
    void this.connect('open');
  }

  sendInput(data: string): boolean {
    const socket = this.binding?.socket;
    if (this.disposed || this.state.kind !== 'connected' || socket?.readyState !== WS_OPEN) {
      return false;
    }
    const frames = encodeStdinFrames(data);
    // Budget the whole payload up front: a multi-frame input (e.g. a large
    // paste split across 64 KiB frames) must be all-or-nothing. Sending the
    // first frames and then hitting the budget would deliver a truncated
    // command to the PTY with no way for the caller to recover.
    const totalBytes = frames.reduce((sum, frame) => sum + frame.byteLength, 0);
    if (socket.bufferedAmount + totalBytes > MAX_OUTBOUND_BUFFERED_BYTES) {
      return false;
    }
    for (const frame of frames) {
      try {
        socket.send(frame);
      } catch {
        return false;
      }
    }
    return true;
  }

  resize(cols: number, rows: number): void {
    assertTerminalDimensions(cols, rows);
    this.cols = cols;
    this.rows = rows;
    if (this.resizeTimer !== null) this.timers.clearTimeout(this.resizeTimer);
    this.resizeTimer = this.timers.setTimeout(() => {
      this.resizeTimer = null;
      const socket = this.binding?.socket;
      if (this.disposed || this.state.kind !== 'connected' || socket?.readyState !== WS_OPEN) {
        return;
      }
      const frame = encodeResizeFrame(this.cols, this.rows);
      if (socket.bufferedAmount + frame.byteLength > MAX_OUTBOUND_BUFFERED_BYTES) return;
      try {
        socket.send(frame);
      } catch {
        // The close event owns retry decisions; never create a client-side queue.
      }
    }, 100);
  }

  snapshot(): TerminalSessionSnapshot {
    return {
      state: this.state,
      metadata: this.metadata ? cloneMetadata(this.metadata) : null,
      lastOffset: this.lastOffset,
    };
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.final = true;
    this.generation++;
    this.clearTimers();
    this.grantAbort?.abort();
    this.grantAbort = null;
    this.releaseSocket(true);
  }

  private async connect(kind: 'open' | 'resume'): Promise<void> {
    if (this.disposed || this.final) return;
    const generation = ++this.generation;
    this.grantAbort?.abort();
    const controller = new AbortController();
    this.grantAbort = controller;
    const request: TerminalGrantRequest = {
      kind,
      sandboxId: kind === 'open' ? this.sandboxId : (this.metadata?.sandboxId ?? this.sandboxId),
      cols: this.cols,
      rows: this.rows,
      ...(kind === 'open'
        ? this.requestedContainerId
          ? { containerId: this.requestedContainerId }
          : {}
        : {
            containerId: this.metadata?.containerId,
            sessionId: this.metadata?.sessionId,
            lastOffset: this.lastOffset,
          }),
    };
    let connection: TerminalConnection;
    try {
      connection = await this.requester(request, controller.signal);
    } catch (error) {
      if (this.isStale(generation) || isAbortError(error)) return;
      this.grantAbort = null;
      this.handleConnectFailure(error, kind);
      return;
    }
    this.grantAbort = null;
    if (this.isStale(generation)) {
      closeSocket(connection.socket);
      return;
    }
    this.metadata = cloneMetadata(connection.metadata);
    this.emitSnapshot();
    this.bindSocket(connection.socket, generation, kind);
  }

  private bindSocket(socket: WebSocket, generation: number, kind: 'open' | 'resume'): void {
    this.releaseSocket(false);
    socket.binaryType = 'arraybuffer';
    const binding: SocketBinding = {
      socket,
      generation,
      kind,
      opened: false,
      handlers: {
        open: (() => this.handleOpen(binding)) as EventListener,
        message: ((event: MessageEvent) =>
          this.enqueueMessage(binding, event.data)) as EventListener,
        close: ((event: CloseEvent) => this.handleClose(binding, event)) as EventListener,
        error: (() => undefined) as EventListener,
      },
    };
    this.binding = binding;
    socket.addEventListener('open', binding.handlers.open);
    socket.addEventListener('message', binding.handlers.message);
    socket.addEventListener('close', binding.handlers.close);
    socket.addEventListener('error', binding.handlers.error);
  }

  private handleOpen(binding: SocketBinding): void {
    if (!this.isCurrent(binding)) return;
    if (binding.socket.protocol !== TERMINAL_SUBPROTOCOL) {
      this.failProtocol(binding, 1008);
    }
  }

  private enqueueMessage(binding: SocketBinding, data: unknown): void {
    if (!this.isCurrent(binding)) return;
    const size = incomingSize(data);
    if (size < 0) {
      this.failProtocol(binding, 1008);
      return;
    }
    if (size > MAX_TERMINAL_PAYLOAD_BYTES + 1) {
      this.failProtocol(binding, 1009);
      return;
    }
    if (this.inboundPendingBytes + size > MAX_INBOUND_PENDING_BYTES) {
      this.finish('SLOW_CONSUMER');
      closeSocket(binding.socket, 1013);
      return;
    }
    this.inboundPendingBytes += size;
    this.inboundTail = this.inboundTail
      .then(async () => {
        if (!this.isCurrent(binding)) return;
        try {
          const frameData = await resolveIncoming(data);
          if (!this.isCurrent(binding)) return;
          this.handleFrame(binding, decodeServerFrame(frameData));
        } catch {
          if (this.isCurrent(binding)) this.failProtocol(binding, 1008);
        }
      })
      .finally(() => {
        this.inboundPendingBytes = Math.max(0, this.inboundPendingBytes - size);
      });
  }

  private handleFrame(binding: SocketBinding, frame: ReturnType<typeof decodeServerFrame>): void {
    if (!this.isCurrent(binding) || this.final) return;
    switch (frame.type) {
      case 'opened': {
        if (!this.metadata || frame.sessionId !== this.metadata.sessionId || binding.opened) {
          this.failProtocol(binding, 1008);
          return;
        }
        if (binding.kind === 'resume' && frame.replay.from < this.lastOffset) {
          this.failProtocol(binding, 1008);
          return;
        }
        binding.opened = true;
        if (frame.replay.truncated) this.decoder = new TextDecoder();
        this.lastOffset = frame.replay.from;
        this.retryAttempt = 0;
        this.state = { kind: 'connected' };
        this.emitSnapshot();
        this.onOpened?.({
          resumed: binding.kind === 'resume',
          truncated: frame.replay.truncated,
          replayFrom: frame.replay.from,
        });
        return;
      }
      case 'stdout': {
        if (!binding.opened) {
          this.failProtocol(binding, 1008);
          return;
        }
        const nextOffset = this.lastOffset + frame.payload.byteLength;
        if (!Number.isSafeInteger(nextOffset)) {
          this.failProtocol(binding, 1008);
          return;
        }
        this.lastOffset = nextOffset;
        const decoded = this.decoder.decode(frame.payload, { stream: true });
        if (decoded) this.onOutput(decoded);
        return;
      }
      case 'exit':
        this.exitCode = frame.exitCode;
        return;
      case 'error':
        this.finishFromStatus(binding, frame.code);
        return;
      case 'close':
        this.finishFromStatus(binding, frame.reason);
    }
  }

  private handleClose(binding: SocketBinding, event: CloseEvent): void {
    if (!this.isCurrent(binding)) return;
    this.detachBinding(binding);
    if (this.disposed || this.final) return;
    if (this.exitCode !== undefined) {
      this.finish('RUNTIME_EXITED', this.exitCode);
      return;
    }
    if (event.code === 1000) {
      this.finish('USER_CLOSED', this.exitCode);
      return;
    }
    const serverReason = serverCloseCodeReason(event.code);
    if (serverReason) {
      this.finish(serverReason);
      return;
    }
    if (!this.metadata?.sessionId) {
      this.finish('INTERNAL');
      return;
    }
    this.scheduleResume();
  }

  private handleConnectFailure(error: unknown, kind: 'open' | 'resume'): void {
    const code = stableErrorCode(error);
    if (kind === 'resume' && code === null) {
      this.scheduleResume();
      return;
    }
    this.finish(code ?? 'INTERNAL');
  }

  private scheduleResume(): void {
    if (this.disposed || this.final) return;
    if (this.retryAttempt >= RETRY_DELAYS_MS.length) {
      this.finish('SESSION_LOST');
      return;
    }
    this.retryAttempt++;
    const attempt = this.retryAttempt as 1 | 2 | 3;
    this.state = { kind: 'detached', attempt };
    this.emitSnapshot();
    if (this.retryTimer !== null) this.timers.clearTimeout(this.retryTimer);
    this.retryTimer = this.timers.setTimeout(
      () => {
        this.retryTimer = null;
        void this.connect('resume');
      },
      RETRY_DELAYS_MS[attempt - 1],
    );
  }

  private finish(reason: TerminalReasonCode, exitCode = this.exitCode): void {
    if (this.final || this.disposed) return;
    this.final = true;
    this.clearTimers();
    this.grantAbort?.abort();
    this.grantAbort = null;
    const tail = this.decoder.decode();
    if (tail) this.onOutput(tail);
    this.state = {
      kind: 'closed',
      reason,
      ...(exitCode !== undefined ? { exitCode } : {}),
      canStartNewSession: reason !== 'USER_CLOSED',
    };
    this.emitSnapshot();
  }

  private finishFromStatus(
    binding: SocketBinding,
    reason: TerminalReasonCode,
    exitCode = this.exitCode,
  ): void {
    this.finish(reason, exitCode);
    if (this.binding === binding) this.releaseSocket(true);
  }

  private failProtocol(binding: SocketBinding, closeCode: 1008 | 1009): void {
    if (!this.isCurrent(binding)) return;
    this.finish('PROTOCOL_ERROR');
    closeSocket(binding.socket, closeCode);
  }

  private emitSnapshot(): void {
    if (!this.disposed) this.onSnapshot(this.snapshot());
  }

  private isStale(generation: number): boolean {
    return this.disposed || generation !== this.generation;
  }

  private isCurrent(binding: SocketBinding): boolean {
    return !this.disposed && this.binding === binding && binding.generation === this.generation;
  }

  private clearTimers(): void {
    if (this.retryTimer !== null) this.timers.clearTimeout(this.retryTimer);
    if (this.resizeTimer !== null) this.timers.clearTimeout(this.resizeTimer);
    this.retryTimer = null;
    this.resizeTimer = null;
  }

  private releaseSocket(close: boolean): void {
    const binding = this.binding;
    if (!binding) return;
    this.detachBinding(binding);
    if (close) closeSocket(binding.socket);
  }

  private detachBinding(binding: SocketBinding): void {
    binding.socket.removeEventListener('open', binding.handlers.open);
    binding.socket.removeEventListener('message', binding.handlers.message);
    binding.socket.removeEventListener('close', binding.handlers.close);
    binding.socket.removeEventListener('error', binding.handlers.error);
    if (this.binding === binding) this.binding = null;
  }
}

function parseGrantResponse(value: unknown): TerminalGrantResponse {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new TerminalClientError('PROTOCOL_ERROR');
  }
  const raw = value as Record<string, unknown>;
  const token = raw.token;
  const wsUrl = raw.wsUrl;
  const sessionId = raw.sessionId;
  const sandboxId = raw.sandboxId;
  const containerId = raw.containerId;
  const expiresAt = raw.expiresAt;
  if (
    typeof token !== 'string' ||
    !/^[A-Za-z0-9_-]+$/.test(token) ||
    typeof wsUrl !== 'string' ||
    typeof sessionId !== 'string' ||
    sessionId.length === 0 ||
    typeof sandboxId !== 'string' ||
    sandboxId.length === 0 ||
    typeof containerId !== 'string' ||
    containerId.length === 0 ||
    typeof expiresAt !== 'string' ||
    Number.isNaN(Date.parse(expiresAt)) ||
    !Array.isArray(raw.containers)
  ) {
    throw new TerminalClientError('PROTOCOL_ERROR');
  }
  const containers = raw.containers.map(parseContainer);
  return { token, wsUrl, sessionId, sandboxId, containerId, expiresAt, containers };
}

function parseContainer(value: unknown): TerminalContainer {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    throw new TerminalClientError('PROTOCOL_ERROR');
  }
  const raw = value as Record<string, unknown>;
  if (
    typeof raw.containerId !== 'string' ||
    raw.containerId.length === 0 ||
    (raw.name !== undefined && typeof raw.name !== 'string') ||
    (raw.type !== undefined && typeof raw.type !== 'string') ||
    typeof raw.status !== 'number' ||
    !Number.isInteger(raw.status)
  ) {
    throw new TerminalClientError('PROTOCOL_ERROR');
  }
  return {
    containerId: raw.containerId,
    ...(raw.name ? { name: raw.name as string } : {}),
    ...(raw.type ? { type: raw.type as string } : {}),
    status: raw.status,
  };
}

function cloneMetadata(metadata: TerminalGrantMetadata): TerminalGrantMetadata {
  return { ...metadata, containers: metadata.containers.map((container) => ({ ...container })) };
}

function stableErrorCode(error: unknown): TerminalReasonCode | null {
  if (error instanceof TerminalClientError) return error.code;
  if (error instanceof ApiError) {
    const body = error.body;
    if (body && typeof body === 'object' && 'error' in body) {
      const code = (body as { error?: unknown }).error;
      if (isTerminalReasonCode(code)) return code;
    }
    if (isTerminalReasonCode(error.message)) return error.message;
    // A generic ApiError (e.g. a transient 5xx or an nginx 502 without a
    // terminal code) is not an authoritative terminal verdict. Returning null
    // lets the resume path retry instead of permanently killing a session that
    // may still be alive on the cubelet.
    return null;
  }
  return null;
}

function isAbortError(error: unknown): boolean {
  return error instanceof DOMException && error.name === 'AbortError';
}

function serverCloseCodeReason(code: number): TerminalReasonCode | null {
  switch (code) {
    case 1008:
    case 1009:
      return 'PROTOCOL_ERROR';
    case 1011:
      return 'INTERNAL';
    case 1012:
      return 'SERVER_DRAINING';
    case 1013:
      return 'SLOW_CONSUMER';
    default:
      return null;
  }
}

function incomingSize(data: unknown): number {
  if (data instanceof ArrayBuffer) return data.byteLength;
  if (ArrayBuffer.isView(data)) return data.byteLength;
  if (typeof Blob !== 'undefined' && data instanceof Blob) return data.size;
  return -1;
}

async function resolveIncoming(data: unknown): Promise<ArrayBuffer | ArrayBufferView> {
  if (data instanceof ArrayBuffer || ArrayBuffer.isView(data)) return data;
  if (typeof Blob !== 'undefined' && data instanceof Blob) return data.arrayBuffer();
  throw new TerminalClientError('PROTOCOL_ERROR');
}

function closeSocket(socket: WebSocket, code = 1000): void {
  if (socket.readyState !== WS_CONNECTING && socket.readyState !== WS_OPEN) return;
  try {
    socket.close(code, code === 1000 ? 'terminal closed' : 'terminal protocol error');
  } catch {
    // Socket cleanup must stay best-effort and must never expose transport details.
  }
}
