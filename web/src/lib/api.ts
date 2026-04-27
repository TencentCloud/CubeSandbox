// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Minimal fetch wrapper. Auth header can be injected via the api-key header.

export type ApiInit = RequestInit & { params?: Record<string, string | number | boolean | undefined> };

const BASE = '/cubeapi/v1'; // same-origin via Vite proxy in dev; prefixed in prod

function buildQuery(params?: ApiInit['params']): string {
  if (!params) return '';
  const usp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === '') continue;
    usp.set(k, String(v));
  }
  const s = usp.toString();
  return s ? `?${s}` : '';
}

export class ApiError extends Error {
  status: number;
  body?: unknown;
  constructor(status: number, message: string, body?: unknown) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

export async function api<T = unknown>(path: string, init: ApiInit = {}): Promise<T> {
  const { params, headers, ...rest } = init;
  const query = buildQuery(params);

  const apiKey = localStorage.getItem('cube.apiKey') ?? '';
  const url = `${BASE}${path}${query}`;
  const resp = await fetch(url, {
    ...rest,
    headers: {
      ...(rest.body != null ? { 'Content-Type': 'application/json' } : {}),
      ...(apiKey ? { 'X-API-Key': apiKey } : {}),
      ...(headers ?? {}),
    },
  });
  const text = await resp.text();
  const body = text ? safeJson(text) : undefined;
  if (!resp.ok) {
    const msg = (body && typeof body === 'object' && 'error' in body && (body as any).error)
      || `${resp.status} ${resp.statusText}`;
    throw new ApiError(resp.status, String(msg), body);
  }
  return body as T;
}

function safeJson(s: string): unknown {
  try {
    return JSON.parse(s);
  } catch {
    return s;
  }
}
