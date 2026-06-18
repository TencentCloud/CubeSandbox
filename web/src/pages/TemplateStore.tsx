// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useState, useRef, useEffect, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { createPortal } from 'react-dom';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { agentHubApi, templateApi, storeApi, type TemplateSummary, type ImageMeta, type StoreCatalogItem } from '@/api/client';
import { showToast } from '@/components/ui/ToastProvider';
import { CATEGORIES, type CategoryId, type StoreTemplate } from '@/data/templateStore';
import { getInstalledTemplates } from '@/lib/template-match';
import { Card, CardContent } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Skeleton } from '@/components/ui/skeleton';
import { Code2, Globe, Bot, Box, Server, Search, X, ChevronDown, Package, Loader2, Plus, AlertTriangle, RefreshCw } from 'lucide-react';
import { cn } from '@/lib/utils';

// ── helpers ───────────────────────────────────────────────────────────────────

function categoryIcon(category: StoreCatalogItem['category']) {
  switch (category) {
    case 'code':    return Code2;
    case 'browser': return Globe;
    case 'ai':      return Bot;
    case 'web':     return Server;
    case 'base':    return Box;
    default:        return Box;
  }
}

function isOpenClawTemplate(item: StoreTemplate): boolean {
  return item.id === 'openclaw-lite' || item.id === 'openclaw-aio';
}


// ── InstallModal ──────────────────────────────────────────────────────────────

type InstallPhase =
  | { kind: 'idle' }
  | { kind: 'submitting' }
  | { kind: 'polling'; templateID: string }
  | { kind: 'ready';   templateID: string }
  | { kind: 'failed';  message: string };

interface InstallModalProps {
  item: StoreTemplate;
  enableForAgentHub?: boolean;
  onClose: () => void;
}

