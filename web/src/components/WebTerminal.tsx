// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import '@xterm/xterm/css/xterm.css';

import { useCallback, useEffect, useRef, useState } from 'react';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal } from '@xterm/xterm';
import { ClipboardPaste, Eraser, Plug, RotateCw, Unplug } from 'lucide-react';
import { useTranslation } from 'react-i18next';

import { sandboxApi } from '@/api/client';
import { Button } from '@/components/ui/button';
import {
  buildTerminalWebSocketUrl,
  decodeServerControl,
  encodeClose,
  encodeKeepalive,
  encodeResize,
  terminalSubprotocols,
} from '@/lib/terminalProtocol';

type ConnectionState = 'idle' | 'authorizing' | 'connecting' | 'connected' | 'exited' | 'error';

interface WebTerminalProps {
  sandboxID: string;
  enabled: boolean;
  containerID?: string;
}

export function WebTerminal({ sandboxID, enabled, containerID }: WebTerminalProps) {
  const { t } = useTranslation('sandboxDetail');
  const mountRef = useRef<HTMLDivElement>(null);
  const terminalRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const socketRef = useRef<WebSocket | null>(null);
  const readyRef = useRef(false);
  const stateRef = useRef<ConnectionState>('idle');
  const generationRef = useRef(0);
  const keepaliveRef = useRef<number | null>(null);
  const resizeTimerRef = useRef<number | null>(null);
  const pendingInputRef = useRef<Uint8Array[]>([]);
  const [state, setStateValue] = useState<ConnectionState>('idle');
  const [message, setMessage] = useState<string | null>(null);

  const setState = useCallback((next: ConnectionState) => {
    stateRef.current = next;
    setStateValue(next);
  }, []);

  const stopKeepalive = useCallback(() => {
    if (keepaliveRef.current !== null) {
      window.clearInterval(keepaliveRef.current);
      keepaliveRef.current = null;
    }
  }, []);

  const closeSocket = useCallback(
    (sendControl: boolean) => {
      generationRef.current += 1;
      stopKeepalive();
      readyRef.current = false;
      pendingInputRef.current = [];
      const socket = socketRef.current;
      socketRef.current = null;
      if (socket && socket.readyState < WebSocket.CLOSING) {
        if (sendControl && socket.readyState === WebSocket.OPEN) socket.send(encodeClose());
        socket.close(1000, 'terminal closed');
      }
    },
    [stopKeepalive],
  );

  useEffect(() => {
    const mount = mountRef.current;
    if (!mount) return;
    const terminal = new Terminal({
      allowTransparency: true,
      cursorBlink: true,
      fontFamily: '"JetBrains Mono Variable", ui-monospace, monospace',
      fontSize: 13,
      lineHeight: 1.25,
      scrollback: 5_000,
      theme: {
        background: '#00000000',
        foreground: '#dbeafe',
        cursor: '#60a5fa',
        selectionBackground: '#33415599',
      },
    });
    const fit = new FitAddon();
    terminal.loadAddon(fit);
    terminal.open(mount);
    fit.fit();
    terminalRef.current = terminal;
    fitRef.current = fit;

    const dataDisposable = terminal.onData((data) => {
      const bytes = new TextEncoder().encode(data);
      const socket = socketRef.current;
      if (!socket || socket.readyState !== WebSocket.OPEN) return;
      if (!readyRef.current) {
        pendingInputRef.current.push(bytes);
        return;
      }
      socket.send(bytes);
    });
    const resizeDisposable = terminal.onResize(({ cols, rows }) => {
      if (resizeTimerRef.current !== null) window.clearTimeout(resizeTimerRef.current);
      resizeTimerRef.current = window.setTimeout(() => {
        const socket = socketRef.current;
        if (readyRef.current && socket?.readyState === WebSocket.OPEN) {
          socket.send(encodeResize(cols, rows));
        }
      }, 100);
    });
    const observer = new ResizeObserver(() => fit.fit());
    observer.observe(mount);

    return () => {
      closeSocket(true);
      if (resizeTimerRef.current !== null) window.clearTimeout(resizeTimerRef.current);
      observer.disconnect();
      dataDisposable.dispose();
      resizeDisposable.dispose();
      terminal.dispose();
      terminalRef.current = null;
      fitRef.current = null;
    };
  }, [closeSocket]);

  useEffect(() => {
    if (!enabled && stateRef.current !== 'idle') {
      closeSocket(true);
      setState('idle');
      setMessage(t('terminal.paused'));
    }
  }, [closeSocket, enabled, setState, t]);

  const connect = useCallback(async () => {
    if (!enabled || stateRef.current === 'authorizing' || stateRef.current === 'connecting') return;
    closeSocket(false);
    const generation = generationRef.current;
    const terminal = terminalRef.current;
    const fit = fitRef.current;
    if (!terminal || !fit) return;
    fit.fit();
    terminal.focus();
    setMessage(null);
    setState('authorizing');

    try {
      const grant = await sandboxApi.createTerminalSession(
        sandboxID,
        Math.max(terminal.cols, 2),
        Math.max(terminal.rows, 1),
        containerID,
      );
      if (generation !== generationRef.current) return;
      setState('connecting');
      const socket = new WebSocket(
        buildTerminalWebSocketUrl(sandboxID),
        terminalSubprotocols(grant.grant),
      );
      socket.binaryType = 'arraybuffer';
      socketRef.current = socket;

      socket.onopen = () => {
        if (generation !== generationRef.current) return;
        keepaliveRef.current = window.setInterval(() => {
          if (socket.readyState === WebSocket.OPEN) socket.send(encodeKeepalive());
        }, 20_000);
      };
      socket.onmessage = (event) => {
        if (generation !== generationRef.current) return;
        if (typeof event.data !== 'string') {
          terminal.write(new Uint8Array(event.data as ArrayBuffer));
          return;
        }
        try {
          const control = decodeServerControl(event.data);
          if (control.type === 'ready') {
            readyRef.current = true;
            setState('connected');
            for (const bytes of pendingInputRef.current) socket.send(bytes);
            pendingInputRef.current = [];
            terminal.focus();
          } else if (control.type === 'exit') {
            readyRef.current = false;
            setState('exited');
            terminal.writeln(
              `\r\n\x1b[90m[${t('terminal.exit', { code: control.exitCode ?? 0 })}]\x1b[0m`,
            );
          } else {
            readyRef.current = false;
            setState('error');
            const text = control.message || t('terminal.connectionFailed');
            setMessage(text);
            terminal.writeln(`\r\n\x1b[31m[${text}]\x1b[0m`);
          }
        } catch {
          setState('error');
          setMessage(t('terminal.protocolError'));
          socket.close(1002, 'invalid terminal control');
        }
      };
      socket.onerror = () => {
        if (generation !== generationRef.current) return;
        setState('error');
        setMessage(t('terminal.connectionFailed'));
      };
      socket.onclose = () => {
        if (generation !== generationRef.current) return;
        stopKeepalive();
        readyRef.current = false;
        socketRef.current = null;
        if (stateRef.current === 'connecting' || stateRef.current === 'connected') {
          setState('error');
          setMessage(t('terminal.disconnected'));
        }
      };
    } catch (error) {
      if (generation !== generationRef.current) return;
      setState('error');
      setMessage(error instanceof Error ? error.message : t('terminal.connectionFailed'));
    }
  }, [closeSocket, containerID, enabled, sandboxID, setState, stopKeepalive, t]);

  const disconnect = useCallback(() => {
    closeSocket(true);
    setState('idle');
    setMessage(null);
  }, [closeSocket, setState]);

  const canConnect = enabled && !['authorizing', 'connecting', 'connected'].includes(state);

  return (
    <div className="overflow-hidden rounded-lg border border-border/80 bg-[#060b14] shadow-inner">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-white/10 bg-white/[0.03] px-3 py-2">
        <div className="flex min-w-0 items-center gap-2 text-xs">
          <span
            className={`h-2 w-2 rounded-full ${
              state === 'connected'
                ? 'bg-cube-ok'
                : state === 'error'
                  ? 'bg-cube-err'
                  : state === 'authorizing' || state === 'connecting'
                    ? 'animate-pulse bg-cube-warn'
                    : 'bg-cube-mute'
            }`}
          />
          <span className="font-mono text-slate-300">{t(`terminal.states.${state}`)}</span>
          {message ? <span className="truncate text-slate-500">— {message}</span> : null}
        </div>
        <div className="flex items-center gap-1">
          <Button
            size="sm"
            variant="ghost"
            className="h-7 text-slate-300 hover:bg-white/10 hover:text-white"
            onClick={() => {
              void navigator.clipboard
                .readText()
                .then((text) => terminalRef.current?.paste(text))
                .catch(() => setMessage(t('terminal.clipboardDenied')));
            }}
          >
            <ClipboardPaste size={13} /> {t('terminal.paste')}
          </Button>
          <Button
            size="sm"
            variant="ghost"
            className="h-7 text-slate-300 hover:bg-white/10 hover:text-white"
            onClick={() => terminalRef.current?.clear()}
          >
            <Eraser size={13} /> {t('terminal.clear')}
          </Button>
          {state === 'connected' ? (
            <Button
              size="sm"
              variant="ghost"
              className="h-7 text-slate-300 hover:bg-white/10 hover:text-white"
              onClick={disconnect}
            >
              <Unplug size={13} /> {t('terminal.disconnect')}
            </Button>
          ) : (
            <Button
              size="sm"
              variant="ghost"
              className="h-7 text-slate-200 hover:bg-white/10 hover:text-white"
              disabled={!canConnect}
              onClick={() => void connect()}
            >
              {state === 'exited' || state === 'error' ? <RotateCw size={13} /> : <Plug size={13} />}
              {state === 'exited' || state === 'error'
                ? t('terminal.reconnect')
                : t('terminal.connect')}
            </Button>
          )}
        </div>
      </div>
      <div ref={mountRef} className="h-[420px] p-3" aria-label={t('terminal.title')} />
      {!enabled ? (
        <div className="border-t border-white/10 px-3 py-2 text-xs text-slate-500">
          {t('terminal.paused')}
        </div>
      ) : null}
    </div>
  );
}
