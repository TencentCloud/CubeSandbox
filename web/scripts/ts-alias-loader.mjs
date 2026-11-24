// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Resolve Vite-style import specifiers (the `@/` src alias, extensionless
// relative paths, directory imports) under `node --test`.
// Registered by ts-alias-register.mjs.

import { existsSync, statSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const SRC = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'src');

function withExtension(target) {
  if (existsSync(target) && statSync(target).isDirectory()) {
    const index = path.join(target, 'index');
    for (const ext of ['.ts', '.tsx']) {
      if (existsSync(index + ext)) return index + ext;
    }
  }
  for (const ext of ['', '.ts', '.tsx']) {
    if (existsSync(target + ext)) return target + ext;
  }
  return target;
}

export async function resolve(spec, context, next) {
  if (spec.startsWith('@/')) {
    return next(pathToFileURL(withExtension(path.join(SRC, spec.slice(2)))).href, context);
  }
  if ((spec.startsWith('./') || spec.startsWith('../')) && context.parentURL?.startsWith('file:')) {
    const base = path.dirname(fileURLToPath(context.parentURL));
    return next(pathToFileURL(withExtension(path.resolve(base, spec))).href, context);
  }
  return next(spec, context);
}
