// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useRef, useState, useCallback, useEffect } from 'react';

export type ConnectionState = 'disconnected' | 'connecting' | 'connected' | 'reconnecting';

interface UseTerminalSocketOptions {
  sandboxID: string;
  container?: string;
  onMessage: (data: ArrayBuffer) => void;
  onClose: (reason: string) => void;
}

interface UseTerminalSocketReturn {
  connectionState: ConnectionState;
  send: (data: string | ArrayBuffer) => void;
  connect: () => void;
  disconnect: () => void;
}

const INITIAL_RETRY_DELAY_MS = 1000;

/**
 * Manages the WebSocket lifecycle for a terminal session.
 *
 * Features:
 * - Indefinite reconnection with exponential backoff (30s ceiling)
 * - Connection state tracking
 * - Binary and text frame sending (text for resize control messages)
 */
export function useTerminalSocket({
  sandboxID,
  container = 'default',
  onMessage,
  onClose,
}: UseTerminalSocketOptions): UseTerminalSocketReturn {
  const wsRef = useRef<WebSocket | null>(null);
  const [connectionState, setConnectionState] = useState<ConnectionState>('disconnected');
  const retryCountRef = useRef(0);
  const retryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const mountedRef = useRef(true);
  const explicitDisconnectRef = useRef(false);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (retryTimerRef.current) clearTimeout(retryTimerRef.current);
      if (wsRef.current && (wsRef.current.readyState === WebSocket.OPEN || wsRef.current.readyState === WebSocket.CONNECTING)) {
        wsRef.current.close(1000, 'component unmounted');
      }
    };
  }, []);

  const buildWsUrl = useCallback(() => {
    const sessionToken = localStorage.getItem('cube.session') ?? '';
    const isSecure = window.location.protocol === 'https:';
    const proto = isSecure ? 'wss' : 'ws';
    const host = window.location.host;
    return `${proto}://${host}/cubeapi/v1/sandboxes/${sandboxID}/terminal?token=${encodeURIComponent(sessionToken)}&container=${encodeURIComponent(container)}`;
  }, [sandboxID, container]);

  const connect = useCallback(() => {
    if (wsRef.current && (wsRef.current.readyState === WebSocket.OPEN || wsRef.current.readyState === WebSocket.CONNECTING)) {
      return;
    }

    explicitDisconnectRef.current = false;
    setConnectionState(retryCountRef.current > 0 ? 'reconnecting' : 'connecting');
    const url = buildWsUrl();
    const ws = new WebSocket(url);
    ws.binaryType = 'arraybuffer';
    wsRef.current = ws;

    ws.onopen = () => {
      if (!mountedRef.current) return;
      retryCountRef.current = 0;
      setConnectionState('connected');
    };

    ws.onmessage = (event: MessageEvent) => {
      if (!mountedRef.current) return;
      if (event.data instanceof ArrayBuffer) {
        onMessage(event.data);
      }
    };

    ws.onclose = (event: CloseEvent) => {
      if (!mountedRef.current) return;
      const reason = event.reason || 'connection closed';

      if (event.code === 1000 || explicitDisconnectRef.current) {
        // Clean close or explicit disconnect
        explicitDisconnectRef.current = false;
        setConnectionState('disconnected');
        onClose(reason);
        return;
      }

      // Unexpected close — retry indefinitely with exponential backoff
      retryCountRef.current += 1;
      setConnectionState('reconnecting');
      const delay = Math.min(INITIAL_RETRY_DELAY_MS * Math.pow(2, retryCountRef.current - 1), 30000);
      retryTimerRef.current = setTimeout(() => {
        if (mountedRef.current) connect();
      }, delay);
    };

    ws.onerror = () => {
      // onclose will fire after onerror; handle reconnection there
    };
  }, [buildWsUrl, onMessage, onClose]);

  const disconnect = useCallback(() => {
    if (retryTimerRef.current) {
      clearTimeout(retryTimerRef.current);
      retryTimerRef.current = null;
    }
    explicitDisconnectRef.current = true; // prevent reconnect
    if (wsRef.current) {
      wsRef.current.close(1000, 'user disconnect');
      wsRef.current = null;
    }
    setConnectionState('disconnected');
  }, []);

  const send = useCallback((data: string | ArrayBuffer) => {
    if (wsRef.current && wsRef.current.readyState === WebSocket.OPEN) {
      wsRef.current.send(data);
    }
  }, []);

  return { connectionState, send, connect, disconnect };
}