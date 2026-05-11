// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState, useRef, useEffect } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate, useParams, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { templateApi } from '@/api/client';
import { ApiError } from '@/lib/api';
import { Card, CardHeader, CardTitle, CardDescription, CardContent } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Skeleton } from '@/components/ui/skeleton';
import { ArrowLeft, RefreshCw, Trash2, ChevronDown, ChevronUp } from 'lucide-react';
import { cn } from '@/lib/utils';

// ── helpers ─────────────────────────────────────────────────────────────────

function statusTone(status: string): string {
  switch (status.toUpperCase()) {
    case 'READY':   return 'bg-cube-green/15  text-cube-green  border-cube-green/30';
    case 'BUILDING': return 'bg-cube-amber/15 text-cube-amber  border-cube-amber/30';
    case 'FAILED':  return 'bg-cube-rose/15   text-cube-rose   border-cube-rose/30';
    default:        return 'bg-muted          text-muted-foreground border-border';
  }
}

function StatusBadge({ status }: { status: string }) {
  const { t } = useTranslation('templateDetail');
  const label = t(`status.${status.toLowerCase()}`, { defaultValue: status });
  return (
    <span className={cn('inline-flex items-center rounded-full border px-2.5 py-0.5 text-xs font-medium', statusTone(status))}>
      {label}
    </span>
  );
}

function Field({ label, value, mono }: { label: string; value?: string | null; mono?: boolean }) {
  return (
    <div className="flex flex-col gap-0.5">
      <span className="text-[11px] uppercase tracking-wider text-muted-foreground">{label}</span>
      <span className={cn('text-sm break-all', mono && 'font-mono text-xs')}>{value ?? '—'}</span>
    </div>
  );
}

interface SectionProps {
  title: string;
  description?: string;
  children: React.ReactNode;
  variant?: 'default' | 'danger';
}

function Section({ title, description, children, variant = 'default' }: SectionProps) {
  return (
    <Card className={cn(variant === 'danger' && 'border-destructive/40')}>
      <CardHeader className="pb-3">
        <CardTitle className={cn('text-base', variant === 'danger' && 'text-destructive')}>
          {title}
        </CardTitle>
        {description && <CardDescription>{description}</CardDescription>}
      </CardHeader>
      <CardContent>{children}</CardContent>
    </Card>
  );
}

// ── rebuild progress bar ─────────────────────────────────────────────────────

function ProgressBar({ value }: { value: number }) {
  return (
    <div className="h-1.5 w-full rounded-full bg-muted overflow-hidden">
      <div
        className="h-full rounded-full bg-primary transition-all duration-500"
        style={{ width: `${Math.min(100, Math.max(0, value))}%` }}
      />
    </div>
  );
}

// ── build log viewer ─────────────────────────────────────────────────────────

function LogViewer({ templateID, buildID }: { templateID: string; buildID: string }) {
  const bottomRef = useRef<HTMLDivElement>(null);
  const { data: logsData, isLoading } = useQuery({
    queryKey: ['template-build-logs', templateID, buildID],
    queryFn: () => templateApi.getBuildLogs(templateID, buildID),
    refetchInterval: 2000,
  });
  const lines = logsData?.lines ?? [];

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' });
  }, [lines]);

  if (isLoading) return <Skeleton className="h-40 w-full" />;

  return (
    <div className="rounded-md bg-muted/50 border p-3 font-mono text-xs overflow-y-auto max-h-72 space-y-0.5">
      {lines.length === 0 && (
        <span className="text-muted-foreground">No logs yet…</span>
      )}
      {lines.map((line, i) => (
        <div key={i} className="break-all">{line}</div>
      ))}
      <div ref={bottomRef} />
    </div>
  );
}

// ── replica table ─────────────────────────────────────────────────────────────

interface Replica {
  node_id?: string;
  node_ip?: string;
  phase?: string;
  status?: string;
  spec?: string;
  artifact_id?: string;
  snapshot_path?: string;
  last_job_id?: string;
}

