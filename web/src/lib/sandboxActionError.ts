// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { ApiError } from '@/lib/api';

type TranslateFn = (...args: any[]) => string;

/** 将 pause/resume/kill 等 lifecycle 失败转成可展示的文案. */
export function formatSandboxActionError(err: unknown, t: TranslateFn): string {
  const raw = err instanceof Error ? err.message : String(err);
  const status = err instanceof ApiError ? err.status : 0;

  if (status === 409 && /resume rejected by paused_resource_release_ratio/i.test(raw)) {
    const match = raw.match(/need (\d+MB) \+ used (\d+MB) > mem quota (\d+MB)/);
    if (match) {
      return t('errors.resumeCapacityDetail', {
        need: match[1],
        used: match[2],
        quota: match[3],
      });
    }
    return t('errors.resumeCapacity');
  }

  if (status === 409) {
    return t('errors.conflict', { message: raw });
  }

  return t('errors.actionFailed', { message: raw || t('errors.unknown') });
}
