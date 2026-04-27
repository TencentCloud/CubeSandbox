// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { create } from 'zustand';

interface CommandPaletteState {
  isOpen: boolean;
  open: (v: boolean) => void;
  toggle: () => void;
}

export const useCommandPaletteStore = create<CommandPaletteState>((set) => ({
  isOpen: false,
  open: (v) => set({ isOpen: v }),
  toggle: () => set((s) => ({ isOpen: !s.isOpen })),
}));
