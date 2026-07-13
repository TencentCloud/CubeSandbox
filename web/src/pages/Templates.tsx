// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useMemo, useState } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { ChevronDown, ChevronRight } from 'lucide-react';
import { templateApi, type TemplateCompatMatrix, type TemplateCompatRow } from '@/api/client';
import { Card, CardHeader, CardTitle, CardDescription, CardContent } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { AlertTriangle, Package, Plus, Trash2, X } from 'lucide-react';
import { formatRelative, formatDeleteError } from '@/lib/utils';

// ── create template modal ────────────────────────────────────────────────────

interface CreateModalProps {
  onClose: () => void;
}

interface CreateFormState {
  image: string;
  instanceType: string;
  writableLayerSize: string;
  exposedPorts: string;
  probePort: string;
  probePath: string;
  cpu: string;
  memory: string;
  envVars: string;
  allowInternet: boolean;
  // advanced
  networkType: string;
  nodes: string;
  command: string;
  args: string;
  dns: string;
  allowOut: string;
  denyOut: string;
  registryUsername: string;
  registryPassword: string;
  withCubeCa: boolean;
}

const INITIAL_FORM: CreateFormState = {
  image: '',
  instanceType: '',
  writableLayerSize: '1G',
  exposedPorts: '',
  probePort: '',
  probePath: '',
  cpu: '',
  memory: '',
  envVars: '',
  allowInternet: false,
  networkType: '',
  nodes: '',
  command: '',
  args: '',
  dns: '',
  allowOut: '',
  denyOut: '',
  registryUsername: '',
  registryPassword: '',
  withCubeCa: true,
};

/** Split a comma- or newline-separated list into a clean string array. */
function splitList(value: string): string[] {
  return value
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter(Boolean);
}

/** Parse exposed ports input; returns an array of valid ports or null if any token is invalid. */
function parsePorts(input: string): { ok: true; ports: number[] } | { ok: false } {
  const tokens = input
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean);
  if (tokens.length === 0) return { ok: true, ports: [] };
  const ports: number[] = [];
  for (const tok of tokens) {
    if (!/^\d{1,5}$/.test(tok)) return { ok: false };
    const n = Number(tok);
    if (!Number.isFinite(n) || n < 1 || n > 65535) return { ok: false };
    ports.push(n);
  }
  return { ok: true, ports };
}

/** Build the createTemplate API body from the form state, dropping empty fields. */
function buildCreateBody(state: CreateFormState): Record<string, unknown> {
  const body: Record<string, unknown> = {
    image: state.image.trim(),
  };

  const setStr = (k: string, v: string) => {
    const t = v.trim();
    if (t) body[k] = t;
  };
  const setNum = (k: string, v: string) => {
    const t = v.trim();
    if (!t) return;
    const n = Number(t);
    if (Number.isFinite(n) && n >= 0) body[k] = n;
  };
  const setList = (k: string, v: string) => {
    const list = splitList(v);
    if (list.length > 0) body[k] = list;
  };

  setStr('instanceType', state.instanceType);
  setStr('writableLayerSize', state.writableLayerSize);
  const ports = parsePorts(state.exposedPorts);
  if (ports.ok && ports.ports.length > 0) body.exposedPorts = ports.ports;
  setNum('probePort', state.probePort);
  setStr('probePath', state.probePath);
  setNum('cpu', state.cpu);
  setNum('memory', state.memory);
  setList('env', state.envVars);
  if (state.allowInternet) body.allowInternetAccess = true;

  setStr('networkType', state.networkType);
  setList('nodes', state.nodes);
  setList('command', state.command);
  setList('args', state.args);
  setList('dns', state.dns);
  setList('allowOut', state.allowOut);
  setList('denyOut', state.denyOut);
  setStr('registryUsername', state.registryUsername);
  setStr('registryPassword', state.registryPassword);
  body.with_cube_ca = state.withCubeCa;

  return body;
}

