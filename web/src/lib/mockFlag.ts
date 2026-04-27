// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Mock-mode toggle. Enabled via any of:
//   - VITE_USE_MOCK=1 at build time
//   - ?mock=1 query string (persisted to localStorage)
//   - localStorage.setItem('cube.useMock', '1')
//
// Read is kept synchronous so the api() wrapper can branch without awaits.

const LS_KEY = 'cube.useMock';

// Parse URL once at load, persist for subsequent navigations.
if (typeof window !== 'undefined') {
  const usp = new URLSearchParams(window.location.search);
  const v = usp.get('mock');
  if (v === '1' || v === 'true') localStorage.setItem(LS_KEY, '1');
  if (v === '0' || v === 'false') localStorage.setItem(LS_KEY, '0');
}

export function isMockEnabled(): boolean {
  try {
    if (import.meta.env.VITE_USE_MOCK === '1' || import.meta.env.VITE_USE_MOCK === 'true') return true;
  } catch {
    /* ignore */
  }
  if (typeof window === 'undefined') return false;
  return localStorage.getItem(LS_KEY) === '1';
}

export function setMockEnabled(on: boolean) {
  localStorage.setItem(LS_KEY, on ? '1' : '0');
  // Force a soft reload so all react-query caches reset.
  window.location.reload();
}
