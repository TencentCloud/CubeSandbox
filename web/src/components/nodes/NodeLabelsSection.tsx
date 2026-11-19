// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import * as Dialog from '@radix-ui/react-dialog';
import { clusterApi, machineTypeApi } from '@/api/client';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Section } from '@/components/ui/section';
import { showToast } from '@/components/ui/ToastProvider';
import {
  ISOLATION_KEY,
  MACHINE_TYPE_KEY,
  diffLabels,
  isMachineTypeKey,
  isReadOnlyLabelKey,
  isReplaceProtectedKey,
  readOnlyLabelsOf,
  userLabelsOf,
} from '@/lib/nodeLabels';

/**
 * Maps a label-write failure to a translation key: 403 means the key is
 * control/data-plane owned, 400 means the request was rejected.
 */
type LabelErrorKey = 'labels.errorReadOnly' | 'labels.errorInvalid' | 'labels.errorGeneric';

function labelErrorKey(status: number | undefined): LabelErrorKey {
  if (status === 403) return 'labels.errorReadOnly';
  if (status === 400) return 'labels.errorInvalid';
  return 'labels.errorGeneric';
}

function statusOf(err: unknown): number | undefined {
  const status = (err as { status?: unknown } | null)?.status;
  return typeof status === 'number' ? status : undefined;
}

/** A draft row keeps its own id so removing a row does not remount the rest. */
interface DraftRow {
  id: string;
  key: string;
  value: string;
}

let draftRowSeq = 0;
function newDraftRow(key = '', value = ''): DraftRow {
  draftRowSeq += 1;
  return { id: `label-row-${draftRowSeq}`, key, value };
}

