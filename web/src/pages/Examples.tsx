// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState, useEffect, useMemo, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { useQuery, useMutation } from '@tanstack/react-query';
import { api } from '@/lib/api';
import { templateApi, clusterApi, type TemplateSummary } from '@/api/client';
import { Card } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import { CodeBlock } from '@/components/CodeBlock';
import {
  Play,
  Terminal,
  CheckCircle2,
  XCircle,
  Clock,
  ChevronRight,
  Sparkles,
  Rocket,
  FolderOpen,
  PauseCircle,
  Globe2,
  Search,
  Copy,
  Cpu,
  Layers,
  ChevronDown,
  Check,
  Inbox,
  FileCode2,
  Timer,
} from 'lucide-react';
import { cn, copyToClipboard } from '@/lib/utils';

// ── Types ────────────────────────────────────────────────────────────────────

interface ExampleMeta {
  id: string;
  filename: string;
  title: string;
  description: string;
  category: string;
}

interface RunExampleResponse {
  stdout: string;
  stderr: string;
  exit_code: number;
  success: boolean;
}

// ── API ──────────────────────────────────────────────────────────────────────

const examplesApi = {
  list: () => api<ExampleMeta[]>('/examples'),
  getSource: (id: string) =>
    api<{ id: string; filename: string; source: string }>(`/examples/${id}`),
  run: (id: string, templateId?: string) =>
    api<RunExampleResponse>('/examples/run', {
      method: 'POST',
      body: JSON.stringify({ id, template_id: templateId || undefined }),
    }),
};

// ── Category helpers ──────────────────────────────────────────────────────────

const CATEGORY_META: Record<
  string,
  { label: string; icon: typeof Rocket; tone: 'info' | 'ok' | 'warn' | 'mute'; gradient: string }
> = {
  basics: {
    label: 'Basics',
    icon: Rocket,
    tone: 'info',
    gradient: 'from-primary/20 via-primary/5 to-transparent',
  },
  filesystem: {
    label: 'Filesystem',
    icon: FolderOpen,
    tone: 'ok',
    gradient: 'from-cube-emerald/20 via-cube-emerald/5 to-transparent',
  },
  lifecycle: {
    label: 'Lifecycle',
    icon: PauseCircle,
    tone: 'warn',
    gradient: 'from-cube-amber/20 via-cube-amber/5 to-transparent',
  },
  network: {
    label: 'Network',
    icon: Globe2,
    tone: 'mute',
    gradient: 'from-cube-violet/20 via-cube-violet/5 to-transparent',
  },
};

const CATEGORY_ORDER = ['basics', 'filesystem', 'lifecycle', 'network'];

// ── RunOutput ────────────────────────────────────────────────────────────────

function RunOutput({ result, isRunning }: { result: RunExampleResponse | null; isRunning: boolean }) {
  if (isRunning) {
    return (
      <div className="flex items-center gap-3 rounded-lg border border-primary/20 bg-primary/5 px-4 py-3 text-sm text-foreground">
        <span className="relative flex h-2.5 w-2.5">
          <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-primary/60" />
          <span className="relative inline-flex h-2.5 w-2.5 rounded-full bg-primary" />
        </span>
        <span className="text-muted-foreground">{`Running example… this may take a few seconds.`}</span>
      </div>
    );
  }
  if (!result) return null;

  return (
    <div className="space-y-2.5">
      <div className="flex items-center gap-2 text-xs">
        {result.success ? (
          <span className="inline-flex items-center gap-1.5 rounded-full bg-cube-emerald/10 px-2.5 py-1 font-medium text-cube-emerald ring-1 ring-cube-emerald/30">
            <CheckCircle2 size={12} />
            Exited 0
          </span>
        ) : (
          <span className="inline-flex items-center gap-1.5 rounded-full bg-destructive/10 px-2.5 py-1 font-medium text-destructive ring-1 ring-destructive/30">
            <XCircle size={12} />
            {`Exited ${result.exit_code}`}
          </span>
        )}
        <span className="text-muted-foreground/70">
          {result.stdout ? `${result.stdout.split('\n').filter(Boolean).length} lines` : 'no output'}
        </span>
      </div>

      {result.stdout && (
        <div className="overflow-hidden rounded-lg border border-border/60 bg-muted/30">
          <pre className="max-h-72 overflow-auto p-4 font-mono text-[12.5px] leading-relaxed text-foreground/90 whitespace-pre-wrap">
            {result.stdout}
          </pre>
        </div>
      )}
      {result.stderr && (
        <div className="overflow-hidden rounded-lg border border-destructive/30 bg-destructive/5">
          <pre className="max-h-48 overflow-auto p-4 font-mono text-[12.5px] leading-relaxed text-destructive whitespace-pre-wrap">
            {result.stderr}
          </pre>
        </div>
      )}
    </div>
  );
}

