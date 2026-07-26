// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useRef, useState } from 'react';
import * as Dialog from '@radix-ui/react-dialog';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import { Maximize2, RefreshCw, TerminalSquare, X } from 'lucide-react';

import { Button } from '@/components/ui/button';
import { getSessionToken } from '@/lib/session';

/** JSON terminal frames; keepalive uses a compact K text frame decoded before JSON by CubeMaster. */
type TerminalFrame =
  | { type: 'open'; sandboxId: string; containerId: string; cols: number; rows: number }
  | { type: 'input'; data: string }
  | { type: 'resize'; cols: number; rows: number };

const TERMINAL_KEEPALIVE_FRAME = 'K';

function terminalSocketUrl(sandboxId: string): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${protocol}//${window.location.host}/cubeapi/v1/sandboxes/${encodeURIComponent(sandboxId)}/terminal/ws`;
}

/** A terminal panel that speaks the versioned JSON frame contract of the API gateway. */
export function SandboxTerminalDialog({
  open,
  sandboxId,
  targets,
  onOpenChange,
}: {
  open: boolean;
  sandboxId: string;
  targets: Array<{ containerID: string; name: string }>;
  onOpenChange(open: boolean): void;
}) {
  const hostRef = useRef<HTMLDivElement>(null);
  const socketRef = useRef<WebSocket | null>(null);
  const [status, setStatus] = useState<'connecting' | 'connected' | 'disconnected'>('connecting');
  const [fullScreen, setFullScreen] = useState(false);
  const [containerId, setContainerId] = useState('');
  const [connectionNonce, setConnectionNonce] = useState(0);

  useEffect(() => {
    if (!targets.some((target) => target.containerID === containerId)) {
      setContainerId(targets[0]?.containerID ?? '');
    }
  }, [containerId, targets]);

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

    const target = targets.find((item) => item.containerID === containerId) ?? targets[0];
    if (!target) {
      terminal.writeln('\x1b[31mNo running container is available for terminal login.\x1b[0m');
      return () => terminal.dispose();
    }
    const sessionToken = getSessionToken();
    const socket = sessionToken
      ? new WebSocket(terminalSocketUrl(sandboxId), ['cube-terminal', sessionToken])
      : new WebSocket(terminalSocketUrl(sandboxId), ['cube-terminal']);
    socketRef.current = socket;
    const pendingFrames: TerminalFrame[] = [];
    const maxPendingFrames = 16;
    let dropNotified = false;
    const send = (frame: TerminalFrame) => {
      if (socket.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify(frame));
      } else if (socket.readyState === WebSocket.CONNECTING && pendingFrames.length < maxPendingFrames) {
        pendingFrames.push(frame);
      } else if (!dropNotified) {
        // Queue full (or socket unavailable): any frame type may be dropped, notify once.
        dropNotified = true;
        terminal.writeln('\r\n\x1b[33mSome terminal input was ignored while the terminal was unavailable.\x1b[0m');
      }
    };

    socket.onopen = () => {
      setStatus('connected');
      const openFrame: TerminalFrame = {
        type: 'open',
        sandboxId,
        containerId: target.containerID,
        cols: terminal.cols,
        rows: terminal.rows,
      };
      socket.send(JSON.stringify(openFrame));
      pendingFrames.splice(0).forEach(send);
    };
    socket.onmessage = (event) => {
      const receive = (data: string) => {
        try {
          const frame = JSON.parse(data) as { type?: string; data?: string; message?: string };
          if (frame.type === 'output') terminal.write(frame.data ?? '');
          else if (frame.type === 'error') terminal.writeln(`\r\n\x1b[31m${frame.message ?? 'Terminal error'}\x1b[0m`);
          else if (frame.type === 'exit') terminal.writeln('\r\n\x1b[90mTerminal process exited.\x1b[0m');
        } catch { terminal.write(data); }
      };
      if (typeof event.data === 'string') receive(event.data);
      else if (event.data instanceof Blob) void event.data.text().then(receive);
    };
    socket.onerror = () => terminal.writeln('\r\n\x1b[31mTerminal connection failed.\x1b[0m');
    let keepalive: number | undefined;
    socket.onclose = () => {
      if (keepalive !== undefined) window.clearInterval(keepalive);
      setStatus('disconnected');
      terminal.writeln('\r\n\x1b[90mTerminal session closed.\x1b[0m');
    };

    const disposable = terminal.onData((data) => send({ type: 'input', data }));
    let resizeTimer: number | undefined;
    const resize = () => {
      window.clearTimeout(resizeTimer);
      resizeTimer = window.setTimeout(() => {
        fit.fit();
        send({ type: 'resize', cols: terminal.cols, rows: terminal.rows });
      }, 100);
    };
    const observer = new ResizeObserver(resize);
    observer.observe(hostRef.current);

    keepalive = window.setInterval(() => {
      if (socket.readyState === WebSocket.OPEN) socket.send(TERMINAL_KEEPALIVE_FRAME);
    }, 20_000);

    return () => {
      if (keepalive !== undefined) window.clearInterval(keepalive);
      window.clearTimeout(resizeTimer);
      observer.disconnect();
      disposable.dispose();
      socket.close(1000, 'terminal panel closed');
      socketRef.current = null;
      terminal.dispose();
      setStatus('connecting');
    };
  }, [open, sandboxId, targets, containerId, connectionNonce]);

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
            {targets.length > 1 && (
              <select
                aria-label="Terminal container"
                className="h-8 max-w-40 rounded-md border border-border bg-background px-2 text-xs"
                value={containerId}
                onChange={(event) => setContainerId(event.target.value)}
              >
                {targets.map((target) => <option key={target.containerID} value={target.containerID}>{target.name || target.containerID}</option>)}
              </select>
            )}
            <span className={`h-2 w-2 rounded-full ${status === 'connected' ? 'bg-cube-ok' : status === 'connecting' ? 'bg-cube-warn animate-pulse' : 'bg-muted-foreground'}`} aria-label={status} />
            {status === 'disconnected' && <Button size="sm" variant="ghost" onClick={() => setConnectionNonce((value) => value + 1)}><RefreshCw size={14} /> Reconnect</Button>}
            <Button size="icon" variant="ghost" title="Toggle fullscreen" onClick={() => setFullScreen((value) => !value)}><Maximize2 size={15} /></Button>
            <Dialog.Close asChild><Button size="icon" variant="ghost" title="Close terminal"><X size={16} /></Button></Dialog.Close>
          </div>
          <div className="min-h-0 flex-1 bg-[#111318] p-3"><div ref={hostRef} className="h-full w-full" /></div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
