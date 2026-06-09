// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useQuery } from '@tanstack/react-query';
import { versionApi } from '@/api/client';

/**
 * Returns the control-plane version reported by the backend, falling back to
 * the build-time injected __APP_VERSION__ when the backend is unavailable or
 * predates the version-matrix endpoint (offline / older deployments).
 */
export function useControlPlaneVersion(): string {
  const { data } = useQuery({
    queryKey: ['versions'],
    queryFn: versionApi.matrix,
    refetchInterval: 30_000,
    staleTime: 30_000,
    retry: 0,
  });

  const backendVersion = data?.controlPlane?.version?.trim();
  if (backendVersion) {
    return backendVersion.replace(/^v/, '');
  }
  return __APP_VERSION__;
}
