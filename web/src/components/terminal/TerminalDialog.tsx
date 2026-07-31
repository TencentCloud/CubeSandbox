// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import * as Dialog from '@radix-ui/react-dialog';
import * as DropdownMenu from '@radix-ui/react-dropdown-menu';
import * as Tooltip from '@radix-ui/react-tooltip';
import { FitAddon } from '@xterm/addon-fit';
import { Terminal as XTerm } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import {
  AArrowDown,
  AArrowUp,
  Maximize2,
  Minimize2,
  Plus,
  RotateCcw,
  TerminalSquare,
  X,
} from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { cn, short } from '@/lib/utils';
import type {
  TerminalContainer,
  TerminalSessionSnapshot,
  TerminalOpenedEvent,
} from '@/lib/terminal/client';
import { assertTerminalDimensions } from '@/lib/terminal/protocol';
import { useTerminalSession } from './useTerminalSession';

const FONT_SIZE_KEY = 'cube.terminal.fontSize';
const DEFAULT_FONT_SIZE = 14;
const MIN_FONT_SIZE = 12;
const MAX_FONT_SIZE = 20;

interface TerminalDialogProps {
  open: boolean;
  onOpenChange(open: boolean): void;
  sandboxId: string;
}

interface TerminalTab {
  id: string;
  sequence: number;
  requestedContainerId?: string;
  snapshot: TerminalSessionSnapshot | null;
}

