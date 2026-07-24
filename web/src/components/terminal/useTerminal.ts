// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useCallback, useEffect, useRef, useState } from 'react';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebLinksAddon } from '@xterm/addon-web-links';
import { ensureFreshToken } from '@/lib/api';

export type TerminalStatus = 'connecting' | 'ready' | 'disconnected' | 'exited' | 'error';

const DEFAULT_FONT_SIZE = 13;
const MIN_FONT_SIZE = 10;
const MAX_FONT_SIZE = 22;
// How long to wait for the WS handshake + first server frame before declaring
// the connection failed and offering a reconnect.
const CONNECT_TIMEOUT_MS = 15_000;
// Trailing-edge debounce for `resize` WS messages. ResizeObserver → fit() →
// term.onResize can fire at display refresh rate during window drags; the
// server only needs the settled size.
const RESIZE_DEBOUNCE_MS = 100;
// WebSocket subprotocol prefix carrying the session token. Keeps the token out
// of the URL (server access logs, browser history). The backend reads it from
// Sec-WebSocket-Protocol but never selects it back.
const TOKEN_SUBPROTOCOL_PREFIX = 'cube-terminal.';

// Token-free base subprotocol the backend selects in the 101 response when
// offered — Chrome requires a selection when subprotocols were offered.
const BASE_SUBPROTOCOL = 'cube-terminal';

// Server sends raw PTY bytes as standard base64; decode to bytes and hand them
// to xterm (which accepts Uint8Array and does its own UTF-8 decoding).
function decodeBase64(data: string): Uint8Array {
  const bin = atob(data);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

// btoa is binary-string based, so UTF-8 encode first, then build a binary string.
function encodeBase64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}

function buildWsUrl(sandboxID: string, cols: number, rows: number, containerID?: string): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = new URL(
    `${proto}//${window.location.host}/cubeapi/v1/sandboxes/${encodeURIComponent(sandboxID)}/terminal/ws`,
  );
  url.searchParams.set('cols', String(cols));
  url.searchParams.set('rows', String(rows));
  // Optional target container (ID or name); omitted means the primary container.
  if (containerID) url.searchParams.set('container', containerID);
  return url.toString();
}

// Browsers cannot set WebSocket headers, so the session token travels as a
// subprotocol (`cube-terminal.<token>`) instead of a URL query param. The
// token-free base protocol `cube-terminal` is offered first: the server
// selects exactly that one in the 101 response (Chrome aborts the handshake
// when offered subprotocols go unanswered), so the token is never echoed
// back. Returns undefined for auth-disabled deployments (empty token) so
// the WebSocket is constructed without protocols.
function wsProtocols(token: string | null): string[] | undefined {
  return token ? [BASE_SUBPROTOCOL, TOKEN_SUBPROTOCOL_PREFIX + token] : undefined;
}

interface TerminalSession {
  containerRef: React.RefCallback<HTMLDivElement>;
  status: TerminalStatus;
  exitCode: number | null;
  errorMessage: string | null;
  reconnect: () => void;
  fontSize: number;
  increaseFontSize: () => void;
  decreaseFontSize: () => void;
}

/**
 * Owns one xterm.js instance plus its WebSocket session for a single sandbox.
 * Each TerminalDialog calls this hook independently, so multiple terminals can
 * coexist. Everything is torn down when `open` flips to false. `containerID`
 * selects the target container (multi-container sandboxes); changing it
 * reconnects the session against the new container.
 */
