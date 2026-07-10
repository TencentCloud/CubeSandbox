// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useCallback, useEffect, useRef, useState } from 'react';
import * as Dialog from '@radix-ui/react-dialog';
import { Terminal as XTerm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import {
  Clipboard,
  ClipboardPaste,
  Expand,
  Minus,
  Plus,
  RefreshCw,
  Shrink,
  Square,
  Unplug,
  X,
} from 'lucide-react';
import { useTranslation } from 'react-i18next';

import { sandboxApi, type SandboxContainer } from '@/api/client';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import {
  hasTerminalContainerSelector,
  selectTerminalContainer,
  terminalResizeMessage,
} from '@/lib/terminal';
import '@xterm/xterm/css/xterm.css';

type ConnectionState = 'connecting' | 'connected' | 'disconnected' | 'error';

interface SandboxTerminalDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  sandboxID: string;
  containers?: SandboxContainer[] | null;
}

export function SandboxTerminalDialog({
  open,
  onOpenChange,
  sandboxID,
  containers,
}: SandboxTerminalDialogProps) {
  const { t } = useTranslation('terminal');
  const [terminalHost, setTerminalHost] = useState<HTMLDivElement | null>(null);
  const terminalRef = useRef<XTerm | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const socketRef = useRef<WebSocket | null>(null);
  const manualCloseRef = useRef(false);
  const connectingRef = useRef(false);
  const connectAttemptRef = useRef(0);
  const [availableContainers, setAvailableContainers] = useState<SandboxContainer[]>(containers ?? []);
  const [selectedContainer, setSelectedContainer] = useState(
    selectTerminalContainer(containers ?? [], sandboxID)?.containerID ?? '',
  );
  const [connectionState, setConnectionState] = useState<ConnectionState>('disconnected');
  const [fontSize, setFontSize] = useState(13);
  const [fullscreen, setFullscreen] = useState(false);
  const [terminalReady, setTerminalReady] = useState(false);

  const writeNotice = useCallback((message: string) => {
    terminalRef.current?.writeln(`\r\n\x1b[90m${message}\x1b[0m`);
  }, []);

  const sendResize = useCallback(() => {
    const terminal = terminalRef.current;
    const fit = fitRef.current;
    if (!terminal || !fit) return;
    try {
      fit.fit();
    } catch {
      return;
    }
    const socket = socketRef.current;
    if (socket?.readyState === WebSocket.OPEN) {
      socket.send(terminalResizeMessage(terminal.cols, terminal.rows));
    }
  }, []);

  const disconnect = useCallback((quiet = false) => {
    connectAttemptRef.current += 1;
    const socket = socketRef.current;
    manualCloseRef.current = true;
    if (socket?.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify({ type: 'close' }));
    }
    socket?.close(1000, 'client disconnect');
    socketRef.current = null;
    connectingRef.current = false;
    setConnectionState('disconnected');
    if (!quiet) writeNotice(t('messages.disconnected'));
  }, [t, writeNotice]);

  const connect = useCallback(async (containerOverride?: string) => {
    const terminal = terminalRef.current;
    if (!terminal || connectingRef.current || socketRef.current?.readyState === WebSocket.OPEN) return;
    const attempt = ++connectAttemptRef.current;
    connectingRef.current = true;
    manualCloseRef.current = false;
    setConnectionState('connecting');
    writeNotice(t('messages.connecting'));

    try {
      let targetContainers = availableContainers;
      if (targetContainers.length === 0) {
        const detail = await sandboxApi.get(sandboxID);
        if (attempt !== connectAttemptRef.current || manualCloseRef.current) return;
        targetContainers = detail.containers ?? [];
        setAvailableContainers(targetContainers);
      }
      if (attempt !== connectAttemptRef.current || manualCloseRef.current) return;
      const containerID = selectTerminalContainer(
        targetContainers,
        sandboxID,
        containerOverride || selectedContainer || undefined,
      )?.containerID;
      if (!containerID) throw new Error(t('messages.noContainers'));
      setSelectedContainer(containerID);

      const session = await sandboxApi.createTerminalSession(sandboxID, {
        containerID,
        cols: terminal.cols,
        rows: terminal.rows,
      });
      if (attempt !== connectAttemptRef.current || manualCloseRef.current) return;
      const url = new URL(session.websocketURL, window.location.origin);
      url.protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
      const socket = new WebSocket(url);
      let expectedClose = false;
      socket.binaryType = 'arraybuffer';
      socketRef.current = socket;

      socket.onopen = () => {
        if (socketRef.current !== socket) return;
        connectingRef.current = false;
        sendResize();
      };
      socket.onmessage = (event) => {
        if (socketRef.current !== socket) return;
        if (typeof event.data !== 'string') {
          const bytes = event.data instanceof ArrayBuffer ? new Uint8Array(event.data) : event.data;
          if (bytes instanceof Blob) {
            void bytes.arrayBuffer().then((buffer) => terminal.write(new Uint8Array(buffer)));
          } else {
            terminal.write(bytes);
          }
          return;
        }
        try {
          const control = JSON.parse(event.data) as {
            type?: string;
            message?: string;
            code?: number;
          };
          if (control.type === 'ready') {
            setConnectionState('connected');
            terminal.focus();
          } else if (control.type === 'exit') {
            expectedClose = true;
            writeNotice(t('messages.exited', { code: control.code ?? 0 }));
            setConnectionState('disconnected');
          } else if (control.type === 'error') {
            expectedClose = true;
            writeNotice(t('messages.error', { message: control.message ?? t('messages.unknownError') }));
            setConnectionState('error');
          } else if (control.type === 'close') {
            expectedClose = true;
            writeNotice(control.message ?? t('messages.disconnected'));
            setConnectionState('disconnected');
          }
        } catch {
          writeNotice(t('messages.invalidControl'));
        }
      };
      socket.onerror = () => {
        if (socketRef.current === socket) setConnectionState('error');
      };
      socket.onclose = () => {
        if (socketRef.current !== socket) return;
        connectingRef.current = false;
        socketRef.current = null;
        setConnectionState((current) => current === 'error' ? current : 'disconnected');
        if (!manualCloseRef.current && !expectedClose) writeNotice(t('messages.unexpectedDisconnect'));
      };
    } catch (error) {
      if (attempt !== connectAttemptRef.current) return;
      connectingRef.current = false;
      setConnectionState('error');
      writeNotice(t('messages.error', {
        message: error instanceof Error ? error.message : t('messages.unknownError'),
      }));
    }
  }, [availableContainers, sandboxID, selectedContainer, sendResize, t, writeNotice]);

  useEffect(() => {
    if (!open || !terminalHost) return;
    setAvailableContainers(containers ?? []);
    setSelectedContainer(selectTerminalContainer(containers ?? [], sandboxID)?.containerID ?? '');
    setConnectionState('disconnected');
    setFullscreen(false);
    const fitAddon = new FitAddon();
    const terminal = new XTerm({
      cursorBlink: true,
      cursorStyle: 'block',
      fontFamily: '"JetBrains Mono Variable", ui-monospace, SFMono-Regular, monospace',
      fontSize,
      scrollback: 5000,
      convertEol: false,
      allowProposedApi: false,
      theme: {
        background: '#101214',
        foreground: '#e6e8eb',
        cursor: '#f3f4f6',
        selectionBackground: '#4b556380',
        black: '#17191c',
        red: '#ef6b73',
        green: '#75c58e',
        yellow: '#ddb96a',
        blue: '#77a8dc',
        magenta: '#c394d8',
        cyan: '#69bdc1',
        white: '#d6d8dc',
        brightBlack: '#686d76',
      },
    });
    terminal.loadAddon(fitAddon);
    terminal.open(terminalHost);
    terminalRef.current = terminal;
    fitRef.current = fitAddon;
    const inputDisposable = terminal.onData((data) => {
      const socket = socketRef.current;
      if (socket?.readyState === WebSocket.OPEN) socket.send(new TextEncoder().encode(data));
    });
    const binaryDisposable = terminal.onBinary((data) => {
      const bytes = Uint8Array.from(data, (character) => character.charCodeAt(0));
      const socket = socketRef.current;
      if (socket?.readyState === WebSocket.OPEN) socket.send(bytes);
    });
    const frame = requestAnimationFrame(() => {
      sendResize();
      setTerminalReady(true);
    });
    const resizeObserver = new ResizeObserver(() => requestAnimationFrame(sendResize));
    resizeObserver.observe(terminalHost);

    return () => {
      cancelAnimationFrame(frame);
      resizeObserver.disconnect();
      inputDisposable.dispose();
      binaryDisposable.dispose();
      connectAttemptRef.current += 1;
      manualCloseRef.current = true;
      socketRef.current?.close(1000, 'dialog closed');
      socketRef.current = null;
      terminal.dispose();
      terminalRef.current = null;
      fitRef.current = null;
      connectingRef.current = false;
      setTerminalReady(false);
    };
    // Terminal construction is intentionally scoped to one dialog opening.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, terminalHost]);

  useEffect(() => {
    if (open && terminalReady) void connect();
  }, [connect, open, terminalReady]);

  useEffect(() => {
    if (terminalRef.current) {
      terminalRef.current.options.fontSize = fontSize;
      requestAnimationFrame(sendResize);
    }
  }, [fontSize, sendResize]);

  const switchContainer = (containerID: string) => {
    setSelectedContainer(containerID);
    disconnect(true);
    writeNotice(t('messages.switchingContainer'));
    window.setTimeout(() => void connect(containerID), 0);
  };

  const copySelection = async () => {
    const selection = terminalRef.current?.getSelection();
    if (selection) await navigator.clipboard.writeText(selection);
  };

  const pasteClipboard = async () => {
    terminalRef.current?.paste(await navigator.clipboard.readText());
    terminalRef.current?.focus();
  };

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/60 backdrop-blur-[1px]" />
        <Dialog.Content
          className={cn(
            'fixed z-50 flex flex-col overflow-hidden border border-border bg-background shadow-2xl focus:outline-none',
            fullscreen
              ? 'inset-0'
              : 'left-1/2 top-1/2 h-[min(78vh,760px)] w-[min(94vw,1120px)] -translate-x-1/2 -translate-y-1/2 rounded-md',
          )}
        >
          <div className="flex min-h-12 flex-wrap items-center gap-2 border-b border-border bg-muted/40 px-3 py-2">
            <Dialog.Title className="min-w-0 flex-1 truncate font-mono text-sm font-medium">
              {t('title', { sandboxID })}
            </Dialog.Title>
            <Dialog.Description className="sr-only">{t('description')}</Dialog.Description>
            {hasTerminalContainerSelector(availableContainers) && (
              <label className="flex items-center gap-2 text-xs text-muted-foreground">
                <span>{t('container')}</span>
                <select
                  value={selectedContainer}
                  onChange={(event) => switchContainer(event.target.value)}
                  className="h-8 max-w-64 rounded-md border border-border bg-background px-2 font-mono text-xs text-foreground"
                >
                  {availableContainers.map((container) => (
                    <option key={container.containerID} value={container.containerID}>
                      {container.name || container.containerID}
                    </option>
                  ))}
                </select>
              </label>
            )}
            <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <span className={cn(
                'h-2 w-2 rounded-full',
                connectionState === 'connected' && 'bg-cube-ok',
                connectionState === 'connecting' && 'animate-pulse bg-cube-warn',
                connectionState === 'error' && 'bg-cube-err',
                connectionState === 'disconnected' && 'bg-muted-foreground/50',
              )} />
              {t(`status.${connectionState}`)}
            </span>
            <div className="flex items-center gap-0.5">
              <Button size="icon" variant="ghost" title={t('tools.copy')} aria-label={t('tools.copy')} onClick={() => void copySelection()}>
                <Clipboard size={14} />
              </Button>
              <Button size="icon" variant="ghost" title={t('tools.paste')} aria-label={t('tools.paste')} onClick={() => void pasteClipboard()}>
                <ClipboardPaste size={14} />
              </Button>
              <Button size="icon" variant="ghost" title={t('tools.fontSmaller')} aria-label={t('tools.fontSmaller')} onClick={() => setFontSize((size) => Math.max(10, size - 1))}>
                <Minus size={14} />
              </Button>
              <Button size="icon" variant="ghost" title={t('tools.fontLarger')} aria-label={t('tools.fontLarger')} onClick={() => setFontSize((size) => Math.min(20, size + 1))}>
                <Plus size={14} />
              </Button>
              <Button size="icon" variant="ghost" title={fullscreen ? t('tools.restore') : t('tools.fullscreen')} aria-label={fullscreen ? t('tools.restore') : t('tools.fullscreen')} onClick={() => setFullscreen((value) => !value)}>
                {fullscreen ? <Shrink size={14} /> : <Expand size={14} />}
              </Button>
              <Button size="icon" variant="ghost" title={t('tools.reconnect')} aria-label={t('tools.reconnect')} disabled={connectionState === 'connecting'} onClick={() => { disconnect(true); window.setTimeout(() => void connect(), 0); }}>
                <RefreshCw size={14} />
              </Button>
              <Button size="icon" variant="ghost" title={t('tools.disconnect')} aria-label={t('tools.disconnect')} disabled={connectionState === 'disconnected'} onClick={() => disconnect()}>
                <Unplug size={14} />
              </Button>
            </div>
            <Dialog.Close asChild>
              <Button size="icon" variant="ghost" title={t('tools.close')} aria-label={t('tools.close')}>
                <X size={15} />
              </Button>
            </Dialog.Close>
          </div>
          <div className="relative min-h-0 flex-1 bg-[#101214] p-2">
            <div ref={setTerminalHost} className="h-full w-full" />
            {connectionState === 'connecting' && (
              <div className="pointer-events-none absolute right-4 top-3 text-xs text-neutral-500">
                <Square className="mr-1 inline animate-pulse" size={8} fill="currentColor" />
                {t('status.connecting')}
              </div>
            )}
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
