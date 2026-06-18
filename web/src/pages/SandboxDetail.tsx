// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useRef, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate, useParams, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { sandboxApi } from '@/api/client';
import { Card, CardTitle, CardDescription, CardHeader } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Badge, type Tone } from '@/components/ui/badge';
import { Tabs, TabsList, TabsTrigger, TabsContent } from '@/components/ui/tabs';
import { CodeTerminal } from '@/components/CodeTerminal';

import { Skeleton } from '@/components/ui/skeleton';
import { DropdownMenu, DropdownMenuTrigger, DropdownMenuContent, DropdownMenuItem } from '@/components/ui/dropdown-menu';
import {
  ArrowLeft, Pause, Play, Trash2, RefreshCw, FileText,
  Cpu, MemoryStick, User, Clock, Globe, Server, Activity,
  ArrowUpDown, Download,
} from 'lucide-react';
import { cn, formatBytes, formatRelative } from '@/lib/utils';
import { formatSandboxActionError } from '@/lib/sandboxActionError';
import { SandboxActionErrorBanner } from '@/components/SandboxActionErrorBanner';

// ── Log level colors ────────────────────────────────────────────────────────
const LEVEL_CLASS: Record<string, string> = {
  debug: 'text-muted-foreground/50',
  info: 'text-foreground/60',
  warn: 'text-cube-warn/70',
  error: 'text-cube-err/70',
};

const LEVEL_BADGE: Record<string, string> = {
  debug: 'bg-muted text-muted-foreground',
  info: 'bg-blue-500/10 text-blue-500',
  warn: 'bg-cube-warn/10 text-cube-warn',
  error: 'bg-cube-err/10 text-cube-err',
};

function formatLogDateTime(ts: string): string {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return new Intl.DateTimeFormat(undefined, {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
    hour12: false,
  }).format(d);
}

function escapeCsvField(field: string): string {
  if (field.includes(',') || field.includes('"') || field.includes('\n')) {
    return `"${field.replace(/"/g, '""')}"`;
  }
  return field;
}

