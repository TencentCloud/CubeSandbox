// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import * as Dialog from '@radix-ui/react-dialog';
import { AlertTriangle, Cpu, Server } from 'lucide-react';
import { clusterApi, machineTypeApi, type MachineTypeView } from '@/api/client';
import { NodesHeaderTabs } from '@/components/nodes/NodesHeaderTabs';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { showToast } from '@/components/ui/ToastProvider';
import { validateMachineTypeValue } from '@/lib/nodeLabels';
import { cn } from '@/lib/utils';

function shortHash(value: string) {
  if (!value) return '';
  return value.length > 12 ? `${value.slice(0, 12)}…` : value;
}

export default function MachineTypesPage() {
  const { t } = useTranslation('nodes');
  const queryClient = useQueryClient();
  const typesQuery = useQuery({
    queryKey: ['machine-types'],
    queryFn: machineTypeApi.list,
    refetchInterval: 15_000,
  });
  const nodesQuery = useQuery({
    queryKey: ['nodes'],
    queryFn: clusterApi.nodes,
    refetchInterval: 15_000,
  });
  const types = typesQuery.data ?? [];
  const nodes = nodesQuery.data ?? [];

  const [createOpen, setCreateOpen] = useState(false);
  const [editing, setEditing] = useState<MachineTypeView | null>(null);
  const [managing, setManaging] = useState<MachineTypeView | null>(null);
  const [deleting, setDeleting] = useState<MachineTypeView | null>(null);

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ['machine-types'] });
    queryClient.invalidateQueries({ queryKey: ['nodes'] });
  };

  return (
    <div className="animate-fade-in space-y-5">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <NodesHeaderTabs
          subtitle={t('machineTypes.subtitle')}
          nodeCount={nodesQuery.data?.length}
          machineTypeCount={types.length}
        />
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          {t('machineTypes.create')}
        </Button>
      </div>

      {types.length === 0 && !typesQuery.isLoading && (
        <Card>
          <div className="py-16 text-center text-sm text-muted-foreground">
            {t('machineTypes.empty')}
          </div>
        </Card>
      )}

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
        {types.map((mt) => (
          <Card key={mt.name} className="space-y-4">
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="flex flex-wrap items-center gap-2">
                  <h2 className="text-base font-semibold">{mt.displayName || mt.name}</h2>
                  <Badge tone="mute" className="font-mono text-[11px]">
                    {mt.name}
                  </Badge>
                </div>
                {mt.description && (
                  <p className="mt-1 text-sm text-muted-foreground">{mt.description}</p>
                )}
              </div>
              <span className="shrink-0 text-sm text-muted-foreground text-num">
                {t('machineTypes.nodeCount', { healthy: mt.healthyNodeCount, total: mt.nodeCount })}
              </span>
            </div>

            {mt.warnings.includes('multiple_cpuid') && (
              <p className="flex items-start gap-2 rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-sm text-amber-700 dark:text-amber-300">
                <AlertTriangle size={14} className="mt-0.5 shrink-0" />
                <span>{t('machineTypes.cpuWarning')}</span>
              </p>
            )}
            {mt.warnings.includes('missing_host_facts') && (
              <p className="flex items-start gap-2 rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-sm text-amber-700 dark:text-amber-300">
                <AlertTriangle size={14} className="mt-0.5 shrink-0" />
                <span>{t('machineTypes.factsWarning')}</span>
              </p>
            )}

            <div className="grid gap-3 text-xs sm:grid-cols-2">
              <div>
                <div className="mb-1 flex items-center gap-1 text-muted-foreground">
                  <Cpu size={12} /> {t('machineTypes.cpus')}
                </div>
                <div className="space-y-1">
                  {(mt.cpuidGroups.length ? mt.cpuidGroups : []).map((group) => (
                    <div
                      key={group.cpuidHash || 'unknown'}
                      className="font-mono text-foreground/80"
                    >
                      {group.unknown
                        ? t('machineTypes.unknownHardware')
                        : group.cpuModel || shortHash(group.cpuidHash)}{' '}
                      <span className="text-muted-foreground">×{group.nodeCount}</span>
                    </div>
                  ))}
                  {mt.cpuidGroups.length === 0 && <div className="text-muted-foreground">—</div>}
                </div>
              </div>
              <div>
                <div className="mb-1 text-muted-foreground">{t('machineTypes.kernels')}</div>
                <div className="space-y-1">
                  {mt.kernelReleases.map((kernel) => (
                    <div key={kernel.release || 'unknown'} className="font-mono text-foreground/80">
                      {kernel.release || t('machineTypes.unknownHardware')}{' '}
                      <span className="text-muted-foreground">×{kernel.nodeCount}</span>
                    </div>
                  ))}
                  {mt.kernelReleases.length === 0 && <div className="text-muted-foreground">—</div>}
                </div>
              </div>
            </div>

            <div className="flex flex-wrap gap-2">
              <Button size="sm" variant="outline" asChild>
                <Link to={`/nodes?machineType=${encodeURIComponent(mt.name)}`}>
                  {t('machineTypes.viewNodes')}
                </Link>
              </Button>
              <Button size="sm" variant="outline" onClick={() => setManaging(mt)}>
                {t('machineTypes.manageNodes')}
              </Button>
              <Button size="sm" variant="outline" onClick={() => setEditing(mt)}>
                {t('machineTypes.edit')}
              </Button>
              <Button size="sm" variant="destructive" onClick={() => setDeleting(mt)}>
                {t('machineTypes.delete')}
              </Button>
            </div>
          </Card>
        ))}
      </div>

      <MachineTypeFormDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        onSaved={() => {
          invalidate();
          showToast(t('machineTypes.created'));
        }}
      />
      <MachineTypeFormDialog
        open={editing !== null}
        existing={editing}
        onOpenChange={(open) => !open && setEditing(null)}
        onSaved={() => {
          invalidate();
          showToast(t('machineTypes.updated'));
        }}
      />
      <MembersDialog
        machineType={managing}
        nodes={nodes}
        onOpenChange={(open) => !open && setManaging(null)}
        onSaved={invalidate}
      />
      <DeleteDialog
        machineType={deleting}
        onOpenChange={(open) => !open && setDeleting(null)}
        onSaved={() => {
          invalidate();
          showToast(t('machineTypes.deleted'));
        }}
      />
    </div>
  );
}

