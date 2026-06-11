// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { storeApi, type StoreCatalogItem } from '@/api/client';

// Re-export the API type as the primary store template type.
export type StoreTemplate = StoreCatalogItem;

export const CATEGORIES = [
  { id: 'all', label: '全部' },
  { id: 'code', label: '代码执行' },
  { id: 'browser', label: '浏览器' },
  { id: 'ai', label: 'AI · LLM' },
  { id: 'web', label: 'Web 服务' },
  { id: 'base', label: '基础镜像' },
] as const;

export type CategoryId = (typeof CATEGORIES)[number]['id'];
