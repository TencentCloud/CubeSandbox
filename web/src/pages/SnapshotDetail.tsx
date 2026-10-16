// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { snapshotApi, type SnapshotReplica } from '@/api/client';
import { ApiError } from '@/lib/api';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import { ArrowLeft, Check, Copy, Trash2 } from 'lucide-react';
import { cn, formatArtifactBytes, formatDeleteError, copyToClipboard } from '@/lib/utils';

// ── status helpers ────────────────────────────────────────────────────────────

function statusDotClass(status: string) {
  switch (status.toUpperCase()) {
    case 'READY':
      return 'bg-cube-ok';
    case 'CREATING':
      return 'bg-cube-warn animate-pulse';
    case 'FAILED':
      return 'bg-cube-err';
    default:
      return 'bg-muted-foreground';
  }
}

function statusTextClass(status: string) {
  switch (status.toUpperCase()) {
    case 'READY':
      return 'text-cube-ok';
    case 'CREATING':
      return 'text-cube-warn';
    case 'FAILED':
      return 'text-cube-err';
    default:
      return 'text-muted-foreground';
  }
}

// ── shared pieces ─────────────────────────────────────────────────────────────

function CopyButton({ text, className }: { text: string; className?: string }) {
  const { t } = useTranslation('snapshotDetail');
  const [copied, setCopied] = useState(false);
  return (
    <button
      onClick={() => {
        copyToClipboard(text, t('copied'));
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      }}
      className={cn(
        'text-muted-foreground/50 hover:text-muted-foreground transition-colors',
        className,
      )}
      title={t('copy')}
    >
      {copied ? <Check className="h-3 w-3 text-cube-ok" /> : <Copy className="h-3 w-3" />}
    </button>
  );
}

