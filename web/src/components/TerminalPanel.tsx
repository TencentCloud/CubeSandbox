// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useRef, useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import { WebLinksAddon } from '@xterm/addon-web-links';
import { Button } from '@/components/ui/button';
import {
  X,
  Maximize2,
  Minimize2,
  Plus,
  Minus,
  RefreshCw,
  TerminalIcon,
  Loader2,
  Wifi,
  WifiOff,
} from 'lucide-react';
import { useTerminalSocket, type ConnectionState } from '@/hooks/useTerminalSocket';
import { useTerminalResize } from '@/hooks/useTerminalResize';
import { cn } from '@/lib/utils';

// ── xterm.js CSS ──────────────────────────────────────────────────────────
import '@xterm/xterm/css/xterm.css';

interface TerminalPanelProps {
  sandboxID: string;
  /** Container name within the sandbox. */
  container?: string;
  /** List of available container names in this sandbox (for the selector). */
  containers?: string[];
  /** Called when the user closes the panel. */
  onClose: () => void;
  /** Whether the panel is open. */
  open: boolean;
}

const STATUS_LABELS: Record<ConnectionState, string> = {
  disconnected: 'disconnected',
  connecting: 'connecting',
  connected: 'connected',
  reconnecting: 'reconnecting',
};

const STATUS_ICONS: Record<ConnectionState, React.ReactNode> = {
  disconnected: <WifiOff className="h-3 w-3" />,
  connecting: <Loader2 className="h-3 w-3 animate-spin" />,
  connected: <Wifi className="h-3 w-3 text-green-400" />,
  reconnecting: <Loader2 className="h-3 w-3 animate-spin text-amber-400" />,
};

