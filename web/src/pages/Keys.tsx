// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Button } from '@/components/ui/button';
import { KeyRound, Save, Check } from 'lucide-react';

export default function KeysPage() {
  const [key, setKey] = useState('');
  const [saved, setSaved] = useState(false);
  const { t } = useTranslation('keys');

  useEffect(() => {
    setKey(localStorage.getItem('cube.apiKey') ?? '');
  }, []);

  const save = () => {
    localStorage.setItem('cube.apiKey', key.trim());
    setSaved(true);
    setTimeout(() => setSaved(false), 1500);
  };

  return (
    <div className="animate-fade-in space-y-5">
      <header>
        <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          {t('subtitle', { interpolation: { escapeValue: false } })}
        </p>
      </header>

      <Card>
        <CardHeader>
          <div className="flex items-center gap-3">
            <span className="flex h-10 w-10 items-center justify-center rounded-lg bg-primary/10 text-primary">
              <KeyRound size={18} />
            </span>
            <div>
              <CardTitle>{t('cardTitle')}</CardTitle>
              <CardDescription>{t('cardDesc')}</CardDescription>
            </div>
          </div>
        </CardHeader>
        <div className="mt-2 flex gap-2">
          <Input
            type="password"
            placeholder="sk-cube-…"
            value={key}
            onChange={(e) => setKey(e.target.value)}
            autoComplete="off"
          />
          <Button onClick={save}>
            {saved ? <Check size={14} /> : <Save size={14} />} {saved ? t('saved') : t('save')}
          </Button>
        </div>
      </Card>
    </div>
  );
}
