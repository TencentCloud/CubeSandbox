// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useRef, useState } from 'react';
import * as Dialog from '@radix-ui/react-dialog';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import { Maximize2, TerminalSquare, X } from 'lucide-react';

import { Button } from '@/components/ui/button';

type TerminalFrame =
  | { type: 'open'; sandboxId: string; cols: number; rows: number }
  | { type: 'input'; data: string }
  | { type: 'resize'; cols: number; rows: number };

function terminalSocketUrl(sandboxId: string): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${protocol}//${window.location.host}/cubeapi/v1/sandboxes/${encodeURIComponent(sandboxId)}/terminal/ws`;
}

/** A terminal panel that speaks the versioned JSON frame contract of the API gateway. */
export function SandboxTerminalDialog({
  open,
  sandboxId,
  onOpenChange,
}: {
  open: boolean;
  sandboxId: string;
  onOpenChange(open: boolean): void;
}) {
  const hostRef = useRef<HTMLDivElement>(null);
  const socketRef = useRef<WebSocket | null>(null);
  const [status, setStatus] = useState<'connecting' | 'connected' | 'disconnected'>('connecting');
  const [fullScreen, setFullScreen] = useState(false);

  useEffect(() => {
    if (!open || !hostRef.current) return;

    const terminal = new Terminal({
      cursorBlink: true,
      convertEol: true,
      fontFamily: 'JetBrains Mono Variable, ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 13,
      scrollback: 5_000,
      theme: {
        background: '#111318',
        foreground: '#e8edf6',
        cursor: '#8bb8ff',
        selectionBackground: '#315a91',
      },
    });
    const fit = new FitAddon();
    terminal.loadAddon(fit);
    terminal.open(hostRef.current);
    fit.fit();
    terminal.writeln('\x1b[90mConnecting to sandbox terminal…\x1b[0m');

    const socket = new WebSocket(terminalSocketUrl(sandboxId));
    socketRef.current = socket;
    const send = (frame: TerminalFrame) => {
      if (socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(frame));
    };

    socket.onopen = () => {
      setStatus('connected');
      send({ type: 'open', sandboxId, cols: terminal.cols, rows: terminal.rows });
    };
    socket.onmessage = (event) => {
      if (typeof event.data === 'string') terminal.write(event.data);
      else if (event.data instanceof Blob) void event.data.text().then((data) => terminal.write(data));
    };
    socket.onerror = () => terminal.writeln('\r\n\x1b[31mTerminal connection failed.\x1b[0m');
    socket.onclose = () => {
      setStatus('disconnected');
      terminal.writeln('\r\n\x1b[90mTerminal session closed.\x1b[0m');
    };

    const disposable = terminal.onData((data) => send({ type: 'input', data }));
    const resize = () => {
      fit.fit();
      send({ type: 'resize', cols: terminal.cols, rows: terminal.rows });
    };
    const observer = new ResizeObserver(resize);
    observer.observe(hostRef.current);

    return () => {
      observer.disconnect();
      disposable.dispose();
      socket.close(1000, 'terminal panel closed');
      socketRef.current = null;
      terminal.dispose();
      setStatus('connecting');
    };
  }, [open, sandboxId]);

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content className={`fixed z-50 flex flex-col overflow-hidden border border-border/60 bg-card shadow-2xl ${fullScreen ? 'inset-3 rounded-xl' : 'left-1/2 top-1/2 h-[min(720px,calc(100vh-3rem))] w-[min(980px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-2xl'}`}>
          <div className="flex items-center gap-3 border-b border-border/60 px-4 py-3">
            <div className="grid h-8 w-8 place-items-center rounded-lg bg-primary/10 text-primary"><TerminalSquare size={16} /></div>
            <div className="min-w-0 flex-1">
              <Dialog.Title className="text-sm font-semibold">Sandbox terminal</Dialog.Title>
              <Dialog.Description className="truncate font-mono text-xs text-muted-foreground">{sandboxId}</Dialog.Description>
            </div>
            <span className={`h-2 w-2 rounded-full ${status === 'connected' ? 'bg-cube-ok' : status === 'connecting' ? 'bg-cube-warn animate-pulse' : 'bg-muted-foreground'}`} aria-label={status} />
            <Button size="icon" variant="ghost" title="Toggle fullscreen" onClick={() => setFullScreen((value) => !value)}><Maximize2 size={15} /></Button>
            <Dialog.Close asChild><Button size="icon" variant="ghost" title="Close terminal"><X size={16} /></Button></Dialog.Close>
          </div>
          <div className="min-h-0 flex-1 bg-[#111318] p-3"><div ref={hostRef} className="h-full w-full" /></div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
