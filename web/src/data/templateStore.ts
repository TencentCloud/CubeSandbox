// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

export interface StoreTemplate {
  id: string;
  name: string;
  description: string;
  image: string;
  image_cn: string;
  image_intl: string;
  digest?: string;
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
    name: '代码执行沙箱',
    description: '官方代码执行环境，预装 Python3 + Jupyter Kernel，兼容 E2B SDK',
    image_cn: 'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest',
    image_intl: 'cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest',
    image: 'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest',
    digest: 'sha256:a7b8654aac5b90e241b98e195ae1d8c85d59fe1fb8c282bcccf1071f877db20f',
    tags: ['Python', 'Jupyter', '官方'],
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
    name: '浏览器沙箱',
    description: '预装 Chromium，支持网页自动化',
    image_cn: 'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest',
    image_intl: 'cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest',
    image: 'cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-browser:latest',
    digest: 'sha256:1786786af8510c34eda64ebec5b0a61a98583cb311c3045c0222910ec0680d60',
    tags: ['浏览器', 'Chromium', '官方'],
    category: 'browser',
    size_mb: 1530,
    expose_ports: [49983],
    probe_port: 49983,
    probe_path: '/health',
    writable_layer_size: '1G',
    official: true,
  },
  {
    id: 'cubesandbox-base',
    name: '基础镜像',
    description: '最小化基础镜像，仅含 envd，适合自定义构建',
    image_cn: 'ghcr.io/tencentcloud/cubesandbox-base:latest',
    image_intl: 'ghcr.io/tencentcloud/cubesandbox-base:latest',
    image: 'ghcr.io/tencentcloud/cubesandbox-base:latest',
    tags: ['基础', 'envd', '官方'],
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
