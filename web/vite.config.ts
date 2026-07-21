// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { execSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import path from 'node:path';

import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

const pkg = JSON.parse(readFileSync(path.resolve(__dirname, './package.json'), 'utf-8'));

// Displayed app version, aligned with the release tag. Release builds inject the
// tag via CUBE_VERSION (release-one-click.yml passes github.ref_name); otherwise
// it derives from the nearest tag (`git describe --tags --abbrev=0`), falling
// back to package.json only when no tag is reachable (e.g. forks).
// A leading "v" is stripped so the UI can render a single canonical "v<x>".
function resolveAppVersion(): string {
  const fromEnv = process.env.CUBE_VERSION?.trim();
  if (fromEnv) return fromEnv.replace(/^v/, '');
  try {
    const described = execSync('git describe --tags --abbrev=0', {
      stdio: ['ignore', 'pipe', 'ignore'],
    })
      .toString()
      .trim();
    if (described) return described.replace(/^v/, '');
  } catch {
    // No reachable tag (e.g. a fork without tags) — fall back to package.json.
  }
  return pkg.version;
}

export default defineConfig({
  define: {
    __APP_VERSION__: JSON.stringify(resolveAppVersion()),
  },
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: 5173,
    proxy: {
      // CubeOps (ops/admin endpoints) — rewrite /opsapi → /api
      '/opsapi': {
        target: 'http://127.0.0.1:3010',
        rewrite: (path) => path.replace(/^\/opsapi/, '/api'),
      },
      // CubeAPI (SDK/E2B endpoints) — proxy specific API paths to avoid
      // conflicting with vite's own static file serving.
      '/sandboxes': 'http://127.0.0.1:3000',
      '/v2/sandboxes': 'http://127.0.0.1:3000',
      '/templates': 'http://127.0.0.1:3000',
      '/snapshots': 'http://127.0.0.1:3000',
      '/health': 'http://127.0.0.1:3000',
      // Legacy /cubeapi proxy for backward compat during transition
      '/cubeapi': 'http://127.0.0.1:3000',
    },
  },
});
