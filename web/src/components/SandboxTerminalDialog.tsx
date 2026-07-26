// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
} from 'react';
import * as Dialog from '@radix-ui/react-dialog';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal as XTerm } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import {
  ClipboardCopy,
  Eraser,
  Maximize2,
  Minimize2,
  RotateCcw,
  ScanLine,
  Terminal,
  X,
} from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { sandboxApi } from '@/api/client';
import type { SandboxContainer } from '@/api/client';
import { Button } from '@/components/ui/button';
import { isMockEnabled } from '@/lib/mockFlag';
import { cn } from '@/lib/utils';
import {
  decodeBase64Bytes,
  parseTerminalServerMessage,
  terminalInputFrame,
  terminalResizeFrame,
  toTerminalWebSocketUrl,
  type TerminalServerMessage,
} from '@/lib/terminalProtocol';

type ConnectionState = 'idle' | 'connecting' | 'connected' | 'closed' | 'error';
type TerminalFrame = { x: number; y: number; width: number; height: number };

const MIN_TERMINAL_WIDTH = 520;
const MIN_TERMINAL_HEIGHT = 360;
const TERMINAL_MARGIN = 16;

interface Props {
  sandboxId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  restoreKey?: number;
}

export function SandboxTerminalDialog({ sandboxId, open, onOpenChange, restoreKey = 0 }: Props) {
  const { t } = useTranslation('sandboxDetail');
  const contentRef = useRef<HTMLDivElement | null>(null);
  const mountRef = useRef<HTMLDivElement | null>(null);
  const termRef = useRef<XTerm | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const resizeTimerRef = useRef<number | null>(null);
  const [status, setStatus] = useState<ConnectionState>('idle');
  const [error, setError] = useState<string | null>(null);
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [execId, setExecId] = useState<string | null>(null);
  const [containers, setContainers] = useState<SandboxContainer[]>([]);
  const [selectedContainerId, setSelectedContainerId] = useState<string | undefined>();
  const [sessionKey, setSessionKey] = useState(0);
  const [mountElement, setMountElement] = useState<HTMLDivElement | null>(null);
  const [maximized, setMaximized] = useState(false);
  const [minimized, setMinimized] = useState(false);
  const [frame, setFrame] = useState<TerminalFrame>(() => initialTerminalFrame());

  const setTerminalMount = useCallback((node: HTMLDivElement | null) => {
    mountRef.current = node;
    setMountElement(node);
  }, []);

  useEffect(() => {
    if (!open || !mountElement) return;

    let disposed = false;
    const term = new XTerm({
      cursorBlink: true,
      convertEol: true,
      fontFamily: '"JetBrains Mono Variable", "JetBrains Mono", ui-monospace, SFMono-Regular, monospace',
      fontSize: 13,
      lineHeight: 1.18,
      scrollback: 5000,
      theme: {
        background: '#070b12',
        foreground: '#dce7f8',
        cursor: '#8fb6ff',
        selectionBackground: '#2d4f7f',
        black: '#0d1117',
        red: '#ff6b7a',
        green: '#4fd6a6',
        yellow: '#f7c66f',
        blue: '#77a7ff',
        magenta: '#c894ff',
        cyan: '#5ad7e8',
        white: '#e8eef8',
        brightBlack: '#657184',
        brightRed: '#ff8b97',
        brightGreen: '#6be4b9',
        brightYellow: '#ffd98a',
        brightBlue: '#9bbfff',
        brightMagenta: '#d7aaff',
        brightCyan: '#7de6f2',
        brightWhite: '#ffffff',
      },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(mountElement);
    term.focus();
    termRef.current = term;
    fitRef.current = fit;
    const mockTerminal = import.meta.env.DEV && isMockEnabled();

    const fitTerminal = () => {
      try {
        fit.fit();
      } catch {
        // xterm can throw while the dialog is entering layout.
      }
    };
    const sendResize = () => {
      const ws = wsRef.current;
      if (ws?.readyState === WebSocket.OPEN) {
        ws.send(terminalResizeFrame(term.rows, term.cols));
      }
    };
    const scheduleResize = () => {
      if (resizeTimerRef.current !== null) {
        window.clearTimeout(resizeTimerRef.current);
      }
      resizeTimerRef.current = window.setTimeout(() => {
        resizeTimerRef.current = null;
        sendResize();
      }, 120);
    };
    requestAnimationFrame(fitTerminal);

    const resizeObserver = new ResizeObserver(() => {
      requestAnimationFrame(() => {
        fitTerminal();
        scheduleResize();
      });
    });
    resizeObserver.observe(mountElement);

    const sendTerminalInput = (data: string) => {
      if (mockTerminal) {
        term.write(data);
        if (data === '\r') {
          term.write('\n$ ');
        }
        return;
      }
      const ws = wsRef.current;
      if (ws?.readyState === WebSocket.OPEN) {
        ws.send(terminalInputFrame(data));
      }
    };

    const dataDisposable = term.onData(sendTerminalInput);
    const pasteListener = (event: ClipboardEvent) => {
      const text = event.clipboardData?.getData('text/plain') ?? '';
      if (text.length > 0) {
        event.preventDefault();
        event.stopPropagation();
        sendTerminalInput(text);
      }
    };
    mountElement.addEventListener('paste', pasteListener, true);
    const resizeDisposable = term.onResize(({ rows, cols }) => {
      if (resizeTimerRef.current !== null) {
        window.clearTimeout(resizeTimerRef.current);
      }
      resizeTimerRef.current = window.setTimeout(() => {
        resizeTimerRef.current = null;
        const ws = wsRef.current;
        if (ws?.readyState === WebSocket.OPEN) {
          ws.send(terminalResizeFrame(rows, cols));
        }
      }, 120);
    });

    async function connect() {
      setStatus('connecting');
      setError(null);
      setSessionId(null);
      setExecId(null);
      term.writeln(t('terminal.connecting'));
      try {
        fitTerminal();
        const detail = await sandboxApi.get(sandboxId);
        const runningContainers = terminalContainers(detail.containers);
        setContainers(runningContainers);
        if ((detail.containers?.length ?? 0) > 0 && runningContainers.length === 0) {
          throw new Error(t('terminal.errors.noRunningContainer'));
        }
        const containerId = selectContainer(runningContainers, selectedContainerId);
        if (containerId && containerId !== selectedContainerId) {
          setSelectedContainerId(containerId);
          return;
        }
        const ticket = await sandboxApi.createTerminalTicket(sandboxId, {
          containerID: containerId,
          rows: term.rows,
          cols: term.cols,
        });
        if (disposed) return;
        if (mockTerminal) {
          setStatus('connected');
          setSessionId(`mock-${sandboxId}`);
          setExecId('mock-exec');
          term.writeln(t('terminal.started', {
            container: ticket.containerID ?? containerId ?? sandboxId,
            execId: 'mock-exec',
          }));
          term.writeln(
            `Mock terminal attached to ${ticket.containerID ?? containerId ?? sandboxId}.`,
          );
          term.write('$ ');
          return;
        }
        const ws = new WebSocket(toTerminalWebSocketUrl(ticket.websocketUrl));
        wsRef.current = ws;

        ws.onopen = () => {
          if (disposed) return;
          ws.send(terminalResizeFrame(term.rows, term.cols));
        };
        ws.onmessage = (event) => {
          if (disposed || typeof event.data !== 'string') return;
          handleServerMessage(term, event.data, {
            setStatus,
            setError,
            setSessionId,
            setExecId,
            containerId: ticket.containerID ?? containerId ?? sandboxId,
            t: (key, options) => t(key as any, options as any),
          });
        };
        ws.onclose = () => {
          if (disposed) return;
          setStatus((prev) =>
            prev === 'error' || prev === 'closed' ? prev : 'closed',
          );
        };
        ws.onerror = () => {
          if (disposed) return;
          setStatus('error');
          setError(t('terminal.errors.websocket'));
        };
      } catch (err) {
        if (disposed) return;
        const message = err instanceof Error ? err.message : String(err);
        setStatus('error');
        setError(message);
        term.writeln(`\r\n${t('terminal.errors.openFailed', { message })}`);
      }
    }

    void connect();

    return () => {
      disposed = true;
      resizeObserver.disconnect();
      mountElement.removeEventListener('paste', pasteListener, true);
      dataDisposable.dispose();
      resizeDisposable.dispose();
      if (resizeTimerRef.current !== null) {
        window.clearTimeout(resizeTimerRef.current);
        resizeTimerRef.current = null;
      }
      wsRef.current?.close();
      wsRef.current = null;
      term.dispose();
      termRef.current = null;
      fitRef.current = null;
    };
  }, [mountElement, open, sandboxId, selectedContainerId, sessionKey, t]);

  useEffect(() => {
    const onWindowResize = () => {
      setFrame((value) => clampTerminalFrame(value));
      window.requestAnimationFrame(fit);
    };
    window.addEventListener('resize', onWindowResize);
    return () => window.removeEventListener('resize', onWindowResize);
  }, []);

  useEffect(() => {
    const node = contentRef.current;
    if (!open || maximized || !node) return;

    const observer = new ResizeObserver(() => {
      const rect = node.getBoundingClientRect();
      setFrame((value) => {
        if (
          Math.abs(value.width - rect.width) < 2
          && Math.abs(value.height - rect.height) < 2
        ) {
          return value;
        }
        return clampTerminalFrame({
          ...value,
          width: rect.width,
          height: rect.height,
        });
      });
      window.requestAnimationFrame(fit);
    });
    observer.observe(node);
    return () => observer.disconnect();
  }, [maximized, open]);

  useEffect(() => {
    if (!open) return;
    setMinimized(false);
    window.requestAnimationFrame(() => {
      fit();
      termRef.current?.focus();
    });
  }, [open, restoreKey]);

  const reconnect = () => {
    wsRef.current?.close();
    setSessionKey((value) => value + 1);
  };

  const changeContainer = (containerId: string) => {
    termRef.current?.writeln(`\r\n${t('terminal.container.switching', { id: containerId })}`);
    setStatus('connecting');
    setError(null);
    setSelectedContainerId(containerId);
    wsRef.current?.close();
    setSessionKey((value) => value + 1);
  };

  const fit = () => {
    fitRef.current?.fit();
    const term = termRef.current;
    const ws = wsRef.current;
    if (term && ws?.readyState === WebSocket.OPEN) {
      ws.send(terminalResizeFrame(term.rows, term.cols));
    }
  };

  const copySelection = () => {
    const selection = termRef.current?.getSelection() ?? '';
    if (selection) void navigator.clipboard?.writeText(selection);
    termRef.current?.focus();
  };

  const clearTerminal = () => {
    termRef.current?.clear();
    termRef.current?.focus();
  };

  const toggleMaximized = () => {
    setMinimized(false);
    setMaximized((value) => !value);
    window.requestAnimationFrame(() => {
      window.requestAnimationFrame(fit);
    });
  };

  const minimize = () => {
    setMinimized(true);
  };

  const startDrag = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (maximized || event.button !== 0) return;
    const target = event.target as HTMLElement;
    if (target.closest('button,select,input,a,[role="button"]')) return;

    event.preventDefault();
    const startX = event.clientX;
    const startY = event.clientY;
    const startFrame = frame;

    const move = (moveEvent: PointerEvent) => {
      const next = clampTerminalFrame({
        ...startFrame,
        x: startFrame.x + moveEvent.clientX - startX,
        y: startFrame.y + moveEvent.clientY - startY,
      });
      setFrame(next);
    };
    const stop = () => {
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerup', stop);
      window.removeEventListener('pointercancel', stop);
      window.requestAnimationFrame(fit);
    };

    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', stop);
    window.addEventListener('pointercancel', stop);
  };

  return (
    <Dialog.Root modal={false} open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Content
          ref={contentRef}
          className={cn(
            'fixed z-50 flex min-h-0 min-w-0 max-w-[calc(100vw-1rem)] max-h-[calc(100vh-1rem)] flex-col overflow-hidden rounded-lg border border-border/70 bg-card shadow-2xl',
            minimized && 'pointer-events-none opacity-0',
            maximized
              ? 'inset-2 h-auto w-auto resize-none sm:inset-4'
              : 'resize',
          )}
          style={
            maximized
              ? undefined
              : {
                  left: frame.x,
                  top: frame.y,
                  width: frame.width,
                  height: frame.height,
                }
          }
          onEscapeKeyDown={(event) => event.preventDefault()}
          onPointerDownOutside={(event) => {
            event.preventDefault();
            minimize();
          }}
          onInteractOutside={(event) => event.preventDefault()}
        >
          <div
            className={cn(
              'flex items-center gap-2 border-b border-border/60 px-3 py-2 sm:gap-3 sm:px-4 sm:py-3',
              maximized ? 'cursor-default' : 'cursor-move select-none',
            )}
            onPointerDown={startDrag}
          >
            <div className="flex min-w-0 flex-1 items-center gap-2">
              <Terminal size={16} className="text-primary" />
              <Dialog.Title className="truncate font-mono text-sm font-semibold">
                {t('terminal.title', { id: sandboxId })}
              </Dialog.Title>
              <span
                className={cn(
                  'ml-1 inline-flex h-2 w-2 rounded-full',
                  status === 'connected' && 'bg-cube-ok',
                  status === 'connecting' && 'bg-cube-warn',
                  (status === 'closed' || status === 'idle') && 'bg-cube-mute',
                  status === 'error' && 'bg-cube-err',
                )}
              />
              <span className="text-xs text-muted-foreground">{t(`terminal.status.${status}` as any)}</span>
              <Dialog.Description className="sr-only">{t('terminal.description')}</Dialog.Description>
            </div>
            <div className="flex items-center gap-1">
              {containers.length > 1 ? (
                <select
                  className="h-8 max-w-[120px] rounded-md border border-border/70 bg-background px-2 text-xs sm:max-w-[220px]"
                  value={selectedContainerId ?? ''}
                  title={t('terminal.container.select')}
                  aria-label={t('terminal.container.select')}
                  onChange={(event) => changeContainer(event.target.value)}
                >
                  {containers.map((container) => (
                    <option key={container.containerID} value={container.containerID}>
                      {formatContainerLabel(container)}
                    </option>
                  ))}
                </select>
              ) : null}
              <Button
                size="icon"
                variant="ghost"
                title={t('terminal.actions.fit')}
                aria-label={t('terminal.actions.fit')}
                onClick={fit}
              >
                <ScanLine size={14} />
              </Button>
              <Button
                size="icon"
                variant="ghost"
                title={t('terminal.actions.copySelection')}
                aria-label={t('terminal.actions.copySelection')}
                onClick={copySelection}
                className="hidden sm:inline-flex"
              >
                <ClipboardCopy size={14} />
              </Button>
              <Button
                size="icon"
                variant="ghost"
                title={t('terminal.actions.clear')}
                aria-label={t('terminal.actions.clear')}
                onClick={clearTerminal}
                className="hidden sm:inline-flex"
              >
                <Eraser size={14} />
              </Button>
              <Button
                size="icon"
                variant="ghost"
                title={maximized ? t('terminal.actions.restore') : t('terminal.actions.maximize')}
                aria-label={maximized ? t('terminal.actions.restore') : t('terminal.actions.maximize')}
                onClick={toggleMaximized}
              >
                {maximized ? <Minimize2 size={14} /> : <Maximize2 size={14} />}
              </Button>
              <Button
                size="icon"
                variant="ghost"
                title={t('terminal.actions.reconnect')}
                aria-label={t('terminal.actions.reconnect')}
                onClick={reconnect}
              >
                <RotateCcw size={14} />
              </Button>
              <Dialog.Close asChild>
                <Button
                  size="icon"
                  variant="ghost"
                  title={t('terminal.actions.close')}
                  aria-label={t('terminal.actions.close')}
                >
                  <X size={14} />
                </Button>
              </Dialog.Close>
            </div>
          </div>
          {error || status === 'closed' ? (
            <div className="flex items-center justify-between gap-3 border-b border-cube-err/30 bg-cube-err/10 px-4 py-2 text-xs text-cube-err">
              <span>{error ?? t('terminal.closedMessage')}</span>
              <Button size="sm" variant="outline" onClick={reconnect}>
                <RotateCcw size={13} />
                {t('terminal.actions.reconnect')}
              </Button>
            </div>
          ) : null}
          <div className="min-h-0 flex-1 bg-[#070b12] p-2">
            <div ref={setTerminalMount} className="h-full w-full overflow-hidden rounded-md" />
          </div>
          <div className="flex items-center justify-between gap-3 border-t border-border/60 px-3 py-2 text-xs text-muted-foreground sm:px-4">
            <span className="hidden truncate sm:inline">
              {t('terminal.session', { id: compactId(sessionId) })}
            </span>
            <span className="hidden truncate md:inline">
              {t('terminal.exec', { id: compactId(execId) })}
            </span>
            <span className="truncate">
              {selectedContainerId
                ? t('terminal.container.current', { id: selectedContainerId })
                : t('terminal.scrollback', { lines: 5000 })}
            </span>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function terminalContainers(containers?: SandboxContainer[] | null): SandboxContainer[] {
  return (containers ?? [])
    .filter((container) => container.state === 'running')
    .map((container) => ({
      ...container,
      containerID: container.containerID || container.name || '',
    }))
    .filter((container) => container.containerID);
}

function selectContainer(
  containers: SandboxContainer[],
  selectedContainerId?: string,
): string | undefined {
  if (containers.length === 0) return undefined;
  if (selectedContainerId && containers.some((container) => container.containerID === selectedContainerId)) {
    return selectedContainerId;
  }
  const sandboxContainer = containers.find((container) => container.kind === 'sandbox');
  return (sandboxContainer ?? containers[0]).containerID;
}

function formatContainerLabel(container: SandboxContainer): string {
  const name = container.name && container.name !== container.containerID ? `${container.name} · ` : '';
  const kind = container.kind ? ` · ${container.kind}` : '';
  return `${name}${container.containerID}${kind}`;
}

function initialTerminalFrame(): TerminalFrame {
  if (typeof window === 'undefined') {
    return { x: 120, y: 80, width: 1120, height: 760 };
  }
  const width = Math.min(1120, Math.max(280, window.innerWidth - TERMINAL_MARGIN * 2));
  const height = Math.min(760, Math.max(240, window.innerHeight - TERMINAL_MARGIN * 2));
  return {
    x: Math.max(TERMINAL_MARGIN, Math.round((window.innerWidth - width) / 2)),
    y: Math.max(TERMINAL_MARGIN, Math.round((window.innerHeight - height) / 2)),
    width,
    height,
  };
}

function clampTerminalFrame(frame: TerminalFrame): TerminalFrame {
  if (typeof window === 'undefined') return frame;
  const maxWidth = Math.max(280, window.innerWidth - TERMINAL_MARGIN * 2);
  const maxHeight = Math.max(240, window.innerHeight - TERMINAL_MARGIN * 2);
  const minWidth = Math.min(MIN_TERMINAL_WIDTH, maxWidth);
  const minHeight = Math.min(MIN_TERMINAL_HEIGHT, maxHeight);
  const width = Math.min(Math.max(frame.width, minWidth), maxWidth);
  const height = Math.min(Math.max(frame.height, minHeight), maxHeight);
  return {
    x: Math.min(
      Math.max(frame.x, TERMINAL_MARGIN),
      Math.max(TERMINAL_MARGIN, window.innerWidth - width - TERMINAL_MARGIN),
    ),
    y: Math.min(
      Math.max(frame.y, TERMINAL_MARGIN),
      Math.max(TERMINAL_MARGIN, window.innerHeight - height - TERMINAL_MARGIN),
    ),
    width,
    height,
  };
}

function handleServerMessage(
  term: XTerm,
  raw: string,
  handlers: {
    setStatus: (status: ConnectionState) => void;
    setError: (message: string | null) => void;
    setSessionId: (sessionId: string | null) => void;
    setExecId: (execId: string | null) => void;
    containerId: string;
    t: (key: string, options?: Record<string, unknown>) => string;
  },
) {
  const { setStatus, setError, setSessionId, setExecId, containerId, t } = handlers;
  let msg: TerminalServerMessage;
  try {
    msg = parseTerminalServerMessage(raw);
  } catch {
    setStatus('error');
    setError(t('terminal.errors.invalidMessage'));
    return;
  }
  switch (msg.type) {
    case 'start':
      setStatus('connected');
      setError(null);
      setSessionId(typeof msg.sessionId === 'string' ? msg.sessionId : null);
      setExecId(typeof msg.execId === 'string' ? msg.execId : null);
      term.writeln(t('terminal.started', {
        container: containerId,
        execId: compactId(typeof msg.execId === 'string' ? msg.execId : null),
      }));
      break;
    case 'output':
      if (typeof msg.data === 'string') term.write(decodeBase64Bytes(msg.data));
      break;
    case 'exit':
      setStatus('closed');
      if (typeof msg.error === 'string' && msg.error) {
        setError(msg.error);
        term.writeln(`\r\n${t('terminal.exitedWithError', { message: msg.error })}`);
      } else {
        term.writeln(
          `\r\n${t('terminal.exited', { code: typeof msg.exitCode === 'number' ? msg.exitCode : 0 })}`,
        );
      }
      break;
    case 'error':
      setStatus('error');
      {
        const message =
          typeof msg.message === 'string' ? msg.message : t('terminal.errors.websocket');
        setError(message);
        term.writeln(`\r\n${message}`);
      }
      break;
    case 'idleTimeout':
      setStatus('closed');
      {
        const seconds =
          typeof msg.idleTimeoutSeconds === 'number' ? msg.idleTimeoutSeconds : '-';
        const message = t('terminal.idleTimeout', { seconds });
        setError(message);
        term.writeln(`\r\n${message}`);
      }
      break;
    case 'streamEnd':
      setStatus('closed');
      break;
    default:
      break;
  }
}

function compactId(value: string | null): string {
  if (!value) return '-';
  return value.length > 16 ? `${value.slice(0, 8)}…${value.slice(-6)}` : value;
}
