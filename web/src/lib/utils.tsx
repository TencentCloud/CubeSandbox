// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import React from 'react';
import { clsx, type ClassValue } from 'clsx';
import { twMerge } from 'tailwind-merge';

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

export function getStatusBadge(status: string, t: any, error?: string) {
  switch (status) {
    case 'pending':
      return (
        <span className="inline-flex items-center text-muted-foreground font-medium text-[11px] gap-1.5">
          <span className="h-1.5 w-1.5 rounded-full bg-muted-foreground/50"></span>
          {t('jobPending')}
        </span>
      );
    case 'running':
      return (
        <span className="inline-flex items-center text-primary font-medium text-[11px] gap-1.5">
          <span className="h-1.5 w-1.5 rounded-full bg-primary animate-pulse"></span>
          {t('jobRunning')}
        </span>
      );
    case 'succeeded':
      return (
        <span className="inline-flex items-center text-emerald-600 font-medium text-[11px] gap-1.5">
          <span className="h-1.5 w-1.5 rounded-full bg-emerald-500"></span>
          {t('jobSucceeded')}
        </span>
      );
    case 'failed':
      return (
        <div className="flex items-center gap-2">
          <span className="inline-flex items-center text-destructive font-medium text-[11px] gap-1.5">
            <span className="h-1.5 w-1.5 rounded-full bg-destructive"></span>
            {t('jobFailed')}
          </span>
          {error && (
            <span className="text-xs text-destructive/80 line-clamp-1 max-w-xs">{error}</span>
          )}
        </div>
      );
    case 'cancelled':
      return (
        <span className="inline-flex items-center text-muted-foreground font-medium text-[11px] gap-1.5">
          <span className="h-1.5 w-1.5 rounded-full bg-muted-foreground/30"></span>
          {t('jobCancelled')}
        </span>
      );
    default:
      return (
        <span className="inline-flex items-center text-muted-foreground font-medium text-[11px] gap-1.5">
          <span className="h-1.5 w-1.5 rounded-full bg-border"></span>
          {status}
        </span>
      );
  }
}

export function formatBytes(mib: number | undefined | null): string {
  if (mib == null) return '—';
  if (mib < 1024) return `${mib} MiB`;
  return `${(mib / 1024).toFixed(1)} GiB`;
}

export function formatCpu(cpuMilli?: number | null, cpuCount?: number | string | null): string {
  if (cpuMilli == null && cpuCount == null) return '—';
  // Prefer the exact millicore value; fall back to cpuCount. The fallback
  // tolerates the legacy K8s-style string ("2000m") older CubeOps returned,
  // which would otherwise render as "NaNm" through arithmetic.
  let milli = cpuMilli && cpuMilli > 0 ? cpuMilli : 0;
  if (milli <= 0 && cpuCount != null) {
    milli = cpuCountToMilli(cpuCount);
  }
  if (milli <= 0) return '0';
  if (milli % 1000 === 0) return `${milli / 1000}C`;
  return `${milli}m`;
}

function cpuCountToMilli(cpuCount: number | string): number {
  if (typeof cpuCount === 'string') {
    const s = cpuCount.trim();
    if (s.endsWith('m')) return Number(s.slice(0, -1)) || 0;
    return Number(s) * 1000 || 0;
  }
  return cpuCount * 1000;
}

export function formatArtifactBytes(n: number): string {
  if (!n) return '—';
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MiB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GiB`;
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

/**
 * Copy text to clipboard with execCommand fallback for HTTP (non-HTTPS) environments.
 * On success, dispatches a 'cube:toast' custom event so ToastProvider can show a notification.
 */
export function copyToClipboard(text: string, message = 'Copied'): void {
  const dispatch = (ok: boolean) => {
    if (ok) {
      window.dispatchEvent(new CustomEvent('cube:toast', { detail: { message } }));
    }
  };

  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard
      .writeText(text)
      .then(() => dispatch(true))
      .catch(() => {
        fallbackCopy(text, dispatch);
      });
  } else {
    fallbackCopy(text, dispatch);
  }
}

function fallbackCopy(text: string, cb: (ok: boolean) => void) {
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.cssText = 'position:fixed;top:-9999px;left:-9999px;opacity:0';
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    const ok = document.execCommand('copy');
    document.body.removeChild(ta);
    cb(ok);
  } catch {
    cb(false);
  }
}

/**
 * Translate a template-deletion API error into a human-friendly message.
 * Falls back to the raw error message if no known pattern matches.
 */
export function formatDeleteError(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err);
  if (/template is still in use/i.test(raw)) {
    return '该模板当前有沙箱实例正在使用，请先销毁所有关联沙箱后再删除。';
  }
  if (/build job is still active|attempt in progress/i.test(raw)) {
    return '模板正在构建中，请等待构建完成后再删除。';
  }
  if (/cleanup locator is missing/i.test(raw)) {
    return '模板清理信息不完整，无法自动删除，请联系管理员。';
  }
  return raw;
}
