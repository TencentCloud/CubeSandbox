// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import * as Dialog from '@radix-ui/react-dialog';
import { X, RotateCw, Maximize2, Minimize2 } from 'lucide-react';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';
import { useTerminalSocket, type TerminalStatus } from './useTerminalSocket';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  sandboxID: string;
}

/** Terminal colours tuned to the dashboard's dark surface. */
const XTERM_THEME = {
  background: '#0b0f19',
  foreground: '#e2e8f0',
  cursor: '#e2e8f0',
  selectionBackground: '#334155',
  black: '#0b0f19',
  red: '#f87171',
  green: '#4ade80',
  yellow: '#facc15',
  blue: '#60a5fa',
  magenta: '#c084fc',
  cyan: '#22d3ee',
  white: '#e2e8f0',
  brightBlack: '#475569',
  brightRed: '#fca5a5',
  brightGreen: '#86efac',
  brightYellow: '#fde047',
  brightBlue: '#93c5fd',
  brightMagenta: '#d8b4fe',
  brightCyan: '#67e8f9',
  brightWhite: '#f8fafc',
};

const STATUS_TONE: Record<TerminalStatus, 'ok' | 'warn' | 'err' | 'mute'> = {
  idle: 'mute',
  connecting: 'warn',
  connected: 'ok',
  reconnecting: 'warn',
  disconnected: 'warn',
  exited: 'mute',
  error: 'err',
};

export function TerminalDialog({ open, onOpenChange, sandboxID }: Props) {
  const { t } = useTranslation('terminal');
  const [fullscreen, setFullscreen] = useState(false);

  // Radix renders portal children on a second pass, so a plain ref is still
  // null when the mount effect first runs. Tracking the node in state makes
  // the effect fire once the element actually exists.
  const [host, setHost] = useState<HTMLDivElement | null>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  // Buffers output that arrives before xterm has mounted (the socket opens as
  // soon as the dialog does, which can beat the first layout pass).
  const pendingRef = useRef<string[]>([]);

  const handleOutput = useCallback((data: string) => {
    if (termRef.current) {
      termRef.current.write(data);
    } else {
      pendingRef.current.push(data);
    }
  }, []);

  const getSize = useCallback(() => {
    const term = termRef.current;
    return term ? { cols: term.cols, rows: term.rows } : { cols: 80, rows: 24 };
  }, []);

  const socket = useTerminalSocket({
    sandboxID,
    enabled: open,
    onOutput: handleOutput,
    getSize,
  });

  const { sendInput, sendResize } = socket;

  // Mount xterm once the dialog content exists, and tear it down on close so
  // a reopened dialog starts from a clean screen.
  useEffect(() => {
    if (!open || !host) return;

    const term = new Terminal({
      cursorBlink: true,
      fontFamily: '"JetBrains Mono Variable", "JetBrains Mono", ui-monospace, monospace',
      fontSize: 13,
      lineHeight: 1.2,
      theme: XTERM_THEME,
      scrollback: 5000,
      convertEol: false,
      allowProposedApi: true,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    fit.fit();
    term.focus();

    termRef.current = term;
    fitRef.current = fit;
    for (const chunk of pendingRef.current) term.write(chunk);
    pendingRef.current = [];

    const disposable = term.onData((data) => sendInput(data));

    // Keep the PTY's window size in step with the rendered element.
    const observer = new ResizeObserver(() => {
      try {
        fit.fit();
        sendResize(term.cols, term.rows);
      } catch {
        // fit() throws while the element is detached mid-close; harmless.
      }
    });
    observer.observe(host);

    return () => {
      observer.disconnect();
      disposable.dispose();
      term.dispose();
      termRef.current = null;
      fitRef.current = null;
      pendingRef.current = [];
    };
  }, [open, host, sendInput, sendResize]);

  // Refit after the fullscreen toggle changes the container's box.
  useEffect(() => {
    if (!open) return;
    const term = termRef.current;
    const fit = fitRef.current;
    if (!term || !fit) return;
    const id = window.setTimeout(() => {
      try {
        fit.fit();
        sendResize(term.cols, term.rows);
      } catch {
        // See above.
      }
    }, 50);
    return () => window.clearTimeout(id);
  }, [fullscreen, open, sendResize]);

  const statusLabel =
    socket.status === 'exited'
      ? t('status.exited', { code: socket.exitCode ?? 0 })
      : socket.status === 'idle'
        ? t('status.connecting')
        : t(`status.${socket.status}`);

  const hint =
    socket.status === 'disconnected' && socket.canReconnect
      ? t('hints.reconnectAvailable')
      : socket.status === 'disconnected'
        ? t('hints.sessionGone')
        : socket.status === 'error'
          ? (socket.detail ?? t('errors.socket'))
          : null;

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content
          className={cn(
            'fixed left-1/2 top-1/2 z-50 -translate-x-1/2 -translate-y-1/2',
            'flex flex-col overflow-hidden rounded-2xl border border-border/60 bg-card shadow-2xl',
            fullscreen
              ? 'h-[calc(100vh-1.5rem)] w-[calc(100vw-1.5rem)]'
              : 'h-[min(640px,calc(100vh-3rem))] w-[min(960px,calc(100vw-2rem))]',
          )}
          // xterm owns keyboard focus; Radix's auto-focus would steal it.
          onOpenAutoFocus={(event) => event.preventDefault()}
        >
          <div className="flex items-center justify-between border-b border-border/60 px-6 py-3">
            <div className="min-w-0">
              <Dialog.Title className="text-base font-semibold">{t('title')}</Dialog.Title>
              <Dialog.Description className="mt-0.5 truncate font-mono text-xs text-muted-foreground">
                {t('subtitle', { sandboxID })}
              </Dialog.Description>
            </div>
            <div className="flex shrink-0 items-center gap-2">
              <Badge tone={STATUS_TONE[socket.status]}>{statusLabel}</Badge>
              {socket.canReconnect && (
                <Button size="sm" variant="outline" onClick={socket.reconnect}>
                  <RotateCw size={14} /> {t('actions.reconnect')}
                </Button>
              )}
              <Button
                size="icon"
                variant="ghost"
                title={t('actions.fullscreen')}
                onClick={() => setFullscreen((v) => !v)}
              >
                {fullscreen ? <Minimize2 size={14} /> : <Maximize2 size={14} />}
              </Button>
              <Dialog.Close asChild>
                <button
                  type="button"
                  aria-label={t('actions.close')}
                  className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                >
                  <X size={16} />
                </button>
              </Dialog.Close>
            </div>
          </div>

          {hint && (
            <div className="border-b border-border/60 bg-cube-warn/10 px-6 py-2 text-xs text-cube-warn">
              {hint}
            </div>
          )}

          <div className="min-h-0 flex-1 bg-[#0b0f19] p-2">
            <div ref={setHost} className="h-full w-full" data-testid="terminal-host" />
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
