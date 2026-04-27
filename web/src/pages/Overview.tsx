// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { clusterApi, sandboxApi, templateApi } from '@/api/client';
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { Link } from 'react-router-dom';
import { Boxes, Cpu, HardDrive, Package, Server, TrendingUp } from 'lucide-react';
import { formatRelative, short } from '@/lib/utils';

export default function OverviewPage() {
  const { t } = useTranslation('overview');
  const { t: tCommon } = useTranslation('common');

  const cluster = useQuery({ queryKey: ['cluster'], queryFn: clusterApi.overview, refetchInterval: 10_000 });
  const sandboxes = useQuery({ queryKey: ['sandboxes'], queryFn: () => sandboxApi.list(), refetchInterval: 5_000 });
  const templates = useQuery({ queryKey: ['templates'], queryFn: templateApi.list, refetchInterval: 30_000 });

  const running = sandboxes.data?.length ?? 0;
  const totalCpuMilli = cluster.data?.totalCpuMilli ?? 0;
  const cpuUsedMilli = Math.max(totalCpuMilli - (cluster.data?.allocatableCpuMilli ?? 0), 0);
  const cpuUsedPct = totalCpuMilli > 0 ? Math.round(Math.min((cpuUsedMilli / totalCpuMilli) * 100, 100)) : 0;
  const memUsedPct = cluster.data
    ? Math.round(((cluster.data.totalMemoryMB - cluster.data.allocatableMemoryMB) / Math.max(cluster.data.totalMemoryMB, 1)) * 100)
    : 0;

  return (
    <div className="animate-fade-in space-y-6">
      <header className="flex items-end justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t('subtitle')}</p>
        </div>
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <span className="inline-flex h-2 w-2 animate-pulse-soft rounded-full bg-cube-emerald" />
          {tCommon('live')}
        </div>
      </header>

      {/* KPI grid */}
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-4">
        <Kpi
          label={t('kpi.runningSandboxes')}
          icon={<Boxes size={16} />}
          tone="info"
          value={sandboxes.isLoading ? '—' : String(running)}
          hint={t('kpi.templatesAvailable', { count: templates.data?.length ?? 0 })}
        />
        <Kpi
          label={t('kpi.cpuUtilization')}
          icon={<Cpu size={16} />}
          tone="warn"
          value={cluster.isLoading ? '—' : `${cpuUsedPct}%`}
          hint={cluster.data
            ? t('kpi.coresUsed', {
                used: (cpuUsedMilli / 1000).toFixed(1),
                total: (totalCpuMilli / 1000).toFixed(1),
              })
            : ''}
          progress={cpuUsedPct}
        />
        <Kpi
          label={t('kpi.memoryUtilization')}
          icon={<HardDrive size={16} />}
          tone="info"
          value={cluster.isLoading ? '—' : `${memUsedPct}%`}
          hint={cluster.data
            ? t('kpi.memoryUsed', {
                used: (cluster.data.totalMemoryMB - cluster.data.allocatableMemoryMB) / 1024 | 0,
                total: (cluster.data.totalMemoryMB / 1024) | 0,
              })
            : ''}
          progress={memUsedPct}
        />
        <Kpi
          label={t('kpi.healthyNodes')}
          icon={<Server size={16} />}
          tone="ok"
          value={cluster.isLoading ? '—' : `${cluster.data?.healthyNodes ?? 0}/${cluster.data?.nodeCount ?? 0}`}
          hint={t('kpi.maxSlots', { count: cluster.data?.maxMvmSlots ?? 0 })}
        />
      </div>

      {/* Split: recent sandboxes / templates */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader>
            <div>
              <CardTitle>{t('recentSandboxes')}</CardTitle>
              <CardDescription>{t('recentSandboxesDesc')}</CardDescription>
            </div>
            <Link to="/sandboxes" className="text-xs text-primary hover:underline">
              {tCommon('viewAll')}
            </Link>
          </CardHeader>
          <div className="mt-2 divide-y divide-white/5">
            {sandboxes.isLoading &&
              Array.from({ length: 4 }).map((_, i) => (
                <div key={i} className="flex items-center gap-4 py-3">
                  <Skeleton className="h-6 w-24" />
                  <Skeleton className="h-4 w-40" />
                  <Skeleton className="ml-auto h-4 w-16" />
                </div>
              ))}
            {sandboxes.data?.slice(0, 6).map((sb) => (
              <Link
                key={sb.sandboxID}
                to={`/sandboxes/${sb.sandboxID}`}
                className="flex items-center gap-4 py-3 text-sm transition hover:bg-muted/50"
              >
                <Badge tone="info">{sb.state ?? 'running'}</Badge>
                <span className="font-mono text-xs text-foreground/80">{short(sb.sandboxID)}</span>
                <span className="text-muted-foreground">{sb.templateID ?? sb.alias ?? '—'}</span>
                <span className="ml-auto text-xs text-muted-foreground">
                  {formatRelative(sb.startedAt)}
                </span>
              </Link>
            ))}
            {sandboxes.data?.length === 0 && (
              <div className="py-10 text-center text-sm text-muted-foreground">{t('noSandboxes')}</div>
            )}
          </div>
        </Card>

        <Card>
          <CardHeader>
            <div>
              <CardTitle>{t('templatePipeline')}</CardTitle>
              <CardDescription>{t('templatePipelineDesc')}</CardDescription>
            </div>
            <Link to="/templates" className="text-xs text-primary hover:underline">
              {tCommon('manage')}
            </Link>
          </CardHeader>
          <div className="mt-2 space-y-2">
            {templates.isLoading &&
              Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-10 w-full" />)}
            {templates.data?.slice(0, 5).map((tpl) => (
              <Link
                key={tpl.templateID}
                to={`/templates/${tpl.templateID}`}
                className="flex items-center gap-3 rounded-lg border border-border/60 p-3 transition hover:border-primary/30"
              >
                <span className="flex h-8 w-8 items-center justify-center rounded-md bg-primary/10 text-primary">
                  <Package size={14} />
                </span>
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm font-medium">{tpl.templateID}</div>
                  <div className="truncate font-mono text-[11px] text-muted-foreground">{tpl.templateID}</div>
                </div>
                <Badge tone={tpl.status === 'ready' ? 'ok' : tpl.status === 'failed' ? 'err' : 'warn'}>
                  {tpl.version ?? tpl.status}
                </Badge>
              </Link>
            ))}
            {templates.data?.length === 0 && (
              <div className="py-6 text-center text-sm text-muted-foreground">{t('noTemplates')}</div>
            )}
          </div>
        </Card>
      </div>
    </div>
  );
}

function Kpi({
  label,
  value,
  hint,
  icon,
  tone,
  progress,
}: {
  label: string;
  value: string;
  hint?: string;
  icon: React.ReactNode;
  tone: 'ok' | 'warn' | 'err' | 'info';
  progress?: number;
}) {
  const { t: tCommon } = useTranslation('common');
  const barClass =
    tone === 'ok'
      ? 'from-cube-emerald/70 to-cube-emerald'
      : tone === 'warn'
      ? 'from-cube-amber/70 to-cube-amber'
      : tone === 'err'
      ? 'from-cube-rose/70 to-cube-rose'
      : 'from-primary/70 to-cube-violet';
  return (
    <Card>
      <div className="flex items-start justify-between">
        <div>
          <div className="text-xs uppercase tracking-wider text-muted-foreground">{label}</div>
          <div className="mt-2 text-3xl font-semibold tracking-tight">{value}</div>
          {hint && <div className="mt-1 text-xs text-muted-foreground">{hint}</div>}
        </div>
        <span className="flex h-8 w-8 items-center justify-center rounded-md bg-muted text-muted-foreground">
          {icon}
        </span>
      </div>
      {progress != null && (
        <div className="mt-4 h-1.5 overflow-hidden rounded-full bg-muted">
          <div
            className={`h-full bg-gradient-to-r ${barClass} transition-all`}
            style={{ width: `${Math.max(2, Math.min(100, progress))}%` }}
          />
        </div>
      )}
      {progress == null && (
        <div className="mt-4 flex items-center gap-1 text-[11px] text-muted-foreground">
          <TrendingUp size={12} />
          <span>{tCommon('trendingStable')}</span>
        </div>
      )}
    </Card>
  );
}
