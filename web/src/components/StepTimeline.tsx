// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.
//
// Horizontal step timeline used by the SandboxCases page. The component is
// intentionally dependency-free — it draws the timeline with plain divs so
// that we don't pull in an extra date-fns / d3 / framer-motion bundle for a
// widget that renders at most a dozen rows.

import { CheckCircle2, CircleDashed, OctagonX, TriangleAlert } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';

export type StepStatus = 'ok' | 'warn' | 'err' | 'skipped';

export interface StepLog {
  name: string;
  plane: 'control' | 'data' | string;
  status: StepStatus | string;
  duration_ms: number;
  message: string;
}

export interface StepTimelineProps {
  steps: StepLog[];
  /** Total wall-clock duration of the run; used to scale bar widths. */
  totalMs?: number;
  /** Optional override for the heading. Defaults to a translation key. */
  title?: string;
  className?: string;
}

function statusIcon(status: string) {
  if (status === 'ok') return CheckCircle2;
  if (status === 'warn') return TriangleAlert;
  if (status === 'err') return OctagonX;
  return CircleDashed;
}

function statusTone(status: string): { icon: string; bar: string; chip: string } {
  if (status === 'ok')
    return {
      icon: 'text-cube-emerald',
      bar: 'bg-gradient-to-r from-cube-emerald/80 to-cube-emerald/40',
      chip: 'chip-ok',
    };
  if (status === 'warn')
    return {
      icon: 'text-cube-amber',
      bar: 'bg-gradient-to-r from-cube-amber/80 to-cube-amber/40',
      chip: 'chip-warn',
    };
  if (status === 'err')
    return {
      icon: 'text-destructive',
      bar: 'bg-gradient-to-r from-destructive/80 to-destructive/40',
      chip: 'chip-err',
    };
  return {
    icon: 'text-muted-foreground',
    bar: 'bg-gradient-to-r from-muted-foreground/40 to-muted-foreground/10',
    chip: 'chip-mute',
  };
}

export function StepTimeline({ steps, totalMs, title, className }: StepTimelineProps) {
  const { t } = useTranslation('examples');
  const scale = (totalMs && totalMs > 0 ? totalMs : steps.reduce((s, x) => s + x.duration_ms, 0)) || 1;

  if (steps.length === 0) {
    return (
      <div
        className={cn(
          'rounded-lg border border-dashed border-border/60 bg-card/30 px-4 py-6 text-center text-xs text-muted-foreground',
          className,
        )}
      >
        {title && <p className="mb-1.5 text-sm font-medium text-foreground/80">{title}</p>}
        {t('timeline.empty')}
      </div>
    );
  }

  const controlSteps = steps.filter((s) => s.plane === 'control');
  const dataSteps = steps.filter((s) => s.plane === 'data');

  return (
    <div className={cn('space-y-3', className)}>
      {title && (
        <div className="flex items-center gap-2">
          <span className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground/80">
            {title}
          </span>
          <span className="text-[10px] text-muted-foreground/50">· {steps.length}</span>
        </div>
      )}
      <div className="space-y-2.5">
        {controlSteps.length > 0 && (
          <PlaneGroup
            label={t('timeline.controlPlane')}
            tone="cyan"
            steps={controlSteps}
            scale={scale}
          />
        )}
        {dataSteps.length > 0 && (
          <PlaneGroup
            label={t('timeline.dataPlane')}
            tone="violet"
            steps={dataSteps}
            scale={scale}
          />
        )}
      </div>
    </div>
  );
}

interface PlaneGroupProps {
  label: string;
  tone: 'cyan' | 'violet';
  steps: StepLog[];
  scale: number;
}

function PlaneGroup({ label, tone, steps, scale }: PlaneGroupProps) {
  return (
    <div className="space-y-1.5">
      <div className="flex items-center gap-1.5 px-0.5">
        <span
          className={cn(
            'h-1.5 w-1.5 rounded-full',
            tone === 'cyan' ? 'bg-cube-cyan' : 'bg-cube-violet',
          )}
        />
        <span className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground/70">
          {label}
        </span>
        <span className="text-[10px] text-muted-foreground/40">· {steps.length}</span>
      </div>
      <div className="space-y-1.5">
        {steps.map((s, idx) => {
          const Icon = statusIcon(s.status);
          const tone = statusTone(s.status);
          const widthPct = Math.max(2, Math.min(100, (s.duration_ms / scale) * 100));
          return (
            <div
              key={`${s.name}-${idx}`}
              className="group relative grid grid-cols-[16px_minmax(0,1fr)_minmax(0,2fr)_auto] items-center gap-2.5 rounded-md border border-border/40 bg-card/30 px-2.5 py-1.5 transition-colors hover:border-border/70 hover:bg-card/60"
              title={s.message}
            >
              <Icon size={13} className={cn('shrink-0', tone.icon)} />
              <div className="min-w-0">
                <p className="truncate text-xs font-medium text-foreground/85">{s.name}</p>
                <div className="mt-1 h-1 w-full overflow-hidden rounded-full bg-muted/40">
                  <div className={cn('h-full rounded-full', tone.bar)} style={{ width: `${widthPct}%` }} />
                </div>
              </div>
              <p className="hidden truncate text-[11px] text-muted-foreground/70 group-hover:text-muted-foreground sm:block">
                {s.message}
              </p>
              <div className="flex items-center gap-1.5">
                <span className={cn('chip text-[9px]', tone.chip)}>{s.status}</span>
                <span className="font-mono text-[10px] tabular-nums text-muted-foreground/70">
                  {s.duration_ms}ms
                </span>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}