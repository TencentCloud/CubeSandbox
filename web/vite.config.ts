// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import path from 'node:path';

import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/cubeapi': 'http://127.0.0.1:3000',
    },
  },
});
