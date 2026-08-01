// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useCallback, useEffect, useRef, useState } from 'react';
import { sandboxApi } from '@/api/client';
import { OPS_BASE } from '@/lib/api';

export type TerminalStatus =
  'idle' | 'connecting' | 'connected' | 'reconnecting' | 'disconnected' | 'exited' | 'error';

/** Frames the CubeOps terminal bridge sends to the browser. */
interface ServerMessage {
  type: 'ready' | 'output' | 'exit' | 'error' | 'pong';
  data?: string;
  pid?: number;
  exitCode?: number;
  message?: string;
}

export interface TerminalSocketOptions {
  sandboxID: string;
  /** Connect only while this is true; flipping it to false tears down. */
  enabled: boolean;
  /** Called with each decoded chunk of PTY output. */
  onOutput: (data: string) => void;
  /** Current terminal size, read when (re)connecting. */
  getSize: () => { cols: number; rows: number };
}

export interface TerminalSocket {
  status: TerminalStatus;
  /** Human-readable detail for the error/exited states. */
  detail: string | null;
  exitCode: number | null;
  /** True when the shell is still alive and a reconnect can resume it. */
  canReconnect: boolean;
  sendInput: (data: string) => void;
  sendResize: (cols: number, rows: number) => void;
  reconnect: () => void;
}

