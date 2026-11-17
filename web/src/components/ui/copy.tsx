// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState } from 'react';
import { Check, Copy } from 'lucide-react';
import { cn, copyToClipboard } from '@/lib/utils';

// CopyButton is an icon-only copy control; labels are passed in so callers
// can supply their own localized strings.
export function CopyButton({
  text,
  className,
  copiedLabel,
  copyLabel,
}: {
  text: string;
  className?: string;
  copiedLabel: string;
  copyLabel: string;
}) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      onClick={() => {
        copyToClipboard(text, copiedLabel);
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      }}
      className={cn(
        'text-muted-foreground/50 hover:text-muted-foreground transition-colors',
        className,
      )}
      title={copyLabel}
    >
      {copied ? <Check className="h-3 w-3 text-cube-ok" /> : <Copy className="h-3 w-3" />}
    </button>
  );
}

// CopyableText is a click-to-copy text span with a hover copy affordance.
export function CopyableText({
  text,
  display,
  className,
  copiedLabel,
  clickToCopyLabel,
}: {
  text: string;
  display?: string;
  className?: string;
  copiedLabel: string;
  clickToCopyLabel: string;
}) {
  const [copied, setCopied] = useState(false);
  return (
    <span
      className={cn('inline-flex items-center gap-1.5 cursor-pointer group', className)}
      onClick={() => {
        copyToClipboard(text, copiedLabel);
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      }}
      title={copied ? copiedLabel : clickToCopyLabel}
    >
      <span>{display ?? text}</span>
      <span className="opacity-0 group-hover:opacity-100 transition-opacity">
        {copied ? (
          <Check className="h-3 w-3 text-cube-ok" />
        ) : (
          <Copy className="h-3 w-3 text-muted-foreground/50" />
        )}
      </span>
    </span>
  );
}
