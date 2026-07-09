// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { Maximize2, Minus, Plus, X } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { sandboxTerminalWebSocketUrl, type SandboxContainer } from '@/api/client';
import { cn } from '@/lib/utils';

type TerminalState = 'idle' | 'connecting' | 'connected' | 'disconnected' | 'error';

interface ServerMessage {
  type: 'status' | 'output' | 'error' | 'exit';
  data?: string;
  message?: string;
  status?: string;
  code?: number;
}

export function TerminalPanel({
  sandboxID,
  open,
  onOpenChange,
  containers,
}: {
  sandboxID: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  containers?: SandboxContainer[] | null;
}) {
  const { t } = useTranslation('terminal');
  const hostRef = useRef<HTMLDivElement>(null);
  const terminalRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const lastResizeRef = useRef<{ rows: number; cols: number } | null>(null);
  const serverExitRef = useRef(false);
  const [state, setState] = useState<TerminalState>('idle');
  const [fontSize, setFontSize] = useState(13);
  const [fullscreen, setFullscreen] = useState(false);
  const [selectedContainer, setSelectedContainer] = useState('');
  const decoder = useMemo(() => new TextDecoder(), []);
  const selectableContainers = useMemo(
    () => (containers ?? []).filter((container) => container.containerID),
    [containers],
  );

  useEffect(() => {
    if (!open) {
      setSelectedContainer('');
      return;
    }
    if (
      selectedContainer &&
      !selectableContainers.some((container) => container.containerID === selectedContainer)
    ) {
      setSelectedContainer('');
    }
  }, [open, selectableContainers, selectedContainer]);

  useEffect(() => {
    if (!open || !hostRef.current) return;

    const terminal = new Terminal({
      cursorBlink: true,
      convertEol: true,
      scrollback: 5000,
      fontFamily: '"JetBrains Mono Variable", "JetBrains Mono", monospace',
      fontSize,
      theme: {
        background: '#0b1020',
        foreground: '#d6deff',
        cursor: '#ffffff',
      },
    });
    const fit = new FitAddon();
    terminal.loadAddon(fit);
    terminal.open(hostRef.current);
    terminal.focus();
    fit.fit();
    lastResizeRef.current = null;
    serverExitRef.current = false;

    terminalRef.current = terminal;
    fitRef.current = fit;
    setState('connecting');
    terminal.writeln(t('status.starting'));

    const url = sandboxTerminalWebSocketUrl(sandboxID, {
      rows: terminal.rows,
      cols: terminal.cols,
      container: selectedContainer || undefined,
    });
    const ws = new WebSocket(url);
    wsRef.current = ws;

    const sendResize = () => {
      if (ws.readyState === WebSocket.OPEN) {
        const next = { rows: terminal.rows, cols: terminal.cols };
        const last = lastResizeRef.current;
        if (last?.rows === next.rows && last.cols === next.cols) return;
        lastResizeRef.current = next;
        ws.send(JSON.stringify({ type: 'resize', rows: next.rows, cols: next.cols }));
      }
    };

    const dataDisposable = terminal.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'input', data }));
      }
    });
    const resizeDisposable = terminal.onResize(({ rows, cols }) => {
      if (ws.readyState === WebSocket.OPEN) {
        const last = lastResizeRef.current;
        if (last?.rows === rows && last.cols === cols) return;
        lastResizeRef.current = { rows, cols };
        ws.send(JSON.stringify({ type: 'resize', rows, cols }));
      }
    });

    ws.onopen = () => {
      setState('connected');
      sendResize();
    };
    ws.onmessage = (event) => {
      const msg = safeParse(event.data);
      if (!msg) return;
      if (msg.type === 'status') {
        terminal.writeln(`\r\n${msg.status === 'ready' ? t('status.ready') : msg.status ?? ''}`);
        setState('connected');
      } else if (msg.type === 'output' && msg.data) {
        terminal.write(decoder.decode(base64ToBytes(msg.data)));
      } else if (msg.type === 'error') {
        setState('error');
        terminal.writeln(`\r\n${t('error')}: ${msg.message ?? ''}`);
      } else if (msg.type === 'exit') {
        serverExitRef.current = true;
        setState('disconnected');
        terminal.writeln(`\r\n${formatExitMessage(t('closed'), msg)}`);
      }
    };
    ws.onerror = () => {
      setState('error');
      terminal.writeln(`\r\n${t('error')}`);
    };
    ws.onclose = () => {
      setState((current) => (current === 'error' ? 'error' : 'disconnected'));
      if (!serverExitRef.current) {
        terminal.writeln(`\r\n${t('status.reconnecting')}`);
      }
    };

    const onWindowResize = () => {
      fit.fit();
      sendResize();
    };
    window.addEventListener('resize', onWindowResize);

    return () => {
      window.removeEventListener('resize', onWindowResize);
      dataDisposable.dispose();
      resizeDisposable.dispose();
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'close' }));
      }
      ws.close();
      terminal.dispose();
      terminalRef.current = null;
      fitRef.current = null;
      wsRef.current = null;
      lastResizeRef.current = null;
      serverExitRef.current = false;
      setState('idle');
    };
  }, [decoder, fontSize, open, sandboxID, selectedContainer, t]);

  useEffect(() => {
    terminalRef.current?.options && (terminalRef.current.options.fontSize = fontSize);
    requestAnimationFrame(() => fitRef.current?.fit());
  }, [fontSize, fullscreen]);

  if (!open) return null;

  const disconnect = () => onOpenChange(false);

  return (
    <div className="fixed inset-0 z-50 bg-background/80 backdrop-blur-sm">
      <div
        className={cn(
          'absolute flex flex-col overflow-hidden border border-border bg-background shadow-2xl',
          fullscreen
            ? 'inset-3 rounded-lg'
            : 'bottom-6 right-6 h-[620px] w-[min(980px,calc(100vw-48px))] rounded-lg',
        )}
      >
        <div className="flex items-center gap-2 border-b border-border/70 px-3 py-2">
          <div className="min-w-0 flex-1">
            <div className="truncate text-sm font-medium">{t('title')}</div>
            <div className="truncate font-mono text-xs text-muted-foreground">
              {t('subtitle', { sandboxID })}
            </div>
          </div>
          <span
            className={cn(
              'rounded-full px-2 py-1 text-xs',
              state === 'connected'
                ? 'bg-cube-ok/15 text-cube-ok'
                : state === 'error'
                  ? 'bg-cube-err/15 text-cube-err'
                  : 'bg-muted text-muted-foreground',
            )}
          >
            {t(state === 'idle' ? 'disconnected' : state)}
          </span>
          {selectableContainers.length > 1 ? (
            <label className="flex min-w-[180px] items-center gap-2 text-xs text-muted-foreground">
              <span>{t('container')}</span>
              <select
                className="min-w-0 flex-1 rounded-md border border-border bg-background px-2 py-1 text-xs text-foreground"
                value={selectedContainer}
                title={t('container')}
                onChange={(event) => setSelectedContainer(event.target.value)}
              >
                <option value="">{t('defaultContainer')}</option>
                {selectableContainers.map((container) => (
                  <option key={container.containerID} value={container.containerID}>
                    {containerLabel(container)}
                  </option>
                ))}
              </select>
            </label>
          ) : null}
          <Button size="icon" variant="ghost" title={t('fontSize')} onClick={() => setFontSize((v) => Math.max(10, v - 1))}>
            <Minus size={14} />
          </Button>
          <Button size="icon" variant="ghost" title={t('fontSize')} onClick={() => setFontSize((v) => Math.min(22, v + 1))}>
            <Plus size={14} />
          </Button>
          <Button size="icon" variant="ghost" title={t('fullscreen')} onClick={() => setFullscreen((v) => !v)}>
            <Maximize2 size={14} />
          </Button>
          <Button size="icon" variant="ghost" title={t('disconnect')} onClick={disconnect}>
            <X size={14} />
          </Button>
        </div>
        <div ref={hostRef} className="min-h-0 flex-1 bg-[#0b1020] p-2" />
      </div>
    </div>
  );
}

function safeParse(raw: unknown): ServerMessage | null {
  if (typeof raw !== 'string') return null;
  try {
    return JSON.parse(raw) as ServerMessage;
  } catch {
    return null;
  }
}

function base64ToBytes(value: string): Uint8Array {
  const binary = window.atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

function formatExitMessage(label: string, msg: ServerMessage): string {
  const code = msg.code != null ? ` (${msg.code})` : '';
  const reason = msg.message ? `: ${msg.message}` : '';
  return `${label}${code}${reason}`;
}

function containerLabel(container: SandboxContainer): string {
  return container.name?.trim() || container.containerID;
}
