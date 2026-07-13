// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState, useRef, useEffect } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate, useParams, Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { templateApi, type TemplateNodeCompat, type TemplateCompatRow } from '@/api/client';
import { ApiError } from '@/lib/api';
import { Card, CardHeader, CardTitle, CardDescription, CardContent } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import { ArrowLeft, RefreshCw, Trash2, ChevronDown, ChevronUp, Copy, Check, AlertTriangle } from 'lucide-react';
import { cn, formatDeleteError, copyToClipboard } from '@/lib/utils';
import { extractTemplateRuntimeConfig, extractTemplateNetworkPolicy } from '@/lib/templateConfig';
import { BoolBadge } from '@/components/ui/typography';

// ── status helpers ────────────────────────────────────────────────────────────

function statusDotClass(status: string) {
  switch (status.toUpperCase()) {
    case 'READY':    return 'bg-cube-ok';
    case 'BUILDING':
    case 'RUNNING':  return 'bg-cube-warn animate-pulse';
    case 'FAILED':   return 'bg-cube-err';
    default:         return 'bg-muted-foreground';
  }
}
function statusTextClass(status: string) {
  switch (status.toUpperCase()) {
    case 'READY':    return 'text-cube-ok';
    case 'BUILDING':
    case 'RUNNING':  return 'text-cube-warn';
    case 'FAILED':   return 'text-cube-err';
    default:         return 'text-muted-foreground';
  }
}
function StatusBadge({ status }: { status: string }) {
  const { t } = useTranslation('templateDetail');
  const tone = (() => {
    switch (status.toUpperCase()) {
      case 'READY':    return 'bg-cube-ok/15 text-cube-ok border-cube-ok/30';
      case 'BUILDING':
      case 'RUNNING':  return 'bg-cube-warn/15 text-cube-warn border-cube-warn/30';
      case 'FAILED':   return 'bg-cube-err/15 text-cube-err border-cube-err/30';
      default:         return 'bg-muted text-muted-foreground border-border';
    }
  })();
  return (
    <span className={cn('inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium', tone)}>
      {t(`status.${status.toLowerCase()}` as 'status.ready', { defaultValue: status })}
    </span>
  );
}

function compatTone(status: string) {
  switch (status.toUpperCase()) {
    case 'OK':      return 'bg-cube-ok/15 text-cube-ok border-cube-ok/30';
    case 'STALE':   return 'bg-destructive/10 text-destructive border-destructive/30';
    case 'UNKNOWN': return 'bg-cube-warn/15 text-cube-warn border-cube-warn/30';
    default:        return 'bg-muted text-muted-foreground border-border';
  }
}

function isStaleCompat(status: string) {
  return status.toUpperCase() === 'STALE';
}

function CompatBadge({ status }: { status: string }) {
  const { t } = useTranslation('templateDetail');
  const normalizedStatus = status.toUpperCase();
  return (
    <span className={cn('inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium', compatTone(normalizedStatus))}>
      {t(`compat.status.${normalizedStatus}` as 'compat.status.OK', { defaultValue: status })}
    </span>
  );
}

// ── copy button ───────────────────────────────────────────────────────────────

function CopyButton({ text, className }: { text: string; className?: string }) {
  const { t } = useTranslation('templateDetail');
  const [copied, setCopied] = useState(false);
  return (
    <button
      onClick={() => {
        copyToClipboard(text, t('copied'));
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      }}
      className={cn('text-muted-foreground/50 hover:text-muted-foreground transition-colors', className)}
      title={t('copy')}
    >
      {copied ? <Check className="h-3 w-3 text-cube-ok" /> : <Copy className="h-3 w-3" />}
    </button>
  );
}

// ── field ─────────────────────────────────────────────────────────────────────

function Field({ label, value, mono, copyable, dim }: {
  label: string; value?: string | null; mono?: boolean; copyable?: boolean; dim?: boolean;
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium">{label}</span>
      <span className={cn(
        'text-sm break-all flex items-center gap-1.5',
        mono && 'font-mono text-sm',
        dim && 'text-muted-foreground',
      )}>
        <span>{value ?? '—'}</span>
        {copyable && value && <CopyButton text={value} />}
      </span>
    </div>
  );
}

