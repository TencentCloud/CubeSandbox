// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { ArrowLeft, Copy, Terminal, FileText, Play, Square, Radio, Keyboard } from 'lucide-react';
import { auditApi, type AuditTimelineItem } from '@/api/audit';
import { Card, CardHeader, CardTitle, CardContent } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { cn, copyToClipboard } from '@/lib/utils';
import { formatDuration } from '@/pages/Audit';

type Kind = AuditTimelineItem['kind'];
type KindFilter = 'all' | 'command' | 'file' | 'lifecycle';

const KIND_ICON: Record<Kind, typeof Terminal> = {
  create: Play,
  kill: Square,
  api: Radio,
  command: Terminal,
  stdin: Keyboard,
  signal: Radio,
  file: FileText,
  process: Radio,
};

function kindMatches(kind: Kind, f: KindFilter): boolean {
  if (f === 'all') return true;
  if (f === 'command') return kind === 'command' || kind === 'stdin' || kind === 'signal' || kind === 'process';
  if (f === 'file') return kind === 'file';
  return kind === 'create' || kind === 'kill' || kind === 'api';
}

export default function AuditDetailPage() {
  const { sandboxID = '' } = useParams();
  const [params] = useSearchParams();
  const days = Number(params.get('days') ?? 3);
  const { t } = useTranslation('audit');
  const [kind, setKind] = useState<KindFilter>('all');

  const { data, isLoading, error } = useQuery({
    queryKey: ['audit', 'detail', sandboxID, days],
    queryFn: () => auditApi.detail(sandboxID, days),
  });

  const timeline = useMemo(() => (data?.timeline ?? []).filter((i) => kindMatches(i.kind, kind)), [data, kind]);

  const KIND_TABS: { key: KindFilter; label: string; n?: number }[] = [
    { key: 'all', label: t('detail.kinds.all'), n: data?.timeline.length },
    { key: 'command', label: t('detail.kinds.command'), n: data?.commands },
    { key: 'file', label: t('detail.kinds.file'), n: data?.files },
    { key: 'lifecycle', label: t('detail.kinds.lifecycle') },
  ];

  return (
    <div className="animate-fade-in space-y-5">
      <header className="flex items-center gap-3">
        <Link to={`/audit?days=${days}`}>
          <Button variant="ghost" size="icon">
            <ArrowLeft size={16} />
          </Button>
        </Link>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <h1 className="truncate font-mono text-xl font-medium tracking-tight">{sandboxID}</h1>
            <button
              className="text-muted-foreground hover:text-foreground"
              onClick={() => copyToClipboard(sandboxID, t('detail.copied'))}
              title={t('detail.copy')}
            >
              <Copy size={14} />
            </button>
            {data && <Badge tone={data.state === 'killed' ? 'mute' : 'ok'}>{t(`state.${data.state}`)}</Badge>}
          </div>
          <p className="mt-1 text-sm text-muted-foreground">{t('detail.subtitle')}</p>
        </div>
      </header>

      {error && (
        <Card className="!p-4 text-sm text-cube-err">
          {t('loadError')}: {String((error as Error).message ?? error)}
        </Card>
      )}

      <Card>
        <CardHeader>
          <CardTitle>{t('detail.summary')}</CardTitle>
        </CardHeader>
        <CardContent>
          {isLoading && <Skeleton className="h-24 w-full" />}
          {data && (
            <dl className="grid grid-cols-2 gap-x-6 gap-y-3 text-sm md:grid-cols-4">
              <Field label={t('col.template')} value={data.template ?? '—'} />
              <Field label={t('col.client')} value={data.client ?? '—'} mono />
              <Field label={t('col.created')} value={data.createdAt ?? data.firstSeen} mono />
              <Field label={t('detail.ended')} value={data.killedAt ?? `${data.lastSeen} (${t('detail.lastSeen')})`} mono />
              <Field label={t('col.duration')} value={formatDuration(data.durationSec)} mono />
              <Field label={t('detail.timeout')} value={data.timeout != null ? `${data.timeout}s` : '—'} mono />
              <Field label={t('detail.internet')} value={data.allowInternetAccess == null ? '—' : data.allowInternetAccess ? t('detail.yes') : t('detail.no')} />
              <Field label={t('detail.requests')} value={`${data.requests} · ${data.commands} ${t('detail.kinds.command')} · ${data.files} ${t('detail.kinds.file')}`} mono />
              <div className="col-span-2 md:col-span-4">
                <dt className="text-xs uppercase tracking-wider text-muted-foreground/85">{t('detail.metadata')}</dt>
                <dd className="mt-1 flex flex-wrap gap-1.5">
                  {data.metadata && Object.keys(data.metadata).length > 0 ? (
                    Object.entries(data.metadata).map(([k, v]) => (
                      <span key={k} className="rounded bg-muted/60 px-1.5 py-0.5 font-mono text-xs">
                        {k}={String(v)}
                      </span>
                    ))
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )}
                </dd>
              </div>
              {data.network != null && (
                <div className="col-span-2 md:col-span-4">
                  <dt className="text-xs uppercase tracking-wider text-muted-foreground/85">{t('detail.network')}</dt>
                  <dd className="mt-1 overflow-auto rounded-md bg-muted/60 p-2 font-mono text-xs ring-1 ring-border/60">
                    {JSON.stringify(data.network)}
                  </dd>
                </div>
              )}
            </dl>
          )}
        </CardContent>
      </Card>

      <Card className="!p-0 overflow-hidden">
        <div className="flex items-center justify-between border-b border-border/60 px-4 py-3">
          <h2 className="text-sm font-medium">{t('detail.timeline')}</h2>
          <div className="flex items-center gap-1 rounded-lg border border-border/60 bg-muted/40 p-1">
            {KIND_TABS.map(({ key, label, n }) => (
              <button
                key={key}
                onClick={() => setKind(key)}
                className={cn(
                  'rounded-md px-3 py-1 text-xs font-medium transition-all',
                  kind === key
                    ? 'bg-background text-foreground shadow-sm ring-1 ring-border/60'
                    : 'text-muted-foreground hover:text-foreground',
                )}
              >
                {label}
                {n != null && <span className="ml-1.5 text-num text-muted-foreground">{n}</span>}
              </button>
            ))}
          </div>
        </div>
        {isLoading &&
          Array.from({ length: 6 }).map((_, i) => (
            <div key={i} className="border-b border-border/60 px-4 py-3">
              <Skeleton className="h-5 w-full" />
            </div>
          ))}
        {timeline.map((item, i) => (
          <TimelineRow key={i} item={item} />
        ))}
        {data && timeline.length === 0 && (
          <div className="py-12 text-center text-sm text-muted-foreground">{t('empty')}</div>
        )}
      </Card>
    </div>
  );
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs uppercase tracking-wider text-muted-foreground/85">{label}</dt>
      <dd className={cn('mt-0.5 truncate', mono && 'font-mono text-xs')} title={value}>
        {value}
      </dd>
    </div>
  );
}

