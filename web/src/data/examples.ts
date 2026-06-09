// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { Beaker, FileText, Timer, Network, type LucideIcon } from 'lucide-react';

export type ExampleCategoryId = 'basics' | 'filesystem' | 'lifecycle' | 'network';

export interface ExampleCategory {
  /** Stable id matching `category` field on the backend ExampleMeta */
  id: ExampleCategoryId;
  /** i18n key under examples.categories.<id> */
  labelKey: string;
  /** i18n key under examples.categoriesDesc.<id> */
  descKey: string;
  icon: LucideIcon;
  /** Tailwind classes for the category accent (text + ring + bg) */
  accent: string;
  /** Order to display categories in the list */
  order: number;
}

/**
 * Static metadata for example categories. The example *registry* itself
 * (titles / ids / descriptions) lives in the backend `examples` handler and
 * is fetched at runtime — only the chrome (icon, accent color, ordering)
 * lives here so the UI stays rich without round-tripping strings.
 */
export const EXAMPLE_CATEGORIES: ExampleCategory[] = [
  {
    id: 'basics',
    labelKey: 'categories.basics',
    descKey: 'categoriesDesc.basics',
    icon: Beaker,
    accent: 'from-cube-emerald/25 to-cube-emerald/5 text-cube-emerald ring-cube-emerald/30',
    order: 1,
  },
  {
    id: 'filesystem',
    labelKey: 'categories.filesystem',
    descKey: 'categoriesDesc.filesystem',
    icon: FileText,
    accent: 'from-cube-cyan/25 to-cube-cyan/5 text-cube-cyan ring-cube-cyan/30',
    order: 2,
  },
  {
    id: 'lifecycle',
    labelKey: 'categories.lifecycle',
    descKey: 'categoriesDesc.lifecycle',
    icon: Timer,
    accent: 'from-cube-amber/25 to-cube-amber/5 text-cube-amber ring-cube-amber/30',
    order: 3,
  },
  {
    id: 'network',
    labelKey: 'categories.network',
    descKey: 'categoriesDesc.network',
    icon: Network,
    accent: 'from-cube-violet/25 to-cube-violet/5 text-cube-violet ring-cube-violet/30',
    order: 4,
  },
];

export function findCategory(id: string | undefined): ExampleCategory | undefined {
  if (!id) return undefined;
  return EXAMPLE_CATEGORIES.find((c) => c.id === id);
}
