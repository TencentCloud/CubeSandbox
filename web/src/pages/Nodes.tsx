// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import * as DropdownMenu from '@radix-ui/react-dropdown-menu';
import { Ban, Cpu, HardDrive, MoreVertical, Server, ShieldCheck } from 'lucide-react';
import { clusterApi, type ClusterNodeView } from '@/api/client';
import { Card, CardHeader, CardTitle, CardDescription } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { IsolateNodeDialog } from '@/components/IsolateNodeDialog';
import { cn } from '@/lib/utils';

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
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-40" />
          ))}
        </div>
      )}

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3 2xl:grid-cols-4">
        {data?.map((n) => (
          <NodeCard key={n.nodeID} node={n} />
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

// ── Node card ────────────────────────────────────────────────────────────────

function NodeCard({ node: n }: { node: ClusterNodeView }) {
  const { t } = useTranslation('nodes');
  const [isolateOpen, setIsolateOpen] = useState(false);
  const isReady = n.status.toLowerCase() === 'ready';
  const isDegraded = !isReady;

  return (
    <>
      <Card className="panel-hover relative h-full p-0 overflow-hidden">
        <Link to={`/nodes/${n.nodeID}`} className="block p-5 pr-12 transition-opacity hover:opacity-90">
          <CardHeader>
            <div className="flex items-center gap-3">
              <span className="flex h-9 w-9 items-center justify-center rounded-md bg-muted text-muted-foreground">
                <Server size={16} />
              </span>
              <div className="min-w-0">
                <CardTitle className="flex items-center gap-2">
                  <span className="relative flex h-2 w-2 shrink-0">
                    {isReady && (
                      <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-green-400 opacity-60" />
                    )}
                    <span className={cn('relative inline-flex rounded-full h-2 w-2', isReady ? 'bg-green-400' : 'bg-amber-400')} />
                  </span>
                  {n.hostname && n.hostname !== n.nodeID ? n.hostname : n.nodeID}
                  {n.isolated && <Badge tone="warn">{t('isolated')}</Badge>}
                  {isDegraded && <Badge tone="err">{t('degraded')}</Badge>}
                </CardTitle>
                {n.hostname && n.hostname !== n.nodeID && (
                  <CardDescription className="font-mono text-xs">{n.nodeID}</CardDescription>
                )}
              </div>
            </div>
          </CardHeader>

          <div className="mt-2 grid grid-cols-1 gap-3 text-xs xl:grid-cols-2 xl:gap-4">
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
        </Link>

        {/* Three-dot menu — sibling of the Link, not nested inside an <a> */}
        <div className="absolute right-3 top-3 z-10">
          <DropdownMenu.Root>
            <DropdownMenu.Trigger asChild>
              <button
                type="button"
                aria-label={t('moreOptions')}
                onClick={(e) => e.preventDefault()}
                className="inline-flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground/70 transition-colors hover:bg-muted hover:text-foreground focus:outline-none focus-visible:ring-2 focus-visible:ring-primary/40"
              >
                <MoreVertical size={15} />
              </button>
            </DropdownMenu.Trigger>
            <DropdownMenu.Portal>
              <DropdownMenu.Content
                align="end"
                sideOffset={4}
                className="z-50 w-fit whitespace-nowrap overflow-hidden rounded-lg border border-border/60 bg-popover/95 p-1 shadow-xl backdrop-blur-xl animate-fade-in"
              >
                <DropdownMenu.Item
                  onSelect={() => setIsolateOpen(true)}
                  className={cn(
                    'flex cursor-pointer items-center gap-1.5 rounded px-2 py-1 text-xs outline-none',
                    n.isolated
                      ? 'text-cube-ok hover:bg-cube-ok/10 focus-visible:bg-cube-ok/10 data-[highlighted]:bg-cube-ok/10'
                      : 'text-cube-warn hover:bg-cube-warn/10 focus-visible:bg-cube-warn/10 data-[highlighted]:bg-cube-warn/10',
                  )}
                >
                  {n.isolated ? <ShieldCheck size={12} /> : <Ban size={12} />}
                  {n.isolated ? t('isolation.unisolate') : t('isolation.isolate')}
                </DropdownMenu.Item>
              </DropdownMenu.Content>
            </DropdownMenu.Portal>
          </DropdownMenu.Root>
        </div>
      </Card>

      <IsolateNodeDialog
        open={isolateOpen}
        onOpenChange={setIsolateOpen}
        nodeID={n.nodeID}
        isCurrentlyIsolated={n.isolated}
      />
    </>
  );
}

// ── Meter sub-component ──────────────────────────────────────────────────────

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
