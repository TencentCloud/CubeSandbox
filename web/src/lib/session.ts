// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Lightweight WebUI session storage. JWT access/refresh tokens are sent as
// `Authorization: Bearer <accessToken>` (see lib/api.ts) and validated by
// CubeOps's /auth/session endpoint.

const ACCESS_TOKEN_KEY = 'cube.accessToken';
const REFRESH_TOKEN_KEY = 'cube.refreshToken';
const USER_KEY = 'cube.sessionUser';
const AUTH_STATUS_KEY = 'cube.authStatus';

export type AuthStatus = 'allowed' | 'guest';

export function getSessionToken(): string {
  return localStorage.getItem(ACCESS_TOKEN_KEY) ?? '';
}

export function getRefreshToken(): string {
  return localStorage.getItem(REFRESH_TOKEN_KEY) ?? '';
}

export function getSessionUser(): string {
  return localStorage.getItem(USER_KEY) ?? '';
}

export function setSession(accessToken: string, refreshToken: string, username: string): void {
  localStorage.setItem(ACCESS_TOKEN_KEY, accessToken);
  localStorage.setItem(REFRESH_TOKEN_KEY, refreshToken);
  localStorage.setItem(USER_KEY, username);
  setLastAuthStatus('allowed');
}

export function clearSession(): void {
  localStorage.removeItem(ACCESS_TOKEN_KEY);
  localStorage.removeItem(REFRESH_TOKEN_KEY);
  localStorage.removeItem(USER_KEY);
  // Legacy cleanup
  localStorage.removeItem('cube.session');
  setLastAuthStatus('guest');
}

export function getLastAuthStatus(): AuthStatus | null {
  const value = sessionStorage.getItem(AUTH_STATUS_KEY);
  return value === 'allowed' || value === 'guest' ? value : null;
}

export function setLastAuthStatus(status: AuthStatus): void {
  sessionStorage.setItem(AUTH_STATUS_KEY, status);
}