export default function TerminalPanel({
  sandboxID,
  container: initialContainer = 'default',
  containers = ['default'],
  onClose,
  open,
}: TerminalPanelProps) {
  const { t } = useTranslation('terminal');
  const terminalRef = useRef<HTMLDivElement>(null);
  const term = useRef<Terminal | null>(null);
  const fitAddonRef = useRef<FitAddon | null>(null);
  const [fontSize, setFontSize] = useState(14);
  const [isFullscreen, setIsFullscreen] = useState(false);
  const [selectedContainer, setSelectedContainer] = useState(initialContainer);
  const [closeReason, setCloseReason] = useState<string | null>(null);

  // ── Initialize xterm.js ─────────────────────────────────────────────────
  useEffect(() => {
    if (!open) return;
    if (term.current) return; // already initialized

    const fitAddon = new FitAddon();
    fitAddonRef.current = fitAddon;

    const instance = new Terminal({
      fontSize,
      fontFamily: '"JetBrains Mono", "Cascadia Code", "Fira Code", monospace',
      cursorBlink: true,
      cursorStyle: 'bar',
      scrollback: 5000,
      theme: {
        background: '#1a1b26',
        foreground: '#c0caf5',
        cursor: '#c0caf5',
        selectionBackground: '#364A82',
        black: '#15161e',
        red: '#f7768e',
        green: '#9ece6a',
        yellow: '#e0af68',
        blue: '#7aa2f7',
        magenta: '#bb9af7',
        cyan: '#7dcfff',
        white: '#a9b1d6',
        brightBlack: '#414868',
        brightRed: '#f7768e',
        brightGreen: '#9ece6a',
        brightYellow: '#e0af68',
        brightBlue: '#7aa2f7',
        brightMagenta: '#bb9af7',
        brightCyan: '#7dcfff',
        brightWhite: '#c0caf5',
      },
      allowProposedApi: true,
      allowTransparency: false,
    });

    instance.loadAddon(fitAddon);
    instance.loadAddon(new WebLinksAddon());
    term.current = instance;

    // Schedule mount into DOM on next microtask so terminalRef is attached.
    requestAnimationFrame(() => {
      if (terminalRef.current && term.current === instance) {
        instance.open(terminalRef.current);
        fitAddon.fit();
      }
    });

    return () => {
      instance.dispose();
      term.current = null;
      fitAddonRef.current = null;
    };
  }, [open]); // eslint-disable-line react-hooks/exhaustive-deps
  // ^ fontSize intentionally excluded: changing font size via `terminal.options.fontSize`
  //   is zero-cost and preserves the terminal state (scrollback, etc.).

  // ── Apply font size changes without recreating terminal ────────────────
  useEffect(() => {
    if (!term.current) return;
    term.current.options.fontSize = fontSize;
    fitAddonRef.current?.fit();
  }, [fontSize]);

  // ── WebSocket ───────────────────────────────────────────────────────────
  const onMessage = useCallback((data: ArrayBuffer) => {
    if (!term.current) return;
    const arr = new Uint8Array(data);
    // Check for stderr prefix (byte 0x02)
    if (arr.length > 1 && arr[0] === 0x02) {
      // stderr — write with distinct rendering (no ANSI, could color differently)
      term.current.write(arr.slice(1));
    } else {
      term.current.write(arr);
    }
  }, []);

  const onWsClose = useCallback((reason: string) => {
    setCloseReason(reason);
  }, []);

  const { connectionState, send, connect, disconnect } = useTerminalSocket({
    sandboxID,
    container: selectedContainer,
    onMessage,
    onClose: onWsClose,
  });

  // ── Resize ──────────────────────────────────────────────────────────────
  const onResize = useCallback(
    (cols: number, rows: number) => {
      send(JSON.stringify({ type: 'resize', cols, rows }));
    },
    [send],
  );

  useTerminalResize({
    terminal: term.current,
    containerRef: terminalRef,
    fitAddon: fitAddonRef.current,
    onResize,
    enabled: open && connectionState === 'connected',
  });

  // ── Connect on mount ────────────────────────────────────────────────────
  useEffect(() => {
    if (!open || !term.current) return;
    connect();
    return () => disconnect();
  }, [open, connect, disconnect, selectedContainer]);

  // ── Terminal input → WebSocket ──────────────────────────────────────────
  useEffect(() => {
    if (!term.current) return;
    const instance = term.current;

    const disposable = instance.onData((data: string) => {
      send(data);
    });

    return () => {
      disposable.dispose();
    };
  }, [send]);

  // ── Font size ───────────────────────────────────────────────────────────
  const changeFontSize = useCallback((delta: number) => {
    setFontSize((prev) => Math.max(10, Math.min(24, prev + delta)));
  }, []);

  // ── Reconnect ───────────────────────────────────────────────────────────
  const handleReconnect = useCallback(() => {
    setCloseReason(null);
    connect();
  }, [connect]);

  // ── Container switch ────────────────────────────────────────────────────
  const handleContainerChange = useCallback((name: string) => {
    disconnect();
    if (term.current) {
      term.current.clear();
    }
    setCloseReason(null);
    setSelectedContainer(name);
    // connect() will be called by the useEffect when selectedContainer changes
  }, [disconnect]);

  // ── Render ──────────────────────────────────────────────────────────────
  if (!open) return null;

  return (
    <div
      className={cn(
        'flex flex-col border border-border bg-card shadow-2xl rounded-lg overflow-hidden',
        isFullscreen
          ? 'fixed inset-4 z-50'
          : 'h-[500px] min-w-[600px]',
      )}
    >
      {/* ── Toolbar ────────────────────────────────────────────────────── */}
      <div className="flex items-center justify-between px-3 py-1.5 bg-muted/50 border-b border-border shrink-0">
        <div className="flex items-center gap-2">
          <TerminalIcon className="h-4 w-4 text-muted-foreground" />
          <span className="text-xs font-medium text-foreground/70">
            {sandboxID}
          </span>

          {/* Container selector */}
          {containers.length > 1 && (
            <select
              className="text-xs bg-muted border border-border rounded px-1.5 py-0.5 text-foreground/80"
              value={selectedContainer}
              onChange={(e) => handleContainerChange(e.target.value)}
            >
              {containers.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          )}

          {/* Connection status */}
          <span
            className={cn(
              'flex items-center gap-1 text-xs',
              connectionState === 'connected' && 'text-green-400',
              connectionState === 'reconnecting' && 'text-amber-400',
              connectionState === 'connecting' && 'text-blue-400',
              connectionState === 'disconnected' && 'text-muted-foreground',
            )}
          >
            {STATUS_ICONS[connectionState]}
            {STATUS_LABELS[connectionState]}
          </span>

          {/* Close reason */}
          {closeReason && (
            <span className="text-xs text-cube-err/80 ml-2">({closeReason})</span>
          )}
        </div>

        <div className="flex items-center gap-1">
          {/* Font size controls */}
          <Button
            variant="ghost"
            size="sm"
            className="h-6 w-6 p-0"
            onClick={() => changeFontSize(-1)}
            title={t('toolbar.fontSizeDown', 'Decrease font size') as string}
          >
            <Minus className="h-3 w-3" />
          </Button>
          <span className="text-xs text-muted-foreground min-w-[24px] text-center">
            {fontSize}
          </span>
          <Button
            variant="ghost"
            size="sm"
            className="h-6 w-6 p-0"
            onClick={() => changeFontSize(1)}
            title={t('toolbar.fontSizeUp', 'Increase font size') as string}
          >
            <Plus className="h-3 w-3" />
          </Button>

          {/* Reconnect */}
          {connectionState === 'disconnected' && (
            <Button
              variant="ghost"
              size="sm"
              className="h-6 px-1.5 text-xs"
              onClick={handleReconnect}
              title={t('toolbar.reconnect', 'Reconnect') as string}
            >
              <RefreshCw className="h-3 w-3 mr-1" />
              {t('toolbar.reconnect', 'Reconnect') as string}
            </Button>
          )}

          {/* Fullscreen toggle */}
          <Button
            variant="ghost"
            size="sm"
            className="h-6 w-6 p-0"
            onClick={() => setIsFullscreen((prev) => !prev)}
            title={
              isFullscreen
                ? (t('toolbar.exitFullscreen', 'Exit fullscreen') as string)
                : (t('toolbar.fullscreen', 'Fullscreen') as string)
            }
          >
            {isFullscreen ? (
              <Minimize2 className="h-3 w-3" />
            ) : (
              <Maximize2 className="h-3 w-3" />
            )}
          </Button>

          {/* Close */}
          <Button
            variant="ghost"
            size="sm"
            className="h-6 w-6 p-0 hover:text-cube-err"
            onClick={onClose}
            title={t('toolbar.close', 'Close terminal') as string}
          >
            <X className="h-4 w-4" />
          </Button>
        </div>
      </div>

      {/* ── Terminal container ───────────────────────────────────────────── */}
      <div
        ref={terminalRef}
        className="flex-1 min-h-0"
      />
    </div>
  );
}