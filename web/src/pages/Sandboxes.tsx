// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useMemo, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { sandboxApi, type RunningSandbox } from '@/api/client';
import { Card, CardTitle, CardDescription, CardHeader } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { Pause, Play, Trash2, Search, Plus, Filter } from 'lucide-react';
import { formatBytes, formatRelative, short } from '@/lib/utils';

export default function SandboxesPage() {
  const [q, setQ] = useState('');
  const qc = useQueryClient();
  const { t } = useTranslation('sandboxes');

  const { data, isLoading } = useQuery({
    queryKey: ['sandboxes'],
    queryFn: () => sandboxApi.list(),
    refetchInterval: 5_000,
  });

  const killMut = useMutation({
    mutationFn: (id: string) => sandboxApi.kill(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['sandboxes'] }),
  });
  const pauseMut = useMutation({
    mutationFn: (id: string) => sandboxApi.pause(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['sandboxes'] }),
  });
  const resumeMut = useMutation({
    mutationFn: (id: string) => sandboxApi.resume(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['sandboxes'] }),
  });

  const filtered = useMemo(() => {
    if (!data) return [];
    if (!q.trim()) return data;
    const needle = q.toLowerCase();
    return data.filter((sb) =>
      [sb.sandboxID, sb.templateID, sb.alias, sb.clientID]
        .filter(Boolean)
        .some((v) => String(v).toLowerCase().includes(needle))
    );
  }, [data, q]);

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

      <Card className="!p-3">
        <div className="flex items-center gap-2">
          <div className="relative flex-1">
            <Search className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-muted-foreground" size={14} />
            <Input
              placeholder={t('filterPlaceholder')}
              value={q}
              onChange={(e) => setQ(e.target.value)}
              className="pl-9"
            />
          </div>
          <Button variant="outline" size="sm">
            <Filter size={14} /> {t('status')}
          </Button>
        </div>
      </Card>

      <Card className="!p-0 overflow-hidden">
        <div className="grid grid-cols-[120px_minmax(200px,1.2fr)_minmax(160px,1fr)_110px_120px_120px_120px] gap-2 border-b border-border/60 px-4 py-3 text-[11px] uppercase tracking-wider text-muted-foreground">
          <div>{t('col.state')}</div>
          <div>{t('col.sandboxId')}</div>
          <div>{t('col.template')}</div>
          <div>{t('col.cpu')}</div>
          <div>{t('col.memory')}</div>
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
            busy={killMut.isPending || pauseMut.isPending || resumeMut.isPending}
          />
        ))}
        {filtered.length === 0 && !isLoading && (
          <div className="py-16 text-center text-sm text-muted-foreground">
            {t('noMatch')}
          </div>
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
    <div className="grid grid-cols-[120px_minmax(200px,1.2fr)_minmax(160px,1fr)_110px_120px_120px_120px] gap-2 border-b border-border/60 px-4 py-3 text-sm transition hover:bg-muted/50">
      <div>
        <Badge tone={tone as any}>{state}</Badge>
      </div>
      <div className="flex flex-col">
        <Link to={`/sandboxes/${sb.sandboxID}`} className="font-mono text-xs text-foreground hover:text-primary">
          {short(sb.sandboxID)}
        </Link>
        {sb.alias && <span className="text-[11px] text-muted-foreground">{t('alias', { alias: sb.alias })}</span>}
      </div>
      <div className="truncate text-xs text-muted-foreground">{sb.templateID ?? '—'}</div>
      <div className="text-xs text-muted-foreground">{sb.cpuCount != null ? t('vcpu', { count: sb.cpuCount }) : '—'}</div>
      <div className="text-xs text-muted-foreground">{formatBytes(sb.memoryMB)}</div>
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
          <Trash2 size={14} className="text-cube-rose" />
        </Button>
      </div>
    </div>
  );
}
