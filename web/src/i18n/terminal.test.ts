// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { describe, expect, it } from 'vitest';
import en from '@/locales/en/terminal.json';
import zh from '@/locales/zh/terminal.json';
import { resources } from './resources';
import { TERMINAL_REASON_CODES } from '@/lib/terminal/protocol';

describe('terminal i18n', () => {
  it('registers the terminal namespace for both languages with exact key parity', () => {
    expect(resources.en.terminal).toBe(en);
    expect(resources.zh.terminal).toBe(zh);
    expect(flatKeys(en)).toEqual(flatKeys(zh));
  });

  it('has stable translated text for every HTTP, status, and close reason', () => {
    for (const code of TERMINAL_REASON_CODES) {
      expect(en.errors[code]).toBeTruthy();
      expect(zh.errors[code]).toBeTruthy();
    }
    expect(en.errors.fallback).toBeTruthy();
    expect(zh.errors.fallback).toBeTruthy();
  });
});

function flatKeys(value: unknown, prefix = ''): string[] {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return [prefix];
  return Object.entries(value)
    .flatMap(([key, child]) => flatKeys(child, prefix ? `${prefix}.${key}` : key))
    .sort();
}
