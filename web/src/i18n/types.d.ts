// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import type { resources } from './resources';

declare module 'i18next' {
  interface CustomTypeOptions {
    defaultNS: 'common';
    resources: typeof resources['en'];
  }
}
