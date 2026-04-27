// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate, useParams, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { sandboxApi } from '@/api/client';
import { Card, CardTitle, CardDescription, CardHeader } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { ArrowLeft, Pause, Play, Trash2 } from 'lucide-react';
import { formatBytes, formatRelative } from '@/lib/utils';

export default function SandboxDetailPage() {
  const { sandboxID = '' } = useParams();
  const nav = useNavigate();
  const qc = useQueryClient();
  const { t } = useTranslation('sandboxDetail');

  const { data, isLoading } = useQuery({
    queryKey: ['sandbox', sandboxID],
    queryFn: () => sandboxApi.get(sandboxID),
    enabled: !!sandboxID,
    refetchInterval: 5_000,
  });
  const logs = useQuery({
    queryKey: ['sandbox-logs', sandboxID],
    queryFn: () => sandboxApi.logs(sandboxID),
    enabled: !!sandboxID,
    refetchInterval: 10_000,
  });

  const kill = useMutation({
    mutationFn: () => sandboxApi.kill(sandboxID),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sandboxes'] });
      nav('/sandboxes');
    },
  });
  const pause = useMutation({
    mutationFn: () => sandboxApi.pause(sandboxID),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sandboxes'] });
      qc.invalidateQueries({ queryKey: ['sandbox', sandboxID] });
    },
  });
  const resume = useMutation({
    mutationFn: () => sandboxApi.resume(sandboxID),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sandboxes'] });
      qc.invalidateQueries({ queryKey: ['sandbox', sandboxID] });
    },
  });

  const state = data?.state ?? 'running';
  const tone = state === 'paused' ? 'warn' : state === 'running' ? 'ok' : 'mute';

  return (
    <div className="animate-fade-in space-y-5">
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

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
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
              <Field label={t('fields.client')} value={data?.clientID ?? '—'} mono />
              <Field label={t('fields.alias')} value={data?.alias ?? '—'} />
              <Field label={t('fields.ends')} value={formatRelative(data?.endAt)} />
              <Field label={t('fields.domain')} value={data?.domain ?? '—'} mono />
            </dl>
          )}
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>{t('runtime')}</CardTitle>
            <CardDescription>{t('runtimeDesc')}</CardDescription>
          </CardHeader>
          <ul className="space-y-2 text-sm">
            <li className="flex justify-between"><span className="text-muted-foreground">{t('fields.started')}</span><span>{formatDateTime(data?.startedAt)}</span></li>
            <li className="flex justify-between"><span className="text-muted-foreground">{t('fields.ends')}</span><span>{formatDateTime(data?.endAt)}</span></li>
            <li className="flex justify-between"><span className="text-muted-foreground">{t('fields.state')}</span><span>{state}</span></li>
            <li className="flex justify-between"><span className="text-muted-foreground">{t('fields.envd')}</span><span>{data?.envdVersion ?? '—'}</span></li>
          </ul>
        </Card>

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

      <Card>
        <CardHeader>
          <CardTitle>{t('logs')}</CardTitle>
          <CardDescription>{t('logsDesc')}</CardDescription>
        </CardHeader>
        <pre className="max-h-[360px] overflow-auto rounded-md bg-muted/60 p-3 font-mono text-[11px] leading-relaxed text-muted-foreground ring-1 ring-border/60">
{logs.isLoading
  ? t('logsLoading')
  : (logs.data?.logs ?? [])
      .map((entry) => `[${entry.level}] ${entry.timestamp} ${entry.message}`)
      .join('\n')}
        </pre>
      </Card>
    </div>
  );
}

function formatDateTime(value?: string | null): string {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat(undefined, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(date);
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <dt className="text-[11px] uppercase tracking-wider text-muted-foreground">{label}</dt>
      <dd className={mono ? 'mt-0.5 truncate font-mono text-xs' : 'mt-0.5 truncate'}>{value}</dd>
    </div>
  );
}
