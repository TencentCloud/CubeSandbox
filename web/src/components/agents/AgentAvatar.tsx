// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useMemo } from 'react';
import { cn } from '@/lib/utils';

interface AgentAvatarProps {
  seed: string;
  size?: number;
  className?: string;
  rounded?: boolean;
}

// 沉稳的企业级渐变配色，确保浅/深色背景下白字都清晰
const PALETTES: Array<[string, string]> = [
  ['#1e3a8a', '#3b82f6'], // blue
  ['#0f766e', '#14b8a6'], // teal
  ['#3730a3', '#6366f1'], // indigo
  ['#155e75', '#0891b2'], // cyan
  ['#7c2d12', '#ea580c'], // orange
  ['#581c87', '#a855f7'], // purple
  ['#9d174d', '#db2777'], // rose
  ['#334155', '#64748b'], // slate
];

function hashString(input: string): number {
  let hash = 0;
  for (let i = 0; i < input.length; i += 1) {
    hash = (hash << 5) - hash + input.charCodeAt(i);
    hash |= 0;
  }
  return Math.abs(hash);
}

function deriveMonogram(seed: string): string {
  const trimmed = (seed || '').trim();
  if (!trimmed) return '?';
  const chars = Array.from(trimmed);
  const first = chars[0];
  // 中日韩字符取单字（如「晓」「析」）
  if (/[\u3400-\u9fff\u3040-\u30ff\uac00-\ud7af]/.test(first)) {
    return first;
  }
  // 拉丁字符取前两个词的首字母
  const words = trimmed.split(/[\s·._-]+/).filter(Boolean);
  if (words.length >= 2) {
    return (words[0][0] + words[1][0]).toUpperCase();
  }
  return trimmed.slice(0, 2).toUpperCase();
}

export function AgentAvatar({
  seed,
  size = 64,
  className,
  rounded = true,
}: AgentAvatarProps) {
  const { monogram, gradient } = useMemo(() => {
    const key = seed || 'guest';
    const [from, to] = PALETTES[hashString(key) % PALETTES.length];
    return {
      monogram: deriveMonogram(key),
      gradient: `linear-gradient(135deg, ${from} 0%, ${to} 100%)`,
    };
  }, [seed]);

  return (
    <div
      role="img"
      aria-label={seed}
      style={{
        width: size,
        height: size,
        background: gradient,
        fontSize: Math.round(size * 0.42),
      }}
      className={cn(
        'flex select-none items-center justify-center font-semibold leading-none text-white shadow-sm ring-2 ring-white/40 dark:ring-white/10',
        rounded ? 'rounded-full' : 'rounded-lg',
        className
      )}
    >
      {monogram}
    </div>
  );
}
