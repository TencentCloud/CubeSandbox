// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState } from 'react';
import { createPortal } from 'react-dom';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import type { TFunction } from 'i18next';
import {
  Archive,
  RefreshCw,
  Upload,
  ChevronRight,
  Box,
  Disc,
  Cpu,
  Microchip,
  FileClock,
} from 'lucide-react';
import { warehouseApi, type WarehouseComponentSummary } from '@/api/client';
import { ApiError } from '@/lib/api';
import { Button } from '@/components/ui/button';
import { Card } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { formatArtifactBytes } from '@/lib/utils';
import { ImportTab } from '@/components/warehouse/ImportTab';

export default function WarehousePage() {
  const { t } = useTranslation('warehouse');
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [showImport, setShowImport] = useState(false);

  const listQ = useQuery({
    queryKey: ['warehouse', 'components'],
    queryFn: warehouseApi.listComponents,
  });
  const jobsQ = useQuery({
    queryKey: ['warehouse', 'jobs'],
    queryFn: () => warehouseApi.preinstallJobs({ limit: 50, offset: 0 }),
    refetchInterval: 5000,
  });
  const importsQ = useQuery({
    queryKey: ['warehouse', 'imports'],
    queryFn: () => warehouseApi.listImports({ limit: 50, offset: 0 }),
    refetchInterval: 5000,
  });

  const activeJobs = [
    ...(jobsQ.data?.jobs ?? []).filter((j) => j.status === 'pending' || j.status === 'running'),
    ...(importsQ.data?.jobs ?? []).filter((j) => j.status === 'pending' || j.status === 'running'),
  ];

  return (
    <div className="animate-fade-in space-y-6 pt-4 pb-12">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="flex items-center gap-2 text-xl font-semibold">
            <Archive size={18} />
            {t('title')}
          </h1>
          <p className="mt-1 max-w-3xl text-sm text-muted-foreground">{t('subtitle')}</p>
        </div>
        <div className="flex shrink-0 gap-2">
          <Button variant="outline" size="sm" onClick={() => navigate('/warehouse/jobs')}>
            <FileClock size={14} className="mr-1.5" />
            {t('jobs')}
            {activeJobs.length > 0 && (
              <span className="ml-1.5 flex h-4 min-w-[1rem] items-center justify-center rounded-full bg-primary/10 px-1 text-[10px] font-medium text-primary">
                {activeJobs.length}
              </span>
            )}
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              void qc.invalidateQueries({ queryKey: ['warehouse'] });
            }}
          >
            <RefreshCw size={14} />
            {t('refresh')}
          </Button>
          <Button size="sm" onClick={() => setShowImport(true)}>
            <Upload size={14} />
            {t('import')}
          </Button>
        </div>
      </div>

      <div className="flex items-center gap-2 rounded-md bg-muted/30 border border-border/50 px-3 py-2.5">
        <span className="h-1.5 w-1.5 rounded-full bg-blue-500"></span>
        <span className="text-xs text-muted-foreground">{t('timeoutNote')}</span>
      </div>

      {listQ.isLoading && (
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-[180px]" />
          ))}
        </div>
      )}
      {listQ.isError && (
        <p className="text-sm text-destructive">
          {warehouseDisabledMessage(listQ.error, t('disabled')) || t('error')}
        </p>
      )}
      {listQ.data && (
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          {listQ.data.components.map((row) => (
            <ComponentCard key={row.name} row={row} t={t} />
          ))}
        </div>
      )}

      {showImport &&
        createPortal(
          <div
            className="fixed inset-0 z-[100] flex items-center justify-center bg-foreground/40 backdrop-blur-sm animate-fade-in"
            onClick={() => setShowImport(false)}
          >
            <div
              className="max-h-[90vh] w-[520px] overflow-auto"
              onClick={(e) => e.stopPropagation()}
            >
              <ImportTab
                onDone={() => {
                  void qc.invalidateQueries({ queryKey: ['warehouse'] });
                  setShowImport(false);
                }}
                onCancel={() => setShowImport(false)}
              />
            </div>
          </div>,
          document.body,
        )}
    </div>
  );
}

