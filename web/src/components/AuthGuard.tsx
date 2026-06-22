// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useState } from 'react';
import { Navigate, Outlet, useLocation } from 'react-router-dom';
import { Loader2 } from 'lucide-react';
import { authApi } from '@/api/client';
import { getLastAuthStatus, setLastAuthStatus } from '@/lib/session';

type GuardState = 'checking' | 'allowed' | 'guest';

/**
 * Route guard for the WebUI. Calls /auth/session on mount: when login is
 * enforced (database configured) and the session is missing/expired, the user
 * is redirected to /login. When no database is configured, the app runs open.
 */
export function AuthGuard() {
  const location = useLocation();
  const [state, setState] = useState<GuardState>('checking');

  useEffect(() => {
    let cancelled = false;
    authApi
      .session()
      .then((res) => {
        if (cancelled) return;
        const nextState = !res.authRequired || res.authenticated ? 'allowed' : 'guest';
        setLastAuthStatus(nextState);
        setState(nextState);
      })
      .catch(() => {
        // Keep previously verified sessions usable during transient backend
        // errors, but do not grant access when there is no verified state.
        if (!cancelled) setState(getLastAuthStatus() ?? 'guest');
      });
    return () => {
      cancelled = true;
    };
  }, []);

  if (state === 'checking') {
    return (
      <div className="flex min-h-screen items-center justify-center bg-background text-muted-foreground">
        <Loader2 size={20} className="animate-spin" />
      </div>
    );
  }

  if (state === 'guest') {
    return <Navigate to="/login" replace state={{ from: location.pathname + location.search }} />;
  }

  return <Outlet />;
}
