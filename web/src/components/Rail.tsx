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
  KeyRound,
  Settings,
  Github,
} from 'lucide-react';
import { cn } from '@/lib/utils';

const NAV_ITEMS = [
  { to: '/', icon: LayoutDashboard, key: 'overview' },
  { to: '/sandboxes', icon: Boxes, key: 'sandboxes' },
  { to: '/templates', icon: Package, key: 'templates' },
  { to: '/nodes', icon: Server, key: 'nodes' },
  { to: '/network', icon: Network, key: 'network' },
  { to: '/observability', icon: Activity, key: 'observability' },
  { to: '/keys', icon: KeyRound, key: 'apiKeys' },
  { to: '/settings', icon: Settings, key: 'settings' },
] as const;

export function Rail() {
  const loc = useLocation();
  const { t } = useTranslation('nav');

  return (
    <aside className="fixed inset-y-0 left-0 z-20 flex w-[68px] flex-col items-center justify-between border-r border-border/60 bg-background/60 py-4 backdrop-blur-xl">
      <div className="flex flex-col items-center gap-2">
        <div className="mb-3 flex h-10 w-10 items-center justify-center rounded-xl bg-muted/60 ring-1 ring-border/60 glow-ring">
          <img src="/assets/cube-logo.svg" alt="CubeSandbox" className="h-7 w-7" />
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
                'group relative flex h-10 w-10 items-center justify-center rounded-lg text-muted-foreground transition-all duration-150 ease-cube',
                'hover:bg-muted hover:text-foreground',
                active && 'bg-primary/15 text-primary ring-1 ring-primary/30'
              )}
            >
              <Icon size={18} strokeWidth={1.75} />
              <span className="pointer-events-none absolute left-12 whitespace-nowrap rounded-md bg-popover/95 px-2 py-1 text-xs text-popover-foreground opacity-0 shadow-xl ring-1 ring-border/60 transition-opacity group-hover:opacity-100">
                {label}
              </span>
            </NavLink>
          );
        })}
      </div>
      <div className="flex flex-col items-center gap-4 pb-2">
        <a
          href="https://github.com/tencentcloud/CubeSandbox"
          target="_blank"
          rel="noopener noreferrer"
          className="group relative flex h-10 w-10 items-center justify-center rounded-lg text-muted-foreground transition-all duration-150 ease-cube hover:bg-muted hover:text-foreground"
        >
          <Github size={18} strokeWidth={1.75} />
          <span className="pointer-events-none absolute left-12 whitespace-nowrap rounded-md bg-popover/95 px-2 py-1 text-xs text-popover-foreground opacity-0 shadow-xl ring-1 ring-border/60 transition-opacity group-hover:opacity-100">
            GitHub
          </span>
        </a>
        <div className="text-[10px] tracking-widest text-muted-foreground/60">v0.1</div>
      </div>
    </aside>
  );
}
