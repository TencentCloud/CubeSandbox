// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.
//
// SandboxCases — the "examples/" dashboard. Replaces the old
// Examples.tsx with:
//   * a two-column scenario → case navigator on the left
//   * a Monaco editor + run bar + stdout/stderr on the right
//   * a topology graph (control + data plane) underneath
//   * a horizontal step timeline summarising the run
//
// The page reuses the existing TemplateDropdown (lifted out of the old
// page) so we don't have to redesign that piece. AI / LLM scenarios are
// not exposed (they're `hidden` on the Rust side and absent from the
// scenario registry).

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { usePersistedState } from '@/hooks/usePersistedState';
import { useTranslation } from 'react-i18next';
import { useMutation, useQuery } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import {
  Boxes,
  Check,
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  Clock,
  Cpu,
  Copy,
  FileCode2,
  FlaskConical,
  FolderOpen,
  Globe2,
  Inbox,
  Layers,
  Loader2,
  Monitor,
  Network,
  Play,
  RotateCcw,
  Search,
  Sparkles,
  Terminal,
  Timer,
  XCircle,
  AlertTriangle,
} from 'lucide-react';
import type { LucideIcon } from 'lucide-react';

import {
  clusterApi,
  examplesApi,
  storeApi,
  templateApi,
  type RunExampleBody,
  type TemplateSummary,
} from '@/api/client';
import { getTemplateMatchStatus, type TemplateMatchStatus } from '@/lib/template-match';
import { Card } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import { CodeEditor, type EditorLanguage } from '@/components/CodeEditor';
import { TopologyGraph } from '@/components/TopologyGraph';
import {
  EXAMPLE_CATEGORIES,
  EXAMPLE_SCENARIOS,
  type ExampleCategoryId,
  type ExampleScenario,
  type ScenarioFile,
} from '@/data/exampleScenarios';
import { cn, copyToClipboard } from '@/lib/utils';

// ─── i18n helpers ────────────────────────────────────────────────────────────

function categoryIcon(id: ExampleCategoryId): LucideIcon {
  const meta = EXAMPLE_CATEGORIES.find((c) => c.id === id);
  return meta?.icon ?? Boxes;
}

// Icon lookup for the left-rail scenario header. Falls back to FlaskConical
// so that adding a new scenario without an icon stays renderable.
function scenarioIcon(s: ExampleScenario): LucideIcon {
  return s.icon ?? FlaskConical;
}

function languageToEditor(lang: string): EditorLanguage {
  const l = (lang ?? '').toLowerCase();
  if (l === 'python') return 'python';
  if (l === 'go') return 'go';
  if (l === 'bash' || l === 'sh' || l === 'shell') return 'shell';
  if (l === 'javascript' || l === 'js') return 'javascript';
  if (l === 'typescript' || l === 'ts') return 'typescript';
  if (l === 'markdown' || l === 'md') return 'markdown';
  return 'python';
}

function languageLabel(lang: string, t: (k: string) => unknown): string {
  const l = (lang ?? '').toLowerCase();
  if (l === 'python') return String(t('languages.python'));
  if (l === 'go') return String(t('languages.go'));
  if (l === 'bash' || l === 'sh' || l === 'shell') return String(t('languages.bash'));
  if (l === 'javascript' || l === 'js') return String(t('languages.javascript'));
  return lang;
}

// ─── Template dropdown (lifted from old Examples.tsx) ───────────────────────

interface TemplateDropdownProps {
  templates: TemplateSummary[];
  defaultTemplateId?: string;
  value: string | undefined;
  onChange: (id: string | undefined) => void;
}

function TemplateDropdown({
  templates,
  defaultTemplateId,
  value,
  onChange,
}: TemplateDropdownProps) {
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

  useEffect(() => {
    if (open) {
      const id = setTimeout(() => searchRef.current?.focus(), 30);
      return () => clearTimeout(id);
    } else {
      setFilter('');
    }
  }, [open]);

  const isDefault = value === defaultTemplateId;

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return templates;
    return templates.filter(
      (tpl) =>
        tpl.templateID.toLowerCase().includes(q) ||
        (tpl.instanceType ?? '').toLowerCase().includes(q) ||
        (tpl.status ?? '').toLowerCase().includes(q),
    );
  }, [templates, filter]);

  const grouped = useMemo(() => {
    const ready: TemplateSummary[] = [];
    const building: TemplateSummary[] = [];
    const other: TemplateSummary[] = [];
    for (const tpl of filtered) {
      const s = tpl.status.toLowerCase();
      if (s === 'ready') ready.push(tpl);
      else if (s === 'building' || s === 'pending') building.push(tpl);
      else other.push(tpl);
    }
    return { ready, building, other };
  }, [filtered]);

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
          <span className="max-w-[180px] truncate font-mono text-foreground/90">{value ?? t('templateSelector.placeholder')}</span>
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
              {filtered.length}/{templates.length}
            </span>
          </div>

          {templates.length > 4 && (
            <div className="border-b border-border/60 px-2.5 py-2">
              <div className="relative">
                <Search className="pointer-events-none absolute left-2 top-1/2 -translate-y-1/2 text-muted-foreground" size={11} />
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
                            <Badge tone={statusLower === 'ready' ? 'ok' : statusLower === 'building' ? 'warn' : 'mute'} className="shrink-0 text-[10px]">
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

          <div className="border-t border-border/60 bg-muted/20 px-3 py-1.5 text-[10px] text-muted-foreground/70">
            {t('templateSelector.hint')}
          </div>
        </div>
      )}
    </div>
  );
}

