// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import * as Dialog from '@radix-ui/react-dialog';
import { X, Check, Wrench, TriangleAlert } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { cn } from '@/lib/utils';
import { AgentAvatar } from './AgentAvatar';
import { useAgentStore } from '@/state/agentStore';
import type { Agent, AgentEngine, AgentPersistenceMode } from '@/data/agents';
import { agentHubApi, type AgentTemplateDto } from '@/api/client';

const DEFAULT_LLM_MODEL = 'deepseek/deepseek-v4-flash';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  initialTemplateId?: string;
  /** Whether a DeepSeek API key is configured; when false, creation is blocked. */
  apiKeyConfigured?: boolean;
  /** Default LLM model configured in AgentHub settings. */
  llmModel?: string;
  /** Opens the API key settings flow from the guidance banner. */
  onConfigureApiKey?: () => void;
  onError?: (message: string) => void;
}

export function CreateAgentDialog({
  open,
  onOpenChange,
  initialTemplateId = '',
  apiKeyConfigured = true,
  llmModel = DEFAULT_LLM_MODEL,
  onConfigureApiKey,
  onError,
}: Props) {
  const { t } = useTranslation('agentHub');

  const addAgent = useAgentStore((s) => s.addAgent);
  const removeAgent = useAgentStore((s) => s.removeAgent);

  const [name, setName] = useState('');
  const [engine, setEngine] = useState<AgentEngine>('openclaw');
  const [botId, setBotId] = useState('');
  const [secret, setSecret] = useState('');
  const [persistenceMode, setPersistenceMode] =
    useState<AgentPersistenceMode>('shared_files');
  const [templates, setTemplates] = useState<AgentTemplateDto[]>([]);
  const [selectedTemplateId, setSelectedTemplateId] = useState('');
  const [templatesLoading, setTemplatesLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setTemplatesLoading(true);
    agentHubApi
      .listTemplates()
      .then((items) => {
        if (!cancelled) setTemplates(items);
      })
      .catch(() => {
        if (!cancelled) setTemplates([]);
      })
      .finally(() => {
        if (!cancelled) setTemplatesLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open]);

  useEffect(() => {
    if (open) setSelectedTemplateId(initialTemplateId);
  }, [initialTemplateId, open]);

  const selectedTemplate = templates.find((template) => template.templateId === selectedTemplateId);
  const inheritedPersistenceMode = selectedTemplate?.persistenceMode;
  const effectivePersistenceMode = inheritedPersistenceMode ?? persistenceMode;

  const reset = () => {
    setName('');
    setEngine('openclaw');
    setBotId('');
    setSecret('');
    setPersistenceMode('shared_files');
    setSelectedTemplateId('');
    setError(null);
    setSubmitting(false);
  };

  const handleSubmit = async () => {
    const n = name.trim();
    if (!n) {
      setError(t('dialog.errors.nameRequired'));
      return;
    }
    if (!apiKeyConfigured) {
      setError(t('dialog.errors.apiKeyRequired'));
      return;
    }
    const payload = {
      name: n,
      engine: 'openclaw' as const,
      templateId: selectedTemplateId || undefined,
      persistenceMode: effectivePersistenceMode,
      botId: botId.trim() || undefined,
      botSecret: secret.trim() || undefined,
    };
    // 乐观占位：列表里立即出现「孵化中…」卡片，对话框秒级关闭
    const placeholderId = `pending:create:${Date.now()}`;
    const placeholder: Agent = {
      id: placeholderId,
      name: n,
      status: 'starting',
      engine: 'openclaw',
      env: 'linux',
      model: llmModel,
      version: '-',
      bots: [],
      botsAvailable: [],
      avatar: n,
      avatarTone: 'sky',
      persistenceMode: effectivePersistenceMode,
    };
    setSubmitting(true);
    setError(null);
    addAgent(placeholder);
    reset();
    onOpenChange(false);
    try {
      const created = await agentHubApi.create(payload);
      removeAgent(placeholderId);
      addAgent(created);
    } catch (e) {
      removeAgent(placeholderId);
      onError?.(e instanceof Error ? e.message : String(e));
    }
  };

  const handleOpenChange = (next: boolean) => {
    if (!next) reset();
    onOpenChange(next);
  };

  return (
    <Dialog.Root open={open} onOpenChange={handleOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay
          className="fixed inset-0 z-40 bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in"
        />
        <Dialog.Content
          className={cn(
            'fixed left-1/2 top-1/2 z-50 w-[min(720px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2',
            'max-h-[calc(100vh-3rem)] overflow-hidden rounded-2xl border border-border/60 bg-card shadow-2xl',
            'flex flex-col'
          )}
        >
          {/* Header */}
          <div className="flex items-center justify-between border-b border-border/60 px-6 py-4">
            <Dialog.Title className="text-base font-semibold">
              {t('dialog.title')}
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

          {/* Body */}
          <div className="space-y-6 overflow-y-auto px-6 py-5">
            {!apiKeyConfigured && (
              <div className="flex items-start gap-3 rounded-lg border border-amber-200/70 bg-amber-50 px-4 py-3 text-sm text-amber-900 dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-100">
                <TriangleAlert size={16} className="mt-0.5 shrink-0" />
                <div className="flex-1">
                  <p className="font-medium">{t('dialog.apiKeyRequired.title')}</p>
                  <p className="mt-0.5 text-xs leading-relaxed text-amber-800/90 dark:text-amber-100/80">
                    {t('dialog.apiKeyRequired.description')}
                  </p>
                </div>
                <Button size="sm" variant="outline" onClick={() => onConfigureApiKey?.()}>
                  {t('dialog.apiKeyRequired.action')}
                </Button>
              </div>
            )}
            {/* * 助手信息 */}
            <Section label={t('dialog.sections.info')} required>
              <div className="flex items-center gap-3 rounded-lg border border-border/60 bg-background px-3 py-2 focus-within:border-primary/60 focus-within:ring-2 focus-within:ring-primary/15">
                <AgentAvatar seed={name || 'new-assistant'} size={36} />
                <input
                  value={name}
                  onChange={(e) => {
                    setName(e.target.value);
                    if (error) setError(null);
                  }}
                  placeholder={t('dialog.namePlaceholder')}
                  className="flex-1 bg-transparent text-sm outline-none placeholder:text-muted-foreground/60"
                  autoFocus
                  maxLength={32}
                />
              </div>
              {error && <p className="mt-1 text-xs text-rose-500">{error}</p>}
            </Section>

            {/* * 工作引擎 */}
            <Section label={t('dialog.sections.engine')} required>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <EngineCard
                  selected={engine === 'openclaw'}
                  title={t('dialog.engineOptions.openclaw.title')}
                  desc={t('dialog.engineOptions.openclaw.description')}
                  glyph="O"
                  glyphBg="bg-rose-500/10 text-rose-500 ring-rose-500/30"
                  onClick={() => setEngine('openclaw')}
                />
                <EngineCard
                  disabled
                  title={t('dialog.engineOptions.hermes.title')}
                  desc={t('dialog.engineOptions.hermes.description')}
                  glyph="H"
                  glyphBg="bg-violet-500/10 text-violet-500 ring-violet-500/30"
                  badge={t('dialog.engineOptions.hermes.comingSoon')}
                />
              </div>
            </Section>

            {/* * 助手模板 */}
            <Section label={t('dialog.sections.template')} hint={t('dialog.templateHint')}>
              <select
                value={selectedTemplateId}
                disabled={submitting || templatesLoading}
                onChange={(e) => setSelectedTemplateId(e.target.value)}
                className="h-10 w-full rounded-lg border border-border/60 bg-background px-3 text-sm text-foreground outline-none transition-colors focus:border-primary/60 focus:ring-2 focus:ring-primary/15 disabled:cursor-not-allowed disabled:opacity-50"
              >
                <option value="">
                  {templatesLoading
                    ? t('dialog.templateLoading')
                    : t('dialog.templateOptions.default')}
                </option>
                {templates.map((template) => (
                  <option key={template.templateId} value={template.templateId}>
                    {template.name} · {template.model} · {template.version}
                  </option>
                ))}
              </select>
              {selectedTemplate && (
                <div className="mt-2 rounded-lg border border-border/60 bg-muted/20 p-3 text-xs">
                  <div className="font-medium text-foreground">
                    {t('dialog.templateSelected', { templateId: selectedTemplate.templateId })}
                  </div>
                  <div className="mt-2 grid gap-1.5 text-muted-foreground sm:grid-cols-2">
                    <span>{t('dialog.templateFields.model')}: {selectedTemplate.model}</span>
                    <span>{t('dialog.templateFields.version')}: {selectedTemplate.version}</span>
                    {inheritedPersistenceMode && (
                      <span>
                        {t('dialog.templateFields.persistence')}: {t(`dialog.persistenceOptions.${inheritedPersistenceMode === 'shared_files' ? 'sharedFiles' : 'fullSnapshot'}.title`)}
                      </span>
                    )}
                    <span>{t('dialog.templateFields.sourceAgent')}: {selectedTemplate.sourceAgentId}</span>
                    <span>{t('dialog.templateFields.createdAt')}: {selectedTemplate.createdAt || '-'}</span>
                  </div>
                </div>
              )}
            </Section>

            {/* * 状态管理模式 */}
            <Section
              label={t('dialog.sections.persistence')}
              hint={
                inheritedPersistenceMode
                  ? t('dialog.persistenceInheritedHint')
                  : t('dialog.persistenceHint')
              }
            >
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <EngineCard
                  selected={effectivePersistenceMode === 'shared_files'}
                  disabled={Boolean(inheritedPersistenceMode)}
                  title={t('dialog.persistenceOptions.sharedFiles.title')}
                  desc={t('dialog.persistenceOptions.sharedFiles.description')}
                  glyph="F"
                  glyphBg="bg-emerald-500/10 text-emerald-500 ring-emerald-500/30"
                  onClick={() => {
                    if (!inheritedPersistenceMode) setPersistenceMode('shared_files');
                  }}
                />
                <EngineCard
                  selected={effectivePersistenceMode === 'full_snapshot'}
                  disabled={Boolean(inheritedPersistenceMode)}
                  title={t('dialog.persistenceOptions.fullSnapshot.title')}
                  desc={t('dialog.persistenceOptions.fullSnapshot.description')}
                  glyph="S"
                  glyphBg="bg-sky-500/10 text-sky-500 ring-sky-500/30"
                  onClick={() => {
                    if (!inheritedPersistenceMode) setPersistenceMode('full_snapshot');
                  }}
                />
              </div>
            </Section>

            {/* * 模型 */}
            <Section label={t('dialog.sections.model')} required>
              <div className="flex items-center justify-between rounded-lg border border-border/60 bg-background px-3 py-2.5 text-sm">
                <div className="flex items-center gap-2">
                  <span className="inline-flex h-5 w-5 items-center justify-center rounded-md bg-primary/15 text-[10px] font-bold text-primary">
                    DS
                  </span>
                  <span className="font-medium">{llmModel}</span>
                </div>
                <span className="text-xs text-muted-foreground">
                  {t('dialog.modelHint')}
                </span>
              </div>
            </Section>

            {/* 填入配置 */}
            <Section label={t('dialog.sections.botConfig')} hint={t('dialog.botConfigHint')}>
              <div className="rounded-lg border border-dashed border-border/70 bg-muted/20 px-4 py-3">
                <FieldRow label={t('dialog.fields.botId')}>
                  <Input
                    value={botId}
                    onChange={(e) => setBotId(e.target.value)}
                    placeholder={t('dialog.fields.botIdPlaceholder')}
                  />
                </FieldRow>
                <div className="my-3 h-px bg-border/60" />
                <FieldRow label={t('dialog.fields.secret')}>
                  <Input
                    type="password"
                    value={secret}
                    onChange={(e) => setSecret(e.target.value)}
                    placeholder={t('dialog.fields.secretPlaceholder')}
                  />
                </FieldRow>
              </div>
            </Section>
          </div>

          {/* Footer */}
          <div className="flex items-center justify-between border-t border-border/60 px-6 py-3">
            <Dialog.Close asChild>
              <Button variant="outline" size="sm">
                {t('dialog.actions.cancel')}
              </Button>
            </Dialog.Close>
            <Button
              size="sm"
              onClick={handleSubmit}
              disabled={!name.trim() || submitting || !apiKeyConfigured}
              className="min-w-[88px]"
            >
              {submitting ? t('dialog.actions.submitting') : t('dialog.actions.submit')}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function Section({
  label,
  required = false,
  hint,
  children,
}: {
  label: string;
  required?: boolean;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <section>
      <h3 className="mb-2 text-sm font-medium">
        {required && <span className="mr-1 text-rose-500">*</span>}
        {label}
      </h3>
      {hint && <p className="-mt-1 mb-2 text-xs text-muted-foreground">{hint}</p>}
      {children}
    </section>
  );
}

function EngineCard({
  selected = false,
  disabled = false,
  title,
  desc,
  glyph,
  glyphBg,
  badge,
  onClick,
}: {
  selected?: boolean;
  disabled?: boolean;
  title: string;
  desc: string;
  glyph: string;
  glyphBg: string;
  badge?: string;
  onClick?: () => void;
}) {
  return (
    <button
      type="button"
      disabled={disabled}
      onClick={onClick}
      className={cn(
        'relative flex w-full items-start gap-3 rounded-xl border p-4 text-left transition-all',
        selected
          ? 'border-primary/60 bg-primary/5 ring-2 ring-primary/30'
          : 'border-border/60 bg-background hover:bg-muted/40',
        disabled && 'cursor-not-allowed opacity-60 hover:bg-background'
      )}
    >
      <span
        className={cn(
          'flex h-8 w-8 shrink-0 items-center justify-center rounded-lg text-sm font-bold ring-1',
          glyphBg
        )}
      >
        {glyph}
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <span className="text-sm font-semibold">{title}</span>
          {badge && (
            <span className="inline-flex items-center gap-1 rounded-full bg-amber-500/10 px-2 py-0.5 text-[10px] font-medium text-amber-600 ring-1 ring-amber-500/30 dark:text-amber-300">
              <Wrench size={9} />
              {badge}
            </span>
          )}
        </div>
        <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{desc}</p>
      </div>
      {selected && !disabled && (
        <span className="absolute right-3 top-3 inline-flex h-5 w-5 items-center justify-center rounded-full bg-primary text-primary-foreground">
          <Check size={12} />
        </span>
      )}
    </button>
  );
}

function FieldRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center gap-3 text-sm">
      <label className="w-16 shrink-0 text-muted-foreground">{label}</label>
      <div className="flex-1">{children}</div>
    </div>
  );
}
