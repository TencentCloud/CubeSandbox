// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Small wrapper so `npm run test` works from any cwd.
//
// `node --test` forks a child process per test file and resolves a relative
// `--import` specifier against a different base than the parent shell (Node 24
// resolves it against the directory it thinks the run started in, not `web/`).
// Passing an absolute path built at runtime removes that ambiguity.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const webRoot = path.dirname(here);
const register = path.join(here, 'ts-alias-register.mjs');

const args = [
  '--test',
  '--experimental-strip-types',
  '--import',
  register,
  'src/lib/*.test.ts',
  'src/mocks/*.test.ts',
];

const child = spawn(process.execPath, args, {
  cwd: webRoot,
  stdio: 'inherit',
  // Let the shell expand the globs above; Node itself does not.
  shell: true,
});

child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
