// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { Search, Command as CmdIcon, Bell, Sparkles, FlaskConical } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from './ui/button';
import { LanguageSwitcher } from './LanguageSwitcher';
import { ThemeToggle } from './ThemeToggle';
import { useCommandPaletteStore } from '@/store/ui';
import { isMockEnabled, setMockEnabled } from '@/lib/mockFlag';

export function TopBar() {
  const openPalette = useCommandPaletteStore((s) => s.open);
  const mockOn = isMockEnabled();
  const { t } = useTranslation('topbar');

  return (
    <header className="sticky top-0 z-10 -mx-6 mb-6 flex items-center justify-between gap-4 border-b border-border/60 bg-background/70 px-6 py-3 backdrop-blur-xl">
      <div className="flex items-center gap-3">
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <Sparkles size={14} className="text-cube-violet" />
          <span className="font-medium text-foreground/90">CubeSandbox</span>
          <span className="text-muted-foreground/60">/</span>
          <span className="text-foreground/70">{t('breadcrumb')}</span>
        </div>
      </div>

      <button
        onClick={() => openPalette(true)}
        className="group flex h-9 w-[420px] items-center justify-between gap-3 rounded-lg border border-border/60 bg-muted/40 px-3 text-sm text-muted-foreground transition hover:border-primary/30 hover:bg-muted/70"
      >
        <span className="flex items-center gap-2">
          <Search size={14} />
          {t('searchPlaceholder')}
        </span>
        <kbd className="flex items-center gap-1 rounded border border-border/60 bg-background/60 px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
          <CmdIcon size={10} />K
        </kbd>
      </button>

      <div className="flex items-center gap-2">
        <LanguageSwitcher />
        <ThemeToggle />
        <Button
          variant={mockOn ? 'default' : 'outline'}
          size="sm"
          title={mockOn ? t('mockOnTitle') : t('mockOffTitle')}
          onClick={() => setMockEnabled(!mockOn)}
        >
          <FlaskConical size={14} />
          {mockOn ? t('mockOn') : t('mockOff')}
        </Button>
        <Button variant="ghost" size="icon" aria-label={t('notifications')}>
          <Bell size={16} />
        </Button>
        <div className="ml-1 flex h-8 w-8 items-center justify-center rounded-full bg-gradient-to-br from-cube-violet/60 to-primary/80 text-xs font-semibold text-primary-foreground">
          C
        </div>
      </div>
    </header>
  );
}
