// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import * as DropdownMenu from '@radix-ui/react-dropdown-menu';
import { Languages, Check } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';

const LANGS = [
  { code: 'en', label: 'English' },
  { code: 'zh', label: '简体中文' },
] as const;

export function LanguageSwitcher() {
  const { i18n } = useTranslation('topbar');
  const current = i18n.language as 'en' | 'zh';

  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <button
          className={cn(
            'flex h-9 items-center gap-1.5 rounded-lg border border-border/60 bg-muted/40 px-3',
            'text-sm text-muted-foreground transition hover:border-primary/30 hover:bg-muted/70',
            'focus:outline-none'
          )}
          aria-label="Switch language"
        >
          <Languages size={14} />
          <span className="text-xs font-medium uppercase">{current}</span>
        </button>
      </DropdownMenu.Trigger>

      <DropdownMenu.Portal>
        <DropdownMenu.Content
          align="end"
          sideOffset={6}
          className="z-50 min-w-[140px] overflow-hidden rounded-lg border border-border/60 bg-popover/95 p-1 shadow-2xl backdrop-blur-xl animate-fade-in"
        >
          {LANGS.map(({ code, label }) => (
            <DropdownMenu.Item
              key={code}
              onSelect={() => i18n.changeLanguage(code)}
              className="flex cursor-pointer items-center justify-between rounded-md px-3 py-2 text-sm text-foreground/90 outline-none hover:bg-primary/15 hover:text-foreground"
            >
              {label}
              {current === code && <Check size={13} className="text-primary" />}
            </DropdownMenu.Item>
          ))}
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  );
}
