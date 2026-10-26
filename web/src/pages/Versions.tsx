// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  GitBranch,
  CheckCircle2,
  AlertTriangle,
  AlertOctagon,
  Search,
  RefreshCw,
  PackageOpen,
  ChevronRight,
  X,
} from 'lucide-react';
import { versionApi, type VersionMatrixDto } from '@/api/client';
import { Card } from '@/components/ui/card';
import { Skeleton } from '@/components/ui/skeleton';
import { MonoId } from '@/components/ui/typography';
import { cn, formatRelative } from '@/lib/utils';

// ── Types ───────────────────────────────────────────────────────────────────

type ComponentRow = VersionMatrixDto['components'][number];
type NodeRow = VersionMatrixDto['nodes'][number];

type CompFilter = 'all' | 'consistent' | 'multiVersion' | 'undeclared';
type NodeFilter = 'all' | 'healthy' | 'notReady';

function declaredVersionsFor(row: ComponentRow): string[] {
  const versions =
    row.declaredVersions && row.declaredVersions.length > 0
      ? row.declaredVersions
      : row.declaredVersion
        ? [row.declaredVersion]
        : [];
  return versions.filter((v) => v && v !== 'unknown');
}

function isVersionUndeclared(row: ComponentRow, version: string): boolean {
  const declared = declaredVersionsFor(row);
  return (
    declared.length > 0 &&
    version !== '' &&
    version !== 'unknown' &&
    !declared.some((declaredVersion) => versionMatchesDeclared(declaredVersion, version))
  );
}

const platformVersionSuffixes = ['-amd64', '-arm64', '-x86_64', '-aarch64'] as const;

// Declared manifest versions stay canonical; only actual is normalized.
function versionMatchesDeclared(declared: string, actual: string): boolean {
  if (declared === actual) {
    return true;
  }
  return stripPlatformVersionSuffix(actual) === declared;
}

function stripPlatformVersionSuffix(version: string): string {
  let normalized = version;
  let changed = true;
  while (changed) {
    changed = false;
    for (const suffix of platformVersionSuffixes) {
      if (normalized.endsWith(suffix)) {
        normalized = normalized.slice(0, -suffix.length);
        changed = true;
        break;
      }
    }
  }
  return normalized;
}

function rowHasUndeclaredVersion(row: ComponentRow): boolean {
  return (row.versions ?? []).some((g) => isVersionUndeclared(row, g.version));
}

function hasReleaseDeclaration(rows: ComponentRow[]): boolean {
  return rows.some((row) => declaredVersionsFor(row).length > 0);
}

function displayVersionIdentity(version: string): string {
  const marker = '@sha256:';
  const markerIndex = version.indexOf(marker);
  if (markerIndex > 0) {
    const tag = version.slice(0, markerIndex);
    const hash = version.slice(markerIndex + marker.length);
    return `${tag} @ ${hash.slice(0, 12)}`;
  }
  if (version.startsWith('sha256:')) {
    return `sha256:${version.slice('sha256:'.length, 'sha256:'.length + 12)}`;
  }
  return version;
}

// ── Page ────────────────────────────────────────────────────────────────────