export function useTerminal(
  sandboxID: string,
  open: boolean,
  containerID?: string,
): TerminalSession {
  // The container lives inside a Radix Dialog portal whose mount timing is not
  // synchronized with this effect, so a plain ref + effect can fire before the
  // element exists and never retry. A callback ref + state re-runs the effect
  // exactly when the element appears.
  const [containerEl, setContainerEl] = useState<HTMLDivElement | null>(null);
  const containerRef = useCallback((el: HTMLDivElement | null) => setContainerEl(el), []);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const [status, setStatus] = useState<TerminalStatus>('connecting');
  // Mirror of `status` readable from timeouts/ws handlers without an updater,
  // so side effects (ws.close, setErrorMessage) stay out of setState updaters —
  // React may invoke updaters twice under StrictMode.
  const statusRef = useRef<TerminalStatus>('connecting');
  const [exitCode, setExitCode] = useState<number | null>(null);
  const [errorMessage, setErrorMessage] = useState<string | null>(null);
  const [session, setSession] = useState(0);
  const [fontSize, setFontSize] = useState(DEFAULT_FONT_SIZE);

  const reconnect = useCallback(() => setSession((s) => s + 1), []);
  const increaseFontSize = useCallback(
    () => setFontSize((s) => Math.min(MAX_FONT_SIZE, s + 1)),
    [],
  );
  const decreaseFontSize = useCallback(
    () => setFontSize((s) => Math.max(MIN_FONT_SIZE, s - 1)),
    [],
  );

  useEffect(() => {
    if (!open || !containerEl) return;
    const container = containerEl;

    const updateStatus = (next: TerminalStatus) => {
      statusRef.current = next;
      setStatus(next);
    };
    updateStatus('connecting');
    setExitCode(null);
    setErrorMessage(null);

    const term = new Terminal({
      cursorBlink: true,
      fontSize,
      fontFamily:
        '"JetBrains Mono Variable", "JetBrains Mono", ui-monospace, Menlo, Consolas, monospace',
      theme: {
        background: '#0b0e14',
        foreground: '#d6deeb',
        cursor: '#7aa2f7',
        selectionBackground: '#33415c',
      },
    });
    termRef.current = term;
    const fit = new FitAddon();
    fitRef.current = fit;
    term.loadAddon(fit);
    term.loadAddon(new WebLinksAddon());
    term.open(container);
    fit.fit();

    // Ctrl+Shift+V paste (selection copy works natively in xterm).
    term.attachCustomKeyEventHandler((event) => {
      if (
        event.type === 'keydown' &&
        event.ctrlKey &&
        event.shiftKey &&
        event.key.toLowerCase() === 'v'
      ) {
        navigator.clipboard
          .readText()
          .then((text) => term.paste(text))
          .catch(() => {
            /* clipboard permission denied */
          });
        return false;
      }
      return true;
    });
    // Right-click paste, where the clipboard permission is granted.
    const onContextMenu = (event: MouseEvent) => {
      event.preventDefault();
      navigator.clipboard
        .readText()
        .then((text) => term.paste(text))
        .catch(() => {
          /* clipboard permission denied */
        });
    };

    let closedByUs = false;
    let ws: WebSocket | null = null;
    let watchdog: number | undefined;
    let resizeTimer: number | undefined;
    let pendingResize: { cols: number; rows: number } | null = null;
    let inputDisposable: { dispose(): void } | null = null;
    let resizeDisposable: { dispose(): void } | null = null;
    let observer: ResizeObserver | null = null;

    // Connect asynchronously: refresh the access token first so a long-idle
    // page does not fail the WS handshake on an expired token (the HTTP layer
    // auto-refreshes on 401, but the subprotocol token bypasses it).
    void (async () => {
      const token = await ensureFreshToken();
      if (closedByUs) return;

      try {
        ws = new WebSocket(
          buildWsUrl(sandboxID, term.cols, term.rows, containerID),
          wsProtocols(token),
        );
      } catch (err) {
        setErrorMessage(String(err));
        updateStatus('error');
        return;
      }
      const socket = ws;
      // Server frames are JSON text; ask for ArrayBuffer (not Blob) on any
      // stray binary frame so onmessage can detect and ignore it cheaply.
      socket.binaryType = 'arraybuffer';

      container.addEventListener('contextmenu', onContextMenu);

      // Watchdog: if the handshake neither succeeds nor fails promptly (e.g. a
      // stalled proxy), surface an error with a reconnect option instead of
      // spinning on "connecting" forever.
      watchdog = window.setTimeout(() => {
        if (statusRef.current !== 'connecting') return;
        setErrorMessage(null);
        socket.close();
        updateStatus('error');
      }, CONNECT_TIMEOUT_MS);

      const sendResizeNow = (cols: number, rows: number) => {
        if (socket.readyState === WebSocket.OPEN) {
          socket.send(JSON.stringify({ type: 'resize', cols, rows }));
        }
      };

      // Trailing-edge debounce: coalesce a burst of resizes (window drags) into
      // one message carrying the final size, sent 100 ms after the burst settles.
      const sendResize = (cols: number, rows: number) => {
        pendingResize = { cols, rows };
        if (resizeTimer !== undefined) window.clearTimeout(resizeTimer);
        resizeTimer = window.setTimeout(() => {
          resizeTimer = undefined;
          const pending = pendingResize;
          pendingResize = null;
          if (pending) sendResizeNow(pending.cols, pending.rows);
        }, RESIZE_DEBOUNCE_MS);
      };

      inputDisposable = term.onData((data) => {
        if (socket.readyState === WebSocket.OPEN) {
          socket.send(JSON.stringify({ type: 'input', data: encodeBase64(data) }));
        }
      });
      resizeDisposable = term.onResize(({ cols, rows }) => sendResize(cols, rows));

      observer = new ResizeObserver(() => {
        try {
          fit.fit();
        } catch {
          /* container not laid out yet */
        }
      });
      observer.observe(container);

      socket.onopen = () => {
        // Tell the server the initial size right away; any resize fired while
        // the socket was still CONNECTING was dropped by sendResizeNow's
        // readyState guard (and its debounced replay may also have raced ahead).
        if (resizeTimer !== undefined) {
          window.clearTimeout(resizeTimer);
          resizeTimer = undefined;
          pendingResize = null;
        }
        sendResizeNow(term.cols, term.rows);
      };
      socket.onmessage = (event) => {
        if (typeof event.data !== 'string') {
          console.warn('[terminal] ignoring non-text WebSocket frame');
          return;
        }
        let msg: { type?: string; data?: string; code?: number; message?: string };
        try {
          msg = JSON.parse(event.data);
        } catch {
          return;
        }
        switch (msg.type) {
          case 'ready':
            updateStatus('ready');
            term.focus();
            break;
          case 'output':
            if (typeof msg.data === 'string') term.write(decodeBase64(msg.data));
            break;
          case 'exit':
            setExitCode(typeof msg.code === 'number' ? msg.code : null);
            updateStatus('exited');
            break;
          case 'error':
            setErrorMessage(msg.message ?? null);
            updateStatus('error');
            break;
        }
      };
      socket.onclose = () => {
        if (closedByUs) return;
        if (statusRef.current !== 'exited' && statusRef.current !== 'error') {
          updateStatus('disconnected');
        }
      };
      socket.onerror = () => {
        if (closedByUs) return;
        if (statusRef.current !== 'ready') updateStatus('error');
      };
    })();

    return () => {
      closedByUs = true;
      if (watchdog !== undefined) window.clearTimeout(watchdog);
      if (resizeTimer !== undefined) {
        window.clearTimeout(resizeTimer);
        resizeTimer = undefined;
      }
      pendingResize = null;
      observer?.disconnect();
      container.removeEventListener('contextmenu', onContextMenu);
      inputDisposable?.dispose();
      resizeDisposable?.dispose();
      ws?.close();
      term.dispose();
      termRef.current = null;
      fitRef.current = null;
    };
    // `session` bumps on reconnect; fontSize is applied by the effect below.
    // `containerID` switching tears the session down and reconnects against
    // the new container.
  }, [open, sandboxID, session, containerEl, containerID]);

  // Apply font-size changes to the live terminal and re-fit.
  useEffect(() => {
    const term = termRef.current;
    if (!term || term.options.fontSize === fontSize) return;
    term.options.fontSize = fontSize;
    try {
      fitRef.current?.fit();
    } catch {
      /* container not laid out yet */
    }
  }, [fontSize]);

  return {
    containerRef,
    status,
    exitCode,
    errorMessage,
    reconnect,
    fontSize,
    increaseFontSize,
    decreaseFontSize,
  };
}
