// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState } from 'react';
import { createPortal } from 'react-dom';
import { Link, useParams } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import {
  ArrowLeft,
  Download,
  Trash2,
  CheckCircle2,
  AlertCircle,
  Box,
  Disc,
  Cpu,
  Microchip,
  Layers,
  Info,
  RefreshCw,
} from 'lucide-react';
import { warehouseApi, type WarehouseArtifact } from '@/api/client';
import { ApiError } from '@/lib/api';
import { Button } from '@/components/ui/button';
import { Card } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { showToast } from '@/components/ui/ToastProvider';
import { cn, formatArtifactBytes } from '@/lib/utils';

export default function WarehouseComponentPage() {
  const { component = '' } = useParams();
  const { t } = useTranslation('warehouse');
  const qc = useQueryClient();
  const name = decodeURIComponent(component);
  const [target, setTarget] = useState<{ version: string; artifact: WarehouseArtifact } | null>(
    null,
  );

  const q = useQuery({
    queryKey: ['warehouse', 'component', name],
    queryFn: () => warehouseApi.getComponent(name),
    enabled: name !== '',
  });

  const del = useMutation({
    mutationFn: (row: { version: string; arch: string }) =>
      warehouseApi.deleteVersion(name, row.version, row.arch),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['warehouse'] });
    },
    onError: (err: unknown) => {
      showToast(err instanceof ApiError ? err.message : String(err), 'warn');
    },
  });

  const about = t(`about.${name}`, { defaultValue: '' });

  let Icon = Box;
  if (name === 'cube-image') Icon = Disc;
  if (name === 'cube-agent') Icon = Cpu;
  if (name === 'cube-kernel-scf') Icon = Microchip;

  if (q.isLoading) {
    return (
      <div className="space-y-4 pt-4">
        <Skeleton className="h-5 w-40" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }

  if (q.isError || !q.data) {
    const disabled =
      q.error instanceof ApiError &&
      (q.error.status === 501 ||
        (typeof q.error.body === 'object' &&
          q.error.body !== null &&
          'code' in q.error.body &&
          (q.error.body as { code?: string }).code === 'warehouse_disabled'));
    const msg = disabled
      ? t('disabled')
      : q.error instanceof ApiError
        ? q.error.message
        : t('error');
    return (
      <div className="space-y-4 pt-4">
        <BackLink />
        <p className="text-sm text-destructive">{msg}</p>
      </div>
    );
  }

  const versions = q.data.versions ?? [];

  return (
    <div className="animate-fade-in space-y-6 pt-4 pb-12">
      <BackLink />
      <header className="flex items-start gap-4">
        <div className="flex h-12 w-12 items-center justify-center rounded-xl bg-primary/10 text-primary shrink-0">
          <Icon size={24} />
        </div>
        <div>
          <h1 className="font-mono text-xl font-semibold tracking-tight">{q.data.name}</h1>
          {about ? <p className="mt-1 max-w-2xl text-sm text-muted-foreground">{about}</p> : null}
        </div>
      </header>

      {versions.length === 0 ? (
        <Card className="flex flex-col items-center justify-center py-16 text-center shadow-sm border-dashed">
          <div className="flex h-12 w-12 items-center justify-center rounded-full bg-muted/50 text-muted-foreground mb-4">
            <Layers size={24} />
          </div>
          <div className="text-base font-medium">{t('componentEmpty')}</div>
          <div className="mt-1 text-sm text-muted-foreground max-w-sm">
            {t('componentEmptyHint')}
          </div>
          <Link to="/warehouse" className="mt-6">
            <Button variant="outline">{t('backToWarehouse')}</Button>
          </Link>
        </Card>
      ) : (
        <div className="space-y-6">
          {versions.map((group) => (
            <Card key={group.version} className="overflow-hidden p-0 shadow-sm">
              <div className="border-b border-border/60 bg-muted/30 px-5 py-3 flex items-center justify-between">
                <div className="flex items-center gap-2">
                  <span className="inline-flex items-center rounded-md border border-border/80 px-2 py-0.5 text-sm font-mono transition-colors focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2 bg-background shadow-sm">
                    {group.version}
                  </span>
                </div>
              </div>
              <div className="overflow-x-auto">
                <table className="w-full text-sm">
                  <thead className="bg-muted/10 text-left text-xs text-muted-foreground uppercase tracking-wider">
                    <tr>
                      <th className="px-5 py-3 font-medium w-32">{t('arch')}</th>
                      <th className="px-5 py-3 font-medium w-32">{t('size')}</th>
                      <th className="px-5 py-3 font-medium w-32">{t('source')}</th>
                      <th className="px-5 py-3 font-medium min-w-[200px]">{t('installed')}</th>
                      <th className="px-5 py-3 font-medium min-w-[200px]">{t('missing')}</th>
                      <th className="px-5 py-3 w-32" />
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-border/40">
                    {group.artifacts.map((art) => (
                      <tr key={art.arch} className="hover:bg-muted/10 transition-colors">
                        <td className="px-5 py-4 font-medium text-foreground/80">{art.arch}</td>
                        <td className="px-5 py-4 text-muted-foreground">
                          {formatArtifactBytes(art.sizeBytes)}
                        </td>
                        <td className="px-5 py-4 text-muted-foreground">{art.source}</td>
                        <td className="px-5 py-4">
                          {art.nodesInstalled == null ? (
                            <span className="text-muted-foreground italic text-xs">
                              {t('coverageUnavailable')}
                            </span>
                          ) : art.nodesInstalled.length === 0 ? (
                            <span className="text-muted-foreground text-xs">—</span>
                          ) : (
                            <div className="flex flex-wrap gap-1.5">
                              {art.nodesInstalled.map((n) => (
                                <span
                                  key={n}
                                  className="inline-flex items-center rounded-md border px-1.5 py-0 text-[10px] font-mono transition-colors focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2 border-border/80 bg-background text-muted-foreground shadow-sm"
                                >
                                  {n}
                                </span>
                              ))}
                            </div>
                          )}
                        </td>
                        <td className="px-5 py-4">
                          {art.nodesMissing == null ? (
                            <span className="text-muted-foreground italic text-xs">
                              {t('coverageUnavailable')}
                            </span>
                          ) : art.nodesMissing.length === 0 ? (
                            <span className="inline-flex items-center text-muted-foreground font-medium text-[11px] gap-1.5 w-max">
                              <span className="h-1.5 w-1.5 rounded-full bg-emerald-500"></span>
                              {t('allCovered')}
                            </span>
                          ) : (
                            <div className="flex flex-wrap gap-1.5">
                              {art.nodesMissing.map((n) => (
                                <span
                                  key={n}
                                  className="inline-flex items-center rounded-md border px-1.5 py-0 text-[10px] font-mono transition-colors focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2 border-border/80 bg-background text-muted-foreground gap-1 shadow-sm"
                                >
                                  <span className="h-1 w-1 rounded-full bg-amber-500"></span>
                                  {n}
                                </span>
                              ))}
                            </div>
                          )}
                        </td>
                        <td className="px-5 py-4 text-right">
                          <div className="flex justify-end gap-2">
                            <Button
                              variant="outline"
                              size="sm"
                              className={cn(
                                'h-8 px-2 text-xs transition-colors',
                                !art.nodesMissing || art.nodesMissing.length === 0
                                  ? 'opacity-50'
                                  : 'hover:bg-primary/10 hover:text-primary hover:border-primary/30',
                              )}
                              disabled={!art.nodesMissing || art.nodesMissing.length === 0}
                              onClick={() => setTarget({ version: group.version, artifact: art })}
                            >
                              <Download size={14} className="mr-1.5" />
                              {t('preinstall')}
                            </Button>
                            <Button
                              variant="ghost"
                              size="sm"
                              className="h-8 w-8 p-0 text-muted-foreground hover:bg-destructive/10 hover:text-destructive transition-colors"
                              onClick={() => {
                                if (
                                  window.confirm(
                                    t('deleteConfirm', {
                                      component: name,
                                      version: group.version,
                                      arch: art.arch,
                                    }),
                                  )
                                ) {
                                  del.mutate({ version: group.version, arch: art.arch });
                                }
                              }}
                              title={t('delete')}
                            >
                              <Trash2 size={14} />
                              <span className="sr-only">{t('delete')}</span>
                            </Button>
                          </div>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </Card>
          ))}
        </div>
      )}

      {target && (
        <PreinstallDialog
          component={name}
          version={target.version}
          artifact={target.artifact}
          onClose={() => setTarget(null)}
        />
      )}
    </div>
  );
}

function BackLink() {
  const { t } = useTranslation('warehouse');
  return (
    <Link
      to="/warehouse"
      className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground"
    >
      <ArrowLeft className="h-4 w-4" />
      {t('backToWarehouse')}
    </Link>
  );
}

function PreinstallDialog({
  component,
  version,
  artifact,
  onClose,
}: {
  component: string;
  version: string;
  artifact: WarehouseArtifact;
  onClose: () => void;
}) {
  const { t } = useTranslation('warehouse');
  const qc = useQueryClient();
  const missing = artifact.nodesMissing ?? [];
  const [selected, setSelected] = useState<string[]>(missing);

  const mut = useMutation({
    mutationFn: () =>
      warehouseApi.preinstall({
        nodeIds: selected,
        arch: artifact.arch,
        component,
        version,
      }),
    onSuccess: () => {
      showToast(t('preinstallHint'), 'success');
      void qc.invalidateQueries({ queryKey: ['warehouse'] });
      onClose();
    },
    onError: (err: unknown) =>
      showToast(err instanceof ApiError ? err.message : String(err), 'warn'),
  });

  return createPortal(
    <div
      className="fixed inset-0 z-[100] flex items-center justify-center bg-foreground/40 backdrop-blur-sm transition-all"
      onClick={onClose}
    >
      <Card
        className="w-[480px] p-0 overflow-hidden shadow-2xl border-0 sm:rounded-xl animate-fade-in"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="bg-muted/30 px-6 py-4 border-b border-border/50">
          <h2 className="text-lg font-semibold">{t('preinstall')}</h2>
          <div className="flex items-center gap-2 mt-1.5">
            <span className="inline-flex items-center rounded-md border border-border/80 px-2 py-0.5 text-xs font-mono transition-colors focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2 bg-background">
              {component}
            </span>
            <span className="text-muted-foreground text-xs">/</span>
            <span className="inline-flex items-center rounded-md border px-2 py-0.5 text-xs font-mono transition-colors focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2 bg-muted/50 text-muted-foreground">
              {version}
            </span>
            <span className="text-muted-foreground text-xs">/</span>
            <span className="font-mono text-xs text-muted-foreground">{artifact.arch}</span>
          </div>
        </div>

        <div className="p-6 space-y-4">
          <div className="flex items-center gap-2 rounded-md bg-muted/30 border border-border/50 px-3 py-2.5">
            <span className="h-1.5 w-1.5 rounded-full bg-blue-500"></span>
            <span className="text-xs text-muted-foreground">{t('preinstallHint')}</span>
          </div>

          <div>
            <div className="flex items-center justify-between mb-3">
              <label className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                {t('selectNodes')}
              </label>
              <div className="text-xs text-muted-foreground">
                {selected.length} / {missing.length} selected
              </div>
            </div>

            <div className="max-h-56 overflow-y-auto pr-2 space-y-1.5 custom-scrollbar">
              {missing.map((n) => {
                const isSelected = selected.includes(n);
                return (
                  <label
                    key={n}
                    className={cn(
                      'flex items-center gap-3 p-2.5 rounded-lg border transition-all cursor-pointer select-none group',
                      isSelected
                        ? 'border-primary/30 bg-muted/20 text-foreground'
                        : 'border-border/60 hover:bg-muted/30 text-muted-foreground hover:text-foreground',
                    )}
                  >
                    <div
                      className={cn(
                        'flex h-4 w-4 shrink-0 items-center justify-center rounded border transition-colors',
                        isSelected
                          ? 'bg-primary border-primary text-primary-foreground'
                          : 'border-input bg-background group-hover:border-primary/30',
                      )}
                    >
                      {isSelected && <CheckCircle2 size={12} strokeWidth={3} />}
                    </div>
                    <input
                      type="checkbox"
                      className="hidden"
                      checked={isSelected}
                      onChange={(e) => {
                        setSelected((cur) =>
                          e.target.checked ? [...cur, n] : cur.filter((x) => x !== n),
                        );
                      }}
                    />
                    <span className="font-mono text-sm">{n}</span>
                  </label>
                );
              })}
            </div>
          </div>
        </div>

        <div className="bg-muted/30 px-6 py-4 border-t border-border/50 flex justify-end gap-3">
          <Button variant="outline" onClick={onClose} className="h-9">
            {t('cancel')}
          </Button>
          <Button
            disabled={selected.length === 0 || mut.isPending}
            onClick={() => mut.mutate()}
            className="h-9"
          >
            {mut.isPending ? (
              <RefreshCw size={14} className="mr-2 animate-spin" />
            ) : (
              <Download size={14} className="mr-2" />
            )}
            {t('preinstall')}
          </Button>
        </div>
      </Card>
    </div>,
    document.body,
  );
}
