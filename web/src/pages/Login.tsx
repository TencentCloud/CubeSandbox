// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useLocation, useNavigate } from 'react-router-dom';
import { LogIn, Loader2, Cuboid } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { authApi } from '@/api/client';
import { ApiError } from '@/lib/api';
import { setSession } from '@/lib/session';

export default function LoginPage() {
  const { t } = useTranslation('auth');
  const navigate = useNavigate();
  const location = useLocation();
  const [username, setUsername] = useState('admin');
  const [password, setPassword] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const redirectTo = (location.state as { from?: string } | null)?.from ?? '/';

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!username.trim() || !password) {
      setError(t('login.invalid'));
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      const res = await authApi.login({ username: username.trim(), password });
      setSession(res.token, res.username);
      navigate(redirectTo, { replace: true });
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setError(t('login.invalid'));
      } else {
        setError(err instanceof Error ? err.message : t('login.error'));
      }
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-4">
      <div className="w-full max-w-sm">
        <div className="mb-6 flex flex-col items-center text-center">
          <span className="flex h-12 w-12 items-center justify-center rounded-2xl bg-primary/10 text-primary">
            <Cuboid size={24} />
          </span>
          <h1 className="mt-3 text-xl font-semibold tracking-tight">{t('login.title')}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t('login.subtitle')}</p>
        </div>

        <form
          onSubmit={handleSubmit}
          className="space-y-4 rounded-2xl border border-border/60 bg-card p-6 shadow-sm"
        >
          <div className="space-y-1.5">
            <label className="text-sm font-medium" htmlFor="login-username">
              {t('login.username')}
            </label>
            <Input
              id="login-username"
              value={username}
              autoComplete="username"
              placeholder={t('login.usernamePlaceholder')}
              onChange={(e) => {
                setUsername(e.target.value);
                if (error) setError(null);
              }}
            />
          </div>
          <div className="space-y-1.5">
            <label className="text-sm font-medium" htmlFor="login-password">
              {t('login.password')}
            </label>
            <Input
              id="login-password"
              type="password"
              value={password}
              autoComplete="current-password"
              placeholder={t('login.passwordPlaceholder')}
              onChange={(e) => {
                setPassword(e.target.value);
                if (error) setError(null);
              }}
            />
          </div>

          {error && <p className="text-sm text-rose-500">{error}</p>}

          <Button type="submit" className="w-full gap-2" disabled={submitting}>
            {submitting ? <Loader2 size={16} className="animate-spin" /> : <LogIn size={16} />}
            {submitting ? t('login.submitting') : t('login.submit')}
          </Button>

          <p className="text-center text-xs text-muted-foreground">{t('login.defaultHint')}</p>
        </form>
      </div>
    </div>
  );
}
