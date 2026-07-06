// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import * as React from 'react';
import { cn } from '@/lib/utils';

type Tone = 'default' | 'info' | 'ok' | 'warn' | 'err' | 'mute';

export function Badge({
  className,
  tone = 'default',
  ...props
}: React.HTMLAttributes<HTMLSpanElement> & { tone?: Tone }) {
  const toneClass =
    tone === 'ok'
      ? 'chip-ok'
      : tone === 'warn'
      ? 'chip-warn'
      : tone === 'err'
      ? 'chip-err'
      : tone === 'info'
      ? 'chip-info'
      : tone === 'mute'
      ? 'chip-mute'
      : 'chip bg-secondary text-secondary-foreground';
  return <span className={cn(toneClass, className)} {...props} />;
}
