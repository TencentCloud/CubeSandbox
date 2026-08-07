// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { clusterApi } from '@/api/client';
import {
  formatIsolationError,
  IsolateConfirmDialog,
} from '@/components/nodes/IsolateConfirmDialog';
import * as DropdownMenu from '@radix-ui/react-dropdown-menu';
import { Card, CardHeader, CardTitle, CardDescription } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import { showToast } from '@/components/ui/ToastProvider';
import { Cpu, HardDrive, Server, ShieldCheck, ShieldOff, MoreHorizontal } from 'lucide-react';
import { cn, formatRelative, formatCondition, getConditionTone } from '@/lib/utils';

export default function NodesPage() {
  const { data, isLoading } = useQuery({
    queryKey: ['nodes'],
    queryFn: clusterApi.nodes,
    refetchInterval: 15_000,
  });
  const { t } = useTranslation('nodes');
  const { t: td } = useTranslation('nodeDetail');
  const qc = useQueryClient();
  const [confirmNodeID, setConfirmNodeID] = useState<string | null>(null);

  const isolate = useMutation({
    mutationFn: (nodeID: string) => clusterApi.isolate(nodeID),
    onSuccess: async () => {
      setConfirmNodeID(null);
      showToast(td('isolation.isolatedToast'));
      await qc.invalidateQueries({ queryKey: ['nodes'] });
    },
  });

  const unisolate = useMutation({
    mutationFn: (nodeID: string) => clusterApi.unisolate(nodeID),
    onSuccess: async () => {
      showToast(td('isolation.unisolatedToast'));
      await qc.invalidateQueries({ queryKey: ['nodes'] });
    },
  });

  const isolationPending = isolate.isPending || unisolate.isPending;

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
          <Link
            key={n.nodeID}
            to={`/nodes/${n.nodeID}`}
            className="block hover:opacity-90 transition-opacity"
          >
            <Card className="panel-hover h-full">
              <CardHeader>
                <div className="flex items-center gap-3">
                  <span className="flex h-9 w-9 items-center justify-center rounded-md bg-muted text-muted-foreground">
                    <Server size={16} />
                  </span>
                  <div className="min-w-0 flex-1">
                    <CardTitle className="flex items-center gap-2">
                      <span className="relative flex h-2 w-2 shrink-0">
                        {n.status.toLowerCase() === 'ready' && (
                          <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-green-400 opacity-60" />
                        )}
                        <span
                          className={cn(
                            'relative inline-flex rounded-full h-2 w-2',
                            n.status.toLowerCase() === 'ready' ? 'bg-green-400' : 'bg-amber-400',
                          )}
                        />
                      </span>
                      {n.hostname && n.hostname !== n.nodeID ? n.hostname : n.nodeID}
                      {n.schedulingDisabled && (
                        <Badge tone="warn">{t('isolated')}</Badge>
                      )}
                    </CardTitle>
                    {n.hostname && n.hostname !== n.nodeID && (
                      <CardDescription className="font-mono text-xs">{n.nodeID}</CardDescription>
                    )}
                  </div>
                </div>
                <DropdownMenu.Root>
                  <DropdownMenu.Trigger asChild>
                    <Button
                      type="button"
                      variant="ghost"
                      size="icon"
                      className="shrink-0 h-8 w-8 text-muted-foreground hover:text-foreground -mr-2 -mt-2"
                      disabled={isolationPending}
                      onClick={(e) => {
                        e.preventDefault();
                        e.stopPropagation();
                      }}
                    >
                      <MoreHorizontal size={16} />
                    </Button>
                  </DropdownMenu.Trigger>
                  <DropdownMenu.Portal>
                    <DropdownMenu.Content
                      align="end"
                      sideOffset={4}
                      className="z-50 min-w-[140px] overflow-hidden rounded-lg border border-border/60 bg-popover/95 p-1 shadow-2xl backdrop-blur-xl animate-fade-in"
                      onClick={(e) => {
                        e.preventDefault();
                        e.stopPropagation();
                      }}
                      onPointerDown={(e) => e.stopPropagation()}
                      onPointerUp={(e) => e.stopPropagation()}
                    >
                      {n.schedulingDisabled ? (
                        <DropdownMenu.Item
                          className="flex cursor-pointer select-none items-center gap-2 rounded-md px-2 py-1.5 text-sm outline-none hover:bg-accent hover:text-accent-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50"
                          disabled={isolationPending}
                          onSelect={() => {
                            unisolate.mutate(n.nodeID);
                          }}
                        >
                          <ShieldCheck size={14} />
                          {unisolate.isPending && unisolate.variables === n.nodeID
                            ? td('isolation.unisolating')
                            : td('isolation.unisolate')}
                        </DropdownMenu.Item>
                      ) : (
                        <DropdownMenu.Item
                          className="flex cursor-pointer select-none items-center gap-2 rounded-md px-2 py-1.5 text-sm outline-none hover:bg-accent hover:text-accent-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50 text-destructive focus:bg-destructive/10 focus:text-destructive"
                          disabled={isolationPending}
                          onSelect={() => {
                            isolate.reset();
                            setConfirmNodeID(n.nodeID);
                          }}
                        >
                          <ShieldOff size={14} />
                          {td('isolation.isolate')}
                        </DropdownMenu.Item>
                      )}
                    </DropdownMenu.Content>
                  </DropdownMenu.Portal>
                </DropdownMenu.Root>
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
                            100,
                        )
                      : 0
                  }
                  detail={`${((n.resources.totalMemoryMB - n.resources.allocatableMemoryMB) / 1024).toFixed(1)} / ${(n.resources.totalMemoryMB / 1024).toFixed(1)} GiB`}
                />
              </div>

              {n.conditions && n.conditions.length > 0 && (
                <div className="mt-4 space-y-2 border-t border-border/60 pt-3">
                  {n.conditions.slice(0, 3).map((c, i) => (
                    <div key={i} className="flex items-center justify-between text-xs">
                      <Badge tone={getConditionTone(c.type, c.status)}>
                        {formatCondition(c.type, c.status)}
                      </Badge>
                      <span className="text-muted-foreground">
                        {formatRelative(c.lastTransitionTime)}
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

      <IsolateConfirmDialog
        open={confirmNodeID !== null}
        onClose={() => {
          if (!isolate.isPending) setConfirmNodeID(null);
        }}
        onConfirm={() => {
          if (confirmNodeID) isolate.mutate(confirmNodeID);
        }}
        pending={isolate.isPending}
        error={
          isolate.isError ? formatIsolationError(isolate.error, td('isolation.failed')) : null
        }
      />
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
  const tone =
    pct > 85
      ? 'from-cube-err/80 to-cube-err'
      : pct > 65
        ? 'from-cube-warn/80 to-cube-warn'
        : 'from-primary/70 to-cube-accent';
  return (
    <div>
      <div className="flex items-center justify-between text-muted-foreground">
        <span className="flex items-center gap-1.5">
          {icon}
          {label}
        </span>
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
