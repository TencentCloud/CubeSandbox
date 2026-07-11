// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useRef, useState } from 'react';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { createTerminalSocket, base64ToUtf8, type TerminalMessage } from '@/lib/terminalSocket';
import { useThemeStore, resolveEffective } from '@/store/theme';

export type TerminalStatus = 'connecting' | 'connected' | 'disconnected';

const MIN_FONT_SIZE = 8;
const MAX_FONT_SIZE = 32;

interface UseTerminalOptions {
  sandboxID: string;
  containerID?: string;
  enabled?: boolean;
  initialFontSize?: number;
}

export function getTerminalTheme(effective: 'light' | 'dark') {
  if (effective === 'light') {
    return {
      background: '#faf9f7',
      foreground: '#2b2622',
      cursor: '#2b2622',
      cursorAccent: '#faf9f7',
      selectionBackground: '#d4cfc7',
      black: '#2b2622',
      red: '#a83838',
      green: '#2e7a4f',
      yellow: '#8a6a1f',
      blue: '#2b5f8c',
      magenta: '#6b3d81',
      cyan: '#2b6e6e',
      white: '#e8e4de',
      brightBlack: '#5c534a',
      brightRed: '#c44747',
      brightGreen: '#3a9a66',
      brightYellow: '#b0862a',
      brightBlue: '#3a7ab8',
      brightMagenta: '#8b55a6',
      brightCyan: '#3a8e8e',
      brightWhite: '#ffffff',
    };
  }
  return {
    background: '#0b0f14',
    foreground: '#e2e8f0',
    cursor: '#e2e8f0',
    cursorAccent: '#0b0f14',
    selectionBackground: '#1e293b',
    black: '#0b0f14',
    red: '#f87171',
    green: '#34d399',
    yellow: '#fbbf24',
    blue: '#60a5fa',
    magenta: '#c084fc',
    cyan: '#22d3ee',
    white: '#cbd5e1',
    brightBlack: '#475569',
    brightRed: '#fca5a5',
    brightGreen: '#6ee7b7',
    brightYellow: '#fde68a',
    brightBlue: '#93c5fd',
    brightMagenta: '#d8b4fe',
    brightCyan: '#67e8f9',
    brightWhite: '#ffffff',
  };
}

