// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Entry point for `node --test --import ./scripts/ts-alias-register.mjs`.
//
// `--import` resolves a bare relative path against the process working
// directory, not against this file, and Node 24 no longer rewrites that
// relative path the way Node 22 did. Register with an absolute URL derived
// from this module's own location so the specifier works from any cwd and on
// any supported Node version.
import { register } from 'node:module';

register(new URL('./ts-alias-loader.mjs', import.meta.url));