// ── copy tooltip wrapper ─────────────────────────────────────────────────────

function CopyableText({ text, display, className }: { text: string; display?: string; className?: string }) {
  const { t } = useTranslation('templateDetail');
  const [copied, setCopied] = useState(false);
  const handleCopy = () => {
    copyToClipboard(text);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };
  return (
    <span
      className={cn('inline-flex items-center gap-1.5 cursor-pointer group', className)}
      onClick={handleCopy}
      title={copied ? t('copied') : t('clickToCopy', { text })}
    >
      <span>{display ?? text}</span>
      <span className="opacity-0 group-hover:opacity-100 transition-opacity">
        {copied ? <Check className="h-3 w-3 text-cube-ok" /> : <Copy className="h-3 w-3 text-muted-foreground/50" />}
      </span>
    </span>
  );
}

// ── section (borderless) ──────────────────────────────────────────────────────

function Section({ title, description, children, danger, className }: {
  title: string; description?: string; children: React.ReactNode; danger?: boolean; className?: string;
}) {
  return (
    <div className={cn('py-6 border-t border-border/70', danger && 'border-destructive/20', className)}>
      <div className="mb-4">
        <h2 className={cn('text-base font-semibold tracking-tight', danger && 'text-destructive')}>{title}</h2>
        {description && <p className="text-xs text-muted-foreground mt-1">{description}</p>}
      </div>
      {children}
    </div>
  );
}

// ── monoblock ─────────────────────────────────────────────────────────────────
// Labeled single-line-or-list display for network rule strings (DNS /
// allowOut / denyOut). Visually matches the env vars block; returns null
// when the value is empty so the calling Section can skip it cleanly.
// Copy button is absolute-positioned inside the pre block, vertically
// centered, so long wrapped text never collides with it (right padding
// reserves a clear column for the icon).

function Monoblock({ label, value }: { label: string; value?: string | null }) {
  if (!value) return null;
  return (
    <div className="flex flex-col gap-1.5">
      <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium">{label}</span>
      <div className="relative">
        <pre className="rounded-md border border-border/50 bg-muted/30 pl-3 pr-10 py-2 font-mono text-xs whitespace-pre-wrap break-all leading-relaxed text-foreground/90">
          {value}
        </pre>
        <CopyButton
          text={value}
          className="absolute top-1/2 -translate-y-1/2 right-2 p-1.5"
        />
      </div>
    </div>
  );
}

// ── progress bar ──────────────────────────────────────────────────────────────

function ProgressBar({ value }: { value: number }) {
  return (
    <div className="h-0.5 w-full rounded-full bg-border overflow-hidden">
      <div
        className="h-full rounded-full bg-cube-warn transition-all duration-500"
        style={{ width: `${Math.min(100, Math.max(0, value))}%` }}
      />
    </div>
  );
}

// ── log viewer ────────────────────────────────────────────────────────────────

function LogViewer({ templateID, buildID }: { templateID: string; buildID: string }) {
  const bottomRef = useRef<HTMLDivElement>(null);
  const { data: logsData, isLoading } = useQuery({
    queryKey: ['template-build-logs', templateID, buildID],
    queryFn: () => templateApi.getBuildLogs(templateID, buildID),
    refetchInterval: 2000,
  });
  const lines = logsData?.lines ?? [];
  useEffect(() => { bottomRef.current?.scrollIntoView({ behavior: 'smooth' }); }, [lines]);
  if (isLoading) return <Skeleton className="h-40 w-full" />;
  return (
    <div className="rounded-md bg-muted/40 border border-border/50 p-3 font-mono text-xs overflow-y-auto max-h-64 space-y-0.5 mt-3">
      {lines.length === 0 && <span className="text-muted-foreground">No logs yet…</span>}
      {lines.map((line, i) => <div key={i} className="break-all leading-relaxed">{line}</div>)}
      <div ref={bottomRef} />
    </div>
  );
}

// ── replica table ─────────────────────────────────────────────────────────────