function ComponentCard({ row, t }: { row: WarehouseComponentSummary; t: TFunction<'warehouse'> }) {
  const about = t(`about.${row.name}`, { defaultValue: '' });

  let Icon = Box;
  if (row.name === 'cube-image') Icon = Disc;
  if (row.name === 'cube-agent') Icon = Cpu;
  if (row.name === 'cube-kernel-scf') Icon = Microchip;

  return (
    <Link
      to={`/warehouse/${encodeURIComponent(row.name)}`}
      className="block hover:opacity-90 transition-opacity"
    >
      <Card className="panel-hover h-full flex flex-col p-5">
        <div className="flex items-start justify-between gap-3">
          <div className="flex items-center gap-3">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-primary/10 text-primary">
              <Icon size={20} />
            </div>
            <div>
              <h2 className="font-mono text-sm font-semibold">{row.name}</h2>
              {about ? (
                <p className="mt-0.5 text-xs text-muted-foreground line-clamp-1">{about}</p>
              ) : null}
            </div>
          </div>
          <ChevronRight size={16} className="text-muted-foreground mt-2" />
        </div>

        <div className="mt-5 grid grid-cols-3 gap-2 border-t border-border/50 pt-4 text-sm">
          <div className="flex flex-col gap-1">
            <span className="text-[11px] font-medium text-muted-foreground uppercase tracking-wider">
              {t('versionCount')}
            </span>
            <span className="font-semibold text-foreground">{row.versionCount}</span>
          </div>
          <div className="flex flex-col gap-1">
            <span className="text-[11px] font-medium text-muted-foreground uppercase tracking-wider">
              {t('arch')}
            </span>
            <span className="font-semibold text-foreground">
              {row.arches.length ? row.arches.join(' · ') : '—'}
            </span>
          </div>
          <div className="flex flex-col gap-1">
            <span className="text-[11px] font-medium text-muted-foreground uppercase tracking-wider">
              {t('size')}
            </span>
            <span className="font-semibold text-foreground">
              {formatArtifactBytes(row.sizeBytes)}
            </span>
          </div>
        </div>

        <div className="mt-4 flex items-center justify-between">
          <span className="text-[11px] font-medium text-muted-foreground uppercase tracking-wider">
            {t('coverage')}
          </span>
          {row.nodesMissing == null ? (
            <span className="inline-flex items-center text-muted-foreground font-normal text-[11px] gap-1.5">
              <span className="h-1.5 w-1.5 rounded-full bg-muted-foreground/50"></span>
              {t('coverageUnavailable')}
            </span>
          ) : row.versionCount === 0 ? (
            <span className="inline-flex items-center text-muted-foreground font-normal text-[11px] gap-1.5">
              <span className="h-1.5 w-1.5 rounded-full bg-border"></span>
              {t('noArtifacts')}
            </span>
          ) : row.nodesMissing === 0 ? (
            <span className="inline-flex items-center text-muted-foreground font-medium text-[11px] gap-1.5">
              <span className="h-1.5 w-1.5 rounded-full bg-emerald-500"></span>
              {t('allCovered')}
            </span>
          ) : (
            <span className="inline-flex items-center text-muted-foreground font-medium text-[11px] gap-1.5">
              <span className="h-1.5 w-1.5 rounded-full bg-amber-500"></span>
              {t('nodesMissingCount', { count: row.nodesMissing })}
            </span>
          )}
        </div>
      </Card>
    </Link>
  );
}

function warehouseDisabledMessage(err: unknown, disabled: string): string {
  if (!(err instanceof ApiError)) {
    return '';
  }
  const code =
    err.body && typeof err.body === 'object' && 'code' in err.body
      ? String((err.body as { code?: string }).code ?? '')
      : '';
  if (err.status === 501 || code === 'warehouse_disabled') {
    return disabled;
  }
  return '';
}
