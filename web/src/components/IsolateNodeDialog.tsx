// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import * as Dialog from '@radix-ui/react-dialog';
import { useTranslation } from 'react-i18next';
import { clusterApi } from '@/api/client';
import { Button } from '@/components/ui/button';

interface IsolateNodeDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  nodeID: string;
  isCurrentlyIsolated: boolean;
}

/**
 * Shared confirm dialog for toggling a node's isolation state.
 *
 * Used by both the nodes list (Nodes.tsx) and the node detail page
 * (NodeDetail.tsx) so the wording, validation and mutation flow stay
 * consistent across surfaces.
 */
export function IsolateNodeDialog({
  open,
  onOpenChange,
  nodeID,
  isCurrentlyIsolated,
}: IsolateNodeDialogProps) {
  const { t } = useTranslation('nodeDetail');
  const queryClient = useQueryClient();
  const [reason, setReason] = useState('');

  // Reset the reason whenever the dialog re-opens so a stale value from
  // a previous operation never leaks into a fresh confirmation.
  useEffect(() => {
    if (open) setReason('');
  }, [open]);

  const isolationMutation = useMutation({
    mutationFn: (vars: { isolated: boolean; reason?: string }) =>
      clusterApi.setNodeIsolation(nodeID, vars.isolated, vars.reason),
    onSuccess: () => {
      onOpenChange(false);
      setReason('');
      queryClient.invalidateQueries({ queryKey: ['node', nodeID] });
      queryClient.invalidateQueries({ queryKey: ['nodes'] });
    },
  });

  // While the mutation is in flight, keep the dialog open and prevent
  // outside dismissal from racing the in-flight request.
  const handleOpenChange = (next: boolean) => {
    if (!next && isolationMutation.isPending) return;
    onOpenChange(next);
  };

  // isIsolate=true: user is isolating a currently-scheduled node
  // isIsolate=false: user is resuming a previously-isolated node
  const isIsolate = !isCurrentlyIsolated;

  return (
    <Dialog.Root open={open} onOpenChange={handleOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-[60] bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-[70] w-[min(460px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-2xl border border-border/60 bg-card p-6 shadow-2xl">
          <Dialog.Title className="text-base font-semibold">
            {isIsolate ? t('isolation.dialog.isolateTitle') : t('isolation.dialog.unisolateTitle')}
          </Dialog.Title>
          <Dialog.Description className="mt-2 text-sm text-muted-foreground">
            {isIsolate ? t('isolation.dialog.isolateDesc') : t('isolation.dialog.unisolateDesc')}
          </Dialog.Description>
          {isIsolate && (
            <div className="mt-4 space-y-1.5">
              <label className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
                {t('isolation.reasonLabel')}
              </label>
              <input
                type="text"
                value={reason}
                maxLength={512}
                onChange={(e) => setReason(e.target.value)}
                placeholder={t('isolation.reasonPlaceholder')}
                className="w-full rounded-lg border border-border/60 bg-background px-3 py-2 text-sm outline-none focus:border-cube-accent"
              />
            </div>
          )}
          {isolationMutation.isError && (
            <p className="mt-3 text-sm text-cube-err">{t('isolation.error')}</p>
          )}
          <div className="mt-6 flex justify-end gap-2">
            <Button
              type="button"
              variant="outline"
              disabled={isolationMutation.isPending}
              onClick={() => handleOpenChange(false)}
            >
              {t('isolation.actions.cancel')}
            </Button>
            <Button
              type="button"
              disabled={isolationMutation.isPending}
              onClick={() =>
                isolationMutation.mutate(
                  isIsolate
                    ? { isolated: true, reason: reason.trim() || undefined }
                    : { isolated: false },
                )
              }
            >
              {isolationMutation.isPending
                ? t('isolation.actions.processing')
                : isIsolate
                ? t('isolation.actions.confirmIsolate')
                : t('isolation.actions.confirmUnisolate')}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
