// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useQuery } from '@tanstack/react-query';
import { Link, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import type { ElementType, ReactNode } from 'react';
import { ArrowLeft, RefreshCw, FileClock, Download } from 'lucide-react';
import { warehouseApi, type WarehouseImportJob, type WarehousePreinstallJob } from '@/api/client';
import { Button } from '@/components/ui/button';
import { Card } from '@/components/ui/card';
import { Pagination } from '@/components/ui/pagination';
import { Skeleton } from '@/components/ui/skeleton';
import { cn, formatArtifactBytes, getStatusBadge } from '@/lib/utils';

const PAGE_SIZE = 20;

type JobsTab = 'import' | 'preinstall';

function parseTab(raw: string | null): JobsTab {
  return raw === 'preinstall' ? 'preinstall' : 'import';
}

function parsePage(raw: string | null): number {
  const n = Number(raw);
  return Number.isInteger(n) && n > 0 ? n : 1;
}

export default function WarehouseJobsPage() {
  const { t } = useTranslation('warehouse');
  const [params, setParams] = useSearchParams();
  const tab = parseTab(params.get('tab'));
  const page = parsePage(params.get('page'));
  const offset = (page - 1) * PAGE_SIZE;

  const setTab = (next: JobsTab) => {
    const nextParams = new URLSearchParams();
    nextParams.set('tab', next);
    setParams(nextParams);
  };

  const setPage = (next: number) => {
    const nextParams = new URLSearchParams();
    nextParams.set('tab', tab);
    if (next > 1) {
      nextParams.set('page', String(next));
    }
    setParams(nextParams);
  };

  const importsQ = useQuery({
    queryKey: ['warehouse', 'imports', page],
    queryFn: () => warehouseApi.listImports({ limit: PAGE_SIZE, offset }),
    refetchInterval: 5000,
    enabled: tab === 'import',
  });
  const jobsQ = useQuery({
    queryKey: ['warehouse', 'jobs', page],
    queryFn: () => warehouseApi.preinstallJobs({ limit: PAGE_SIZE, offset }),
    refetchInterval: 5000,
    enabled: tab === 'preinstall',
  });

  const activeQ = tab === 'import' ? importsQ : jobsQ;
  const importJobs = importsQ.data?.jobs ?? [];
  const preinstallJobs = jobsQ.data?.jobs ?? [];
  const total = activeQ.data?.total ?? 0;
  const loading =
    activeQ.isLoading && (tab === 'import' ? importJobs.length === 0 : preinstallJobs.length === 0);
  const pager =
    total > 0 ? (
      <Pagination
        page={page}
        pageSize={PAGE_SIZE}
        total={total}
        onPage={setPage}
        totalLabel={t('pageTotal', { total })}
        prevLabel={t('pagePrev')}
        nextLabel={t('pageNext')}
        jumpLabel={t('pageJump')}
        jumpUnit={t('pageJumpUnit')}
      />
    ) : null;

  return (
    <div className="animate-fade-in space-y-6 pt-4 pb-12">
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Link
          to="/warehouse"
          className="flex items-center gap-1 hover:text-foreground transition-colors"
        >
          <ArrowLeft size={14} />
          {t('title')}
        </Link>
        <span className="text-border">/</span>
        <span className="text-foreground font-medium">{t('jobs')}</span>
      </div>

      <div className="flex items-start justify-between gap-4">
        <div>
          <h1 className="flex items-center gap-2 text-xl font-semibold">
            <FileClock size={18} />
            {t('jobs')}
          </h1>
          <p className="mt-1 max-w-3xl text-sm text-muted-foreground">{t('jobsSubtitle')}</p>
        </div>
        <Button variant="outline" size="sm" onClick={() => void activeQ.refetch()}>
          <RefreshCw size={14} className={activeQ.isFetching ? 'animate-spin' : ''} />
          {t('refresh')}
        </Button>
      </div>

      <div className="flex items-center rounded-lg border border-border/60 bg-muted/40 p-1 gap-1 w-fit">
        {(
          [
            { key: 'import', label: t('importJobs') },
            { key: 'preinstall', label: t('preinstallJobs') },
          ] as const
        ).map(({ key, label }) => (
          <button
            key={key}
            type="button"
            onClick={() => setTab(key)}
            className={cn(
              'rounded-md px-3 py-1 text-xs font-medium transition-all',
              tab === key
                ? 'bg-background text-foreground shadow-sm ring-1 ring-border/60'
                : 'text-muted-foreground hover:text-foreground',
            )}
          >
            {label}
          </button>
        ))}
      </div>

      {loading ? (
        <Skeleton className="h-48 w-full" />
      ) : tab === 'import' ? (
        <JobSection
          emptyTitle={t('noImportJobs')}
          emptyDesc={t('noImportJobsDesc')}
          icon={Download}
        >
          {importJobs.length === 0 ? null : <ImportJobsTable jobs={importJobs} footer={pager} />}
        </JobSection>
      ) : (
        <JobSection emptyTitle={t('noJobs')} emptyDesc={t('noJobsDesc')} icon={FileClock}>
          {preinstallJobs.length === 0 ? null : (
            <PreinstallJobsTable jobs={preinstallJobs} footer={pager} />
          )}
        </JobSection>
      )}
    </div>
  );
}

function JobSection({
  emptyTitle,
  emptyDesc,
  icon: Icon,
  children,
}: {
  emptyTitle: string;
  emptyDesc: string;
  icon: ElementType;
  children: ReactNode;
}) {
  return (
    <section className="space-y-3">
      {children ?? (
        <Card className="flex flex-col items-center justify-center py-12 text-center border-dashed">
          <div className="flex h-10 w-10 items-center justify-center rounded-full bg-muted/50 mb-3">
            <Icon className="h-5 w-5 text-muted-foreground" />
          </div>
          <p className="text-sm font-medium">{emptyTitle}</p>
          <p className="text-xs text-muted-foreground mt-1 max-w-[280px]">{emptyDesc}</p>
        </Card>
      )}
    </section>
  );
}

function JobsTableCard({ children, footer }: { children: ReactNode; footer?: ReactNode }) {
  return (
    <Card className="overflow-hidden p-0 border-border/60 shadow-sm">
      <div className="overflow-x-auto">
        <table className="w-full text-sm">{children}</table>
      </div>
      {footer ? <div className="border-t border-border/40">{footer}</div> : null}
    </Card>
  );
}

function ImportJobsTable({ jobs, footer }: { jobs: WarehouseImportJob[]; footer?: ReactNode }) {
  const { t } = useTranslation('warehouse');
  return (
    <JobsTableCard footer={footer}>
      <thead className="bg-muted/30 text-left text-xs text-muted-foreground uppercase tracking-wider">
        <tr>
          <th className="px-4 py-3 font-medium">{t('source')}</th>
          <th className="px-4 py-3 font-medium">{t('repo')}</th>
          <th className="px-4 py-3 font-medium">{t('version')}</th>
          <th className="px-4 py-3 font-medium">{t('arch')}</th>
          <th className="px-4 py-3 font-medium">{t('size')}</th>
          <th className="px-4 py-3 font-medium">{t('status')}</th>
        </tr>
      </thead>
      <tbody className="divide-y divide-border/40">
        {jobs.map((job) => (
          <tr key={job.id} className="hover:bg-muted/20 transition-colors">
            <td className="px-4 py-3">{t(`source.${job.source}`, { defaultValue: job.source })}</td>
            <td className="px-4 py-3 font-mono text-xs">
              {job.source === 'upload' ? '—' : job.sourceRef}
            </td>
            <td className="px-4 py-3 font-mono text-xs text-muted-foreground">{job.tag || '—'}</td>
            <td className="px-4 py-3 font-mono text-xs">{job.arch}</td>
            <td className="px-4 py-3 text-muted-foreground">
              {formatArtifactBytes(job.bytesTotal)}
            </td>
            <td className="px-4 py-3">{getStatusBadge(job.status, t, job.error)}</td>
          </tr>
        ))}
      </tbody>
    </JobsTableCard>
  );
}

function PreinstallJobsTable({
  jobs,
  footer,
}: {
  jobs: WarehousePreinstallJob[];
  footer?: ReactNode;
}) {
  const { t } = useTranslation('warehouse');
  return (
    <JobsTableCard footer={footer}>
      <thead className="bg-muted/30 text-left text-xs text-muted-foreground uppercase tracking-wider">
        <tr>
          <th className="px-4 py-3 font-medium">{t('node')}</th>
          <th className="px-4 py-3 font-medium">{t('component')}</th>
          <th className="px-4 py-3 font-medium">{t('version')}</th>
          <th className="px-4 py-3 font-medium">{t('status')}</th>
        </tr>
      </thead>
      <tbody className="divide-y divide-border/40">
        {jobs.map((job) => (
          <tr key={job.id} className="hover:bg-muted/20 transition-colors">
            <td className="px-4 py-3 font-mono text-xs">{job.nodeId}</td>
            <td className="px-4 py-3">{job.component}</td>
            <td className="px-4 py-3 font-mono text-xs text-muted-foreground">
              {job.version} <span className="mx-1 text-border">/</span> {job.arch}
            </td>
            <td className="px-4 py-3">{getStatusBadge(job.status, t, job.error)}</td>
          </tr>
        ))}
      </tbody>
    </JobsTableCard>
  );
}