// ─── Left rail: scenario + case list ─────────────────────────────────────────

interface CaseRowProps {
  scenario: ExampleScenario;
  file: ScenarioFile;
  selected: boolean;
  onSelect: () => void;
}

function CaseRow({ scenario, file, selected, onSelect }: CaseRowProps) {
  const { t, i18n } = useTranslation('examples');
  const isZh = (i18n.language ?? 'en').toLowerCase().startsWith('zh');
  const title = isZh ? file.titleZh : file.titleEn;
  const desc = isZh ? file.descriptionZh : file.descriptionEn;

  return (
    <button
      onClick={onSelect}
      className={cn(
        'group relative w-full overflow-hidden rounded-lg border p-2.5 text-left transition-all duration-200',
        'hover:border-primary/40 hover:bg-muted/40',
        selected
          ? 'border-primary/50 bg-primary/5 shadow-sm ring-1 ring-primary/20'
          : 'border-border/60 bg-card/40',
      )}
    >
      {selected && (
        <span className="pointer-events-none absolute inset-y-0 left-0 w-0.5 bg-gradient-to-b from-primary to-cube-violet" />
      )}
      <div className="flex items-start gap-2.5">
        <div className="min-w-0 flex-1">
          <p className="truncate text-[12.5px] font-medium text-foreground">{title}</p>
          <p className="mt-0.5 line-clamp-2 text-[11px] leading-snug text-muted-foreground">{desc}</p>
          <div className="mt-1.5 flex items-center gap-1">
            <span className="inline-flex items-center gap-1 rounded bg-muted/60 px-1.5 py-0.5 font-mono text-[9.5px] text-muted-foreground">
              <FileCode2 size={9} />
              {file.filename}
            </span>
            <span className="inline-flex items-center gap-1 rounded bg-muted/40 px-1.5 py-0.5 text-[9.5px] uppercase tracking-wider text-muted-foreground/70">
              {languageLabel(file.language, (k) => t(k as never) as unknown)}
            </span>
            <span className="inline-flex items-center gap-1 rounded bg-muted/30 px-1.5 py-0.5 text-[9.5px] text-muted-foreground/70">
              {scenario.id}
            </span>
          </div>
        </div>
        {selected && <ChevronRight size={11} className="mt-0.5 shrink-0 text-primary" />}
      </div>
    </button>
  );
}

interface ScenarioGroupProps {
  scenario: ExampleScenario;
  selectedFileId: string | null;
  onSelect: (file: ScenarioFile) => void;
}

function ScenarioGroup({ scenario, selectedFileId, onSelect }: ScenarioGroupProps) {
  const { t, i18n } = useTranslation('examples');
  const isZh = (i18n.language ?? 'en').toLowerCase().startsWith('zh');
  const Icon = scenarioIcon(scenario);
  const title = isZh ? scenario.titleZh : scenario.titleEn;

  // Auto-expand a group when it contains the selected case, otherwise show
  // it collapsed to keep the left rail scannable.
  const hasSelected = scenario.files.some((f) => `${scenario.id}:${f.id}` === selectedFileId);
  const [open, setOpen] = useState(hasSelected);
  useEffect(() => {
    if (hasSelected) setOpen(true);
  }, [hasSelected]);

  return (
    <div className="space-y-1.5">
      <button
        onClick={() => setOpen((v) => !v)}
        className={cn(
          'group flex w-full items-center gap-2 rounded-md px-1.5 py-1.5 text-left transition-colors',
          'hover:bg-muted/40',
        )}
      >
        <ChevronRight
          size={13}
          className={cn(
            'shrink-0 text-muted-foreground transition-transform duration-150',
            open && 'rotate-90 text-primary',
          )}
        />
        <span
          className={cn(
            'flex h-7 w-7 shrink-0 items-center justify-center rounded-md bg-gradient-to-br ring-1',
            scenario.accent,
            'ring-border/60',
          )}
        >
          <Icon size={13} className="text-foreground/80" />
        </span>
        <span className="flex-1 truncate text-[13.5px] font-semibold tracking-tight text-foreground">{title}</span>
        <span className="rounded-full bg-muted/50 px-1.5 py-0.5 text-[10.5px] font-medium tabular-nums text-muted-foreground/70">
          {scenario.files.length}
        </span>
      </button>
      {open && (
        <div className="space-y-1.5 pl-1">
          {scenario.files.map((f) => {
            const fid = `${scenario.id}:${f.id}`;
            return (
              <CaseRow
                key={fid}
                scenario={scenario}
                file={f}
                selected={fid === selectedFileId}
                onSelect={() => onSelect(f)}
              />
            );
          })}
        </div>
      )}
    </div>
  );
}