export default function VersionsPage() {
  const { t } = useTranslation('versions');

  const { data, isLoading, isError, refetch, isFetching, dataUpdatedAt } = useQuery({
    queryKey: ['versions'],
    queryFn: versionApi.matrix,
    refetchInterval: 15_000,
  });

  const components = data?.components ?? [];
  const nodes = data?.nodes ?? [];

  const { multiVersionCount, undeclaredCount, reportingCount } = useMemo(() => {
    const reporting = nodes.filter((n) => n.healthy).length;
    const multiVersion = components.filter((c) => !c.consistent).length;
    const undeclared = components.filter(rowHasUndeclaredVersion).length;
    return {
      multiVersionCount: multiVersion,
      undeclaredCount: undeclared,
      reportingCount: reporting,
    };
  }, [components, nodes]);

  return (
    <div className="animate-fade-in space-y-6">
      {/* Header */}
      <header className="flex items-end justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t('subtitle')}</p>
        </div>
        <div className="flex items-center gap-3 text-xs text-muted-foreground">
          <span className="inline-flex h-1.5 w-1.5 animate-pulse-soft rounded-full bg-cube-ok" />
          {dataUpdatedAt
            ? t('lastRefreshed', { time: formatRelative(new Date(dataUpdatedAt).toISOString()) })
            : t('loading')}
          <button
            onClick={() => refetch()}
            className="inline-flex h-7 w-7 items-center justify-center rounded-md border border-border/60 bg-card/40 text-muted-foreground transition-colors hover:border-primary/30 hover:text-foreground disabled:opacity-50"
            disabled={isFetching}
            title={t('retry')}
          >
            <RefreshCw size={12} className={cn(isFetching && 'animate-spin')} />
          </button>
        </div>
      </header>

      {/* Loading skeleton */}
      {isLoading && (
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            {Array.from({ length: 4 }).map((_, i) => (
              <Skeleton key={i} className="h-24" />
            ))}
          </div>
          <Skeleton className="h-32 w-full" />
          <Skeleton className="h-64 w-full" />
        </div>
      )}

      {/* Error state */}
      {isError && !isLoading && (
        <Card>
          <div className="flex flex-col items-center gap-3 py-10 text-center">
            <span className="flex h-10 w-10 items-center justify-center rounded-lg bg-cube-err/10 text-cube-err ring-1 ring-cube-err/30">
              <AlertOctagon size={18} />
            </span>
            <p className="text-sm text-muted-foreground">{t('error')}</p>
            <button
              onClick={() => refetch()}
              className="inline-flex items-center gap-1.5 rounded-md border border-border/60 bg-card/40 px-3 py-1.5 text-xs text-foreground/80 hover:border-primary/30 hover:text-foreground"
            >
              <RefreshCw size={12} />
              {t('retry')}
            </button>
          </div>
        </Card>
      )}

      {/* Body */}
      {!isLoading && !isError && data && (
        <>
          {/* KPI strip */}
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            <KpiCard
              label={t('kpi.components')}
              value={components.length}
              tone="info"
              hint={t('kpi.componentsHint')}
            />
            <KpiCard
              label={t('kpi.reporting')}
              value={`${reportingCount}/${nodes.length}`}
              tone={reportingCount === nodes.length && nodes.length > 0 ? 'ok' : 'warn'}
              hint={t('kpi.reportingHint')}
            />
            <KpiCard
              label={t('kpi.multiVersion')}
              value={multiVersionCount}
              tone={multiVersionCount === 0 ? 'ok' : 'info'}
              hint={t('kpi.multiVersionHint')}
            />
            <KpiCard
              label={t('kpi.undeclared')}
              value={undeclaredCount}
              tone={undeclaredCount === 0 ? 'ok' : 'warn'}
              hint={t('kpi.undeclaredHint')}
            />
          </div>

          {/* Control plane reference (compact) */}
          {data.controlPlane?.version && (
            <Card className="p-4">
              <div className="flex items-center gap-3">
                <span className="flex h-9 w-9 items-center justify-center rounded-lg bg-muted text-muted-foreground ring-1 ring-border/60">
                  <GitBranch size={16} />
                </span>
                <div className="min-w-0 flex-1">
                  <div className="text-xs uppercase tracking-wider text-muted-foreground/70">
                    {t('controlPlane')}
                  </div>
                  <div className="mt-0.5 flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
                    <MonoId size="base">{data.controlPlane.version}</MonoId>
                    {data.controlPlane.commit && (
                      <MonoId size="xs" muted>
                        {data.controlPlane.commit.slice(0, 12)}
                      </MonoId>
                    )}
                    {data.controlPlane.buildTime && (
                      <MonoId size="xs" muted>
                        {data.controlPlane.buildTime}
                      </MonoId>
                    )}
                  </div>
                </div>
              </div>
            </Card>
          )}

          {/* Components section */}
          {components.length > 0 ? (
            <ComponentsSection
              components={components}
              hasReleaseDeclaration={hasReleaseDeclaration(components)}
            />
          ) : (
            <EmptyState icon={PackageOpen} title={t('empty')} hint={t('emptyHint')} />
          )}

          {/* Node × Component matrix */}
          {nodes.length > 0 && components.length > 0 && (
            <MatrixSection nodes={nodes} components={components} />
          )}
        </>
      )}
    </div>
  );
}

