// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import enCommon from '@/locales/en/common.json';
import enNav from '@/locales/en/nav.json';
import enTopbar from '@/locales/en/topbar.json';
import enCommand from '@/locales/en/command.json';
import enOverview from '@/locales/en/overview.json';
import enSandboxes from '@/locales/en/sandboxes.json';
import enSandboxDetail from '@/locales/en/sandboxDetail.json';
import enTemplates from '@/locales/en/templates.json';
import enNodes from '@/locales/en/nodes.json';
import enKeys from '@/locales/en/keys.json';
import enPlaceholder from '@/locales/en/placeholder.json';
import enTheme from '@/locales/en/theme.json';

import zhCommon from '@/locales/zh/common.json';
import zhNav from '@/locales/zh/nav.json';
import zhTopbar from '@/locales/zh/topbar.json';
import zhCommand from '@/locales/zh/command.json';
import zhOverview from '@/locales/zh/overview.json';
import zhSandboxes from '@/locales/zh/sandboxes.json';
import zhSandboxDetail from '@/locales/zh/sandboxDetail.json';
import zhTemplates from '@/locales/zh/templates.json';
import zhNodes from '@/locales/zh/nodes.json';
import zhKeys from '@/locales/zh/keys.json';
import zhPlaceholder from '@/locales/zh/placeholder.json';
import zhTheme from '@/locales/zh/theme.json';

export const resources = {
  en: {
    common: enCommon,
    nav: enNav,
    topbar: enTopbar,
    command: enCommand,
    overview: enOverview,
    sandboxes: enSandboxes,
    sandboxDetail: enSandboxDetail,
    templates: enTemplates,
    nodes: enNodes,
    keys: enKeys,
    placeholder: enPlaceholder,
    theme: enTheme,
  },
  zh: {
    common: zhCommon,
    nav: zhNav,
    topbar: zhTopbar,
    command: zhCommand,
    overview: zhOverview,
    sandboxes: zhSandboxes,
    sandboxDetail: zhSandboxDetail,
    templates: zhTemplates,
    nodes: zhNodes,
    keys: zhKeys,
    placeholder: zhPlaceholder,
    theme: zhTheme,
  },
} as const;

export type AppResources = typeof resources;
