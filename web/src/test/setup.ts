// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

// Vitest setup: registers jest-dom matchers (toBeInTheDocument etc.) on
// vitest's expect and stubs browser APIs jsdom does not implement.
import '@testing-library/jest-dom/vitest';

// jsdom has no ResizeObserver; components under test (xterm fit handling)
// construct one, so provide a no-op stub.
if (typeof globalThis.ResizeObserver === 'undefined') {
  class ResizeObserverStub {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  globalThis.ResizeObserver = ResizeObserverStub as unknown as typeof ResizeObserver;
}
