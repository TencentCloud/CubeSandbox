// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useMemo, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { sandboxApi, type RunningSandbox } from '@/api/client';
import { Card } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { Pause, Play, Trash2, Search, Plus } from 'lucide-react';
import { formatBytes, formatRelative, short } from '@/lib/utils';
import { cn } from '@/lib/utils';
import { formatSandboxActionError } from '@/lib/sandboxActionError';
import { SandboxActionErrorBanner } from '@/components/SandboxActionErrorBanner';

type StateFilter = 'all' | 'running' | 'paused';

export default function SandboxesPage() {
  const [q, setQ] = useState('');
  const [stateFilter, setStateFilter] = useState<StateFilter>('all');
  const qc = useQueryClient();
  const { t } = useTranslation('sandboxes');

  const { data, isLoading } = useQuery({
    queryKey: ['sandboxes', stateFilter],
    queryFn: () =>
      sandboxApi.list({ state: stateFilter === 'all' ? undefined : stateFilter }),
    refetchInterval: 5_000,
  });

  const [pendingId, setPendingId] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const onLifecycleError = (err: unknown) => {
    setActionError(formatSandboxActionError(err, t));
  };

  const killMut = useMutation({
    mutationFn: (id: string) => { setPendingId(id); return sandboxApi.kill(id); },
    onMutate: () => setActionError(null),
    onError: onLifecycleError,
    onSettled: () => { setPendingId(null); qc.invalidateQueries({ queryKey: ['sandboxes'] }); },
  });
  const pauseMut = useMutation({
    mutationFn: (id: string) => { setPendingId(id); return sandboxApi.pause(id); },
    onMutate: () => setActionError(null),
    onError: onLifecycleError,
    onSettled: () => { setPendingId(null); qc.invalidateQueries({ queryKey: ['sandboxes'] }); },
  });
  const resumeMut = useMutation({
    mutationFn: (id: string) => { setPendingId(id); return sandboxApi.resume(id); },
    onMutate: () => setActionError(null),
    onError: onLifecycleError,
    onSettled: () => { setPendingId(null); qc.invalidateQueries({ queryKey: ['sandboxes'] }); },
  });

  const filtered = useMemo(() => {
    if (!data) return [];
    if (!q.trim()) return data;
    const needle = q.toLowerCase();
    return data.filter((sb) =>
      [sb.sandboxID, sb.templateID, sb.alias, sb.clientID]
        .filter(Boolean)
        .some((v) => String(v).toLowerCase().includes(needle)),
    );
  }, [data, q]);

  const STATE_TABS: { key: StateFilter; label: string }[] = [
    { key: 'all', label: t('filter.all') },
    { key: 'running', label: t('filter.running') },
    { key: 'paused', label: t('filter.paused') },
  ];

  return (
    <div className="animate-fade-in space-y-5">
      <header className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t('subtitle')}</p>
        </div>
        <Link to="/sandboxes/new">
          <Button>
            <Plus size={14} /> {t('newSandbox')}
          </Button>
        </Link>
      </header>

      <SandboxActionErrorBanner message={actionError} onDismiss={() => setActionError(null)} />

      <Card className="!p-3">
        <div className="flex items-center gap-2">
          <div className="relative flex-1">
            <Search
              className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground"
              size={14}
            />
            <Input
              placeholder={t('filterPlaceholder')}
              value={q}
              onChange={(e) => setQ(e.target.value)}
              className="pl-9"
            />
          </div>
          {/* State filter tabs */}
          <div className="flex items-center rounded-lg border border-border/60 bg-muted/40 p-1 gap-1">
            {STATE_TABS.map(({ key, label }) => (
              <button
                key={key}
                onClick={() => setStateFilter(key)}
                className={cn(
                  'rounded-md px-3 py-1 text-xs font-medium transition-all',
                  stateFilter === key
                    ? 'bg-background text-foreground shadow-sm ring-1 ring-border/60'
                    : 'text-muted-foreground hover:text-foreground',
                )}
              >
                {label}
                {/* 显示对应状态的数量角标 */}
                {key !== 'all' && data && (
                  <span
                    className={cn(
                      'ml-1.5 rounded-full px-1.5 py-0.5 text-xs text-num',
                      key === 'running'
                        ? 'bg-cube-ok/20 text-cube-ok'
                        : 'bg-cube-warn/20 text-cube-warn',
                    )}
                  >
                    {data.filter((sb) => (sb.state ?? 'running') === key).length}
                  </span>
                )}
              </button>
            ))}
          </div>
        </div>
      </Card>

      <Card className="!p-0 overflow-hidden">
        <div className="grid grid-cols-[120px_minmax(200px,1.2fr)_minmax(160px,1fr)_110px_120px_130px_120px_120px] gap-2 border-b border-border/60 px-4 py-3 text-xs uppercase tracking-wider font-medium text-muted-foreground/85">
          <div>{t('col.state')}</div>
          <div>{t('col.sandboxId')}</div>
          <div>{t('col.template')}</div>
          <div>{t('col.cpu')}</div>
          <div>{t('col.memory')}</div>
          <div>{t('col.node')}</div>
          <div>{t('col.started')}</div>
          <div className="text-right">{t('col.actions')}</div>
        </div>
        {isLoading &&
          Array.from({ length: 6 }).map((_, i) => (
            <div key={i} className="border-b border-border/60 px-4 py-3">
              <Skeleton className="h-5 w-full" />
            </div>
          ))}
        {filtered.map((sb) => (
          <Row
            key={sb.sandboxID}
            sb={sb}
            onKill={() => killMut.mutate(sb.sandboxID)}
            onPause={() => pauseMut.mutate(sb.sandboxID)}
            onResume={() => resumeMut.mutate(sb.sandboxID)}
            busy={pendingId === sb.sandboxID}
          />
        ))}
        {filtered.length === 0 && !isLoading && (
          <div className="py-16 text-center text-sm text-muted-foreground">{t('noMatch')}</div>
        )}
      </Card>
    </div>
  );
}

