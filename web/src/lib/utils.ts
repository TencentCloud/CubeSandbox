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
  if (Number.isNaN(d.getTime())) return '—';
  // diffSec > 0 ⇒ ts is in the past (e.g. startedAt)
  // diffSec < 0 ⇒ ts is in the future (e.g. endAt with active TTL)
  // The original implementation only handled the past branch and clamped
  // every future timestamp into "1 second ago" via `-Math.max(1, …)`.
  const diffSec = (Date.now() - d.getTime()) / 1000;
  const rtf = new Intl.RelativeTimeFormat(locale ?? navigator.language, { numeric: 'auto' });
  const abs = Math.abs(diffSec);
  // Intl.RelativeTimeFormat takes a SIGNED value: negative ⇒ past, positive ⇒ future.
  // We invert `diffSec` because diffSec was computed as past-positive above.
  const sign = diffSec >= 0 ? -1 : 1;
  if (abs < 60) {
    // Avoid the "0 seconds ago" / "in 0 seconds" edge by flooring to at least 1.
    return rtf.format(sign * Math.max(1, Math.floor(abs)), 'second');
  }
  if (abs < 3600) return rtf.format(sign * Math.floor(abs / 60), 'minute');
  if (abs < 86400) return rtf.format(sign * Math.floor(abs / 3600), 'hour');
  return rtf.format(sign * Math.floor(abs / 86400), 'day');
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
    navigator.clipboard.writeText(text).then(() => dispatch(true)).catch(() => {
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