function MachineTypeFormDialog({
  open,
  existing,
  onOpenChange,
  onSaved,
}: {
  open: boolean;
  existing?: MachineTypeView | null;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation('nodes');
  const [name, setName] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [description, setDescription] = useState('');

  useEffect(() => {
    if (!open) return;
    setName(existing?.name ?? '');
    setDisplayName(existing?.displayName ?? '');
    setDescription(existing?.description ?? '');
  }, [open, existing]);

  const nameError = !existing && validateMachineTypeValue(name.trim()) === 'shape';
  const save = useMutation({
    mutationFn: () => {
      if (existing) {
        return machineTypeApi.update(existing.name, { displayName, description });
      }
      return machineTypeApi.create({
        name: name.trim(),
        displayName,
        description,
      });
    },
    onSuccess: () => {
      onOpenChange(false);
      onSaved();
    },
    onError: () => showToast(t('machineTypes.failed'), 'warn'),
  });

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 max-h-[calc(100vh-3rem)] w-[min(40rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 overflow-y-auto rounded-lg border border-border bg-background p-5 shadow-lg">
          <Dialog.Title className="mb-3 text-base font-semibold">
            {existing ? t('machineTypes.editTitle') : t('machineTypes.createTitle')}
          </Dialog.Title>
          <div className="space-y-3">
            {!existing && (
              <label className="block space-y-1">
                <span className="text-xs text-muted-foreground">{t('machineTypes.name')}</span>
                <Input
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  className="font-mono"
                />
                <span
                  className={cn('text-xs', nameError ? 'text-cube-err' : 'text-muted-foreground')}
                >
                  {nameError ? t('machineTypes.nameInvalid') : t('machineTypes.nameHint')}
                </span>
              </label>
            )}
            <label className="block space-y-1">
              <span className="text-xs text-muted-foreground">{t('machineTypes.displayName')}</span>
              <Input value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
            </label>
            <label className="block space-y-1">
              <span className="text-xs text-muted-foreground">{t('machineTypes.description')}</span>
              <Input value={description} onChange={(e) => setDescription(e.target.value)} />
            </label>
          </div>
          <div className="mt-4 flex justify-end gap-2">
            <Button size="sm" variant="outline" onClick={() => onOpenChange(false)}>
              {t('machineTypes.cancel')}
            </Button>
            <Button
              size="sm"
              disabled={
                save.isPending ||
                (!existing && (validateMachineTypeValue(name) !== null || nameError))
              }
              onClick={() => save.mutate()}
            >
              {save.isPending ? t('machineTypes.saving') : t('machineTypes.save')}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function MembersDialog({
  machineType,
  nodes,
  onOpenChange,
  onSaved,
}: {
  machineType: MachineTypeView | null;
  nodes: Array<{
    nodeID: string;
    address?: string;
    healthy: boolean;
    machineType: string;
    hostFacts?: { cpuModel?: string; cpuidHash?: string; hostKernelRelease?: string } | null;
  }>;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation('nodes');
  const [selected, setSelected] = useState<string[]>([]);
  const [search, setSearch] = useState('');
  const [bucket, setBucket] = useState<'all' | 'unassigned' | 'current' | 'other'>('all');

  const current = useMemo(
    () => new Set(machineType?.nodes.map((node) => node.nodeID) ?? []),
    [machineType],
  );

  useEffect(() => {
    if (!machineType) return;
    setSelected(machineType.nodes.map((node) => node.nodeID));
    setSearch('');
    setBucket('all');
  }, [machineType]);

  const open = machineType !== null;
  const filtered = nodes.filter((node) => {
    const q = search.trim().toLowerCase();
    if (q && !node.nodeID.toLowerCase().includes(q) && !(node.address ?? '').includes(q)) {
      return false;
    }
    if (bucket === 'unassigned') return !node.machineType;
    if (bucket === 'current') return current.has(node.nodeID);
    if (bucket === 'other') return Boolean(node.machineType) && !current.has(node.nodeID);
    return true;
  });

  const selectedNodes = nodes.filter((node) => selected.includes(node.nodeID));
  const hashes = new Set(selectedNodes.map((node) => node.hostFacts?.cpuidHash).filter(Boolean));
  const added = selected.filter(
    (id) => !current.has(id) && !nodes.find((n) => n.nodeID === id)?.machineType,
  );
  const moved = selected.filter((id) => {
    const node = nodes.find((n) => n.nodeID === id);
    return node?.machineType && !current.has(id);
  });
  const removed = [...current].filter((id) => !selected.includes(id));

  const save = useMutation({
    mutationFn: () => machineTypeApi.replaceNodes(machineType!.name, selected),
    onSuccess: (res) => {
      onSaved();
      const failed = res.results.filter((item) => !item.ok);
      if (failed.length > 0) {
        showToast(
          t('machineTypes.membersPartial', {
            count: failed.length,
            error: failed[0]?.error ?? '',
          }),
          'warn',
        );
        return;
      }
      onOpenChange(false);
      showToast(t('machineTypes.savedMembers'));
    },
    onError: () => showToast(t('machineTypes.failed'), 'warn'),
  });

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(next) => {
        if (next && machineType) setSelected(machineType.nodes.map((node) => node.nodeID));
        if (!next) {
          setSearch('');
          setBucket('all');
        }
        onOpenChange(next);
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 max-h-[calc(100vh-3rem)] w-[min(52rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 overflow-y-auto rounded-lg border border-border bg-background p-5 shadow-lg">
          <Dialog.Title className="mb-3 text-base font-semibold">
            {t('machineTypes.membersTitle', {
              name: machineType?.displayName || machineType?.name,
            })}
          </Dialog.Title>
          <div className="mb-3 flex flex-wrap gap-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder={t('machineTypes.membersSearch')}
              className="h-8 max-w-xs"
            />
            {(
              [
                ['all', t('machineTypes.membersAll')],
                ['unassigned', t('machineTypes.membersUnassigned')],
                ['current', t('machineTypes.membersCurrent')],
                ['other', t('machineTypes.membersOther')],
              ] as const
            ).map(([id, label]) => (
              <Button
                key={id}
                size="sm"
                variant={bucket === id ? 'default' : 'outline'}
                onClick={() => setBucket(id)}
              >
                {label}
              </Button>
            ))}
          </div>
          <div className="max-h-80 space-y-1 overflow-y-auto rounded-md border border-border/60 p-2">
            {filtered.length === 0 && (
              <p className="py-6 text-center text-sm text-muted-foreground">
                {t('machineTypes.membersEmpty')}
              </p>
            )}
            {filtered.map((node) => (
              <label
                key={node.nodeID}
                className="flex items-center justify-between gap-3 rounded-md px-2 py-1.5 text-sm hover:bg-muted/40"
              >
                <span className="flex min-w-0 items-center gap-2">
                  <input
                    type="checkbox"
                    checked={selected.includes(node.nodeID)}
                    onChange={(e) =>
                      setSelected((cur) =>
                        e.target.checked
                          ? [...cur, node.nodeID]
                          : cur.filter((id) => id !== node.nodeID),
                      )
                    }
                  />
                  <Server size={13} className="text-muted-foreground" />
                  <span className="truncate font-mono">{node.nodeID}</span>
                  <span className="text-xs text-muted-foreground">{node.address}</span>
                </span>
                <span className="shrink-0 text-right font-mono text-[11px] text-muted-foreground">
                  {node.machineType || '—'}
                  {node.hostFacts?.cpuModel ? ` · ${node.hostFacts.cpuModel}` : ''}
                  {node.hostFacts?.hostKernelRelease
                    ? ` · ${node.hostFacts.hostKernelRelease}`
                    : ''}
                </span>
              </label>
            ))}
          </div>
          <div className="mt-3 space-y-1 text-xs text-muted-foreground">
            {added.length > 0 && <div>{t('machineTypes.memberAdd', { count: added.length })}</div>}
            {moved.length > 0 && (
              <div>{t('machineTypes.memberMoveIn', { count: moved.length })}</div>
            )}
            {removed.length > 0 && (
              <div>{t('machineTypes.memberRemove', { count: removed.length })}</div>
            )}
            {hashes.size > 1 && (
              <div className="text-amber-600">{t('machineTypes.memberCpuWarning')}</div>
            )}
          </div>
          <div className="mt-4 flex justify-end gap-2">
            <Button size="sm" variant="outline" onClick={() => onOpenChange(false)}>
              {t('machineTypes.cancel')}
            </Button>
            <Button size="sm" disabled={save.isPending} onClick={() => save.mutate()}>
              {save.isPending ? t('machineTypes.saving') : t('machineTypes.save')}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function DeleteDialog({
  machineType,
  onOpenChange,
  onSaved,
}: {
  machineType: MachineTypeView | null;
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  const { t } = useTranslation('nodes');
  const [unbind, setUnbind] = useState(false);
  const inUse = (machineType?.nodeCount ?? 0) > 0;
  const remove = useMutation({
    mutationFn: () => machineTypeApi.remove(machineType!.name, unbind),
    onSuccess: () => {
      onOpenChange(false);
      onSaved();
    },
    onError: () => showToast(t('machineTypes.failed'), 'warn'),
  });

  return (
    <Dialog.Root
      open={machineType !== null}
      onOpenChange={(next) => {
        if (next) setUnbind(false);
        onOpenChange(next);
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/40" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-[min(28rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-lg border border-border bg-background p-5 shadow-lg">
          <Dialog.Title className="mb-2 text-base font-semibold">
            {t('machineTypes.deleteTitle')}
          </Dialog.Title>
          <Dialog.Description className="mb-4 text-sm text-muted-foreground">
            {inUse
              ? t('machineTypes.deleteInUse', {
                  name: machineType?.name,
                  count: machineType?.nodeCount,
                })
              : t('machineTypes.deleteEmpty', { name: machineType?.name })}
          </Dialog.Description>
          {inUse && (
            <label className="mb-4 flex items-start gap-2 text-sm">
              <input
                type="checkbox"
                checked={unbind}
                onChange={(e) => setUnbind(e.target.checked)}
              />
              <span>{t('machineTypes.unbindAndDelete')}</span>
            </label>
          )}
          <div className="flex justify-end gap-2">
            <Button size="sm" variant="outline" onClick={() => onOpenChange(false)}>
              {t('machineTypes.cancel')}
            </Button>
            <Button
              size="sm"
              variant="destructive"
              disabled={remove.isPending || (inUse && !unbind)}
              onClick={() => remove.mutate()}
            >
              {remove.isPending ? t('machineTypes.saving') : t('machineTypes.delete')}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
