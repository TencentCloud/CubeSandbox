// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { createPortal } from 'react-dom';
import { useTranslation } from 'react-i18next';
import { X } from 'lucide-react';
import { ApiError } from '@/lib/api';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card';

export function formatIsolationError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    if (typeof err.body === 'object' && err.body && 'error' in err.body) {
      const msg = (err.body as { error?: string }).error;
      if (msg) return msg;
    }
    if (err.message) return err.message;
  }
  if (err instanceof Error && err.message) return err.message;
  return fallback;
}

type IsolateConfirmDialogProps = {
  open: boolean;
  onClose: () => void;
  onConfirm: () => void;
  pending?: boolean;
  error?: string | null;
};

export function IsolateConfirmDialog({
  open,
  onClose,
  onConfirm,
  pending = false,
  error,
}: IsolateConfirmDialogProps) {
  const { t } = useTranslation('nodeDetail');

  if (!open) return null;

  return createPortal(
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm">
      <Card className="w-full max-w-sm shadow-xl">
        <CardHeader className="flex flex-row items-center justify-between pb-3">
          <CardTitle className="text-base">{t('isolation.confirmTitle')}</CardTitle>
          <button
            type="button"
            onClick={onClose}
            className="text-muted-foreground hover:text-foreground"
            disabled={pending}
          >
            <X className="h-4 w-4" />
          </button>
        </CardHeader>
        <CardContent className="space-y-4">
          <p className="text-sm text-muted-foreground">{t('isolation.confirmDesc')}</p>
          {error && <p className="text-xs text-destructive">{error}</p>}
          <div className="flex justify-end gap-2">
            <Button variant="outline" size="sm" onClick={onClose} disabled={pending}>
              {t('isolation.cancel')}
            </Button>
            <Button variant="destructive" size="sm" disabled={pending} onClick={onConfirm}>
              {pending ? t('isolation.isolating') : t('isolation.confirm')}
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>,
    document.body,
  );
}
