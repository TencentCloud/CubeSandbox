// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect } from 'react';
import { Command } from 'cmdk';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  Boxes,
  Package,
  Server,
  LayoutDashboard,
  Activity,
  KeyRound,
  Settings,
  Plus,
} from 'lucide-react';
import { useCommandPaletteStore } from '@/store/ui';

export function CommandPalette() {
  const isOpen = useCommandPaletteStore((s) => s.isOpen);
  const setOpen = useCommandPaletteStore((s) => s.open);
  const nav = useNavigate();
  const { t } = useTranslation('command');
  const { t: tNav } = useTranslation('nav');

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        setOpen(true);
      }
      if (e.key === 'Escape') setOpen(false);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [setOpen]);

  const go = (to: string) => {
    setOpen(false);
    nav(to);
  };

  if (!isOpen) return null;

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center bg-foreground/40 backdrop-blur-sm animate-fade-in"
      onClick={() => setOpen(false)}
    >
      <div
        className="mt-[12vh] w-[640px] overflow-hidden rounded-xl border border-border/60 bg-popover/95 shadow-2xl ring-1 ring-primary/10"
        onClick={(e) => e.stopPropagation()}
      >
        <Command label={t('label')} className="[&_[cmdk-input]]:bg-transparent">
          <Command.Input
            autoFocus
            placeholder={t('placeholder')}
            className="h-12 w-full border-b border-border/60 bg-transparent px-4 text-sm outline-none placeholder:text-muted-foreground"
          />
          <Command.List className="max-h-[420px] overflow-y-auto p-2">
            <Command.Empty className="py-8 text-center text-sm text-muted-foreground">
              {t('noResults')}
            </Command.Empty>

            <Command.Group heading={t('groupNavigate')} className="px-2 pb-2 pt-1 text-[11px] uppercase tracking-widest text-muted-foreground">
              <Item icon={<LayoutDashboard size={14} />} label={tNav('overview')} onSelect={() => go('/')} />
              <Item icon={<Boxes size={14} />} label={tNav('sandboxes')} onSelect={() => go('/sandboxes')} />
              <Item icon={<Package size={14} />} label={tNav('templates')} onSelect={() => go('/templates')} />
              <Item icon={<Server size={14} />} label={tNav('nodes')} onSelect={() => go('/nodes')} />
              <Item icon={<Activity size={14} />} label={tNav('observability')} onSelect={() => go('/observability')} />
              <Item icon={<KeyRound size={14} />} label={tNav('apiKeys')} onSelect={() => go('/keys')} />
              <Item icon={<Settings size={14} />} label={tNav('settings')} onSelect={() => go('/settings')} />
            </Command.Group>

            <Command.Group heading={t('groupActions')} className="px-2 pb-2 pt-1 text-[11px] uppercase tracking-widest text-muted-foreground">
              <Item icon={<Plus size={14} />} label={t('createSandbox')} onSelect={() => go('/sandboxes/new')} />
            </Command.Group>
          </Command.List>
        </Command>
      </div>
    </div>
  );
}

function Item({ icon, label, onSelect }: { icon: React.ReactNode; label: string; onSelect: () => void }) {
  return (
    <Command.Item
      onSelect={onSelect}
      className="flex cursor-pointer items-center gap-3 rounded-md px-3 py-2 text-sm text-foreground/90 aria-selected:bg-primary/15 aria-selected:text-foreground"
    >
      <span className="text-muted-foreground">{icon}</span>
      {label}
    </Command.Item>
  );
}