function InstallModal({ item, enableForAgentHub = false, onClose }: InstallModalProps) {
  const { t } = useTranslation('store');
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [writableLayerSize, setWritableLayerSize] = useState(item.writable_layer_size);
  const [phase, setPhase] = useState<InstallPhase>({ kind: 'idle' });
  const [enabling, setEnabling] = useState(false);
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const enableAgentTemplate = useCallback(
    async (templateID: string) => {
      setEnabling(true);
      try {
        await agentHubApi.registerMarketTemplate({
          templateId: templateID,
          name: t(item.name_key as 'official', { defaultValue: item.id }),
          model: 'deepseek/deepseek-v4-flash',
          version: item.id,
          recommended: true,
        });
        showToast(t('toast.agentTemplateEnabled'));
        onClose();
        navigate(`/agenthub?createTemplate=${encodeURIComponent(templateID)}`);
      } catch (err) {
        setPhase({ kind: 'failed', message: err instanceof Error ? err.message : String(err) });
      } finally {
        setEnabling(false);
      }
    },
    [item, navigate, onClose, t],
  );

  const stopPolling = useCallback(() => {
    if (pollRef.current) {
      clearInterval(pollRef.current);
      pollRef.current = null;
    }
  }, []);

  useEffect(() => () => stopPolling(), [stopPolling]);

  const startPolling = useCallback(
    (tplID: string) => {
      let attempts = 0;
      const MAX = 60; // 60 × 2s = 2min
      pollRef.current = setInterval(async () => {
        attempts++;
        try {
          const list = await templateApi.list();
          qc.setQueryData(['templates'], list);
          const found = list.find((t) => t.templateID === tplID);
          if (found?.status?.toUpperCase() === 'READY') {
            stopPolling();
            setPhase({ kind: 'ready', templateID: tplID });
            if (enableForAgentHub) void enableAgentTemplate(tplID);
          } else if (found?.status?.toUpperCase() === 'FAILED') {
            stopPolling();
            setPhase({ kind: 'failed', message: found.lastError ?? t('installModal.buildFailed') });
          } else if (attempts >= MAX) {
            stopPolling();
            setPhase({ kind: 'failed', message: t('installModal.timeout') });
          }
        } catch {
          // ignore transient errors
        }
      }, 2000);
    },
    [enableAgentTemplate, enableForAgentHub, qc, stopPolling],
  );

  const mutation = useMutation({
    mutationFn: () =>
      templateApi.create({
        image: item.image_cn,
        exposedPorts: item.expose_ports,
        probePort: item.probe_port,
        probePath: item.probe_path,
        writableLayerSize: writableLayerSize.trim() || item.writable_layer_size,
        dns: item.dns.length > 0 ? item.dns : undefined,
      }),
    onMutate: () => setPhase({ kind: 'submitting' }),
    onSuccess: (data) => {
      const id =
        (data as { templateID?: string } | null)?.templateID ??
        '';
      setPhase({ kind: 'polling', templateID: id });
      startPolling(id);
    },
    onError: (err) => {
      setPhase({ kind: 'failed', message: (err as Error)?.message ?? t('installModal.failed') });
    },
  });

  const isBuilding = phase.kind === 'submitting' || phase.kind === 'polling';
  const isDone     = phase.kind === 'ready';

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm">
      <Card className="w-full max-w-2xl shadow-xl">
        {/* header */}
        <div className="flex items-center justify-between border-b px-6 py-5">
          <div>
            <p className="text-base font-semibold font-mono">{item.image.split("/").pop()}</p>
            <p className="mt-1 text-sm text-muted-foreground">{t('installModal.subtitle')}</p>
          </div>
          <button
            onClick={() => { stopPolling(); onClose(); }}
            className="text-muted-foreground hover:text-foreground"
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        <CardContent className="space-y-5 pt-5 px-6 pb-6">
          {/* 镜像信息（只读） */}
          <div className="rounded-lg border bg-muted/30 p-4 space-y-2 text-sm font-mono">
            <div><span className="text-muted-foreground">image: </span>{item.image_cn}</div>
            <div><span className="text-muted-foreground">expose-port: </span>{item.expose_ports.join(', ')}</div>
            <div><span className="text-muted-foreground">probe: </span>{item.probe_port}</div>
            <div><span className="text-muted-foreground">probe-path: </span>{item.probe_path}</div>
            {item.dns.length > 0 && (
              <div><span className="text-muted-foreground">dns: </span>{item.dns.join(', ')}</div>
            )}
          </div>

          {/* 可编辑参数 */}
          <div className="space-y-3">
            <div className="space-y-1.5">
              <label className="text-sm font-medium text-muted-foreground">writable-layer-size</label>
              <Input
                placeholder="1G"
                value={writableLayerSize}
                disabled={isBuilding || isDone}
                onChange={(e) => setWritableLayerSize(e.target.value)}
              />
            </div>
          </div>

          {/* 安装进度 */}
          {(phase.kind === 'polling' || enabling) && (
            <div className="flex items-center gap-2 rounded-md border bg-muted/30 px-3 py-2 text-xs text-muted-foreground">
              <Loader2 className="h-3.5 w-3.5 animate-spin shrink-0" />
              <span>
                {enabling ? t('installModal.enablingAgentHub') : t('installModal.building')}
                {phase.kind === 'polling' && phase.templateID && (
                  <span className="ml-1 font-mono text-foreground">{phase.templateID}</span>
                )}
              </span>
            </div>
          )}

          {/* 成功 */}
          {phase.kind === 'ready' && (
            <div className="rounded-md border border-green-500/30 bg-green-500/10 px-3 py-2 text-xs text-green-600 dark:text-green-400">
              ✅ {t('installModal.success')}
              <button
                className="ml-2 underline"
                onClick={() => { stopPolling(); onClose(); navigate(`/templates/${phase.templateID}`); }}
              >
                {t('installModal.viewTemplate')} {phase.templateID}
              </button>
            </div>
          )}

          {/* 失败 */}
          {phase.kind === 'failed' && (
            <p className="rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-xs text-destructive">
              ❌ {phase.message}
            </p>
          )}

          <div className="flex justify-end gap-2 pt-1">
            <Button
              variant="outline"
              size="sm"
              onClick={() => { stopPolling(); onClose(); }}
            >
              {isDone ? t('installModal.close') : t('installModal.cancel')}
            </Button>
            {!isDone && (
              <Button
                size="sm"
                disabled={isBuilding || enabling}
                onClick={() => mutation.mutate()}
              >
                {isBuilding || enabling ? (
                  <><Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" />{t('installModal.installing')}</>
                ) : phase.kind === 'failed' ? (
                  t('installModal.retry')
                ) : (
                  t('installModal.confirm')
                )}
              </Button>
            )}
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

// ── InstalledDropdown ─────────────────────────────────────────────────────────

interface InstalledDropdownProps {
  installed: TemplateSummary[];
  onInstallAnother: () => void;
}

function InstalledDropdown({ installed, onInstallAnother }: InstalledDropdownProps) {
  const { t } = useTranslation('store');
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState({ top: 0, right: 0 });
  const triggerRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const navigate = useNavigate();

  const openMenu = () => {
    if (triggerRef.current) {
      const rect = triggerRef.current.getBoundingClientRect();
      setPos({
        top: rect.bottom + window.scrollY + 4,
        right: window.innerWidth - rect.right + window.scrollX,
      });
    }
    setOpen(true);
  };

  useEffect(() => {
    if (!open) return;
    function handle(e: MouseEvent) {
      if (
        menuRef.current && !menuRef.current.contains(e.target as Node) &&
        triggerRef.current && !triggerRef.current.contains(e.target as Node)
      ) {
        setOpen(false);
      }
    }
    document.addEventListener('mousedown', handle);
    return () => document.removeEventListener('mousedown', handle);
  }, [open]);

  if (installed.length === 1) {
    return (
      <div className="flex items-center gap-1.5">
        <Button
          variant="outline"
          size="sm"
          className="text-green-600 border-green-500/40 hover:bg-green-500/10"
          onClick={() => navigate(`/templates/${installed[0].templateID}`)}
        >
          ✓ {t('installed')}
        </Button>
        <Button
          variant="ghost"
          size="sm"
          className="px-2 text-muted-foreground"
          title={t('installAdditionalHint')}
          onClick={onInstallAnother}
        >
          <Plus className="h-3.5 w-3.5" />
        </Button>
      </div>
    );
  }

  return (
    <div className="flex items-center gap-1.5">
      <Button
        ref={triggerRef}
        variant="outline"
        size="sm"
        className="text-green-600 border-green-500/40 hover:bg-green-500/10"
        onClick={() => (open ? setOpen(false) : openMenu())}
      >
        ✓ {t('installedCount', { count: installed.length })} <ChevronDown className="ml-1 h-3 w-3" />
      </Button>
      <Button
        variant="ghost"
        size="sm"
        className="px-2 text-muted-foreground"
        title={t('installAdditionalHint')}
        onClick={onInstallAnother}
      >
        <Plus className="h-3.5 w-3.5" />
      </Button>
      {open && createPortal(
        <div
          ref={menuRef}
          style={{ position: 'absolute', top: pos.top, right: pos.right, zIndex: 9999 }}
          className="w-64 max-h-60 overflow-y-auto rounded-lg border bg-popover shadow-xl"
        >
          {installed.map((tpl) => (
            <button
              key={tpl.templateID}
              className="flex w-full items-center gap-2 px-3 py-2 text-left text-xs hover:bg-muted"
              onClick={() => { setOpen(false); navigate(`/templates/${tpl.templateID}`); }}
            >
              <Package className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
              <span className="font-mono truncate flex-1 min-w-0">{tpl.templateID}</span>
              <Badge
                tone={tpl.status?.toUpperCase() === 'READY' ? 'ok' : 'warn'}
                className="ml-auto text-xs shrink-0"
              >
                {tpl.status}
              </Badge>
            </button>
          ))}
        </div>,
        document.body,
      )}
    </div>
  );
}

// ── StoreCard ─────────────────────────────────────────────────────────────────

interface StoreCardProps {
  item: StoreCatalogItem;
  installed: TemplateSummary[];
  onInstall: () => void;
  onInstallAndEnable: () => void;
  onEnableInstalled: (template: TemplateSummary) => void;
  enabling?: boolean;
  liveMeta?: ImageMeta;
}

function StoreCard({ item, installed, onInstall, onInstallAndEnable, onEnableInstalled, enabling = false, liveMeta }: StoreCardProps) {
  const { t } = useTranslation('store');
  const installedDigest = installed.length > 0
    ? (installed[0].imageInfo ?? '').split('sha256:')[1]
      ? 'sha256:' + (installed[0].imageInfo ?? '').split('sha256:')[1]
      : null
    : null;
  const latestDigest = liveMeta?.digest_short ?? null;
  const hasUpdate = installedDigest != null && latestDigest != null && installedDigest !== latestDigest;
  const displaySizeMb = liveMeta?.size_mb ?? item.size_mb;
  const Icon = categoryIcon(item.category);
  const isInstalled = installed.length > 0;

  return (
    <Card className="flex flex-col h-full relative overflow-hidden">
      {item.official && !hasUpdate && (
        <span className="absolute top-2.5 right-2.5 rounded-full bg-primary/10 px-2 py-0.5 text-xs font-medium text-primary ring-1 ring-primary/20">
          {t('official')}
        </span>
      )}
      {hasUpdate && (
        <span className="absolute top-2.5 right-2.5 flex items-center gap-1 rounded-full bg-amber-500/15 px-2 py-0.5 text-xs font-medium text-amber-500 ring-1 ring-amber-500/30">
          <AlertTriangle size={10} />
          {t('hasUpdate')}
        </span>
      )}

      <div className="p-4 flex-1 space-y-3">
        {/* icon + name */}
        <div className="flex items-center gap-3">
          <span className="flex h-10 w-10 items-center justify-center rounded-lg bg-gradient-to-br from-primary/20 to-cube-accent/20 text-primary ring-1 ring-primary/20">
            <Icon size={18} />
          </span>
          <div>
            <p className="text-sm font-semibold leading-tight">{item.image.split("/").pop()}</p>
            <p className="text-xs text-muted-foreground mt-0.5 text-num">{displaySizeMb >= 1000 ? (displaySizeMb / 1024).toFixed(1) + " GB" : displaySizeMb + " MB"}</p>
          </div>
        </div>

        {/* description */}
        <p className="text-xs text-muted-foreground leading-relaxed">
          {t(item.description_key as 'official', { defaultValue: '' })}
        </p>

        {/* tags */}
        <div className="flex flex-wrap gap-1">
          {item.tags.map((tag) => (
            <Badge key={tag} tone="mute" className="text-xs">
              {t(`tagLabels.${tag}` as 'official', { defaultValue: tag })}
            </Badge>
          ))}
        </div>
      </div>

      {/* footer */}
      <div className="border-t px-4 py-3 space-y-2">
        <p className="text-xs text-muted-foreground font-mono break-all leading-relaxed">
          {item.image}
        </p>
        <div className="flex flex-wrap justify-end gap-2">
          {isInstalled && isOpenClawTemplate(item) && (
            <Button
              size="sm"
              disabled={enabling}
              onClick={() => onEnableInstalled(installed[0])}
            >
              {enabling ? <Loader2 className="mr-1.5 h-3.5 w-3.5 animate-spin" /> : null}
              {t('enableAgentHub')}
            </Button>
          )}
          {isInstalled ? (
            <InstalledDropdown installed={installed} onInstallAnother={onInstall} />
          ) : isOpenClawTemplate(item) ? (
            <>
              <Button size="sm" onClick={onInstallAndEnable}>{t('installAndEnableAgentHub')}</Button>
              <Button size="sm" variant="outline" onClick={onInstall}>{t('installOnly')}</Button>
            </>
          ) : (
            <Button size="sm" onClick={onInstall}>{t('install')}</Button>
          )}
        </div>
      </div>
    </Card>
  );
}

// ── main page ─────────────────────────────────────────────────────────────────

export default function TemplateStorePage() {
  const [category, setCategory] = useState<CategoryId>('all');
  const [search, setSearch] = useState('');
  const [installing, setInstalling] = useState<StoreTemplate | null>(null);
  const [enableAfterInstall, setEnableAfterInstall] = useState(false);
  const [enablingTemplateId, setEnablingTemplateId] = useState<string | null>(null);
  const { t } = useTranslation('store');
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();

  useEffect(() => {
    const q = searchParams.get('search');
    if (q) setSearch(q);
  }, [searchParams]);

  const { data: templates, isLoading } = useQuery({
    queryKey: ['templates'],
    queryFn: templateApi.list,
    refetchInterval: 30_000,
  });

  const { data: storeCatalog } = useQuery({
    queryKey: ['store-catalog'],
    queryFn: storeApi.catalog,
    staleTime: 5 * 60 * 1000, // 5 minutes
  });

  const { data: storeMeta, refetch: refetchMeta } = useQuery({
    queryKey: ['store-meta'],
    queryFn: storeApi.meta,
    refetchInterval: 6 * 60 * 60 * 1000, // 6 hours
    staleTime: 60 * 60 * 1000,            // 1 hour
  });

  const { mutate: checkUpdates, isPending: isChecking } = useMutation({
    mutationFn: storeApi.refresh,
    onSuccess: (data) => {
      qc.setQueryData(['store-meta'], data);
      // 统计有多少镜像 digest 与当前已安装模板不一致
      const currentTemplates = qc.getQueryData<typeof templates>(['templates']) ?? [];
      const updatedCount = data.images.filter((img) => {
        const latestDigest = img.digest_short;
        if (!latestDigest) return false;
        return currentTemplates.some((tpl) => {
          const info = tpl.imageInfo ?? '';
          if (!info.includes(img.image.split(':')[0])) return false;
          const installedDigest = info.includes('sha256:')
            ? 'sha256:' + info.split('sha256:')[1]
            : null;
          return installedDigest != null && installedDigest !== latestDigest;
        });
      }).length;
      if (updatedCount > 0) {
        showToast(t('toast.updatesFound', { count: updatedCount }), 'warn');
      } else {
        showToast(t('toast.upToDate'));
      }
    },
    onError: () => {
      showToast(t('toast.checkFailed'), 'warn');
    },
  });

  const filtered = (storeCatalog ?? []).filter((item) => {
    if (category !== 'all' && item.category !== category) return false;
    if (search.trim()) {
      const q = search.toLowerCase();
      const name = t(item.name_key as 'official', { defaultValue: '' }).toLowerCase();
      const description = t(item.description_key as 'official', { defaultValue: '' }).toLowerCase();
      return (
        name.includes(q) ||
        description.includes(q) ||
        item.image.toLowerCase().includes(q) ||
        item.id.toLowerCase().includes(q) ||
        item.tags.some((tag) => {
          const label = t(`tagLabels.${tag}` as 'official', { defaultValue: tag }).toLowerCase();
          return tag.toLowerCase().includes(q) || label.includes(q);
        })
      );
    }
    return true;
  });

  const enableInstalledTemplate = useCallback(
    async (item: StoreTemplate, template: TemplateSummary) => {
      setEnablingTemplateId(template.templateID);
      try {
        await agentHubApi.registerMarketTemplate({
          templateId: template.templateID,
          name: t(item.name_key as 'official', { defaultValue: item.id }),
          model: 'deepseek/deepseek-v4-flash',
          version: item.id,
          recommended: true,
        });
        showToast(t('toast.agentTemplateEnabled'));
        navigate(`/agenthub?createTemplate=${encodeURIComponent(template.templateID)}`);
      } catch (err) {
        showToast(err instanceof Error ? err.message : String(err), 'warn');
      } finally {
        setEnablingTemplateId(null);
      }
    },
    [navigate, t],
  );

  return (
    <div className="animate-fade-in space-y-5">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t('subtitle')}</p>
        </div>
        <div className="flex items-center gap-2">
          <Button
            variant="outline"
            size="sm"
            onClick={() => checkUpdates()}
            disabled={isChecking}
            className="gap-1.5 text-xs"
          >
            <RefreshCw className={isChecking ? "h-3.5 w-3.5 animate-spin" : "h-3.5 w-3.5"} />
            {isChecking ? t('checking') : t('checkUpdates')}
          </Button>
          <div className="relative w-56">
          <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-muted-foreground" />
          <Input
            className="pl-8 h-9 text-sm"
            placeholder={t('searchPlaceholder')}
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
          </div>
        </div>
      </header>

      {/* category tabs */}
      <div className="flex gap-1.5 flex-wrap">
        {CATEGORIES.map((cat) => (
          <button
            key={cat.id}
            onClick={() => setCategory(cat.id as CategoryId)}
            className={cn(
              'rounded-full px-3 py-1 text-xs font-medium transition-colors',
              category === cat.id
                ? 'bg-primary text-primary-foreground'
                : 'bg-muted text-muted-foreground hover:bg-muted/80 hover:text-foreground',
            )}
          >
            {t('categories.' + cat.id, cat.label)}
          </button>
        ))}
      </div>

      {/* grid */}
      {isLoading ? (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {Array.from({ length: 3 }).map((_, i) => (
            <Skeleton key={i} className="h-48" />
          ))}
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {filtered.map((item) => (
            <StoreCard
              key={item.id}
              item={item}
              installed={getInstalledTemplates(item, templates ?? [])}
              onInstall={() => {
                setEnableAfterInstall(false);
                setInstalling(item);
              }}
              onInstallAndEnable={() => {
                setEnableAfterInstall(true);
                setInstalling(item);
              }}
              onEnableInstalled={(template) => enableInstalledTemplate(item, template)}
              enabling={getInstalledTemplates(item, templates ?? []).some((tpl) => tpl.templateID === enablingTemplateId)}
              liveMeta={storeMeta?.images.find((m) => m.image === item.image)}
            />
          ))}
          {filtered.length === 0 && (
            <div className="col-span-3 py-16 text-center text-sm text-muted-foreground">
              {t('noResults')}
            </div>
          )}
        </div>
      )}

      {installing && (
        <InstallModal
          item={installing}
          enableForAgentHub={enableAfterInstall}
          onClose={() => setInstalling(null)}
        />
      )}
    </div>
  );
}