// ── Template dropdown ────────────────────────────────────────────────────────

interface TemplateDropdownProps {
  templates: TemplateSummary[];
  defaultTemplateId?: string;
  value: string | undefined;
  onChange: (id: string | undefined) => void;
}

function TemplateDropdown({ templates, defaultTemplateId, value, onChange }: TemplateDropdownProps) {
  const { t } = useTranslation('examples');
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState('');
  const ref = useRef<HTMLDivElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!open) return;
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', handler);
    return () => document.removeEventListener('mousedown', handler);
  }, [open]);

  // Auto-focus search when opened
  useEffect(() => {
    if (open) {
      // small delay so the input is mounted
      const t = setTimeout(() => searchRef.current?.focus(), 30);
      return () => clearTimeout(t);
    } else {
      setFilter('');
    }
  }, [open]);

  const isDefault = value === defaultTemplateId;

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return templates;
    return templates.filter(
      (t) =>
        t.templateID.toLowerCase().includes(q) ||
        (t.instanceType ?? '').toLowerCase().includes(q) ||
        (t.status ?? '').toLowerCase().includes(q),
    );
  }, [templates, filter]);

  // Group by status: ready → building → others
  const grouped = useMemo(() => {
    const ready: TemplateSummary[] = [];
    const building: TemplateSummary[] = [];
    const other: TemplateSummary[] = [];
    for (const t of filtered) {
      const s = t.status.toLowerCase();
      if (s === 'ready') ready.push(t);
      else if (s === 'building' || s === 'pending') building.push(t);
      else other.push(t);
    }
    return { ready, building, other };
  }, [filtered]);

  const totalShown = filtered.length;
  const totalAll = templates.length;

  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className={cn(
          'group inline-flex h-8 items-center gap-2 rounded-md border bg-background/80 pl-2 pr-2 text-xs transition-all',
          'hover:border-primary/40 hover:bg-background',
          'focus:outline-none focus:ring-2 focus:ring-primary/30',
          open
            ? 'border-primary/50 bg-background ring-2 ring-primary/20'
            : 'border-border/60',
        )}
        title={value ?? ''}
      >
        <span
          className={cn(
            'flex h-5 w-5 shrink-0 items-center justify-center rounded transition-colors',
            isDefault
              ? 'bg-gradient-to-br from-primary to-cube-violet text-primary-foreground shadow-sm shadow-primary/30'
              : 'bg-muted text-muted-foreground group-hover:bg-primary/10 group-hover:text-primary',
          )}
        >
          <Cpu size={11} />
        </span>
        <span className="flex min-w-0 items-center gap-1.5">
          <span className="max-w-[180px] truncate font-mono text-foreground/90">{value ?? 'Select template'}</span>
          {isDefault && (
            <span className="inline-flex items-center gap-0.5 rounded-full bg-primary/15 px-1.5 py-px text-[9px] font-semibold uppercase tracking-wider text-primary ring-1 ring-primary/20">
              <Sparkles size={8} />
              default
            </span>
          )}
        </span>
        <ChevronDown
          size={12}
          className={cn('shrink-0 text-muted-foreground transition-transform duration-200', open && 'rotate-180 text-primary')}
        />
      </button>

      {open && (
        <div
          className={cn(
            'absolute right-0 top-9 z-30 w-80 overflow-hidden rounded-xl border border-border/80 bg-popover/95 shadow-2xl backdrop-blur-xl',
            'animate-fade-in',
          )}
        >
          {/* Header */}
          <div className="flex items-center justify-between border-b border-border/60 bg-muted/30 px-3 py-2">
            <div className="flex items-center gap-1.5">
              <span className="flex h-5 w-5 items-center justify-center rounded bg-primary/10 text-primary">
                <Layers size={11} />
              </span>
              <p className="text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">
                {t('templateSelector.title')}
              </p>
            </div>
            <span className="font-mono text-[10px] text-muted-foreground/70">
              {totalShown}/{totalAll}
            </span>
          </div>

          {/* Search */}
          {templates.length > 4 && (
            <div className="border-b border-border/60 px-2.5 py-2">
              <div className="relative">
                <Search
                  className="pointer-events-none absolute left-2 top-1/2 -translate-y-1/2 text-muted-foreground"
                  size={11}
                />
                <input
                  ref={searchRef}
                  value={filter}
                  onChange={(e) => setFilter(e.target.value)}
                  placeholder={t('templateSelector.searchPlaceholder')}
                  className={cn(
                    'h-7 w-full rounded-md border border-border/60 bg-background pl-7 pr-2 text-xs',
                    'placeholder:text-muted-foreground/60',
                    'focus:outline-none focus:ring-1 focus:ring-primary/40',
                  )}
                />
              </div>
            </div>
          )}

          {/* List */}
          <div className="max-h-72 overflow-y-auto py-1">
            {filtered.length === 0 ? (
              <div className="flex flex-col items-center gap-1.5 px-3 py-6 text-center">
                <Inbox size={16} className="text-muted-foreground/50" />
                <p className="text-xs text-muted-foreground">{t('templateSelector.empty')}</p>
              </div>
            ) : (
              <>
                {(['ready', 'building', 'other'] as const).map((groupKey) => {
                  const items = grouped[groupKey];
                  if (!items.length) return null;
                  return (
                    <div key={groupKey} className="space-y-0.5">
                      <p className="flex items-center gap-1.5 px-3 pt-2 pb-1 text-[9px] font-semibold uppercase tracking-wider text-muted-foreground/70">
                        <span
                          className={cn(
                            'h-1.5 w-1.5 rounded-full',
                            groupKey === 'ready' && 'bg-cube-emerald',
                            groupKey === 'building' && 'bg-cube-amber',
                            groupKey === 'other' && 'bg-muted-foreground/40',
                          )}
                        />
                        {t(`templateSelector.group.${groupKey}`)}
                        <span className="font-mono text-muted-foreground/50">· {items.length}</span>
                      </p>
                      {items.map((tpl) => {
                        const isSelected = tpl.templateID === value;
                        const statusLower = tpl.status.toLowerCase();
                        return (
                          <button
                            key={tpl.templateID}
                            onClick={() => {
                              onChange(tpl.templateID);
                              setOpen(false);
                            }}
                            className={cn(
                              'group/item flex w-full items-center gap-2.5 rounded-md px-2.5 py-1.5 text-left text-xs transition-colors',
                              'hover:bg-muted/70',
                              isSelected && 'bg-primary/8 ring-1 ring-inset ring-primary/20',
                            )}
                          >
                            <span
                              className={cn(
                                'flex h-6 w-6 shrink-0 items-center justify-center rounded-md transition-colors',
                                isSelected
                                  ? 'bg-primary/15 text-primary'
                                  : 'bg-muted/60 text-muted-foreground group-hover/item:text-foreground',
                              )}
                            >
                              <FileCode2 size={11} />
                            </span>
                            <div className="min-w-0 flex-1">
                              <div className="flex items-center gap-1.5">
                                <span
                                  className={cn(
                                    'truncate font-mono',
                                    isSelected ? 'font-semibold text-foreground' : 'text-foreground/85',
                                  )}
                                >
                                  {tpl.templateID}
                                </span>
                                {tpl.templateID === defaultTemplateId && (
                                  <span className="inline-flex items-center gap-0.5 rounded bg-primary/15 px-1 text-[9px] font-medium text-primary">
                                    <Sparkles size={7} />
                                    default
                                  </span>
                                )}
                              </div>
                              {tpl.instanceType && (
                                <p className="mt-0.5 truncate text-[10px] text-muted-foreground/80">
                                  {tpl.instanceType}
                                  {tpl.version ? ` · ${tpl.version}` : ''}
                                </p>
                              )}
                            </div>
                            <Badge
                              tone={statusLower === 'ready' ? 'ok' : statusLower === 'building' ? 'warn' : 'mute'}
                              className="shrink-0 text-[10px]"
                            >
                              {tpl.status}
                            </Badge>
                            {isSelected && <Check size={12} className="shrink-0 text-primary" />}
                          </button>
                        );
                      })}
                    </div>
                  );
                })}
              </>
            )}
          </div>

          {/* Footer hint */}
          <div className="border-t border-border/60 bg-muted/20 px-3 py-1.5 text-[10px] text-muted-foreground/70">
            {t('templateSelector.hint')}
          </div>
        </div>
      )}
    </div>
  );
}

