// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import * as Dialog from '@radix-ui/react-dialog';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import { Loader2, RefreshCw, X } from 'lucide-react';

import { terminalApi } from '@/api/client';
import { Button } from '@/components/ui/button';

import '@xterm/xterm/css/xterm.css';

type TerminalStatus = 'connecting' | 'connected' | 'exited' | 'error' | 'disconnected';

interface TerminalDialogProps {
  sandboxID: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

interface TerminalServerMessage {
  type: 'ready' | 'exit' | 'error';
  pid?: number;
  exitCode?: number;
  message?: string;
}

export function TerminalDialog({ sandboxID, open, onOpenChange }: TerminalDialogProps) {
  const { t } = useTranslation('sandboxes');
  const hostRef = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<TerminalStatus>('connecting');
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    if (!open || !hostRef.current) return;

    let disposed = false;
    let ready = false;
    let socket: WebSocket | null = null;
    const encoder = new TextEncoder();
    const terminal = new Terminal({
      cursorBlink: true,
      convertEol: false,
      fontFamily: 'JetBrains Mono Variable, ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 13,
      lineHeight: 1.2,
      scrollback: 5_000,
      allowProposedApi: false,
      theme: {
        background: '#090d14',
        foreground: '#e5e7eb',
        cursor: '#8ba7ff',
        selectionBackground: '#3759a866',
        black: '#111827',
        red: '#f87171',
        green: '#34d399',
        yellow: '#fbbf24',
        blue: '#60a5fa',
        magenta: '#c084fc',
        cyan: '#22d3ee',
        white: '#e5e7eb',
        brightBlack: '#6b7280',
        brightRed: '#fca5a5',
        brightGreen: '#6ee7b7',
        brightYellow: '#fde68a',
        brightBlue: '#93c5fd',
        brightMagenta: '#d8b4fe',
        brightCyan: '#67e8f9',
        brightWhite: '#f9fafb',
      },
    });
    const fit = new FitAddon();
    terminal.loadAddon(fit);
    terminal.open(hostRef.current);
    terminal.writeln(`\x1b[2m${t('terminal.connectingTo', { sandboxID })}\x1b[0m`);
    setStatus('connecting');

    const sendResize = () => {
      if (ready && socket?.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ type: 'resize', rows: terminal.rows, cols: terminal.cols }));
      }
    };
    const resize = () => {
      try {
        fit.fit();
        sendResize();
      } catch {
        // The dialog may be closing while ResizeObserver delivers a final event.
      }
    };
    const animationFrame = requestAnimationFrame(() => {
      resize();
      terminal.focus();
    });
    const observer = new ResizeObserver(resize);
    observer.observe(hostRef.current);

    const dataDisposable = terminal.onData((data) => {
      if (ready && socket?.readyState === WebSocket.OPEN) {
        const encoded = encoder.encode(data);
        // Keep every browser frame comfortably below CubeOps' 256 KiB cap,
        // including large clipboard pastes.
        for (let offset = 0; offset < encoded.length; offset += 32 * 1024) {
          socket.send(encoded.slice(offset, offset + 32 * 1024));
        }
      }
    });
    const connect = async () => {
      try {
        const ticket = await terminalApi.createTicket(sandboxID);
        if (disposed) return;
        const url = new URL(
          `/opsapi/v1/sdk/sandboxes/${encodeURIComponent(sandboxID)}/terminal/ws`,
          window.location.href,
        );
        url.protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        socket = new WebSocket(url, [
          ticket.protocol || 'cube-terminal',
          `cube-ticket.${ticket.ticket}`,
        ]);
        socket.binaryType = 'arraybuffer';

        socket.onmessage = (event) => {
          if (disposed) return;
          if (event.data instanceof ArrayBuffer) {
            terminal.write(new Uint8Array(event.data));
            return;
          }
          if (event.data instanceof Blob) {
            void event.data.arrayBuffer().then((data) => {
              if (!disposed) terminal.write(new Uint8Array(data));
            });
            return;
          }
          try {
            const message = JSON.parse(String(event.data)) as TerminalServerMessage;
            if (message.type === 'ready') {
              ready = true;
              setStatus('connected');
              terminal.writeln(
                `\r\n\x1b[2m${t('terminal.connected', { pid: message.pid })}\x1b[0m`,
              );
              resize();
              terminal.focus();
            } else if (message.type === 'exit') {
              ready = false;
              setStatus('exited');
              terminal.writeln(
                `\r\n\x1b[33m${t('terminal.exited', { code: message.exitCode ?? '?' })}\x1b[0m`,
              );
              if (message.message) terminal.writeln(`\x1b[31m${message.message}\x1b[0m`);
            } else if (message.type === 'error') {
              setStatus('error');
              terminal.writeln(
                `\r\n\x1b[31m${message.message || t('terminal.connectionFailed')}\x1b[0m`,
              );
            }
          } catch {
            terminal.writeln(`\r\n\x1b[31m${t('terminal.invalidResponse')}\x1b[0m`);
          }
        };
        socket.onerror = () => {
          if (!disposed) setStatus('error');
        };
        socket.onclose = () => {
          if (disposed) return;
          ready = false;
          setStatus((current) => (current === 'exited' ? current : 'disconnected'));
          terminal.writeln(`\r\n\x1b[2m${t('terminal.disconnected')}\x1b[0m`);
        };
      } catch (error) {
        if (disposed) return;
        setStatus('error');
        const message = error instanceof Error ? error.message : t('terminal.connectionFailed');
        terminal.writeln(`\r\n\x1b[31m${message}\x1b[0m`);
      }
    };

    void connect();

    return () => {
      disposed = true;
      ready = false;
      cancelAnimationFrame(animationFrame);
      observer.disconnect();
      dataDisposable.dispose();
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ type: 'close' }));
        socket.close(1000, 'terminal dialog closed');
      } else {
        socket?.close();
      }
      terminal.dispose();
    };
  }, [attempt, open, sandboxID, t]);

  const retryable = status === 'error' || status === 'disconnected' || status === 'exited';

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 flex h-[min(720px,calc(100vh-3rem))] w-[min(1100px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 flex-col overflow-hidden rounded-2xl border border-border/60 bg-card shadow-2xl">
          <div className="flex items-center gap-3 border-b border-border/60 px-4 py-3">
            <div className="min-w-0 flex-1">
              <Dialog.Title className="truncate text-sm font-semibold">
                {t('terminal.title', { sandboxID })}
              </Dialog.Title>
              <Dialog.Description className="mt-0.5 text-xs text-muted-foreground">
                {t(`terminal.status.${status}`)}
              </Dialog.Description>
            </div>
            {status === 'connecting' ? (
              <Loader2 size={15} className="animate-spin text-muted-foreground" />
            ) : null}
            {retryable ? (
              <Button size="sm" variant="outline" onClick={() => setAttempt((value) => value + 1)}>
                <RefreshCw size={14} /> {t('terminal.retry')}
              </Button>
            ) : null}
            <Dialog.Close asChild>
              <Button size="icon" variant="ghost" title={t('terminal.close')}>
                <X size={16} />
              </Button>
            </Dialog.Close>
          </div>
          <div className="min-h-0 flex-1 bg-[#090d14] p-3">
            <div ref={hostRef} className="h-full w-full overflow-hidden" />
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

export default TerminalDialog;