// ─── Run output panel ───────────────────────────────────────────────────────

function extractNoVncUrl(text: string): string | null {
  const m = text.match(/https?:\/\/[^\s]*vnc\.html[^\s]*/);
  return m ? m[0] : null;
}

function RunOutput({ stdout, stderr, exitCode, success, elapsedMs, ranEdited }: {
  stdout: string;
  stderr: string;
  exitCode: number;
  success: boolean;
  elapsedMs: number;
  ranEdited: boolean;
}) {
  const { t } = useTranslation('examples');
  const novncUrl = extractNoVncUrl(stdout);
  return (
    <div className="space-y-2.5">
      <div className="flex flex-wrap items-center gap-2 text-xs">
        {success ? (
          <span className="inline-flex items-center gap-1.5 rounded-full bg-cube-emerald/10 px-2.5 py-1 font-medium text-cube-emerald ring-1 ring-cube-emerald/30">
            <CheckCircle2 size={12} />
            {t('output.completed')} · exit 0
          </span>
        ) : (
          <span className="inline-flex items-center gap-1.5 rounded-full bg-destructive/10 px-2.5 py-1 font-medium text-destructive ring-1 ring-destructive/30">
            <XCircle size={12} />
            {t('output.failed')} · exit {exitCode}
          </span>
        )}
        {ranEdited && (
          <span className="inline-flex items-center gap-1 rounded-full bg-cube-amber/10 px-2.5 py-1 text-[10.5px] font-medium text-cube-amber ring-1 ring-cube-amber/30">
            {t('output.runEdited')}
          </span>
        )}
        {!ranEdited && (
          <span className="inline-flex items-center gap-1 rounded-full bg-muted/40 px-2.5 py-1 text-[10.5px] font-medium text-muted-foreground ring-1 ring-border/60">
            {t('output.runOnDisk')}
          </span>
        )}
      </div>

      {novncUrl && (
        <div className="space-y-1.5">
          <div className="flex items-center gap-1.5 text-xs font-medium text-foreground/80">
            <Monitor size={12} />
            {t('novncPreview')}
          </div>
          <div className="overflow-hidden rounded-lg border border-border/60">
            <iframe
              src={novncUrl}
              title="Sandbox noVNC"
              className="h-[660px] w-full"
              style={{ border: 'none' }}
            />
          </div>
        </div>
      )}

      {stdout && (
        <div className="overflow-hidden rounded-lg border border-border/60 bg-muted/30">
          <pre className="max-h-72 overflow-auto p-4 font-mono text-[12px] leading-relaxed text-foreground/90 whitespace-pre-wrap">
            {stdout}
          </pre>
        </div>
      )}
      {stderr && (
        <div className="overflow-hidden rounded-lg border border-destructive/30 bg-destructive/5">
          <pre className="max-h-48 overflow-auto p-4 font-mono text-[12px] leading-relaxed text-destructive whitespace-pre-wrap">
            {stderr}
          </pre>
        </div>
      )}
    </div>
  );
}

// ─── Main page ──────────────────────────────────────────────────────────────

