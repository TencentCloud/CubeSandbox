// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useState } from 'react';

/**
 * A drop-in replacement for `useState` that persists the value to `localStorage`.
 * On mount the stored value is preferred over `defaultValue`; every subsequent
 * change is written back so it survives page navigation and browser restarts.
 */
export function usePersistedState<T>(
  key: string,
  defaultValue: T,
): [T, React.Dispatch<React.SetStateAction<T>>] {
  const [value, setValue] = useState<T>(() => {
    try {
      const stored = localStorage.getItem(key);
      if (stored !== null) return JSON.parse(stored) as T;
    } catch {
      // ignore corrupt / unavailable storage
    }
    return defaultValue;
  });

  useEffect(() => {
    try {
      localStorage.setItem(key, JSON.stringify(value));
    } catch {
      // quota exceeded or private browsing — silently degrade
    }
  }, [key, value]);

  return [value, setValue];
}
