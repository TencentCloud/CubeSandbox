// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useTranslation } from 'react-i18next';
import type { ParseKeys } from 'i18next';
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import type { LucideIcon } from 'lucide-react';

type PlaceholderKey = ParseKeys<'placeholder'>;

export function Placeholder({
  titleKey,
  descriptionKey,
  icon: Icon,
}: {
  titleKey: PlaceholderKey;
  descriptionKey: PlaceholderKey;
  icon: LucideIcon;
}) {
  const { t } = useTranslation('placeholder');
  const { t: tCommon } = useTranslation('common');

  return (
    <div className="animate-fade-in space-y-5">
      <header className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t(titleKey)}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t(descriptionKey)}</p>
        </div>
        <Badge tone="info">{tCommon('preview')}</Badge>
      </header>
      <Card>
        <CardHeader>
          <div className="flex items-center gap-3">
            <span className="flex h-10 w-10 items-center justify-center rounded-lg bg-primary/10 text-primary">
              <Icon size={18} />
            </span>
            <div>
              <CardTitle>{tCommon('comingSoon')}</CardTitle>
              <CardDescription>{tCommon('comingSoonDesc')}</CardDescription>
            </div>
          </div>
        </CardHeader>
      </Card>
    </div>
  );
}