// ── KPI card ────────────────────────────────────────────────────────────────

function KpiCard({
  label,
  value,
  tone,
  hint,
}: {
  label: string;
  value: number | string;
  tone: 'ok' | 'warn' | 'err' | 'info';
  hint?: string;
}) {
  // Tone is conveyed by the value color only — no card ring (a colored
  // ring on one card was visually heavier than the information warranted).
  const valueClass =
    tone === 'ok'
      ? 'text-cube-ok'
      : tone === 'warn'
        ? 'text-cube-warn'
        : tone === 'err'
          ? 'text-cube-err'
          : 'text-foreground';
  return (
    <div className="rounded-xl border border-border/60 bg-card/40 p-4">
      <div className="text-xs uppercase tracking-wider text-muted-foreground/70">{label}</div>
      <div className={cn('mt-1.5 text-2xl font-semibold tabular-nums leading-none', valueClass)}>
        {value}
      </div>
      {hint && <div className="mt-1.5 text-xs text-muted-foreground/70">{hint}</div>}
    </div>
  );
}

// ── Components section ──────────────────────────────────────────────────────

function ComponentsSection({
  components,
  hasReleaseDeclaration,
}: {
  components: ComponentRow[];
  hasReleaseDeclaration: boolean;
}) {
  const { t } = useTranslation('versions');
  const [query, setQuery] = useState('');
  const [filter, setFilter] = useState<CompFilter>('all');
  const [expanded, setExpanded] = useState<string | null>(null);

  const counts = useMemo(() => {
    return {
      all: components.length,
      consistent: components.filter((c) => c.consistent).length,
      multiVersion: components.filter((c) => !c.consistent).length,
      undeclared: components.filter(rowHasUndeclaredVersion).length,
    };
  }, [components]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return components.filter((c) => {
      if (q && !c.component.toLowerCase().includes(q)) return false;
      const hasUndeclared = rowHasUndeclaredVersion(c);
      if (filter === 'consistent' && !c.consistent) return false;
      if (filter === 'multiVersion' && c.consistent) return false;
      if (filter === 'undeclared' && !hasUndeclared) return false;
      return true;
    });
  }, [components, query, filter]);

  return (
    <section className="space-y-3">
      <SectionHeader
        title={t('componentsSection')}
        right={
          <SearchAndFilter
            query={query}
            onQuery={setQuery}
            placeholder={t('search.components')}
            filter={filter}
            onFilter={setFilter}
            options={[
              { value: 'all', label: t('filter.all'), count: counts.all },
              { value: 'consistent', label: t('filter.consistent'), count: counts.consistent },
              {
                value: 'multiVersion',
                label: t('filter.multiVersion'),
                count: counts.multiVersion,
              },
              { value: 'undeclared', label: t('filter.undeclared'), count: counts.undeclared },
            ]}
          />
        }
      />
      <div className="rounded-xl border border-border/60 bg-card/40 divide-y divide-border/40">
        {filtered.map((c) => (
          <ComponentRowItem
            key={c.component}
            row={c}
            hasReleaseDeclaration={hasReleaseDeclaration}
            expanded={expanded === c.component}
            onToggle={() => setExpanded((prev) => (prev === c.component ? null : c.component))}
          />
        ))}
        {filtered.length === 0 && (
          <div className="py-10 text-center text-sm text-muted-foreground">{t('noMatch')}</div>
        )}
      </div>
    </section>
  );
}

