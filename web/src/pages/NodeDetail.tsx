// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useQuery } from '@tanstack/react-query';
import { Link, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { clusterApi, sandboxApi, templateApi } from '@/api/client';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { ArrowLeft, Package, Box, Activity } from 'lucide-react';
import { cn, formatRelative } from '@/lib/utils';

// ── Resource bar ──────────────────────────────────────────────────────────────

function ResourceBar({ pct }: { pct: number }) {
  const color =
    pct > 85 ? 'bg-cube-err' :
    pct > 65 ? 'bg-cube-warn' :
    'bg-cube-ok';
  return (
    <div className="h-1 w-full rounded-full bg-white/8 overflow-hidden">
      <div
        className={cn('h-full rounded-full transition-all duration-700', color)}
        style={{ width: `${Math.max(1, Math.min(100, pct))}%` }}
      />
    </div>
  );
}

// ── KPI card ──────────────────────────────────────────────────────────────────

function KpiCard({
  label,
  pct,
  used,
  total,
  unit,
}: {
  label: string;
  pct: number;
  used: string;
  total: string;
  unit: string;
}) {
  const color =
    pct > 85 ? 'text-cube-err' :
    pct > 65 ? 'text-cube-warn' :
    'text-foreground';

  return (
    <div className="rounded-xl border border-border/60 bg-card/40 p-4 space-y-3">
      <div className="text-xs text-muted-foreground tracking-wider uppercase font-medium">{label}</div>
      <div className="flex items-end justify-between gap-2">
        <span className={cn('text-3xl font-semibold tabular-nums leading-none', color)}>
          {pct}<span className="text-base font-normal text-muted-foreground ml-0.5">%</span>
        </span>
        <span className="text-sm text-muted-foreground pb-0.5 text-num">
          {used} / {total} {unit}
        </span>
      </div>
      <ResourceBar pct={pct} />
    </div>
  );
}

// ── Section wrapper ───────────────────────────────────────────────────────────

function Section({ title, children, action }: { title: string; children: React.ReactNode; action?: React.ReactNode }) {
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-medium uppercase tracking-wider text-muted-foreground">{title}</span>
        {action}
      </div>
      {children}
    </div>
  );
}

// ── Stat row ──────────────────────────────────────────────────────────────────

function StatRow({ label, value, mono }: { label: string; value?: string | null; mono?: boolean }) {
  return (
    <div className="flex items-center justify-between py-2 border-b border-white/5 last:border-0">
      <span className="text-sm text-muted-foreground">{label}</span>
      <span className={cn('text-sm text-foreground/90', mono && 'font-mono')}>{value ?? '—'}</span>
    </div>
  );
}

// ── Condition row ─────────────────────────────────────────────────────────────

function ConditionRow({ type, status, reason, message, time }: {
  type: string; status: string; reason?: string; message?: string; time?: string | null;
}) {
  const ok = status === 'True';
  return (
    <div className="flex items-start justify-between gap-4 py-2.5 border-b border-white/5 last:border-0">
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className={cn('inline-block h-1.5 w-1.5 rounded-full shrink-0', ok ? 'bg-cube-ok' : 'bg-cube-warn')} />
          <span className="text-base font-medium">{type}</span>
          <span className={cn('text-xs font-medium px-1.5 py-0.5 rounded border',
            ok ? 'text-cube-ok border-cube-ok/30 bg-cube-ok/5' : 'text-cube-warn border-cube-warn/30 bg-cube-warn/5'
          )}>{status}</span>
        </div>
        {reason && <p className="mt-0.5 text-sm text-muted-foreground pl-3.5">{reason}</p>}
        {message && <p className="mt-0.5 text-xs text-muted-foreground/60 break-all pl-3.5">{message}</p>}
      </div>
      {time && <span className="shrink-0 text-xs text-muted-foreground/60 mt-0.5">{formatRelative(time)}</span>}
    </div>
  );
}

// ── Main page ─────────────────────────────────────────────────────────────────

