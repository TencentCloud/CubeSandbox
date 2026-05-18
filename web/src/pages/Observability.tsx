// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useRuntimeConfig } from '@/hooks/useRuntimeConfig';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  Activity, Server, Gauge, Package,
  ExternalLink, Loader2, Wifi, WifiOff,
  CheckCircle2, XCircle, AlertTriangle,
} from 'lucide-react';
import { clusterApi, sandboxApi, templateApi } from '@/api/client';
import { Skeleton } from '@/components/ui/skeleton';
import { cn, formatRelative } from '@/lib/utils';

// ── Shared primitives ─────────────────────────────────────────────────────────

function SectionHeader({ icon: Icon, title, desc }: {
  icon: React.ElementType; title: string; desc?: string;
}) {
  return (
    <div className="flex items-start gap-3 mb-5">
      <div className="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-white/[0.04] border border-white/8">
        <Icon size={15} className="text-cube-cyan/70" />
      </div>
      <div>
        <h2 className="text-base font-semibold tracking-tight">{title}</h2>
        {desc && <p className="text-sm text-muted-foreground mt-0.5">{desc}</p>}
      </div>
    </div>
  );
}

function KpiCard({ label, value, color }: { label: string; value: number | string; color?: string }) {
  return (
    <div className="rounded-xl border border-white/8 bg-white/[0.03] px-5 py-4 flex flex-col gap-1">
      <span className="text-xs text-muted-foreground uppercase tracking-widest">{label}</span>
      <span className={cn('text-3xl font-semibold tabular-nums', color ?? 'text-foreground')}>{value}</span>
    </div>
  );
}

// ── Section 1: Sandbox Health ─────────────────────────────────────────────────

