// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Shared status → tailwind class helpers for template/snapshot detail views.

export function statusDotClass(status: string): string {
  switch (status.toUpperCase()) {
    case 'READY':
      return 'bg-cube-ok';
    case 'BUILDING':
    case 'RUNNING':
    case 'CREATING':
      return 'bg-cube-warn animate-pulse';
    case 'FAILED':
      return 'bg-cube-err';
    default:
      return 'bg-muted-foreground';
  }
}

export function statusTextClass(status: string): string {
  switch (status.toUpperCase()) {
    case 'READY':
      return 'text-cube-ok';
    case 'BUILDING':
    case 'RUNNING':
    case 'CREATING':
      return 'text-cube-warn';
    case 'FAILED':
      return 'text-cube-err';
    default:
      return 'text-muted-foreground';
  }
}
