// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { templateApi } from '@/api/client';
import { Card, CardHeader, CardTitle, CardDescription, CardContent } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { Package, Plus, Trash2, X } from 'lucide-react';
import { formatRelative, formatDeleteError } from '@/lib/utils';

// ── create template modal ────────────────────────────────────────────────────

interface CreateModalProps {
  onClose: () => void;
}

function CreateTemplateModal({ onClose }: CreateModalProps) {
  const { t } = useTranslation('templates');
  const qc = useQueryClient();
  const [image, setImage] = useState('');
  const [instanceType, setInstanceType] = useState('');
  const [writableLayerSize, setWritableLayerSize] = useState('1G');
  const [exposedPorts, setExposedPorts] = useState('');
  const [probePort, setProbePort] = useState('');
  const [probePath, setProbePath] = useState('');
  const [cpu, setCpu] = useState('');
  const [memory, setMemory] = useState('');
  const [envVars, setEnvVars] = useState('');
  const [allowInternet, setAllowInternet] = useState(false);

  const mutation = useMutation({
    mutationFn: () => {
      const ports = exposedPorts.split(',').map(s => parseInt(s.trim(), 10)).filter(n => !isNaN(n) && n > 0);
      const envList = envVars.split('\n').map(s => s.trim()).filter(Boolean);
      return templateApi.create({
        image,
        instanceType: instanceType.trim() || undefined,
        writableLayerSize: writableLayerSize.trim() || undefined,
        exposedPorts: ports.length > 0 ? ports : undefined,
        probePort: probePort.trim() ? parseInt(probePort.trim(), 10) : undefined,
        probePath: probePath.trim() || undefined,
        cpu: cpu.trim() ? parseInt(cpu.trim(), 10) : undefined,
        memory: memory.trim() ? parseInt(memory.trim(), 10) : undefined,
        env: envList.length > 0 ? envList : undefined,
        allowInternetAccess: allowInternet || undefined,
      });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['templates'] });
      onClose();
    },
  });

  const valid = image.trim().length > 0 && writableLayerSize.trim().length > 0 && exposedPorts.trim().length > 0;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm">
      <Card className="w-full max-w-2xl shadow-xl overflow-y-auto max-h-[90vh]">
        <CardHeader className="flex flex-row items-center justify-between pb-3">
          <CardTitle className="text-base">{t('create.title')}</CardTitle>
          <button onClick={onClose} className="text-muted-foreground hover:text-foreground">
            <X className="h-4 w-4" />
          </button>
        </CardHeader>
        <CardContent className="space-y-4">
          {/* Row 1: image */}
          <div className="grid grid-cols-1 gap-3">
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                {t('create.image')} <span className="text-destructive text-sm font-bold">*</span>
              </label>
              <Input
                placeholder="registry.example.com/image:tag"
                value={image}
                onChange={(e) => setImage(e.target.value.trim())}
              />
            </div>
          </div>
          {/* Row 2: instanceType + writableLayerSize */}
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                {t('create.instanceType')}
              </label>
              <Input
                placeholder={t('instanceDefault')}
                value={instanceType}
                onChange={(e) => setInstanceType(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                {t('create.writableLayerSize')} <span className="text-destructive text-sm font-bold">*</span>
              </label>
              <Input
                placeholder="1G"
                value={writableLayerSize}
                onChange={(e) => setWritableLayerSize(e.target.value)}
              />
              <p className="text-xs text-muted-foreground">{t('create.writableLayerSizeHint')}</p>
            </div>
          </div>
          {/* Row 3: exposedPorts 占全宽 */}
          <div className="space-y-1.5">
            <label className="text-xs font-medium text-muted-foreground">
              {t('create.exposedPorts')} <span className="text-destructive text-sm font-bold">*</span>
            </label>
            <Input
              placeholder="49983"
              value={exposedPorts}
              onChange={(e) => { setExposedPorts(e.target.value); const first = e.target.value.split(',')[0].trim(); if (first) setProbePort(first); }}
            />
            <p className="text-xs text-muted-foreground">{t('create.exposedPortsHint')}</p>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                {t('create.probePort')}
              </label>
              <Input
                placeholder="49983"
                value={probePort}
                onChange={(e) => setProbePort(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                {t('create.probePath')}
              </label>
              <Input
                placeholder="/health"
                value={probePath}
                onChange={(e) => setProbePath(e.target.value)}
              />
            </div>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">CPU (millicores)</label>
              <Input placeholder="2000" value={cpu} onChange={(e) => setCpu(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">Memory (MiB)</label>
              <Input placeholder="2000" value={memory} onChange={(e) => setMemory(e.target.value)} />
            </div>
          </div>
          <div className="space-y-1.5">
            <label className="text-xs font-medium text-muted-foreground">env</label>
            <textarea
              className="w-full rounded-md border bg-background px-3 py-2 text-sm font-mono resize-y min-h-[64px] focus:outline-none focus:ring-1 focus:ring-ring placeholder:text-muted-foreground/40"
              placeholder={"APP_ENV=production\nDEBUG=false"}
              value={envVars}
              onChange={(e) => setEnvVars(e.target.value)}
            />
            <p className="text-xs text-muted-foreground">每行一条，格式 KEY=VALUE</p>
          </div>
          <label className="flex items-center gap-2 cursor-pointer select-none">
            <input
              type="checkbox"
              className="h-4 w-4 rounded border"
              checked={allowInternet}
              onChange={(e) => setAllowInternet(e.target.checked)}
            />
            <span className="text-sm">allow-internet-access</span>
          </label>

          {mutation.isError && (
            <p className="text-xs text-destructive">
              {(mutation.error as Error)?.message ?? t('create.error')}
            </p>
          )}

          <div className="flex justify-end gap-2 pt-1">
            <Button variant="outline" size="sm" onClick={onClose}>
              {t('create.cancel')}
            </Button>
            <Button
              size="sm"
              disabled={!valid || mutation.isPending}
              onClick={() => mutation.mutate()}
            >
              {mutation.isPending ? t('create.creating') : t('create.submit')}
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

// ── delete confirm modal ────────────────────────────────────────────────────

interface DeleteModalProps {
  templateID: string;
  onClose: () => void;
}

function DeleteTemplateModal({ templateID, onClose }: DeleteModalProps) {
  const { t } = useTranslation('templates');
  const qc = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => templateApi.remove(templateID),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['templates'] });
      onClose();
    },
  });

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm">
      <Card className="w-full max-w-sm shadow-xl">
        <CardHeader className="flex flex-row items-center justify-between pb-3">
          <CardTitle className="text-base text-destructive">
            {t('delete.title', { defaultValue: '删除模板' })}
          </CardTitle>
          <button onClick={onClose} className="text-muted-foreground hover:text-foreground">
            <X className="h-4 w-4" />
          </button>
        </CardHeader>
        <CardContent className="space-y-4">
          <p className="text-sm text-muted-foreground">
            {t('delete.confirmDesc', { defaultValue: '确定要删除模板' })}{' '}
            <span className="font-mono font-medium text-foreground">{templateID}</span>
            {' '}{t('delete.confirmDescSuffix', { defaultValue: '吗？此操作不可撤销。' })}
          </p>
          {mutation.isError && (
            <p className="text-xs text-destructive">
              {formatDeleteError(mutation.error)}
            </p>
          )}
          <div className="flex justify-end gap-2">
            <Button variant="outline" size="sm" onClick={onClose}>
              {t('delete.cancel', { defaultValue: '取消' })}
            </Button>
            <Button
              variant="destructive"
              size="sm"
              disabled={mutation.isPending}
              onClick={() => mutation.mutate()}
            >
              {mutation.isPending
                ? t('delete.deleting', { defaultValue: '删除中…' })
                : t('delete.confirm', { defaultValue: '确认删除' })}
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

// ── main page ────────────────────────────────────────────────────────────────

export default function TemplatesPage() {
  const { data, isLoading } = useQuery({ queryKey: ['templates'], queryFn: templateApi.list });
  const { t } = useTranslation('templates');
  const [showCreate, setShowCreate] = useState(false);
  const [deletingID, setDeletingID] = useState<string | null>(null);

  return (
    <div className="animate-fade-in space-y-5">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t('subtitle')}</p>
        </div>
        <Button onClick={() => setShowCreate(true)}>
          <Plus size={14} /> {t('create.button')}
        </Button>
      </header>

      {isLoading && (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-28" />
          ))}
        </div>
      )}

      {data && data.length === 0 && (
        <Card>
          <div className="py-16 text-center text-sm text-muted-foreground">
            {t('noTemplates')}
          </div>
        </Card>
      )}

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
        {data?.map((tpl) => (
          <div key={tpl.templateID} className="relative group">
            <Link to={`/templates/${tpl.templateID}`} className="block">
              <Card className="panel-hover h-full">
                <CardHeader>
                  <div className="flex items-center gap-3">
                    <span className="flex h-10 w-10 items-center justify-center rounded-lg bg-gradient-to-br from-primary/20 to-cube-accent/20 text-primary ring-1 ring-primary/20">
                      <Package size={18} />
                    </span>
                    <div>
                      <CardTitle className="text-base">{tpl.templateID}</CardTitle>
                      <CardDescription className="font-mono text-xs">{tpl.templateID}</CardDescription>
                    </div>
                  </div>
                  <Badge tone={tpl.status.toLowerCase() === 'ready' ? 'ok' : tpl.status.toLowerCase() === 'failed' ? 'err' : 'warn'}>
                    {tpl.status}
                  </Badge>
                </CardHeader>
                <div className="grid grid-cols-2 gap-3 pt-3 text-xs text-muted-foreground">
                  <div>
                    <div className="text-xs uppercase tracking-wider">{t('col.instance')}</div>
                    <div className="mt-0.5 text-foreground/80">{tpl.instanceType ?? t('instanceDefault')}</div>
                  </div>
                  <div>
                    <div className="text-xs uppercase tracking-wider">{t('col.created')}</div>
                    <div className="mt-0.5 text-foreground/80">{formatRelative(tpl.createdAt)}</div>
                  </div>
                </div>
                <div className="mt-3 space-y-1 text-xs text-muted-foreground">
                  <div className="truncate">{t('col.version')}: <span className="text-foreground/80">{tpl.version ?? '—'}</span></div>
                  <div className="truncate">{t('col.image')}: <span className="text-foreground/80">{tpl.imageInfo ?? '—'}</span></div>
                </div>
              </Card>
            </Link>
            {/* delete button — visible on hover, always shown for failed templates */}
            <button
              className={[
                'absolute top-2.5 right-2.5 z-10 flex items-center justify-center',
                'h-7 w-7 rounded-md border bg-background shadow-sm',
                'text-muted-foreground hover:text-destructive hover:border-destructive/50',
                'transition-opacity duration-150',
                tpl.status.toLowerCase() === 'failed'
                  ? 'opacity-100'
                  : 'opacity-0 group-hover:opacity-100',
              ].join(' ')}
              title={t('delete.button', { defaultValue: '删除模板' })}
              onClick={(e) => {
                e.preventDefault();
                e.stopPropagation();
                setDeletingID(tpl.templateID);
              }}
            >
              <Trash2 className="h-3.5 w-3.5" />
            </button>
          </div>
        ))}
      </div>

      {showCreate && <CreateTemplateModal onClose={() => setShowCreate(false)} />}
      {deletingID && (
        <DeleteTemplateModal
          templateID={deletingID}
          onClose={() => setDeletingID(null)}
        />
      )}
    </div>
  );
}
