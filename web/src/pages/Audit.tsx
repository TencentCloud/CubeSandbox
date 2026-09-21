// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Search, ScrollText, RefreshCw } from 'lucide-react';
import { auditApi, type AuditSandboxRow } from '@/api/audit';
import { Card } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { Pagination } from '@/components/ui/pagination';
import { cn, short } from '@/lib/utils';

const DAY_OPTIONS = [1, 3, 7, 30] as const;
const PAGE_SIZE = 50;

export function formatDuration(sec?: number | null): string {
  if (sec == null) return '—';
  if (sec < 60) return `${sec}s`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ${sec % 60}s`;
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  return `${h}h ${m}m`;
}

export default function AuditPage() {
  const { t } = useTranslation('audit');
  const [params, setParams] = useSearchParams();
  const days = Number(params.get('days') ?? 3);
  const template = params.get('template') ?? '';
  const keyName = params.get('key') ?? '';
  const page = Number(params.get('page') ?? 1);
  const [q, setQ] = useState(params.get('q') ?? '');
  const [debouncedQ, setDebouncedQ] = useState(q);

  useEffect(() => {
    const h = setTimeout(() => setDebouncedQ(q.trim()), 300);
    return () => clearTimeout(h);
  }, [q]);

  const setParam = (patch: Record<string, string | number | undefined>) => {
    const next = new URLSearchParams(params);
    for (const [k, v] of Object.entries(patch)) {
      if (v === undefined || v === '' || v === null) next.delete(k);
      else next.set(k, String(v));
    }
    setParams(next, { replace: true });
  };

  useEffect(() => {
    if ((params.get('q') ?? '') !== debouncedQ) setParam({ q: debouncedQ, page: 1 });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [debouncedQ]);

  const { data, isLoading, isFetching, error, refetch } = useQuery({
    queryKey: ['audit', days, template, keyName, debouncedQ, page],
    queryFn: () => auditApi.list({ days, template, key: keyName, q: debouncedQ, page, size: PAGE_SIZE }),
    refetchInterval: 15_000,
  });

  const stats = useMemo(
    () => [
      { label: t('stats.sandboxes'), value: data?.sandboxes ?? '—' },
      { label: t('stats.killed'), value: data?.killed ?? '—' },
      { label: t('stats.requests'), value: data?.events ?? '—' },
      { label: t('stats.days'), value: data ? data.files.length : '—' },
    ],
    [data, t],
  );

  return (
    <div className="animate-fade-in space-y-5">
      <header className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t('subtitle')}</p>
        </div>
        <Button variant="outline" onClick={() => void refetch()} disabled={isFetching}>
          <RefreshCw size={14} className={cn(isFetching && 'animate-spin')} /> {t('refresh')}
        </Button>
      </header>

      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        {stats.map((s) => (
          <Card key={s.label} className="!p-4">
            <div className="text-xs uppercase tracking-wider text-muted-foreground/85">{s.label}</div>
            <div className="mt-1 text-2xl font-semibold text-num">{s.value}</div>
          </Card>
        ))}
      </div>

      <Card className="!p-3">
        <div className="flex flex-wrap items-center gap-2">
          <div className="relative min-w-[240px] flex-1">
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
          <div className="flex items-center gap-1 rounded-lg border border-border/60 bg-muted/40 p-1">
            {DAY_OPTIONS.map((d) => (
              <button
                key={d}
                onClick={() => setParam({ days: d, page: 1 })}
                className={cn(
                  'rounded-md px-3 py-1 text-xs font-medium transition-all',
                  days === d
                    ? 'bg-background text-foreground shadow-sm ring-1 ring-border/60'
                    : 'text-muted-foreground hover:text-foreground',
                )}
              >
                {t('range', { count: d })}
              </button>
            ))}
          </div>
          <select
            value={template}
            onChange={(e) => setParam({ template: e.target.value, page: 1 })}
            className="h-9 rounded-md border border-border/60 bg-background px-2 text-sm text-foreground"
          >
            <option value="">{t('allTemplates')}</option>
            {(data?.templates ?? []).map((tpl) => (
              <option key={tpl} value={tpl}>
                {tpl}
              </option>
            ))}
          </select>
          <select
            value={keyName}
            onChange={(e) => setParam({ key: e.target.value, page: 1 })}
            className="h-9 rounded-md border border-border/60 bg-background px-2 text-sm text-foreground"
          >
            <option value="">{t('allKeys')}</option>
            {(data?.keys ?? []).map((k) => (
              <option key={k} value={k}>
                {k}
              </option>
            ))}
          </select>
        </div>
      </Card>

      {error && (
        <Card className="!p-4 text-sm text-cube-err">
          {t('loadError')}: {String((error as Error).message ?? error)}
        </Card>
      )}

      <Card className="!p-0 overflow-hidden">
        <div className="grid grid-cols-[150px_140px_minmax(140px,1fr)_100px_120px_90px_90px_70px_70px_minmax(200px,1.4fr)] gap-2 border-b border-border/60 px-4 py-3 text-xs uppercase tracking-wider font-medium text-muted-foreground/85">
          <div>{t('col.created')}</div>
          <div>{t('col.sandbox')}</div>
          <div>{t('col.template')}</div>
          <div>{t('col.key')}</div>
          <div>{t('col.client')}</div>
          <div>{t('col.state')}</div>
          <div>{t('col.duration')}</div>
          <div className="text-right">{t('col.commands')}</div>
          <div className="text-right">{t('col.files')}</div>
          <div>{t('col.firstCommand')}</div>
        </div>
        {isLoading &&
          Array.from({ length: 8 }).map((_, i) => (
            <div key={i} className="border-b border-border/60 px-4 py-3">
              <Skeleton className="h-5 w-full" />
            </div>
          ))}
        {data?.items.map((row) => <Row key={row.id} row={row} days={days} />)}
        {data && data.items.length === 0 && (
          <div className="flex flex-col items-center gap-2 py-16 text-sm text-muted-foreground">
            <ScrollText size={28} strokeWidth={1.25} />
            {t('empty')}
          </div>
        )}
      </Card>

      {data && data.total > PAGE_SIZE && (
        <Pagination
          page={page}
          pageSize={PAGE_SIZE}
          total={data.total}
          onPage={(p) => setParam({ page: p })}
          totalLabel={t('pagination.total', { count: data.total })}
          prevLabel={t('pagination.prev')}
          nextLabel={t('pagination.next')}
          jumpLabel={t('pagination.jump')}
        />
      )}
    </div>
  );
}

function Row({ row, days }: { row: AuditSandboxRow; days: number }) {
  const { t } = useTranslation('audit');
  const meta = row.metadata ? Object.entries(row.metadata) : [];
  return (
    <Link
      to={`/audit/${row.id}?days=${days}`}
      className="grid grid-cols-[150px_140px_minmax(140px,1fr)_100px_120px_90px_90px_70px_70px_minmax(200px,1.4fr)] items-center gap-2 border-b border-border/60 px-4 py-2.5 text-sm transition-colors hover:bg-muted/40"
    >
      <div className="text-num text-xs text-muted-foreground">{row.createdAt ?? row.firstSeen}</div>
      <div className="font-mono text-xs" title={row.id}>
        {short(row.id, 8, 4)}
      </div>
      <div className="truncate">
        <span className="truncate">{row.template ?? '—'}</span>
        {meta.length > 0 && (
          <div className="mt-0.5 flex flex-wrap gap-1">
            {meta.slice(0, 3).map(([k, v]) => (
              <span key={k} className="rounded bg-muted/60 px-1 text-[10px] text-muted-foreground" title={`${k}=${v}`}>
                {k}={String(v).slice(0, 18)}
              </span>
            ))}
          </div>
        )}
      </div>
      <div className="truncate text-xs">{row.keyName ?? '—'}</div>
      <div className="font-mono text-xs">{row.client ?? '—'}</div>
      <div>
        <Badge tone={row.state === 'killed' ? 'mute' : 'ok'}>{t(`state.${row.state}`)}</Badge>
      </div>
      <div className="text-num text-xs">{formatDuration(row.durationSec)}</div>
      <div className="text-num text-right text-xs">{row.commands}</div>
      <div className="text-num text-right text-xs">{row.files}</div>
      <div className="truncate font-mono text-xs text-muted-foreground" title={row.firstCommand ?? ''}>
        {row.firstCommand ?? '—'}
      </div>
    </Link>
  );
}
