// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useCallback, useEffect, useRef } from 'react';
import type { Terminal } from '@xterm/xterm';
import type { FitAddon } from '@xterm/addon-fit';

interface UseTerminalResizeOptions {
  terminal: Terminal | null;
  containerRef: React.RefObject<HTMLDivElement | null>;
  /** Optional FitAddon instance for precise character measurement. */
  fitAddon: FitAddon | null;
  onResize: (cols: number, rows: number) => void;
  enabled?: boolean;
}

/**
 * Observes the terminal container element and automatically fits the
 * xterm.js instance to the available space. Sends resize events to the
 * backend via the `onResize` callback.
 */
export function useTerminalResize({
  terminal,
  containerRef,
  fitAddon,
  onResize,
  enabled = true,
}: UseTerminalResizeOptions) {
  const onResizeRef = useRef(onResize);
  onResizeRef.current = onResize;

  const fitTerminal = useCallback(() => {
    if (!terminal || !terminal.element) return;

    try {
      if (fitAddon) {
        // Use FitAddon for precise column/row calculation.
        fitAddon.fit();
        onResizeRef.current(terminal.cols, terminal.rows);
      } else {
        // Fallback: approximate from container dimensions.
        const container = containerRef.current;
        if (!container) return;

        const parent = container.parentElement;
        if (!parent) return;

        const charWidth = 9.6;
        const charHeight = 18;
        const paddingX = 16;
        const paddingY = 8;
        const rect = parent.getBoundingClientRect();
        const cols = Math.max(20, Math.floor((rect.width - paddingX) / charWidth));
        const rows = Math.max(5, Math.floor((rect.height - paddingY) / charHeight));

        terminal.resize(cols, rows);
        onResizeRef.current(cols, rows);
      }
    } catch (e) {
      console.warn('Failed to fit terminal:', e);
    }
  }, [terminal, containerRef, fitAddon]);

  useEffect(() => {
    if (!enabled) return;

    let debounceTimer: ReturnType<typeof setTimeout> | null = null;

    const observer = new ResizeObserver(() => {
      // Debounce: only send the final resize after drag/resize ends.
      if (debounceTimer) clearTimeout(debounceTimer);
      debounceTimer = setTimeout(() => {
        fitTerminal();
      }, 150);
    });

    const container = containerRef.current;
    if (container) {
      observer.observe(container.parentElement ?? container);
    }

    return () => {
      if (debounceTimer) clearTimeout(debounceTimer);
      observer.disconnect();
    };
  }, [enabled, fitTerminal, containerRef]);

  return { fitTerminal };
}