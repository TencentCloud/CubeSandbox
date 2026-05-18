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
