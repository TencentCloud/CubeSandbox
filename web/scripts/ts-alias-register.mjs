// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Entry point for `node --test --import ./scripts/ts-alias-register.mjs`.
import { register } from 'node:module';

register('./ts-alias-loader.mjs', import.meta.url);