function CreateTemplateModal({ onClose }: CreateModalProps) {
  const { t } = useTranslation('templates');
  const qc = useQueryClient();
  const [form, setForm] = useState<CreateFormState>(INITIAL_FORM);
  const [showAdvanced, setShowAdvanced] = useState(false);

  const portsValidation = useMemo(() => parsePorts(form.exposedPorts), [form.exposedPorts]);
  const portsInvalid = !portsValidation.ok;
  const imageValid = form.image.trim().length > 0;
  // writableLayerSize is required by CubeMaster (500 writable_layer_size is required
  // otherwise). INITIAL_FORM seeds a sensible default of "1G", but a user could
  // still clear the field manually.
  const wlayerValid = form.writableLayerSize.trim().length > 0;
  const formInvalid = !imageValid || portsInvalid || !wlayerValid;

  const update = <K extends keyof CreateFormState>(key: K, value: CreateFormState[K]) => {
    setForm((prev) => ({ ...prev, [key]: value }));
  };

  // Sync probePort to the first exposed port when the user finishes editing
  // the exposedPorts input (onBlur), instead of on every keystroke. Doing it
  // on every keystroke causes "8" to be auto-synced while the user is still
  // typing "80" — once probePort is non-empty, the next keystroke won't
  // overwrite it, so the sync would be stuck at "8" forever.
  const syncProbePortFromExposedPorts = (exposedPortsValue: string) => {
    setForm((prev) => {
      if (prev.probePort.trim()) return prev; // user has customized probePort; don't touch it
      const parsed = parsePorts(exposedPortsValue);
      const first = parsed.ok ? parsed.ports[0] : undefined;
      if (first === undefined) return prev;
      return { ...prev, probePort: String(first) };
    });
  };

  const mutation = useMutation({
    mutationFn: () => templateApi.create(buildCreateBody(form) as Parameters<typeof templateApi.create>[0]),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['templates'] });
      onClose();
    },
  });

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm">
      <Card className="w-full max-w-3xl shadow-xl overflow-y-auto max-h-[90vh]">
        <CardHeader className="flex flex-row items-center justify-between pb-4">
          <CardTitle className="text-lg">{t('create.title')}</CardTitle>
          <button onClick={onClose} className="text-muted-foreground hover:text-foreground">
            <X className="h-5 w-5" />
          </button>
        </CardHeader>
        <CardContent className="space-y-5 text-base [&_label]:text-sm [&_label]:font-medium [&_p]:text-sm [&_input[type='text']]:h-10 [&_input[type='text']]:text-base [&_input[type='text']]:px-3.5 [&_input[type='number']]:h-10 [&_input[type='number']]:text-base [&_input[type='password']]:h-10 [&_input[type='password']]:text-base [&_textarea]:text-base [&_textarea]:min-h-[80px] [&_textarea]:p-3">
          {/* image */}
          <div className="space-y-1.5">
            <label className="text-xs font-medium text-muted-foreground">
              {t('create.image')} <span className="text-destructive text-sm font-bold">*</span>
            </label>
            <Input
              placeholder="registry.example.com/image:tag"
              value={form.image}
              onChange={(e) => update('image', e.target.value)}
            />
            <p className="text-xs text-muted-foreground">{t('create.imageHint')}</p>
          </div>

          {/* instanceType + writableLayerSize */}
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                {t('create.instanceType')}
              </label>
              <Input
                placeholder={t('instanceDefault')}
                value={form.instanceType}
                onChange={(e) => update('instanceType', e.target.value)}
              />
              <p className="text-xs text-muted-foreground">{t('create.instanceTypeHint')}</p>
            </div>
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                {t('create.writableLayerSize')}{' '}
                <span className="text-destructive text-sm font-bold" aria-label={t('create.required')}>
                  *
                </span>
              </label>
              <Input
                placeholder="1G"
                value={form.writableLayerSize}
                onChange={(e) => update('writableLayerSize', e.target.value)}
                aria-invalid={!wlayerValid}
              />
              <p className="text-xs text-muted-foreground">{t('create.writableLayerSizeHint')}</p>
            </div>
          </div>

          {/* exposedPorts */}
          <div className="space-y-1.5">
            <label className="text-xs font-medium text-muted-foreground">
              {t('create.exposedPorts')}
            </label>
            <Input
              placeholder="49983,9000"
              value={form.exposedPorts}
              onChange={(e) => update('exposedPorts', e.target.value)}
              onBlur={(e) => syncProbePortFromExposedPorts(e.target.value)}
            />
            {portsInvalid ? (
              <p className="text-xs text-destructive">{t('create.exposedPortsError')}</p>
            ) : (
              <p className="text-xs text-muted-foreground">{t('create.exposedPortsHint')}</p>
            )}
          </div>

          {/* probePort + probePath */}
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                {t('create.probePort')}
              </label>
              <Input
                placeholder="80"
                value={form.probePort}
                onChange={(e) => update('probePort', e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                {t('create.probePath')}
              </label>
              <Input
                placeholder="/health"
                value={form.probePath}
                onChange={(e) => update('probePath', e.target.value)}
              />
              <p className="text-xs text-muted-foreground">{t('create.probePathHint')}</p>
            </div>
          </div>
          <p className="text-xs text-muted-foreground -mt-2">
            {t('create.probeAutoSyncHint')}
          </p>

          {/* cpu + memory */}
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                {t('create.cpu')}
              </label>
              <Input
                placeholder="2000"
                value={form.cpu}
                onChange={(e) => update('cpu', e.target.value)}
              />
              <p className="text-xs text-muted-foreground">{t('create.cpuHint')}</p>
            </div>
            <div className="space-y-1.5">
              <label className="text-xs font-medium text-muted-foreground">
                {t('create.memory')}
              </label>
              <Input
                placeholder="2000"
                value={form.memory}
                onChange={(e) => update('memory', e.target.value)}
              />
              <p className="text-xs text-muted-foreground">{t('create.memoryHint')}</p>
            </div>
          </div>

          {/* env */}
          <div className="space-y-1.5">
            <label className="text-xs font-medium text-muted-foreground">
              {t('create.env')}
            </label>
            <textarea
              className="w-full rounded-md border bg-background px-3 py-2 text-sm font-mono resize-y min-h-[64px] focus:outline-none focus:ring-1 focus:ring-ring placeholder:text-muted-foreground/40"
              placeholder={'APP_ENV=production\nDEBUG=false'}
              value={form.envVars}
              onChange={(e) => update('envVars', e.target.value)}
            />
            <p className="text-xs text-muted-foreground">{t('create.envHint')}</p>
          </div>

          {/* allow-internet */}
          <label className="flex items-center gap-2 cursor-pointer select-none">
            <input
              type="checkbox"
              className="h-4 w-4 rounded border"
              checked={form.allowInternet}
              onChange={(e) => update('allowInternet', e.target.checked)}
            />
            <span className="text-sm">{t('create.allowInternetAccess')}</span>
          </label>

          {/* advanced toggle */}
          <div className="pt-2 border-t border-border/60">
            <button
              type="button"
              className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground hover:text-foreground"
              onClick={() => setShowAdvanced((v) => !v)}
            >
              {showAdvanced ? <ChevronDown className="h-3.5 w-3.5" /> : <ChevronRight className="h-3.5 w-3.5" />}
              {showAdvanced ? t('create.advanced.hide') : t('create.advanced.show')}
            </button>

            {showAdvanced && (
              <div className="mt-3 space-y-4">
                {/* network + nodes */}
                <div className="grid grid-cols-2 gap-3">
                  <div className="space-y-1.5">
                    <label className="text-xs font-medium text-muted-foreground">
                      {t('create.networkType')}
                    </label>
                    <Input
                      placeholder="tap"
                      value={form.networkType}
                      onChange={(e) => update('networkType', e.target.value)}
                    />
                    <p className="text-xs text-muted-foreground">{t('create.networkTypeHint')}</p>
                  </div>
                  <div className="space-y-1.5">
                    <label className="text-xs font-medium text-muted-foreground">
                      {t('create.nodes')}
                    </label>
                    <Input
                      placeholder={t('create.nodesPlaceholder')}
                      value={form.nodes}
                      onChange={(e) => update('nodes', e.target.value)}
                    />
                    <p className="text-xs text-muted-foreground">{t('create.nodesHint')}</p>
                  </div>
                </div>

                {/* command + args */}
                <div className="grid grid-cols-2 gap-3">
                  <div className="space-y-1.5">
                    <label className="text-xs font-medium text-muted-foreground">
                      {t('create.command')}
                    </label>
                    <textarea
                      className="w-full rounded-md border bg-background px-3 py-2 text-sm font-mono resize-y min-h-[64px] focus:outline-none focus:ring-1 focus:ring-ring placeholder:text-muted-foreground/40"
                      placeholder={'/usr/local/bin/entrypoint.sh'}
                      value={form.command}
                      onChange={(e) => update('command', e.target.value)}
                    />
                    <p className="text-xs text-muted-foreground">{t('create.commandHint')}</p>
                  </div>
                  <div className="space-y-1.5">
                    <label className="text-xs font-medium text-muted-foreground">
                      {t('create.args')}
                    </label>
                    <textarea
                      className="w-full rounded-md border bg-background px-3 py-2 text-sm font-mono resize-y min-h-[64px] focus:outline-none focus:ring-1 focus:ring-ring placeholder:text-muted-foreground/40"
                      placeholder={'--config\n/etc/app.conf'}
                      value={form.args}
                      onChange={(e) => update('args', e.target.value)}
                    />
                    <p className="text-xs text-muted-foreground">{t('create.argsHint')}</p>
                  </div>
                </div>

                {/* dns + allowOut/denyOut */}
                <div className="space-y-1.5">
                  <label className="text-xs font-medium text-muted-foreground">
                    {t('create.dns')}
                  </label>
                  <Input
                    placeholder="1.1.1.1,8.8.8.8"
                    value={form.dns}
                    onChange={(e) => update('dns', e.target.value)}
                  />
                  <p className="text-xs text-muted-foreground">{t('create.dnsHint')}</p>
                </div>

                <div className="grid grid-cols-2 gap-3">
                  <div className="space-y-1.5">
                    <label className="text-xs font-medium text-muted-foreground">
                      {t('create.allowOut')}
                    </label>
                    <textarea
                      className="w-full rounded-md border bg-background px-3 py-2 text-sm font-mono resize-y min-h-[64px] focus:outline-none focus:ring-1 focus:ring-ring placeholder:text-muted-foreground/40"
                      placeholder={'10.0.0.0/8\n192.168.0.0/16'}
                      value={form.allowOut}
                      onChange={(e) => update('allowOut', e.target.value)}
                    />
                    <p className="text-xs text-muted-foreground">{t('create.allowOutHint')}</p>
                  </div>
                  <div className="space-y-1.5">
                    <label className="text-xs font-medium text-muted-foreground">
                      {t('create.denyOut')}
                    </label>
                    <textarea
                      className="w-full rounded-md border bg-background px-3 py-2 text-sm font-mono resize-y min-h-[64px] focus:outline-none focus:ring-1 focus:ring-ring placeholder:text-muted-foreground/40"
                      placeholder={'169.254.0.0/16'}
                      value={form.denyOut}
                      onChange={(e) => update('denyOut', e.target.value)}
                    />
                    <p className="text-xs text-muted-foreground">{t('create.denyOutHint')}</p>
                  </div>
                </div>

                {/* registry */}
                <div className="grid grid-cols-2 gap-3">
                  <div className="space-y-1.5">
                    <label className="text-xs font-medium text-muted-foreground">
                      {t('create.registryUsername')}
                    </label>
                    <Input
                      autoComplete="off"
                      value={form.registryUsername}
                      onChange={(e) => update('registryUsername', e.target.value)}
                    />
                  </div>
                  <div className="space-y-1.5">
                    <label className="text-xs font-medium text-muted-foreground">
                      {t('create.registryPassword')}
                    </label>
                    <Input
                      type="password"
                      autoComplete="off"
                      value={form.registryPassword}
                      onChange={(e) => update('registryPassword', e.target.value)}
                    />
                  </div>
                </div>

                {/* with_cube_ca */}
                <label className="flex items-center gap-2 cursor-pointer select-none">
                  <input
                    type="checkbox"
                    className="h-4 w-4 rounded border"
                    checked={form.withCubeCa}
                    onChange={(e) => update('withCubeCa', e.target.checked)}
                  />
                  <span className="text-sm">{t('create.withCubeCa')}</span>
                </label>
              </div>
            )}
          </div>

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
              disabled={formInvalid || mutation.isPending}
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
  const { data: compat } = useQuery({
    queryKey: ['templates', 'compat'],
    queryFn: templateApi.compat,
    refetchInterval: 30_000,
  });
  const { t } = useTranslation('templates');
  const [showCreate, setShowCreate] = useState(false);
  const [deletingID, setDeletingID] = useState<string | null>(null);
  const [tab, setTab] = useState<'list' | 'compat'>('list');
  const compatByTemplate = new Map((compat?.templates ?? []).map((row) => [row.templateID, row]));

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

      {(compat?.summary.staleTemplates ?? 0) > 0 && (
        <Card className="border-destructive/30 bg-destructive/5">
          <div className="flex items-center justify-between gap-3 p-4 text-sm">
            <div className="flex items-center gap-2 text-destructive">
              <AlertTriangle size={16} />
              <span>
                {t('compat.banner', {
                  templates: compat?.summary.staleTemplates,
                  replicas: compat?.summary.staleReplicas,
                })}
              </span>
            </div>
            <Button variant="secondary" size="sm" onClick={() => setTab('compat')}>
              {t('compat.view')}
            </Button>
          </div>
        </Card>
      )}

      <div className="flex gap-2">
        <Button variant={tab === 'list' ? 'default' : 'secondary'} size="sm" onClick={() => setTab('list')}>
          {t('tabs.list')}
        </Button>
        <Button variant={tab === 'compat' ? 'default' : 'secondary'} size="sm" onClick={() => setTab('compat')}>
          {t('tabs.compat')}
          {(compat?.summary.staleTemplates ?? 0) > 0 && (
            <Badge tone="err" className="ml-2">{compat?.summary.staleTemplates}</Badge>
          )}
        </Button>
      </div>

      {tab === 'compat' && <TemplateCompatPanel matrix={compat} />}

      {tab === 'list' && isLoading && (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {Array.from({ length: 6 }).map((_, i) => (
            <Skeleton key={i} className="h-28" />
          ))}
        </div>
      )}

      {tab === 'list' && data && data.length === 0 && (
        <Card>
          <div className="py-16 text-center text-sm text-muted-foreground">
            {t('noTemplates')}
          </div>
        </Card>
      )}

      {tab === 'list' && <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
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
                  {compatByTemplate.get(tpl.templateID)?.overall === 'STALE' ? (
                    <Badge tone="err">{t('compat.status.STALE')}</Badge>
                  ) : (
                    <Badge tone={tpl.status.toLowerCase() === 'ready' ? 'ok' : tpl.status.toLowerCase() === 'failed' ? 'err' : 'warn'}>
                      {tpl.status}
                    </Badge>
                  )}
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
                  {tpl.jobID ? (
                    <div className="truncate font-mono">{t('col.jobID')}: <span className="text-foreground/80">{tpl.jobID}</span></div>
                  ) : null}
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
      </div>}

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

function TemplateCompatPanel({ matrix }: { matrix?: TemplateCompatMatrix }) {
  const { t } = useTranslation('templates');
  if (!matrix) {
    return (
      <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
        {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-24" />)}
      </div>
    );
  }
  return (
    <div className="space-y-4">
      <div className="grid grid-cols-1 gap-3 md:grid-cols-5">
        <CompatKpi label={t('compat.kpi.staleTemplates')} value={matrix.summary.staleTemplates} tone="err" />
        <CompatKpi label={t('compat.kpi.staleReplicas')} value={matrix.summary.staleReplicas} tone="err" />
        <CompatKpi label={t('compat.kpi.affectedNodes')} value={matrix.summary.affectedNodes} tone="warn" />
        <CompatKpi label={t('compat.kpi.missingReplicas')} value={matrix.summary.missingReplicas} tone="warn" />
        <CompatKpi label={t('compat.kpi.unknownReplicas')} value={matrix.summary.unknownReplicas} tone="mute" />
      </div>
      {matrix.templates.length === 0 ? (
        <Card><div className="p-8 text-center text-sm text-muted-foreground">{t('noTemplates')}</div></Card>
      ) : (
        <div className="space-y-3">
          {matrix.templates.map((row) => <CompatTemplateRow key={row.templateID} row={row} />)}
        </div>
      )}
    </div>
  );
}

function CompatKpi({ label, value, tone }: { label: string; value: number; tone: 'err' | 'warn' | 'mute' }) {
  return (
    <Card>
      <div className="p-4">
        <div className="text-xs text-muted-foreground">{label}</div>
        <div className={['mt-2 text-2xl font-semibold', compatKpiToneClass(tone)].join(' ')}>
          {value}
        </div>
      </div>
    </Card>
  );
}

function compatKpiToneClass(tone: 'err' | 'warn' | 'mute') {
  switch (tone) {
    case 'err':
      return 'text-destructive';
    case 'warn':
      return 'text-warning';
    default:
      return 'text-muted-foreground';
  }
}

function CompatTemplateRow({ row }: { row: TemplateCompatRow }) {
  const { t } = useTranslation('templates');
  const queryClient = useQueryClient();
  const adoptMutation = useMutation({
    mutationFn: () => templateApi.adoptCompatBaseline(row.templateID),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['templates', 'compat'] }),
  });
  const hasUnknown = row.nodes.some((node) => node.compatStatus === 'UNKNOWN');
  return (
    <Card>
      <div className="space-y-3 p-4">
        <div className="flex items-center justify-between gap-3">
          <Link to={`/templates/${row.templateID}`} className="font-mono text-sm hover:text-primary">{row.templateID}</Link>
          <div className="flex items-center gap-2">
            {hasUnknown && (
              <Button
                size="sm"
                variant="secondary"
                disabled={adoptMutation.isPending}
                onClick={() => {
                  if (window.confirm(t('compat.adoptConfirm'))) {
                    adoptMutation.mutate();
                  }
                }}
              >
                {t('compat.adoptBaseline')}
              </Button>
            )}
            <Badge tone={compatTone(row.overall)}>{t(`compat.status.${row.overall}`, { defaultValue: row.overall })}</Badge>
          </div>
        </div>
        <div className="grid grid-cols-1 gap-2 lg:grid-cols-2">
          {row.nodes.map((node) => (
            <div key={node.nodeID} className="rounded-lg border border-border/60 bg-card/40 p-3 text-xs">
              <div className="flex items-center justify-between gap-2">
                <span className="font-mono">{node.nodeID}</span>
                <Badge tone={compatTone(node.compatStatus)}>
                  {t(`compat.status.${node.compatStatus}`, { defaultValue: node.compatStatus })}
                </Badge>
              </div>
              <div className="mt-2 space-y-1 text-muted-foreground">
                <CompatVersionLine label="guest" bound={node.boundGuestImageVersion} current={node.currentGuestImageVersion} />
                <CompatVersionLine label="agent" bound={node.boundAgentVersion} current={node.currentAgentVersion} />
                <CompatVersionLine label="kernel" bound={node.boundKernelVersion} current={node.currentKernelVersion} />
              </div>
            </div>
          ))}
        </div>
      </div>
    </Card>
  );
}

function CompatVersionLine({ label, bound, current }: { label: string; bound?: string | null; current?: string | null }) {
  return (
    <div className="flex justify-between gap-3">
      <span>{label}</span>
      <span className="truncate font-mono text-foreground/80">{bound ?? '—'} → {current ?? '—'}</span>
    </div>
  );
}

function compatTone(status: string): 'ok' | 'err' | 'warn' | 'mute' {
  if (status === 'OK') return 'ok';
  if (status === 'STALE') return 'err';
  if (status === 'MISSING') return 'warn';
  return 'mute';
}
