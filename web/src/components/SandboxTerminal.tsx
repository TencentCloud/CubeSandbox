// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useCallback, useEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal as XTerm } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import { Copy, Maximize2, Minimize2, RefreshCw, TerminalSquare, X } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { sandboxApi } from '@/api/client';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';

type ConnectionState = 'connecting' | 'connected' | 'disconnected' | 'error';

interface SandboxTerminalProps {
  sandboxID: string;
  onClose: () => void;
}

function toWebSocketUrl(path: string): string {
  const url = new URL(path, window.location.origin);
  url.protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return url.toString();
}

export function SandboxTerminal({ sandboxID, onClose }: SandboxTerminalProps) {
  const { t } = useTranslation('sandboxDetail');
  const terminalHost = useRef<HTMLDivElement>(null);
  const terminal = useRef<XTerm | null>(null);
  const fitAddon = useRef<FitAddon | null>(null);
  const socket = useRef<WebSocket | null>(null);
  const [containerId, setContainerId] = useState('');
  const [shell, setShell] = useState<'/bin/sh' | '/bin/bash'>('/bin/sh');
  const [fontSize, setFontSize] = useState(14);
  const [fullScreen, setFullScreen] = useState(false);
  const [connectionState, setConnectionState] = useState<ConnectionState>('connecting');
  const [connectionMessage, setConnectionMessage] = useState('');
  const [attempt, setAttempt] = useState(0);
  const [terminalReady, setTerminalReady] = useState(false);

  const terminalInfo = useQuery({
    queryKey: ['sandbox-terminal', sandboxID],
    queryFn: () => sandboxApi.terminalInfo(sandboxID),
    retry: false,
  });

  useEffect(() => {
    const first = terminalInfo.data?.containers[0]?.id;
    if (first && !containerId) setContainerId(first);
  }, [containerId, terminalInfo.data]);

  const sendResize = useCallback(() => {
    const term = terminal.current;
    const ws = socket.current;
    fitAddon.current?.fit();
    if (term && ws?.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
    }
  }, []);

  useEffect(() => {
    if (!terminalHost.current) return;
    const term = new XTerm({
      cursorBlink: true,
      cursorStyle: 'bar',
      fontFamily: '"JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize,
      scrollback: 5_000,
      theme: { background: '#09090b', foreground: '#e4e4e7', cursor: '#a1a1aa' },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(terminalHost.current);
    terminal.current = term;
    fitAddon.current = fit;
    setTerminalReady(true);
    requestAnimationFrame(sendResize);

    const dataSubscription = term.onData((data) => {
      if (socket.current?.readyState === WebSocket.OPEN) socket.current.send(data);
    });
    term.attachCustomKeyEventHandler((event) => {
      if (!((event.ctrlKey || event.metaKey) && event.shiftKey)) return true;
      if (event.key.toLowerCase() === 'c') {
        const selected = term.getSelection();
        if (selected) void navigator.clipboard?.writeText(selected);
        return false;
      }
      if (event.key.toLowerCase() === 'v') {
        void navigator.clipboard?.readText().then((text) => {
          if (text && socket.current?.readyState === WebSocket.OPEN) socket.current.send(text);
        });
        return false;
      }
      return true;
    });
    const observer = new ResizeObserver(sendResize);
    observer.observe(terminalHost.current);

    return () => {
      observer.disconnect();
      dataSubscription.dispose();
      terminal.current = null;
      fitAddon.current = null;
      setTerminalReady(false);
      term.dispose();
    };
  }, [fontSize, sendResize]);

  useEffect(() => {
    if (!terminalReady || !containerId || !terminalInfo.data?.enabled || !terminal.current) return;
    let cancelled = false;
    const term = terminal.current;
    const connect = async () => {
      setConnectionState('connecting');
      setConnectionMessage('');
      term.writeln(`\x1b[90m${t('terminal.connecting')}\x1b[0m`);
      try {
        const session = await sandboxApi.createTerminalSession(sandboxID, { containerId, shell });
        if (cancelled) return;
        const ws = new WebSocket(toWebSocketUrl(session.websocketPath));
        ws.binaryType = 'arraybuffer';
        socket.current = ws;
        ws.onopen = () => {
          if (cancelled) return;
          setConnectionState('connected');
          term.focus();
          sendResize();
        };
        ws.onmessage = async (event) => {
          if (typeof event.data === 'string') {
            try {
              const control = JSON.parse(event.data) as { type?: string; message?: string };
              if (control.type === 'exit' || control.type === 'error') {
                term.writeln(`\r\n\x1b[33m${control.message ?? t('terminal.disconnected')}\x1b[0m`);
                setConnectionState(control.type === 'error' ? 'error' : 'disconnected');
                return;
              }
            } catch {
              // Normal shell input/output can be text too.
            }
            term.write(event.data);
            return;
          }
          const bytes = event.data instanceof Blob
            ? new Uint8Array(await event.data.arrayBuffer())
            : new Uint8Array(event.data as ArrayBuffer);
          term.write(bytes);
        };
        ws.onerror = () => {
          if (!cancelled) {
            setConnectionState('error');
            setConnectionMessage(t('terminal.connectionFailed'));
          }
        };
        ws.onclose = () => {
          if (!cancelled) setConnectionState((current) => current === 'error' ? current : 'disconnected');
        };
      } catch (error) {
        if (!cancelled) {
          setConnectionState('error');
          setConnectionMessage(error instanceof Error ? error.message : t('terminal.connectionFailed'));
          term.writeln(`\x1b[31m${t('terminal.connectionFailed')}\x1b[0m`);
        }
      }
    };
    void connect();
    return () => {
      cancelled = true;
      socket.current?.close();
      socket.current = null;
    };
  }, [attempt, containerId, sandboxID, sendResize, shell, t, terminalInfo.data?.enabled, terminalReady]);

  const copySelection = () => {
    const selected = terminal.current?.getSelection();
    if (selected) void navigator.clipboard?.writeText(selected);
  };

  const enabled = terminalInfo.data?.enabled === true;
  const dialogClass = fullScreen ? 'inset-2' : 'inset-4 md:inset-12';
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-3 backdrop-blur-sm" role="dialog" aria-modal="true" aria-label={t('terminal.title')}>
      <section className={cn('absolute flex flex-col overflow-hidden rounded-xl border border-border bg-card shadow-2xl', dialogClass)}>
        <header className="flex flex-wrap items-center gap-2 border-b border-border bg-muted/30 px-3 py-2">
          <TerminalSquare size={16} className="text-primary" />
          <span className="font-medium">{t('terminal.title')}</span>
          <span className={cn('text-xs', connectionState === 'connected' ? 'text-emerald-500' : 'text-muted-foreground')}>
            {connectionState === 'connected' ? t('terminal.connected') : t(`terminal.${connectionState}`)}
          </span>
          <div className="ml-auto flex items-center gap-1">
            <Button size="icon" variant="ghost" title={t('terminal.copy')} onClick={copySelection}><Copy size={15} /></Button>
            <Button size="icon" variant="ghost" title={t('terminal.fontSmaller')} onClick={() => setFontSize((value) => Math.max(10, value - 1))}>A−</Button>
            <Button size="icon" variant="ghost" title={t('terminal.fontLarger')} onClick={() => setFontSize((value) => Math.min(22, value + 1))}>A+</Button>
            <Button size="icon" variant="ghost" title={t('terminal.reconnect')} onClick={() => setAttempt((value) => value + 1)}><RefreshCw size={15} /></Button>
            <Button size="icon" variant="ghost" title={t('terminal.fullscreen')} onClick={() => setFullScreen((value) => !value)}>
              {fullScreen ? <Minimize2 size={15} /> : <Maximize2 size={15} />}
            </Button>
            <Button size="icon" variant="ghost" title={t('terminal.close')} onClick={onClose}><X size={16} /></Button>
          </div>
        </header>
        <div className="flex flex-wrap items-center gap-3 border-b border-border px-3 py-2 text-xs">
          <label className="flex items-center gap-2">{t('terminal.container')}
            <select className="rounded border border-border bg-background px-2 py-1" value={containerId} onChange={(event) => setContainerId(event.target.value)} disabled={!enabled}>
              {terminalInfo.data?.containers.map((container) => <option value={container.id} key={container.id}>{container.name}</option>)}
            </select>
          </label>
          <label className="flex items-center gap-2">{t('terminal.shell')}
            <select className="rounded border border-border bg-background px-2 py-1" value={shell} onChange={(event) => setShell(event.target.value as '/bin/sh' | '/bin/bash')} disabled={!enabled}>
              <option value="/bin/sh">/bin/sh</option><option value="/bin/bash">/bin/bash</option>
            </select>
          </label>
          <span className="text-muted-foreground">{t('terminal.shortcuts')}</span>
        </div>
        {!terminalInfo.isLoading && !enabled ? <p className="px-3 py-2 text-sm text-destructive">{terminalInfo.data?.reason ?? terminalInfo.error?.message ?? t('terminal.unavailable')}</p> : null}
        {connectionMessage ? <p className="px-3 py-2 text-xs text-destructive">{connectionMessage}</p> : null}
        <div ref={terminalHost} className="min-h-0 flex-1 bg-[#09090b] p-2" />
      </section>
    </div>
  );
}