export function TerminalDialog({ open, onOpenChange, sandboxId }: TerminalDialogProps) {
  const { t } = useTranslation('terminal');
  const [tabs, setTabs] = useState<TerminalTab[]>([]);
  const [activeTabId, setActiveTabId] = useState('');
  const [containers, setContainers] = useState<TerminalContainer[]>([]);
  const [fontSize, setFontSize] = useState(() => readTerminalFontSize());
  const [fullscreen, setFullscreen] = useState(false);
  const [fullscreenMessage, setFullscreenMessage] = useState('');
  const [fitEpoch, setFitEpoch] = useState(0);
  const contentRef = useRef<HTMLDivElement>(null);
  const sequenceRef = useRef(0);
  const initialTabRef = useRef<TerminalTab | null>(null);

  const startTab = useCallback(
    (containerId?: string) => {
      const container = containerId
        ? containers.find((item) => item.containerId === containerId)
        : containers.length === 1
          ? containers[0]
          : undefined;
      if (!container || container.status !== 1) return;

      const sequence = ++sequenceRef.current;
      const tab: TerminalTab = {
        id: `terminal-${sequence}`,
        sequence,
        requestedContainerId: container.containerId,
        snapshot: null,
      };
      setTabs((current) => [...current, tab]);
      setActiveTabId(tab.id);
    },
    [containers],
  );

  useEffect(() => {
    if (!open) return;
    if (!initialTabRef.current) {
      const sequence = ++sequenceRef.current;
      initialTabRef.current = { id: `terminal-${sequence}`, sequence, snapshot: null };
    }
    const initial = initialTabRef.current;
    setTabs((current) => {
      if (current.length > 0) return current;
      return [initial];
    });
    setActiveTabId((current) => current || initial.id);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onFullscreenChange = () => {
      setFullscreen(document.fullscreenElement === contentRef.current);
      setFitEpoch((value) => value + 1);
    };
    document.addEventListener('fullscreenchange', onFullscreenChange);
    return () => document.removeEventListener('fullscreenchange', onFullscreenChange);
  }, [open]);

  const updateTabSnapshot = useCallback((tabId: string, snapshot: TerminalSessionSnapshot) => {
    setTabs((current) => current.map((tab) => (tab.id === tabId ? { ...tab, snapshot } : tab)));
    if (snapshot.metadata) {
      setContainers(snapshot.metadata.containers);
    }
  }, []);

  const closeTab = useCallback(
    (tabId: string) => {
      const index = tabs.findIndex((tab) => tab.id === tabId);
      const next = tabs.filter((tab) => tab.id !== tabId);
      setTabs(next);
      setActiveTabId((active) =>
        active === tabId ? (next[Math.min(index, next.length - 1)]?.id ?? '') : active,
      );
    },
    [tabs],
  );

  const handleOpenChange = useCallback(
    (next: boolean) => {
      if (!next) {
        if (document.fullscreenElement === contentRef.current && document.exitFullscreen) {
          void document.exitFullscreen().catch(() => undefined);
        }
        setTabs([]);
        setActiveTabId('');
        setContainers([]);
        initialTabRef.current = null;
        setFullscreen(false);
        setFullscreenMessage('');
      }
      onOpenChange(next);
    },
    [onOpenChange],
  );

  const changeFontSize = useCallback((value: number) => {
    const next = Math.min(MAX_FONT_SIZE, Math.max(MIN_FONT_SIZE, Math.round(value)));
    setFontSize(next);
    writeTerminalFontSize(next);
  }, []);

  const toggleFullscreen = useCallback(async () => {
    setFullscreenMessage('');
    const element = contentRef.current;
    if (!element || !document.fullscreenEnabled || !element.requestFullscreen) {
      setFullscreenMessage(t('fullscreenUnavailable'));
      return;
    }
    try {
      if (document.fullscreenElement === element) await document.exitFullscreen();
      else await element.requestFullscreen();
    } catch {
      setFullscreenMessage(t('fullscreenFailed'));
    }
  }, [t]);

  const activeTab = tabs.find((tab) => tab.id === activeTabId) ?? null;
  const canStartNewSession = containers.some((container) => container.status === 1);

  return (
    <Dialog.Root open={open} onOpenChange={handleOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-background/75 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content
          ref={contentRef}
          className={cn(
            'fixed left-[50dvw] top-[50dvh] z-50 flex -translate-x-1/2 -translate-y-1/2 flex-col overflow-hidden',
            'h-[min(760px,calc(100dvh-0.5rem))] w-[min(1180px,calc(100dvw-0.5rem))]',
            'rounded-lg border border-border/70 bg-card shadow-2xl',
            'focus:outline-none data-[state=open]:animate-fade-in',
            'fullscreen:inset-0 fullscreen:h-screen fullscreen:w-screen fullscreen:translate-x-0 fullscreen:translate-y-0 fullscreen:rounded-none fullscreen:border-0',
          )}
        >
          <Dialog.Description className="sr-only">{t('description')}</Dialog.Description>
          <header className="flex min-h-14 shrink-0 items-center gap-3 border-b border-border/60 px-3 sm:px-4">
            <TerminalSquare size={17} className="shrink-0 text-primary" />
            <div className="min-w-0 flex-1">
              <Dialog.Title className="text-sm font-semibold sm:text-base">
                {t('title')}
              </Dialog.Title>
              <p className="truncate font-mono text-[11px] text-muted-foreground" title={sandboxId}>
                {sandboxId}
              </p>
            </div>
            <TooltipIconButton
              label={fullscreen ? t('exitFullscreen') : t('fullscreen')}
              onClick={() => void toggleFullscreen()}
            >
              {fullscreen ? <Minimize2 size={15} /> : <Maximize2 size={15} />}
            </TooltipIconButton>
            <TooltipIconButton label={t('close')} onClick={() => handleOpenChange(false)}>
              <X size={16} />
            </TooltipIconButton>
          </header>

          <div className="flex h-10 shrink-0 items-stretch border-b border-border/60 bg-muted/30 px-2 pt-1">
            <div className="flex min-w-0 flex-1 items-stretch overflow-x-auto" role="tablist">
              {tabs.map((tab) => {
                const label = tabLabel(tab, containers, t('tab', { number: tab.sequence }));
                const selected = tab.id === activeTabId;
                return (
                  <div
                    key={tab.id}
                    className={cn(
                      'mr-1 flex min-w-0 max-w-56 items-center rounded-t-md border border-b-0',
                      selected
                        ? 'border-border bg-background text-foreground'
                        : 'border-transparent text-muted-foreground hover:bg-background/50',
                    )}
                  >
                    <button
                      type="button"
                      role="tab"
                      aria-selected={selected}
                      aria-controls={`${tab.id}-panel`}
                      className="min-w-0 flex-1 truncate px-3 py-2 text-left text-xs"
                      title={label}
                      onClick={() => setActiveTabId(tab.id)}
                    >
                      {label}
                    </button>
                    <TooltipIconButton
                      label={t('closeTab', { name: label })}
                      compact
                      onClick={() => closeTab(tab.id)}
                    >
                      <X size={12} />
                    </TooltipIconButton>
                  </div>
                );
              })}
              <NewTerminalTabButton
                containers={containers}
                canStartNewSession={canStartNewSession}
                onStartTab={startTab}
              />
            </div>
            <div
              className="ml-2 flex shrink-0 items-center gap-0.5 border-l border-border/60 pl-2"
              aria-label={t('fontSize')}
            >
              <TooltipIconButton
                label={t('decreaseFont')}
                disabled={fontSize <= MIN_FONT_SIZE}
                onClick={() => changeFontSize(fontSize - 1)}
              >
                <AArrowDown size={15} />
              </TooltipIconButton>
              <TooltipIconButton
                label={t('increaseFont')}
                disabled={fontSize >= MAX_FONT_SIZE}
                onClick={() => changeFontSize(fontSize + 1)}
              >
                <AArrowUp size={15} />
              </TooltipIconButton>
            </div>
          </div>

          <main className="relative min-h-0 flex-1 bg-[#0b0d0f]">
            {tabs.length === 0 ? (
              <div className="flex h-full flex-col items-center justify-center gap-3 text-center text-sm text-neutral-400">
                <TerminalSquare size={24} />
                <p>{t('noSessions')}</p>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={containers.length !== 1 || !canStartNewSession}
                  onClick={() => startTab()}
                >
                  <RotateCcw size={14} /> {t('startNewSession')}
                </Button>
              </div>
            ) : null}
            {tabs.map((tab) => (
              <TerminalPane
                key={tab.id}
                id={tab.id}
                active={tab.id === activeTabId}
                sandboxId={sandboxId}
                requestedContainerId={tab.requestedContainerId}
                fontSize={fontSize}
                fitEpoch={fitEpoch}
                onSnapshot={updateTabSnapshot}
                onStartNewSession={(containerId) => startTab(containerId)}
                canRestartTarget={isRunningContainer(
                  containers,
                  tab.snapshot?.metadata?.containerId ?? tab.requestedContainerId,
                )}
              />
            ))}
          </main>

          <div className="sr-only" aria-live="polite">
            {fullscreenMessage}
          </div>
          {fullscreenMessage ? (
            <div className="absolute right-3 top-16 z-10 rounded-md border border-cube-warn/30 bg-background px-3 py-2 text-xs text-cube-warn shadow-lg">
              {fullscreenMessage}
            </div>
          ) : null}
          {activeTab?.snapshot?.state.kind === 'detached' ? (
            <div className="sr-only" aria-live="assertive">
              {t('status.detached', { attempt: activeTab.snapshot.state.attempt })}
            </div>
          ) : null}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function NewTerminalTabButton({
  containers,
  canStartNewSession,
  onStartTab,
}: {
  containers: TerminalContainer[];
  canStartNewSession: boolean;
  onStartTab(containerId?: string): void;
}) {
  const { t } = useTranslation('terminal');

  if (containers.length <= 1) {
    const container = containers[0];
    return (
      <TooltipIconButton
        label={t('newSession')}
        compact
        disabled={!container || !canStartNewSession}
        onClick={() => onStartTab(container.containerId)}
      >
        <Plus size={15} />
      </TooltipIconButton>
    );
  }

  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <button
          type="button"
          aria-label={t('newSession')}
          title={t('newSession')}
          disabled={!canStartNewSession}
          className="mr-1 inline-flex h-7 w-7 shrink-0 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-40"
        >
          <Plus size={15} />
        </button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content
          align="start"
          sideOffset={6}
          aria-label={t('selectContainer')}
          className="z-[70] min-w-52 rounded-md border border-border/60 bg-popover p-1 text-popover-foreground shadow-md data-[state=open]:animate-fade-in"
        >
          <DropdownMenu.Label className="px-2.5 py-1.5 text-xs font-medium text-muted-foreground">
            {t('chooseContainer')}
          </DropdownMenu.Label>
          <DropdownMenu.Separator className="my-1 h-px bg-border/70" />
          {containers.map((container) => (
            <DropdownMenu.Item
              key={container.containerId}
              disabled={container.status !== 1}
              onSelect={() => onStartTab(container.containerId)}
              className="flex cursor-pointer select-none items-center gap-2 rounded-sm px-2.5 py-2 text-xs outline-none transition-colors hover:bg-muted focus:bg-muted data-[disabled]:pointer-events-none data-[disabled]:opacity-50"
            >
              <span className="min-w-0 flex-1 truncate">{containerLabel(container)}</span>
              {container.status !== 1 ? (
                <span className="shrink-0 text-muted-foreground">{t('containerUnavailable')}</span>
              ) : null}
            </DropdownMenu.Item>
          ))}
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  );
}

interface TerminalPaneProps {
  id: string;
  active: boolean;
  sandboxId: string;
  requestedContainerId?: string;
  fontSize: number;
  fitEpoch: number;
  canRestartTarget: boolean;
  onSnapshot(tabId: string, snapshot: TerminalSessionSnapshot): void;
  onStartNewSession(containerId?: string): void;
}

function TerminalPane({
  id,
  active,
  sandboxId,
  requestedContainerId,
  fontSize,
  fitEpoch,
  canRestartTarget,
  onSnapshot,
  onStartNewSession,
}: TerminalPaneProps) {
  const { t } = useTranslation('terminal');
  const hostRef = useRef<HTMLDivElement>(null);
  const terminalRef = useRef<XTerm | null>(null);
  const scheduleFitRef = useRef<(() => void) | null>(null);

  const writeSystemLine = useCallback((message: string, tone: 'muted' | 'warning' = 'muted') => {
    const color = tone === 'warning' ? '\u001b[33m' : '\u001b[90m';
    terminalRef.current?.write(`\r\n${color}--- ${message} ---\u001b[0m\r\n`);
  }, []);

  const handleOpened = useCallback(
    (event: TerminalOpenedEvent) => {
      if (!event.resumed) return;
      writeSystemLine(t('recovered'));
      if (event.truncated) writeSystemLine(t('replayTruncated'), 'warning');
    },
    [t, writeSystemLine],
  );

  const { snapshot, start, sendInput, resize, dispose } = useTerminalSession({
    sandboxId,
    containerId: requestedContainerId,
    onOutput: (data) => terminalRef.current?.write(data),
    onOpened: handleOpened,
  });

  useEffect(() => onSnapshot(id, snapshot), [id, onSnapshot, snapshot]);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    const terminal = new XTerm({
      cursorBlink: true,
      scrollback: 5000,
      fontFamily: '"JetBrains Mono Variable", "JetBrains Mono", ui-monospace, monospace',
      fontSize,
      theme: {
        background: '#0b0d0f',
        foreground: '#e5e7eb',
        cursor: '#d1d5db',
        selectionBackground: '#374151',
      },
    });
    const fitAddon = new FitAddon();
    terminal.loadAddon(fitAddon);
    terminal.open(host);
    terminalRef.current = terminal;

    let animationFrame: number | null = null;
    let started = false;
    let disposed = false;
    const fit = () => {
      animationFrame = null;
      if (disposed || !host.isConnected || host.clientWidth <= 0 || host.clientHeight <= 0) return;
      try {
        fitAddon.fit();
        assertTerminalDimensions(terminal.cols, terminal.rows);
      } catch {
        return;
      }
      if (!started) {
        started = true;
        start(terminal.cols, terminal.rows);
      } else {
        resize(terminal.cols, terminal.rows);
      }
    };
    const scheduleFit = () => {
      if (animationFrame !== null) cancelAnimationFrame(animationFrame);
      animationFrame = requestAnimationFrame(fit);
    };
    scheduleFitRef.current = scheduleFit;
    const observer = new ResizeObserver(scheduleFit);
    observer.observe(host);
    const input = terminal.onData((data) => {
      void sendInput(data);
    });
    scheduleFit();

    return () => {
      disposed = true;
      if (animationFrame !== null) cancelAnimationFrame(animationFrame);
      scheduleFitRef.current = null;
      observer.disconnect();
      input.dispose();
      dispose();
      fitAddon.dispose();
      terminal.dispose();
      terminalRef.current = null;
    };
  }, [dispose, requestedContainerId, resize, sandboxId, sendInput, start]);

  useEffect(() => {
    if (!terminalRef.current) return;
    terminalRef.current.options.fontSize = fontSize;
    if (active) scheduleFitRef.current?.();
  }, [active, fitEpoch, fontSize]);

  const stateText = useMemo(() => {
    switch (snapshot.state.kind) {
      case 'connecting':
        return t('status.connecting');
      case 'connected':
        return t('status.connected');
      case 'detached':
        return t('status.detached', { attempt: snapshot.state.attempt });
      case 'closed':
        if (snapshot.state.reason === 'RUNTIME_EXITED' && snapshot.state.exitCode !== undefined) {
          return t('exitCode', { code: snapshot.state.exitCode });
        }
        return t(`errors.${snapshot.state.reason}`, { defaultValue: t('errors.fallback') });
    }
  }, [snapshot.state, t]);

  const metadata = snapshot.metadata;
  const container = metadata?.containers.find((item) => item.containerId === metadata.containerId);
  const statusTone =
    snapshot.state.kind === 'connected'
      ? 'bg-cube-ok'
      : snapshot.state.kind === 'detached'
        ? 'bg-cube-warn'
        : snapshot.state.kind === 'closed'
          ? 'bg-cube-err'
          : 'bg-muted-foreground';

  return (
    <section
      id={`${id}-panel`}
      role="tabpanel"
      aria-hidden={!active}
      className={cn('absolute inset-0 min-h-0 flex-col', active ? 'flex' : 'hidden')}
    >
      <div
        ref={hostRef}
        aria-label={t('terminalRegion')}
        className="min-h-0 flex-1 overflow-hidden p-2 [&_.xterm]:h-full [&_.xterm-viewport]:!overflow-y-auto"
      />
      <footer className="grid h-14 shrink-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-3 border-t border-white/10 bg-[#111315] px-3 text-[11px] text-neutral-400">
        <div className="grid min-w-0 grid-cols-2 gap-x-4 sm:grid-cols-3">
          <span className="flex min-w-0 items-center gap-1.5 truncate" aria-live="polite">
            <span className={cn('h-1.5 w-1.5 shrink-0 rounded-full', statusTone)} />
            <span className="truncate">{stateText}</span>
          </span>
          <span className="min-w-0 truncate" title={metadata?.containerId}>
            <span className="text-neutral-500">{t('metadata.container')}:</span>{' '}
            {container
              ? containerLabel(container)
              : requestedContainerId
                ? short(requestedContainerId)
                : '—'}
          </span>
          <span className="hidden min-w-0 truncate font-mono sm:block" title={metadata?.sessionId}>
            <span className="font-sans text-neutral-500">{t('metadata.session')}:</span>{' '}
            {metadata?.sessionId ? short(metadata.sessionId) : '—'}
          </span>
        </div>
        {snapshot.state.kind === 'closed' && snapshot.state.canStartNewSession ? (
          <Button
            size="sm"
            variant="outline"
            disabled={!canRestartTarget}
            className="h-8 border-white/15 bg-transparent text-neutral-200 hover:bg-white/10"
            onClick={() => onStartNewSession(metadata?.containerId ?? requestedContainerId)}
          >
            <RotateCcw size={13} />
            <span className="hidden sm:inline">{t('startNewSession')}</span>
          </Button>
        ) : null}
      </footer>
    </section>
  );
}

