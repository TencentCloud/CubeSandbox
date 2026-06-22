// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

export interface StoreTemplate {
  id: string;
  /** i18n key under store.items.<id>.name */
  nameKey: string;
  /** i18n key under store.items.<id>.description */
  descriptionKey: string;
  image: string;
  image_cn: string;
  image_intl: string;
  digest?: string;
  /** stable tag keys, resolved via store.tagLabels.<key> */
  tags: string[];
  category: 'code' | 'browser' | 'ai' | 'base';
  size_mb: number;
  expose_ports: number[];
  probe_port: number;
  probe_path: string;
  writable_layer_size: string;
  official: boolean;
}

export const STORE_TEMPLATES: StoreTemplate[] = [
  {
    id: 'sandbox-code',
    nameKey: 'items.sandbox-code.name',
    descriptionKey: 'items.sandbox-code.description',
    image_cn: 'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest',
    image_intl: 'cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest',
    image: 'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest',
    digest: 'sha256:a7b8654aac5b90e241b98e195ae1d8c85d59fe1fb8c282bcccf1071f877db20f',
    tags: ['python', 'jupyter', 'official'],
    category: 'code',
    size_mb: 207,
    expose_ports: [49983, 49999],
    probe_port: 49999,
    probe_path: '/',
    writable_layer_size: '1G',
    official: true,
  },
  {
    id: 'sandbox-browser',
    nameKey: 'items.sandbox-browser.name',
    descriptionKey: 'items.sandbox-browser.description',
    image_cn: 'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest',
    image_intl: 'cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest',
    image: 'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest',
    digest: 'sha256:1786786af8510c34eda64ebec5b0a61a98583cb311c3045c0222910ec0680d60',
    tags: ['browser', 'chromium', 'official'],
    category: 'browser',
    size_mb: 1530,
    expose_ports: [49983],
    probe_port: 49983,
    probe_path: '/health',
    writable_layer_size: '1G',
    official: true,
  },
  {
    // 轻量版龙虾助手镜像。
    id: 'openclaw-lite',
    nameKey: 'items.openclaw-lite.name',
    descriptionKey: 'items.openclaw-lite.description',
    image_cn: 'cube-sandbox-image.tencentcloudcr.com/demo/lightweight-openclaw-deepseek-wecom:latest',
    image_intl: 'cube-sandbox-image.tencentcloudcr.com/demo/lightweight-openclaw-deepseek-wecom:latest',
    image: 'cube-sandbox-image.tencentcloudcr.com/demo/lightweight-openclaw-deepseek-wecom:latest',
    tags: ['agent', 'openclaw', 'lite', 'deepseek'],
    category: 'ai',
    size_mb: 2600,
    expose_ports: [49983, 18789, 49999],
    probe_port: 49983,
    probe_path: '/health',
    writable_layer_size: '2G',
    official: true,
  },
  {
    // all-in-one 龙虾助手：内置浏览器等完整工具链的全功能镜像。
    id: 'openclaw-aio',
    nameKey: 'items.openclaw-aio.name',
    descriptionKey: 'items.openclaw-aio.description',
    image_cn: 'cube-sandbox-image.tencentcloudcr.com/demo/aio-sandbox-envd-openclaw:latest',
    image_intl: 'cube-sandbox-image.tencentcloudcr.com/demo/aio-sandbox-envd-openclaw:latest',
    image: 'cube-sandbox-image.tencentcloudcr.com/demo/aio-sandbox-envd-openclaw:latest',
    digest: 'sha256:47680d7bc13ea7c57aeb88dff59ef2c44b0facb508e8c9066d479d7d458e0a66',
    tags: ['agent', 'openclaw', 'aio', 'browser', 'deepseek'],
    category: 'ai',
    size_mb: 6350,
    expose_ports: [49983, 18789, 8080],
    probe_port: 49983,
    probe_path: '/health',
    writable_layer_size: '4G',
    official: true,
  },
  {
    id: 'cubesandbox-base',
    nameKey: 'items.cubesandbox-base.name',
    descriptionKey: 'items.cubesandbox-base.description',
    image_cn: 'ghcr.io/tencentcloud/cubesandbox-base:latest',
    image_intl: 'ghcr.io/tencentcloud/cubesandbox-base:latest',
    image: 'ghcr.io/tencentcloud/cubesandbox-base:latest',
    tags: ['base', 'envd', 'official'],
    category: 'base',
    size_mb: 98,
    expose_ports: [49983],
    probe_port: 49983,
    probe_path: '/health',
    writable_layer_size: '1G',
    official: true,
  },
];

export const CATEGORIES = [
  { id: 'all', label: '全部' },
  { id: 'code', label: '代码执行' },
  { id: 'browser', label: '浏览器' },
  { id: 'ai', label: 'AI · LLM' },
  { id: 'base', label: '基础镜像' },
] as const;

export type CategoryId = (typeof CATEGORIES)[number]['id'];