export function NodeLabelsSection({
  nodeID,
  labels,
}: {
  nodeID: string;
  labels: Record<string, string>;
}) {
  const { t } = useTranslation('nodeDetail');
  const queryClient = useQueryClient();
  const [editingKey, setEditingKey] = useState<string | null>(null);
  const [editingValue, setEditingValue] = useState('');
  const [addingRow, setAddingRow] = useState(false);
  const [newKey, setNewKey] = useState('');
  const [newValue, setNewValue] = useState('');
  const [deleteKey, setDeleteKey] = useState<string | null>(null);
  const [editAllOpen, setEditAllOpen] = useState(false);
  const [draft, setDraft] = useState<DraftRow[]>([]);

  const machineType = labels[MACHINE_TYPE_KEY] ?? '';
  const typesQuery = useQuery({
    queryKey: ['machine-types'],
    queryFn: machineTypeApi.list,
  });
  const catalog = typesQuery.data ?? [];
  const bound = catalog.find((item) => item.name === machineType);
  const orphan = Boolean(machineType) && typesQuery.isSuccess && !bound;
  const [pickedType, setPickedType] = useState('');
  const userLabels = userLabelsOf(labels);
  const userKeys = Object.keys(userLabels).sort((a, b) => a.localeCompare(b));
  const readOnlyLabels = readOnlyLabelsOf(labels);
  const readOnlyKeys = Object.keys(readOnlyLabels).sort((a, b) => a.localeCompare(b));
  // Isolation is control-plane state, not a Cubelet label: keep it out of the
  // read-only group.
  const isolationValue = Object.hasOwn(labels, ISOLATION_KEY) ? labels[ISOLATION_KEY] : null;

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ['node', nodeID] });
    queryClient.invalidateQueries({ queryKey: ['nodes'] });
  };

  const fail = (err: unknown) => {
    showToast(t(labelErrorKey(statusOf(err))), 'warn');
  };

  const saveOne = useMutation({
    mutationFn: (pair: { key: string; value: string }) =>
      clusterApi.setNodeLabel(nodeID, { [pair.key]: pair.value }),
    onSuccess: (_, pair) => {
      invalidate();
      setEditingKey(null);
      setAddingRow(false);
      setNewKey('');
      setNewValue('');
      showToast(t('labels.saved'));
    },
    onError: fail,
  });

  const removeOne = useMutation({
    mutationFn: (key: string) => clusterApi.deleteNodeLabel(nodeID, key),
    onSuccess: () => {
      invalidate();
      setDeleteKey(null);
      showToast(t('labels.deleted'));
    },
    onError: fail,
  });

  const replaceAll = useMutation({
    mutationFn: (next: Record<string, string>) => clusterApi.replaceNodeLabels(nodeID, next),
    onSuccess: () => {
      invalidate();
      setEditAllOpen(false);
      showToast(t('labels.saved'));
    },
    onError: fail,
  });

  const openEditAll = () => {
    setDraft(userKeys.map((key) => newDraftRow(key, userLabels[key])));
    setEditAllOpen(true);
  };

  // Drop protected keys so the replace payload only carries user labels.
  const draftLabels = () => {
    const out: Record<string, string> = {};
    for (const row of draft) {
      const key = row.key.trim();
      if (!key || isReplaceProtectedKey(key)) continue;
      out[key] = row.value;
    }
    return out;
  };

  const nextLabels = draftLabels();
  const diff = diffLabels(userLabels, nextLabels);
  const draftKeys = draft.map((row) => row.key.trim()).filter(Boolean);
  const duplicateKeys = draftKeys.filter((key, i) => draftKeys.indexOf(key) !== i);
  const draftHasProtectedKey = draftKeys.some((key) => isReplaceProtectedKey(key));
  const canReplace = duplicateKeys.length === 0 && !draftHasProtectedKey;
  const bindType = useMutation({
    mutationFn: (name: string) => clusterApi.setNodeMachineType(nodeID, name),
    onSuccess: () => {
      invalidate();
      setPickedType('');
      showToast(t('labels.machineTypeSaved'));
    },
    onError: fail,
  });
  const clearType = useMutation({
    mutationFn: () => clusterApi.clearNodeMachineType(nodeID),
    onSuccess: () => {
      invalidate();
      setDeleteKey(null);
      showToast(t('labels.deleted'));
    },
    onError: fail,
  });

  return (
    <Section
      title={t('labels.title')}
      action={
        <Button size="sm" variant="outline" onClick={openEditAll}>
          {t('labels.editAll')}
        </Button>
      }
    >
      <div className="rounded-xl border border-border/60 bg-card/40 px-4 py-1">
        {/* Machine type first and always visible: "not set" must be explicit. */}
        <div className="flex flex-wrap items-center justify-between gap-4 py-2.5 border-b border-white/5">
          <div className="min-w-0">
            <div className="text-sm text-muted-foreground">{t('labels.machineType')}</div>
            <div className="font-mono text-xs text-muted-foreground/60 truncate">
              {MACHINE_TYPE_KEY}
            </div>
          </div>
          <div className="flex flex-col items-end gap-2">
            <div className="flex flex-wrap items-center justify-end gap-2">
              {machineType ? (
                <Badge
                  tone={orphan ? 'warn' : 'mute'}
                  className="max-w-[16rem] truncate font-mono"
                  title={machineType}
                >
                  {bound?.displayName ? `${bound.displayName} (${machineType})` : machineType}
                </Badge>
              ) : (
                <span className="text-sm text-muted-foreground">
                  {t('labels.machineTypeUnset')}
                </span>
              )}
              <select
                value={pickedType}
                onChange={(e) => setPickedType(e.target.value)}
                aria-label={t('labels.machineTypeChoose')}
                className="h-8 rounded-md border border-input bg-background/50 px-2 font-mono text-xs"
              >
                <option value="">{t('labels.machineTypeChoose')}</option>
                {catalog.map((item) => (
                  <option key={item.name} value={item.name}>
                    {item.displayName ? `${item.displayName} (${item.name})` : item.name}
                  </option>
                ))}
              </select>
              <Button
                size="sm"
                disabled={!pickedType || pickedType === machineType || bindType.isPending}
                onClick={() => bindType.mutate(pickedType)}
              >
                {bindType.isPending ? t('labels.saving') : t('labels.machineTypeBind')}
              </Button>
              {machineType && (
                <Button
                  size="sm"
                  variant="destructive"
                  onClick={() => setDeleteKey(MACHINE_TYPE_KEY)}
                >
                  {t('labels.machineTypeUnbind')}
                </Button>
              )}
              <Button size="sm" variant="outline" asChild>
                <Link to="/nodes/machine-types">{t('labels.machineTypeCreate')}</Link>
              </Button>
            </div>
            <p className="max-w-xl text-right text-xs text-muted-foreground">
              {orphan
                ? t('labels.machineTypeOrphan', { name: machineType })
                : machineType
                  ? t('labels.machineTypeBound', { name: bound?.displayName || machineType })
                  : t('labels.machineTypeUnbound')}
            </p>
          </div>
        </div>

        <div className="py-2 border-b border-white/5">
          <div className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium py-1.5">
            {t('labels.other')}
          </div>
          {userKeys.length === 0 ? (
            <p className="py-2 text-sm text-muted-foreground">{t('labels.empty')}</p>
          ) : (
            userKeys.map((key) => (
              <div key={key} className="flex flex-wrap items-center justify-between gap-4 py-2">
                <div className="min-w-0 flex-1">
                  <div className="font-mono text-sm text-foreground/90 break-all">{key}</div>
                  <div className="font-mono text-xs text-muted-foreground/80 break-all">
                    {userLabels[key] || '—'}
                  </div>
                </div>
                {editingKey === key ? (
                  <div className="flex items-center gap-1.5">
                    <Input
                      value={editingValue}
                      onChange={(e) => setEditingValue(e.target.value)}
                      className="h-8 w-44 font-mono"
                      aria-label={key}
                    />
                    <Button
                      size="sm"
                      onClick={() => saveOne.mutate({ key, value: editingValue })}
                      disabled={saveOne.isPending}
                    >
                      {saveOne.isPending ? t('labels.saving') : t('labels.save')}
                    </Button>
                    <Button size="sm" variant="outline" onClick={() => setEditingKey(null)}>
                      {t('labels.cancel')}
                    </Button>
                  </div>
                ) : (
                  <div className="flex items-center gap-1.5">
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => {
                        setEditingKey(key);
                        setEditingValue(userLabels[key]);
                      }}
                    >
                      {t('labels.edit')}
                    </Button>
                    <Button size="sm" variant="destructive" onClick={() => setDeleteKey(key)}>
                      {t('labels.delete')}
                    </Button>
                  </div>
                )}
              </div>
            ))
          )}
        </div>

        {isolationValue !== null && (
          <div className="py-2 border-b border-white/5">
            <div className="flex items-center justify-between gap-4 py-2">
              <div className="min-w-0">
                <div className="font-mono text-sm text-foreground/90 break-all">
                  {ISOLATION_KEY}
                </div>
                <div className="font-mono text-xs text-muted-foreground/80 break-all">
                  {isolationValue || '—'}
                </div>
              </div>
              <Badge tone="mute">{t('labels.isolationTag')}</Badge>
            </div>
            <p className="text-xs text-muted-foreground pb-1">{t('labels.isolationHint')}</p>
          </div>
        )}

        {readOnlyKeys.length > 0 && (
          <div className="py-2 border-b border-white/5">
            <div className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium py-1.5">
              {t('labels.readOnly')}
            </div>
            {readOnlyKeys.map((key) => (
              <div key={key} className="flex items-center justify-between gap-4 py-2">
                <div className="min-w-0">
                  <div className="font-mono text-sm text-foreground/90 break-all">{key}</div>
                  <div className="font-mono text-xs text-muted-foreground/80 break-all">
                    {readOnlyLabels[key] || '—'}
                  </div>
                </div>
                <Badge tone="mute">{t('labels.readOnlyTag')}</Badge>
              </div>
            ))}
            <p className="text-xs text-muted-foreground pb-1">{t('labels.readOnlyHint')}</p>
          </div>
        )}

        <div className="py-3">
          {addingRow ? (
            <div className="flex flex-wrap items-center gap-1.5">
              <Input
                value={newKey}
                onChange={(e) => setNewKey(e.target.value)}
                placeholder={t('labels.keyPlaceholder')}
                className="h-8 w-56 font-mono"
                aria-label={t('labels.keyPlaceholder')}
              />
              <Input
                value={newValue}
                onChange={(e) => setNewValue(e.target.value)}
                placeholder={t('labels.valuePlaceholder')}
                className="h-8 w-44 font-mono"
                aria-label={t('labels.valuePlaceholder')}
              />
              <Button
                size="sm"
                onClick={() => saveOne.mutate({ key: newKey.trim(), value: newValue })}
                disabled={!newKey.trim() || isMachineTypeKey(newKey.trim()) || saveOne.isPending}
              >
                {saveOne.isPending ? t('labels.saving') : t('labels.save')}
              </Button>
              <Button size="sm" variant="outline" onClick={() => setAddingRow(false)}>
                {t('labels.cancel')}
              </Button>
              <p className="w-full text-xs text-muted-foreground">{t('labels.keyHint')}</p>
            </div>
          ) : (
            <Button size="sm" variant="outline" onClick={() => setAddingRow(true)}>
              {t('labels.add')}
            </Button>
          )}
        </div>
      </div>

      <Dialog.Root open={deleteKey !== null} onOpenChange={(open) => !open && setDeleteKey(null)}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/40 z-40" />
          <Dialog.Content className="fixed left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 z-50 w-[min(28rem,calc(100vw-2rem))] rounded-lg border border-border bg-background p-5 shadow-lg">
            <Dialog.Title className="text-base font-semibold mb-2">
              {t('labels.deleteTitle')}
            </Dialog.Title>
            <Dialog.Description className="text-sm text-muted-foreground mb-4 break-all">
              {t('labels.deleteDesc', { key: deleteKey ?? '' })}
            </Dialog.Description>
            {deleteKey !== null && isMachineTypeKey(deleteKey) && (
              <p className="text-sm text-cube-warn mb-4">{t('labels.deleteMachineTypeWarning')}</p>
            )}
            <div className="flex justify-end gap-2">
              <Button size="sm" variant="outline" onClick={() => setDeleteKey(null)}>
                {t('labels.cancel')}
              </Button>
              <Button
                size="sm"
                variant="destructive"
                disabled={removeOne.isPending || clearType.isPending}
                onClick={() => {
                  if (deleteKey === null) return;
                  if (isMachineTypeKey(deleteKey)) clearType.mutate();
                  else removeOne.mutate(deleteKey);
                }}
              >
                {removeOne.isPending || clearType.isPending
                  ? t('labels.deleting')
                  : t('labels.delete')}
              </Button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>

      <Dialog.Root open={editAllOpen} onOpenChange={setEditAllOpen}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 bg-black/40 z-40" />
          <Dialog.Content className="fixed left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 z-50 w-[min(920px,calc(100vw-2rem))] max-h-[calc(100vh-3rem)] overflow-y-auto rounded-lg border border-border bg-background p-5 shadow-lg">
            <Dialog.Title className="text-base font-semibold mb-1">
              {t('labels.replaceTitle')}
            </Dialog.Title>
            <Dialog.Description className="text-sm text-muted-foreground mb-3">
              {t('labels.replaceDesc')}
            </Dialog.Description>

            <div className="space-y-2">
              {draft.map((row, index) => {
                const key = row.key.trim();
                const duplicated = key && duplicateKeys.includes(key);
                return (
                  <div key={row.id} className="flex flex-wrap items-center gap-1.5">
                    <Input
                      value={row.key}
                      onChange={(e) => {
                        const next = [...draft];
                        next[index] = { ...row, key: e.target.value };
                        setDraft(next);
                      }}
                      className="h-8 flex-1 min-w-[10rem] font-mono"
                      placeholder="key"
                      aria-label={t('labels.keyPlaceholder')}
                      aria-invalid={Boolean(duplicated)}
                    />
                    <Input
                      value={row.value}
                      onChange={(e) => {
                        const next = [...draft];
                        next[index] = { ...row, value: e.target.value };
                        setDraft(next);
                      }}
                      className="h-8 flex-1 min-w-[8rem] font-mono"
                      placeholder="value"
                      aria-label={t('labels.valuePlaceholder')}
                    />
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => setDraft(draft.filter((_, i) => i !== index))}
                      aria-label={t('labels.delete')}
                    >
                      ×
                    </Button>
                    {duplicated && (
                      <p className="w-full text-xs text-cube-err">
                        {t('labels.duplicateKey', { key })}
                      </p>
                    )}
                  </div>
                );
              })}
              <Button
                size="sm"
                variant="outline"
                onClick={() => setDraft([...draft, newDraftRow()])}
              >
                {t('labels.addRow')}
              </Button>
            </div>

            {draftHasProtectedKey && (
              <p className="text-xs text-cube-err mt-2">{t('labels.replaceProtected')}</p>
            )}

            {/* replace deletes anything not listed; show that before committing. */}
            <div className="rounded-md border border-border/60 bg-muted/20 px-3 py-2 mt-3 text-sm space-y-1">
              {diff.removed.length > 0 ? (
                <div>
                  <div className="text-muted-foreground">
                    {t('labels.replaceRemoved', { count: diff.removed.length })}
                  </div>
                  <div className="font-mono text-xs break-all text-cube-warn">
                    {diff.removed.join(', ')}
                  </div>
                </div>
              ) : (
                <div className="text-muted-foreground">{t('labels.replaceNoneRemoved')}</div>
              )}
              {diff.added.length > 0 && (
                <div className="font-mono text-xs text-muted-foreground break-all">
                  {t('labels.replaceAdded')}: {diff.added.join(', ')}
                </div>
              )}
              {diff.changed.length > 0 && (
                <div className="font-mono text-xs text-muted-foreground break-all">
                  {t('labels.replaceChanged')}: {diff.changed.join(', ')}
                </div>
              )}
              <div className="text-xs text-muted-foreground pt-1">
                {t('labels.replaceReservedHint')}
              </div>
            </div>

            <div className="flex justify-end gap-2 mt-4">
              <Button size="sm" variant="outline" onClick={() => setEditAllOpen(false)}>
                {t('labels.cancel')}
              </Button>
              <Button
                size="sm"
                disabled={!canReplace || replaceAll.isPending}
                onClick={() => replaceAll.mutate(nextLabels)}
              >
                {replaceAll.isPending ? t('labels.saving') : t('labels.confirmReplace')}
              </Button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>
    </Section>
  );
}
