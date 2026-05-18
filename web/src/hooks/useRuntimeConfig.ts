// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useQuery } from '@tanstack/react-query';
import { clusterApi } from '@/api/client';

/** Shared hook for the cluster runtime config — keeps cache policy consistent. */
export function useRuntimeConfig() {
  return useQuery({
    queryKey: ['runtime-config'],
    queryFn: () => clusterApi.config(),
    staleTime: 60_000,
  });
}