function ComponentRowItem({
  row,
  hasReleaseDeclaration,
  expanded,
  onToggle,
}: {
  row: ComponentRow;
  hasReleaseDeclaration: boolean;
  expanded: boolean;
  onToggle: () => void;
}) {
  const { t } = useTranslation('versions');
  const declared = declaredVersionsFor(row);
  const declaredLabel = declared.map(displayVersionIdentity).join(' / ');
  const hasUndeclared = rowHasUndeclaredVersion(row);

  let badge: React.ReactNode;
  if (hasUndeclared) {
    badge = (
      <span className="chip-warn">
        <AlertTriangle size={11} /> {t('undeclared')}
      </span>
    );
  } else if (row.consistent) {
    badge = (
      <span className="chip-ok">
        <CheckCircle2 size={11} /> {t('singleVersion')}
      </span>
    );
  } else {
    badge = (
      <span className="chip-info">
        {t('multiVersionWithCount', { count: row.versions.length })}
      </span>
    );
  }

  return (
    <div>
      <button
        onClick={onToggle}
        className="flex w-full items-start justify-between gap-4 px-4 py-3 text-left transition-colors hover:bg-muted/40"
      >
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <MonoId>{row.component}</MonoId>
            {badge}
          </div>
          {/* Inline release declarations + actual versions for left-to-right scan. */}
          <div className="mt-1.5 flex flex-wrap items-center gap-x-1.5 gap-y-1 text-xs">
            <span className="text-muted-foreground/70">{t('declared')}:</span>
            {declaredLabel ? (
              <span className="font-mono text-foreground/80">{declaredLabel}</span>
            ) : !hasReleaseDeclaration ? (
              <span className="text-muted-foreground/60 italic">{t('noDeclarationReference')}</span>
            ) : (
              <span className="text-muted-foreground/60 italic">{t('noDeclared')}</span>
            )}
            {row.versions.map((g) => {
              const undeclared = isVersionUndeclared(row, g.version);
              const noRef = !declaredLabel;
              const chipClass = undeclared
                ? 'border-cube-warn/40 bg-cube-warn/[0.08] text-cube-warn'
                : noRef
                  ? 'border-cube-mute/30 bg-cube-mute/[0.06] text-foreground/80'
                  : 'border-cube-ok/30 bg-cube-ok/[0.06] text-foreground/80';
              return (
                <span
                  key={g.version}
                  title={`${g.version}\n${g.nodes.join(', ')}`}
                  className={cn(
                    'inline-flex items-center gap-1 rounded-md border px-2 py-0.5 font-mono text-xs ml-1',
                    chipClass,
                  )}
                >
                  {undeclared && <AlertTriangle size={10} />}
                  {displayVersionIdentity(g.version)}
                  <span className="text-muted-foreground/60">×{g.nodes.length}</span>
                </span>
              );
            })}
          </div>
        </div>
        <ChevronRight
          size={14}
          className={cn(
            'mt-1 shrink-0 text-muted-foreground/50 transition-transform',
            expanded && 'rotate-90',
          )}
        />
      </button>
      {expanded && (
        <div className="border-t border-border/40 bg-muted/20 px-4 py-3">
          <div className="mb-2 text-xs uppercase tracking-wider text-muted-foreground/70">
            {t('affectedNodes')}
          </div>
          <div className="flex flex-wrap gap-1.5">
            {row.versions.flatMap((g) =>
              g.nodes.map((nodeID) => (
                <Link
                  key={`${g.version}-${nodeID}`}
                  to={`/nodes/${nodeID}`}
                  className={cn(
                    'inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 font-mono text-xs transition-colors hover:bg-muted',
                    isVersionUndeclared(row, g.version)
                      ? 'border-cube-warn/30 text-cube-warn hover:border-cube-warn/60'
                      : 'border-border/60 text-foreground/80 hover:border-primary/30',
                  )}
                >
                  {nodeID}
                  <span className="text-muted-foreground/60">
                    {displayVersionIdentity(g.version)}
                  </span>
                </Link>
              )),
            )}
          </div>
        </div>
      )}
    </div>
  );
}

// ── Node × Component matrix ─────────────────────────────────────────────────