export default function SandboxCasesPage() {
  const { t, i18n } = useTranslation('examples');
  const isZh = (i18n.language ?? 'en').toLowerCase().startsWith('zh');

  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [activeCategory, setActiveCategory] = useState<ExampleCategoryId | 'all'>('all');
  const [search, setSearch] = useState('');
  const [selectedTemplateId, setSelectedTemplateId] = useState<string | undefined>(undefined);
  const [editedCode, setEditedCode] = useState<string>('');
  const [codeDirty, setCodeDirty] = useState(false);
  const [cubeApiUrl, setCubeApiUrl] = usePersistedState<string>('cube-api-url', '');
  const [cubeProxyIp, setCubeProxyIp] = usePersistedState<string>('cube-proxy-ip', '');
  const [configExpanded, setConfigExpanded] = useState(false);
  const [draftApiUrl, setDraftApiUrl] = useState('');
  const [draftProxyIp, setDraftProxyIp] = useState('');

  const { data: templates } = useQuery({
    queryKey: ['templates'],
    queryFn: () => templateApi.list(),
    refetchInterval: 30_000,
  });

  const { data: config } = useQuery({
    queryKey: ['config'],
    queryFn: () => clusterApi.config(),
  });

  // Pre-fill cluster config overrides from server config (only on first load)
  useEffect(() => {
    if (!config) return;
    if (!cubeApiUrl && config.apiEndpoint) {
      setCubeApiUrl(config.apiEndpoint);
      setDraftApiUrl(config.apiEndpoint);
    }
    if (!cubeProxyIp && config.proxyNodeIp) {
      setCubeProxyIp(config.proxyNodeIp);
      setDraftProxyIp(config.proxyNodeIp);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [config]);

  const { data: storeCatalog } = useQuery({
    queryKey: ['store-catalog'],
    queryFn: storeApi.catalog,
    staleTime: 5 * 60 * 1000,
  });

  const defaultTemplateId = config?.defaultTemplateId;
  const firstTemplateId = templates?.[0]?.templateID;

  // ── Pick initial selection: first scenario, first file ─────────────
  useEffect(() => {
    if (selectedId !== null) return;
    const firstScenario = EXAMPLE_SCENARIOS[0];
    if (firstScenario?.files[0]) {
      setSelectedId(`${firstScenario.id}:${firstScenario.files[0].id}`);
    }
  }, [selectedId]);

  // ── Source query (re-fetches when selection changes) ─────────────
  const { data: sourceData, isLoading: isSourceLoading } = useQuery({
    queryKey: ['examples', 'source', selectedId],
    queryFn: () => examplesApi.source(selectedId!),
    enabled: !!selectedId,
  });
  const sourceCode = sourceData?.source ?? '';

  // Reset edited code when selection changes; track "dirty" via diff.
  useEffect(() => {
    setEditedCode(sourceCode);
    setCodeDirty(false);
    runMutation.reset();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sourceCode, selectedId]);

  const handleEditorChange = useCallback(
    (next: string) => {
      setEditedCode(next);
      setCodeDirty(next !== sourceCode);
    },
    [sourceCode],
  );

  const handleRestore = useCallback(() => {
    setEditedCode(sourceCode);
    setCodeDirty(false);
  }, [sourceCode]);

  // ── Run mutation ──────────────────────────────────────────────────
  const runMutation = useMutation({
    mutationFn: (body: RunExampleBody) => examplesApi.run(body),
  });

  const handleCopySource = useCallback(() => {
    const text = codeDirty ? editedCode : sourceCode;
    if (!text) return;
    copyToClipboard(text, t('copied'));
  }, [codeDirty, editedCode, sourceCode, t]);

  // ── Selected file / scenario derived state ────────────────────────
  const selectedParts = selectedId?.split(':') ?? [];
  const selectedScenarioId = selectedParts[0];
  const selectedFileId = selectedParts[1];
  const selectedScenario = useMemo(
    () => EXAMPLE_SCENARIOS.find((s) => s.id === selectedScenarioId),
    [selectedScenarioId],
  );
  const selectedFile = useMemo(
    () =>
      selectedScenario?.files.find((f) => f.id === selectedFileId) ?? null,
    [selectedScenario, selectedFileId],
  );

  // ── Template match status based on storeItemId ────────────────────
  const templateMatchStatus = useMemo<TemplateMatchStatus>(() => {
    const sid = selectedScenario?.storeItemId;
    if (!sid || !storeCatalog || !templates) return { kind: 'not_installed' } as TemplateMatchStatus;
    const catalogItem = storeCatalog.find((c) => c.id === sid);
    if (!catalogItem) return { kind: 'not_installed' } as TemplateMatchStatus;
    return getTemplateMatchStatus(catalogItem, templates);
  }, [selectedScenario, storeCatalog, templates]);

  const recommendedTemplateId = templateMatchStatus.kind === 'ready'
    ? templateMatchStatus.templates[0]?.templateID
    : undefined;

  const needsInstall = templateMatchStatus.kind === 'not_installed' && !!selectedScenario?.storeItemId;

  const missingImageName = useMemo(() => {
    const sid = selectedScenario?.storeItemId;
    if (!sid || !storeCatalog) return '';
    const catalogItem = storeCatalog.find((c) => c.id === sid);
    if (!catalogItem) return '';
    return catalogItem.image.split('/').pop() ?? '';
  }, [selectedScenario, storeCatalog]);

  // Reset template selection when scenario changes so recommended template auto-applies
  useEffect(() => {
    setSelectedTemplateId(undefined);
  }, [selectedScenarioId]);

  const effectiveTemplateId = selectedTemplateId ?? recommendedTemplateId ?? defaultTemplateId ?? firstTemplateId;

  const handleRun = useCallback(() => {
    if (!selectedId) return;
    const body: RunExampleBody = {
      id: selectedId,
      template_id: effectiveTemplateId,
      language: sourceData?.language,
    };
    if (codeDirty) {
      body.code = editedCode;
    }
    // Pass cluster config overrides if user has edited them
    if (cubeApiUrl.trim() && cubeApiUrl !== config?.apiEndpoint) {
      body.api_url = cubeApiUrl.trim();
    }
    if (cubeProxyIp.trim() && cubeProxyIp !== config?.proxyNodeIp) {
      body.proxy_node_ip = cubeProxyIp.trim();
    }
    runMutation.mutate(body);
  }, [
    selectedId,
    effectiveTemplateId,
    sourceData?.language,
    codeDirty,
    editedCode,
    cubeApiUrl,
    cubeProxyIp,
    config?.apiEndpoint,
    config?.proxyNodeIp,
    runMutation,
  ]);

  // ── Filter scenarios by category + search ─────────────────────────
  const filteredScenarios = useMemo(() => {
    const q = search.trim().toLowerCase();
    return EXAMPLE_SCENARIOS.filter((sc) => {
      if (activeCategory !== 'all' && sc.category !== activeCategory) return false;
      if (!q) return true;
      if ((isZh ? sc.titleZh : sc.titleEn).toLowerCase().includes(q)) return true;
      if (sc.id.toLowerCase().includes(q)) return true;
      return sc.files.some(
        (f) =>
          f.id.toLowerCase().includes(q) ||
          f.filename.toLowerCase().includes(q) ||
          (isZh ? f.titleZh : f.titleEn).toLowerCase().includes(q),
      );
    });
  }, [activeCategory, search, isZh]);

  // ── Stats for the hero ────────────────────────────────────────────
  const totalCases = useMemo(
    () => EXAMPLE_SCENARIOS.reduce((sum, sc) => sum + sc.files.length, 0),
    [],
  );
  const totalCategories = EXAMPLE_CATEGORIES.length;

  return (
    <div className="animate-fade-in space-y-5">
      {/* ── Hero ──────────────────────────────────────────────── */}
      <header className="relative overflow-hidden rounded-2xl border border-border/60 bg-gradient-to-br from-card/80 via-card/60 to-card/40 p-6">
        <div className="pointer-events-none absolute -right-20 -top-20 h-64 w-64 rounded-full bg-primary/5 blur-3xl" />
        <div className="pointer-events-none absolute -bottom-12 -left-12 h-48 w-48 rounded-full bg-cube-violet/5 blur-3xl" />
        <div className="relative flex flex-col gap-4 md:flex-row md:items-end md:justify-between">
          <div className="space-y-1.5">
            <div className="flex items-center gap-2">
              <span className="inline-flex h-7 w-7 items-center justify-center rounded-md bg-gradient-to-br from-primary to-cube-violet text-primary-foreground shadow-sm shadow-primary/30">
                <FlaskConical size={14} />
              </span>
              <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
              <Badge tone="info" className="text-[10px]">
                {t('badge')}
              </Badge>
            </div>
            <p className="max-w-2xl text-sm text-muted-foreground">{t('subtitle')}</p>
          </div>
          <div className="flex items-center gap-2 text-xs">
            <div className="rounded-lg border border-border/60 bg-card/60 px-3 py-1.5">
              <span className="text-muted-foreground">scenarios · </span>
              <span className="font-mono font-semibold text-foreground">{EXAMPLE_SCENARIOS.length}</span>
            </div>
            <div className="rounded-lg border border-border/60 bg-card/60 px-3 py-1.5">
              <span className="text-muted-foreground">cases · </span>
              <span className="font-mono font-semibold text-foreground">{totalCases}</span>
            </div>
            <div className="rounded-lg border border-border/60 bg-card/60 px-3 py-1.5">
              <span className="text-muted-foreground">categories · </span>
              <span className="font-mono font-semibold text-foreground">{totalCategories}</span>
            </div>
          </div>
        </div>
      </header>

      {/* ── Connection Config Bar ─────────────────────────── */}
      <div className="overflow-hidden rounded-xl border border-border/60 bg-card/50 shadow-sm backdrop-blur-sm">
        {/* Collapsed header — always visible */}
        <button
          type="button"
          onClick={() => {
            if (!configExpanded) {
              // Open: sync draft from current effective values
              setDraftApiUrl(cubeApiUrl);
              setDraftProxyIp(cubeProxyIp);
            }
            setConfigExpanded((v) => !v);
          }}
          className={cn(
            'flex w-full items-center gap-3 px-4 py-2.5 text-left transition-colors',
            'hover:bg-muted/30',
          )}
        >
          <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-lg bg-gradient-to-br from-primary/15 to-cube-violet/10 text-primary">
            <Globe2 size={13} />
          </span>
          <span className="text-sm font-semibold tracking-wide text-foreground/80">
            {t('clusterConfig.label')}
          </span>
          {/* Collapsed preview of current values */}
          <span className="flex flex-1 items-center gap-2.5 overflow-x-auto">
            <span className="inline-flex items-center gap-1.5 rounded-lg border border-border/50 bg-muted/40 px-3 py-1">
              <span className="text-xs font-semibold text-muted-foreground/70">CubeAPI</span>
              <span className={cn('truncate font-mono text-xs', cubeApiUrl ? 'text-foreground/80' : 'text-muted-foreground/50 italic')}>{cubeApiUrl || t('clusterConfig.notSet')}</span>
            </span>
            <span className="inline-flex items-center gap-1.5 rounded-lg border border-border/50 bg-muted/40 px-3 py-1">
              <span className="text-xs font-semibold text-muted-foreground/70">CubeProxy</span>
              <span className={cn('truncate font-mono text-xs', cubeProxyIp ? 'text-foreground/80' : 'text-muted-foreground/50 italic')}>{cubeProxyIp || t('clusterConfig.notSet')}</span>
            </span>
          </span>
          <ChevronRight
            size={14}
            className={cn(
              'shrink-0 text-muted-foreground/50 transition-transform duration-200',
              configExpanded && 'rotate-90',
            )}
          />
        </button>

        {/* Expanded edit panel */}
        {configExpanded && (
          <div className="border-t border-border/40 bg-background/60 px-5 py-4">
            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              {/* CubeAPI row */}
              <div className="flex flex-col gap-1.5">
                <label className="flex items-center gap-1.5 text-sm font-medium text-muted-foreground/80">
                  <Boxes size={12} className="text-primary/60" />
                  {t('clusterConfig.apiUrl')}
                </label>
                <div className="relative">
                  <span className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-muted-foreground/40">
                    <Globe2 size={12} />
                  </span>
                  <input
                    value={draftApiUrl}
                    onChange={(e) => setDraftApiUrl(e.target.value)}
                    placeholder="http://127.0.0.1:3000"
                    className={cn(
                      'h-9 w-full rounded-lg border border-input bg-background pl-8 pr-3',
                      'text-xs font-mono text-foreground/90',
                      'placeholder:text-muted-foreground/40',
                      'transition-all duration-150',
                      'hover:border-primary/30',
                      'focus:border-primary/50 focus:outline-none focus:ring-2 focus:ring-primary/15',
                    )}
                  />
                </div>
              </div>
              {/* CubeProxy row */}
              <div className="flex flex-col gap-1.5">
                <label className="flex items-center gap-1.5 text-sm font-medium text-muted-foreground/80">
                  <Network size={12} className="text-cube-violet/60" />
                  {t('clusterConfig.proxyIp')}
                </label>
                <div className="relative">
                  <span className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-muted-foreground/40">
                    <Network size={12} />
                  </span>
                  <input
                    value={draftProxyIp}
                    onChange={(e) => setDraftProxyIp(e.target.value)}
                    placeholder="127.0.0.1"
                    className={cn(
                      'h-9 w-full rounded-lg border border-input bg-background pl-8 pr-3',
                      'text-xs font-mono text-foreground/90',
                      'placeholder:text-muted-foreground/40',
                      'transition-all duration-150',
                      'hover:border-primary/30',
                      'focus:border-primary/50 focus:outline-none focus:ring-2 focus:ring-primary/15',
                    )}
                  />
                </div>
              </div>
            </div>
            {/* Hint */}
            <p className="mt-3 text-[10.5px] text-muted-foreground/60">{t('clusterConfig.hint')}</p>
            {/* Actions */}
            <div className="mt-4 flex items-center gap-3">
              <button
                type="button"
                onClick={() => {
                  setCubeApiUrl(draftApiUrl.trim());
                  setCubeProxyIp(draftProxyIp.trim());
                  setConfigExpanded(false);
                }}
                className={cn(
                  'inline-flex h-9 items-center gap-2 rounded-lg bg-primary px-5',
                  'text-xs font-semibold text-primary-foreground',
                  'shadow-sm shadow-primary/25',
                  'transition-all duration-150',
                  'hover:bg-primary/90 hover:shadow-md hover:shadow-primary/20',
                  'active:scale-[0.97]',
                )}
              >
                <Check size={13} strokeWidth={2.5} />
                {t('clusterConfig.save')}
              </button>
              <button
                type="button"
                onClick={() => setConfigExpanded(false)}
                className={cn(
                  'inline-flex h-9 items-center gap-1.5 rounded-lg border border-border/60 bg-background px-5',
                  'text-xs font-medium text-muted-foreground',
                  'transition-all duration-150',
                  'hover:border-border hover:bg-muted/40 hover:text-foreground/80',
                  'active:scale-[0.97]',
                )}
              >
                {t('clusterConfig.cancel')}
              </button>
            </div>
          </div>
        )}
      </div>

      {/* ── Toolbar ───────────────────────────────────────────── */}
      <div className="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
        <div className="flex items-center gap-2">
          <div className="relative w-full lg:w-72">
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
          {EXAMPLE_CATEGORIES.map((cat) => {
            const Icon = cat.icon;
            return (
              <button
                key={cat.id}
                onClick={() => setActiveCategory(cat.id)}
                className={cn(
                  'inline-flex items-center gap-1.5 rounded-full px-3 py-1 text-xs font-medium transition-colors',
                  activeCategory === cat.id
                    ? 'bg-primary text-primary-foreground shadow-sm shadow-primary/20'
                    : 'bg-muted/40 text-muted-foreground hover:bg-muted/70 hover:text-foreground',
                )}
              >
                <Icon size={11} />
                {t(cat.i18nKey as never)}
              </button>
            );
          })}
        </div>
      </div>

      {/* ── Main grid ─────────────────────────────────────────── */}
      <div className="grid grid-cols-1 gap-5 xl:grid-cols-[340px_1fr]">
        {/* Left rail */}
        <div className="space-y-2">
          <div className="flex items-center gap-1.5 px-1">
            <FlaskConical size={11} className="text-muted-foreground" />
            <p className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground/80">
              {t('sidebar.scenarios')}
            </p>
            <span className="text-[10px] text-muted-foreground/50">
              · {filteredScenarios.length}
            </span>
          </div>
          {filteredScenarios.length === 0 ? (
            <div className="flex flex-col items-center justify-center gap-2 rounded-xl border border-dashed border-border/60 py-12 text-center">
              <Search className="h-5 w-5 text-muted-foreground/50" />
              <p className="text-sm text-muted-foreground">{t('noResults')}</p>
            </div>
          ) : (
            <div className="space-y-3 rounded-xl border border-border/60 bg-card/30 p-2.5">
              {filteredScenarios.map((sc) => (
                <ScenarioGroup
                  key={sc.id}
                  scenario={sc}
                  selectedFileId={selectedId}
                  onSelect={(f) => setSelectedId(`${sc.id}:${f.id}`)}
                />
              ))}
            </div>
          )}
        </div>

        {/* Right workspace */}
        <div className="space-y-4 min-w-0">
          {!selectedScenario || !selectedFile ? (
            <Card className="flex h-80 flex-col items-center justify-center gap-3 border-dashed bg-card/30 p-8 text-center">
              <span className="flex h-12 w-12 items-center justify-center rounded-full bg-gradient-to-br from-primary/15 to-cube-violet/15 text-primary">
                <FlaskConical size={20} />
              </span>
              <div className="space-y-1">
                <p className="text-sm font-medium text-foreground">{t('selectHintTitle')}</p>
                <p className="text-xs text-muted-foreground">{t('selectHint')}</p>
              </div>
            </Card>
          ) : (
            <>
              {/* Code panel */}
              <Card className="overflow-hidden p-0">
                <div className="flex flex-wrap items-center justify-between gap-2 border-b border-border/60 bg-muted/20 px-4 py-2.5">
                  <div className="flex min-w-0 items-center gap-2">
                    <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-md bg-primary/10 text-primary">
                      <FileCode2 size={13} />
                    </span>
                    <span className="truncate text-sm font-medium text-foreground">
                      {isZh ? selectedFile.titleZh : selectedFile.titleEn}
                    </span>
                    <span className="hidden font-mono text-[11px] text-muted-foreground sm:inline">
                      · {selectedFile.filename}
                    </span>
                    {codeDirty && (
                      <span className="inline-flex items-center gap-1 rounded-full bg-cube-amber/15 px-2 py-0.5 text-[10px] font-medium text-cube-amber ring-1 ring-cube-amber/30">
                        {t('editor.dirty')}
                      </span>
                    )}
                  </div>
                  <div className="flex items-center gap-2">
                    <TemplateDropdown
                      templates={templates ?? []}
                      defaultTemplateId={defaultTemplateId}
                      value={effectiveTemplateId}
                      onChange={setSelectedTemplateId}
                    />
                    {templateMatchStatus.kind === 'ready' && recommendedTemplateId && (
                      <span className="inline-flex items-center gap-1.5 rounded-md border border-cube-emerald/40 bg-cube-emerald/10 px-2 py-1 text-[11px] font-medium text-cube-emerald">
                        <CheckCircle2 size={12} />
                        {t('templateSelector.templateReady')}
                      </span>
                    )}
                    {templateMatchStatus.kind === 'building' && (
                      <span className="inline-flex items-center gap-1.5 rounded-md border border-cube-amber/40 bg-cube-amber/10 px-2 py-1 text-[11px] font-medium text-cube-amber">
                        <Loader2 size={12} className="animate-spin" />
                        {t('templateSelector.templateBuilding')}
                      </span>
                    )}
                    {needsInstall && missingImageName && (
                      <Link
                        to={`/store?search=${encodeURIComponent(missingImageName)}`}
                        className="inline-flex items-center gap-1.5 rounded-md border border-orange-500/40 bg-orange-500/10 px-2 py-1 text-[11px] font-medium text-orange-600 dark:text-orange-400 transition-colors hover:bg-orange-500/20"
                      >
                        <AlertTriangle size={12} />
                        {t('templateSelector.needsInstallWithName', { name: missingImageName })}
                      </Link>
                    )}
                    {codeDirty && (
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={handleRestore}
                        className="h-7 gap-1 px-2 text-[11px]"
                        title={t('editor.restoreHint')}
                      >
                        <RotateCcw size={11} />
                        {t('editor.restore')}
                      </Button>
                    )}
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
                      disabled={runMutation.isPending || !effectiveTemplateId || isSourceLoading}
                      onClick={handleRun}
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
                <div className="bg-[#0b0d12] p-3">
                  {isSourceLoading ? (
                    <div className="space-y-2 p-2">
                      {Array.from({ length: 8 }).map((_, i) => (
                        <Skeleton key={i} className="h-3" style={{ width: `${40 + Math.random() * 60}%` }} />
                      ))}
                    </div>
                  ) : (
                    <CodeEditor
                      value={editedCode}
                      language={languageToEditor(sourceData?.language ?? selectedFile.language)}
                      onChange={handleEditorChange}
                      height={420}
                      minHeight={360}
                      ariaLabel={`${selectedFile.filename} source`}
                    />
                  )}
                </div>
                {codeDirty && (
                  <p className="border-t border-cube-amber/20 bg-cube-amber/5 px-4 py-1.5 text-[11px] text-cube-amber">
                    {t('editor.dirtyHint')}
                  </p>
                )}
              </Card>

              {/* Output panel */}
              <Card className="overflow-hidden p-0">
                <div className="flex items-center justify-between border-b border-border/60 bg-muted/20 px-4 py-2.5">
                  <div className="flex items-center gap-2">
                    <span className="flex h-6 w-6 items-center justify-center rounded-md bg-cube-emerald/10 text-cube-emerald">
                      <Terminal size={13} />
                    </span>
                    <span className="text-sm font-medium text-foreground">{t('output.title')}</span>
                    {runMutation.isPending && (
                      <span className="inline-flex items-center gap-1 rounded-full bg-primary/10 px-2 py-0.5 text-[10px] font-medium text-primary">
                        <Timer size={9} className="animate-pulse" />
                        {t('running')}
                      </span>
                    )}
                  </div>
                  {runMutation.data && (
                    <div className="flex items-center gap-2">
                      <span className="text-[10px] text-muted-foreground/70">
                        {runMutation.data.success ? t('output.completed') : t('output.failed')}
                      </span>
                      <span className="inline-flex items-center gap-1 rounded-full bg-muted/60 px-2.5 py-1 font-mono text-[10.5px] font-medium tabular-nums text-muted-foreground ring-1 ring-border/60">
                        <Timer size={10} />
                        {t('output.elapsed', { ms: runMutation.data.elapsed_ms } as unknown as Record<string, unknown>)}
                      </span>
                    </div>
                  )}
                </div>
                <div className="p-4">
                  {runMutation.isError ? (
                    <div className="rounded-lg border border-destructive/30 bg-destructive/5 px-4 py-3 text-sm text-destructive">
                      {(runMutation.error as Error)?.message ?? t('output.failed')}
                    </div>
                  ) : !runMutation.data && !runMutation.isPending ? (
                    <div className="flex flex-col items-center justify-center gap-2 py-8 text-center">
                      <span className="flex h-10 w-10 items-center justify-center rounded-full bg-muted/50 text-muted-foreground">
                        <Inbox size={18} />
                      </span>
                      <p className="text-sm text-muted-foreground">{t('outputHint')}</p>
                      <Button size="sm" variant="outline" onClick={handleRun} className="mt-1">
                        <Play size={12} />
                        {t('run')}
                      </Button>
                    </div>
                  ) : runMutation.isPending ? (
                    <div className="flex items-center gap-3 rounded-lg border border-primary/20 bg-primary/5 px-4 py-3 text-sm text-foreground">
                      <span className="relative flex h-2.5 w-2.5">
                        <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-primary/60" />
                        <span className="relative inline-flex h-2.5 w-2.5 rounded-full bg-primary" />
                      </span>
                      <span className="text-muted-foreground">{t('runningHint')}</span>
                    </div>
                  ) : runMutation.data ? (
                    <RunOutput
                      stdout={runMutation.data.stdout}
                      stderr={runMutation.data.stderr}
                      exitCode={runMutation.data.exit_code}
                      success={runMutation.data.success}
                      elapsedMs={runMutation.data.elapsed_ms}
                      ranEdited={runMutation.data.ran_edited}
                    />
                  ) : null}
                </div>
              </Card>

              {/* Topology */}
              <Card className="overflow-hidden p-0">
                <div className="flex items-center justify-between border-b border-border/60 bg-muted/20 px-4 py-2.5">
                  <div className="flex items-center gap-2">
                    <span className="flex h-6 w-6 items-center justify-center rounded-md bg-cube-cyan/10 text-cube-cyan">
                      <Network size={13} />
                    </span>
                    <span className="text-sm font-medium text-foreground">{t('topology.title')}</span>
                    <Badge tone="info" className="text-[9px]">
                      {isZh ? selectedScenario.titleZh : selectedScenario.titleEn}
                    </Badge>
                  </div>
                  <span className="text-[10px] text-muted-foreground/60">
                    {t('topology.hint')}
                  </span>
                </div>
                <div className="p-3">
                  <TopologyGraph
                    nodes={selectedScenario.topology.nodes}
                    edges={selectedScenario.topology.edges}
                    height={420}
                  />
                </div>
              </Card>
            </>
          )}
        </div>
      </div>
    </div>
  );
}
