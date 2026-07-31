// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import '@testing-library/jest-dom/vitest';

class TestResizeObserver implements ResizeObserver {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

globalThis.ResizeObserver = TestResizeObserver;

Object.defineProperty(globalThis.navigator, 'clipboard', {
  configurable: true,
  value: {
    readText: async () => '',
    writeText: async () => undefined,
  },
});