export function useTerminal(containerRef: React.RefObject<HTMLDivElement | null>, options: UseTerminalOptions) {
  const { sandboxID, containerID, enabled = true, initialFontSize = 13 } = options;
  const [status, setStatus] = useState<TerminalStatus>('connecting');
  const [fontSize, setFontSizeState] = useState(initialFontSize);
  const [isFullscreen, setIsFullscreen] = useState(false);
  const [disconnectReason, setDisconnectReason] = useState<string | undefined>();

  const terminalRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const socketRef = useRef<ReturnType<typeof createTerminalSocket> | null>(null);
  const connectRef = useRef<() => void>(() => {});
  const resizeObserverRef = useRef<ResizeObserver | null>(null);
  const rafRef = useRef<number | null>(null);
  const mode = useThemeStore((s) => s.mode);
  const effective = resolveEffective(mode);

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    if (!enabled) {
      setStatus('disconnected');
      setDisconnectReason(undefined);
      return;
    }

    const term = new Terminal({
      fontFamily: '"JetBrains Mono Variable", "JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      fontSize,
      lineHeight: 1.25,
      cursorBlink: true,
      cursorStyle: 'block',
      scrollback: 5000,
      allowProposedApi: false,
      theme: getTerminalTheme(effective),
    });

    const fitAddon = new FitAddon();
    term.loadAddon(fitAddon);
    term.open(container);

    terminalRef.current = term;
    fitRef.current = fitAddon;

    const fitTimer = window.setTimeout(() => {
      try {
        fitAddon.fit();
      } catch {
        // Container may be hidden; ignore.
      }
    }, 0);

    const disposables = [
      // Forward to the current socket so a reconnect swaps underneath us.
      term.onData((data) => socketRef.current?.sendInput(data)),
      term.onResize(({ cols, rows }) => socketRef.current?.sendResize(cols, rows)),
    ];

    const ro = new ResizeObserver(() => {
      if (rafRef.current != null) {
        window.cancelAnimationFrame(rafRef.current);
      }
      rafRef.current = window.requestAnimationFrame(() => {
        rafRef.current = null;
        try {
          fitAddon.fit();
        } catch {
          // ignore
        }
      });
    });
    ro.observe(container);
    resizeObserverRef.current = ro;

    const connect = () => {
      setStatus('connecting');
      setDisconnectReason(undefined);
      const socket = createTerminalSocket(sandboxID, containerID);
      socketRef.current = socket;

      socket.onopen = () => {
        setStatus('connected');
        try {
          fitAddon.fit();
        } catch {
          // ignore
        }
      };

      socket.onmessage = (message: TerminalMessage) => {
        if (message.type === 'output') {
          term.write(base64ToUtf8(message.data));
        } else if (message.type === 'error') {
          term.writeln(`\r\n\x1b[31m${message.message}\x1b[0m`);
        } else if (message.type === 'close') {
          setStatus('disconnected');
          if (message.reason) {
            setDisconnectReason(message.reason);
          }
        }
      };

      socket.onclose = (event) => {
        setStatus('disconnected');
        setDisconnectReason(event?.reason);
      };

      socket.onerror = (event) => {
        setStatus('disconnected');
        setDisconnectReason(event.message);
        term.writeln(`\r\n\x1b[31m${event.message}\x1b[0m`);
      };
    };

    connectRef.current = connect;
    connect();

    return () => {
      window.clearTimeout(fitTimer);
      if (rafRef.current != null) {
        window.cancelAnimationFrame(rafRef.current);
        rafRef.current = null;
      }
      ro.disconnect();
      disposables.forEach((d) => d.dispose());
      const socket = socketRef.current;
      if (socket) {
        socket.onopen = null;
        socket.onmessage = null;
        socket.onclose = null;
        socket.onerror = null;
        socket.close();
      }
      term.dispose();
      terminalRef.current = null;
      fitRef.current = null;
      socketRef.current = null;
      resizeObserverRef.current = null;
      connectRef.current = () => {};
    };
  }, [sandboxID, containerID, enabled, containerRef]);

  useEffect(() => {
    if (terminalRef.current) {
      terminalRef.current.options.theme = getTerminalTheme(effective);
    }
  }, [effective]);

  useEffect(() => {
    const term = terminalRef.current;
    if (!term) return;
    term.options.fontSize = fontSize;
    try {
      fitRef.current?.fit();
    } catch {
      // ignore
    }
  }, [fontSize]);

  const reconnect = () => {
    const old = socketRef.current;
    if (old) {
      // Detach handlers so the old socket doesn't flip status after we close it.
      old.onopen = null;
      old.onmessage = null;
      old.onclose = null;
      old.onerror = null;
      old.close();
    }
    connectRef.current();
  };

  const fit = () => {
    try {
      fitRef.current?.fit();
    } catch {
      // ignore
    }
  };

  const setFontSize = (size: number) => {
    setFontSizeState(Math.max(MIN_FONT_SIZE, Math.min(MAX_FONT_SIZE, size)));
  };

  const increaseFontSize = () => setFontSize(fontSize + 1);
  const decreaseFontSize = () => setFontSize(fontSize - 1);

  const toggleFullscreen = () => {
    setIsFullscreen((prev) => !prev);
    // Give the layout a tick to settle before refitting.
    window.setTimeout(() => fit(), 0);
  };

  return {
    status,
    fit,
    reconnect,
    terminal: terminalRef.current,
    fontSize,
    setFontSize,
    increaseFontSize,
    decreaseFontSize,
    isFullscreen,
    toggleFullscreen,
    disconnectReason,
  };
}