function CopyableText({
  text,
  display,
  className,
}: {
  text: string;
  display?: string;
  className?: string;
}) {
  const { t } = useTranslation('snapshotDetail');
  const [copied, setCopied] = useState(false);
  return (
    <span
      className={cn('inline-flex items-center gap-1.5 cursor-pointer group', className)}
      onClick={() => {
        copyToClipboard(text, t('copied'));
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      }}
      title={copied ? t('copied') : t('clickToCopy', { text })}
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

function Section({
  title,
  description,
  danger,
  children,
}: {
  title: string;
  description?: string;
  danger?: boolean;
  children: React.ReactNode;
}) {
  return (
    <div
      className={cn(
        'py-6 border-b border-border/50 last:border-b-0',
        danger && 'border-destructive/20',
      )}
    >
      <div className="flex flex-col gap-1 mb-4">
        <span
          className={cn('text-base font-bold', danger ? 'text-destructive' : 'text-foreground')}
        >
          {title}
        </span>
        {description && <span className="text-sm text-muted-foreground/70">{description}</span>}
      </div>
      {children}
    </div>
  );
}

function Field({
  label,
  value,
  mono,
  copyable,
  dim,
}: {
  label: string;
  value?: React.ReactNode;
  mono?: boolean;
  copyable?: boolean;
  dim?: boolean;
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-semibold">
        {label}
      </span>
      <span
        className={cn(
          'text-base font-medium break-all flex items-center gap-1.5',
          mono && 'font-mono text-base',
          dim && 'text-muted-foreground font-normal',
        )}
      >
        {value ?? '—'}
        {copyable && typeof value === 'string' && value && <CopyButton text={value} />}
      </span>
    </div>
  );
}

// ── single replica summary ────────────────────────────────────────────────────

function SnapshotReplicaSummary({ replica }: { replica: SnapshotReplica }) {
  const { t } = useTranslation('snapshotDetail');
  const versions = [
    replica.guestImageVersion && `guest-image: ${replica.guestImageVersion}`,
    replica.shimVersion && `shim: ${replica.shimVersion}`,
    replica.agentVersion && `agent: ${replica.agentVersion}`,
    replica.kernelVersion && `kernel: ${replica.kernelVersion}`,
  ].filter(Boolean);
  return (
    <div className="grid grid-cols-2 sm:grid-cols-3 gap-x-8 gap-y-5">
      <Field
        label={t('fields.node')}
        value={
          replica.nodeID || replica.nodeIP ? (
            <Link
              to={`/nodes/${encodeURIComponent(replica.nodeID || replica.nodeIP || '')}`}
              className="font-mono text-sm text-primary hover:underline"
            >
              {replica.nodeIP ?? replica.nodeID}
            </Link>
          ) : null
        }
      />
      <Field
        label={t('fields.phase')}
        value={
          <span className="inline-flex items-center gap-1.5">
            <span className={cn('h-2 w-2 rounded-full', statusDotClass(replica.phase ?? ''))} />
            <span className={cn('text-base font-medium', statusTextClass(replica.phase ?? ''))}>
              {replica.phase ?? '—'}
            </span>
          </span>
        }
      />
      <Field label={t('fields.spec')} value={replica.spec ?? '—'} mono />
      <Field
        label={t('fields.compat')}
        value={
          <span
            className={cn(
              'inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium',
              replica.compatStatus?.toUpperCase() === 'OK'
                ? 'bg-cube-ok/15 text-cube-ok border-cube-ok/30'
                : 'bg-muted text-muted-foreground border-border',
            )}
          >
            {replica.compatStatus ?? '—'}
          </span>
        }
      />
      {versions.length > 0 && (
        <div className="flex flex-col gap-1.5">
          <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-semibold">
            {t('fields.versions')}
          </span>
          <span className="flex flex-col gap-0.5 font-mono text-sm text-muted-foreground">
            {versions.map((v) => (
              <span key={v}>{v}</span>
            ))}
          </span>
        </div>
      )}
      {replica.lastJobID && (
        <Field
          label={t('fields.lastJob')}
          value={
            <CopyableText
              text={replica.lastJobID}
              className="font-mono text-sm break-all text-muted-foreground"
            />
          }
        />
      )}
    </div>
  );
}

// ── replica table ─────────────────────────────────────────────────────────────

function SnapshotReplicaTable({ replicas }: { replicas: SnapshotReplica[] }) {
  const { t } = useTranslation('snapshotDetail');
  if (replicas.length === 0) {
    return <p className="text-sm text-muted-foreground">{t('empty.replicas')}</p>;
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm" style={{ minWidth: '980px' }}>
        <thead>
          <tr className="border-b border-border/50">
            {[
              t('fields.node'),
              t('fields.phase'),
              t('fields.spec'),
              t('fields.compat'),
              t('fields.versions'),
              t('fields.lastJob'),
            ].map((h) => (
              <th key={h} className="tbl-th pl-0 pr-8 py-2">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-border/30">
          {replicas.map((r, i) => (
            <tr key={i} className="hover:bg-cube-ok/5 transition-colors">
              <td className="py-3 pr-8 text-sm font-medium text-num whitespace-nowrap">
                {r.nodeIP ?? r.nodeID ?? '—'}
              </td>
              <td className="py-3 pr-8 whitespace-nowrap">
                <span className="inline-flex items-center gap-1.5">
                  <span className={cn('h-1.5 w-1.5 rounded-full', statusDotClass(r.phase ?? ''))} />
                  <span className={cn('text-sm', statusTextClass(r.phase ?? ''))}>
                    {r.phase ?? '—'}
                  </span>
                </span>
              </td>
              <td className="py-3 pr-8 font-mono text-sm text-muted-foreground whitespace-nowrap">
                {r.spec ?? '—'}
              </td>
              <td className="py-3 pr-8 whitespace-nowrap">
                <span
                  className={cn(
                    'inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium',
                    r.compatStatus?.toUpperCase() === 'OK'
                      ? 'bg-cube-ok/15 text-cube-ok border-cube-ok/30'
                      : 'bg-muted text-muted-foreground border-border',
                  )}
                >
                  {r.compatStatus ?? '—'}
                </span>
              </td>
              <td className="py-3 pr-8 text-xs text-muted-foreground whitespace-nowrap">
                <div className="flex flex-col gap-0.5 font-mono">
                  {r.guestImageVersion && <span>guest-image: {r.guestImageVersion}</span>}
                  {r.shimVersion && <span>shim: {r.shimVersion}</span>}
                  {r.agentVersion && <span>agent: {r.agentVersion}</span>}
                  {r.kernelVersion && <span>kernel: {r.kernelVersion}</span>}
                </div>
              </td>
              <td className="py-3 font-mono text-sm text-muted-foreground">
                {r.lastJobID ? (
                  <CopyableText
                    text={r.lastJobID}
                    display={r.lastJobID.slice(0, 20) + '…'}
                    className="font-mono text-sm text-muted-foreground"
                  />
                ) : (
                  '—'
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ── main page ─────────────────────────────────────────────────────────────────

export default function SnapshotDetailPage() {
  const { snapshotID } = useParams<{ snapshotID: string }>();
  const { t } = useTranslation('snapshotDetail');
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [showDeleteConfirm, setShowDeleteConfirm] = useState(false);

  const {
    data: snap,
    isLoading,
    isError,
    error,
  } = useQuery({
    queryKey: ['snapshot', snapshotID],
    queryFn: () => snapshotApi.get(snapshotID!),
    refetchInterval: 10_000,
  });

  const deleteMutation = useMutation({
    mutationFn: () => snapshotApi.remove(snapshotID!),
    onSuccess: async () => {
      // Prefix-match invalidates every paginated list entry; no manual cache write needed.
      await qc.invalidateQueries({ queryKey: ['snapshots'] });
      navigate('/templates?tab=snapshots');
    },
  });

  if (isLoading) {
    return (
      <div className="px-6 py-8 space-y-6">
        <Skeleton className="h-5 w-40" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }

  // 404 is authoritative (deleted elsewhere) even when stale data is retained.
  const is404 = isError && error instanceof ApiError && error.status === 404;
  if (is404 || (!isError && !snap)) {
    return (
      <div className="px-6 py-8">
        <Link
          to="/templates?tab=snapshots"
          className="flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground mb-6"
        >
          <ArrowLeft className="h-4 w-4" /> {t('backToSnapshots')}
        </Link>
        <p className="text-sm text-muted-foreground">{t('notFound')}</p>
      </div>
    );
  }

  if (isError) {
    return (
      <div className="px-6 py-8">
        <Link
          to="/templates?tab=snapshots"
          className="flex items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground mb-6"
        >
          <ArrowLeft className="h-4 w-4" /> {t('backToSnapshots')}
        </Link>
        <p className="text-sm text-muted-foreground">{t('loadError')}</p>
      </div>
    );
  }

  const replicas = snap.replicas ?? [];
  const refs = snap.runtimeRefSandboxes ?? [];
  const status = snap.status ?? 'UNKNOWN';
  const headerAccentClass =
    status.toUpperCase() === 'FAILED' ? 'border-cube-err' : 'border-cube-ok';

  return (
    <div className="px-6 py-8">
      {/* back */}
      <Link
        to="/templates?tab=snapshots"
        className="inline-flex items-center gap-1.5 text-xs text-muted-foreground hover:text-foreground mb-8 transition-colors"
      >
        <ArrowLeft className="h-3.5 w-3.5" /> {t('backToSnapshots')}
      </Link>

      {/* ── hero header ── */}
      <div className="flex items-start justify-between gap-6 pb-6 border-b border-border/50">
        <div className={cn('min-w-0 space-y-2 border-l-[3px] pl-3', headerAccentClass)}>
          <div className="flex items-center gap-1.5">
            <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium">
              {t('snapshotId')}
            </span>
          </div>
          <div className="flex items-center gap-2">
            <h1 className="text-xl font-bold font-mono tracking-tight truncate">
              {snap.snapshotID}
            </h1>
            <CopyButton text={snap.snapshotID} />
          </div>
          {snap.displayName && (
            <p className="text-base font-medium text-foreground/80 truncate max-w-lg">
              {snap.displayName}
            </p>
          )}
          {snap.createdAt && (
            <p className="text-sm text-muted-foreground/60">
              {t('createdAt')}{' '}
              {new Date(snap.createdAt).toLocaleString(undefined, {
                year: 'numeric',
                month: 'numeric',
                day: 'numeric',
                hour: '2-digit',
                minute: '2-digit',
              })}
            </p>
          )}
        </div>

        {/* right: status kpi strip */}
        <div
          className={cn(
            'flex items-stretch shrink-0 divide-x',
            status.toUpperCase() === 'FAILED' ? 'divide-cube-err/20' : 'divide-cube-ok/20',
          )}
        >
          {[
            {
              label: t('fields.status'),
              content: (
                <span className="inline-flex items-center gap-1.5">
                  <span className={cn('h-2 w-2 rounded-full', statusDotClass(status))} />
                  <span className={cn('text-base font-bold', statusTextClass(status))}>
                    {status}
                  </span>
                </span>
              ),
            },
            {
              label: t('fields.backend'),
              content: (
                <span className="text-lg font-bold text-num">{snap.storageBackend ?? '—'}</span>
              ),
            },
            {
              label: t('fields.version'),
              content: <span className="text-lg font-bold text-num">{snap.version ?? '—'}</span>,
            },
            {
              label: t('fields.rootfsSize'),
              content: (
                <span className="text-lg font-bold text-num">
                  {snap.rootfsSizeBytesAtSnapshot
                    ? formatArtifactBytes(snap.rootfsSizeBytesAtSnapshot)
                    : '—'}
                </span>
              ),
            },
          ].map(({ label, content }) => (
            <div key={label} className="px-5 flex flex-col gap-1 first:pl-0 last:pr-0">
              <span className="text-xs uppercase tracking-wider text-muted-foreground/70 font-medium">
                {label}
              </span>
              {content}
            </div>
          ))}
        </div>
      </div>

      {/* ── origin ── */}
      <Section title={t('section.origin')} description={t('section.originDesc')}>
        <div className="grid grid-cols-2 sm:grid-cols-3 gap-x-8 gap-y-5">
          <Field
            label={t('fields.originSandbox')}
            value={
              snap.originSandboxID ? (
                <Link
                  to={`/sandboxes/${snap.originSandboxID}`}
                  className="font-mono text-sm text-primary hover:underline"
                >
                  {snap.originSandboxID}
                </Link>
              ) : null
            }
            copyable={false}
          />
          <Field
            label={t('fields.originNode')}
            value={
              snap.originNodeID ? (
                <Link
                  to={`/nodes/${encodeURIComponent(snap.originNodeID)}`}
                  className="font-mono text-sm text-primary hover:underline"
                >
                  {snap.originNodeID}
                </Link>
              ) : null
            }
          />
          <Field label={t('fields.instanceType')} value={snap.instanceType ?? '—'} />
          <Field label={t('fields.snapshotID')} value={snap.snapshotID} mono copyable />
          <Field label={t('fields.displayName')} value={snap.displayName || null} />
          <Field
            label={t('fields.remoteStatus')}
            value={snap.remoteStatus || t('fields.notSynced')}
            dim
          />
        </div>

        {snap.lastError && (
          <div className="mt-5 rounded-md border border-destructive/30 bg-destructive/5 p-3">
            <p className="text-xs uppercase tracking-wider font-medium text-destructive/70 mb-1.5">
              {t('fields.lastError')}
            </p>
            <p className="font-mono text-xs break-all text-destructive/80 leading-relaxed">
              {snap.lastError}
            </p>
          </div>
        )}
      </Section>

      {/* ── data location ── */}
      <Section title={t('section.replicas')} description={t('section.replicasDesc')}>
        {replicas.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t('empty.replicas')}</p>
        ) : replicas.length === 1 ? (
          <SnapshotReplicaSummary replica={replicas[0]} />
        ) : (
          <SnapshotReplicaTable replicas={replicas} />
        )}
      </Section>

      {/* ── runtime refs ── */}
      <Section title={t('section.refs')} description={t('section.refsDesc')}>
        {refs.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t('empty.refs')}</p>
        ) : (
          <div className="flex flex-col gap-2">
            {refs.map((sbx) => (
              <div key={sbx} className="flex items-center gap-2">
                <span className="h-1.5 w-1.5 rounded-full bg-cube-ok" />
                <Link
                  to={`/sandboxes/${sbx}`}
                  className="font-mono text-base font-medium text-primary hover:underline break-all"
                >
                  {sbx}
                </Link>
                <CopyButton text={sbx} />
              </div>
            ))}
          </div>
        )}
      </Section>

      {/* ── danger zone ── */}
      <Section title={t('section.danger')} description={t('section.dangerDesc')} danger>
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
            {deleteMutation.isError && (
              <p className="text-xs text-destructive">
                {formatDeleteError(deleteMutation.error, t)}
              </p>
            )}
          </div>
        ) : (
          <Button variant="destructive" size="sm" onClick={() => setShowDeleteConfirm(true)}>
            <Trash2 className="h-4 w-4 mr-1.5" />
            {t('delete.button')}
          </Button>
        )}
      </Section>
    </div>
  );
}
