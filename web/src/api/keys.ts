// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.
//
// SDK API keys managed by the e2b-compatible gateway (e2b-shim). Each key is
// bound to a set of templates: it can only create sandboxes from those
// templates and only sees / operates sandboxes of those templates. Admin keys
// bypass the template check. The gateway authenticates these management calls
// by forwarding this session's JWT to CubeOps (`/api/v1/auth/session`).
//
// Wire path: WebUI nginx `/keysapi/*` -> e2b-shim `/_gw/api/*`.

import { ops } from '@/lib/api';

const KEYS_BASE = '/keysapi';

export interface ApiKeyRecord {
  key: string;
  name: string;
  templates: string[];
  admin: boolean;
  enabled: boolean;
  createdAt?: string | null;
  updatedAt?: string | null;
  note?: string | null;
}

export interface ApiKeyInput {
  name: string;
  templates: string[];
  admin?: boolean;
  enabled?: boolean;
  note?: string;
  /** Optional explicit key (`e2b_<hex>`); generated when omitted. */
  key?: string;
}

export interface GatewayTemplate {
  templateID: string;
  aliases: string[];
  status: string;
  instanceType?: string | null;
  createdAt?: string | null;
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  // Reuse the session token the ops() helper keeps in localStorage; keysapi is
  // not under /opsapi/v1 so we cannot call ops() directly.
  const token = localStorage.getItem('cube.accessToken') ?? '';
  const resp = await fetch(`${KEYS_BASE}${path}`, {
    ...init,
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(init.headers ?? {}),
    },
  });
  if (resp.status === 401) {
    // Session expired: let ops() run its refresh path, then retry once.
    await ops('/auth/session').catch(() => undefined);
    const token2 = localStorage.getItem('cube.accessToken') ?? '';
    const retry = await fetch(`${KEYS_BASE}${path}`, {
      ...init,
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/json',
        ...(token2 ? { Authorization: `Bearer ${token2}` } : {}),
        ...(init.headers ?? {}),
      },
    });
    return finish<T>(retry);
  }
  return finish<T>(resp);
}

async function finish<T>(resp: Response): Promise<T> {
  if (!resp.ok) {
    let msg = `${resp.status} ${resp.statusText}`;
    try {
      const body = (await resp.json()) as { message?: string; error?: string };
      msg = body.message ?? body.error ?? msg;
    } catch {
      /* ignore */
    }
    throw new Error(msg);
  }
  return (await resp.json()) as T;
}

export const keysApi = {
  list: () => request<ApiKeyRecord[]>('/keys'),
  templates: () => request<GatewayTemplate[]>('/templates'),
  create: (input: ApiKeyInput) => request<ApiKeyRecord>('/keys', { method: 'POST', body: JSON.stringify(input) }),
  update: (key: string, patch: Partial<ApiKeyInput>) =>
    request<ApiKeyRecord>(`/keys/${encodeURIComponent(key)}`, { method: 'PUT', body: JSON.stringify(patch) }),
  remove: (key: string) => request<{ deleted: string }>(`/keys/${encodeURIComponent(key)}`, { method: 'DELETE' }),
};
