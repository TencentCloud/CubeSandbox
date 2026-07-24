// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useMemo, useRef, useState } from 'react';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import { useTranslation } from 'react-i18next';
import { X, RotateCw, Maximize2 } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { terminalApi } from '@/api/client';
import { cn } from '@/lib/utils';

type ConnectionState = 'idle' | 'connecting' | 'connected' | 'closed' | 'error';

interface TerminalMessage {
  type: 'status' | 'started' | 'output' | 'error' | 'closed';
  status?: string;
  message?: string;
  pid?: number;
  data?: string;
  exitCode?: number | null;
}

export function SandboxTerminalDialog({
  sandboxID,
  containerID,
  open,
  onOpenChange,
}: {
  sandboxID: string;
  containerID?: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation('sandboxDetail');
  const containerRef = useRef<HTMLDivElement>(null);
  const terminalRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const socketRef = useRef<WebSocket | null>(null);
  const cleanupRef = useRef<(() => void) | null>(null);
  const connectSeqRef = useRef(0);
  const [connectionState, setConnectionState] = useState<ConnectionState>('idle');
  const [fullscreen, setFullscreen] = useState(false);
  const [pid, setPid] = useState<number | null>(null);
  const title = useMemo(() => t('terminal.title', { id: sandboxID }), [sandboxID, t]);

  const connect = async () => {
    if (!open || !containerRef.current) return;
    const connectSeq = connectSeqRef.current + 1;
    connectSeqRef.current = connectSeq;
    cleanupRef.current?.();
    cleanupRef.current = null;

    const fit = new FitAddon();
    const term = new Terminal({
      cursorBlink: true,
      convertEol: true,
      fontFamily: 'JetBrains Mono Variable, ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 13,
      lineHeight: 1.15,
      theme: {
        background: '#080b12',
        foreground: '#d7dde8',
        cursor: '#ffffff',
        selectionBackground: '#334155',
      },
    });
    terminalRef.current = term;
    fitRef.current = fit;
    term.loadAddon(fit);
    term.open(containerRef.current);
    fit.fit();
    term.focus();
    term.writeln(t('terminal.connecting'));
    setConnectionState('connecting');
    setPid(null);

    let session;
    try {
      session = await terminalApi.createSession(sandboxID, { containerID });
    } catch (err) {
      if (connectSeq !== connectSeqRef.current) {
        term.dispose();
        return;
      }
      setConnectionState('error');
      term.writeln(`\r\n${t('terminal.error', { message: err instanceof Error ? err.message : String(err) })}`);
      terminalRef.current = null;
      fitRef.current = null;
      term.dispose();
      return;
    }
    if (connectSeq !== connectSeqRef.current || !open || !containerRef.current) {
      term.dispose();
      return;
    }
    const endpoint = resolveTerminalEndpoint(session.websocketPath);
    const ws = new WebSocket(endpoint, ['cube-terminal.v1', `grant.${session.grant}`]);
    socketRef.current = ws;

    const sendResize = () => {
      fit.fit();
      const { rows, cols } = term;
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', rows, cols }));
      }
    };

    ws.onopen = () => {
      setConnectionState('connected');
      sendResize();
    };
    ws.onmessage = (event) => {
      const message = parseMessage(event.data);
      if (!message) return;
      switch (message.type) {
        case 'status':
          if (message.message) term.writeln(message.message);
          break;
        case 'started':
          setPid(message.pid ?? null);
          term.writeln(t('terminal.connected', { pid: message.pid ?? '-' }));
          break;
        case 'output':
          if (message.data) {
            term.write(decodeBase64(message.data));
          }
          break;
        case 'error':
          setConnectionState('error');
          term.writeln(`\r\n${t('terminal.error', { message: message.message ?? '' })}`);
          break;
        case 'closed':
          setConnectionState('closed');
          term.writeln(`\r\n${t('terminal.closed', { code: message.exitCode ?? '-' })}`);
          break;
      }
    };
    ws.onerror = () => {
      setConnectionState('error');
      term.writeln(`\r\n${t('terminal.socketError')}`);
    };
    ws.onclose = () => {
      setConnectionState((current) => (current === 'error' ? 'error' : 'closed'));
    };

    const disposable = term.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'input', data }));
      }
    });

    const observer = new ResizeObserver(sendResize);
    observer.observe(containerRef.current);

    const cleanup = () => {
      observer.disconnect();
      disposable.dispose();
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'close' }));
      }
      ws.close();
      term.dispose();
      if (socketRef.current === ws) socketRef.current = null;
      if (terminalRef.current === term) terminalRef.current = null;
      if (fitRef.current === fit) fitRef.current = null;
    };
    cleanupRef.current = cleanup;
    return cleanup;
  };

  useEffect(() => {
    if (!open) return undefined;
    void connect();
    return () => {
      connectSeqRef.current += 1;
      cleanupRef.current?.();
      cleanupRef.current = null;
    };
    // Reconnect only when the dialog is opened for a new sandbox.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, sandboxID, containerID]);

  useEffect(() => {
    if (!open) {
      connectSeqRef.current += 1;
      cleanupRef.current?.();
      cleanupRef.current = null;
      setConnectionState('idle');
      setPid(null);
      setFullscreen(false);
    }
  }, [open]);

  if (!open) return null;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-background/80 p-4 backdrop-blur-sm">
      <div
        className={cn(
          'flex flex-col overflow-hidden rounded-lg border border-border bg-card shadow-2xl',
          fullscreen ? 'h-[calc(100vh-32px)] w-[calc(100vw-32px)]' : 'h-[72vh] w-[min(1040px,calc(100vw-32px))]',
        )}
      >
        <div className="flex min-h-12 items-center gap-3 border-b border-border px-4">
          <div className="min-w-0 flex-1">
            <div className="truncate text-sm font-medium">{title}</div>
            <div className="truncate font-mono text-xs text-muted-foreground">
              {pid ? t('terminal.pid', { pid }) : t('terminal.noPid')}
            </div>
          </div>
          <Badge tone={connectionState === 'connected' ? 'ok' : connectionState === 'error' ? 'err' : 'mute'}>
            {t(`terminal.state.${connectionState}` as const)}
          </Badge>
          <Button size="icon" variant="ghost" title={t('terminal.reconnect')} onClick={connect}>
            <RotateCw size={14} />
          </Button>
          <Button
            size="icon"
            variant="ghost"
            title={t('terminal.fullscreen')}
            onClick={() => setFullscreen((value) => !value)}
          >
            <Maximize2 size={14} />
          </Button>
          <Button size="icon" variant="ghost" title={t('terminal.close')} onClick={() => onOpenChange(false)}>
            <X size={15} />
          </Button>
        </div>
        <div className="min-h-0 flex-1 bg-[#080b12] p-3">
          <div ref={containerRef} className="h-full w-full overflow-hidden" />
        </div>
      </div>
    </div>
  );
}

function parseMessage(data: unknown): TerminalMessage | null {
  if (typeof data !== 'string') return null;
  try {
    return JSON.parse(data) as TerminalMessage;
  } catch {
    return null;
  }
}

function decodeBase64(value: string): string {
  const binary = window.atob(value);
  const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0));
  return new TextDecoder().decode(bytes);
}

function resolveTerminalEndpoint(path: string): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${protocol}//${window.location.host}${path}`;
}