function TooltipIconButton({
  label,
  children,
  disabled,
  compact,
  onClick,
}: {
  label: string;
  children: ReactNode;
  disabled?: boolean;
  compact?: boolean;
  onClick(): void;
}) {
  return (
    <Tooltip.Provider delayDuration={250}>
      <Tooltip.Root>
        <Tooltip.Trigger asChild>
          <span className="inline-flex" tabIndex={disabled ? 0 : -1}>
            <button
              type="button"
              aria-label={label}
              title={label}
              disabled={disabled}
              onClick={onClick}
              className={cn(
                'inline-flex items-center justify-center rounded-md text-muted-foreground transition-colors',
                'hover:bg-muted hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring',
                'disabled:pointer-events-none disabled:opacity-40',
                compact ? 'h-7 w-7' : 'h-8 w-8',
              )}
            >
              {children}
            </button>
          </span>
        </Tooltip.Trigger>
        <Tooltip.Portal>
          <Tooltip.Content
            sideOffset={5}
            className="z-[70] rounded-md border border-border/60 bg-popover px-2 py-1 text-xs text-popover-foreground shadow-md"
          >
            {label}
            <Tooltip.Arrow className="fill-popover" />
          </Tooltip.Content>
        </Tooltip.Portal>
      </Tooltip.Root>
    </Tooltip.Provider>
  );
}