function StatusChip({ status }: { status: number }) {
  const tone = status >= 500 ? 'err' : status >= 400 ? 'warn' : 'ok';
  return (
    <Badge tone={tone} className="text-num">
      {status}
    </Badge>
  );
}

function TimelineRow({ item }: { item: AuditTimelineItem }) {
  const { t } = useTranslation('audit');
  const Icon = KIND_ICON[item.kind];
  const time = item.ts.slice(11);
  let body: React.ReactNode;
  switch (item.kind) {
    case 'command':
      body = (
        <div className="min-w-0">
          <code className="block whitespace-pre-wrap break-all font-mono text-xs">{item.cmd}</code>
          <div className="mt-0.5 text-xs text-muted-foreground">
            {item.cwd && <span className="mr-3">cwd {item.cwd}</span>}
            {item.user && <span className="mr-3">user {item.user}</span>}
            {item.pty && <span className="mr-3">pty</span>}
            {item.envKeys.length > 0 && <span>env {item.envKeys.join(', ')}</span>}
          </div>
        </div>
      );
      break;
    case 'file':
      body = (
        <div className="min-w-0 font-mono text-xs">
          <span className="mr-2 rounded bg-muted/60 px-1">{item.op}</span>
          <span className="break-all">{item.path ?? ''}</span>
          {item.user && <span className="ml-2 text-muted-foreground">user {item.user}</span>}
          {item.op === 'write' && item.reqBytes != null && (
            <span className="ml-2 text-muted-foreground">{item.reqBytes} B</span>
          )}
          {item.op === 'read' && item.respBytes != null && (
            <span className="ml-2 text-muted-foreground">{item.respBytes} B</span>
          )}
        </div>
      );
      break;
    case 'create':
      body = (
        <div className="text-xs">
          <span className="font-medium">{t('detail.events.create')}</span>
          <span className="ml-2 text-muted-foreground">
            {item.template} {item.timeout != null && `· timeout ${item.timeout}s`}
          </span>
        </div>
      );
      break;
    case 'kill':
      body = <div className="text-xs font-medium">{t('detail.events.kill')}</div>;
      break;
    case 'stdin':
      body = (
        <div className="min-w-0 font-mono text-xs">
          <span className="mr-2 rounded bg-muted/60 px-1">stdin</span>
          <span className="break-all text-muted-foreground">{item.body ?? ''}</span>
        </div>
      );
      break;
    case 'api':
      body = (
        <div className="font-mono text-xs text-muted-foreground">
          {item.method} {item.path}
          {item.query ? `?${item.query}` : ''}
        </div>
      );
      break;
    default:
      body = (
        <div className="font-mono text-xs text-muted-foreground">
          {item.kind} {'op' in item ? item.op : ''}
        </div>
      );
  }
  const latency = 'latencyMs' in item && item.latencyMs != null ? `${item.latencyMs}ms` : '';
  return (
    <div className="grid grid-cols-[90px_28px_minmax(0,1fr)_70px_70px] items-start gap-2 border-b border-border/60 px-4 py-2 text-sm">
      <div className="text-num text-xs text-muted-foreground">{time}</div>
      <Icon size={14} strokeWidth={1.75} className="mt-0.5 text-muted-foreground" />
      {body}
      <div className="text-num text-right text-xs text-muted-foreground">{latency}</div>
      <div className="text-right">
        <StatusChip status={item.status} />
      </div>
    </div>
  );
}