function SandboxSection() {
  const { t } = useTranslation('observability');

  const { data: all, isLoading: loading } = useQuery({
    queryKey: ['sandboxes-obs'],
    queryFn: () => sandboxApi.list(),
    staleTime: 10_000,
    refetchInterval: 10_000,
  });

  const running = all?.filter(s => s.state === 'running') ?? [];
  const paused = all?.filter(s => s.state === 'paused') ?? [];

  const totalCount = all?.length ?? 0;
  const runningCount = running.length;
  const pausedCount = paused.length;
  const recent = [...(all ?? [])].slice(0, 5);

  // distribution bar widths
  const runningPct = totalCount > 0 ? Math.round((runningCount / totalCount) * 100) : 0;
  const pausedPct = totalCount > 0 ? Math.round((pausedCount / totalCount) * 100) : 0;

  return (
    <div>
      <SectionHeader icon={Activity} title={t('sandboxes.title')} desc={t('sandboxes.desc')} />

      {/* KPI row */}
      <div className="grid grid-cols-3 gap-3 mb-5">
        {loading ? (
          Array.from({ length: 3 }).map((_, i) => (
            <div key={i} className="rounded-xl border border-white/8 bg-white/[0.03] px-5 py-4">
              <Skeleton className="h-3 w-16 mb-2" />
              <Skeleton className="h-8 w-10" />
            </div>
          ))
        ) : (
          <>
            <KpiCard label={t('sandboxes.total')} value={totalCount} />
            <KpiCard label={t('sandboxes.running')} value={runningCount} color="text-cube-emerald" />
            <KpiCard label={t('sandboxes.paused')} value={pausedCount} color="text-cube-amber" />
          </>
        )}
      </div>

      {/* Distribution bar */}
      {!loading && totalCount > 0 && (
        <div className="mb-5 rounded-xl border border-white/8 bg-white/[0.03] px-5 py-4">
          <p className="text-xs text-muted-foreground uppercase tracking-widest mb-3">{t('sandboxes.distribution')}</p>
          <div className="flex h-2 w-full overflow-hidden rounded-full bg-white/[0.06]">
            <div className="bg-cube-emerald transition-all" style={{ width: `${runningPct}%` }} />
            <div className="bg-cube-amber transition-all" style={{ width: `${pausedPct}%` }} />
          </div>
          <div className="mt-2 flex items-center gap-4 text-xs text-muted-foreground">
            <span className="flex items-center gap-1.5"><span className="h-2 w-2 rounded-full bg-cube-emerald" />{t('sandboxes.running')} {runningPct}%</span>
            <span className="flex items-center gap-1.5"><span className="h-2 w-2 rounded-full bg-cube-amber" />{t('sandboxes.paused')} {pausedPct}%</span>
          </div>
        </div>
      )}

      {/* Recent list */}
      <div className="rounded-xl border border-white/8 bg-white/[0.03] overflow-x-auto">
        <div className="px-5 py-3 border-b border-white/6">
          <p className="text-xs text-muted-foreground uppercase tracking-widest">{t('sandboxes.recent')}</p>
        </div>
        <table className="w-full text-sm" style={{ minWidth: '560px' }}>
          <thead>
            <tr className="border-b border-white/6">
              {[t('sandboxes.colId'), t('sandboxes.colTemplate'), t('sandboxes.colState'), t('sandboxes.colCreated')].map(h => (
                <th key={h} className="px-5 py-2.5 text-left text-xs uppercase tracking-widest text-muted-foreground/60 font-medium">{h}</th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y divide-white/5">
            {loading ? (
              Array.from({ length: 3 }).map((_, i) => (
                <tr key={i}>{[1,2,3,4].map(j => <td key={j} className="px-5 py-3"><Skeleton className="h-4 w-20" /></td>)}</tr>
              ))
            ) : recent.length === 0 ? (
              <tr><td colSpan={4} className="px-5 py-6 text-sm text-muted-foreground text-center">{t('sandboxes.empty')}</td></tr>
            ) : (
              recent.map(s => (
                <tr key={s.sandboxID} className="hover:bg-white/[0.02] transition-colors">
                  <td className="px-5 py-3">
                    <Link to={`/sandboxes/${s.sandboxID}`} className="inline-flex items-center gap-1.5 font-mono text-xs text-foreground/80 hover:text-cube-cyan transition-colors">
                      {s.sandboxID.slice(0, 8)}…<ExternalLink size={10} className="opacity-50" />
                    </Link>
                  </td>
                  <td className="px-5 py-3 font-mono text-xs text-muted-foreground">{s.templateID ?? '—'}</td>
                  <td className="px-5 py-3">
                    <span className={cn('text-xs font-medium', s.state === 'running' ? 'text-cube-emerald' : 'text-cube-amber')}>{s.state}</span>
                  </td>
                  <td className="px-5 py-3 text-xs text-muted-foreground">{formatRelative(s.startedAt)}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// ── Section 2: Node Health ────────────────────────────────────────────────────

function NodeSection() {
  const { t } = useTranslation('observability');
  const { data: nodes, isLoading } = useQuery({
    queryKey: ['nodes'],
    queryFn: () => clusterApi.nodes(),
    staleTime: 15_000,
    refetchInterval: 15_000,
  });

  const healthyCount = nodes?.filter(n => n.healthy).length ?? 0;
  const totalCount = nodes?.length ?? 0;

  return (
    <div>
      <SectionHeader icon={Server} title={t('nodes.title')} desc={t('nodes.desc')} />

      {/* KPI */}
      <div className="grid grid-cols-2 gap-3 mb-5">
        {isLoading ? (
          Array.from({ length: 2 }).map((_, i) => (
            <div key={i} className="rounded-xl border border-white/8 bg-white/[0.03] px-5 py-4">
              <Skeleton className="h-3 w-16 mb-2" /><Skeleton className="h-8 w-10" />
            </div>
          ))
        ) : (
          <>
            <KpiCard label={t('nodes.healthyCount')} value={healthyCount} color={healthyCount === totalCount ? 'text-cube-emerald' : 'text-cube-rose'} />
            <KpiCard label={t('nodes.totalCount')} value={totalCount} />
          </>
        )}
      </div>

      {/* Node list */}
      <div className="space-y-3">
        {isLoading ? (
          Array.from({ length: 2 }).map((_, i) => (
            <div key={i} className="rounded-xl border border-white/8 bg-white/[0.03] px-5 py-4">
              <Skeleton className="h-4 w-32 mb-3" />
              <Skeleton className="h-2 w-full mb-2" />
              <Skeleton className="h-2 w-full" />
            </div>
          ))
        ) : !nodes || nodes.length === 0 ? (
          <div className="rounded-xl border border-white/8 bg-white/[0.03] px-5 py-6 text-sm text-muted-foreground text-center">{t('nodes.empty')}</div>
        ) : (
          nodes.map(node => (
            <div key={node.nodeID} className={cn('rounded-xl border bg-white/[0.03] px-5 py-4', node.healthy ? 'border-white/8' : 'border-cube-rose/30 bg-cube-rose/[0.03]')}>
              <div className="flex items-center justify-between mb-4">
                <Link to={`/nodes/${node.nodeID}`} className="inline-flex items-center gap-2 hover:text-cube-cyan transition-colors">
                  <span className={cn('h-2 w-2 rounded-full', node.healthy ? 'bg-cube-emerald animate-pulse' : 'bg-cube-rose')} />
                  <span className="font-mono text-sm text-foreground/80">{node.address ?? node.nodeID}</span>
                  <ExternalLink size={11} className="opacity-40" />
                </Link>
                <span className={cn('text-xs font-medium', node.healthy ? 'text-cube-emerald' : 'text-cube-rose')}>
                  {node.healthy ? t('nodes.healthy') : t('nodes.degraded')}
                </span>
              </div>
              {/* CPU bar */}
              <div className="space-y-2.5">
                <div>
                  <div className="flex justify-between text-xs text-muted-foreground mb-1">
                    <span>{t('nodes.cpu')}</span>
                    <span>{node.saturationPct}%</span>
                  </div>
                  <div className="h-1.5 w-full rounded-full bg-white/[0.06] overflow-hidden">
                    <div
                      className={cn('h-full rounded-full transition-all', node.saturationPct > 80 ? 'bg-cube-rose' : node.saturationPct > 60 ? 'bg-cube-amber' : 'bg-cube-cyan')}
                      style={{ width: `${node.saturationPct}%` }}
                    />
                  </div>
                </div>
                {/* Memory bar */}
                <div>
                  <div className="flex justify-between text-xs text-muted-foreground mb-1">
                    <span>{t('nodes.memory')}</span>
                    <span>{node.memorySaturationPct}%</span>
                  </div>
                  <div className="h-1.5 w-full rounded-full bg-white/[0.06] overflow-hidden">
                    <div
                      className={cn('h-full rounded-full transition-all', node.memorySaturationPct > 80 ? 'bg-cube-rose' : node.memorySaturationPct > 60 ? 'bg-cube-amber' : 'bg-cube-violet')}
                      style={{ width: `${node.memorySaturationPct}%` }}
                    />
                  </div>
                </div>
              </div>
            </div>
          ))
        )}
      </div>
    </div>
  );
}

// ── Section 3: API Monitor ────────────────────────────────────────────────────

function ApiSection() {
  const { t } = useTranslation('observability');
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState<{ ok: boolean; latency?: number; msg?: string } | null>(null);

  const { data: cfg, isLoading } = useRuntimeConfig();

  const handleTest = async () => {
    setTesting(true);
    setResult(null);
    const t0 = performance.now();
    try {
      await clusterApi.config();
      setResult({ ok: true, latency: Math.round(performance.now() - t0) });
    } catch (e) {
      setResult({ ok: false, msg: e instanceof Error ? e.message : String(e) });
    } finally {
      setTesting(false);
    }
  };

  return (
    <div>
      <SectionHeader icon={Gauge} title={t('api.title')} desc={t('api.desc')} />
      <div className="rounded-xl border border-white/8 bg-white/[0.03] px-5 py-1 mb-3">
        {isLoading ? (
          <div className="space-y-3 py-3">{[1,2].map(i => <Skeleton key={i} className="h-4 w-full" />)}</div>
        ) : (
          <>
            <div className="flex items-center justify-between py-2.5 border-b border-white/5">
              <span className="text-sm text-muted-foreground">{t('api.rateLimit')}</span>
              <span className="font-mono text-sm">{cfg?.rateLimitPerSec ?? '—'} req/s</span>
            </div>
            <div className="flex items-center justify-between py-2.5">
              <span className="text-sm text-muted-foreground">{t('api.auth')}</span>
              {cfg?.authEnabled ? (
                <span className="inline-flex items-center gap-1 text-cube-emerald text-xs font-medium"><CheckCircle2 size={12} />{t('api.authOn')}</span>
              ) : (
                <span className="inline-flex items-center gap-1 text-muted-foreground text-xs"><XCircle size={12} />{t('api.authOff')}</span>
              )}
            </div>
          </>
        )}
      </div>

      {/* Test button + result */}
      <div className="flex items-center gap-3">
        <button
          onClick={handleTest}
          disabled={testing}
          className="inline-flex items-center gap-1.5 rounded-lg border border-white/8 bg-white/[0.03] px-3 py-2 text-sm text-muted-foreground hover:border-primary/30 hover:text-foreground transition-colors disabled:opacity-50"
        >
          {testing ? <Loader2 size={13} className="animate-spin" /> : <Wifi size={13} />}
          {testing ? t('api.testing') : t('api.test')}
        </button>
        {result && (
          <div className={cn(
            'flex items-center gap-2 rounded-lg border px-3 py-2 text-sm animate-fade-in',
            result.ok ? 'border-cube-emerald/20 bg-cube-emerald/[0.06] text-cube-emerald' : 'border-cube-rose/20 bg-cube-rose/[0.06] text-cube-rose'
          )}>
            {result.ok
              ? <><Wifi size={13} />{t('api.connected')} · {result.latency}ms</>
              : <><WifiOff size={13} />{result.msg}</>}
          </div>
        )}
      </div>
    </div>
  );
}

// ── Section 4: Template Build Status ─────────────────────────────────────────

function TemplateSection() {
  const { t } = useTranslation('observability');
  const { data: templates, isLoading } = useQuery({
    queryKey: ['templates'],
    queryFn: () => templateApi.list(),
    staleTime: 30_000,
    refetchInterval: 30_000,
  });

  const ready = templates?.filter(t => t.status.toLowerCase() === 'ready').length ?? 0;
  const building = templates?.filter(t => t.status.toLowerCase() === 'building').length ?? 0;
  const failed = templates?.filter(t => t.status.toLowerCase() === 'failed') ?? [];

  return (
    <div>
      <SectionHeader icon={Package} title={t('templates.title')} desc={t('templates.desc')} />

      {/* Status badges */}
      <div className="flex items-center gap-3 mb-5 flex-wrap">
        {isLoading ? (
          Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-7 w-20 rounded-full" />)
        ) : (
          <>
            <span className="inline-flex items-center gap-1.5 rounded-full border border-cube-emerald/20 bg-cube-emerald/[0.08] px-3 py-1 text-xs font-medium text-cube-emerald">
              <CheckCircle2 size={11} />{t('templates.ready')} · {ready}
            </span>
            {building > 0 && (
              <span className="inline-flex items-center gap-1.5 rounded-full border border-cube-amber/20 bg-cube-amber/[0.08] px-3 py-1 text-xs font-medium text-cube-amber">
                <Loader2 size={11} className="animate-spin" />{t('templates.building')} · {building}
              </span>
            )}
            {failed.length > 0 && (
              <span className="inline-flex items-center gap-1.5 rounded-full border border-cube-rose/20 bg-cube-rose/[0.08] px-3 py-1 text-xs font-medium text-cube-rose">
                <AlertTriangle size={11} />{t('templates.failed')} · {failed.length}
              </span>
            )}
          </>
        )}
      </div>

      {/* Failed templates */}
      <div className="rounded-xl border border-white/8 bg-white/[0.03] overflow-x-auto">
        <div className="px-5 py-3 border-b border-white/6">
          <p className="text-xs text-muted-foreground uppercase tracking-widest">{t('templates.failedList')}</p>
        </div>
        {isLoading ? (
          <div className="px-5 py-4 space-y-2">{[1,2].map(i => <Skeleton key={i} className="h-4 w-full" />)}</div>
        ) : failed.length === 0 ? (
          <div className="px-5 py-6 text-sm text-muted-foreground text-center flex items-center justify-center gap-2">
            <CheckCircle2 size={14} className="text-cube-emerald" />{t('templates.noFailed')}
          </div>
        ) : (
          <table className="w-full text-sm" style={{ minWidth: '560px' }}>
            <thead>
              <tr className="border-b border-white/6">
                {[t('templates.colId'), t('templates.colStatus'), t('templates.colVersion'), t('templates.colError')].map(h => (
                  <th key={h} className="px-5 py-2.5 text-left text-xs uppercase tracking-widest text-muted-foreground/60 font-medium">{h}</th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-white/5">
              {failed.map(tpl => (
                <tr key={tpl.templateID} className="hover:bg-white/[0.02] transition-colors">
                  <td className="px-5 py-3">
                    <Link to={`/templates/${tpl.templateID}`} className="inline-flex items-center gap-1.5 font-mono text-xs text-foreground/80 hover:text-cube-cyan transition-colors">
                      {tpl.templateID}<ExternalLink size={10} className="opacity-50" />
                    </Link>
                  </td>
                  <td className="px-5 py-3">
                    <span className="text-xs font-medium text-cube-rose">{tpl.status}</span>
                  </td>
                  <td className="px-5 py-3 font-mono text-xs text-muted-foreground">{tpl.version ?? '—'}</td>
                  <td className="px-5 py-3 text-xs text-cube-rose/80 max-w-xs truncate">{tpl.lastError ?? '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}

// ── Page ──────────────────────────────────────────────────────────────────────

export default function ObservabilityPage() {
  const { t } = useTranslation('observability');
  return (
    <div className="animate-fade-in space-y-10 py-8">
      <div className="flex items-center gap-3 border-b border-border/50 pb-6">
        <Activity size={20} className="text-cube-cyan/70" />
        <div>
          <h1 className="text-xl font-semibold tracking-tight">{t('title')}</h1>
          <p className="text-sm text-muted-foreground mt-0.5">{t('description')}</p>
        </div>
      </div>

      <SandboxSection />
      <NodeSection />
      <ApiSection />
      <TemplateSection />
    </div>
  );
}
