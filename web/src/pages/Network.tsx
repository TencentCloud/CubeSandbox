// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { clusterApi } from '@/api/client';
import { useRuntimeConfig } from '@/hooks/useRuntimeConfig';
import { Skeleton } from '@/components/ui/skeleton';
import { Shield, Gauge, Server, ExternalLink, Minus } from 'lucide-react';
import { cn } from '@/lib/utils';
import { BoolBadge, MetricValue } from '@/components/ui/typography';

// ── helpers ───────────────────────────────────────────────────────────────────

function SectionHeader({ icon: Icon, title, description }: {
  icon: React.ElementType; title: string; description?: string;
}) {
  return (
    <div className="flex items-start gap-3 mb-5">
      <div className="mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-muted/40 border border-border/60">
        <Icon size={15} className="text-cube-info/80" />
      </div>
      <div>
        <h2 className="text-base font-semibold tracking-tight">{title}</h2>
        {description && <p className="text-sm text-muted-foreground mt-0.5">{description}</p>}
      </div>
    </div>
  );
}

function InfoRow({ label, value, mono, badge }: {
  label: string; value?: React.ReactNode; mono?: boolean; badge?: React.ReactNode;
}) {
  return (
    <div className="flex items-center justify-between py-2.5 border-b border-border/40 last:border-0">
      <span className="text-sm text-muted-foreground">{label}</span>
      <span className={cn('text-sm text-foreground/90 flex items-center gap-2', mono && 'font-mono')}>
        {badge}
        {value}
      </span>
    </div>
  );
}

// ── Section 1: API Gateway ────────────────────────────────────────────────────

function GatewaySection() {
  const { t } = useTranslation('network');
  const { data, isLoading } = useRuntimeConfig();

  return (
    <div>
      <SectionHeader icon={Gauge} title={t('gateway.title')} description={t('gateway.desc')} />
      <div className="rounded-xl border border-border/60 bg-card/40 px-5 py-1">
        {isLoading ? (
          <div className="space-y-3 py-3">
            {[1, 2, 3, 4].map(i => <Skeleton key={i} className="h-4 w-full" />)}
          </div>
        ) : (
          <>
            <InfoRow
              label={t('gateway.rateLimit')}
              value={<MetricValue value={data?.rateLimitPerSec ?? '—'} unit="req/s · per API Key" />}
            />
            <InfoRow label={t('gateway.auth')} value={undefined} badge={<BoolBadge value={data?.authEnabled} trueLabel={t('gateway.authOn')} falseLabel={t('gateway.authOff')} />} />
            <InfoRow label={t('gateway.domain')} value={data?.sandboxDomain ?? '—'} />
            <InfoRow label={t('gateway.instanceType')} value={data?.instanceType ?? '—'} />
          </>
        )}
      </div>
    </div>
  );
}

// ── Section 2: Node quota / concurrency ───────────────────────────────────────

function NodeQuotaSection() {
  const { t } = useTranslation('network');
  const { data: nodes, isLoading } = useQuery({
    queryKey: ['nodes'],
    queryFn: () => clusterApi.nodes(),
    staleTime: 15_000,
    refetchInterval: 15_000,
  });

  return (
    <div>
      <SectionHeader icon={Server} title={t('quota.title')} description={t('quota.desc')} />
      <div className="rounded-xl border border-border/60 bg-card/40 overflow-x-auto">
        <table className="w-full text-sm" style={{ minWidth: '700px' }}>
          <thead>
            <tr className="border-b border-border/50">
              {[t('quota.colNode'), t('quota.colStatus'), t('quota.colConcurrent'), t('quota.colCpuQuota'), t('quota.colMemQuota'), t('quota.colMvmSlots')].map(h => (
                <th key={h} className="tbl-th">{h}</th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y divide-border/40">
            {isLoading ? (
              Array.from({ length: 2 }).map((_, i) => (
                <tr key={i}>
                  {[1, 2, 3, 4, 5, 6].map(j => (
                    <td key={j} className="px-5 py-3"><Skeleton className="h-4 w-20" /></td>
                  ))}
                </tr>
              ))
            ) : !nodes || nodes.length === 0 ? (
              <tr>
                <td colSpan={6} className="px-5 py-6 text-sm text-muted-foreground text-center">{t('quota.empty')}</td>
              </tr>
            ) : (
              nodes.map((node) => (
                <tr key={node.nodeID} className="hover:bg-muted/30 transition-colors">
                  <td className="px-5 py-3">
                    <Link
                      to={`/nodes/${node.nodeID}`}
                      className="inline-flex items-center gap-1.5 text-sm text-foreground/90 text-num hover:text-primary transition-colors"
                    >
                      {node.address ?? node.nodeID}
                      <ExternalLink size={10} className="opacity-50" />
                    </Link>
                  </td>
                  <td className="px-5 py-3">
                    <span className="inline-flex items-center gap-1.5">
                      <span className={cn('h-1.5 w-1.5 rounded-full', node.healthy ? 'bg-cube-ok' : 'bg-cube-err')} />
                      <span className={cn('text-xs font-medium', node.healthy ? 'text-cube-ok' : 'text-cube-err')}>
                        {node.healthy ? t('quota.ready') : t('quota.degraded')}
                      </span>
                    </span>
                  </td>
                  <td className="px-5 py-3 text-sm tbl-td-num">
                    {node.resources.createConcurrentNum != null ? node.resources.createConcurrentNum : <Minus size={12} className="text-muted-foreground/40" />}
                  </td>
                  <td className="px-5 py-3 text-sm tbl-td-num">
                    {node.resources.quotaCpu > 0
                      ? <MetricValue value={(node.resources.quotaCpu / 1000).toFixed(1)} unit="cores" />
                      : <Minus size={12} className="text-muted-foreground/40" />}
                  </td>
                  <td className="px-5 py-3 text-sm tbl-td-num">
                    {node.resources.quotaMemMB > 0
                      ? <MetricValue value={(node.resources.quotaMemMB / 1024).toFixed(1)} unit="GiB" />
                      : <Minus size={12} className="text-muted-foreground/40" />}
                  </td>
                  <td className="px-5 py-3 text-sm tbl-td-num">{node.resources.maxMvmSlots}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// ── Page ──────────────────────────────────────────────────────────────────────

export default function NetworkPage() {
  const { t } = useTranslation('network');
  return (
    <div className="animate-fade-in space-y-10 py-8">
      {/* page header */}
      <div className="flex items-center gap-3 border-b border-border/50 pb-6">
        <Shield size={20} className="text-cube-info/70" />
        <div>
          <h1 className="text-xl font-semibold tracking-tight">{t('title')}</h1>
          <p className="text-sm text-muted-foreground mt-0.5">{t('description')}</p>
        </div>
      </div>

      <GatewaySection />
      <NodeQuotaSection />
    </div>
  );
}
