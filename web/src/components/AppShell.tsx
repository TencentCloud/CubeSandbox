// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { Outlet } from 'react-router-dom';
import { Rail } from './Rail';
import { TopBar } from './TopBar';
import { CommandPalette } from './CommandPalette';
import { ToastProvider } from './ui/ToastProvider';
import { useGlobalHotkeys } from '@/hooks/useGlobalHotkeys';

function HotkeyMount() {
  useGlobalHotkeys();
  return null;
}

export function AppShell() {
  return (
    <div className="min-h-screen bg-background text-foreground">
      <Rail />
      <main className="ml-[68px] min-h-screen px-6 pb-10">
        <TopBar />
        <div className="mx-auto max-w-[1800px]">
          <Outlet />
        </div>
      </main>
      <CommandPalette />
      <ToastProvider />
      <HotkeyMount />
    </div>
  );
}
