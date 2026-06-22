// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Lightweight WebUI session storage. The token is sent as `X-Session-Token`
// (see lib/api.ts) and validated by CubeAPI's /auth/session endpoint.

const TOKEN_KEY = 'cube.session';
const USER_KEY = 'cube.sessionUser';
const AUTH_STATUS_KEY = 'cube.authStatus';

export type AuthStatus = 'allowed' | 'guest';

export function getSessionToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? '';
}

export function getSessionUser(): string {
  return localStorage.getItem(USER_KEY) ?? '';
}

export function setSession(token: string, username: string): void {
  localStorage.setItem(TOKEN_KEY, token);
  localStorage.setItem(USER_KEY, username);
  setLastAuthStatus('allowed');
}

export function clearSession(): void {
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem(USER_KEY);
  setLastAuthStatus('guest');
}

export function getLastAuthStatus(): AuthStatus | null {
  const value = sessionStorage.getItem(AUTH_STATUS_KEY);
  return value === 'allowed' || value === 'guest' ? value : null;
}

export function setLastAuthStatus(status: AuthStatus): void {
  sessionStorage.setItem(AUTH_STATUS_KEY, status);
}
