// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import * as Dialog from '@radix-ui/react-dialog';
import { X, KeyRound, Check, Loader2, ShieldCheck, TriangleAlert, Globe } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { cn } from '@/lib/utils';
import { agentHubApi, type AgentSettingsDto } from '@/api/client';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Fired after a successful save so callers can refresh onboarding state. */
  onSaved?: (settings: AgentSettingsDto) => void;
}

type SettingsSourceKey = 'database' | 'none';

function settingsSourceKey(source: string | undefined): SettingsSourceKey {
  return source === 'database' ? 'database' : 'none';
}

export function AgentSettingsDialog({ open, onOpenChange, onSaved }: Props) {
  const { t } = useTranslation('agentHub');
  const [settings, setSettings] = useState<AgentSettingsDto | null>(null);
  const [provider, setProvider] = useState('deepseek');
  const [baseUrl, setBaseUrl] = useState('https://api.deepseek.com');
  const [model, setModel] = useState('deepseek/deepseek-v4-flash');
  const [apiKey, setApiKey] = useState('');
  const [credentialMode, setCredentialMode] = useState<'egress' | 'env'>('egress');
  const [domain, setDomain] = useState('');
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    setApiKey('');
    setSaved(false);
    agentHubApi
      .getSettings()
      .then((s) => {
        if (!cancelled) {
          setSettings(s);
          setProvider(s.llmProvider || 'deepseek');
          setBaseUrl(s.llmBaseUrl || 'https://api.deepseek.com');
          setModel(s.llmModel || 'deepseek/deepseek-v4-flash');
          setCredentialMode(s.llmCredentialMode || 'egress');
          setDomain(s.gatewayDomain ?? '');
        }
      })
      .catch((e) => {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open]);

  const persistenceDisabled = settings ? !settings.persistenceEnabled : false;
  const configured = settings?.llmApiKeyConfigured ?? settings?.deepseekApiKeyConfigured ?? false;

  const key = apiKey.trim();
  const savedProvider = (settings?.llmProvider ?? 'deepseek').trim();
  const savedBaseUrl = (settings?.llmBaseUrl ?? 'https://api.deepseek.com').trim();
  const savedModel = (settings?.llmModel ?? 'deepseek/deepseek-v4-flash').trim();
  const savedCredentialMode = settings?.llmCredentialMode ?? 'egress';
  const savedDomain = (settings?.gatewayDomain ?? '').trim();
  const llmChanged =
    provider.trim() !== savedProvider ||
    baseUrl.trim() !== savedBaseUrl ||
    model.trim() !== savedModel ||
    credentialMode !== savedCredentialMode;
  const domainChanged = domain.trim() !== savedDomain;
  const canSave = !persistenceDisabled && (!!key || llmChanged || domainChanged);

  const save = async () => {
    if (!key && !llmChanged && !domainChanged) {
      setError(t('settings.errors.required'));
      return;
    }
    setSaving(true);
    setError(null);
    try {
      const body: {
        llmProvider?: string;
        llmBaseUrl?: string;
        llmModel?: string;
        llmApiKey?: string;
        llmCredentialMode?: 'egress' | 'env';
        gatewayDomain?: string;
      } = {};
      if (llmChanged) {
        body.llmProvider = provider.trim();
        body.llmBaseUrl = baseUrl.trim();
        body.llmModel = model.trim();
        body.llmCredentialMode = credentialMode;
      }
      if (key) body.llmApiKey = key;
      if (domainChanged) body.gatewayDomain = domain.trim();
      const next = await agentHubApi.updateSettings(body);
      setSettings(next);
      setProvider(next.llmProvider || 'deepseek');
      setBaseUrl(next.llmBaseUrl || 'https://api.deepseek.com');
      setModel(next.llmModel || 'deepseek/deepseek-v4-flash');
      setCredentialMode(next.llmCredentialMode || 'egress');
      setApiKey('');
      setDomain(next.gatewayDomain ?? '');
      setSaved(true);
      onSaved?.(next);
      setTimeout(() => setSaved(false), 1600);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content
          className={cn(
            'fixed left-1/2 top-1/2 z-50 w-[min(560px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2',
            'max-h-[calc(100vh-3rem)] overflow-hidden rounded-2xl border border-border/60 bg-card shadow-2xl',
            'flex flex-col'
          )}
        >
          <div className="flex items-center justify-between border-b border-border/60 px-6 py-4">
            <Dialog.Title className="flex items-center gap-2 text-base font-semibold">
              <KeyRound size={16} className="text-primary" />
              {t('settings.title')}
            </Dialog.Title>
            <Dialog.Close asChild>
              <button
                type="button"
                aria-label="close"
                className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              >
                <X size={16} />
              </button>
            </Dialog.Close>
          </div>

          <div className="space-y-4 overflow-y-auto px-6 py-5">
            <p className="text-sm text-muted-foreground">{t('settings.description')}</p>

            {loading ? (
              <div className="flex items-center gap-2 text-sm text-muted-foreground">
                <Loader2 size={14} className="animate-spin" />
                {t('settings.loading')}
              </div>
            ) : (
              <div
                className={cn(
                  'flex items-center gap-2 rounded-lg border px-3 py-2 text-sm',
                  configured
                    ? 'border-emerald-200/70 bg-emerald-50 text-emerald-700 dark:border-emerald-500/30 dark:bg-emerald-500/10 dark:text-emerald-300'
                    : 'border-amber-200/70 bg-amber-50 text-amber-800 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-200'
                )}
              >
                {configured ? <ShieldCheck size={15} /> : <TriangleAlert size={15} />}
                <span>
                  {configured
                    ? t('settings.status.configured', {
                        provider: settings?.llmProvider ?? 'deepseek',
                        model: settings?.llmModel ?? 'deepseek/deepseek-v4-flash',
                        masked: settings?.llmApiKeyMasked ?? settings?.deepseekApiKeyMasked ?? '',
                        source: t(
                          `settings.source.${settingsSourceKey(settings?.llmApiKeySource ?? settings?.source)}`
                        ),
                        mode: t(`settings.credentialMode.${settings?.llmCredentialMode ?? 'egress'}`),
                      })
                    : t('settings.status.missing')}
                </span>
              </div>
            )}

            <div className="space-y-2">
              <label className="text-sm font-medium" htmlFor="agenthub-llm-provider">
                {t('settings.provider.label')}
              </label>
              <select
                id="agenthub-llm-provider"
                value={provider}
                disabled={saving || persistenceDisabled}
                onChange={(e) => {
                  const next = e.target.value;
                  setProvider(next);
                  if (next === 'deepseek') {
                    setBaseUrl('https://api.deepseek.com');
                    if (!model.trim() || model === savedModel) {
                      setModel('deepseek/deepseek-v4-flash');
                    }
                  }
                  if (error) setError(null);
                }}
                className="h-10 w-full rounded-lg border border-border/60 bg-background px-3 text-sm text-foreground outline-none transition-colors focus:border-primary/60 focus:ring-2 focus:ring-primary/15 disabled:cursor-not-allowed disabled:opacity-50"
              >
                <option value="deepseek">{t('settings.provider.deepseek')}</option>
                <option value="openai-compatible">{t('settings.provider.openaiCompatible')}</option>
                <option value="custom">{t('settings.provider.custom')}</option>
              </select>
            </div>

            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-2">
                <label className="text-sm font-medium" htmlFor="agenthub-llm-base-url">
                  {t('settings.baseUrl.label')}
                </label>
                <Input
                  id="agenthub-llm-base-url"
                  type="url"
                  placeholder="https://api.deepseek.com"
                  value={baseUrl}
                  disabled={saving || persistenceDisabled}
                  spellCheck={false}
                  onChange={(e) => {
                    setBaseUrl(e.target.value);
                    if (error) setError(null);
                  }}
                />
                <p className="text-xs text-muted-foreground">{t('settings.baseUrl.hint')}</p>
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium" htmlFor="agenthub-llm-model">
                  {t('settings.model.label')}
                </label>
                <Input
                  id="agenthub-llm-model"
                  type="text"
                  placeholder="deepseek/deepseek-v4-flash"
                  value={model}
                  disabled={saving || persistenceDisabled}
                  spellCheck={false}
                  onChange={(e) => {
                    setModel(e.target.value);
                    if (error) setError(null);
                  }}
                />
                <p className="text-xs text-muted-foreground">{t('settings.model.hint')}</p>
              </div>
            </div>

            <div className="space-y-2">
              <label className="text-sm font-medium" htmlFor="agenthub-llm-key">
                {t('settings.apiKey.label')}
              </label>
              <Input
                id="agenthub-llm-key"
                type="password"
                placeholder="sk-…"
                value={apiKey}
                disabled={saving || persistenceDisabled}
                autoComplete="off"
                onChange={(e) => {
                  setApiKey(e.target.value);
                  if (error) setError(null);
                }}
              />
              <p className="text-xs text-muted-foreground">{t('settings.apiKey.hint')}</p>
            </div>

            <div className="space-y-2">
              <label className="flex items-center gap-1.5 text-sm font-medium" htmlFor="agenthub-credential-mode">
                <ShieldCheck size={14} className="text-primary" />
                {t('settings.credentialMode.label')}
              </label>
              <select
                id="agenthub-credential-mode"
                value={credentialMode}
                disabled={saving || persistenceDisabled}
                onChange={(e) => {
                  setCredentialMode(e.target.value === 'env' ? 'env' : 'egress');
                  if (error) setError(null);
                }}
                className="h-10 w-full rounded-lg border border-border/60 bg-background px-3 text-sm text-foreground outline-none transition-colors focus:border-primary/60 focus:ring-2 focus:ring-primary/15 disabled:cursor-not-allowed disabled:opacity-50"
              >
                <option value="egress">{t('settings.credentialMode.egress')}</option>
                <option value="env">{t('settings.credentialMode.env')}</option>
              </select>
              <p className="text-xs text-muted-foreground">
                {credentialMode === 'egress'
                  ? t('settings.credentialMode.egressHint')
                  : t('settings.credentialMode.envHint')}
              </p>
            </div>

            <div className="space-y-2">
              <label className="flex items-center gap-1.5 text-sm font-medium" htmlFor="agenthub-gateway-domain">
                <Globe size={14} className="text-primary" />
                {t('settings.gatewayDomain.label')}
              </label>
              <Input
                id="agenthub-gateway-domain"
                type="text"
                placeholder="cube.app"
                value={domain}
                disabled={saving || persistenceDisabled}
                autoComplete="off"
                spellCheck={false}
                onChange={(e) => {
                  setDomain(e.target.value);
                  if (error) setError(null);
                }}
              />
              <p className="text-xs text-muted-foreground">{t('settings.gatewayDomain.hint')}</p>
            </div>

            {persistenceDisabled && (
              <div className="rounded-lg border border-amber-200/70 bg-amber-50 px-3 py-2 text-xs text-amber-800 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-200">
                {t('settings.persistenceDisabled')}
              </div>
            )}
            {error && <p className="text-xs text-rose-500">{error}</p>}
          </div>

          <div className="flex items-center justify-end gap-2 border-t border-border/60 px-6 py-3">
            <Dialog.Close asChild>
              <Button variant="outline" size="sm">
                {t('settings.actions.close')}
              </Button>
            </Dialog.Close>
            <Button
              size="sm"
              onClick={save}
              disabled={saving || !canSave}
              className="min-w-[96px] gap-1.5"
            >
              {saving ? (
                <Loader2 size={14} className="animate-spin" />
              ) : saved ? (
                <Check size={14} />
              ) : (
                <KeyRound size={14} />
              )}
              {saved ? t('settings.actions.saved') : t('settings.actions.save')}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
