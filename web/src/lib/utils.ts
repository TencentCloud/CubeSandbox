// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { clsx, type ClassValue } from 'clsx';
import { twMerge } from 'tailwind-merge';

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export function formatBytes(mib: number | undefined | null): string {
  if (mib == null) return '—';
  if (mib < 1024) return `${mib} MiB`;
  return `${(mib / 1024).toFixed(1)} GiB`;
}

export function formatRelative(ts?: string | number | null, locale?: string): string {
  if (!ts) return '—';
  const d = new Date(ts);
  const diffSec = (Date.now() - d.getTime()) / 1000;
  const rtf = new Intl.RelativeTimeFormat(locale ?? navigator.language, { numeric: 'auto' });
  if (diffSec < 60) return rtf.format(-Math.max(1, Math.floor(diffSec)), 'second');
  if (diffSec < 3600) return rtf.format(-Math.floor(diffSec / 60), 'minute');
  if (diffSec < 86400) return rtf.format(-Math.floor(diffSec / 3600), 'hour');
  return rtf.format(-Math.floor(diffSec / 86400), 'day');
}

export function short(id: string, head = 6, tail = 4): string {
  if (!id) return '';
  if (id.length <= head + tail + 1) return id;
  return `${id.slice(0, head)}…${id.slice(-tail)}`;
}
