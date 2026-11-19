// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { NavLink } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';

export function NodesHeaderTabs({
  subtitle,
  nodeCount,
  machineTypeCount,
}: {
  subtitle: string;
  nodeCount?: number;
  machineTypeCount?: number;
}) {
  const { t } = useTranslation('nodes');
  const tabs = [
    { to: '/nodes', label: t('tabs.nodes'), count: nodeCount, end: true },
    {
      to: '/nodes/machine-types',
      label: t('tabs.machineTypes'),
      count: machineTypeCount,
      end: false,
    },
  ];

  return (
    <header className="space-y-4">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
        <p className="mt-1 text-sm text-muted-foreground">{subtitle}</p>
      </div>
      <div className="flex w-fit gap-1 rounded-lg border border-border/60 bg-card/40 p-1">
        {tabs.map((tab) => (
          <NavLink
            key={tab.to}
            to={tab.to}
            end={tab.end}
            className={({ isActive }) =>
              cn(
                'rounded-md px-3 py-1.5 text-sm text-muted-foreground transition-colors',
                isActive && 'bg-background text-foreground shadow-sm',
              )
            }
          >
            {tab.label}
            {tab.count !== undefined && (
              <span className="ml-2 text-xs text-muted-foreground text-num">{tab.count}</span>
            )}
          </NavLink>
        ))}
      </div>
    </header>
  );
}