export function readTerminalFontSize(storage: Pick<Storage, 'getItem'> = localStorage): number {
  try {
    const value = Number(storage.getItem(FONT_SIZE_KEY));
    return Number.isInteger(value) && value >= MIN_FONT_SIZE && value <= MAX_FONT_SIZE
      ? value
      : DEFAULT_FONT_SIZE;
  } catch {
    return DEFAULT_FONT_SIZE;
  }
}

function writeTerminalFontSize(value: number): void {
  try {
    localStorage.setItem(FONT_SIZE_KEY, String(value));
  } catch {
    // A denied storage write must not prevent terminal use.
  }
}

function containerLabel(container: TerminalContainer): string {
  return container.name?.trim() || container.type?.trim() || short(container.containerId);
}

function isRunningContainer(containers: TerminalContainer[], containerId?: string): boolean {
  return Boolean(
    containerId &&
    containers.some((container) => container.containerId === containerId && container.status === 1),
  );
}

function tabLabel(tab: TerminalTab, containers: TerminalContainer[], fallback: string): string {
  const containerId = tab.snapshot?.metadata?.containerId ?? tab.requestedContainerId;
  if (!containerId) return fallback;
  const container = containers.find((item) => item.containerId === containerId);
  return container ? containerLabel(container) : short(containerId);
}
