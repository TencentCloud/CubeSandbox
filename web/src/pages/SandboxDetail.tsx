// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useRef, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate, useParams, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { sandboxApi } from '@/api/client';
import { Card, CardTitle, CardDescription, CardHeader } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';

import { Skeleton } from '@/components/ui/skeleton';
import { ArrowLeft, Pause, Play, Trash2, RefreshCw } from 'lucide-react';
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

function formatLogTime(ts: string): string {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleTimeString(undefined, { hour12: false, fractionalSecondDigits: 3 });
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
  const tone = state === 'paused' || state === 'pausing' ? 'warn' : state === 'running' ? 'ok' : 'mute';
  const entries = logs.data?.logs ?? [];

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
            <Badge tone={tone as any}>{state}</Badge>
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

      {/* ── Info cards ── */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        {/* Resources */}
        <Card>
          <CardHeader>
            <CardTitle>{t('resources')}</CardTitle>
          </CardHeader>
          {isLoading ? (
            <Skeleton className="h-20 w-full" />
          ) : (
            <dl className="grid grid-cols-2 gap-3 text-sm">
              <Field label={t('fields.vcpu')} value={`${data?.cpuCount ?? '—'}`} />
              <Field label={t('fields.memory')} value={formatBytes(data?.memoryMB)} />
              <Field label={t('fields.client')} value={data?.clientID ?? '—'} />
              <Field label={t('fields.alias')} value={data?.alias ?? '—'} />
              <Field label={t('fields.started')} value={formatRelative(data?.startedAt)} />
              <Field label={t('fields.domain')} value={data?.domain ?? '—'} />
            </dl>
          )}
        </Card>

        {/* Runtime */}
        <Card>
          <CardHeader>
            <CardTitle>{t('runtime')}</CardTitle>
            <CardDescription>{t('runtimeDesc')}</CardDescription>
          </CardHeader>
          <ul className="space-y-2 text-sm">
            <li className="flex justify-between">
              <span className="text-muted-foreground">{t('fields.started')}</span>
              <span>{formatDateTime(data?.startedAt)}</span>
            </li>

            <li className="flex justify-between">
              <span className="text-muted-foreground">{t('fields.state')}</span>
              <span>{state}</span>
            </li>
            <li className="flex justify-between">
              <span className="text-muted-foreground">{t('fields.envd')}</span>
              <span>{data?.envdVersion ?? '—'}</span>
            </li>
          </ul>
        </Card>

        {/* Metadata */}
        <Card>
          <CardHeader>
            <CardTitle>{t('metadata')}</CardTitle>
          </CardHeader>
          <dl className="space-y-1 text-sm">
            {Object.entries(data?.metadata ?? {}).map(([k, v]) => (
              <div key={k} className="flex justify-between gap-3">
                <dt className="truncate text-muted-foreground">{k}</dt>
                <dd className="truncate font-mono text-xs">{v}</dd>
              </div>
            ))}
            {!data?.metadata || Object.keys(data.metadata).length === 0 ? (
              <div className="text-xs text-muted-foreground">{t('noMetadata')}</div>
            ) : null}
          </dl>
        </Card>
      </div>

      {/* ── Logs ── */}
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <div>
              <CardTitle>{t('logs')}</CardTitle>
              <CardDescription>
                {t('logsDesc')}
                {entries.length > 0 && (
                  <span className="ml-2 text-muted-foreground">({entries.length} {t('logsEntries')})</span>
                )}
              </CardDescription>
            </div>
            <Button
              size="icon"
              variant="ghost"
              title={t('logsRefresh')}
              onClick={() => logs.refetch()}
              disabled={logs.isFetching}
            >
              <RefreshCw size={14} className={cn(logs.isFetching && 'animate-spin')} />
            </Button>
          </div>
        </CardHeader>
        <pre
          ref={logRef}
          className="max-h-[400px] overflow-auto rounded-md bg-muted/60 p-3 font-mono text-xs leading-relaxed ring-1 ring-border/60"
        >
          {logs.isLoading ? (
            <span className="text-muted-foreground">{t('logsLoading')}</span>
          ) : entries.length === 0 ? (
            <span className="text-muted-foreground">{t('logsEmpty')}</span>
          ) : (
            entries.map((entry, i) => {
              const lvl = (entry.level ?? 'info').toLowerCase();
              const cls = LEVEL_CLASS[lvl] ?? 'text-foreground';
              return (
                <div key={i} className="flex gap-2">
                  <span className="shrink-0 text-muted-foreground/60">
                    {formatLogTime(entry.timestamp as unknown as string)}
                  </span>
                  <span className={cn('shrink-0 w-10 uppercase font-semibold', cls)}>
                    {lvl}
                  </span>
                  <span className={cn('break-all', cls)}>{entry.message}</span>
                </div>
              );
            })
          )}
        </pre>
      </Card>
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

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <dt className="text-xs uppercase tracking-wider text-muted-foreground">{label}</dt>
      <dd className={mono ? 'mt-0.5 truncate font-mono text-xs' : 'mt-0.5 truncate'}>{value}</dd>
    </div>
  );
}