interface Replica {
  node_id?: string; node_ip?: string; phase?: string; status?: string;
  spec?: string; artifact_id?: string; snapshot_path?: string; last_job_id?: string;
  compat_status?: string;
}

function nodeCompatKey(node?: string | null) {
  return (node ?? '').trim();
}

function compatNodeKey(node: TemplateNodeCompat) {
  return `${node.nodeID}-${node.nodeIP ?? ''}`;
}

function compatNodeLabel(node: TemplateNodeCompat) {
  return node.nodeIP ?? node.nodeID;
}

function findCompatForReplica(replica: Replica, compatNodes: TemplateNodeCompat[]) {
  const replicaNodeID = nodeCompatKey(replica.node_id);
  const replicaNodeIP = nodeCompatKey(replica.node_ip);
  return compatNodes.find((node) => {
    const nodeID = nodeCompatKey(node.nodeID);
    const nodeIP = nodeCompatKey(node.nodeIP);
    return (!!replicaNodeID && replicaNodeID === nodeID) || (!!replicaNodeIP && replicaNodeIP === nodeIP);
  });
}

function versionDeltaItems(node: TemplateNodeCompat) {
  return [
    {
      key: 'guestImage',
      bound: node.boundGuestImageVersion,
      current: node.currentGuestImageVersion,
    },
    {
      key: 'agent',
      bound: node.boundAgentVersion,
      current: node.currentAgentVersion,
    },
    {
      key: 'kernel',
      bound: node.boundKernelVersion,
      current: node.currentKernelVersion,
    },
  ].filter((item) => item.bound && item.current);
}

function VersionDeltaList({ node }: { node: TemplateNodeCompat }) {
  const { t } = useTranslation('templateDetail');
  const deltas = versionDeltaItems(node);
  if (deltas.length === 0) {
    return <span className="text-xs text-muted-foreground">—</span>;
  }
  return (
    <div className="space-y-1">
      {deltas.map((item) => {
        const changed = item.bound !== item.current;
        return (
          <div key={item.key} className="text-xs">
            <span className="text-muted-foreground">{t(`compat.components.${item.key}` as 'compat.components.guestImage')}</span>
            <span className={cn('ml-1 font-mono', changed ? 'text-destructive' : 'text-muted-foreground')}>
              {item.bound ?? '—'} -&gt; {item.current ?? '—'}
            </span>
          </div>
        );
      })}
    </div>
  );
}

function CompatNodeCard({ node, variant = 'default' }: {
  node: TemplateNodeCompat;
  variant?: 'default' | 'warning';
}) {
  const warning = variant === 'warning';
  return (
    <div className={cn(
      'rounded-md border p-3',
      warning ? 'border-destructive/15 bg-background/60' : 'border-border/60 bg-muted/20',
    )}>
      <div className={cn('flex items-center justify-between gap-3', warning && 'mb-2')}>
        <div className="min-w-0">
          <p className={cn('font-mono font-medium text-foreground', warning ? 'text-xs' : 'text-sm')}>
            {compatNodeLabel(node)}
          </p>
          {!warning && node.nodeIP && node.nodeIP !== node.nodeID && (
            <p className="font-mono text-xs text-muted-foreground">{node.nodeID}</p>
          )}
        </div>
        <CompatBadge status={node.compatStatus} />
      </div>
      <div className={cn(!warning && 'mt-3')}>
        <VersionDeltaList node={node} />
      </div>
    </div>
  );
}

