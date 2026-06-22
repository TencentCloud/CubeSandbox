// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { ApiError } from '@/lib/api';

type TranslateFn = (...args: any[]) => string;

/** 将 pause/resume/kill 等 lifecycle 失败转成可展示的文案. */
export function formatSandboxActionError(err: unknown, t: TranslateFn): string {
  const raw = err instanceof Error ? err.message : String(err);
  const status = err instanceof ApiError ? err.status : 0;

  if (status === 409 && /resume rejected by paused_resource_release_ratio/i.test(raw)) {
    // Cubelet rejects on whichever dimension overflows first, so handle both
    // the memory (MB) and the CPU (milli, "m") reason formats.
    //
    // NOTE: these regexes mirror the exact reason strings produced by
    // resumeQuotaRejection in Cubelet/services/cubebox/update.go. They are a
    // cross-language contract; if the Go format strings change, update these
    // regexes too (the Go side guards its format via
    // TestResumeQuotaRejectionMessageFormat). On a mismatch we fall back to the
    // generic errors.resumeCapacity message below.
    const mem = raw.match(/need (\d+MB) \+ used (\d+MB) > mem quota (\d+MB)/);
    if (mem) {
      return t('errors.resumeCapacityDetail', {
        need: mem[1],
        used: mem[2],
        quota: mem[3],
      });
    }
    const cpu = raw.match(/need (\d+m) \+ used (\d+m) > cpu quota (\d+m)/);
    if (cpu) {
      return t('errors.resumeCapacityDetailCpu', {
        need: cpu[1],
        used: cpu[2],
        quota: cpu[3],
      });
    }
    return t('errors.resumeCapacity');
  }

  if (status === 409) {
    return t('errors.conflict', { message: raw });
  }

  return t('errors.actionFailed', { message: raw || t('errors.unknown') });
}