export default function NodeDetailPage() {
  const { nodeID } = useParams<{ nodeID: string }>();
  const { t } = useTranslation('nodeDetail');

  const { data, isLoading, isError } = useQuery({
    queryKey: ['node', nodeID],
    queryFn: () => clusterApi.node(nodeID!),
    enabled: !!nodeID,
    refetchInterval: 10_000,
  });

  const { data: allSandboxes, isLoading: sandboxesLoading } = useQuery({
    queryKey: ['sandboxes'],
    queryFn: () => sandboxApi.list(),
    refetchInterval: 10_000,
    enabled: !!data,
  });

  const { data: allTemplates } = useQuery({
    queryKey: ['templates'],
    queryFn: () => templateApi.list(),
    staleTime: 30_000,
    enabled: !!data,
  });

  // local templates with READY or RUNNING status only
  const localTemplateIDs = new Set(data?.localTemplates ?? []);
  const visibleLocalTemplates = (allTemplates ?? [])
    .filter(t => localTemplateIDs.has(t.templateID) &&
      ['READY', 'RUNNING'].includes((t.status ?? '').toUpperCase()));

  const nodeSandboxes = (allSandboxes ?? []).filter(
    (sb) => sb.clientID === data?.address,
  );

  if (isLoading) {
    return (
      <div className="space-y-6">
        <Skeleton className="h-5 w-32" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }

  if (isError || !data) {
    return (
      <div>
        <Link to="/nodes" className="flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground mb-6">
          <ArrowLeft className="h-4 w-4" /> {t('backToNodes')}
        </Link>
        <p className="text-sm text-muted-foreground">{t('notFound')}</p>
      </div>
    );
  }

  const cpuUsed = data.resources.totalCpuMilli - data.resources.allocatableCpuMilli;
  const memUsed = data.resources.totalMemoryMB - data.resources.allocatableMemoryMB;

  const isReady = data.status.toLowerCase() === 'ready';

  return (
    <div className="animate-fade-in space-y-8">
      {/* back */}
      <Link to="/nodes" className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors">
        <ArrowLeft className="h-4 w-4" /> {t('backToNodes')}
      </Link>

      {/* header */}
      <div className="flex items-start justify-between gap-4">
        <div className="space-y-1.5">
          <div className="flex items-center gap-2.5">
            {/* live indicator */}
            <span className="relative flex h-2 w-2">
              {isReady && <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-cube-ok opacity-60" />}
              <span className={cn('relative inline-flex rounded-full h-2 w-2', isReady ? 'bg-cube-ok' : 'bg-cube-warn')} />
            </span>
            <h1 className="text-2xl font-semibold tracking-tight">{data.hostname ?? data.nodeID}</h1>
          </div>
          <div className="flex items-center gap-3 pl-4.5">
            <span className="font-mono text-sm text-muted-foreground/70">{data.nodeID}</span>
            {data.role && (
              <>
                <span className="text-muted-foreground/30">·</span>
                <span className="text-sm text-muted-foreground">{data.role}</span>
              </>
            )}
            {data.address && (
              <>
                <span className="text-muted-foreground/30">·</span>
                <span className="text-sm text-muted-foreground text-num">{data.address}</span>
              </>
            )}
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0 pt-1">
          <Activity size={13} className="text-muted-foreground/50" />
          <span className="text-sm text-muted-foreground">{formatRelative(data.heartbeatTime)}</span>
        </div>
      </div>

      {/* resource KPIs */}
      <Section title={t('section.resources')}>
        <div className="grid grid-cols-2 gap-3">
          <KpiCard
            label={t('fields.cpu')}
            pct={data.saturationPct}
            used={(cpuUsed / 1000).toFixed(1)}
            total={(data.resources.totalCpuMilli / 1000).toFixed(1)}
            unit="cores"
          />
          <KpiCard
            label={t('fields.memory')}
            pct={data.memorySaturationPct}
            used={(memUsed / 1024).toFixed(1)}
            total={(data.resources.totalMemoryMB / 1024).toFixed(1)}
            unit="GiB"
          />
        </div>

        {/* meta stats */}
        <div className="rounded-xl border border-border/60 bg-card/40 px-6 py-4 mt-1 grid grid-cols-3 divide-x divide-border/40">
          {[
            { label: t('fields.allocCpu'), value: `${(data.resources.allocatableCpuMilli / 1000).toFixed(1)}`, unit: 'cores' },
            { label: t('fields.allocMem'), value: `${(data.resources.allocatableMemoryMB / 1024).toFixed(1)}`, unit: 'GiB' },
            { label: t('fields.maxMvmSlots'), value: String(data.resources.maxMvmSlots), unit: '' },
          ].map(({ label, value, unit }) => (
            <div key={label} className="flex flex-col gap-1 px-5 first:pl-0 last:pr-0">
              <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium">{label}</span>
              <span className="text-xl font-semibold tabular-nums">
                {value}
                {unit && <span className="text-sm font-normal text-muted-foreground ml-1.5">{unit}</span>}
              </span>
            </div>
          ))}
        </div>
      </Section>

      {/* conditions */}
      {data.conditions && data.conditions.length > 0 && (
        <Section title={t('section.conditions')}>
          <div className="rounded-xl border border-border/60 bg-card/40 px-4 py-1">
            {data.conditions.map((c, i) => (
              <ConditionRow
                key={i}
                type={c.type}
                status={c.status}
                reason={c.reason}
                message={c.message}
                time={c.lastTransitionTime}
              />
            ))}
          </div>
        </Section>
      )}

      {/* component versions */}
      {data.versions && data.versions.length > 0 && (
        <Section title={t('section.versions')}>
          <div className="rounded-xl border border-border/60 bg-card/40 px-4 py-1">
            {data.versions.map((v) => (
              <div
                key={v.component}
                className="flex items-center justify-between gap-4 py-2.5 border-b border-white/5 last:border-0"
              >
                <span className="font-mono text-sm text-foreground/90">{v.component}</span>
                <div className="flex items-center gap-3 text-right">
                  <span className="font-mono text-sm text-foreground/80">{v.version || '—'}</span>
                  {v.commit && (
                    <span className="font-mono text-xs text-muted-foreground/60">{v.commit.slice(0, 12)}</span>
                  )}
                </div>
              </div>
            ))}
          </div>
        </Section>
      )}

      {/* local templates */}
      {visibleLocalTemplates.length > 0 && (
        <Section title={t('section.localTemplates')}>
          <div className="flex flex-wrap gap-2">
            {visibleLocalTemplates.map((tpl) => (
              <Link
                key={tpl.templateID}
                to={`/templates/${tpl.templateID}`}
                className="flex items-center gap-1.5 rounded-lg border border-border/60 bg-card/40 px-3 py-1.5 text-sm font-mono text-muted-foreground hover:border-cube-ok/40 hover:text-foreground hover:bg-cube-ok/5 transition-all"
              >
                <Package size={11} className="text-cube-ok/60" />
                {tpl.templateID}
              </Link>
            ))}
          </div>
        </Section>
      )}

      {/* sandboxes */}
      <Section
        title={t('section.sandboxes')}
        action={
          nodeSandboxes.length > 0
            ? <span className="text-sm text-muted-foreground">{nodeSandboxes.length} running</span>
            : undefined
        }
      >
        {sandboxesLoading ? (
          <Skeleton className="h-20 w-full" />
        ) : nodeSandboxes.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t('empty.sandboxes')}</p>
        ) : (
          <div className="rounded-xl border border-border/60 bg-card/40 divide-y divide-border/40">
            {nodeSandboxes.map((sb) => (
              <Link
                key={sb.sandboxID}
                to={`/sandboxes/${sb.sandboxID}`}
                className="flex items-center justify-between gap-3 px-4 py-2.5 hover:bg-muted/40 transition-colors first:rounded-t-xl last:rounded-b-xl"
              >
                <div className="flex items-center gap-2.5 min-w-0">
                  <Box size={12} className="shrink-0 text-muted-foreground/50" />
                  <span className="font-mono text-sm text-foreground/80 truncate">{sb.sandboxID}</span>
                  <span className="text-sm text-muted-foreground/50 truncate hidden sm:block">{sb.templateID}</span>
                </div>
                <div className="flex items-center gap-3 shrink-0">
                  <span className={cn(
                    'inline-flex items-center gap-1.5 text-sm font-medium',
                    sb.state === 'running' ? 'text-cube-ok' : sb.state === 'paused' ? 'text-cube-warn' : 'text-muted-foreground'
                  )}>
                    <span className={cn('h-1.5 w-1.5 rounded-full',
                      sb.state === 'running' ? 'bg-cube-ok' : sb.state === 'paused' ? 'bg-cube-warn' : 'bg-muted-foreground'
                    )} />
                    {sb.state}
                  </span>
                  <span className="text-sm text-muted-foreground/60">{formatRelative(sb.startedAt)}</span>
                </div>
              </Link>
            ))}
          </div>
        )}
      </Section>
    </div>
  );
}
