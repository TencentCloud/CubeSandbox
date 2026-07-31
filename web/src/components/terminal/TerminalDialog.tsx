// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import * as Dialog from '@radix-ui/react-dialog';
import {
  Expand,
  Minimize2,
  Minus,
  Plus,
  PlugZap,
  Power,
  RefreshCw,
  SquareTerminal,
  X,
} from 'lucide-react';
import { Terminal } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';

import { terminalApi, type SandboxContainer } from '@/api/client';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import { terminalWebSocketURL } from '@/lib/terminal';

type TerminalStatus =
  'idle' | 'connecting' | 'connected' | 'reconnected' | 'disconnected' | 'closed' | 'error';

interface Props {
  sandboxID: string;
  containers?: SandboxContainer[];
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

interface ServerControlMessage {
  type: 'ready' | 'error' | 'exit' | 'pong';
  sessionID?: string;
  containerID?: string;
  reconnected?: boolean;
  code?: string;
  message?: string;
  recoverable?: boolean;
  exitCode?: number;
  reason?: string;
}

const MIN_FONT_SIZE = 10;
const MAX_FONT_SIZE = 22;

function primaryContainer(sandboxID: string, containers: SandboxContainer[]): SandboxContainer {
  return (
    containers.find(
      (container) => container.type === 'sandbox' || container.containerID === sandboxID,
    ) ??
    containers[0] ?? {
      containerID: sandboxID,
      name: '',
      state: 'running',
      type: 'sandbox',
      envdPort: 49983,
    }
  );
}

export function TerminalDialog({ sandboxID, containers = [], open, onOpenChange }: Props) {
  const { t } = useTranslation('terminal');
  const availableContainers = useMemo(
    () =>
      containers.filter(
        (container) => container.state === 'running' && (container.envdPort ?? 0) > 0,
      ),
    [containers],
  );
  const initialContainer = primaryContainer(sandboxID, availableContainers);
  const [selectedContainerID, setSelectedContainerID] = useState(initialContainer.containerID);
  const [status, setStatus] = useState<TerminalStatus>('idle');
  const [notice, setNotice] = useState('');
  const [fontSize, setFontSize] = useState(14);
  const [fullscreen, setFullscreen] = useState(false);
  const [sessionID, setSessionID] = useState('');

  const mountRef = useRef<HTMLDivElement>(null);
  const terminalRef = useRef<Terminal | null>(null);
  const socketRef = useRef<WebSocket | null>(null);
  const selectedRef = useRef(selectedContainerID);
  const sessionRef = useRef('');
  const manualCloseRef = useRef(false);
  const connectionGeneration = useRef(0);
  const startConnectionRef = useRef<(reconnect: boolean) => void>(() => undefined);

  selectedRef.current = selectedContainerID;
  sessionRef.current = sessionID;

  const containerLabel = (containerID: string) => {
    const container = availableContainers.find((item) => item.containerID === containerID);
    return container?.name || (container?.type === 'sandbox' ? t('primaryContainer') : containerID);
  };

  useEffect(() => {
    if (!open || !mountRef.current) return;

    const terminal = new Terminal({
      cursorBlink: true,
      cursorStyle: 'block',
      convertEol: false,
      fontFamily: '"JetBrains Mono Variable", "SFMono-Regular", Consolas, monospace',
      fontSize,
      lineHeight: 1.15,
      scrollback: 10_000,
      allowProposedApi: false,
      theme: {
        background: '#080c12',
        foreground: '#dce7f5',
        cursor: '#7aa2f7',
        selectionBackground: '#334a74',
        black: '#151b26',
        red: '#f7768e',
        green: '#9ece6a',
        yellow: '#e0af68',
        blue: '#7aa2f7',
        magenta: '#bb9af7',
        cyan: '#7dcfff',
        white: '#c0caf5',
      },
    });
    const fitAddon = new FitAddon();
    terminal.loadAddon(fitAddon);
    terminal.open(mountRef.current);
    terminalRef.current = terminal;

    terminal.attachCustomKeyEventHandler((event) => {
      const modifier = event.ctrlKey || event.metaKey;
      if (!modifier) return true;
      if (event.key.toLowerCase() === 'c' && terminal.hasSelection()) {
        void navigator.clipboard?.writeText(terminal.getSelection());
        terminal.clearSelection();
        return false;
      }
      if (event.key.toLowerCase() === 'v') {
        void navigator.clipboard?.readText().then((text) => terminal.paste(text));
        return false;
      }
      return true;
    });

    const dataDisposable = terminal.onData((data) => {
      const socket = socketRef.current;
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ type: 'input', data }));
      }
    });
    const resizeDisposable = terminal.onResize(({ rows, cols }) => {
      const socket = socketRef.current;
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ type: 'resize', rows, cols }));
      }
    });

    const fit = () => {
      try {
        fitAddon.fit();
      } catch {
        // The dialog may be between layout states; the next observer callback retries.
      }
    };
    const resizeObserver = new ResizeObserver(fit);
    resizeObserver.observe(mountRef.current);
    requestAnimationFrame(() => {
      fit();
      terminal.focus();
    });

    const closeCurrentSocket = (active: boolean) => {
      const socket = socketRef.current;
      if (!socket) return;
      if (active && socket.readyState === WebSocket.OPEN) {
        manualCloseRef.current = true;
        socket.send(JSON.stringify({ type: 'disconnect' }));
      }
      socket.close();
      socketRef.current = null;
    };

    const startConnection = async (reconnect: boolean) => {
      const generation = ++connectionGeneration.current;
      manualCloseRef.current = false;
      setStatus('connecting');
      const selected = selectedRef.current;
      setNotice(t('messages.connecting', { container: containerLabel(selected) }));
      terminal.options.disableStdin = true;

      try {
        fit();
        const ticket = await terminalApi.ticket(sandboxID, {
          containerID: selected,
          sessionID: reconnect ? sessionRef.current : undefined,
          rows: terminal.rows || 24,
          cols: terminal.cols || 80,
        });
        if (generation !== connectionGeneration.current) return;

        const socket = new WebSocket(terminalWebSocketURL(ticket.wsPath, ticket.ticket));
        socket.binaryType = 'arraybuffer';
        socketRef.current = socket;

        socket.onmessage = (event) => {
          if (generation !== connectionGeneration.current) return;
          if (event.data instanceof ArrayBuffer) {
            terminal.write(new Uint8Array(event.data));
            return;
          }
          if (event.data instanceof Blob) {
            void event.data.arrayBuffer().then((data) => terminal.write(new Uint8Array(data)));
            return;
          }

          let control: ServerControlMessage;
          try {
            control = JSON.parse(String(event.data)) as ServerControlMessage;
          } catch {
            return;
          }
          if (control.type === 'ready') {
            const nextSessionID = control.sessionID ?? '';
            setSessionID(nextSessionID);
            sessionRef.current = nextSessionID;
            setStatus(control.reconnected ? 'reconnected' : 'connected');
            setNotice(
              control.reconnected
                ? t('messages.reconnected')
                : t('messages.ready', { container: containerLabel(selected) }),
            );
            terminal.options.disableStdin = false;
            terminal.focus();
          } else if (control.type === 'error') {
            setNotice(
              control.code === 'idle_timeout'
                ? t('messages.idleTimeout')
                : control.message || t('status.error'),
            );
            if (!control.recoverable) {
              manualCloseRef.current = true;
              terminal.options.disableStdin = true;
              setStatus(control.code === 'idle_timeout' ? 'closed' : 'error');
            }
          } else if (control.type === 'exit') {
            manualCloseRef.current = true;
            terminal.options.disableStdin = true;
            setStatus('closed');
            setNotice(t('messages.sessionClosed'));
            setSessionID('');
            sessionRef.current = '';
          }
        };
        socket.onerror = () => {
          // onclose supplies the user-facing reconnect state.
        };
        socket.onclose = () => {
          if (generation !== connectionGeneration.current) return;
          socketRef.current = null;
          terminal.options.disableStdin = true;
          if (!manualCloseRef.current) {
            setStatus('disconnected');
            setNotice(t('messages.disconnected'));
          }
        };
      } catch (error) {
        if (generation !== connectionGeneration.current) return;
        terminal.options.disableStdin = true;
        setStatus('error');
        setNotice(error instanceof Error ? error.message : String(error));
      }
    };
    startConnectionRef.current = (reconnect) => {
      void startConnection(reconnect);
    };
    startConnectionRef.current(false);

    return () => {
      connectionGeneration.current++;
      closeCurrentSocket(true);
      resizeObserver.disconnect();
      dataDisposable.dispose();
      resizeDisposable.dispose();
      terminal.dispose();
      terminalRef.current = null;
      startConnectionRef.current = () => undefined;
      setFullscreen(false);
      setStatus('idle');
      setNotice('');
      setSessionID('');
      sessionRef.current = '';
    };
    // The terminal lifecycle follows the dialog. Mutable refs carry container
    // and session changes without recreating xterm.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, sandboxID]);

  useEffect(() => {
    if (terminalRef.current) {
      terminalRef.current.options.fontSize = fontSize;
      requestAnimationFrame(() => window.dispatchEvent(new Event('resize')));
    }
  }, [fontSize]);

  useEffect(() => {
    if (open) {
      requestAnimationFrame(() => window.dispatchEvent(new Event('resize')));
    }
  }, [fullscreen, open]);

  const activelyDisconnect = () => {
    const socket = socketRef.current;
    manualCloseRef.current = true;
    if (socket?.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify({ type: 'disconnect' }));
    }
    socket?.close();
    socketRef.current = null;
    if (terminalRef.current) {
      terminalRef.current.options.disableStdin = true;
    }
    setStatus('closed');
    setNotice(t('messages.sessionClosed'));
    setSessionID('');
    sessionRef.current = '';
  };

  const startNewSession = () => {
    activelyDisconnect();
    manualCloseRef.current = false;
    startConnectionRef.current(false);
  };

  const switchContainer = (containerID: string) => {
    activelyDisconnect();
    selectedRef.current = containerID;
    setSelectedContainerID(containerID);
    setNotice(t('messages.switchContainer'));
    manualCloseRef.current = false;
    startConnectionRef.current(false);
  };

  const statusTone =
    status === 'connected' || status === 'reconnected'
      ? 'text-cube-ok'
      : status === 'connecting'
        ? 'text-cube-info'
        : status === 'disconnected' || status === 'error'
          ? 'text-cube-err'
          : 'text-muted-foreground';

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-background/80 backdrop-blur-sm" />
        <Dialog.Content
          className={cn(
            'fixed z-50 flex flex-col overflow-hidden border border-border/70 bg-card shadow-2xl outline-none',
            fullscreen
              ? 'inset-2 rounded-xl'
              : 'left-1/2 top-1/2 h-[min(760px,calc(100vh-3rem))] w-[min(1100px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-2xl',
          )}
        >
          <div className="flex flex-wrap items-center gap-2 border-b border-border/60 px-4 py-3">
            <Dialog.Title className="mr-2 flex items-center gap-2 text-sm font-semibold">
              <SquareTerminal size={16} className="text-primary" />
              {t('title')}
            </Dialog.Title>
            <span className="max-w-[260px] truncate font-mono text-xs text-muted-foreground">
              {sandboxID}
            </span>
            <span className={cn('ml-auto text-xs font-medium', statusTone)}>
              {t(`status.${status}`)}
            </span>

            <label className="sr-only" htmlFor="terminal-container">
              {t('container')}
            </label>
            <select
              id="terminal-container"
              value={selectedContainerID}
              onChange={(event) => switchContainer(event.target.value)}
              className="h-8 max-w-[240px] rounded-md border border-border/60 bg-background px-2 text-xs"
              title={t('messages.switchContainer')}
            >
              {(availableContainers.length ? availableContainers : [initialContainer]).map(
                (container) => (
                  <option key={container.containerID} value={container.containerID}>
                    {containerLabel(container.containerID)}
                  </option>
                ),
              )}
            </select>

            <Button
              size="icon"
              variant="ghost"
              title={t('actions.decreaseFont')}
              aria-label={t('actions.decreaseFont')}
              disabled={fontSize <= MIN_FONT_SIZE}
              onClick={() => setFontSize((size) => Math.max(MIN_FONT_SIZE, size - 1))}
            >
              <Minus size={14} />
            </Button>
            <span className="w-8 text-center font-mono text-xs text-muted-foreground">
              {fontSize}
            </span>
            <Button
              size="icon"
              variant="ghost"
              title={t('actions.increaseFont')}
              aria-label={t('actions.increaseFont')}
              disabled={fontSize >= MAX_FONT_SIZE}
              onClick={() => setFontSize((size) => Math.min(MAX_FONT_SIZE, size + 1))}
            >
              <Plus size={14} />
            </Button>
            <Button
              size="icon"
              variant="ghost"
              title={fullscreen ? t('actions.exitFullscreen') : t('actions.fullscreen')}
              aria-label={fullscreen ? t('actions.exitFullscreen') : t('actions.fullscreen')}
              onClick={() => setFullscreen((value) => !value)}
            >
              {fullscreen ? <Minimize2 size={14} /> : <Expand size={14} />}
            </Button>
            <Dialog.Close asChild>
              <Button
                size="icon"
                variant="ghost"
                title={t('actions.close')}
                aria-label={t('actions.close')}
              >
                <X size={15} />
              </Button>
            </Dialog.Close>
          </div>

          <div className="flex items-center gap-2 border-b border-border/50 bg-muted/30 px-4 py-2 text-xs">
            {status === 'connecting' ? (
              <PlugZap size={13} className="animate-pulse text-cube-info" />
            ) : null}
            <span className="min-w-0 flex-1 truncate text-muted-foreground">{notice}</span>
            {status === 'disconnected' && sessionID ? (
              <Button size="sm" variant="outline" onClick={() => startConnectionRef.current(true)}>
                <RefreshCw size={13} /> {t('actions.reconnect')}
              </Button>
            ) : null}
            {status === 'connected' || status === 'reconnected' ? (
              <Button size="sm" variant="outline" onClick={activelyDisconnect}>
                <Power size={13} /> {t('actions.disconnect')}
              </Button>
            ) : null}
            {status === 'closed' || status === 'error' ? (
              <Button size="sm" variant="outline" onClick={startNewSession}>
                <SquareTerminal size={13} /> {t('actions.newSession')}
              </Button>
            ) : null}
          </div>

          <div className="min-h-0 flex-1 bg-[#080c12] p-2">
            <div ref={mountRef} className="h-full w-full" aria-label={t('title')} />
          </div>
          <p className="border-t border-border/50 px-4 py-2 text-[11px] text-muted-foreground">
            {t('messages.secureTransport')}
          </p>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