function ReplicaTable({ replicas }: { replicas: Replica[] }) {
  const { t } = useTranslation('templateDetail');
  if (replicas.length === 0) {
    return <p className="text-sm text-muted-foreground">{t('empty.replicas')}</p>;
  }

  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr className="border-b text-xs uppercase tracking-wide text-muted-foreground">
            <th className="pb-2 pr-4 text-left font-medium">{t('fields.node')}</th>
            <th className="pb-2 pr-4 text-left font-medium">{t('fields.phase')}</th>
            <th className="pb-2 pr-4 text-left font-medium">{t('fields.spec')}</th>
            <th className="pb-2 pr-4 text-left font-medium">{t('fields.artifactID')}</th>
            <th className="pb-2 text-left font-medium">{t('fields.lastJob')}</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border/50">
          {replicas.map((r, i) => (
            <tr key={i} className="hover:bg-muted/30 transition-colors">
              <td className="py-2 pr-4 font-mono text-xs">{r.node_ip ?? r.node_id ?? '—'}</td>
              <td className="py-2 pr-4">
                <StatusBadge status={r.phase ?? r.status ?? 'UNKNOWN'} />
              </td>
              <td className="py-2 pr-4 font-mono text-xs">{r.spec ?? '—'}</td>
              <td className="py-2 pr-4 font-mono text-xs truncate max-w-[180px]" title={r.artifact_id}>
                {r.artifact_id ? r.artifact_id.slice(0, 20) + '…' : '—'}
              </td>
              <td className="py-2 font-mono text-xs truncate max-w-[160px]" title={r.last_job_id}>
                {r.last_job_id ? r.last_job_id.slice(0, 18) + '…' : '—'}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ── main page ─────────────────────────────────────────────────────────────────

export default function TemplateDetailPage() {
  const { templateID } = useParams<{ templateID: string }>();
  const { t } = useTranslation('templateDetail');
  const navigate = useNavigate();
  const qc = useQueryClient();

  const [showRebuildConfirm, setShowRebuildConfirm] = useState(false);
  const [showDeleteConfirm, setShowDeleteConfirm] = useState(false);
  const [activeBuildID, setActiveBuildID] = useState<string | null>(null);
  const [showLogs, setShowLogs] = useState(false);

  // ── fetch template detail ──
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['template', templateID],
    queryFn: () => templateApi.get(templateID!),
    enabled: !!templateID,
    refetchInterval: activeBuildID ? 3000 : false,
  });

  // ── get cached status from template list (for 404 error context) ──
  const cachedStatus = qc.getQueryData<{ templateID: string; status: string }[]>(['templates'])
    ?.find(t => t.templateID === templateID)?.status?.toUpperCase();

  // ── build status polling ──
  const { data: buildStatus } = useQuery({
    queryKey: ['template-build-status', templateID, activeBuildID],
    queryFn: () => templateApi.getBuildStatus(templateID!, activeBuildID!),
    enabled: !!activeBuildID,
    refetchInterval: 2000,
  });

  // stop polling when build is done
  useEffect(() => {
    if (!buildStatus) return;
    const s = (buildStatus as { status?: string }).status?.toUpperCase();
    if (s === 'READY' || s === 'FAILED') {
      setActiveBuildID(null);
      qc.invalidateQueries({ queryKey: ['template', templateID] });
    }
  }, [buildStatus, templateID, qc]);

  // ── rebuild ──
  const rebuildMutation = useMutation({
    mutationFn: () => templateApi.rebuild(templateID!),
    onSuccess: (job) => {
      const j = job as { jobID?: string };
      if (j.jobID) setActiveBuildID(j.jobID);
      setShowRebuildConfirm(false);
      setShowLogs(true);
    },
  });

  // ── delete ──
  const deleteMutation = useMutation({
    mutationFn: () => templateApi.remove(templateID!),
    onSuccess: () => navigate('/templates'),
  });

  // ── loading / error states ──
  if (isLoading) {
    return (
      <div className="p-6 space-y-4">
        <Skeleton className="h-6 w-48" />
        <Skeleton className="h-32 w-full" />
        <Skeleton className="h-32 w-full" />
      </div>
    );
  }

  // Check if it's a 404 (template still building / not indexed yet)
  const is404 = isError && error instanceof ApiError && error.status === 404;
  const isBuilding404 = is404 && (cachedStatus === 'RUNNING' || cachedStatus === 'BUILDING');
  const isFailed404   = is404 && cachedStatus === 'FAILED';

  if (isError || !data) {
    return (
      <div className="p-6">
        <Link to="/templates" className="flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground mb-4">
          <ArrowLeft className="h-4 w-4" /> {t('backToTemplates')}
        </Link>
        {isBuilding404 ? (
          <div className="space-y-2">
            <p className="text-sm text-muted-foreground">{t('building')}</p>
            <Button variant="outline" size="sm" onClick={() => window.location.reload()}>
              刷新
            </Button>
          </div>
        ) : isFailed404 ? (
          <p className="text-sm text-destructive">{t('buildFailed')}</p>
        ) : (
          <p className="text-sm text-muted-foreground">{t('notFound')}</p>
        )}
      </div>
    );
  }

  const replicas = (data.replicas ?? []) as Replica[];
  const isBuilding = !!activeBuildID || data.status?.toUpperCase() === 'BUILDING';
  const buildProgress = (buildStatus as { progress?: number } | undefined)?.progress ?? 0;

  return (
    <div className="p-6 space-y-5 max-w-4xl">
      {/* back */}
      <Link to="/templates" className="flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground">
        <ArrowLeft className="h-4 w-4" /> {t('backToTemplates')}
      </Link>

      {/* page header */}
      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold font-mono">{data.templateID}</h1>
          <div className="mt-1 flex items-center gap-2">
            <StatusBadge status={data.status ?? 'UNKNOWN'} />
            {data.version && (
              <span className="text-xs text-muted-foreground">{data.version}</span>
            )}
          </div>
        </div>
        <Button
          variant="outline"
          size="sm"
          disabled={isBuilding || rebuildMutation.isPending}
          onClick={() => setShowRebuildConfirm(true)}
        >
          <RefreshCw className={cn('h-4 w-4 mr-1.5', isBuilding && 'animate-spin')} />
          {isBuilding ? t('rebuild.building') : t('rebuild.button')}
        </Button>
      </div>

      {/* rebuild progress */}
      {isBuilding && (
        <div className="space-y-1.5">
          <div className="flex justify-between text-xs text-muted-foreground">
            <span>{t('rebuild.progress', { progress: buildProgress })}</span>
            <button
              className="flex items-center gap-1 hover:text-foreground"
              onClick={() => setShowLogs((v) => !v)}
            >
              {showLogs ? <ChevronUp className="h-3 w-3" /> : <ChevronDown className="h-3 w-3" />}
              {showLogs ? t('rebuild.hideLogs') : t('rebuild.viewLogs')}
            </button>
          </div>
          <ProgressBar value={buildProgress} />
          {showLogs && activeBuildID && (
            <LogViewer templateID={templateID!} buildID={activeBuildID} />
          )}
        </div>
      )}

      {/* basic info */}
      <Section title={t('section.info')} description={t('section.infoDesc')}>
        <div className="grid grid-cols-2 sm:grid-cols-3 gap-4">
          <Field label={t('fields.templateID')} value={data.templateID} mono />
          <Field label={t('fields.status')} value={data.status} />
          <Field label={t('fields.instanceType')} value={data.instanceType ?? '—'} />
          <Field label={t('fields.version')} value={data.version ?? '—'} />
        </div>
      </Section>

      {/* replicas */}
      <Section title={t('section.replicas')} description={t('section.replicasDesc')}>
        <ReplicaTable replicas={replicas} />
      </Section>

      {/* danger zone */}
      <Section title={t('section.danger')} description={t('section.dangerDesc')} >
        {showDeleteConfirm ? (
          <div className="space-y-3">
            <p className="text-sm text-muted-foreground">{t('delete.confirmDesc')}</p>
            <div className="flex gap-2">
              <Button
                variant="destructive"
                size="sm"
                disabled={deleteMutation.isPending}
                onClick={() => deleteMutation.mutate()}
              >
                {deleteMutation.isPending ? t('delete.deleting') : t('delete.confirm')}
              </Button>
              <Button variant="outline" size="sm" onClick={() => setShowDeleteConfirm(false)}>
                {t('delete.cancel')}
              </Button>
            </div>
          </div>
        ) : (
          <Button
            variant="destructive"
            size="sm"
            onClick={() => setShowDeleteConfirm(true)}
          >
            <Trash2 className="h-4 w-4 mr-1.5" />
            {t('delete.button')}
          </Button>
        )}
      </Section>

      {/* rebuild confirm dialog */}
      {showRebuildConfirm && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm">
          <Card className="w-full max-w-sm shadow-xl">
            <CardHeader>
              <CardTitle>{t('rebuild.confirm')}</CardTitle>
              <CardDescription>{t('rebuild.confirmDesc')}</CardDescription>
            </CardHeader>
            <CardContent className="flex gap-2 justify-end">
              <Button
                variant="default"
                size="sm"
                disabled={rebuildMutation.isPending}
                onClick={() => rebuildMutation.mutate()}
              >
                {rebuildMutation.isPending ? t('rebuild.building') : t('rebuild.confirm')}
              </Button>
              <Button variant="outline" size="sm" onClick={() => setShowRebuildConfirm(false)}>
                {t('rebuild.cancel')}
              </Button>
            </CardContent>
          </Card>
        </div>
      )}
    </div>
  );
}