/** Base64 helpers that survive multi-byte UTF-8 (btoa is Latin-1 only). */
function encodeBase64(input: string): string {
  const bytes = new TextEncoder().encode(input);
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

function decodeBase64(input: string): string {
  const binary = atob(input);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return new TextDecoder().decode(bytes);
}

const PING_INTERVAL_MS = 30_000;

/**
 * Drives one web-terminal session: fetches a one-time ticket, opens the
 * WebSocket, and bridges it to the caller's xterm instance.
 *
 * The PTY outlives an abnormal socket drop, so `reconnect()` reattaches to the
 * same shell by PID rather than starting a fresh one.
 */
export function useTerminalSocket({
  sandboxID,
  enabled,
  onOutput,
  getSize,
}: TerminalSocketOptions): TerminalSocket {
  const [status, setStatus] = useState<TerminalStatus>('idle');
  const [detail, setDetail] = useState<string | null>(null);
  const [exitCode, setExitCode] = useState<number | null>(null);
  const [canReconnect, setCanReconnect] = useState(false);

  const socketRef = useRef<WebSocket | null>(null);
  const pidRef = useRef<number | null>(null);
  const pingRef = useRef<ReturnType<typeof setInterval> | null>(null);
  // Identifies the current connection attempt. Every teardown bumps it, so a
  // ticket request still in flight from an earlier attempt knows to abandon
  // itself instead of opening a socket nobody owns — which would leak a live
  // PTY inside the sandbox. A plain "closing" boolean cannot express this: a
  // newer attempt resets it while the older await is still pending.
  const runIDRef = useRef(0);
  // Latest callbacks, so the connect effect does not re-run when the parent
  // re-renders with new closures.
  const onOutputRef = useRef(onOutput);
  const getSizeRef = useRef(getSize);
  onOutputRef.current = onOutput;
  getSizeRef.current = getSize;

  const clearPing = useCallback(() => {
    if (pingRef.current !== null) {
      clearInterval(pingRef.current);
      pingRef.current = null;
    }
  }, []);

  const teardown = useCallback(() => {
    runIDRef.current += 1;
    clearPing();
    const socket = socketRef.current;
    socketRef.current = null;
    if (socket && socket.readyState === WebSocket.OPEN) {
      // Tell the backend this was intentional so it kills the shell instead
      // of parking it for a reconnect that will never come.
      socket.send(JSON.stringify({ type: 'close' }));
    }
    socket?.close();
  }, [clearPing]);

  const connect = useCallback(
    async (resumePID: number | null) => {
      if (typeof WebSocket === 'undefined') {
        setStatus('error');
        setDetail('unsupported');
        return;
      }
      const runID = runIDRef.current;
      setStatus(resumePID === null ? 'connecting' : 'reconnecting');
      setDetail(null);

      let ticket: string;
      let wsPath: string;
      try {
        const resp = await sandboxApi.createTerminalTicket(sandboxID);
        ticket = resp.ticket;
        wsPath = resp.wsPath;
      } catch (err) {
        if (runIDRef.current !== runID) return;
        setStatus('error');
        setDetail(err instanceof Error ? err.message : String(err));
        setCanReconnect(false);
        return;
      }
      // The dialog closed (or reconnected) while the ticket was in flight:
      // abandon this attempt rather than opening an unowned socket.
      if (runIDRef.current !== runID) return;

      const { cols, rows } = getSizeRef.current();
      const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws';
      const params = new URLSearchParams({
        ticket,
        cols: String(cols),
        rows: String(rows),
      });
      if (resumePID !== null) params.set('pid', String(resumePID));
      const url = `${scheme}://${window.location.host}${OPS_BASE}${wsPath}?${params.toString()}`;

      const socket = new WebSocket(url);
      socketRef.current = socket;

      socket.onopen = () => {
        clearPing();
        pingRef.current = setInterval(() => {
          if (socket.readyState === WebSocket.OPEN) {
            socket.send(JSON.stringify({ type: 'ping' }));
          }
        }, PING_INTERVAL_MS);
      };

      socket.onmessage = (event) => {
        // Frames still in flight from a socket we have abandoned must not
        // reach the terminal.
        if (runIDRef.current !== runID) return;
        let msg: ServerMessage;
        try {
          msg = JSON.parse(event.data as string);
        } catch {
          return;
        }
        switch (msg.type) {
          case 'ready':
            pidRef.current = msg.pid ?? null;
            setStatus('connected');
            setCanReconnect(false);
            setExitCode(null);
            break;
          case 'output':
            if (msg.data) onOutputRef.current(decodeBase64(msg.data));
            break;
          case 'exit':
            // The shell itself ended: there is nothing to reconnect to.
            pidRef.current = null;
            setExitCode(msg.exitCode ?? 0);
            setStatus('exited');
            setCanReconnect(false);
            break;
          case 'error':
            setDetail(msg.message ?? null);
            setStatus('error');
            // A failed reattach means the shell is gone for good.
            setCanReconnect(false);
            pidRef.current = null;
            break;
          case 'pong':
            break;
        }
      };

      socket.onclose = () => {
        clearPing();
        if (socketRef.current === socket) socketRef.current = null;
        // A close we asked for (teardown / reconnect) is not a dropped link.
        if (runIDRef.current !== runID) return;
        setStatus((prev) => {
          // exit / error already explain themselves; don't overwrite them.
          if (prev === 'exited' || prev === 'error') return prev;
          setCanReconnect(pidRef.current !== null);
          return 'disconnected';
        });
      };

      socket.onerror = () => {
        // onclose runs next and owns the state transition; without a close
        // code there is nothing more specific to report here.
      };
    },
    [sandboxID, clearPing],
  );

  useEffect(() => {
    if (!enabled) return;
    void connect(null);
    return () => {
      teardown();
      setStatus('idle');
      setDetail(null);
      setExitCode(null);
      setCanReconnect(false);
      pidRef.current = null;
    };
  }, [enabled, connect, teardown]);

  const sendInput = useCallback((data: string) => {
    const socket = socketRef.current;
    if (socket?.readyState !== WebSocket.OPEN) return;
    socket.send(JSON.stringify({ type: 'input', data: encodeBase64(data) }));
  }, []);

  const sendResize = useCallback((cols: number, rows: number) => {
    const socket = socketRef.current;
    if (socket?.readyState !== WebSocket.OPEN) return;
    socket.send(JSON.stringify({ type: 'resize', cols, rows }));
  }, []);

  const reconnect = useCallback(() => {
    if (socketRef.current) teardown();
    void connect(pidRef.current);
  }, [connect, teardown]);

  return { status, detail, exitCode, canReconnect, sendInput, sendResize, reconnect };
}
