// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { clusterApi } from '@/api/client';
import { Card, CardHeader, CardTitle, CardDescription } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { Cpu, HardDrive, Server } from 'lucide-react';
import { cn, formatRelative } from '@/lib/utils';

export default function NodesPage() {
  const { data, isLoading } = useQuery({
    queryKey: ['nodes'],
    queryFn: clusterApi.nodes,
    refetchInterval: 15_000,
  });
  const { t } = useTranslation('nodes');

  return (
    <div className="animate-fade-in space-y-5">
      <header>
        <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
        <p className="mt-1 text-sm text-muted-foreground">{t('subtitle')}</p>
      </header>

      {isLoading && (
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-40" />
          ))}
        </div>
      )}

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        {data?.map((n) => (
          <Link key={n.nodeID} to={`/nodes/${n.nodeID}`} className="block hover:opacity-90 transition-opacity">
            <Card className="panel-hover h-full">
            <CardHeader>
              <div className="flex items-center gap-3">
                <span className="flex h-9 w-9 items-center justify-center rounded-md bg-muted text-muted-foreground">
                  <Server size={16} />
                </span>
                <div className="min-w-0">
                  <CardTitle className="flex items-center gap-2">
                    <span className="relative flex h-2 w-2 shrink-0">
                      {n.status.toLowerCase() === 'ready' && (
                        <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-green-400 opacity-60" />
                      )}
                      <span className={cn('relative inline-flex rounded-full h-2 w-2', n.status.toLowerCase() === 'ready' ? 'bg-green-400' : 'bg-amber-400')} />
                    </span>
                    {n.hostname && n.hostname !== n.nodeID ? n.hostname : n.nodeID}
                  </CardTitle>
                  {n.hostname && n.hostname !== n.nodeID && (
                    <CardDescription className="font-mono text-xs">{n.nodeID}</CardDescription>
                  )}
                </div>
              </div>
            </CardHeader>

            <div className="mt-2 grid grid-cols-2 gap-4 text-xs">
              <Meter
                icon={<Cpu size={13} />}
                label={t('cpu')}
                pct={n.saturationPct}
                detail={`${((n.resources.totalCpuMilli - n.resources.allocatableCpuMilli) / 1000).toFixed(1)} / ${(n.resources.totalCpuMilli / 1000).toFixed(1)} cores`}
              />
              <Meter
                icon={<HardDrive size={13} />}
                label={t('memory')}
                pct={
                  n.resources.totalMemoryMB > 0
                    ? Math.round(
                        ((n.resources.totalMemoryMB - n.resources.allocatableMemoryMB) /
                          n.resources.totalMemoryMB) *
                          100
                      )
                    : 0
                }
                detail={`${(((n.resources.totalMemoryMB - n.resources.allocatableMemoryMB) / 1024)).toFixed(1)} / ${(n.resources.totalMemoryMB / 1024).toFixed(1)} GiB`}
              />
            </div>

            {n.conditions && n.conditions.length > 0 && (
              <div className="mt-4 space-y-1 border-t border-border/60 pt-3">
                {n.conditions.slice(0, 3).map((c, i) => (
                  <div key={i} className="flex items-center justify-between text-xs">
                    <span className="text-muted-foreground">{c.type}</span>
                    <span className="flex items-center gap-2">
                      <Badge tone={c.status === 'True' ? 'ok' : 'warn'}>{c.status}</Badge>
                      <span className="text-muted-foreground">{formatRelative(c.lastTransitionTime)}</span>
                    </span>
                  </div>
                ))}
              </div>
            )}
          </Card>
            </Link>
        ))}
      </div>

      {data?.length === 0 && !isLoading && (
        <Card>
          <div className="py-16 text-center text-sm text-muted-foreground">{t('noNodes')}</div>
        </Card>
      )}
    </div>
  );
}

function Meter({
  icon,
  label,
  pct,
  detail,
}: {
  icon: React.ReactNode;
  label: string;
  pct: number;
  detail: string;
}) {
  const tone = pct > 85 ? 'from-cube-err/80 to-cube-err' : pct > 65 ? 'from-cube-warn/80 to-cube-warn' : 'from-primary/70 to-cube-accent';
  return (
    <div>
      <div className="flex items-center justify-between text-muted-foreground">
        <span className="flex items-center gap-1.5">{icon}{label}</span>
        <span className="text-foreground text-num">{pct}%</span>
      </div>
      <div className="mt-1 h-1.5 overflow-hidden rounded-full bg-muted">
        <div
          className={`h-full bg-gradient-to-r ${tone} transition-all`}
          style={{ width: `${Math.max(2, Math.min(100, pct))}%` }}
        />
      </div>
      <div className="mt-1 text-xs text-muted-foreground text-num">{detail}</div>
    </div>
  );
}