function MatrixSection({ nodes, components }: { nodes: NodeRow[]; components: ComponentRow[] }) {
  const { t } = useTranslation('versions');
  const [query, setQuery] = useState('');
  const [filter, setFilter] = useState<NodeFilter>('all');
  const [sortBy, setSortBy] = useState<{ col: string | null; dir: 'asc' | 'desc' }>({
    col: null,
    dir: 'asc',
  });

  const counts = useMemo(
    () => ({
      all: nodes.length,
      healthy: nodes.filter((n) => n.healthy).length,
      notReady: nodes.filter((n) => !n.healthy).length,
    }),
    [nodes],
  );
  const componentNames = useMemo(() => components.map((c) => c.component), [components]);
  const componentsWithDeclaration = useMemo(
    () =>
      new Set(components.filter((c) => declaredVersionsFor(c).length > 0).map((c) => c.component)),
    [components],
  );

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return nodes
      .filter((n) => {
        const nodeID = n.nodeID ?? '';
        if (q && !nodeID.toLowerCase().includes(q)) return false;
        if (filter === 'healthy' && !n.healthy) return false;
        if (filter === 'notReady' && n.healthy) return false;
        return true;
      })
      .sort((a, b) => {
        const aID = a.nodeID ?? '';
        const bID = b.nodeID ?? '';
        if (sortBy.col == null) return aID.localeCompare(bID);
        const aEntry = a.components.find((e) => e.component === sortBy.col);
        const bEntry = b.components.find((e) => e.component === sortBy.col);
        const av = aEntry?.version ?? '';
        const bv = bEntry?.version ?? '';
        return sortBy.dir === 'asc' ? av.localeCompare(bv) : bv.localeCompare(av);
      });
  }, [nodes, query, filter, sortBy]);

  return (
    <section className="space-y-3">
      <SectionHeader
        title={t('nodesSection')}
        right={
          <SearchAndFilter
            query={query}
            onQuery={setQuery}
            placeholder={t('search.nodes')}
            filter={filter}
            onFilter={(v) => setFilter(v as NodeFilter)}
            options={[
              { value: 'all', label: t('filter.all'), count: counts.all },
              { value: 'healthy', label: t('filter.healthy'), count: counts.healthy },
              { value: 'notReady', label: t('filter.notReady'), count: counts.notReady },
            ]}
          />
        }
      />
      <div className="overflow-x-auto rounded-xl border border-border/60 bg-card/40">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-border/60 bg-popover/50 text-left text-xs uppercase tracking-wider text-muted-foreground/70">
              <th className="sticky left-0 z-10 bg-popover/50 px-4 py-3 font-medium">
                {t('node')}
              </th>
              {componentNames.map((name) => {
                const active = sortBy.col === name;
                return (
                  <th key={name} className="px-4 py-3 font-mono font-medium whitespace-nowrap">
                    <button
                      onClick={() =>
                        setSortBy((s) =>
                          s.col === name
                            ? { col: name, dir: s.dir === 'asc' ? 'desc' : 'asc' }
                            : { col: name, dir: 'asc' },
                        )
                      }
                      className={cn(
                        'inline-flex items-center gap-1 transition-colors',
                        active ? 'text-cube-info' : 'hover:text-foreground',
                      )}
                      title={t('sortByVersion')}
                    >
                      {name}
                      {active && (
                        <span className="text-cube-info">{sortBy.dir === 'asc' ? '↑' : '↓'}</span>
                      )}
                    </button>
                  </th>
                );
              })}
            </tr>
          </thead>
          <tbody className="divide-y divide-border/40">
            {filtered.map((n) => {
              const nodeID = n.nodeID ?? '';
              const byComponent = new Map((n.components ?? []).map((e) => [e.component, e]));
              return (
                <tr
                  key={nodeID}
                  className={cn(
                    'group transition-colors hover:bg-muted/40',
                    !n.healthy && 'bg-cube-err/[0.04]',
                  )}
                >
                  <td className="sticky left-0 z-10 bg-card/40 px-4 py-3 group-hover:bg-muted/40">
                    <Link
                      to={`/nodes/${nodeID}`}
                      className="flex items-center gap-2 text-foreground/90 hover:text-cube-info"
                    >
                      <span
                        className={cn(
                          'h-1.5 w-1.5 shrink-0 rounded-full',
                          n.healthy ? 'bg-cube-ok' : 'bg-cube-err',
                        )}
                      />
                      <MonoId size="sm">{nodeID}</MonoId>
                      <ChevronRight
                        size={11}
                        className="ml-0.5 text-muted-foreground/30 group-hover:text-cube-info"
                      />
                    </Link>
                  </td>
                  {componentNames.map((name) => {
                    const entry = byComponent.get(name);
                    const undeclared =
                      !!entry && componentsWithDeclaration.has(name) && !entry.declared;
                    if (!entry) {
                      return (
                        <td
                          key={name}
                          className="px-4 py-3 text-muted-foreground/40 font-mono text-xs"
                        >
                          {t('missing')}
                        </td>
                      );
                    }
                    return (
                      <td
                        key={name}
                        className={cn('px-4 py-3', undeclared && 'border-l-2 border-cube-warn/50')}
                      >
                        <span
                          className={cn(
                            'inline-flex items-center gap-1 font-mono text-xs',
                            undeclared ? 'text-cube-warn' : 'text-foreground/80',
                          )}
                        >
                          {undeclared && <AlertTriangle size={10} />}
                          <span title={entry.version}>{displayVersionIdentity(entry.version)}</span>
                        </span>
                      </td>
                    );
                  })}
                </tr>
              );
            })}
            {filtered.length === 0 && (
              <tr>
                <td
                  colSpan={componentNames.length + 1}
                  className="py-10 text-center text-sm text-muted-foreground"
                >
                  {t('noMatch')}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      {/* Legend: inline, 11px. The "ok" color is self-evident from chip-ok; only the warn/err mapping benefits from a key. */}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-1 text-[11px] text-muted-foreground/70">
        <span className="uppercase tracking-wider">{t('legend')}:</span>
        <span className="inline-flex items-center gap-1.5">
          <span className="h-1.5 w-1.5 rounded-full bg-cube-ok" /> {t('legend.ok')}
        </span>
        <span className="inline-flex items-center gap-1.5">
          <span className="h-1.5 w-1.5 rounded-full bg-cube-warn" /> {t('legend.warn')}
        </span>
        <span className="inline-flex items-center gap-1.5">
          <span className="h-1.5 w-1.5 rounded-full bg-cube-err" /> {t('legend.err')}
        </span>
      </div>
    </section>
  );
}

// ── Shared primitives ───────────────────────────────────────────────────────

function SectionHeader({ title, right }: { title: string; right?: React.ReactNode }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <span className="text-sm font-medium uppercase tracking-wider text-muted-foreground">
        {title}
      </span>
      {right}
    </div>
  );
}

