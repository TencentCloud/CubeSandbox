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
    <aside className="group/rail fixed inset-y-0 left-0 z-20 flex w-[68px] flex-col justify-between border-r border-border/60 bg-background/60 py-3 backdrop-blur-xl transition-[width] duration-200 ease-out hover:w-[190px]">
      <div className="flex flex-col gap-1 px-3">
        {/* Logo */}
        <div className="mb-4 flex h-10 items-center justify-center gap-3">
          <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-muted/60 ring-1 ring-border/60 glow-ring">
            <img src="/assets/cube-logo.svg" alt="CubeSandbox" className="h-7 w-7" />
          </div>
          <span className="overflow-hidden whitespace-nowrap text-base font-semibold tracking-tight text-foreground opacity-0 transition-opacity duration-150 group-hover/rail:opacity-100">CubeSandbox</span>
        </div>
        {NAV_ITEMS.map(({ to, icon: Icon, key }) => {
          const label = t(key);
          const active = to === '/' ? loc.pathname === '/' : loc.pathname.startsWith(to);
          return (
            <NavLink
              key={to}
              to={to}
              title={label}
              className={cn(
                'relative flex h-10 items-center justify-center gap-3 rounded-lg px-2 text-sm text-muted-foreground transition-all duration-150 ease-cube',
                'hover:bg-muted hover:text-foreground',
                active && 'bg-primary/15 text-primary font-medium',
                'group-hover/rail:justify-start group-hover/rail:px-3'
              )}
            >
              <Icon size={18} strokeWidth={1.75} className="shrink-0" />
              <span className="absolute left-full top-1/2 z-50 ml-2 hidden -translate-y-1/2 rounded-md bg-primary px-2 py-1 text-xs font-medium text-primary-foreground shadow-md group-hover/item:block group-hover/rail:relative group-hover/rail:left-0 group-hover/rail:top-auto group-hover/rail:translate-y-0 group-hover/rail:inline group-hover/rail:bg-transparent group-hover/rail:text-current group-hover/rail:shadow-none group-hover/rail:p-0">{label}</span>
            </NavLink>
          );
        })}
      </div>
      <div className="flex flex-col gap-2 px-3 pb-2">
        <a
          href="https://github.com/tencentcloud/CubeSandbox"
          target="_blank"
          rel="noopener noreferrer"
          title="GitHub"
          className="relative flex h-9 items-center justify-center gap-3 rounded-lg px-2 text-sm text-muted-foreground transition-all duration-150 ease-cube hover:bg-muted hover:text-foreground group-hover/rail:justify-start group-hover/rail:px-3"
        >
          <Github size={18} strokeWidth={1.75} className="shrink-0" />
          <span className="absolute left-full top-1/2 z-50 ml-2 hidden -translate-y-1/2 rounded-md bg-primary px-2 py-1 text-xs font-medium text-primary-foreground shadow-md group-hover/item:block group-hover/rail:relative group-hover/rail:left-0 group-hover/rail:top-auto group-hover/rail:translate-y-0 group-hover/rail:inline group-hover/rail:bg-transparent group-hover/rail:text-current group-hover/rail:shadow-none group-hover/rail:p-0">GitHub</span>
        </a>
        <div className="overflow-hidden whitespace-nowrap text-center text-xs tracking-wider text-muted-foreground/70 text-num opacity-0 transition-opacity duration-150 group-hover/rail:opacity-100 group-hover/rail:text-left">v{version}</div>
      </div>
    </aside>
  );
}
