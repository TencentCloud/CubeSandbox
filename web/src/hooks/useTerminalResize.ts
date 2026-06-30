// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useCallback, useEffect, useRef } from 'react';
import type { Terminal } from '@xterm/xterm';

interface UseTerminalResizeOptions {
  terminal: Terminal | null;
  containerRef: React.RefObject<HTMLDivElement | null>;
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
  onResize,
  enabled = true,
}: UseTerminalResizeOptions) {
  const onResizeRef = useRef(onResize);
  onResizeRef.current = onResize;

  const fitTerminal = useCallback(() => {
    if (!terminal || !terminal.element) return;
    // xterm.js addon-fit handles column/row calculation
    // We use a simple approach: measure the container and trigger resize
    const container = containerRef.current;
    if (!container) return;

    const parent = container.parentElement;
    if (!parent) return;

    // Approximate: each character is ~9.6px wide, ~18px tall at default 14px font
    // In practice, xterm.js addon-fit does this precisely
    const rect = parent.getBoundingClientRect();
    const charWidth = 9.6;
    const charHeight = 18;
    const paddingX = 16;
    const paddingY = 8;
    const cols = Math.max(20, Math.floor((rect.width - paddingX) / charWidth));
    const rows = Math.max(5, Math.floor((rect.height - paddingY) / charHeight));

    try {
      terminal.resize(cols, rows);
      onResizeRef.current(cols, rows);
    } catch {
      // Terminal might not be ready yet
    }
  }, [terminal, containerRef]);

  useEffect(() => {
    if (!enabled) return;

    const observer = new ResizeObserver(() => {
      fitTerminal();
    });

    const container = containerRef.current;
    if (container) {
      observer.observe(container.parentElement ?? container);
    }

    return () => observer.disconnect();
  }, [enabled, fitTerminal, containerRef]);

  return { fitTerminal };
}