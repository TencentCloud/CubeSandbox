// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import * as DropdownMenu from '@radix-ui/react-dropdown-menu';
import { Sun, Moon, Monitor, Check } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import { useThemeStore, type ThemeMode } from '@/store/theme';

const OPTIONS: { value: ThemeMode; icon: typeof Sun; key: 'light' | 'dark' | 'system' }[] = [
  { value: 'light', icon: Sun, key: 'light' },
  { value: 'dark', icon: Moon, key: 'dark' },
  { value: 'system', icon: Monitor, key: 'system' },
];

export function ThemeToggle() {
  const mode = useThemeStore((s) => s.mode);
  const setMode = useThemeStore((s) => s.setMode);
  const { t } = useTranslation('theme');

  const Current = OPTIONS.find((o) => o.value === mode)?.icon ?? Monitor;

  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <button
          className={cn(
            'flex h-9 w-9 items-center justify-center rounded-lg border border-border/60 bg-muted/40',
            'text-muted-foreground transition hover:border-primary/30 hover:bg-muted/70 hover:text-foreground',
            'focus:outline-none'
          )}
          aria-label={t('toggle')}
          title={t('toggle')}
        >
          <Current size={15} />
        </button>
      </DropdownMenu.Trigger>

      <DropdownMenu.Portal>
        <DropdownMenu.Content
          align="end"
          sideOffset={6}
          className="z-50 min-w-[160px] overflow-hidden rounded-lg border border-border/60 bg-popover/95 p-1 shadow-2xl backdrop-blur-xl animate-fade-in"
        >
          {OPTIONS.map(({ value, icon: Icon, key }) => (
            <DropdownMenu.Item
              key={value}
              onSelect={() => setMode(value)}
              className="flex cursor-pointer items-center justify-between rounded-md px-3 py-2 text-sm text-foreground/90 outline-none hover:bg-primary/15 hover:text-foreground"
            >
              <span className="flex items-center gap-2">
                <Icon size={14} />
                {t(key)}
              </span>
              {mode === value && <Check size={13} className="text-primary" />}
            </DropdownMenu.Item>
          ))}
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  );
}