function ReplicaTable({ replicas, compatNodes }: { replicas: Replica[]; compatNodes: TemplateNodeCompat[] }) {
  const { t } = useTranslation('templateDetail');
  if (replicas.length === 0) {
    return <p className="text-sm text-muted-foreground">{t('empty.replicas')}</p>;
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm" style={{ minWidth: '1040px' }}>
        <thead>
          <tr className="border-b border-border/50">
            {[t('fields.node'), t('fields.phase'), t('fields.compat'), t('fields.spec'), t('fields.artifactID'), t('fields.lastJob')].map(h => (
              <th key={h} className="tbl-th pl-0 pr-8 py-2">{h}</th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-border/30">
          {replicas.map((r, i) => {
            const nodeCompat = findCompatForReplica(r, compatNodes);
            const compatStatus = nodeCompat?.compatStatus ?? r.compat_status ?? 'UNKNOWN';
            return (
              <tr key={i} className="hover:bg-cube-ok/5 transition-colors">
                <td className="py-3 pr-8 text-sm font-medium text-num whitespace-nowrap">{r.node_ip ?? r.node_id ?? '—'}</td>
                <td className="py-3 pr-8 whitespace-nowrap">
                  <span className="inline-flex items-center gap-1.5">
                    <span className={cn('h-1.5 w-1.5 rounded-full', statusDotClass(r.phase ?? r.status ?? ''))} />
                    <span className={cn('text-sm', statusTextClass(r.phase ?? r.status ?? ''))}>{r.phase ?? r.status ?? '—'}</span>
                  </span>
                </td>
                <td className="py-3 pr-8 whitespace-nowrap">
                  <CompatBadge status={compatStatus} />
                </td>
                <td className="py-3 pr-8 font-mono text-sm text-muted-foreground whitespace-nowrap">{r.spec ?? '—'}</td>
                <td className="py-3 pr-8 font-mono text-sm text-muted-foreground">
                  {r.artifact_id
                    ? <CopyableText
                        text={r.artifact_id}
                        display={r.artifact_id.slice(0, 28) + '…'}
                        className="font-mono text-sm text-muted-foreground"
                      />
                    : '—'}
                </td>
                <td className="py-3 font-mono text-sm text-muted-foreground">
                  {r.last_job_id
                    ? <CopyableText
                        text={r.last_job_id}
                        display={r.last_job_id.slice(0, 28) + '…'}
                        className="font-mono text-sm text-muted-foreground"
                      />
                    : '—'}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function CompatWarning({ row, onRebuild, disabled }: {
  row: TemplateCompatRow;
  onRebuild: () => void;
  disabled: boolean;
}) {
  const { t } = useTranslation('templateDetail');
  const staleNodes = row.nodes.filter((node) => isStaleCompat(node.compatStatus));
  if (staleNodes.length === 0) return null;
  return (
    <div className="mt-4 rounded-lg border border-destructive/30 bg-destructive/5 p-4">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="flex items-center gap-2 text-destructive">
            <AlertTriangle className="h-4 w-4 shrink-0" />
            <p className="text-sm font-semibold">{t('compat.staleTitle')}</p>
          </div>
          <p className="mt-1 text-xs text-muted-foreground">
            {t('compat.staleDesc', { count: staleNodes.length })}
          </p>
        </div>
        <Button variant="destructive" size="sm" disabled={disabled} onClick={onRebuild} className="shrink-0">
          <RefreshCw className={cn('h-4 w-4 mr-1.5', disabled && 'animate-spin')} />
          {t('rebuild.button')}
        </Button>
      </div>
      <div className="mt-4 space-y-3">
        {staleNodes.map((node) => (
          <CompatNodeCard key={compatNodeKey(node)} node={node} variant="warning" />
        ))}
      </div>
    </div>
  );
}

function CompatSection({ row }: { row?: TemplateCompatRow }) {
  const { t } = useTranslation('templateDetail');
  if (!row) {
    return <p className="text-sm text-muted-foreground">{t('compat.loading')}</p>;
  }
  if (row.nodes.length === 0) {
    return <p className="text-sm text-muted-foreground">{t('compat.empty')}</p>;
  }
  return (
    <div className="space-y-3">
      {row.nodes.map((node) => (
        <CompatNodeCard key={compatNodeKey(node)} node={node} />
      ))}
    </div>
  );
}

function shortImage(imageInfo?: string | null) {
  if (!imageInfo) return null;
  const i = imageInfo.indexOf('@');
  return i > 0 ? imageInfo.slice(0, i) : imageInfo;
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

  // ensure list cache is warm for imageInfo / createdAt
  useQuery({ queryKey: ['templates'], queryFn: () => templateApi.list(), staleTime: 30_000 });

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['template', templateID],
    queryFn: () => templateApi.get(templateID!),
    enabled: !!templateID,
    refetchInterval: activeBuildID ? 3000 : false,
  });

  const { data: compat } = useQuery({
    queryKey: ['templates', 'compat'],
    queryFn: templateApi.compat,
    enabled: !!templateID,
    refetchInterval: 30_000,
  });

  const cachedSummary = qc.getQueryData<Array<{
    templateID: string; status: string; imageInfo?: string | null; createdAt?: string | null; jobID?: string | null;
  }>>(['templates'])?.find(t => t.templateID === templateID);
  const cachedStatus = cachedSummary?.status?.toUpperCase();

  useEffect(() => {
    if (activeBuildID) return;
    const jobID = data?.jobID ?? cachedSummary?.jobID;
    if (!jobID) return;
    const status = (data?.status ?? cachedSummary?.status)?.toUpperCase();
    if (status === 'RUNNING' || status === 'PENDING' || status === 'CREATING' || status === 'BUILDING') {
      setActiveBuildID(jobID);
      setShowLogs(true);
    }
  }, [activeBuildID, cachedSummary, data?.jobID, data?.status]);

  const { data: buildStatus } = useQuery({
    queryKey: ['template-build-status', templateID, activeBuildID],
    queryFn: () => templateApi.getBuildStatus(templateID!, activeBuildID!),
    enabled: !!activeBuildID,
    refetchInterval: 2000,
  });

  useEffect(() => {
    if (!buildStatus) return;
    const s = (buildStatus as { status?: string }).status?.toUpperCase();
    if (s === 'READY' || s === 'FAILED') {
      setActiveBuildID(null);
      qc.invalidateQueries({ queryKey: ['template', templateID] });
      qc.invalidateQueries({ queryKey: ['templates', 'compat'] });
    }
  }, [buildStatus, templateID, qc]);

  const rebuildMutation = useMutation({
    mutationFn: () => templateApi.rebuild(templateID!),
    onSuccess: (job) => {
      const j = job as { jobID?: string };
      if (j.jobID) setActiveBuildID(j.jobID);
      setShowRebuildConfirm(false);
      setShowLogs(true);
      qc.invalidateQueries({ queryKey: ['templates', 'compat'] });
    },
  });

  const deleteMutation = useMutation({
    mutationFn: () => templateApi.remove(templateID!),
    onSuccess: () => navigate('/templates'),
  });

  // ── loading ──
  if (isLoading) {
    return (
      <div className="px-6 py-8 space-y-6">
        <Skeleton className="h-5 w-40" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }

  // ── error / 404 ──
  const is404 = isError && error instanceof ApiError && error.status === 404;
  const isBuilding404 = is404 && (cachedStatus === 'RUNNING' || cachedStatus === 'BUILDING');
  const isFailed404   = is404 && cachedStatus === 'FAILED';

  if (isError || !data) {
    return (
      <div className="px-6 py-8">
        <Link to="/templates" className="flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground mb-6">
          <ArrowLeft className="h-4 w-4" /> {t('backToTemplates')}
        </Link>
        {isBuilding404 ? (
          <div className="space-y-2">
            <p className="text-sm text-muted-foreground">{t('building')}</p>
            <Button variant="outline" size="sm" onClick={() => window.location.reload()}>{t('refresh')}</Button>
          </div>
        ) : isFailed404 ? (
          <div className="space-y-3">
            <p className="text-sm text-destructive">{t('buildFailed')}</p>
            <p className="text-xs text-muted-foreground">{t('buildFailedDeleteHint', { defaultValue: '该模板构建失败，无法查看详情，但你可以将其删除。' })}</p>
            {showDeleteConfirm ? (
              <div className="space-y-3">
                <p className="text-sm text-muted-foreground">{t('delete.confirmDesc')}</p>
                <div className="flex gap-2">
                  <Button variant="destructive" size="sm" disabled={deleteMutation.isPending} onClick={() => deleteMutation.mutate()}>
                    {deleteMutation.isPending ? t('delete.deleting') : t('delete.confirm')}
                  </Button>
                  <Button variant="outline" size="sm" onClick={() => setShowDeleteConfirm(false)}>{t('delete.cancel')}</Button>
                </div>
                {deleteMutation.isError && <p className="text-xs text-destructive">{formatDeleteError(deleteMutation.error)}</p>}
              </div>
            ) : (
              <Button variant="destructive" size="sm" onClick={() => setShowDeleteConfirm(true)}>
                <Trash2 className="h-4 w-4 mr-1.5" />{t('delete.button')}
              </Button>
            )}
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">{t('notFound')}</p>
        )}
      </div>
    );
  }

  const replicas = (data.replicas ?? []) as Replica[];
  const isBuilding = !!activeBuildID || data.status?.toUpperCase() === 'BUILDING';
  const buildProgress = (buildStatus as { progress?: number } | undefined)?.progress ?? 0;
  const cfg = extractTemplateRuntimeConfig(data.createRequest);
  const imgShort = shortImage(cachedSummary?.imageInfo ?? (data as { imageInfo?: string }).imageInfo);
  const createdAt = cachedSummary?.createdAt ?? (data as { createdAt?: string }).createdAt;
  const status = data.status ?? 'UNKNOWN';
  const compatRow = compat?.templates.find((row) => row.templateID === templateID);
  const compatStatus = compatRow?.overall ?? 'UNKNOWN';
  const compatNodes = compatRow?.nodes ?? [];
  const isStale = isStaleCompat(compatStatus);
  const headerAccentClass = isStale ? 'border-destructive' : 'border-cube-ok';

  return (
    <div className="px-6 py-8">

      {/* back */}
      <Link to="/templates" className="inline-flex items-center gap-1.5 text-xs text-muted-foreground hover:text-foreground mb-8 transition-colors">
        <ArrowLeft className="h-3.5 w-3.5" /> {t('backToTemplates')}
      </Link>

      {/* ── hero header ── */}
      <div className="flex items-start justify-between gap-6 pb-6 border-b border-border/50">
        {/* left: id + meta */}
        <div className={cn('min-w-0 space-y-2 border-l-[3px] pl-3', headerAccentClass)}>
          <div className="flex items-center gap-1.5">
            <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium">{t('templateId')}</span>
          </div>
          <div className="flex items-center gap-2">
            <h1 className="text-lg font-semibold font-mono tracking-tight truncate">{data.templateID}</h1>
            <CopyButton text={data.templateID} />
          </div>
          {imgShort && (
            <div className="flex items-center gap-1.5 group">
              <p
                className="text-xs font-mono text-muted-foreground truncate max-w-lg cursor-pointer hover:text-foreground transition-colors"
                title={imgShort}
                onClick={() => copyToClipboard(imgShort, t('imageCopied'))}
              >
                {imgShort}
              </p>
              <CopyButton text={imgShort} className="opacity-0 group-hover:opacity-100" />
            </div>
          )}
          {createdAt && (
            <p className="text-xs text-muted-foreground/60">
              {t('createdAt')} {new Date(createdAt).toLocaleString(undefined, { year: 'numeric', month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })}
            </p>
          )}
        </div>

        {/* right: status kpi strip — 竖线分隔，无边框格子 */}
        <div className={cn('flex items-stretch shrink-0 divide-x', isStale ? 'divide-destructive/20' : 'divide-cube-ok/20')}>
          {[
            { label: t('fields.status'), content: (
              <span className="inline-flex items-center gap-1.5">
                <span className={cn('h-2 w-2 rounded-full', statusDotClass(status))} />
                <span className={cn('text-sm font-semibold', statusTextClass(status))}>{t(`status.${status.toLowerCase()}` as 'status.ready', { defaultValue: status })}</span>
              </span>
            )},
            { label: t('fields.compat'), content: <CompatBadge status={compatStatus} /> },
            { label: t('fields.version'), content: <span className="text-base font-semibold text-num">{data.version ?? '—'}</span> },
            { label: 'Replicas', content: <span className="text-base font-semibold text-num">{replicas.length}</span> },
          ].map(({ label, content }) => (
            <div key={label} className="px-5 flex flex-col gap-1 first:pl-0 last:pr-0">
              <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium">{label}</span>
              {content}
            </div>
          ))}
        </div>
      </div>

      {compatRow && (
        <CompatWarning
          row={compatRow}
          disabled={isBuilding || rebuildMutation.isPending}
          onRebuild={() => setShowRebuildConfirm(true)}
        />
      )}

      {/* ── build progress ── */}
      {isBuilding && (
        <div className="py-4 border-b border-border/50 space-y-2">
          <div className="flex justify-between text-xs text-muted-foreground">
            <span className="flex items-center gap-1.5">
              <span className="h-1.5 w-1.5 rounded-full bg-cube-warn animate-pulse" />
              {t('rebuild.progress', { progress: buildProgress })}
            </span>
            <button className="flex items-center gap-1 hover:text-foreground transition-colors" onClick={() => setShowLogs(v => !v)}>
              {showLogs ? <ChevronUp className="h-3 w-3" /> : <ChevronDown className="h-3 w-3" />}
              {showLogs ? t('rebuild.hideLogs') : t('rebuild.viewLogs')}
            </button>
          </div>
          <ProgressBar value={buildProgress} />
          {showLogs && activeBuildID && <LogViewer templateID={templateID!} buildID={activeBuildID} />}
        </div>
      )}

      {/* ── basic info ── */}
      <Section title={t('section.info')} description={t('section.infoDesc')}>
        {/* 规格分组 */}
        {(cfg?.cpu || cfg?.mem || cfg?.writableLayerSize) && (
          <>
            <div className="flex items-center gap-2 mb-3">
              <div className="h-3.5 w-0.5 rounded-full bg-blue-400/70" />
              {/* specs */}<span className="text-sm font-semibold text-foreground/80">{t('specs')}</span>
            </div>
            <div className="grid grid-cols-2 sm:grid-cols-3 gap-x-8 gap-y-5 mb-6">
              {cfg.cpu && <Field label="CPU" value={cfg.cpu} mono />}
              {cfg.mem && <Field label={t('fields.memory')} value={cfg.mem} mono />}
              {cfg.writableLayerSize && <Field label={t('fields.writableLayerSize')} value={cfg.writableLayerSize} mono />}
            </div>
          </>
        )}

        {/* 属性分组 */}
        <div className="flex items-center gap-2 mb-3">
          <div className="h-3.5 w-0.5 rounded-full bg-blue-400/70" />
          {/* attributes */}<span className="text-sm font-semibold text-foreground/80">{t('attributes')}</span>
        </div>
        <div className="grid grid-cols-2 sm:grid-cols-3 gap-x-8 gap-y-5">
          <Field label={t('fields.templateID')} value={data.templateID} mono copyable />
          <Field label={t('fields.instanceType')} value={data.instanceType ?? '—'} />
          {cfg?.exposedPorts && <Field label={t('exposedPorts')} value={cfg.exposedPorts} mono />}
          {cfg?.probePath && <Field label={t('probePath')} value={`${cfg.probePath} :${cfg.probePort}`} mono dim />}
          {createdAt && <Field label={t('createdAt')} value={new Date(createdAt).toLocaleString()} dim />}
        </div>

        {cfg?.env && (
          <div className="mt-5 flex flex-col gap-1.5">
            <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium">{t('fields.env')}</span>
            <pre className="rounded-md border border-border/50 bg-muted/30 px-3 py-2 font-mono text-xs whitespace-pre-wrap break-all leading-relaxed text-foreground/90">
              {cfg.env}
            </pre>
          </div>
        )}

        {/* error */}
        {data.lastError && (
          <div className="mt-5 rounded-md border border-destructive/30 bg-destructive/5 p-3">
            <p className="text-xs uppercase tracking-wider font-medium text-destructive/70 mb-1.5">{t('fields.lastError')}</p>
            <p className="font-mono text-xs break-all text-destructive/80 leading-relaxed">{data.lastError}</p>
          </div>
        )}
      </Section>

      {/* ── network policy ── */}
      <Section title={t('section.network')} description={t('section.networkDesc')}>
        {(() => {
          const policy = extractTemplateNetworkPolicy(data.createRequest);
          const netType = data.networkType ?? null;
          const internet = data.allowInternetAccess ?? null;
          const hasAny = !!(netType || internet != null || policy.dns || policy.allowOut || policy.denyOut);
          if (!hasAny) {
            return (
              <p className="text-sm text-muted-foreground">{t('empty.noNetworkPolicy')}</p>
            );
          }
          return (
            <div className="space-y-5">
              <div className="grid grid-cols-2 gap-x-8 gap-y-5">
                <div className="flex flex-col gap-1.5">
                  <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium">
                    {t('fields.networkType')}
                  </span>
                  <div>
                    {netType
                      ? <span className="chip-net">{netType}</span>
                      : <span className="text-sm text-muted-foreground">—</span>}
                  </div>
                </div>
                <div className="flex flex-col gap-1.5">
                  <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium">
                    {t('fields.internetAccess')}
                  </span>
                  <div>
                    <BoolBadge
                      value={internet}
                      trueLabel={t('network.allowed')}
                      falseLabel={t('network.blocked')}
                    />
                  </div>
                </div>
              </div>
              <Monoblock label={t('fields.dns')} value={policy.dns} />
              <Monoblock label={t('fields.allowOut')} value={policy.allowOut} />
              <Monoblock label={t('fields.denyOut')} value={policy.denyOut} />
            </div>
          );
        })()}
      </Section>

      {/* ── replicas ── */}
      <Section title={t('section.replicas')} description={t('section.replicasDesc')}>
        <ReplicaTable replicas={replicas} compatNodes={compatNodes} />
      </Section>

      {/* ── compatibility ── */}
      <Section title={t('section.compat')} description={t('section.compatDesc')}>
        <CompatSection row={compatRow} />
      </Section>

      {/* ── rebuild action ── */}
      <Section title={t('rebuild.button')} description={t('rebuild.confirmDesc')} className="border-border/70">
        <Button
          variant="outline"
          size="sm"
          disabled={isBuilding || rebuildMutation.isPending}
          onClick={() => setShowRebuildConfirm(true)}
        >
          <RefreshCw className={cn('h-4 w-4 mr-1.5', isBuilding && 'animate-spin')} />
          {isBuilding ? t('rebuild.building') : t('rebuild.button')}
        </Button>
      </Section>

      {/* ── danger zone ── */}
      <Section title={t('section.danger')} description={t('section.dangerDesc')} danger>
        {showDeleteConfirm ? (
          <div className="space-y-3">
            <p className="text-sm text-muted-foreground">{t('delete.confirmDesc')}</p>
            <div className="flex gap-2">
              <Button variant="destructive" size="sm" disabled={deleteMutation.isPending} onClick={() => deleteMutation.mutate()}>
                {deleteMutation.isPending ? t('delete.deleting') : t('delete.confirm')}
              </Button>
              <Button variant="outline" size="sm" onClick={() => setShowDeleteConfirm(false)}>{t('delete.cancel')}</Button>
            </div>
            {deleteMutation.isError && <p className="text-xs text-destructive">{formatDeleteError(deleteMutation.error)}</p>}
          </div>
        ) : (
          <Button variant="destructive" size="sm" onClick={() => setShowDeleteConfirm(true)}>
            <Trash2 className="h-4 w-4 mr-1.5" />{t('delete.button')}
          </Button>
        )}
      </Section>

      {/* ── rebuild confirm dialog ── */}
      {showRebuildConfirm && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 backdrop-blur-sm">
          <Card className="w-full max-w-sm shadow-2xl border-border/60">
            <CardHeader className="flex-col items-start gap-1">
              <CardTitle className="text-base whitespace-nowrap">{t('rebuild.confirm')}</CardTitle>
              <CardDescription>{t('rebuild.confirmDesc')}</CardDescription>
            </CardHeader>
            <CardContent className="flex gap-2 justify-end">
              <Button variant="default" size="sm" className="whitespace-nowrap" disabled={rebuildMutation.isPending} onClick={() => rebuildMutation.mutate()}>
                {rebuildMutation.isPending ? t('rebuild.building') : t('rebuild.confirm')}
              </Button>
              <Button variant="outline" size="sm" className="whitespace-nowrap" onClick={() => setShowRebuildConfirm(false)}>{t('rebuild.cancel')}</Button>
            </CardContent>
          </Card>
        </div>
      )}
    </div>
  );
}
