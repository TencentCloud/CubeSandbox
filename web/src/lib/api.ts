// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Minimal fetch wrapper with dual base URLs:
// - `api()`  → SDK/E2B endpoints (root path, JWT Bearer auth via CubeOps)
// - `ops()`  → CubeOps ops endpoints (/opsapi/v1 prefix, JWT Bearer auth)

export type ApiInit = RequestInit & {
  params?: Record<string, string | number | boolean | undefined>;
};

const SDK_BASE = ''; // CubeAPI root path (E2B compatible)
const OPS_BASE = '/opsapi/v1'; // CubeOps via nginx proxy

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

// --- Token management ---

function getAccessToken(): string {
  return localStorage.getItem('cube.accessToken') ?? '';
}

function getRefreshToken(): string {
  return localStorage.getItem('cube.refreshToken') ?? '';
}

export function setTokens(accessToken: string, refreshToken?: string) {
  localStorage.setItem('cube.accessToken', accessToken);
  if (refreshToken) {
    localStorage.setItem('cube.refreshToken', refreshToken);
  }
}

export function clearTokens() {
  localStorage.removeItem('cube.accessToken');
  localStorage.removeItem('cube.refreshToken');
  localStorage.removeItem('cube.session'); // legacy cleanup
}

let refreshing: Promise<string | null> | null = null;

async function refreshAccessToken(): Promise<string | null> {
  const rt = getRefreshToken();
  if (!rt) return null;
  try {
    const resp = await fetch(`${OPS_BASE}/auth/refresh`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ refreshToken: rt }),
    });
    if (!resp.ok) return null;
    const data = await resp.json();
    if (data.accessToken) {
      localStorage.setItem('cube.accessToken', data.accessToken);
      // M2: backend rotates the refresh token on each refresh (old one is
      // revoked). We must persist the new refresh token, otherwise the next
      // refresh will use the now-revoked old token and fail with 401,
      // kicking the user out after ~15-30 min.
      if (data.refreshToken) {
        localStorage.setItem('cube.refreshToken', data.refreshToken);
      }
      return data.accessToken as string;
    }
  } catch {
    // network error — fall through
  }
  return null;
}

// Refresh margin: a token expiring within this window counts as stale.
const REFRESH_SKEW_MS = 30_000;

// Reads `exp` from the JWT payload without verifying the signature — the
// server remains the authority; this only decides whether a refresh is worth
// attempting before a non-HTTP caller uses the token.
function accessTokenExpiring(token: string): boolean {
  try {
    const seg = token.split('.')[1];
    const b64 = seg.replace(/-/g, '+').replace(/_/g, '/');
    const payload = JSON.parse(atob(b64 + '='.repeat((4 - (b64.length % 4)) % 4))) as {
      exp?: number;
    };
    return typeof payload.exp === 'number' && payload.exp * 1000 <= Date.now() + REFRESH_SKEW_MS;
  } catch {
    return false; // not a JWT — let the server judge the token as-is
  }
}

// Token freshness for callers that bypass the HTTP layer. The terminal
// WebSocket cannot carry an Authorization header (browser limitation) and
// sends the token as a subprotocol, so api()/ops()'s 401 auto-refresh never
// runs for it; refreshing here keeps a long-idle page from failing the WS
// handshake on an expired token. Resolves to null when there is no token
// (auth-disabled deployments) or the refresh failed.
export async function ensureFreshToken(): Promise<string | null> {
  const token = getAccessToken();
  if (!token) return null;
  if (!accessTokenExpiring(token)) return token;
  if (!refreshing) {
    refreshing = refreshAccessToken().finally(() => {
      refreshing = null;
    });
  }
  return refreshing;
}

// --- SDK API (CubeAPI via CubeOps proxy, JWT Bearer auth) ---

export async function api<T = unknown>(path: string, init: ApiInit = {}): Promise<T> {
  const { params, headers, ...rest } = init;
  const query = buildQuery(params);

  const accessToken = getAccessToken();
  const url = `${SDK_BASE}${path}${query}`;

  const doFetch = (token: string) =>
    fetch(url, {
      ...rest,
      headers: {
        ...(rest.body != null ? { 'Content-Type': 'application/json' } : {}),
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
        ...(headers ?? {}),
      },
    });

  let resp = await doFetch(accessToken);

  // Auto-refresh on 401 (same logic as ops())
  if (resp.status === 401 && accessToken) {
    if (!refreshing) {
      refreshing = refreshAccessToken().finally(() => {
        refreshing = null;
      });
    }
    const newToken = await refreshing;
    if (newToken) {
      resp = await doFetch(newToken);
    }
  }

  const text = await resp.text();
  const body = text ? safeJson(text) : undefined;
  if (!resp.ok) {
    const msg =
      (body && typeof body === 'object' && 'error' in body && (body as any).error) ||
      (body && typeof body === 'object' && 'message' in body && (body as any).message) ||
      `${resp.status} ${resp.statusText}`;
    throw new ApiError(resp.status, String(msg), body);
  }
  return body as T;
}

// --- Ops API (CubeOps, JWT Bearer auth) ---

export async function ops<T = unknown>(path: string, init: ApiInit = {}): Promise<T> {
  const { params, headers, ...rest } = init;
  const query = buildQuery(params);

  const accessToken = getAccessToken();
  const url = `${OPS_BASE}${path}${query}`;

  const doFetch = (token: string) =>
    fetch(url, {
      ...rest,
      headers: {
        ...(rest.body != null ? { 'Content-Type': 'application/json' } : {}),
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
        ...(headers ?? {}),
      },
    });

  let resp = await doFetch(accessToken);

  // Auto-refresh on 401
  if (resp.status === 401 && accessToken) {
    if (!refreshing) {
      refreshing = refreshAccessToken().finally(() => {
        refreshing = null;
      });
    }
    const newToken = await refreshing;
    if (newToken) {
      resp = await doFetch(newToken);
    }
  }

  const text = await resp.text();
  const body = text ? safeJson(text) : undefined;
  if (!resp.ok) {
    const msg =
      (body && typeof body === 'object' && 'error' in body && (body as any).error) ||
      (body && typeof body === 'object' && 'message' in body && (body as any).message) ||
      `${resp.status} ${resp.statusText}`;
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