function Row({
  sb,
  onKill,
  onPause,
  onResume,
  busy,
}: {
  sb: RunningSandbox;
  onKill: () => void;
  onPause: () => void;
  onResume: () => void;
  busy: boolean;
}) {
  const { t } = useTranslation('sandboxes');
  const state = sb.state ?? 'running';
  const tone = state === 'paused' ? 'warn' : state === 'running' ? 'ok' : 'mute';
  return (
    <div className="grid grid-cols-[120px_minmax(200px,1.2fr)_minmax(160px,1fr)_110px_120px_130px_120px_120px] gap-2 border-b border-border/60 px-4 py-3 text-sm transition hover:bg-muted/50">
      <div>
        <Badge tone={tone as any}>{state}</Badge>
      </div>
      <div className="flex flex-col">
        <Link
          to={`/sandboxes/${sb.sandboxID}`}
          className="font-mono text-xs text-foreground hover:text-primary"
        >
          {short(sb.sandboxID)}
        </Link>
        {sb.alias && (
          <span className="text-xs text-muted-foreground">{t('alias', { alias: sb.alias })}</span>
        )}
      </div>
      <div className="truncate text-xs text-muted-foreground">{sb.templateID ?? '—'}</div>
      <div className="text-xs text-muted-foreground text-num">
        {sb.cpuCount != null ? t('vcpu', { count: sb.cpuCount }) : '—'}
      </div>
      <div className="text-xs text-muted-foreground text-num">{formatBytes(sb.memoryMB)}</div>
      <div className="text-xs text-muted-foreground/80 text-num">{sb.clientID || '—'}</div>
      <div className="text-xs text-muted-foreground">{formatRelative(sb.startedAt)}</div>
      <div className="flex justify-end gap-1">
        {state === 'paused' ? (
          <Button size="icon" variant="ghost" title={t('actions.resume')} onClick={onResume} disabled={busy}>
            <Play size={14} />
          </Button>
        ) : (
          <Button size="icon" variant="ghost" title={t('actions.pause')} onClick={onPause} disabled={busy}>
            <Pause size={14} />
          </Button>
        )}
        <Button size="icon" variant="ghost" title={t('actions.kill')} onClick={onKill} disabled={busy}>
          <Trash2 size={14} className="text-cube-err" />
        </Button>
      </div>
    </div>
  );
}
