// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { templateApi } from '@/api/client';
import { Card, CardHeader, CardTitle, CardDescription } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { Package } from 'lucide-react';
import { formatRelative } from '@/lib/utils';

export default function TemplatesPage() {
  const { data, isLoading } = useQuery({ queryKey: ['templates'], queryFn: templateApi.list });
  const { t } = useTranslation('templates');

  return (
    <div className="animate-fade-in space-y-5">
      <header>
        <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
        <p className="mt-1 text-sm text-muted-foreground">{t('subtitle')}</p>
      </header>

      {isLoading && (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-28" />
          ))}
        </div>
      )}

      {data && data.length === 0 && (
        <Card>
          <div className="py-16 text-center text-sm text-muted-foreground">
            {t('noTemplates')}
          </div>
        </Card>
      )}

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
        {data?.map((tpl) => (
          <Link key={tpl.templateID} to={`/templates/${tpl.templateID}`} className="block">
            <Card className="panel-hover h-full">
              <CardHeader>
                <div className="flex items-center gap-3">
                  <span className="flex h-10 w-10 items-center justify-center rounded-lg bg-gradient-to-br from-primary/20 to-cube-violet/20 text-primary ring-1 ring-primary/20">
                    <Package size={18} />
                  </span>
                  <div>
                    <CardTitle className="text-base">{tpl.templateID}</CardTitle>
                    <CardDescription className="font-mono text-[11px]">{tpl.templateID}</CardDescription>
                  </div>
                </div>
                <Badge tone={tpl.status === 'ready' ? 'ok' : tpl.status === 'failed' ? 'err' : 'warn'}>
                  {tpl.status}
                </Badge>
              </CardHeader>
              <div className="grid grid-cols-2 gap-3 pt-3 text-xs text-muted-foreground">
                <div>
                  <div className="text-[10px] uppercase tracking-wider">{t('col.instance')}</div>
                  <div className="mt-0.5 text-foreground/80">{tpl.instanceType ?? t('instanceDefault')}</div>
                </div>
                <div>
                  <div className="text-[10px] uppercase tracking-wider">{t('col.created')}</div>
                  <div className="mt-0.5 text-foreground/80">{formatRelative(tpl.createdAt)}</div>
                </div>
              </div>
              <div className="mt-3 space-y-1 text-xs text-muted-foreground">
                <div className="truncate">{t('col.version')}: <span className="text-foreground/80">{tpl.version ?? '—'}</span></div>
                <div className="truncate">{t('col.image')}: <span className="text-foreground/80">{tpl.imageInfo ?? '—'}</span></div>
              </div>
            </Card>
          </Link>
        ))}
      </div>
    </div>
  );
}