// ── ExampleCard ──────────────────────────────────────────────────────────────

interface ExampleCardProps {
  example: ExampleMeta;
  selected: boolean;
  onSelect: () => void;
}

function ExampleCard({ example, selected, onSelect }: ExampleCardProps) {
  const meta = CATEGORY_META[example.category] ?? CATEGORY_META.basics;
  const Icon = meta.icon;

  return (
    <button
      onClick={onSelect}
      className={cn(
        'group relative w-full overflow-hidden rounded-lg border p-3 text-left transition-all duration-200',
        'hover:border-primary/40 hover:bg-muted/40',
        selected
          ? 'border-primary/50 bg-primary/5 shadow-sm ring-1 ring-primary/20'
          : 'border-border/60 bg-card/40',
      )}
    >
      {selected && (
        <span className="pointer-events-none absolute inset-y-0 left-0 w-0.5 bg-gradient-to-b from-primary to-cube-violet" />
      )}
      <div className="flex items-start gap-3">
        <span
          className={cn(
            'flex h-9 w-9 shrink-0 items-center justify-center rounded-lg ring-1 transition-all',
            'bg-gradient-to-br',
            meta.gradient,
            selected ? 'ring-primary/30' : 'ring-border/60 group-hover:ring-primary/20',
          )}
        >
          <Icon
            size={16}
            className={cn(
              'transition-colors',
              selected ? 'text-primary' : 'text-foreground/80 group-hover:text-primary',
            )}
          />
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-1.5">
            <p className="truncate text-sm font-medium text-foreground">{example.title}</p>
            {selected && <ChevronRight size={12} className="shrink-0 text-primary" />}
          </div>
          <p className="mt-0.5 text-xs leading-snug text-muted-foreground line-clamp-2">
            {example.description}
          </p>
          <div className="mt-2 flex items-center gap-1.5">
            <span className="inline-flex items-center gap-1 rounded bg-muted/60 px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
              <FileCode2 size={9} />
              {example.filename}
            </span>
          </div>
        </div>
      </div>
    </button>
  );
}

