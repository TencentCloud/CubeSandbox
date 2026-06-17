// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { NavLink, useLocation } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  LayoutDashboard,
  Boxes,
  Package,
  Server,
  Network,
  Activity,
  Bot,
  KeyRound,
  Settings,
  Store,
  Layers,
  FlaskConical,
  Github,
} from 'lucide-react';
import { cn } from '@/lib/utils';
import { useControlPlaneVersion } from '@/hooks/useControlPlaneVersion';

const NAV_ITEMS = [
  { to: '/', icon: LayoutDashboard, key: 'overview' },
  { to: '/sandboxes', icon: Boxes, key: 'sandboxes' },
  { to: '/templates', icon: Package, key: 'templates' },
  { to: '/nodes', icon: Server, key: 'nodes' },
  { to: '/versions', icon: Layers, key: 'versions' },
  { to: '/network', icon: Network, key: 'network' },
  { to: '/observability', icon: Activity, key: 'observability' },
  { to: '/keys', icon: KeyRound, key: 'apiKeys' },
  { to: '/store', icon: Store, key: 'store' },
  { to: '/examples', icon: FlaskConical, key: 'examples' },
  { to: '/agenthub', icon: Bot, key: 'agentHub' },
  { to: '/settings', icon: Settings, key: 'settings' },
] as const;

export function Rail() {
  const loc = useLocation();
  const { t } = useTranslation('nav');
  const version = useControlPlaneVersion();

  return (
    <aside className="fixed inset-y-0 left-0 z-20 flex w-[190px] flex-col justify-between border-r border-border/60 bg-background/60 py-4 backdrop-blur-xl">
      <div className="flex flex-col gap-1 px-3">
        <div className="mb-4 flex h-10 items-center gap-3 rounded-xl px-2">
          <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-muted/60 ring-1 ring-border/60 glow-ring">
            <img src="/assets/cube-logo.svg" alt="CubeSandbox" className="h-7 w-7" />
          </div>
          <span className="text-base font-semibold tracking-tight text-foreground">CubeSandbox</span>
        </div>
        {NAV_ITEMS.map(({ to, icon: Icon, key }) => {
          const label = t(key);
          const active = to === '/' ? loc.pathname === '/' : loc.pathname.startsWith(to);
          return (
            <NavLink
              key={to}
              to={to}
              className={cn(
                'group flex h-10 items-center gap-3 rounded-lg px-3 text-sm text-muted-foreground transition-all duration-150 ease-cube',
                'hover:bg-muted hover:text-foreground',
                active && 'bg-primary/15 text-primary font-medium'
              )}
            >
              <Icon size={18} strokeWidth={1.75} className="shrink-0" />
              <span>{label}</span>
            </NavLink>
          );
        })}
      </div>
      <div className="flex flex-col gap-2 px-3 pb-2">
        <a
          href="https://github.com/tencentcloud/CubeSandbox"
          target="_blank"
          rel="noopener noreferrer"
          className="group flex h-9 items-center gap-3 rounded-lg px-3 text-sm text-muted-foreground transition-all duration-150 ease-cube hover:bg-muted hover:text-foreground"
        >
          <Github size={18} strokeWidth={1.75} className="shrink-0" />
          <span>GitHub</span>
        </a>
        <div className="px-3 text-xs tracking-wider text-muted-foreground/70 text-num">v{version}</div>
      </div>
    </aside>
  );
}
