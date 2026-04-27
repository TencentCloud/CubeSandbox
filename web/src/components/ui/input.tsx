// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import * as React from 'react';
import { cn } from '@/lib/utils';

export const Input = React.forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(
  ({ className, type, ...props }, ref) => (
    <input
      ref={ref}
      type={type}
      className={cn(
        'flex h-9 w-full rounded-md border border-input bg-background/50 px-3 py-1 text-sm shadow-sm transition-colors',
        'placeholder:text-muted-foreground file:border-0 file:bg-transparent file:text-sm file:font-medium',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-0',
        'disabled:cursor-not-allowed disabled:opacity-50',
        className
      )}
      {...props}
    />
  )
);
Input.displayName = 'Input';
