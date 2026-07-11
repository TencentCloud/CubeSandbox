// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useRef, useEffect, useState, useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import * as DropdownMenu from '@radix-ui/react-dropdown-menu';
import { CardTitle, CardDescription } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { X, RefreshCw, Maximize2, Minimize2, ZoomIn, ZoomOut, ChevronDown, Check } from 'lucide-react';
import { cn } from '@/lib/utils';
import { useTerminal, getTerminalTheme } from '@/hooks/useTerminal';
import { useThemeStore, resolveEffective } from '@/store/theme';
import { closeReasonMessage } from '@/lib/terminalSocket';
import { sandboxApi } from '@/api/client';
import type { SandboxDetail } from '@/api/client';
import '@/styles/xterm.css';

interface SandboxTerminalProps {
  sandboxID: string;
  onClose: () => void;
}

type Container = NonNullable<SandboxDetail['containers']>[number];

function defaultContainerID(containers?: Container[]): string | undefined {
  if (!containers || containers.length === 0) return undefined;
  const running = containers.find((c) => c.state === 'running');
  return running?.containerID;
}

export default function SandboxTerminal({ sandboxID, onClose }: SandboxTerminalProps) {
  const { t } = useTranslation('sandboxDetail');
  const containerRef = useRef<HTMLDivElement>(null);
  const reconnectTimerRef = useRef<number | null>(null);
  const mode = useThemeStore((s) => s.mode);
  const effective = resolveEffective(mode);

  const { data: detail, isLoading } = useQuery({
    queryKey: ['sandbox', sandboxID],
    queryFn: () => sandboxApi.get(sandboxID),
    enabled: !!sandboxID,
    refetchInterval: 5_000,
  });

  const containers = useMemo(() => detail?.containers ?? [], [detail?.containers]);
  const [selectedContainerID, setSelectedContainerID] = useState<string | undefined>(() =>
    defaultContainerID(containers),
  );

  useEffect(() => {
    if (containers.length === 0) {
      if (selectedContainerID) {
        setSelectedContainerID(undefined);
      }
      return;
    }

    const selectedStillRunning = containers.some(
      (container) =>
        container.containerID === selectedContainerID && container.state === 'running',
    );
    if (!selectedStillRunning) {
      setSelectedContainerID(defaultContainerID(containers));
    }
  }, [containers, selectedContainerID]);

  const selectedContainer = useMemo(
    () => containers.find((c) => c.containerID === selectedContainerID),
    [containers, selectedContainerID],
  );
  const hasRunningContainer = containers.some((container) => container.state === 'running');
  const noRunningContainer = !isLoading && containers.length > 0 && !hasRunningContainer;
  const terminalEnabled = !isLoading && (containers.length === 0 || !!selectedContainerID);

  const {
    status,
    fit,
    reconnect,
    fontSize,
    increaseFontSize,
    decreaseFontSize,
    isFullscreen,
    toggleFullscreen,
    disconnectReason,
  } = useTerminal(containerRef, {
    sandboxID,
    containerID: selectedContainerID,
    enabled: terminalEnabled,
  });

  // Close on Escape; F11 or Ctrl/Cmd+Shift+F toggles fullscreen.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onClose();
      } else if (
        e.key === 'F11' ||
        ((e.ctrlKey || e.metaKey) && e.shiftKey && e.key.toLowerCase() === 'f')
      ) {
        e.preventDefault();
        toggleFullscreen();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose, toggleFullscreen]);

  // Refit the terminal after the popup mounts or fullscreen toggles, in case
  // the container size is still settling.
  useEffect(() => {
    const timer = window.setTimeout(() => fit(), 50);
    return () => window.clearTimeout(timer);
  }, [fit, isFullscreen]);

  useEffect(() => {
    return () => {
      if (reconnectTimerRef.current != null) {
        window.clearTimeout(reconnectTimerRef.current);
      }
    };
  }, []);

  const statusConfig = {
    connecting: { label: t('terminal.connecting'), tone: 'info' as const },
    connected: { label: t('terminal.connected'), tone: 'ok' as const },
    disconnected: { label: t('terminal.disconnected'), tone: 'err' as const },
  };
  const displayStatus = !terminalEnabled && !noRunningContainer ? 'connecting' : status;
  const { label, tone } = statusConfig[displayStatus];

  const disconnectMessage = disconnectReason
    ? closeReasonMessage(undefined, disconnectReason)
    : undefined;

  const showContainerSelector = containers.length > 1;

  return (
    <div
      className={cn(
        'fixed z-50 flex items-center justify-center',
        isFullscreen ? 'inset-0' : 'inset-0 p-4',
      )}
      onClick={onClose}
      role="dialog"
      aria-modal="true"
      aria-label={t('terminal.title')}
    >
      {/* Backdrop */}
      <div
        className={cn('absolute inset-0 bg-black/60 backdrop-blur-sm', isFullscreen && 'bg-black/80')}
        aria-hidden="true"
      />

      {/* Terminal panel */}
      <div
        className={cn(
          'relative flex flex-col overflow-hidden rounded-xl border border-border/60 bg-card shadow-2xl',
          isFullscreen ? 'h-screen w-screen' : 'w-[92vw] max-w-6xl',
        )}
        style={isFullscreen ? undefined : { height: 'min(85vh, 900px)' }}
        onClick={(e) => e.stopPropagation()}
      >
        <header className="flex flex-row items-center justify-between border-b border-border/60 px-4 py-3">
          <div className="flex items-center gap-3">
            <div>
              <CardTitle className="text-sm font-medium">{t('terminal.title')}</CardTitle>
              <CardDescription className="text-xs">
                {sandboxID}
              </CardDescription>
            </div>
            <Badge
              tone={tone}
              className={cn(
                'h-5 px-1.5 text-[10px] uppercase tracking-wider',
                displayStatus === 'connecting' && 'animate-pulse',
              )}
            >
              {label}
            </Badge>
            {showContainerSelector && (
              <DropdownMenu.Root>
                <DropdownMenu.Trigger asChild>
                  <Button
                    variant="outline"
                    size="sm"
                    className="h-7 gap-1 px-2 text-xs"
                    disabled={isLoading || !hasRunningContainer}
                  >
                    {isLoading ? (
                      <Skeleton className="h-3 w-16" />
                    ) : (
                      <>
                        <span className="max-w-[140px] truncate">
                          {selectedContainer?.name ?? t('terminal.container')}
                        </span>
                        <ChevronDown size={12} />
                      </>
                    )}
                  </Button>
                </DropdownMenu.Trigger>
                <DropdownMenu.Portal>
                  <DropdownMenu.Content
                    align="start"
                    sideOffset={6}
                    className="z-50 min-w-[180px] rounded-lg border border-border/60 bg-popover p-1 shadow-lg"
                  >
                    {containers.map((container) => {
                      const isRunning = container.state === 'running';
                      const isSelected = container.containerID === selectedContainerID;
                      return (
                        <DropdownMenu.Item
                          key={container.containerID}
                          disabled={!isRunning}
                          onSelect={() => setSelectedContainerID(container.containerID)}
                          className={cn(
                            'flex items-center justify-between rounded-md px-2 py-1.5 text-xs outline-none',
                            'focus:bg-accent focus:text-accent-foreground',
                            'data-[disabled]:pointer-events-none data-[disabled]:opacity-50',
                          )}
                        >
                          <span className="flex flex-col">
                            <span className="font-medium">{container.name}</span>
                            <span className="text-[10px] text-muted-foreground">
                              {container.state}
                            </span>
                          </span>
                          {isSelected && <Check size={13} className="text-primary" />}
                        </DropdownMenu.Item>
                      );
                    })}
                  </DropdownMenu.Content>
                </DropdownMenu.Portal>
              </DropdownMenu.Root>
            )}
          </div>
          <div className="flex items-center gap-1">
            <Button
              variant="ghost"
              size="icon"
              title={t(isFullscreen ? 'terminal.exitFullscreen' : 'terminal.fullscreen')}
              onClick={toggleFullscreen}
            >
              {isFullscreen ? <Minimize2 size={14} /> : <Maximize2 size={14} />}
            </Button>
            <Button
              variant="ghost"
              size="icon"
              title={t('terminal.fontSizeDecrease')}
              onClick={decreaseFontSize}
              disabled={fontSize <= 8}
            >
              <ZoomOut size={14} />
            </Button>
            <Button
              variant="ghost"
              size="icon"
              title={t('terminal.fontSizeIncrease')}
              onClick={increaseFontSize}
              disabled={fontSize >= 32}
            >
              <ZoomIn size={14} />
            </Button>
            {status === 'disconnected' && terminalEnabled && (
              <Button
                variant="ghost"
                size="icon"
                title={t('terminal.reconnect')}
                onClick={() => {
                  reconnect();
                  reconnectTimerRef.current = window.setTimeout(() => fit(), 0);
                }}
              >
                <RefreshCw size={14} />
              </Button>
            )}
            <Button variant="ghost" size="icon" title={t('terminal.close')} onClick={onClose}>
              <X size={14} />
            </Button>
          </div>
        </header>

        {noRunningContainer && (
          <div className="border-b border-border/60 bg-cube-warn/10 px-4 py-2 text-xs text-cube-warn">
            {t('terminal.errors.noRunningContainers')}
          </div>
        )}

        {status === 'disconnected' && disconnectMessage && (
          <div className="border-b border-border/60 bg-cube-err/10 px-4 py-2 text-xs text-cube-err">
            {t('terminal.errors.disconnected', { reason: disconnectMessage })}
          </div>
        )}

        <div
          ref={containerRef}
          className="min-h-0 flex-1 overflow-hidden"
          style={{ backgroundColor: getTerminalTheme(effective).background }}
        />
      </div>
    </div>
  );
}
