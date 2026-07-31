// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import * as Dialog from '@radix-ui/react-dialog';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import { ClipboardPaste, Copy, Plus, RefreshCw, TerminalSquare, X } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import { sandboxApi } from '@/api/client';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';

type TerminalStatus = 'connecting' | 'connected' | 'closed' | 'error';

interface TerminalDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSessionActiveChange?: (active: boolean) => void;
  sandboxID: string;
}

interface TerminalFrame {
  type?: string;
  message?: string;
  exitCode?: number;
}

interface TerminalSessionState {
  id: string;
  index: number;
  status: TerminalStatus;
  message: string;
  connectionAttempt: number;
}

interface TerminalSessionPaneProps {
  active: boolean;
  sandboxID: string;
  session: TerminalSessionState;
  onReconnect: (id: string) => void;
  onStatusChange: (id: string, status: TerminalStatus, message: string) => void;
}

const terminalMaxSessions = 5;
const terminalGrantProtocolPrefix = 'cube-terminal.grant.';
const terminalCleanCloseCodes = new Set([1000, 1001]);

export function TerminalDialog({
  open,
  onOpenChange,
  onSessionActiveChange,
  sandboxID,
}: TerminalDialogProps): JSX.Element {
  const { t } = useTranslation('sandboxDetail');
  const sequenceRef = useRef(0);
  const translationRef = useRef(t);
  const [sessions, setSessions] = useState<TerminalSessionState[]>([]);
  const [activeSessionID, setActiveSessionID] = useState('');
  translationRef.current = t;

  const createSessionState = useCallback((): TerminalSessionState => {
    sequenceRef.current += 1;
    return {
      id: `terminal-${sequenceRef.current}`,
      index: sequenceRef.current,
      status: 'connecting',
      message: translationRef.current('terminal.status.connecting'),
      connectionAttempt: 0,
    };
  }, []);

  useEffect(() => {
    if (!open) {
      setSessions([]);
      setActiveSessionID('');
      return;
    }
    const session = createSessionState();
    setSessions([session]);
    setActiveSessionID(session.id);
  }, [createSessionState, open, sandboxID]);

  const hasActiveSession = useMemo(
    () =>
      sessions.some((session) => session.status === 'connecting' || session.status === 'connected'),
    [sessions],
  );

  useEffect(() => {
    onSessionActiveChange?.(hasActiveSession);
  }, [hasActiveSession, onSessionActiveChange]);

  useEffect(
    () => () => {
      onSessionActiveChange?.(false);
    },
    [onSessionActiveChange],
  );

  const addSession = () => {
    if (sessions.length >= terminalMaxSessions) return;
    const session = createSessionState();
    setSessions((current) =>
      current.length >= terminalMaxSessions ? current : [...current, session],
    );
    setActiveSessionID(session.id);
  };

  const closeSession = (id: string) => {
    setSessions((current) => {
      const closedIndex = current.findIndex((session) => session.id === id);
      const next = current.filter((session) => session.id !== id);
      if (activeSessionID === id) {
        const fallbackIndex = Math.max(0, Math.min(closedIndex - 1, next.length - 1));
        setActiveSessionID(next[fallbackIndex]?.id ?? '');
      }
      return next;
    });
  };

  const updateSessionStatus = useCallback((id: string, status: TerminalStatus, message: string) => {
    setSessions((current) =>
      current.map((session) => (session.id === id ? { ...session, status, message } : session)),
    );
  }, []);

  const reconnectSession = useCallback(
    (id: string) => {
      setSessions((current) =>
        current.map((session) =>
          session.id === id
            ? {
                ...session,
                status: 'connecting',
                message: t('terminal.status.connecting'),
                connectionAttempt: session.connectionAttempt + 1,
              }
            : session,
        ),
      );
    },
    [t],
  );

  const sessionLimitReached = sessions.length >= terminalMaxSessions;

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/60" />
        <Dialog.Content
          data-resizable="true"
          className="fixed left-4 top-4 z-50 flex h-[calc(100vh-2rem)] w-[calc(100vw-2rem)] resize flex-col overflow-hidden rounded-lg border border-zinc-700 bg-zinc-950 shadow-2xl md:left-8 md:top-8 md:h-[calc(100vh-4rem)] md:w-[calc(100vw-4rem)]"
          style={{
            minWidth: 'min(28rem, calc(100vw - 2rem))',
            minHeight: 'min(22rem, calc(100vh - 2rem))',
            maxWidth: 'calc(100vw - 2rem)',
            maxHeight: 'calc(100vh - 2rem)',
          }}
          onOpenAutoFocus={(event) => event.preventDefault()}
        >
          <Dialog.Title className="sr-only">{t('terminal.title', { sandboxID })}</Dialog.Title>
          <Dialog.Description className="sr-only">{t('terminal.description')}</Dialog.Description>
          <span className="sr-only">{t('terminal.resizeHint')}</span>

          <div className="flex min-h-11 items-center gap-2 border-b border-zinc-800 px-3 text-zinc-200">
            <TerminalSquare size={16} />
            <span className="flex-1 truncate font-mono text-xs">{sandboxID}</span>
            <span className="hidden text-[11px] text-zinc-500 sm:inline">
              {t('terminal.mainContainerOnly')}
            </span>
            <Dialog.Close asChild>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                title={t('terminal.actions.close')}
                aria-label={t('terminal.actions.close')}
              >
                <X size={15} />
              </Button>
            </Dialog.Close>
          </div>

          <div className="flex min-h-10 items-center gap-1 overflow-x-auto border-b border-zinc-800 px-2">
            <div role="tablist" aria-label={t('terminal.tabs.list')} className="flex gap-1">
              {sessions.map((session) => {
                const label = t('terminal.tabs.label', { index: session.index });
                const selected = session.id === activeSessionID;
                return (
                  <div
                    key={session.id}
                    className={cn(
                      'flex items-center rounded-md border text-xs',
                      selected
                        ? 'border-zinc-600 bg-zinc-800 text-zinc-100'
                        : 'border-transparent text-zinc-500 hover:text-zinc-300',
                    )}
                  >
                    <button
                      type="button"
                      role="tab"
                      aria-selected={selected}
                      className="px-3 py-1.5"
                      onClick={() => setActiveSessionID(session.id)}
                    >
                      {label}
                    </button>
                    <button
                      type="button"
                      className="mr-1 rounded p-1 hover:bg-zinc-700"
                      aria-label={t('terminal.tabs.close', { label })}
                      title={t('terminal.tabs.close', { label })}
                      onClick={() => closeSession(session.id)}
                    >
                      <X size={12} />
                    </button>
                  </div>
                );
              })}
            </div>
            <button
              type="button"
              className="rounded p-1.5 text-zinc-400 hover:bg-zinc-800 hover:text-zinc-100 disabled:cursor-not-allowed disabled:opacity-40"
              aria-label={t('terminal.tabs.new')}
              title={
                sessionLimitReached ? t('terminal.status.sessionLimit') : t('terminal.tabs.new')
              }
              disabled={sessionLimitReached}
              onClick={addSession}
            >
              <Plus size={14} />
            </button>
          </div>

          <div className="min-h-0 flex-1">
            {sessions.length === 0 ? (
              <div className="flex h-full flex-col items-center justify-center gap-3 text-sm text-zinc-500">
                <span>{t('terminal.empty')}</span>
                <Button type="button" variant="outline" size="sm" onClick={addSession}>
                  <Plus size={14} /> {t('terminal.tabs.new')}
                </Button>
              </div>
            ) : null}
            {sessions.map((session) => (
              <TerminalSessionPane
                key={session.id}
                active={session.id === activeSessionID}
                sandboxID={sandboxID}
                session={session}
                onReconnect={reconnectSession}
                onStatusChange={updateSessionStatus}
              />
            ))}
          </div>
          <span
            aria-hidden="true"
            className="pointer-events-none absolute bottom-1 right-1 z-10 h-3 w-3 border-b-2 border-r-2 border-zinc-500"
          />
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function TerminalSessionPane({
  active,
  sandboxID,
  session,
  onReconnect,
  onStatusChange,
}: TerminalSessionPaneProps): JSX.Element {
  const { t } = useTranslation('sandboxDetail');
  const [host, setHost] = useState<HTMLDivElement | null>(null);
  const terminalRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const socketRef = useRef<WebSocket | null>(null);
  const resizeTimerRef = useRef(0);
  const connectionGenerationRef = useRef(0);
  const callbacksRef = useRef({ onStatusChange, t });
  const clipboardActionsRef = useRef({
    copy: async () => {},
    paste: async () => {},
  });
  callbacksRef.current = { onStatusChange, t };

  const setStatus = (status: TerminalStatus, message: string) => {
    callbacksRef.current.onStatusChange(session.id, status, message);
  };

  const sendResize = useCallback(() => {
    const terminal = terminalRef.current;
    const fit = fitRef.current;
    if (!terminal || !fit) return;
    fit.fit();
    const socket = socketRef.current;
    if (socket?.readyState === WebSocket.OPEN && terminal.rows > 0 && terminal.cols > 0) {
      socket.send(JSON.stringify({ type: 'resize', rows: terminal.rows, cols: terminal.cols }));
    }
  }, []);

  const scheduleResize = useCallback(() => {
    window.clearTimeout(resizeTimerRef.current);
    resizeTimerRef.current = window.setTimeout(sendResize, 80);
  }, [sendResize]);

  const copySelection = useCallback(async () => {
    const selection = terminalRef.current?.getSelection() ?? '';
    if (!selection) return;
    try {
      await navigator.clipboard.writeText(selection);
    } catch {
      setStatus(session.status, t('terminal.status.clipboardError'));
    }
  }, [session.status, t]);

  const pasteClipboard = useCallback(async () => {
    if (socketRef.current?.readyState !== WebSocket.OPEN) return;
    try {
      const text = await navigator.clipboard.readText();
      if (text) {
        terminalRef.current?.paste(text);
        terminalRef.current?.focus();
      }
    } catch {
      setStatus(session.status, t('terminal.status.clipboardError'));
    }
  }, [session.status, t]);

  clipboardActionsRef.current = {
    copy: copySelection,
    paste: pasteClipboard,
  };

  useEffect(() => {
    if (!host) return;
    const terminal = new Terminal({
      cursorBlink: true,
      fontFamily: '"JetBrains Mono Variable", "JetBrains Mono", monospace',
      fontSize: 13,
      scrollback: 5000,
      theme: { background: '#09090b', foreground: '#e4e4e7', cursor: '#f4f4f5' },
    });
    const fit = new FitAddon();
    terminalRef.current = terminal;
    fitRef.current = fit;
    terminal.loadAddon(fit);
    terminal.open(host);

    terminal.attachCustomKeyEventHandler((event) => {
      if (!(event.shiftKey && (event.ctrlKey || event.metaKey))) return true;
      if (event.key.toLowerCase() === 'c') {
        void clipboardActionsRef.current.copy();
        return false;
      }
      if (event.key.toLowerCase() === 'v') {
        void clipboardActionsRef.current.paste();
        return false;
      }
      return true;
    });

    const input = terminal.onData((data) => {
      const socket = socketRef.current;
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(new TextEncoder().encode(data));
      }
    });
    const observer = new ResizeObserver(scheduleResize);
    observer.observe(host);
    scheduleResize();

    return () => {
      window.clearTimeout(resizeTimerRef.current);
      observer.disconnect();
      input.dispose();
      const socket = socketRef.current;
      socketRef.current = null;
      if (socket && socket.readyState !== WebSocket.CLOSED) {
        socket.close(1000, 'terminal tab closed');
      }
      terminal.dispose();
      terminalRef.current = null;
      fitRef.current = null;
    };
  }, [host, scheduleResize]);

  useEffect(() => {
    if (!active) return;
    scheduleResize();
    terminalRef.current?.focus();
  }, [active, scheduleResize]);

  useEffect(() => {
    const terminal = terminalRef.current;
    if (!host || !terminal) return;
    const generation = connectionGenerationRef.current + 1;
    connectionGenerationRef.current = generation;
    const translate = callbacksRef.current.t;
    let cancelled = false;
    let ended = false;

    const previousSocket = socketRef.current;
    socketRef.current = null;
    if (previousSocket && previousSocket.readyState !== WebSocket.CLOSED) {
      previousSocket.close(1000, 'starting a new terminal shell');
    }
    if (session.connectionAttempt > 0) {
      terminal.reset();
      terminal.writeln(`\r\n${translate('terminal.status.newShell')}`);
    }
    setStatus('connecting', translate('terminal.status.connecting'));

    const showError = (message: string) => {
      ended = true;
      setStatus('error', message);
      terminal.writeln(`\r\n${message}`);
    };

    const handleControlFrame = (frame: TerminalFrame, socket: WebSocket) => {
      if (frame.type === 'status') {
        setStatus('connected', translate('terminal.status.connected'));
        return;
      }
      if (frame.type === 'exit') {
        ended = true;
        const message = translate('terminal.status.exited', { code: frame.exitCode ?? 0 });
        setStatus('closed', message);
        return;
      }
      if (frame.type === 'error') {
        showError(frame.message || translate('terminal.status.error'));
        return;
      }
      showError(translate('terminal.status.protocolError'));
      socket.close(1002, 'invalid terminal protocol response');
    };

    const connect = async () => {
      try {
        const grant = await sandboxApi.createTerminalSession(sandboxID);
        if (cancelled || connectionGenerationRef.current !== generation) return;
        const socket = new WebSocket(toWebSocketURL(grant.websocketURL), [
          grant.protocol,
          `${terminalGrantProtocolPrefix}${grant.grant}`,
        ]);
        socket.binaryType = 'arraybuffer';
        socketRef.current = socket;
        socket.onopen = () => {
          if (cancelled || connectionGenerationRef.current !== generation) return;
          sendResize();
          terminal.focus();
        };
        socket.onmessage = (event) => {
          if (cancelled || connectionGenerationRef.current !== generation) return;
          if (event.data instanceof ArrayBuffer) {
            terminal.write(new Uint8Array(event.data));
            return;
          }
          try {
            handleControlFrame(JSON.parse(String(event.data)) as TerminalFrame, socket);
          } catch {
            showError(translate('terminal.status.protocolError'));
            socket.close(1002, 'invalid terminal protocol response');
          }
        };
        socket.onclose = (event) => {
          if (cancelled || ended || connectionGenerationRef.current !== generation) {
            return;
          }
          if (terminalCleanCloseCodes.has(event.code)) {
            setStatus('closed', event.reason || translate('terminal.status.closed'));
            return;
          }
          const message = event.reason
            ? translate('terminal.status.connectionLostReason', { reason: event.reason })
            : translate('terminal.status.connectionLost');
          showError(message);
        };
      } catch (error) {
        if (cancelled || connectionGenerationRef.current !== generation) return;
        showError(error instanceof Error ? error.message : translate('terminal.status.error'));
      }
    };

    void connect();
    return () => {
      cancelled = true;
      const socket = socketRef.current;
      if (socket && socket.readyState !== WebSocket.CLOSED) {
        socket.close(1000, 'terminal connection replaced');
      }
      if (socketRef.current === socket) {
        socketRef.current = null;
      }
    };
  }, [host, sandboxID, sendResize, session.connectionAttempt, session.id]);

  const reconnectAvailable = session.status === 'closed' || session.status === 'error';

  return (
    <div className={cn('h-full min-h-0 flex-col', active ? 'flex' : 'hidden')}>
      <div className="flex min-h-10 items-center gap-1 border-b border-zinc-800 px-2">
        <Button
          type="button"
          variant="ghost"
          size="sm"
          title={t('terminal.actions.copy')}
          aria-label={t('terminal.actions.copy')}
          onClick={() => void copySelection()}
        >
          <Copy size={14} /> {t('terminal.actions.copy')}
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          title={t('terminal.actions.paste')}
          aria-label={t('terminal.actions.paste')}
          disabled={session.status !== 'connected'}
          onClick={() => void pasteClipboard()}
        >
          <ClipboardPaste size={14} /> {t('terminal.actions.paste')}
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          title={t('terminal.actions.reconnect')}
          aria-label={t('terminal.actions.reconnect')}
          disabled={!reconnectAvailable}
          onClick={() => onReconnect(session.id)}
        >
          <RefreshCw size={14} /> {t('terminal.actions.reconnect')}
        </Button>
        <span className="ml-auto truncate px-2 text-xs text-zinc-400">{session.message}</span>
      </div>
      <div ref={setHost} className="min-h-0 flex-1 bg-[#09090b] p-2 [&_.xterm]:h-full" />
    </div>
  );
}

function toWebSocketURL(path: string): string {
  const url = new URL(path, window.location.origin);
  url.protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  url.host = window.location.host;
  return url.toString();
}
