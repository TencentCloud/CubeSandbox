// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useCallback, useEffect, useRef, useState } from 'react';
import {
  TerminalSessionClient,
  type TerminalOpenedEvent,
  type TerminalSessionClientOptions,
  type TerminalSessionSnapshot,
} from '@/lib/terminal/client';

const initialSnapshot: TerminalSessionSnapshot = {
  state: { kind: 'connecting' },
  metadata: null,
  lastOffset: 0,
};

interface UseTerminalSessionOptions {
  sandboxId: string;
  containerId?: string;
  onOutput(data: string): void;
  onOpened?(event: TerminalOpenedEvent): void;
  requester?: TerminalSessionClientOptions['requester'];
}

export function useTerminalSession(options: UseTerminalSessionOptions) {
  const [snapshot, setSnapshot] = useState<TerminalSessionSnapshot>(initialSnapshot);
  const clientRef = useRef<TerminalSessionClient | null>(null);
  const outputRef = useRef(options.onOutput);
  const openedRef = useRef(options.onOpened);
  outputRef.current = options.onOutput;
  openedRef.current = options.onOpened;

  const start = useCallback(
    (cols: number, rows: number) => {
      if (clientRef.current) return;
      const client = new TerminalSessionClient({
        sandboxId: options.sandboxId,
        containerId: options.containerId,
        cols,
        rows,
        requester: options.requester,
        onSnapshot: setSnapshot,
        onOutput: (data) => outputRef.current(data),
        onOpened: (event) => openedRef.current?.(event),
      });
      clientRef.current = client;
      client.start();
    },
    [options.containerId, options.requester, options.sandboxId],
  );

  const sendInput = useCallback((data: string) => clientRef.current?.sendInput(data) ?? false, []);
  const resize = useCallback((cols: number, rows: number) => {
    clientRef.current?.resize(cols, rows);
  }, []);
  const dispose = useCallback(() => {
    clientRef.current?.dispose();
    clientRef.current = null;
  }, []);

  useEffect(() => dispose, [dispose]);

  return { snapshot, start, sendInput, resize, dispose };
}