// ── Main page ───────────────────────────────────────────────────────────────
export default function SandboxDetailPage() {
  const { sandboxID = '' } = useParams();
  const nav = useNavigate();
  const qc = useQueryClient();
  const { t } = useTranslation('sandboxDetail');

  // ── Sandbox detail ──────────────────────────────────────────────────────
  const { data, isLoading } = useQuery({
    queryKey: ['sandbox', sandboxID],
    queryFn: () => sandboxApi.get(sandboxID),
    enabled: !!sandboxID,
    refetchInterval: 5_000,
  });

  // ── Logs ────────────────────────────────────────────────────────────────
  const logs = useQuery({
    queryKey: ['sandbox-logs', sandboxID],
    queryFn: () => sandboxApi.logs(sandboxID),
    enabled: !!sandboxID,
    refetchInterval: 10_000,
  });
  const logRef = useRef<HTMLPreElement>(null);
  // Auto-scroll to bottom whenever new logs arrive
  useEffect(() => {
    if (logRef.current) {
      logRef.current.scrollTop = logRef.current.scrollHeight;
    }
  }, [logs.data]);

  const [actionError, setActionError] = useState<string | null>(null);
  const onLifecycleError = (err: unknown) => {
    setActionError(formatSandboxActionError(err, t));
  };


  // ── Lifecycle mutations ─────────────────────────────────────────────────
  const kill = useMutation({
    mutationFn: () => sandboxApi.kill(sandboxID),
    onMutate: () => setActionError(null),
    onError: onLifecycleError,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sandboxes'] });
      nav('/sandboxes');
    },
  });
  const pause = useMutation({
    mutationFn: () => sandboxApi.pause(sandboxID),
    onMutate: () => setActionError(null),
    onError: onLifecycleError,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sandboxes'] });
      qc.invalidateQueries({ queryKey: ['sandbox', sandboxID] });
    },
  });
  const resume = useMutation({
    mutationFn: () => sandboxApi.resume(sandboxID),
    onMutate: () => setActionError(null),
    onError: onLifecycleError,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sandboxes'] });
      qc.invalidateQueries({ queryKey: ['sandbox', sandboxID] });
    },
  });

  const state = data?.state ?? 'running';
  const tone: Tone = state === 'paused' || state === 'pausing' ? 'warn' : state === 'running' ? 'ok' : 'mute';
  const entries = logs.data?.logs ?? [];
  const [sortOrder, setSortOrder] = useState<'desc' | 'asc'>('desc');

  // Sort entries by timestamp
  const sortedEntries = [...entries].sort((a, b) => {
    const ta = new Date(a.timestamp as unknown as string).getTime();
    const tb = new Date(b.timestamp as unknown as string).getTime();
    return sortOrder === 'desc' ? tb - ta : ta - tb;
  });

  function exportEvents(format: 'csv' | 'txt') {
    if (sortedEntries.length === 0) return;

    const rows = sortedEntries.map((entry) => {
      const d = new Date(entry.timestamp as unknown as string);
      const ts = Number.isNaN(d.getTime())
        ? String(entry.timestamp)
        : d.toISOString().replace('T', ' ').slice(0, 19);
      const level = (entry.level ?? 'info').toString();
      const message = entry.message ?? '';
      return { ts, level, message };
    });

    let content: string;
    let mimeType: string;
    let ext: string;

    if (format === 'csv') {
      const header = 'Timestamp,Level,Message';
      const csvRows = rows.map((r) =>
        `${escapeCsvField(r.ts)},${escapeCsvField(r.level)},${escapeCsvField(r.message)}`
      );
      content = [header, ...csvRows].join('\r\n');
      mimeType = 'text/csv;charset=utf-8;';
      ext = 'csv';
    } else {
      const txtRows = rows.map((r) => `[${r.ts}] [${r.level}] ${r.message}`);
      content = txtRows.join('\n');
      mimeType = 'text/plain;charset=utf-8;';
      ext = 'txt';
    }

    const bom = '\uFEFF';
    const blob = new Blob([bom + content], { type: mimeType });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `sandbox-${sandboxID}-events.${ext}`;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
  }

  return (
    <div className="animate-fade-in space-y-5">
      {/* ── Header ── */}
      <div className="flex items-center gap-3">
        <Link to="/sandboxes">
          <Button variant="ghost" size="icon">
            <ArrowLeft size={16} />
          </Button>
        </Link>
        <div className="flex-1">
          <div className="flex items-center gap-2">
            <h1 className="font-mono text-xl font-medium tracking-tight">{sandboxID}</h1>
            <Badge tone={tone} className="text-sm px-2.5 py-1">{state}</Badge>
          </div>
          <p className="mt-1 text-xs text-muted-foreground">
            {data?.templateID ?? '—'} · {t('started', { time: formatRelative(data?.startedAt) })}
          </p>
        </div>
        <div className="flex gap-2">
          {state === 'paused' ? (
            <Button variant="outline" onClick={() => resume.mutate()} disabled={resume.isPending}>
              <Play size={14} /> {t('actions.resume')}
            </Button>
          ) : (
            <Button variant="outline" onClick={() => pause.mutate()} disabled={pause.isPending}>
              <Pause size={14} /> {t('actions.pause')}
            </Button>
          )}
          <Button variant="destructive" onClick={() => kill.mutate()} disabled={kill.isPending}>
            <Trash2 size={14} /> {t('actions.kill')}
          </Button>
        </div>
      </div>

      <SandboxActionErrorBanner message={actionError} onDismiss={() => setActionError(null)} />

      {/* ── Tabs ── */}
      <Tabs defaultValue="management">
        <TabsList>
          <TabsTrigger value="management">{t('tab.management')}</TabsTrigger>
          <TabsTrigger value="monitor">{t('tab.monitor')}</TabsTrigger>
          <TabsTrigger value="events">{t('tab.events')}</TabsTrigger>
          <TabsTrigger value="metadata">{t('tab.metadata')}</TabsTrigger>
          <TabsTrigger value="logs">{t('tab.logs')}</TabsTrigger>
        </TabsList>

        {/* ── Tab: 沙箱管理 ── */}
        <TabsContent value="management" className="space-y-4">
          <Card>
            <CardHeader>
              <div>
                <CardTitle>{t('runtime')}</CardTitle>
                <CardDescription>{t('runtimeDesc')}</CardDescription>
              </div>
            </CardHeader>
            <div className="grid grid-cols-2 gap-px bg-border/40 sm:grid-cols-4">
              <RuntimeTile
                icon={<Server size={15} className="text-violet-400" />}
                label={t('fields.sandboxID')}
                value={data?.sandboxID ?? '—'}
                mono
              />
              <RuntimeTile
                icon={<Globe size={15} className="text-cyan-400" />}
                label={t('fields.nodeIP')}
                value={data?.clientID ?? '—'}
                mono
              />
              <RuntimeTile
                icon={<Globe size={15} className="text-cyan-400" />}
                label={t('fields.domain')}
                value={data?.domain ?? '—'}
                mono
              />
              <RuntimeTile
                icon={<Cpu size={15} className="text-blue-400" />}
                label={t('fields.vcpu')}
                value={data?.cpuCount != null ? String(data.cpuCount) : '—'}
              />
              <RuntimeTile
                icon={<MemoryStick size={15} className="text-emerald-400" />}
                label={t('fields.memory')}
                value={data?.memoryMB != null ? `${(data.memoryMB / 1024).toFixed(1)} GB` : '—'}
              />
              <RuntimeTile
                icon={<Server size={15} className="text-violet-400" />}
                label={t('fields.envd')}
                value={data?.envdVersion ?? '—'}
                mono
              />
              <RuntimeTile
                icon={<Activity size={15} className={cn(
                  'text-cube-ok',
                  state === 'paused' && 'text-cube-warn',
                  state !== 'running' && state !== 'paused' && 'text-muted-foreground',
                )} />}
                label={t('fields.state')}
                value={
                  <Badge
                    tone={state === 'paused' ? 'warn' : state === 'running' ? 'ok' : 'mute'}
                    className="gap-1.5"
                  >
                    <span className={cn(
                      'h-1.5 w-1.5 rounded-full bg-current',
                      state === 'running' && 'animate-pulse',
                    )} />
                    {state}
                  </Badge>
                }
              />
              <RuntimeTile
                icon={<Clock size={15} className="text-blue-400" />}
                label={t('fields.started')}
                value={formatDateTime(data?.startedAt)}
              />
            </div>
          </Card>

          {/* Code Terminal */}
          <CodeTerminal
            sandboxId={sandboxID}
            disabled={state !== 'running'}
          />
        </TabsContent>

        {/* ── Tab: 详情 ── */}
        <TabsContent value="monitor" className="space-y-4">
          {/* KPI cards: vCPU & Memory */}
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <MonitorKpi
              icon={<Cpu size={16} className="text-blue-400" />}
              label={t('fields.vcpu')}
              value={data?.cpuCount != null ? String(data.cpuCount) : '—'}
              unit="cores"
            />
            <MonitorKpi
              icon={<MemoryStick size={16} className="text-emerald-400" />}
              label={t('fields.memory')}
              value={data?.memoryMB != null ? formatBytes(data.memoryMB) : '—'}
            />
          </div>

          {/* Meta stats */}
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-1.5 text-lg">
                <Activity size={16} className="text-muted-foreground/60" />
                {t('resources')}
              </CardTitle>
            </CardHeader>
            {isLoading ? (
              <Skeleton className="h-24 w-full" />
            ) : (
              <div className="divide-y divide-border/40">
                <RuntimeRow
                  icon={<User size={15} className="text-amber-400" />}
                  label={t('fields.client')}
                  value={data?.clientID ?? '—'}
                  mono
                />
                <RuntimeRow
                  icon={<Clock size={15} className="text-blue-400" />}
                  label={t('fields.started')}
                  value={formatDateTime(data?.startedAt)}
                />
                <RuntimeRow
                  icon={<Globe size={15} className="text-cyan-400" />}
                  label={t('fields.domain')}
                  value={data?.domain ?? '—'}
                  mono
                />
              </div>
            )}
          </Card>
        </TabsContent>

        {/* ── Tab: 事件 ── */}
        <TabsContent value="events">
          <Card>
            <CardHeader>
              <div className="flex items-center gap-2.5">
                <CardTitle>{t('tab.events')}</CardTitle>
                {entries.length > 0 && (
                  <span className="rounded-md bg-muted/60 px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
                    {entries.length}
                  </span>
                )}
              </div>
              <div className="flex items-center gap-1">
                <Button
                  size="icon"
                  variant="ghost"
                  title={sortOrder === 'desc' ? t('eventsSortDesc') : t('eventsSortAsc')}
                  onClick={() => setSortOrder(sortOrder === 'desc' ? 'asc' : 'desc')}
                >
                  <ArrowUpDown size={14} className={cn(sortOrder === 'desc' ? 'rotate-0' : 'rotate-180 transition-transform')} />
                </Button>
                <Button
                  size="icon"
                  variant="ghost"
                  title={t('logsRefresh')}
                  onClick={() => logs.refetch()}
                  disabled={logs.isFetching}
                >
                  <RefreshCw size={14} className={cn(logs.isFetching && 'animate-spin')} />
                </Button>
                <DropdownMenu>
                  <DropdownMenuTrigger asChild>
                    <Button
                      size="icon"
                      variant="ghost"
                      title={t('eventsExport')}
                      disabled={entries.length === 0}
                    >
                      <Download size={14} />
                    </Button>
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="end">
                    <DropdownMenuItem onClick={() => exportEvents('csv')}>
                      <FileText size={14} />
                      {t('eventsExportCsv')}
                    </DropdownMenuItem>
                    <DropdownMenuItem onClick={() => exportEvents('txt')}>
                      <FileText size={14} />
                      {t('eventsExportTxt')}
                    </DropdownMenuItem>
                  </DropdownMenuContent>
                </DropdownMenu>
              </div>
            </CardHeader>
            <div className="-mt-2 px-5 pb-3 text-xs text-muted-foreground">
              {t('logsDesc')}
            </div>

            {logs.isLoading ? (
              <div className="py-8 text-center text-sm text-muted-foreground">{t('logsLoading')}</div>
            ) : entries.length === 0 ? (
              <div className="py-8 text-center text-sm text-muted-foreground">{t('logsEmpty')}</div>
            ) : (
              <div className="overflow-auto rounded-md ring-1 ring-border/60">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-border/60 bg-muted/40 text-left">
                      <th className="whitespace-nowrap px-4 py-2 text-xs font-medium uppercase tracking-wider text-muted-foreground">
                        {t('eventsTimestamp')}
                      </th>
                      <th className="whitespace-nowrap px-4 py-2 text-xs font-medium uppercase tracking-wider text-muted-foreground">
                        {t('eventsLevel')}
                      </th>
                      <th className="px-4 py-2 text-xs font-medium uppercase tracking-wider text-muted-foreground">
                        {t('eventsMessage')}
                      </th>
                    </tr>
                  </thead>
                  <tbody className="font-mono text-xs">
                    {sortedEntries.map((entry, i) => {
                      const lvl = (entry.level ?? 'info').toLowerCase();
                      const badgeCls = LEVEL_BADGE[lvl] ?? 'bg-muted text-muted-foreground';
                      const textCls = LEVEL_CLASS[lvl] ?? 'text-foreground';
                      return (
                        <tr key={i} className="border-b border-border/30 last:border-0 hover:bg-muted/30">
                          <td className="whitespace-nowrap px-4 py-2 text-muted-foreground/60">
                            {formatLogDateTime(entry.timestamp as unknown as string)}
                          </td>
                          <td className="px-4 py-2">
                            <span className={cn('inline-block rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase', badgeCls)}>
                              {lvl}
                            </span>
                          </td>
                          <td className={cn('break-all px-4 py-2', textCls)}>
                            {entry.message}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </Card>
        </TabsContent>

        {/* ── Tab: 元数据（YAML 风格） ── */}
        <TabsContent value="metadata">
          <Card>
            <pre className="max-h-[600px] overflow-auto rounded-md bg-muted/60 p-5 font-mono text-sm leading-loose ring-1 ring-border/60">
              {data?.metadata && Object.keys(data.metadata).length > 0 ? (
                Object.entries(data.metadata).map(([k, v]) => (
                  <div key={k}>
                    <span className="text-blue-400">{k}</span>
                    <span className="text-muted-foreground">: </span>
                    <span className="text-emerald-400">{String(v)}</span>
                  </div>
                ))
              ) : (
                <span className="text-muted-foreground">{t('noMetadata')}</span>
              )}
            </pre>
          </Card>
        </TabsContent>

        {/* ── Tab: 日志（空白占位） ── */}
        <TabsContent value="logs">
          <Card className="flex flex-col items-center justify-center py-20">
            <FileText size={40} className="text-muted-foreground/30" />
            <p className="mt-4 text-sm text-muted-foreground">{t('logsComingSoon')}</p>
          </Card>
        </TabsContent>
      </Tabs>
    </div>
  );
}

// ── Helpers ─────────────────────────────────────────────────────────────────
function formatDateTime(value?: string | null): string {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat(undefined, {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
    hour12: false,
  }).format(date);
}

function RuntimeTile({ icon, label, value, mono }: { icon: React.ReactNode; label: string; value: React.ReactNode; mono?: boolean }) {
  return (
    <div className="flex flex-col gap-1.5 bg-card px-4 py-3">
      <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
        {icon}
        <span>{label}</span>
      </span>
      <span className={cn('text-sm font-medium text-foreground/90 truncate', mono && 'font-mono')}>
        {value}
      </span>
    </div>
  );
}

function RuntimeRow({ icon, label, value, mono }: { icon: React.ReactNode; label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex items-center justify-between gap-4 py-3 first:pt-0 last:pb-0">
      <span className="flex items-center gap-2 text-sm text-muted-foreground">
        {icon}
        <span>{label}</span>
      </span>
      <span className={cn('text-base font-medium text-foreground/90 truncate', mono && 'font-mono')}>
        {value}
      </span>
    </div>
  );
}

function MonitorKpi({ icon, label, value, unit }: { icon: React.ReactNode; label: string; value: string; unit?: string }) {
  return (
    <div className="rounded-xl border border-border/60 bg-card/40 p-5">
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        {icon}
        <span>{label}</span>
      </div>
      <div className="mt-3 flex items-baseline gap-1.5">
        <span className="text-3xl font-semibold tabular-nums tracking-tight">{value}</span>
        {unit && <span className="text-sm font-normal text-muted-foreground">{unit}</span>}
      </div>
    </div>
  );
}
