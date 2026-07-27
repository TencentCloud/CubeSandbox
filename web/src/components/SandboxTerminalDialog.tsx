// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Interactive Web Terminal panel.
//
// Auth flow (see `CubeAPI/src/handlers/terminal.rs`): the browser first calls
// `POST /cubeapi/v1/sandboxes/:id/terminal/ticket` through the normal `api`
// wrapper (which sends the X-API-Key / X-Session-Token headers), receives a
// short-lived single-use ticket, then opens the WebSocket with `?ticket=...`.
// Credentials never travel in the WebSocket URL, which proxies/LBs would log.
//
// Once connected the socket is wired to an xterm.js instance: keystrokes and
// paste are base64-framed to the PTY, PTY output is decoded back, and window
// resizes are synchronized both ways.

import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import * as Dialog from '@radix-ui/react-dialog';
import { X, TerminalSquare, RotateCw, Maximize2, Minimize2 } from 'lucide-react';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { api } from '@/lib/api';
import { cn } from '@/lib/utils';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  sandboxID: string;
}

type Status = 'connecting' | 'ready' | 'closed' | 'error';

interface TicketResponse {
  ticket: string;
  expiresInSecs: number;
}

// Base64 helpers scoped to UTF-8, so multibyte input/paste round-trips safely.
function encodeBase64(data: string): string {
  const bytes = new TextEncoder().encode(data);
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

function decodeBase64(data: string): Uint8Array {
  const binary = atob(data);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

function terminalWsUrl(sandboxID: string, ticket: string): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  const base = `${proto}//${window.location.host}/cubeapi/v1/sandboxes/${encodeURIComponent(
    sandboxID,
  )}/terminal`;
  return `${base}?ticket=${encodeURIComponent(ticket)}`;
}

export function SandboxTerminalDialog({ open, onOpenChange, sandboxID }: Props) {
  const { t } = useTranslation('sandboxDetail');
  const mountRef = useRef<HTMLDivElement>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const decoderRef = useRef(new TextDecoder());
  const [status, setStatus] = useState<Status>('connecting');
  const [statusDetail, setStatusDetail] = useState<string>('');
  const [fullscreen, setFullscreen] = useState(false);
  // Bumped to force the connect effect to re-run on manual reconnect.
  const [attempt, setAttempt] = useState(0);

  const sendResize = useCallback(() => {
    const term = termRef.current;
    const ws = wsRef.current;
    if (!term || !ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
  }, []);

  // ── Terminal lifecycle: create on open, tear down on close ──────────────
  useEffect(() => {
    if (!open || !mountRef.current) return;

    const term = new Terminal({
      cursorBlink: true,
      convertEol: false,
      fontFamily:
        '"JetBrains Mono Variable", ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      fontSize: 13,
      theme: {
        background: '#0b0f14',
        foreground: '#d5dae1',
        cursor: '#7dd3fc',
      },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(mountRef.current);
    fit.fit();
    term.focus();
    termRef.current = term;
    fitRef.current = fit;

    // `disposed` guards every async callback so a late ticket response or a
    // message arriving after cleanup never touches a disposed terminal.
    let disposed = false;
    let ws: WebSocket | null = null;

    const detachSocket = (socket: WebSocket) => {
      socket.onopen = null;
      socket.onmessage = null;
      socket.onerror = null;
      socket.onclose = null;
    };

    setStatus('connecting');
    setStatusDetail('');

    const inputSub = term.onData((data) => {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'input', data: encodeBase64(data) }));
      }
    });
    const resizeSub = term.onResize(() => sendResize());

    (async () => {
      let ticket: string;
      try {
        const resp = await api<TicketResponse>(`/sandboxes/${sandboxID}/terminal/ticket`, {
          method: 'POST',
        });
        ticket = resp.ticket;
      } catch (err) {
        if (disposed) return;
        setStatus('error');
        setStatusDetail(err instanceof Error ? err.message : t('terminal.status.error'));
        return;
      }
      if (disposed) return;

      const socket = new WebSocket(terminalWsUrl(sandboxID, ticket));
      ws = socket;
      wsRef.current = socket;

      socket.onmessage = (event) => {
        if (disposed) return;
        let msg: { type?: string; data?: string; message?: string; exitCode?: number | null };
        try {
          msg = JSON.parse(typeof event.data === 'string' ? event.data : '');
        } catch {
          return;
        }
        switch (msg.type) {
          case 'ready':
            setStatus('ready');
            sendResize();
            break;
          case 'output':
            if (msg.data) term.write(decoderRef.current.decode(decodeBase64(msg.data)));
            break;
          case 'warning':
            // Non-fatal: surface briefly but keep the session open.
            setStatusDetail(msg.message ?? '');
            break;
          case 'exit':
            setStatus('closed');
            setStatusDetail(t('terminal.exited', { code: msg.exitCode ?? 0 }));
            term.write(
              `\r\n\x1b[90m${t('terminal.exited', { code: msg.exitCode ?? 0 })}\x1b[0m\r\n`,
            );
            break;
          case 'error':
            setStatus('error');
            setStatusDetail(msg.message ?? t('terminal.status.error'));
            term.write(`\r\n\x1b[31m${msg.message ?? t('terminal.status.error')}\x1b[0m\r\n`);
            break;
          default:
            break;
        }
      };

      socket.onerror = () => {
        if (disposed) return;
        setStatus('error');
        setStatusDetail(t('terminal.status.error'));
      };

      socket.onclose = () => {
        if (disposed) return;
        setStatus((prev) => (prev === 'ready' || prev === 'connecting' ? 'closed' : prev));
      };
    })();

    return () => {
      disposed = true;
      inputSub.dispose();
      resizeSub.dispose();
      if (ws) {
        // Null every handler before closing so a message/close/error firing
        // after term.dispose() cannot call into a disposed xterm instance.
        detachSocket(ws);
        ws.close();
      }
      wsRef.current = null;
      term.dispose();
      termRef.current = null;
      fitRef.current = null;
    };
    // `attempt` drives manual reconnect; `sendResize`/`t` are stable enough.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, sandboxID, attempt]);

  // ── Refit on window resize and when toggling fullscreen ─────────────────
  useEffect(() => {
    if (!open) return;
    const onResize = () => fitRef.current?.fit();
    window.addEventListener('resize', onResize);
    // Fullscreen changes the container box; fit after the layout settles.
    const id = window.setTimeout(() => fitRef.current?.fit(), 60);
    return () => {
      window.removeEventListener('resize', onResize);
      window.clearTimeout(id);
    };
  }, [open, fullscreen]);

  const reconnect = () => {
    setStatus('connecting');
    setStatusDetail('');
    setAttempt((n) => n + 1);
  };

  const statusTone =
    status === 'ready'
      ? 'bg-emerald-500'
      : status === 'connecting'
        ? 'bg-amber-500'
        : status === 'error'
          ? 'bg-red-500'
          : 'bg-muted-foreground';

  const canReconnect = status === 'closed' || status === 'error';

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content
          aria-describedby={undefined}
          className={cn(
            'fixed z-50 flex flex-col overflow-hidden rounded-2xl border border-border/60 bg-card shadow-2xl',
            fullscreen
              ? 'inset-3'
              : 'left-1/2 top-1/2 h-[min(640px,calc(100vh-3rem))] w-[min(920px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2',
          )}
          onOpenAutoFocus={(e) => {
            // Let xterm keep focus instead of Radix pulling it to the close button.
            e.preventDefault();
          }}
        >
          <div className="flex items-center justify-between border-b border-border/60 px-4 py-2.5">
            <Dialog.Title className="flex items-center gap-2 text-sm font-semibold">
              <TerminalSquare size={15} className="text-primary" />
              {t('terminal.title')}
              <span className="font-mono text-xs font-normal text-muted-foreground">
                {sandboxID}
              </span>
            </Dialog.Title>
            <div className="flex items-center gap-3">
              <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
                <span className={cn('h-2 w-2 rounded-full', statusTone)} />
                {t(`terminal.status.${status}`)}
              </span>
              {canReconnect && (
                <button
                  type="button"
                  onClick={reconnect}
                  className="flex items-center gap-1 rounded-md px-2 py-1 text-xs text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                >
                  <RotateCw size={13} />
                  {t('terminal.reconnect')}
                </button>
              )}
              <button
                type="button"
                aria-label={t('terminal.fullscreen')}
                onClick={() => setFullscreen((v) => !v)}
                className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              >
                {fullscreen ? <Minimize2 size={15} /> : <Maximize2 size={15} />}
              </button>
              <Dialog.Close asChild>
                <button
                  type="button"
                  aria-label="close"
                  className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                >
                  <X size={15} />
                </button>
              </Dialog.Close>
            </div>
          </div>

          {statusDetail && (
            <div className="border-b border-border/60 bg-muted/40 px-4 py-1.5 text-xs text-muted-foreground">
              {statusDetail}
            </div>
          )}

          <div className="min-h-0 flex-1 bg-[#0b0f14] p-2">
            <div ref={mountRef} className="h-full w-full" />
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
