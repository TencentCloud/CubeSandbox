// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Link, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import * as Dialog from '@radix-ui/react-dialog';
import { clusterApi, sandboxApi, templateApi } from '@/api/client';
import { ApiError } from '@/lib/api';
import type { QuotaViewDto, QuotaSpecDto } from '@/api/client';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { showToast } from '@/components/ui/ToastProvider';
import {
  ArrowLeft,
  Package,
  Box,
  Activity,
  ShieldAlert,
  ShieldCheck,
  X,
  History,
} from 'lucide-react';
import { cn, formatRelative } from '@/lib/utils';

// ── Resource bar ──────────────────────────────────────────────────────────────

function ResourceBar({ pct }: { pct: number }) {
  const color = pct > 85 ? 'bg-cube-err' : pct > 65 ? 'bg-cube-warn' : 'bg-cube-ok';
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
  const color = pct > 85 ? 'text-cube-err' : pct > 65 ? 'text-cube-warn' : 'text-foreground';

  return (
    <div className="rounded-xl border border-border/60 bg-card/40 p-4 space-y-3">
      <div className="text-xs text-muted-foreground tracking-wider uppercase font-medium">
        {label}
      </div>
      <div className="flex items-end justify-between gap-2">
        <span className={cn('text-3xl font-semibold tabular-nums leading-none', color)}>
          {pct}
          <span className="text-base font-normal text-muted-foreground ml-0.5">%</span>
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

function Section({
  title,
  children,
  action,
}: {
  title: string;
  children: React.ReactNode;
  action?: React.ReactNode;
}) {
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between">
        <span className="text-sm font-medium uppercase tracking-wider text-muted-foreground">
          {title}
        </span>
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

function ConditionRow({
  type,
  status,
  reason,
  message,
  time,
}: {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  time?: string | null;
}) {
  const ok = status === 'True';
  return (
    <div className="flex items-start justify-between gap-4 py-2.5 border-b border-white/5 last:border-0">
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span
            className={cn(
              'inline-block h-1.5 w-1.5 rounded-full shrink-0',
              ok ? 'bg-cube-ok' : 'bg-cube-warn',
            )}
          />
          <span className="text-base font-medium">{type}</span>
          <span
            className={cn(
              'text-xs font-medium px-1.5 py-0.5 rounded border',
              ok
                ? 'text-cube-ok border-cube-ok/30 bg-cube-ok/5'
                : 'text-cube-warn border-cube-warn/30 bg-cube-warn/5',
            )}
          >
            {status}
          </span>
        </div>
        {reason && <p className="mt-0.5 text-sm text-muted-foreground pl-3.5">{reason}</p>}
        {message && (
          <p className="mt-0.5 text-xs text-muted-foreground/60 break-all pl-3.5">{message}</p>
        )}
      </div>
      {time && (
        <span className="shrink-0 text-xs text-muted-foreground/60 mt-0.5">
          {formatRelative(time)}
        </span>
      )}
    </div>
  );
}

// ── Quota settings ────────────────────────────────────────────────────────────

interface QuotaFormState {
  mcpu: string;
  mem: string;
  mvm: string;
  concurrent: string;
}

function quotaFormFromView(view?: QuotaViewDto): QuotaFormState {
  // Seed from the effective values so untouched fields round-trip unchanged.
  const a = view?.actual;
  return {
    mcpu: a?.milli_cpu ? String(a.milli_cpu) : '',
    mem: a?.mem_mb ? `${a.mem_mb}Mi` : '',
    mvm: a?.max_mvm_num ? String(a.max_mvm_num) : '',
    concurrent: a?.create_concurrent_num ? String(a.create_concurrent_num) : '',
  };
}

function DiffRow({
  label,
  oldVal,
  newVal,
  changed = true,
}: {
  label: string;
  oldVal: string;
  newVal: string;
  changed?: boolean;
}) {
  return (
    <div className="flex items-baseline gap-2 text-xs">
      <span className="shrink-0 text-muted-foreground/70">{label}</span>
      <span className="min-w-0 flex-1 break-all text-right font-mono">
        <span className={changed ? 'text-muted-foreground/80' : 'text-muted-foreground/50'}>
          {oldVal}
        </span>
        <span className="mx-1.5 text-muted-foreground/40">→</span>
        <span className={changed ? 'font-medium text-foreground' : 'text-muted-foreground/50'}>
          {newVal}
        </span>
      </span>
    </div>
  );
}

// QuotaHistoryDetail renders an audit detail as field-level diffs.
function QuotaHistoryDetail({ detail }: { detail: string }) {
  const { t } = useTranslation('nodeDetail');

  let rec: Record<string, unknown>;
  try {
    rec = JSON.parse(detail) as Record<string, unknown>;
  } catch {
    return <p className="break-all font-mono text-xs text-muted-foreground/80">{detail}</p>;
  }

  const valueLabel = (key: string, v: unknown): string => {
    if (v === null || v === undefined) return t('quota.valueInherit');
    if (v === '') return t('quota.valueUnset');
    if (key === 'node_managed') return v ? t('quota.managedNode') : t('quota.managedCluster');
    if (typeof v === 'number' && v === 0 && key !== 'revision') return t('quota.valueUnset');
    return String(v);
  };

  // Cluster-level record: scalar default change plus fan-out outcome.
  if (typeof rec.new !== 'object' || rec.new === null) {
    const scalar = (v: unknown) =>
      v === null || v === undefined ? t('quota.valueNotSet') : String(v);
    return (
      <div className="space-y-1">
        <DiffRow
          label={t('quota.clusterDefault')}
          oldVal={scalar(rec.old)}
          newVal={scalar(rec.new)}
        />
        <p className="text-xs text-muted-foreground/60">
          {t('quota.propagation', {
            bumped: String(rec.bumped ?? 0),
            pushed: String(rec.pushed ?? 0),
            failed: String(rec.push_failed ?? 0),
          })}
        </p>
      </div>
    );
  }

  const labels: Record<string, string> = {
    mcpu_limit: t('quota.cpu'),
    mem_limit: t('quota.mem'),
    mvm_limit: t('quota.mvm'),
    creation_concurrent_num: t('quota.concurrent'),
    paused_release_ratio: t('quota.ratio'),
    node_managed: t('quota.managedLabel'),
    revision: t('quota.revisionLabel'),
  };
  const next = rec.new as Record<string, unknown>;
  const prev = (typeof rec.old === 'object' && rec.old !== null ? rec.old : {}) as Record<
    string,
    unknown
  >;
  return (
    <div className="space-y-0.5">
      {Object.entries(next)
        .filter(([k]) => labels[k])
        .map(([k, v]) => {
          const oldVal = valueLabel(k, prev[k]);
          const newVal = valueLabel(k, v);
          return (
            <DiffRow
              key={k}
              label={labels[k]}
              oldVal={oldVal}
              newVal={newVal}
              changed={oldVal !== newVal}
            />
          );
        })}
    </div>
  );
}

function QuotaSettingsSection({ nodeID }: { nodeID: string }) {
  const { t, i18n } = useTranslation('nodeDetail');
  const queryClient = useQueryClient();

  const { data: view } = useQuery({
    queryKey: ['node-quota', nodeID],
    queryFn: () => clusterApi.nodeQuota(nodeID),
    refetchInterval: 15_000,
  });

  const [form, setForm] = useState<QuotaFormState>(() => quotaFormFromView(undefined));
  const [dirty, setDirty] = useState(false);
  // Only edited fields are submitted; the API merges absent fields into the stored spec.
  const [dirtyFields, setDirtyFields] = useState<Set<keyof QuotaFormState>>(new Set());
  // The seeding revision, submitted as a CAS token to reject concurrent writes instead of overwriting.
  const [baseRevision, setBaseRevision] = useState<number | undefined>();

  // Re-seed the form while untouched; a save always re-seeds via invalidate.
  useEffect(() => {
    if (!dirty) {
      setForm(quotaFormFromView(view));
      setBaseRevision(view?.spec?.revision);
    }
  }, [view, dirty]);

  const [historyOpen, setHistoryOpen] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const { data: history } = useQuery({
    queryKey: ['node-quota-history', nodeID],
    queryFn: () => clusterApi.nodeQuotaHistory(nodeID),
    enabled: historyOpen,
    staleTime: 5_000,
  });

  const saveMutation = useMutation({
    mutationFn: () => {
      // Submit only the fields the user edited; the API merges absent fields
      // onto the stored spec, so untouched fields keep their desired values.
      const patch: QuotaSpecDto = {
        revision: 0,
        expected_revision: baseRevision ?? 0,
      };
      if (dirtyFields.has('mcpu')) patch.mcpu_limit = Number(normalizeField('mcpu'));
      if (dirtyFields.has('mem')) patch.mem_limit = normalizeField('mem');
      if (dirtyFields.has('mvm')) patch.mvm_limit = Number(normalizeField('mvm'));
      if (dirtyFields.has('concurrent'))
        patch.creation_concurrent_num = Number(normalizeField('concurrent'));
      return clusterApi.setNodeQuota(nodeID, patch);
    },
    onSuccess: ({ push }) => {
      setDirty(false);
      setDirtyFields(new Set());
      queryClient.invalidateQueries({ queryKey: ['node-quota', nodeID] });
      if (push.applied) {
        showToast(t('quota.savedApplied'));
      } else {
        showToast(t('quota.savedSkipped', { reason: push.skip_reason ?? '' }), 'warn');
      }
      // The heartbeat needs ~3s to report the new actual; refresh once more.
      setTimeout(() => queryClient.invalidateQueries({ queryKey: ['node-quota', nodeID] }), 4_000);
    },
    onError: (err: Error) => {
      if (err instanceof ApiError && err.status === 409) {
        showToast(t('quota.conflictOnSave'), 'warn');
        return;
      }
      showToast(
        err.message
          ? t('quota.saveFailedWithReason', { message: err.message })
          : t('quota.saveFailed'),
        'warn',
      );
    },
  });

  const set = (key: keyof QuotaFormState) => (e: React.ChangeEvent<HTMLInputElement>) => {
    setDirty(true);
    setDirtyFields((prev) => {
      const next = new Set(prev);
      next.add(key);
      return next;
    });
    setForm((f) => ({ ...f, [key]: e.target.value }));
  };

  // Mirrors the backend: numeric fields are non-negative integers, mem is a
  // decimal quantity with a binary suffix. Returns an error message or null.
  const validateForm = (): string | null => {
    const numeric: [keyof QuotaFormState, string][] = [
      ['mcpu', t('quota.cpu')],
      ['mvm', t('quota.mvm')],
      ['concurrent', t('quota.concurrent')],
    ];
    for (const [key, label] of numeric) {
      const v = form[key].trim();
      if (v && !/^\d+$/.test(v)) {
        return t('quota.invalidInteger', { field: label });
      }
    }
    const mem = form.mem.trim();
    if (mem && !/^\d+(\.\d+)?(Ki|Mi|Gi|Ti)$/.test(mem)) {
      return t('quota.invalidMem');
    }
    return null;
  };

  const unset = view?.drift === 'no_spec';
  const driftDetected = view?.drift === 'detected';

  // A newer spec revision arrived after the form was seeded: someone else
  // changed the quota. Saving would silently overwrite their write, so block it.
  const staleRevision =
    dirty && baseRevision !== undefined && view?.spec?.revision !== baseRevision;

  // Normalize a form field exactly as the request body does: numeric fields
  // become Number(...) || 0, mem keeps its trimmed string. The diff and the
  // payload both derive from this so the confirmation dialog never disagrees
  // with what is actually submitted.
  const normalizeField = (key: keyof QuotaFormState): string =>
    key === 'mem' ? form[key].trim() : String(Number(form[key]) || 0);

  // Changed fields between the stored spec and the edited form, listed in the
  // save confirmation dialog.
  const baseline = quotaFormFromView(view);
  const changeRows = (
    [
      { key: 'mcpu', label: t('quota.cpu') },
      { key: 'mem', label: t('quota.mem') },
      { key: 'mvm', label: t('quota.mvm') },
      { key: 'concurrent', label: t('quota.concurrent') },
    ] as const
  ).flatMap(({ key, label }) => {
    const normalized = normalizeField(key);
    const oldVal = baseline[key] || t('quota.valueUnset');
    // Empty and 0 both mean "reset to cubelet default", so show the same label.
    const newVal = normalized === '' || normalized === '0' ? t('quota.valueUnset') : normalized;
    return oldVal !== newVal ? [{ key, label, oldVal, newVal }] : [];
  });

  return (
    <Section
      title={t('quota.title')}
      action={
        <div className="flex items-center gap-3">
          <span
            className={cn(
              'inline-block h-2 w-2 rounded-full',
              driftDetected ? 'bg-amber-500' : unset ? 'bg-muted-foreground/40' : 'bg-cube-ok',
            )}
            title={
              driftDetected
                ? t('quota.driftDetected', { message: view?.message ?? '' })
                : unset
                  ? t('quota.driftUnmanaged')
                  : t('quota.driftSynced')
            }
          />
          <button
            className="text-muted-foreground hover:text-foreground transition-colors"
            title={t('quota.history')}
            onClick={() => setHistoryOpen(true)}
          >
            <History size={14} />
          </button>
        </div>
      }
    >
      <div className="rounded-xl border border-border/60 bg-card/40 px-6 py-5 space-y-4">
        {unset && (
          <>
            <p className="text-sm text-muted-foreground">
              {t('quota.unset')}
              {view && (
                <span className="text-muted-foreground/60 ml-1">
                  {t('quota.unsetRef', {
                    cpu: view.actual.milli_cpu,
                    mem: view.actual.mem_mb,
                  })}
                </span>
              )}
            </p>
            <p className="flex items-start gap-2 text-sm text-amber-600">
              <span className="mt-1 inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-amber-500" />
              <span>{t('quota.firstManagedHint')}</span>
            </p>
          </>
        )}

        {driftDetected && (
          <p className="flex items-start gap-2 text-sm text-amber-600">
            <span className="mt-1 inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-amber-500" />
            <span>{t('quota.driftDetected', { message: view?.message ?? '' })}</span>
          </p>
        )}

        <div className="grid grid-cols-2 gap-4 lg:grid-cols-4">
          {(
            [
              {
                key: 'mcpu',
                label: t('quota.cpu'),
                placeholder: String(view?.actual.milli_cpu ?? 128000),
              },
              {
                key: 'mem',
                label: t('quota.mem'),
                placeholder: `${view?.actual.mem_mb ?? 262144}Mi`,
                hint: 'Ki / Mi / Gi / Ti',
              },
              {
                key: 'mvm',
                label: t('quota.mvm'),
                placeholder: String(view?.actual.max_mvm_num ?? 500),
              },
              {
                key: 'concurrent',
                label: t('quota.concurrent'),
                placeholder: String(view?.actual.create_concurrent_num ?? 32),
              },
            ] as {
              key: keyof QuotaFormState;
              label: string;
              placeholder: string;
              hint?: string;
            }[]
          ).map(({ key, label, placeholder, hint }) => (
            <div key={key} className="space-y-1.5">
              <label className="block text-xs uppercase tracking-wider text-muted-foreground/70 font-medium">
                {label}
              </label>
              <Input
                value={form[key]}
                onChange={set(key)}
                placeholder={placeholder}
                className="h-9"
              />
              {hint && <p className="text-[10px] text-muted-foreground/50">{hint}</p>}
            </div>
          ))}
        </div>

        <div className="flex items-center justify-between gap-3 pt-1">
          <p className="text-xs text-muted-foreground/60">
            {staleRevision ? t('quota.conflictStale') : t('quota.hint')}
          </p>
          <Button
            size="sm"
            disabled={!dirty || !view || saveMutation.isPending || staleRevision}
            onClick={() => {
              const err = validateForm();
              if (err) {
                showToast(err, 'warn');
                return;
              }
              setConfirmOpen(true);
            }}
          >
            {saveMutation.isPending ? t('quota.saving') : t('quota.save')}
          </Button>
        </div>
      </div>

      <Dialog.Root open={historyOpen} onOpenChange={setHistoryOpen}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/40 z-40" />
          <Dialog.Content className="fixed left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 z-50 w-full max-w-2xl rounded-lg border border-border bg-background p-5 shadow-lg">
            <div className="flex items-center justify-between mb-4">
              <Dialog.Title className="text-base font-semibold">
                {t('quota.historyTitle')}
              </Dialog.Title>
              <Dialog.Close asChild>
                <button className="text-muted-foreground hover:text-foreground">
                  <X size={16} />
                </button>
              </Dialog.Close>
            </div>
            <div className="space-y-2 max-h-[28rem] overflow-y-auto pr-1">
              {history && history.length > 0 ? (
                history.map((e, i) => (
                  <div
                    key={i}
                    className="rounded-md border border-border/50 px-3.5 py-2.5 space-y-1.5"
                  >
                    <div className="flex items-center gap-2">
                      <span className="text-sm font-medium">{e.operator}</span>
                      <span className="text-xs text-muted-foreground/70 ml-auto">
                        {new Date(e.created_at).toLocaleString(i18n.language)}
                      </span>
                    </div>
                    {e.detail && <QuotaHistoryDetail detail={e.detail} />}
                  </div>
                ))
              ) : (
                <p className="text-sm text-muted-foreground text-center py-4">
                  {t('quota.historyEmpty')}
                </p>
              )}
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>

      <Dialog.Root open={confirmOpen} onOpenChange={setConfirmOpen}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/40 z-40" />
          <Dialog.Content className="fixed left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 z-50 w-full max-w-md rounded-lg border border-border bg-background p-5 shadow-lg">
            <div className="flex items-center justify-between mb-3">
              <Dialog.Title className="text-base font-semibold">
                {t('quota.confirmTitle')}
              </Dialog.Title>
              <Dialog.Close asChild>
                <button className="text-muted-foreground hover:text-foreground">
                  <X size={16} />
                </button>
              </Dialog.Close>
            </div>
            <Dialog.Description className="text-sm text-muted-foreground mb-4">
              {t('quota.confirmDesc', { node: nodeID })}
            </Dialog.Description>
            {unset && (
              <p className="flex items-start gap-2 text-sm text-amber-600 mb-4">
                <span className="mt-1 inline-block h-1.5 w-1.5 shrink-0 rounded-full bg-amber-500" />
                <span>{t('quota.firstManagedConfirm')}</span>
              </p>
            )}
            {changeRows.length > 0 ? (
              <div className="space-y-2 mb-5">
                {changeRows.map((r) => (
                  <div key={r.key} className="rounded-md border border-border/50 px-3 py-2">
                    <div className="mb-1 text-xs font-medium uppercase tracking-wider text-muted-foreground/70">
                      {r.label}
                    </div>
                    <div className="flex items-baseline gap-2 font-mono text-sm break-all">
                      <span className="text-muted-foreground/80">{r.oldVal}</span>
                      <span className="text-muted-foreground/40">→</span>
                      <span className="font-medium text-foreground">{r.newVal}</span>
                    </div>
                  </div>
                ))}
              </div>
            ) : (
              <p className="mb-5 text-sm text-muted-foreground">{t('quota.confirmNoChange')}</p>
            )}
            <div className="flex justify-end gap-2">
              <Dialog.Close asChild>
                <Button size="sm" variant="outline">
                  {t('dialog.cancel')}
                </Button>
              </Dialog.Close>
              <Button
                size="sm"
                disabled={changeRows.length === 0 || saveMutation.isPending}
                onClick={() => {
                  setConfirmOpen(false);
                  saveMutation.mutate();
                }}
              >
                {saveMutation.isPending ? t('quota.saving') : t('quota.confirm')}
              </Button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>
    </Section>
  );
}

// ── Main page ─────────────────────────────────────────────────────────────────

export default function NodeDetailPage() {
  const { nodeID } = useParams<{ nodeID: string }>();
  const { t, i18n } = useTranslation('nodeDetail');
  const queryClient = useQueryClient();

  const { data, isLoading, isError } = useQuery({
    queryKey: ['node', nodeID],
    queryFn: () => clusterApi.node(nodeID!),
    enabled: !!nodeID,
    refetchInterval: 10_000,
  });

  const [isolationDialogOpen, setIsolationDialogOpen] = useState(false);
  const [isolationDetail, setIsolationDetail] = useState('');

  const isolationMutation = useMutation({
    mutationFn: async ({ disabled, detail }: { disabled: boolean; detail: string }) => {
      if (disabled) {
        await clusterApi.isolate(nodeID!, detail || undefined);
      } else {
        await clusterApi.unisolate(nodeID!, detail || undefined);
      }
    },
    onSuccess: (_, { disabled }) => {
      queryClient.invalidateQueries({ queryKey: ['node', nodeID] });
      queryClient.invalidateQueries({ queryKey: ['nodes'] });
      showToast(disabled ? t('toast.isolated') : t('toast.unisolated'));
      setIsolationDialogOpen(false);
      setIsolationDetail('');
    },
    onError: (err: Error) => {
      showToast(err.message || t('toast.isolationFailed'), 'warn');
    },
  });

  const { data: allSandboxes, isLoading: sandboxesLoading } = useQuery({
    queryKey: ['sandboxes'],
    queryFn: () => sandboxApi.list(),
    refetchInterval: 10_000,
    enabled: !!data,
  });

  const [opsDialogOpen, setOpsDialogOpen] = useState(false);
  const { data: operations } = useQuery({
    queryKey: ['node-operations', nodeID],
    queryFn: () => clusterApi.nodeOperations(nodeID!),
    enabled: !!nodeID && opsDialogOpen,
    staleTime: 5_000,
  });

  const { data: allTemplates } = useQuery({
    queryKey: ['templates'],
    queryFn: () => templateApi.list(),
    staleTime: 30_000,
    enabled: !!data,
  });

  // local templates with READY or RUNNING status only
  const localTemplateIDs = new Set(data?.localTemplates ?? []);
  const visibleLocalTemplates = (allTemplates ?? []).filter(
    (t) =>
      localTemplateIDs.has(t.templateID) &&
      ['READY', 'RUNNING'].includes((t.status ?? '').toUpperCase()),
  );

  const nodeSandboxes = (allSandboxes ?? []).filter((sb) => sb.clientID === data?.nodeID);

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
        <Link
          to="/nodes"
          className="flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground mb-6"
        >
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
      <Link
        to="/nodes"
        className="inline-flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground transition-colors"
      >
        <ArrowLeft className="h-4 w-4" /> {t('backToNodes')}
      </Link>

      {/* header */}
      <div className="flex items-start justify-between gap-4">
        <div className="space-y-1.5">
          <div className="flex items-center gap-2.5">
            {/* live indicator */}
            <span className="relative flex h-2 w-2">
              {isReady && (
                <span className="animate-ping absolute inline-flex h-full w-full rounded-full bg-cube-ok opacity-60" />
              )}
              <span
                className={cn(
                  'relative inline-flex rounded-full h-2 w-2',
                  isReady ? 'bg-cube-ok' : 'bg-cube-warn',
                )}
              />
            </span>
            <h1 className="text-2xl font-semibold tracking-tight">
              {data.hostname ?? data.nodeID}
            </h1>
            {data.schedulingDisabled ? (
              <Badge tone="warn" className="gap-1">
                <ShieldAlert size={12} /> {t('status.isolated')}
              </Badge>
            ) : (
              <Badge tone="ok" className="gap-1">
                <ShieldCheck size={12} /> {t('status.notIsolated')}
              </Badge>
            )}
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
        <div className="flex items-center gap-3 shrink-0 pt-1">
          <button
            className="text-muted-foreground hover:text-foreground transition-colors"
            title={t('operations.title')}
            onClick={() => setOpsDialogOpen(true)}
          >
            <History size={16} />
          </button>
          <Button
            size="sm"
            variant={data.schedulingDisabled ? 'default' : 'destructive'}
            onClick={() => {
              setIsolationDetail('');
              setIsolationDialogOpen(true);
            }}
            disabled={isolationMutation.isPending}
          >
            {data.schedulingDisabled ? t('actions.unisolate') : t('actions.isolate')}
          </Button>
          <Dialog.Root open={isolationDialogOpen} onOpenChange={setIsolationDialogOpen}>
            <Dialog.Portal>
              <Dialog.Overlay className="fixed inset-0 bg-black/40 z-40" />
              <Dialog.Content className="fixed left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 z-50 w-full max-w-md rounded-lg border border-border bg-background p-5 shadow-lg">
                <div className="flex items-center justify-between mb-4">
                  <Dialog.Title className="text-base font-semibold">
                    {data.schedulingDisabled ? t('actions.unisolate') : t('actions.isolate')}
                  </Dialog.Title>
                  <Dialog.Close asChild>
                    <button className="text-muted-foreground hover:text-foreground">
                      <X size={16} />
                    </button>
                  </Dialog.Close>
                </div>
                <Dialog.Description className="text-sm text-muted-foreground mb-4">
                  {data.schedulingDisabled ? t('dialog.unisolateDesc') : t('dialog.isolateDesc')}
                </Dialog.Description>
                <label className="block text-sm font-medium mb-1.5">
                  {t('dialog.detailLabel')}
                </label>
                <Input
                  value={isolationDetail}
                  onChange={(e) => setIsolationDetail(e.target.value.slice(0, 200))}
                  placeholder={t('dialog.detailPlaceholder')}
                  maxLength={200}
                  className="mb-1"
                />
                <div className="flex justify-end mb-4">
                  <span className="text-xs text-muted-foreground">
                    {isolationDetail.length}/200
                  </span>
                </div>
                <div className="flex justify-end gap-2">
                  <Dialog.Close asChild>
                    <Button size="sm" variant="outline">
                      {t('dialog.cancel')}
                    </Button>
                  </Dialog.Close>
                  <Button
                    size="sm"
                    variant={data.schedulingDisabled ? 'default' : 'destructive'}
                    disabled={isolationMutation.isPending}
                    onClick={() =>
                      isolationMutation.mutate({
                        disabled: !data.schedulingDisabled,
                        detail: isolationDetail,
                      })
                    }
                  >
                    {data.schedulingDisabled ? t('actions.unisolate') : t('actions.isolate')}
                  </Button>
                </div>
              </Dialog.Content>
            </Dialog.Portal>
          </Dialog.Root>
          <Dialog.Root open={opsDialogOpen} onOpenChange={setOpsDialogOpen}>
            <Dialog.Portal>
              <Dialog.Overlay className="fixed inset-0 bg-black/40 z-40" />
              <Dialog.Content className="fixed left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 z-50 w-full max-w-lg rounded-lg border border-border bg-background p-5 shadow-lg">
                <div className="flex items-center justify-between mb-4">
                  <Dialog.Title className="text-base font-semibold">
                    {t('operations.title')}
                  </Dialog.Title>
                  <Dialog.Close asChild>
                    <button className="text-muted-foreground hover:text-foreground">
                      <X size={16} />
                    </button>
                  </Dialog.Close>
                </div>
                <div className="space-y-2 max-h-80 overflow-y-auto">
                  {operations && operations.length > 0 ? (
                    operations.slice(0, 6).map((op) => (
                      <div
                        key={op.id}
                        className="flex items-start gap-3 rounded-md border border-border/50 px-3 py-2"
                      >
                        <div className="flex-1 min-w-0">
                          <div className="flex items-center gap-2">
                            <span className="text-sm font-medium">
                              {op.type === 'isolate'
                                ? t('operations.typeIsolate')
                                : op.type === 'unisolate'
                                  ? t('operations.typeUnisolate')
                                  : op.type}
                            </span>
                            {op.operator && (
                              <span className="text-xs text-muted-foreground">{op.operator}</span>
                            )}
                            <span className="text-xs text-muted-foreground/70 ml-auto">
                              {new Date(op.created_at).toLocaleString(i18n.language)}
                            </span>
                          </div>
                          {op.detail && (
                            <p className="text-xs text-muted-foreground mt-1 break-all">
                              {op.detail}
                            </p>
                          )}
                        </div>
                      </div>
                    ))
                  ) : (
                    <p className="text-sm text-muted-foreground text-center py-4">
                      {t('operations.empty')}
                    </p>
                  )}
                </div>
              </Dialog.Content>
            </Dialog.Portal>
          </Dialog.Root>
          <Activity size={13} className="text-muted-foreground/50" />
          <span className="text-sm text-muted-foreground">
            {formatRelative(data.heartbeatTime)}
          </span>
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
            {
              label: t('fields.allocCpu'),
              value: `${(data.resources.allocatableCpuMilli / 1000).toFixed(1)}`,
              unit: 'cores',
            },
            {
              label: t('fields.allocMem'),
              value: `${(data.resources.allocatableMemoryMB / 1024).toFixed(1)}`,
              unit: 'GiB',
            },
            { label: t('fields.maxMvmSlots'), value: String(data.resources.maxMvmSlots), unit: '' },
          ].map(({ label, value, unit }) => (
            <div key={label} className="flex flex-col gap-1 px-5 first:pl-0 last:pr-0">
              <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium">
                {label}
              </span>
              <span className="text-xl font-semibold tabular-nums">
                {value}
                {unit && (
                  <span className="text-sm font-normal text-muted-foreground ml-1.5">{unit}</span>
                )}
              </span>
            </div>
          ))}
        </div>
      </Section>

      {/* quota settings */}
      <QuotaSettingsSection nodeID={data.nodeID} />

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
                    <span className="font-mono text-xs text-muted-foreground/60">
                      {v.commit.slice(0, 12)}
                    </span>
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
          nodeSandboxes.length > 0 ? (
            <span className="text-sm text-muted-foreground">{nodeSandboxes.length} running</span>
          ) : undefined
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
                  <span className="font-mono text-sm text-foreground/80 truncate">
                    {sb.sandboxID}
                  </span>
                  <span className="text-sm text-muted-foreground/50 truncate hidden sm:block">
                    {sb.templateID}
                  </span>
                </div>
                <div className="flex items-center gap-3 shrink-0">
                  <span
                    className={cn(
                      'inline-flex items-center gap-1.5 text-sm font-medium',
                      sb.state === 'running'
                        ? 'text-cube-ok'
                        : sb.state === 'paused'
                          ? 'text-cube-warn'
                          : 'text-muted-foreground',
                    )}
                  >
                    <span
                      className={cn(
                        'h-1.5 w-1.5 rounded-full',
                        sb.state === 'running'
                          ? 'bg-cube-ok'
                          : sb.state === 'paused'
                            ? 'bg-cube-warn'
                            : 'bg-muted-foreground',
                      )}
                    />
                    {sb.state}
                  </span>
                  <span className="text-sm text-muted-foreground/60">
                    {formatRelative(sb.startedAt)}
                  </span>
                </div>
              </Link>
            ))}
          </div>
        )}
      </Section>
    </div>
  );
}