function SearchAndFilter<T extends string>({
  query,
  onQuery,
  placeholder,
  filter,
  onFilter,
  options,
}: {
  query: string;
  onQuery: (v: string) => void;
  placeholder: string;
  filter: T;
  onFilter: (v: T) => void;
  options: { value: T; label: string; count: number }[];
}) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <div className="relative">
        <Search
          size={13}
          className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-muted-foreground/60"
        />
        <input
          value={query}
          onChange={(e) => onQuery(e.target.value)}
          placeholder={placeholder}
          className="h-8 w-56 rounded-md border border-border/60 bg-card/40 pl-7 pr-7 text-xs text-foreground/90 placeholder:text-muted-foreground/60 focus:border-primary/40 focus:outline-none"
        />
        {query && (
          <button
            onClick={() => onQuery('')}
            className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground/60 hover:text-foreground"
          >
            <X size={11} />
          </button>
        )}
      </div>
      <div className="flex items-center gap-1">
        {options.map((opt) => {
          const active = filter === opt.value;
          return (
            <button
              key={opt.value}
              onClick={() => onFilter(opt.value)}
              className={cn(
                'inline-flex h-8 items-center gap-1.5 rounded-md px-2.5 text-xs transition-colors',
                active
                  ? 'bg-cube-info/10 text-cube-info ring-1 ring-cube-info/30'
                  : 'text-muted-foreground hover:bg-muted/60 hover:text-foreground',
              )}
            >
              {opt.label}
              <span
                className={cn(
                  'rounded px-1 text-[10px] tabular-nums',
                  active ? 'bg-cube-info/15 text-cube-info' : 'bg-muted text-muted-foreground/80',
                )}
              >
                {opt.count}
              </span>
            </button>
          );
        })}
      </div>
    </div>
  );
}

function EmptyState({
  icon: Icon,
  title,
  hint,
}: {
  icon: React.ElementType;
  title: string;
  hint?: string;
}) {
  return (
    <Card>
      <div className="flex flex-col items-center gap-2 py-12 text-center">
        <span className="flex h-10 w-10 items-center justify-center rounded-lg bg-muted text-muted-foreground ring-1 ring-border/60">
          <Icon size={18} />
        </span>
        <p className="text-sm text-foreground/80">{title}</p>
        {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
      </div>
    </Card>
  );
}
