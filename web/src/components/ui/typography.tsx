// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { CheckCircle2, XCircle } from 'lucide-react';
import { cn } from '@/lib/utils';

type Size = 'xs' | 'sm' | 'base';

const sizeMap: Record<Size, string> = {
  xs: 'text-xs',
  sm: 'text-sm',
  base: 'text-base',
};

/**
 * MonoId — machine identifier rendered in JetBrains Mono.
 * Use for templateID, sandboxID, nodeID, sha256 digest, image short name.
 * Never use this for human-readable strings (IP, domain, version, label).
 */
export function MonoId({
  children,
  className,
  size = 'sm',
  muted,
}: {
  children: React.ReactNode;
  className?: string;
  size?: Size;
  muted?: boolean;
}) {
  return (
    <span
      className={cn(
        'font-mono',
        sizeMap[size],
        muted ? 'text-muted-foreground' : 'text-foreground/90',
        className,
      )}
    >
      {children}
    </span>
  );
}

/**
 * MetricValue — numeric value with optional unit, rendered in sans-serif
 * with tabular figures so column widths stay aligned. Unit is muted to
 * preserve the value/unit hierarchy.
 */
export function MetricValue({
  value,
  unit,
  className,
  size = 'sm',
  emphasis = 'normal',
}: {
  value: React.ReactNode;
  unit?: React.ReactNode;
  className?: string;
  size?: Size;
  emphasis?: 'normal' | 'strong';
}) {
  return (
    <span className={cn('inline-flex items-baseline gap-1 text-num', sizeMap[size], className)}>
      <span className={emphasis === 'strong' ? 'font-semibold text-foreground' : 'text-foreground/90'}>
        {value}
      </span>
      {unit ? <span className="text-muted-foreground">{unit}</span> : null}
    </span>
  );
}

/**
 * Code — inline code / CLI fragment / image reference. Same monospace
 * stack as MonoId but tuned for content that may include separators.
 */
export function Code({
  children,
  className,
  size = 'xs',
}: {
  children: React.ReactNode;
  className?: string;
  size?: Size;
}) {
  return (
    <code
      className={cn(
        'font-mono text-muted-foreground break-all leading-relaxed',
        sizeMap[size],
        className,
      )}
    >
      {children}
    </code>
  );
}

/**
 * BoolBadge — compact yes/no indicator. Renders an em-dash placeholder
 * when value is null/undefined. Use for boolean runtime flags (auth,
 * internet access, readiness) where the absence of a value carries
 * different meaning from "false".
 */
export function BoolBadge({
  value,
  trueLabel,
  falseLabel,
}: {
  value?: boolean | null;
  trueLabel?: string;
  falseLabel?: string;
}) {
  if (value == null) return <span className="text-muted-foreground/50 text-xs">—</span>;
  return value ? (
    <span className="inline-flex items-center gap-1 text-cube-ok text-xs font-medium">
      <CheckCircle2 size={12} /> {trueLabel ?? 'Yes'}
    </span>
  ) : (
    <span className="inline-flex items-center gap-1 text-cube-err text-xs font-medium">
      <XCircle size={12} /> {falseLabel ?? 'No'}
    </span>
  );
}