// ── Empty state for output panel ─────────────────────────────────────────────

function OutputEmpty({ onRun }: { onRun?: () => void }) {
  const { t } = useTranslation('examples');
  return (
    <div className="flex flex-col items-center justify-center gap-2 py-10 text-center">
      <span className="flex h-10 w-10 items-center justify-center rounded-full bg-muted/50 text-muted-foreground">
        <Inbox size={18} />
      </span>
      <p className="text-sm text-muted-foreground">{t('outputHint')}</p>
      {onRun && (
        <Button size="sm" variant="outline" onClick={onRun} className="mt-1">
          <Play size={12} />
          {t('run')}
        </Button>
      )}
    </div>
  );
}

// ── Main page ────────────────────────────────────────────────────────────────

export default function ExamplesPage() {
  const { t } = useTranslation('examples');
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [runResult, setRunResult] = useState<RunExampleResponse | null>(null);
  const [selectedTemplateId, setSelectedTemplateId] = useState<string | undefined>(undefined);
  const [activeCategory, setActiveCategory] = useState<string>('all');
  const [search, setSearch] = useState('');

  const { data: examples, isLoading } = useQuery({
    queryKey: ['examples'],
    queryFn: examplesApi.list,
  });

  const { data: templates } = useQuery({
    queryKey: ['templates'],
    queryFn: () => templateApi.list(),
  });

  const { data: config } = useQuery({
    queryKey: ['config'],
    queryFn: () => clusterApi.config(),
  });

  const defaultTemplateId = config?.defaultTemplateId;
  const firstTemplateId = templates?.[0]?.templateID;
  const effectiveTemplateId = selectedTemplateId ?? defaultTemplateId ?? firstTemplateId;

  useEffect(() => {
    if (effectiveTemplateId && selectedTemplateId === undefined) {
      setSelectedTemplateId(effectiveTemplateId);
    }
  }, [effectiveTemplateId, selectedTemplateId]);

  const runMutation = useMutation({
    mutationFn: (id: string) => examplesApi.run(id, selectedTemplateId),
    onSuccess: (data) => setRunResult(data),
    onMutate: () => setRunResult(null),
  });

  const selected = examples?.find((e) => e.id === selectedId) ?? null;

  const { data: sourceData, isLoading: isSourceLoading } = useQuery({
    queryKey: ['examples', selectedId, 'source'],
    queryFn: () => examplesApi.getSource(selectedId!),
    enabled: !!selectedId,
  });
  const sourceCode = sourceData?.source ?? '';

  // Group + filter
  const filteredList = useMemo(() => {
    if (!examples) return [];
    const q = search.trim().toLowerCase();
    return examples.filter((e) => {
      if (activeCategory !== 'all' && e.category !== activeCategory) return false;
      if (q) {
        return (
          e.title.toLowerCase().includes(q) ||
          e.description.toLowerCase().includes(q) ||
          e.filename.toLowerCase().includes(q)
        );
      }
      return true;
    });
  }, [examples, activeCategory, search]);

  const grouped = useMemo(() => {
    const out: Record<string, ExampleMeta[]> = {};
    for (const e of filteredList) {
      (out[e.category] ??= []).push(e);
    }
    return out;
  }, [filteredList]);

  // Stats for header
  const totalCount = examples?.length ?? 0;
  const categoryCount = useMemo(() => {
    if (!examples) return 0;
    return new Set(examples.map((e) => e.category)).size;
  }, [examples]);

  const handleCopySource = () => {
    if (!sourceCode) return;
    // copyToClipboard 内置 fallback：HTTPS 下用 navigator.clipboard，
    // HTTP（无 Secure Context）下回退到 execCommand('copy')，不再抛异常。
    copyToClipboard(sourceCode, t('copied'));
  };

  const runSelected = () => {
    if (selected) runMutation.mutate(selected.id);
  };

  return (
    <div className="animate-fade-in space-y-5">
      {/* Hero header */}
      <header className="relative overflow-hidden rounded-2xl border border-border/60 bg-gradient-to-br from-card/80 via-card/60 to-card/40 p-6">
        <div className="pointer-events-none absolute -right-20 -top-20 h-64 w-64 rounded-full bg-primary/5 blur-3xl" />
        <div className="pointer-events-none absolute -bottom-12 -left-12 h-48 w-48 rounded-full bg-cube-violet/5 blur-3xl" />
        <div className="relative flex flex-col gap-4 md:flex-row md:items-end md:justify-between">
          <div className="space-y-1.5">
            <div className="flex items-center gap-2">
              <span className="inline-flex h-7 w-7 items-center justify-center rounded-md bg-gradient-to-br from-primary to-cube-violet text-primary-foreground shadow-sm shadow-primary/30">
                <Sparkles size={14} />
              </span>
              <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
              <Badge tone="info" className="text-[10px]">{t('badge')}</Badge>
            </div>
            <p className="max-w-2xl text-sm text-muted-foreground">{t('subtitle')}</p>
          </div>
          <div className="flex items-center gap-2 text-xs">
            <div className="rounded-lg border border-border/60 bg-card/60 px-3 py-1.5">
              <span className="text-muted-foreground">examples · </span>
              <span className="font-mono font-semibold text-foreground">{totalCount}</span>
            </div>
            <div className="rounded-lg border border-border/60 bg-card/60 px-3 py-1.5">
              <span className="text-muted-foreground">categories · </span>
              <span className="font-mono font-semibold text-foreground">{categoryCount}</span>
            </div>
          </div>
        </div>
      </header>

      {/* Toolbar: search + category filter */}
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="relative w-full sm:w-72">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-muted-foreground" />
          <input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder={t('searchPlaceholder')}
            className={cn(
              'h-9 w-full rounded-lg border border-border/60 bg-background pl-8 pr-3 text-sm',
              'placeholder:text-muted-foreground/70',
              'focus:outline-none focus:ring-1 focus:ring-primary/40',
            )}
          />
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          <button
            onClick={() => setActiveCategory('all')}
            className={cn(
              'rounded-full px-3 py-1 text-xs font-medium transition-colors',
              activeCategory === 'all'
                ? 'bg-primary text-primary-foreground shadow-sm shadow-primary/20'
                : 'bg-muted/40 text-muted-foreground hover:bg-muted/70 hover:text-foreground',
            )}
          >
            {t('allCategories')}
          </button>
          {CATEGORY_ORDER.filter((c) => CATEGORY_META[c]).map((cat) => {
            const meta = CATEGORY_META[cat];
            const Icon = meta.icon;
            return (
              <button
                key={cat}
                onClick={() => setActiveCategory(cat)}
                className={cn(
                  'inline-flex items-center gap-1.5 rounded-full px-3 py-1 text-xs font-medium transition-colors',
                  activeCategory === cat
                    ? 'bg-primary text-primary-foreground shadow-sm shadow-primary/20'
                    : 'bg-muted/40 text-muted-foreground hover:bg-muted/70 hover:text-foreground',
                )}
              >
                <Icon size={11} />
                {meta.label}
              </button>
            );
          })}
        </div>
      </div>

      {/* Main two-column layout */}
      <div className="grid grid-cols-1 gap-5 lg:grid-cols-[320px_1fr]">
        {/* Left: example list */}
        <div className="space-y-5">
          {isLoading ? (
            <div className="space-y-2">
              {Array.from({ length: 5 }).map((_, i) => (
                <Skeleton key={i} className="h-[88px] w-full rounded-lg" />
              ))}
            </div>
          ) : filteredList.length === 0 ? (
            <div className="flex flex-col items-center justify-center gap-2 rounded-xl border border-dashed border-border/60 py-12 text-center">
              <Search className="h-6 w-6 text-muted-foreground/50" />
              <p className="text-sm text-muted-foreground">{t('noResults')}</p>
            </div>
          ) : (
            <div className="space-y-5">
              {CATEGORY_ORDER.filter((c) => grouped[c]?.length).map((cat) => {
                const meta = CATEGORY_META[cat];
                const items = grouped[cat];
                if (!items?.length) return null;
                return (
                  <div key={cat} className="space-y-2">
                    <div className="flex items-center gap-1.5 px-1">
                      <meta.icon size={11} className="text-muted-foreground" />
                      <p className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground/80">
                        {meta.label}
                      </p>
                      <span className="text-[10px] text-muted-foreground/50">· {items.length}</span>
                    </div>
                    <div className="space-y-1.5">
                      {items.map((ex) => (
                        <ExampleCard
                          key={ex.id}
                          example={ex}
                          selected={selectedId === ex.id}
                          onSelect={() => {
                            setSelectedId(ex.id);
                            setRunResult(null);
                          }}
                        />
                      ))}
                    </div>
                  </div>
                );
              })}
            </div>
          )}
        </div>

        {/* Right: code + output */}
        <div className="space-y-4 min-w-0">
          {selected ? (
            <>
              {/* Code panel */}
              <Card className="overflow-hidden p-0">
                <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border/60 bg-muted/20 px-4 py-2.5">
                  <div className="flex min-w-0 items-center gap-2">
                    <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-md bg-primary/10 text-primary">
                      <FileCode2 size={13} />
                    </span>
                    <span className="truncate text-sm font-medium text-foreground">{selected.title}</span>
                    <span className="hidden font-mono text-[11px] text-muted-foreground sm:inline">· {selected.filename}</span>
                  </div>
                  <div className="flex items-center gap-2">
                    <TemplateDropdown
                      templates={templates ?? []}
                      defaultTemplateId={defaultTemplateId}
                      value={effectiveTemplateId}
                      onChange={setSelectedTemplateId}
                    />
                    <Button
                      variant="ghost"
                      size="icon"
                      onClick={handleCopySource}
                      disabled={!sourceCode}
                      className="h-7 w-7"
                      title={t('copy')}
                    >
                      <Copy size={13} />
                    </Button>
                    <Button
                      size="sm"
                      disabled={runMutation.isPending || !effectiveTemplateId}
                      onClick={runSelected}
                      className="gap-1.5"
                    >
                      {runMutation.isPending ? (
                        <>
                          <Clock size={13} className="animate-spin" />
                          {t('running')}
                        </>
                      ) : (
                        <>
                          <Play size={13} />
                          {t('run')}
                        </>
                      )}
                    </Button>
                  </div>
                </div>
                <div className="max-h-[460px] overflow-auto bg-[hsl(var(--background))]/40">
                  {isSourceLoading ? (
                    <div className="space-y-2 p-4">
                      {Array.from({ length: 8 }).map((_, i) => (
                        <Skeleton key={i} className="h-3 w-full" style={{ width: `${50 + Math.random() * 50}%` }} />
                      ))}
                    </div>
                  ) : sourceCode ? (
                    <CodeBlock code={sourceCode} language="python" />
                  ) : (
                    <pre className="p-4 font-mono text-xs text-muted-foreground">
                      # {selected.filename}
                    </pre>
                  )}
                </div>
              </Card>

              {/* Output panel */}
              <Card className="overflow-hidden p-0">
                <div className="flex items-center justify-between border-b border-border/60 bg-muted/20 px-4 py-2.5">
                  <div className="flex items-center gap-2">
                    <span className="flex h-6 w-6 items-center justify-center rounded-md bg-cube-emerald/10 text-cube-emerald">
                      <Terminal size={13} />
                    </span>
                    <span className="text-sm font-medium text-foreground">{t('output')}</span>
                    {runMutation.isPending && (
                      <span className="inline-flex items-center gap-1 rounded-full bg-primary/10 px-2 py-0.5 text-[10px] font-medium text-primary">
                        <Timer size={9} className="animate-pulse" />
                        {t('running')}
                      </span>
                    )}
                  </div>
                  {runResult && (
                    <span className="text-[10px] text-muted-foreground/70">
                      {runResult.success ? t('completed') : t('failed')}
                    </span>
                  )}
                </div>
                <div className="p-4">
                  {!runResult && !runMutation.isPending ? (
                    <OutputEmpty onRun={runSelected} />
                  ) : (
                    <RunOutput result={runResult} isRunning={runMutation.isPending} />
                  )}
                </div>
              </Card>
            </>
          ) : (
            <Card className="flex h-80 flex-col items-center justify-center gap-3 border-dashed bg-card/30 p-8 text-center">
              <span className="flex h-12 w-12 items-center justify-center rounded-full bg-gradient-to-br from-primary/15 to-cube-violet/15 text-primary">
                <Sparkles size={20} />
              </span>
              <div className="space-y-1">
                <p className="text-sm font-medium text-foreground">{t('selectHintTitle')}</p>
                <p className="text-xs text-muted-foreground">{t('selectHint')}</p>
              </div>
            </Card>
          )}
        </div>
      </div>
    </div>
  );
}
