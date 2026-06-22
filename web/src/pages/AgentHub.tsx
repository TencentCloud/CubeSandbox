// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useLocation } from 'react-router-dom';
import * as Dialog from '@radix-ui/react-dialog';
import * as DropdownMenu from '@radix-ui/react-dropdown-menu';
import {
  Pencil,
  RotateCcw,
  ExternalLink,
  Terminal,
  MoreHorizontal,
  Info,
  Plus,
  Check,
  Trash2,
  Download,
  HelpCircle,
  ChevronDown,
  Pause,
  Play,
  X,
  LayoutTemplate,
  GitBranch,
  HeartPulse,
  Loader2,
  Settings as SettingsIcon,
} from 'lucide-react';
import { cn } from '@/lib/utils';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { ROBOT_CHANNELS, type Agent, type RobotChannel } from '@/data/agents';
import { useAgentStore } from '@/state/agentStore';
import { AgentAvatar } from '@/components/agents/AgentAvatar';
import { CreateAgentDialog } from '@/components/agents/CreateAgentDialog';
import { AgentSettingsDialog } from '@/components/agents/AgentSettingsDialog';
import { OnboardingGuide } from '@/components/agents/OnboardingGuide';
import { agentHubApi, templateApi, type AgentOperationDto, type AgentSnapshotDto, type AgentTemplateDto } from '@/api/client';

type Tab = 'personal' | 'team';

const HIDE_AGENT_RECOVER = import.meta.env.VITE_HIDE_AGENT_RECOVER === '1';
const OPENCLAW_GATEWAY_PORT = 18789;
const LOGIN_ENV_PORT = 49999;

const MODEL_OPTIONS = [
  { value: 'DeepSeek V4 Flash', labelKey: 'modelDialog.options.deepseekV4Flash' },
  { value: 'DeepSeek V4 Pro', labelKey: 'modelDialog.options.deepseekV4Pro' },
] as const;

function gatewayTokenFromUrl(sourceUrl?: string): string | undefined {
  if (!sourceUrl || typeof window === 'undefined') return undefined;
  try {
    const source = new URL(sourceUrl, window.location.href);
    return new URLSearchParams(source.hash.replace(/^#/, '')).get('token') ?? undefined;
  } catch {
    return undefined;
  }
}

function buildCubeProxyUrl(
  agent: Agent,
  port: number,
  sourceUrl?: string,
  options?: { gateway?: boolean; gatewayDomain?: string },
): string | undefined {
  if (!agent.sandboxId || typeof window === 'undefined') return sourceUrl;

  // When a gateway domain is configured, each assistant gets its own origin via
  // the `<port>-<sandboxId>.<domain>` subdomain. A distinct origin gives the
  // OpenClaw UI its own localStorage, eliminating WebSocket "crosstalk" without
  // the same-origin clearing workaround. The token rides along in the hash.
  const gatewayDomain = options?.gatewayDomain?.trim();
  if (options?.gateway && gatewayDomain) {
    const token = gatewayTokenFromUrl(sourceUrl);
    return `https://${port}-${agent.sandboxId}.${gatewayDomain}/${
      token ? `#token=${encodeURIComponent(token)}` : ''
    }`;
  }

  let source: URL | undefined;
  if (sourceUrl) {
    try {
      source = new URL(sourceUrl, window.location.href);
    } catch {
      source = undefined;
    }
  }

  const sourcePath = source?.pathname.replace(/^\/+/, '') ?? '';
  // Keep the sandbox proxy URL on the SAME origin as the dashboard (including port)
  // so it is served through the WebUI nginx /sandbox/ forward. Same-origin is what
  // lets clearOpenclawClientState() actually clear the OpenClaw UI's localStorage.
  const cubeProxyBase = window.location.origin;
  const target = new URL(
    `/sandbox/${encodeURIComponent(agent.sandboxId)}/${port}/${sourcePath}`,
    cubeProxyBase,
  );

  source?.searchParams.forEach((value, key) => target.searchParams.set(key, value));
  if (options?.gateway) {
    const wsProtocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const wsUrl = `${wsProtocol}//${window.location.host}/sandbox/${encodeURIComponent(agent.sandboxId)}/${port}/`;
    const token = gatewayTokenFromUrl(sourceUrl);
    target.hash = `ws=${wsUrl}${token ? `&token=${encodeURIComponent(token)}` : ''}`;
  }

  return target.toString();
}

// Demo-grade mitigation for OpenClaw WebSocket "crosstalk": all assistants are
// proxied under the same dashboard origin (/sandbox/<id>/<port>/), so the OpenClaw
// control UI shares one localStorage bucket and may reuse a cached gateway/token
// from another assistant instead of the one in the URL. Clearing the OpenClaw keys
// (everything not prefixed with the dashboard's own `cube.` namespace) right before
// opening forces the freshly opened tab to read the wss address + token from the URL.
// NOTE: real deployments should give each assistant a distinct origin (subdomain),
// which makes this unnecessary.
function clearOpenclawClientState(): void {
  if (typeof window === 'undefined') return;
  try {
    const toRemove: string[] = [];
    for (let i = 0; i < window.localStorage.length; i += 1) {
      const key = window.localStorage.key(i);
      if (key && !key.startsWith('cube.')) toRemove.push(key);
    }
    toRemove.forEach((key) => window.localStorage.removeItem(key));
  } catch {
    // Storage may be unavailable (private mode / blocked); ignore.
  }
}

export default function AgentHubPage() {
  const { t } = useTranslation('agentHub');
  const location = useLocation();
  const [tab, setTab] = useState<Tab>('personal');
  const [dialogOpen, setDialogOpen] = useState(false);
  const [templateListOpen, setTemplateListOpen] = useState(false);
  const [initialTemplateId, setInitialTemplateId] = useState('');
  const [modelAgent, setModelAgent] = useState<Agent | null>(null);
  const [wecomAgent, setWecomAgent] = useState<Agent | null>(null);
  const [createError, setCreateError] = useState<string | null>(null);
  const [selectedAgentId, setSelectedAgentId] = useState<string | null>(null);
  const [previewNoticeDismissed, setPreviewNoticeDismissed] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [onboardingLoading, setOnboardingLoading] = useState(true);
  const [apiKeyConfigured, setApiKeyConfigured] = useState(false);
  const [llmModel, setLlmModel] = useState('deepseek/deepseek-v4-flash');
  const [templateReady, setTemplateReady] = useState(false);

  const userAgents = useAgentStore((s) => s.userAgents);
  const setAgents = useAgentStore((s) => s.setAgents);
  const addAgent = useAgentStore((s) => s.addAgent);
  const updateAgent = useAgentStore((s) => s.updateAgent);
  const removeAgent = useAgentStore((s) => s.removeAgent);
  const setGatewayDomain = useAgentStore((s) => s.setGatewayDomain);
  const agents = useMemo(() => userAgents, [userAgents]);
  const personalCount = agents.length;
  const teamCount = 0;

  useEffect(() => {
    let cancelled = false;
    agentHubApi
      .list()
      .then((items) => {
        if (!cancelled) setAgents(items);
      })
      .catch(() => {
        if (!cancelled) setAgents([]);
      });
    return () => {
      cancelled = true;
    };
  }, [setAgents]);

  // First-run readiness: a DeepSeek API key must be configured and at least one
  // 龙虾助手 template (cluster template installed from the market, or a published
  // assistant template) must exist before assistants can be created.
  const refreshOnboarding = useCallback(() => {
    setOnboardingLoading(true);
    return Promise.allSettled([
      agentHubApi.getSettings(),
      templateApi.list(),
      agentHubApi.listTemplates(),
    ]).then(([settingsRes, clusterRes, agentRes]) => {
      setApiKeyConfigured(
        settingsRes.status === 'fulfilled'
          ? settingsRes.value.llmApiKeyConfigured ?? settingsRes.value.deepseekApiKeyConfigured
          : false,
      );
      setLlmModel(
        settingsRes.status === 'fulfilled'
          ? settingsRes.value.llmModel || 'deepseek/deepseek-v4-flash'
          : 'deepseek/deepseek-v4-flash',
      );
      setGatewayDomain(
        settingsRes.status === 'fulfilled' ? settingsRes.value.gatewayDomain ?? '' : '',
      );
      const clusterHasOpenclaw =
        clusterRes.status === 'fulfilled' &&
        clusterRes.value.some(
          (tpl) =>
            /openclaw|wecom-ds/i.test(tpl.templateID) || /openclaw/i.test(tpl.imageInfo ?? ''),
        );
      const hasAgentTemplate = agentRes.status === 'fulfilled' && agentRes.value.length > 0;
      setTemplateReady(clusterHasOpenclaw || hasAgentTemplate);
      setOnboardingLoading(false);
    });
  }, []);

  useEffect(() => {
    void refreshOnboarding();
  }, [refreshOnboarding]);

  useEffect(() => {
    const templateId = new URLSearchParams(location.search).get('createTemplate')?.trim();
    if (!templateId) return;
    setInitialTemplateId(templateId);
    setDialogOpen(true);
  }, [location.search]);

  return (
    <div className="animate-fade-in space-y-6">
      <header className="flex items-end justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('title')}</h1>
          <p className="mt-1 text-sm text-muted-foreground">{t('subtitle')}</p>
        </div>
        <div className="flex items-center gap-2">
          <Button
            type="button"
            variant="outline"
            className="gap-2"
            onClick={() => setSettingsOpen(true)}
          >
            <SettingsIcon size={16} />
            {t('settings.openAction')}
          </Button>
          <Button
            type="button"
            variant="outline"
            className="gap-2"
            onClick={() => setTemplateListOpen(true)}
          >
            <LayoutTemplate size={16} />
            {t('templates.actions.open')}
          </Button>
        </div>
      </header>

      {!previewNoticeDismissed && (
        <div className="rounded-2xl border border-amber-200/70 bg-amber-50 px-4 py-3 text-sm text-amber-900 shadow-sm dark:border-amber-500/30 dark:bg-amber-500/10 dark:text-amber-100">
          <div className="flex items-start justify-between gap-3">
            <div>
              <div className="flex flex-wrap items-center gap-2">
                <span className="rounded-full bg-amber-200/80 px-2 py-0.5 text-[11px] font-semibold uppercase tracking-wide text-amber-900 dark:bg-amber-400/20 dark:text-amber-100">
                  {t('preview.badge')}
                </span>
                <span className="font-medium">{t('preview.title')}</span>
              </div>
              <p className="mt-1 leading-5">{t('preview.description')}</p>
            </div>
            <button
              type="button"
              className="rounded-full p-1 text-amber-800/70 transition hover:bg-amber-200/60 hover:text-amber-950 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-amber-500 dark:text-amber-100/80 dark:hover:bg-amber-400/20 dark:hover:text-amber-50"
              aria-label={t('preview.close')}
              onClick={() => setPreviewNoticeDismissed(true)}
            >
              <X size={16} />
            </button>
          </div>
        </div>
      )}

      <OnboardingGuide
        loading={onboardingLoading}
        apiKeyConfigured={apiKeyConfigured}
        templateReady={templateReady}
        onConfigureApiKey={() => setSettingsOpen(true)}
      />

      <div className="flex items-center gap-2 rounded-full bg-muted/40 p-1 ring-1 ring-border/60 w-fit">
        <TabButton
          active={tab === 'personal'}
          onClick={() => setTab('personal')}
          label={t('tabs.personal')}
          count={personalCount}
        />
        <TabButton
          active={tab === 'team'}
          onClick={() => setTab('team')}
          label={t('tabs.team')}
          count={teamCount}
        />
      </div>

      {tab === 'personal' ? (
        <PersonalGrid
          agents={agents}
          selectedId={selectedAgentId}
          onSelect={setSelectedAgentId}
          onCreate={() => {
            setInitialTemplateId('');
            setDialogOpen(true);
          }}
          onChangeModel={setModelAgent}
          onConfigureWecom={setWecomAgent}
          onCloned={addAgent}
          onDeleted={removeAgent}
        />
      ) : (
        <TeamComingSoon />
      )}

      <CreateAgentDialog
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        initialTemplateId={initialTemplateId}
        apiKeyConfigured={apiKeyConfigured}
        llmModel={llmModel}
        onConfigureApiKey={() => {
          setDialogOpen(false);
          setSettingsOpen(true);
        }}
        onError={setCreateError}
      />
      <AgentSettingsDialog
        open={settingsOpen}
        onOpenChange={setSettingsOpen}
        onSaved={() => {
          void refreshOnboarding();
        }}
      />
      <ActionErrorDialog
        message={createError}
        onOpenChange={(open) => {
          if (!open) setCreateError(null);
        }}
      />
      <TemplateListDialog
        open={templateListOpen}
        onOpenChange={setTemplateListOpen}
        onUseTemplate={(templateId) => {
          setInitialTemplateId(templateId);
          setTemplateListOpen(false);
          setDialogOpen(true);
        }}
      />
      <ModelDialog
        agent={modelAgent}
        onOpenChange={(open) => {
          if (!open) setModelAgent(null);
        }}
        onSaved={updateAgent}
      />
      <WeComConfigDialog
        agent={wecomAgent}
        onOpenChange={(open) => {
          if (!open) setWecomAgent(null);
        }}
        onSaved={updateAgent}
      />
    </div>
  );
}

function TabButton({
  active,
  onClick,
  label,
  count,
}: {
  active: boolean;
  onClick: () => void;
  label: string;
  count: number;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'flex items-center gap-2 rounded-full px-4 py-1.5 text-sm font-medium transition-colors',
        active
          ? 'bg-background text-foreground shadow-sm ring-1 ring-border/60'
          : 'text-muted-foreground hover:text-foreground'
      )}
    >
      <span>{label}</span>
      <span
        className={cn(
          'rounded-full px-1.5 py-0.5 text-[10px] tabular-nums',
          active ? 'bg-primary/15 text-primary' : 'bg-muted text-muted-foreground'
        )}
      >
        {count}
      </span>
    </button>
  );
}

function TemplateListDialog({
  open,
  onOpenChange,
  onUseTemplate,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onUseTemplate: (templateId: string) => void;
}) {
  const { t } = useTranslation('agentHub');
  const [templates, setTemplates] = useState<AgentTemplateDto[]>([]);
  const [loading, setLoading] = useState(false);
  const [actingTemplateId, setActingTemplateId] = useState<string | null>(null);
  const [renameTemplate, setRenameTemplate] = useState<AgentTemplateDto | null>(null);
  const [deleteTemplate, setDeleteTemplate] = useState<AgentTemplateDto | null>(null);
  const [error, setError] = useState<string | null>(null);

  const loadTemplates = () => {
    setLoading(true);
    setError(null);
    agentHubApi
      .listTemplates()
      .then(setTemplates)
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    if (!open) return;
    loadTemplates();
  }, [open]);

  const handleRenameTemplate = async (template: AgentTemplateDto, nextName: string) => {
    if (!nextName || nextName === template.name) {
      setRenameTemplate(null);
      return;
    }
    setActingTemplateId(template.templateId);
    setError(null);
    try {
      await agentHubApi.updateTemplate(template.templateId, { name: nextName });
      setRenameTemplate(null);
      loadTemplates();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setActingTemplateId(null);
    }
  };

  const handleToggleRecommended = async (template: AgentTemplateDto) => {
    setActingTemplateId(template.templateId);
    setError(null);
    try {
      await agentHubApi.updateTemplate(template.templateId, { recommended: !template.recommended });
      loadTemplates();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setActingTemplateId(null);
    }
  };

  const handleDeleteTemplate = async (template: AgentTemplateDto) => {
    setActingTemplateId(template.templateId);
    setError(null);
    try {
      await agentHubApi.deleteTemplate(template.templateId);
      setDeleteTemplate(null);
      loadTemplates();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setActingTemplateId(null);
    }
  };

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 flex max-h-[calc(100vh-3rem)] w-[min(760px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 flex-col rounded-2xl border border-border/60 bg-card shadow-2xl">
          <div className="flex items-start justify-between gap-4 border-b border-border/60 px-6 py-4">
            <div>
              <Dialog.Title className="text-base font-semibold">
                {t('templates.title')}
              </Dialog.Title>
              <Dialog.Description className="mt-1 text-sm text-muted-foreground">
                {t('templates.description')}
              </Dialog.Description>
            </div>
            <div className="flex items-center gap-2">
              <Button
                type="button"
                size="sm"
                variant="outline"
                disabled={loading}
                onClick={loadTemplates}
              >
                {loading ? t('templates.actions.loading') : t('templates.actions.refresh')}
              </Button>
              <Dialog.Close asChild>
                <button
                  type="button"
                  className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                >
                  <span className="sr-only">{t('templates.actions.close')}</span>
                  <X size={18} />
                </button>
              </Dialog.Close>
            </div>
          </div>

          <div className="overflow-y-auto px-6 py-5">
            {error && (
              <div className="mb-4 rounded-xl border border-rose-500/20 bg-rose-500/10 p-3 text-xs text-rose-500">
                {error}
              </div>
            )}
            {!loading && templates.length === 0 ? (
              <div className="rounded-2xl border border-dashed border-border/70 bg-muted/20 px-6 py-10 text-center">
                <LayoutTemplate className="mx-auto text-muted-foreground/50" size={28} />
                <div className="mt-3 text-sm font-medium">{t('templates.emptyTitle')}</div>
                <p className="mt-1 text-xs text-muted-foreground">{t('templates.emptyDescription')}</p>
              </div>
            ) : (
              <div className="space-y-3">
                {templates.map((template) => (
                  <div
                    key={template.templateId}
                    className="rounded-2xl border border-border/60 bg-background p-4"
                  >
                    <div className="flex flex-wrap items-start justify-between gap-3">
                      <div className="min-w-0">
                        <div className="flex items-center gap-2">
                          <div className="truncate text-sm font-semibold">{template.name}</div>
                          {template.recommended && (
                            <span className="rounded-full bg-amber-500/10 px-2 py-0.5 text-[10px] font-medium text-amber-600">
                              {t('templates.badges.recommended')}
                            </span>
                          )}
                        </div>
                        <div className="mt-1 break-all font-mono text-[11px] text-muted-foreground">
                          {template.templateId}
                        </div>
                      </div>
                      <span className="rounded-full bg-primary/10 px-2.5 py-1 text-[11px] font-medium text-primary">
                        {template.model}
                      </span>
                    </div>
                    <div className="mt-3 grid gap-2 text-xs text-muted-foreground sm:grid-cols-2">
                      <TemplateMeta label={t('templates.fields.sourceAgent')} value={template.sourceAgentId} />
                      <TemplateMeta label={t('templates.fields.sourceSnapshot')} value={template.sourceSnapshotId} />
                      <TemplateMeta label={t('templates.fields.sourceSandbox')} value={template.sourceSandboxId} />
                      <TemplateMeta label={t('templates.fields.createdAt')} value={template.createdAt || '-'} />
                    </div>
                    <div className="mt-4 flex flex-wrap justify-end gap-2">
                      <Button
                        type="button"
                        size="sm"
                        variant="outline"
                        onClick={() => void navigator.clipboard?.writeText(template.templateId)}
                      >
                        {t('templates.actions.copyId')}
                      </Button>
                      <Button
                        type="button"
                        size="sm"
                        variant="outline"
                        disabled={actingTemplateId === template.templateId}
                        onClick={() => setRenameTemplate(template)}
                      >
                        {t('templates.actions.rename')}
                      </Button>
                      <Button
                        type="button"
                        size="sm"
                        variant="outline"
                        disabled={actingTemplateId === template.templateId}
                        onClick={() => handleToggleRecommended(template)}
                      >
                        {template.recommended ? t('templates.actions.unrecommend') : t('templates.actions.recommend')}
                      </Button>
                      <Button type="button" size="sm" onClick={() => onUseTemplate(template.templateId)}>
                        {t('templates.actions.useTemplate')}
                      </Button>
                      <Button
                        type="button"
                        size="sm"
                        variant="outline"
                        disabled={actingTemplateId === template.templateId}
                        onClick={() => setDeleteTemplate(template)}
                      >
                        {t('templates.actions.delete')}
                      </Button>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
          <TemplateRenameDialog
            template={renameTemplate}
            saving={Boolean(renameTemplate && actingTemplateId === renameTemplate.templateId)}
            onOpenChange={(open) => {
              if (!open) setRenameTemplate(null);
            }}
            onSubmit={(name) => {
              if (renameTemplate) void handleRenameTemplate(renameTemplate, name);
            }}
          />
          <ConfirmDialog
            open={Boolean(deleteTemplate)}
            title={t('templates.dialogs.deleteTitle')}
            description={t('templates.prompts.delete')}
            confirming={Boolean(deleteTemplate && actingTemplateId === deleteTemplate.templateId)}
            confirmLabel={t('templates.actions.delete')}
            onOpenChange={(open) => {
              if (!open) setDeleteTemplate(null);
            }}
            onConfirm={() => {
              if (deleteTemplate) void handleDeleteTemplate(deleteTemplate);
            }}
          />
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function TemplateMeta({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0">
      <span className="text-muted-foreground/70">{label}</span>
      <span className="ml-2 break-all text-foreground/80">{value}</span>
    </div>
  );
}

function PersonalGrid({
  agents,
  selectedId,
  onSelect,
  onCreate,
  onChangeModel,
  onConfigureWecom,
  onCloned,
  onDeleted,
}: {
  agents: Agent[];
  selectedId: string | null;
  onSelect: (id: string) => void;
  onCreate: () => void;
  onChangeModel: (agent: Agent) => void;
  onConfigureWecom: (agent: Agent) => void;
  onCloned: (agent: Agent) => void;
  onDeleted: (id: string) => void;
}) {
  return (
    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
      {agents.map((agent) =>
        agent.id.startsWith('pending:') ? (
          <PendingAgentCard key={agent.id} agent={agent} />
        ) : (
          <AgentCard
            key={agent.id}
            agent={agent}
            selected={agent.id === selectedId}
            onSelect={() => onSelect(agent.id)}
            onChangeModel={onChangeModel}
            onConfigureWecom={onConfigureWecom}
            onCloned={onCloned}
            onDeleted={onDeleted}
          />
        )
      )}
      <CreateAgentCard onClick={onCreate} />
    </div>
  );
}

function PendingAgentCard({ agent }: { agent: Agent }) {
  const { t } = useTranslation('agentHub');
  const isClone = agent.id.startsWith('pending:clone');
  const label = isClone ? t('card.pending.cloning') : t('card.pending.incubating');
  const hint = isClone ? t('card.pending.cloningHint') : t('card.pending.incubatingHint');
  return (
    <div className="panel relative flex min-h-[360px] flex-col items-center justify-center gap-3 p-5 text-center">
      <div className="absolute right-4 top-4 inline-flex items-center gap-1.5 rounded-full bg-amber-500/10 px-2.5 py-1 text-[11px] font-medium text-amber-600">
        <span className="inline-block h-2 w-2 animate-pulse rounded-full bg-amber-400" />
        {label}
      </div>
      <div className="relative">
        <AgentAvatar seed={agent.name || agent.avatar} size={64} />
        <span className="absolute -bottom-1 -right-1 inline-flex h-6 w-6 items-center justify-center rounded-full bg-card ring-1 ring-border/60">
          <Loader2 size={14} className="animate-spin text-amber-500" />
        </span>
      </div>
      <div className="mt-2 text-base font-semibold">{agent.name}</div>
      <div className="max-w-[80%] text-xs text-muted-foreground">{hint}</div>
    </div>
  );
}

function AgentCard({
  agent,
  selected = false,
  onSelect,
  onChangeModel,
  onConfigureWecom,
  onCloned,
  onDeleted,
}: {
  agent: Agent;
  selected?: boolean;
  onSelect?: () => void;
  onChangeModel: (agent: Agent) => void;
  onConfigureWecom: (agent: Agent) => void;
  onCloned: (agent: Agent) => void;
  onDeleted: (id: string) => void;
}) {
  const { t } = useTranslation('agentHub');
  const [restarting, setRestarting] = useState(false);
  const [pausing, setPausing] = useState(false);
  const [resuming, setResuming] = useState(false);
  const [upgrading, setUpgrading] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [stateAction, setStateAction] = useState<string | null>(null);
  const [snapshots, setSnapshots] = useState<AgentSnapshotDto[]>([]);
  const [operations, setOperations] = useState<AgentOperationDto[]>([]);
  const [selectedSnapshotId, setSelectedSnapshotId] = useState('');
  const [snapshotName, setSnapshotName] = useState('');
  const [cloneName, setCloneName] = useState('');
  const [cloneCount, setCloneCount] = useState(1);
  const [templateName, setTemplateName] = useState('');
  const [publishResult, setPublishResult] = useState<string | null>(null);
  const [stateDialogOpen, setStateDialogOpen] = useState(false);
  const [rollbackConfirmOpen, setRollbackConfirmOpen] = useState(false);
  const [snapshotToDelete, setSnapshotToDelete] = useState<AgentSnapshotDto | null>(null);
  const [snapshotToRename, setSnapshotToRename] = useState<AgentSnapshotDto | null>(null);
  const [recoverResult, setRecoverResult] = useState<string | null>(null);
  const [deleteConfirmOpen, setDeleteConfirmOpen] = useState(false);
  const [pauseConfirmOpen, setPauseConfirmOpen] = useState(false);
  const [recoverConfirmOpen, setRecoverConfirmOpen] = useState(false);
  const [cloneConfirmOpen, setCloneConfirmOpen] = useState(false);
  const [publishConfirmOpen, setPublishConfirmOpen] = useState(false);
  const [healthyTarget, setHealthyTarget] = useState<AgentSnapshotDto | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [gatewayReady, setGatewayReady] = useState(false);
  const updateAgent = useAgentStore((s) => s.updateAgent);
  const addAgent = useAgentStore((s) => s.addAgent);
  const removeAgent = useAgentStore((s) => s.removeAgent);
  const gatewayDomain = useAgentStore((s) => s.gatewayDomain);
  const isRunning = agent.status === 'running';
  const bots = agent.bots.filter(isSupportedRobotChannel);
  const botsAvailable = agent.botsAvailable.filter(isSupportedRobotChannel);
  const actionDisabled = restarting || pausing || resuming || upgrading || deleting || Boolean(stateAction);
  const operationTypeLabels: Record<string, string> = {
    snapshot: t('state.operations.types.snapshot'),
    rollback: t('state.operations.types.rollback'),
    clone: t('state.operations.types.clone'),
    publish_template: t('state.operations.types.publish_template'),
    recover: t('state.operations.types.recover'),
  };
  const operationStatusLabels: Record<AgentOperationDto['status'], string> = {
    running: t('state.operations.status.running'),
    succeeded: t('state.operations.status.succeeded'),
    failed: t('state.operations.status.failed'),
  };
  const snapshotNameById = new Map(
    snapshots.map((s) => [s.snapshotID, s.names[0] || s.snapshotID] as const)
  );
  const healthySnapshotCount = snapshots.filter((s) => s.isHealthy).length;

  useEffect(() => {
    if (!isRunning || !agent.sandboxId) {
      setGatewayReady(false);
      return;
    }

    let cancelled = false;
    let timer: number | undefined;
    const check = async () => {
      try {
        const health = await agentHubApi.getGatewayHealth(agent.id);
        if (cancelled) return;
        setGatewayReady(health.ready);
        if (!health.ready) {
          timer = window.setTimeout(check, 3000);
        }
      } catch {
        if (!cancelled) {
          setGatewayReady(false);
          timer = window.setTimeout(check, 3000);
        }
      }
    };

    setGatewayReady(false);
    void check();
    return () => {
      cancelled = true;
      if (timer) window.clearTimeout(timer);
    };
  }, [agent.id, agent.sandboxId, isRunning]);

  useEffect(() => {
    let cancelled = false;
    agentHubApi
      .listSnapshots(agent.id)
      .then((items) => {
        if (cancelled) return;
        setSnapshots(items);
        if (!selectedSnapshotId && items[0]) setSelectedSnapshotId(items[0].snapshotID);
      })
      .catch(() => {
        if (!cancelled) setSnapshots([]);
      });
    return () => {
      cancelled = true;
    };
  }, [agent.id, selectedSnapshotId]);

  const refreshOperations = async () => {
    try {
      const items = await agentHubApi.listOperations(agent.id);
      setOperations(items.slice(0, 3));
    } catch {
      setOperations([]);
    }
  };

  useEffect(() => {
    void refreshOperations();
  }, [agent.id]);

  const handleRestart = async () => {
    if (actionDisabled) return;
    setRestarting(true);
    setActionError(null);
    try {
      await agentHubApi.restart(agent.id);
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setRestarting(false);
    }
  };

  const handleDelete = async () => {
    if (actionDisabled) return;
    setDeleteConfirmOpen(true);
  };

  const handleConfirmDelete = async () => {
    if (actionDisabled) return;
    setDeleting(true);
    setActionError(null);
    try {
      await agentHubApi.delete(agent.id);
      onDeleted(agent.id);
      setDeleteConfirmOpen(false);
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
      setDeleting(false);
    }
  };

  const handlePause = async () => {
    setPausing(true);
    setActionError(null);
    try {
      const updated = await agentHubApi.pause(agent.id);
      updateAgent(updated);
      setPauseConfirmOpen(false);
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setPausing(false);
    }
  };

  const handleResume = async () => {
    if (actionDisabled) return;
    setResuming(true);
    setActionError(null);
    try {
      const updated = await agentHubApi.resume(agent.id);
      updateAgent(updated);
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setResuming(false);
    }
  };

  const handleUpgrade = async () => {
    if (actionDisabled) return;
    setUpgrading(true);
    setActionError(null);
    try {
      await agentHubApi.upgrade(agent.id);
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setUpgrading(false);
    }
  };

  const refreshSnapshots = async () => {
    setStateAction('list');
    setActionError(null);
    try {
      const items = await agentHubApi.listSnapshots(agent.id);
      setSnapshots(items);
      if (!selectedSnapshotId && items[0]) setSelectedSnapshotId(items[0].snapshotID);
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setStateAction(null);
    }
  };

  const handleCreateSnapshot = async () => {
    setStateAction('snapshot');
    setActionError(null);
    const pendingName = snapshotName || t('state.archives.creatingDefault');
    let job;
    try {
      // 后端立即返回操作 ID（快照在后台执行），避免请求长时间阻塞。
      job = await agentHubApi.createSnapshot(agent.id, { name: snapshotName || undefined });
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
      setStateAction(null);
      return;
    }
    setSnapshotName('');
    setStateAction(null);
    void refreshOperations();

    const opId = job.operationId;
    if (!opId) {
      const items = await agentHubApi.listSnapshots(agent.id);
      setSnapshots(items);
      return;
    }

    // 乐观占位：时间线顶部立即出现「存档中…」节点
    const placeholderId = `pending:${opId}`;
    const placeholder: AgentSnapshotDto = {
      snapshotID: placeholderId,
      names: [pendingName],
      status: 'CREATING',
      templateReferenced: false,
      isHealthy: false,
    };
    setSnapshots((prev) => [placeholder, ...prev]);

    // 后台轮询操作流水，完成后用真实节点替换占位（不阻塞对话框）
    void (async () => {
      const deadline = Date.now() + 120_000;
      while (Date.now() < deadline) {
        await new Promise((resolve) => setTimeout(resolve, 1500));
        let ops: AgentOperationDto[] = [];
        try {
          ops = await agentHubApi.listOperations(agent.id);
        } catch {
          continue;
        }
        setOperations(ops.slice(0, 3));
        const op = ops.find((o) => o.operationId === opId);
        if (!op || op.status === 'running') continue;
        if (op.status === 'succeeded') {
          const items = await agentHubApi.listSnapshots(agent.id);
          setSnapshots(items);
          setSelectedSnapshotId(op.targetId || items[0]?.snapshotID || '');
        } else {
          setActionError(op.errorMessage || t('state.errors.snapshotFailed'));
          setSnapshots((prev) => prev.filter((s) => s.snapshotID !== placeholderId));
        }
        return;
      }
      setSnapshots((prev) => prev.filter((s) => s.snapshotID !== placeholderId));
      const items = await agentHubApi.listSnapshots(agent.id);
      setSnapshots(items);
    })();
  };

  const handleRollback = async () => {
    if (!selectedSnapshotId) return;
    setStateAction('rollback');
    setActionError(null);
    try {
      await agentHubApi.rollback(agent.id, { snapshotId: selectedSnapshotId });
      setRollbackConfirmOpen(false);
      void refreshOperations();
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setStateAction(null);
    }
  };

  const handleDeleteSnapshot = async (snapshot: AgentSnapshotDto) => {
    if (snapshot.templateReferenced) return;
    setStateAction('deleteSnapshot');
    setActionError(null);
    try {
      await agentHubApi.deleteSnapshot(agent.id, snapshot.snapshotID);
      const items = await agentHubApi.listSnapshots(agent.id);
      setSnapshots(items);
      if (selectedSnapshotId === snapshot.snapshotID) {
        setSelectedSnapshotId(items[0]?.snapshotID || '');
      }
      setSnapshotToDelete(null);
      void refreshOperations();
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setStateAction(null);
    }
  };

  const handleToggleHealthy = async (snapshot: AgentSnapshotDto) => {
    setStateAction('healthy');
    setActionError(null);
    try {
      await agentHubApi.updateSnapshot(agent.id, snapshot.snapshotID, {
        isHealthy: !snapshot.isHealthy,
      });
      const items = await agentHubApi.listSnapshots(agent.id);
      setSnapshots(items);
      setHealthyTarget(null);
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setStateAction(null);
    }
  };

  const handleRenameSnapshot = async (name: string) => {
    if (!snapshotToRename) return;
    setStateAction('renameSnapshot');
    setActionError(null);
    try {
      await agentHubApi.updateSnapshot(agent.id, snapshotToRename.snapshotID, { name });
      const items = await agentHubApi.listSnapshots(agent.id);
      setSnapshots(items);
      setSnapshotToRename(null);
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setStateAction(null);
    }
  };

  const handleRecover = async () => {
    setStateAction('recover');
    setActionError(null);
    setRecoverResult(null);
    try {
      const res = await agentHubApi.recover(agent.id);
      setRecoverResult(res.method);
      const items = await agentHubApi.listSnapshots(agent.id);
      setSnapshots(items);
      setRecoverConfirmOpen(false);
      void refreshOperations();
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setStateAction(null);
    }
  };

  const handleClone = async () => {
    setStateAction('clone');
    setActionError(null);
    setCloneConfirmOpen(false);
    const count = Math.min(Math.max(Math.trunc(cloneCount) || 1, 1), 10);
    const base = cloneName || t('state.defaults.cloneName', { name: agent.name });
    const snapshotId = selectedSnapshotId || undefined;
    setCloneName('');
    setCloneCount(1);
    // 乐观占位：列表里立即出现 N 张「分身中…」卡片
    const jobs = Array.from({ length: count }, (_, i) => {
      const name = count > 1 ? `${base} ${i + 1}` : base;
      const placeholderId = `pending:clone:${Date.now()}:${i}`;
      const placeholder: Agent = {
        ...agent,
        id: placeholderId,
        name,
        status: 'starting',
        bots: [],
        botsAvailable: [],
        gatewayUrl: undefined,
        envUrl: undefined,
        sandboxId: undefined,
        wecomConfig: undefined,
      };
      addAgent(placeholder);
      return { placeholderId, name };
    });
    const results = await Promise.allSettled(
      jobs.map((job) => agentHubApi.clone(agent.id, { name: job.name, snapshotId }))
    );
    let firstError: string | null = null;
    results.forEach((res, i) => {
      removeAgent(jobs[i].placeholderId);
      if (res.status === 'fulfilled') {
        onCloned(res.value);
      } else if (!firstError) {
        firstError = res.reason instanceof Error ? res.reason.message : String(res.reason);
      }
    });
    if (firstError) setActionError(firstError);
    void refreshOperations();
    setStateAction(null);
  };

  const handlePublishTemplate = async () => {
    setStateAction('publish');
    setActionError(null);
    setPublishResult(null);
    try {
      const result = await agentHubApi.publishTemplate(agent.id, {
        name: templateName || undefined,
        snapshotId: selectedSnapshotId || undefined,
      });
      setPublishResult(result.templateId);
      const items = await agentHubApi.listSnapshots(agent.id);
      setSnapshots(items);
      setTemplateName('');
      setPublishConfirmOpen(false);
      void refreshOperations();
    } catch (e) {
      setActionError(e instanceof Error ? e.message : String(e));
    } finally {
      setStateAction(null);
    }
  };

  return (
    <div
      onClick={onSelect}
      className={cn(
        'panel relative flex cursor-pointer flex-col p-5 transition-all hover:shadow-md',
        selected
          ? 'border-primary/60 shadow-md ring-2 ring-primary/50'
          : 'hover:border-primary/30'
      )}
    >
      {/* Top row: status + restart + menu */}
      <div className="flex items-center justify-between text-xs">
        <div className="flex items-center gap-1.5">
          <span
            className={cn(
              'inline-block h-2 w-2 rounded-full',
              isRunning ? 'bg-emerald-500' : 'bg-muted-foreground/60'
            )}
          />
          <span className="text-muted-foreground">
            {t(`card.status.${agent.status}` as const)}
          </span>
          <button
            type="button"
            disabled={actionDisabled}
            onClick={handleRestart}
            className="ml-1 text-muted-foreground/70 transition-colors hover:text-foreground disabled:cursor-not-allowed disabled:opacity-50"
            title={t('card.actions.restart')}
          >
            <RotateCcw size={12} className={restarting ? 'animate-spin' : undefined} />
          </button>
          <button
            type="button"
            disabled={actionDisabled}
            onClick={handleRestart}
            className="ml-0.5 text-xs text-muted-foreground/70 hover:text-foreground disabled:cursor-not-allowed disabled:opacity-50"
          >
            {restarting ? t('card.actions.restarting') : t('card.actions.restart')}
          </button>
        </div>
        <AgentActionMenu
          disabled={actionDisabled}
          restarting={restarting}
          pausing={pausing}
          resuming={resuming}
          upgrading={upgrading}
          deleting={deleting}
          onRestart={handleRestart}
          onPause={() => setPauseConfirmOpen(true)}
          onResume={handleResume}
          onUpgrade={handleUpgrade}
          onDelete={handleDelete}
        />
      </div>

      {/* Avatar + name */}
      <div className="mt-4 flex flex-col items-center">
        <AgentAvatar seed={agent.name || agent.avatar} size={64} />
        <div className="mt-3 flex items-center gap-1.5">
          <span className="text-base font-semibold">{agent.name}</span>
          <button
            type="button"
            className="text-muted-foreground/60 hover:text-foreground"
            aria-label="edit name"
          >
            <Pencil size={12} />
          </button>
        </div>
        <div className="mt-2 flex flex-wrap items-center justify-center gap-1.5">
          <EngineChip engine={agent.engine} />
          <EnvChip env={agent.env} />
        </div>
      </div>

      {/* Field rows */}
      <div className="mt-5 space-y-2.5 text-xs">
        {/* Runtime model changes are disabled; the model is fixed when the
            instance is provisioned. Clone or recreate to switch models. */}
        <Row label={t('card.fields.model')} value={agent.model} />
        <Row
          label={t('card.fields.version')}
          value={agent.version}
          action={upgrading ? t('card.actions.upgrading') : t('card.fields.upgrade')}
          actionDisabled={actionDisabled}
          onAction={handleUpgrade}
        />
        {agent.sandboxId && (
          <Row
            label={t('card.fields.sandboxId')}
            value={agent.sandboxId}
          />
        )}
        <div className="flex items-center gap-2">
          <span className="w-12 shrink-0 text-muted-foreground">
            {t('card.fields.robot')}
          </span>
          <Info
            size={11}
            className="shrink-0 text-muted-foreground/50"
            aria-label={t('card.fields.robotHint')}
          />
          <div className="ml-1 flex flex-wrap gap-1">
            {bots.map((b) => (
              <BotChip
                key={b}
                channel={b}
                bound
                onClick={() => onConfigureWecom(agent)}
              />
            ))}
            {botsAvailable.map((b) => (
              <BotChip
                key={b}
                channel={b}
                onClick={() => onConfigureWecom(agent)}
              />
            ))}
          </div>
        </div>
      </div>
      <div className="mt-5 rounded-2xl border border-border/60 bg-muted/20 p-3 text-xs">
        <div className="flex items-center justify-between gap-3">
          <div>
            <div className="font-medium text-foreground">{t('state.title')}</div>
            <div className="mt-0.5 text-[11px] text-muted-foreground">
              {t('state.cardSummary', { count: snapshots.length, healthy: healthySnapshotCount })}
            </div>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            {!HIDE_AGENT_RECOVER && (
              <Button
                type="button"
                size="sm"
                variant="outline"
                disabled={actionDisabled || healthySnapshotCount === 0}
                onClick={() => setRecoverConfirmOpen(true)}
                title={t('state.recover.hint')}
                className="h-8"
              >
                {stateAction === 'recover' ? t('state.recover.recovering') : t('state.recover.action')}
              </Button>
            )}
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={actionDisabled}
              onClick={() => setStateDialogOpen(true)}
              className="h-8"
            >
              {t('state.actions.manage')}
            </Button>
          </div>
        </div>
        {!HIDE_AGENT_RECOVER && recoverResult && (
          <div className="mt-2 rounded-lg bg-emerald-500/10 px-2 py-1 text-[11px] text-emerald-600">
            {recoverResult === 'rollback'
              ? t('state.recover.resultRollback')
              : t('state.recover.resultRestart')}
          </div>
        )}
      </div>
      <Dialog.Root open={stateDialogOpen} onOpenChange={setStateDialogOpen}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 z-40 bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
          <Dialog.Content className="fixed left-1/2 top-1/2 z-50 flex max-h-[calc(100vh-3rem)] w-[min(920px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 flex-col rounded-2xl border border-border/60 bg-card shadow-2xl">
            <div className="flex items-start justify-between gap-4 border-b border-border/60 px-6 py-4">
              <div>
                <Dialog.Title className="text-base font-semibold">{t('state.detailTitle', { name: agent.name })}</Dialog.Title>
                <Dialog.Description className="mt-1 text-sm text-muted-foreground">
                  {t('state.description')}
                </Dialog.Description>
              </div>
              <Dialog.Close asChild>
                <button type="button" className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground">
                  <span className="sr-only">{t('templates.actions.close')}</span>
                  <X size={18} />
                </button>
              </Dialog.Close>
            </div>
            <div className="grid gap-5 overflow-y-auto px-6 py-5 lg:grid-cols-[minmax(0,1fr)_320px]">
              <div className="space-y-3">
                <div className="flex items-center justify-between gap-3">
                  <div>
                    <div className="text-sm font-medium">{t('state.archives.title')}</div>
                    <div className="text-xs text-muted-foreground">{t('state.archives.description')}</div>
                  </div>
                  <Button type="button" size="sm" variant="outline" disabled={actionDisabled} onClick={refreshSnapshots}>
                    {stateAction === 'list' ? t('state.actions.loading') : t('state.actions.refresh')}
                  </Button>
                </div>
                <div className="flex gap-2">
                  <Input
                    value={snapshotName}
                    disabled={actionDisabled}
                    onChange={(e) => setSnapshotName(e.target.value)}
                    placeholder={t('state.placeholders.snapshotName')}
                    className="h-9 text-xs"
                  />
                  <Button type="button" size="sm" disabled={actionDisabled || !isRunning} onClick={handleCreateSnapshot} className="h-9 shrink-0">
                    {stateAction === 'snapshot' ? t('state.actions.snapshotting') : t('state.actions.createSnapshot')}
                  </Button>
                </div>
                {snapshots.length === 0 ? (
                  <div className="rounded-xl border border-dashed border-border/70 bg-muted/20 p-5 text-center text-xs text-muted-foreground">
                    {t('state.archives.empty')}
                  </div>
                ) : (
                  <div className="relative space-y-2">
                    {snapshots.length > 1 && (
                      <div className="pointer-events-none absolute bottom-4 left-2 top-4 w-px bg-border/70" aria-hidden />
                    )}
                    {snapshots.map((snapshot) => {
                      const parentName = snapshot.parentSnapshotID
                        ? snapshotNameById.get(snapshot.parentSnapshotID)
                        : undefined;
                      const isSelected = selectedSnapshotId === snapshot.snapshotID;
                      const isPending = snapshot.status === 'CREATING';
                      return (
                        <div key={snapshot.snapshotID} className="relative pl-7">
                          <span
                            className={cn(
                              'absolute left-2 top-[18px] z-10 h-3 w-3 -translate-x-1/2 rounded-full border-2 border-card',
                              isPending
                                ? 'animate-pulse bg-amber-400'
                                : snapshot.isHealthy
                                  ? 'bg-emerald-500'
                                  : 'bg-muted-foreground/40'
                            )}
                            aria-hidden
                          />
                          <div
                            onClick={() => setSelectedSnapshotId(snapshot.snapshotID)}
                            className={cn(
                              'w-full cursor-pointer rounded-xl border p-3 text-left transition-colors',
                              actionDisabled && 'pointer-events-none opacity-60',
                              isSelected
                                ? 'border-primary/50 bg-primary/5'
                                : 'border-border/60 bg-background hover:border-primary/30'
                            )}
                          >
                            <div className="flex flex-wrap items-start justify-between gap-2">
                              <div className="min-w-0">
                                <div className="truncate text-sm font-medium">{snapshot.names[0] || snapshot.snapshotID}</div>
                                {!isPending && (
                                  <div className="mt-1 break-all font-mono text-[11px] text-muted-foreground">{snapshot.snapshotID}</div>
                                )}
                              </div>
                              <div className="flex flex-wrap items-center justify-end gap-1">
                                {snapshot.isHealthy && (
                                  <span className="rounded-full bg-emerald-500/10 px-2 py-0.5 text-[11px] font-medium text-emerald-600">
                                    {t('state.archives.healthyBadge')}
                                  </span>
                                )}
                                {snapshot.publishedTemplateId && (
                                  <span className="rounded-full bg-indigo-500/10 px-2 py-0.5 text-[11px] text-indigo-600">
                                    {t('state.archives.publishedBadge')}
                                  </span>
                                )}
                                {snapshot.templateReferenced && (
                                  <span className="rounded-full bg-amber-500/10 px-2 py-0.5 text-[11px] text-amber-600">
                                    {t('state.archives.referencedBadge')}
                                  </span>
                                )}
                                {isPending ? (
                                  <span className="rounded-full bg-amber-500/10 px-2 py-0.5 text-[11px] font-medium text-amber-600">
                                    {t('state.archives.creatingBadge')}
                                  </span>
                                ) : (
                                  <span className="rounded-full bg-muted px-2 py-0.5 text-[11px] text-muted-foreground">
                                    {snapshot.status || '-'}
                                  </span>
                                )}
                              </div>
                            </div>
                            {parentName && (
                              <div className="mt-2 inline-flex items-center gap-1 rounded-md bg-muted/40 px-2 py-0.5 text-[11px] text-muted-foreground">
                                <GitBranch size={11} />
                                {t('state.archives.basedOn', { name: parentName })}
                              </div>
                            )}
                            <div className="mt-2 grid gap-1 text-[11px] text-muted-foreground sm:grid-cols-2">
                              <span>{t('state.archives.createdAt')}: {snapshot.createdAt || '-'}</span>
                              <span>{t('state.archives.updatedAt')}: {snapshot.updatedAt || '-'}</span>
                              <span className="break-all sm:col-span-2">{t('state.archives.originSandbox')}: {snapshot.originSandboxID || '-'}</span>
                            </div>
                            {!isPending && (
                              <div className="mt-3 flex flex-wrap justify-end gap-2">
                                <Button
                                  type="button"
                                  size="sm"
                                  variant="outline"
                                  disabled={actionDisabled}
                                  onClick={(event) => {
                                    event.stopPropagation();
                                    setSnapshotToRename(snapshot);
                                  }}
                                  className="h-7"
                                >
                                  {t('state.actions.renameSnapshot')}
                                </Button>
                                <Button
                                  type="button"
                                  size="sm"
                                  variant="outline"
                                  disabled={actionDisabled || stateAction === 'healthy'}
                                  onClick={(event) => {
                                    event.stopPropagation();
                                    setHealthyTarget(snapshot);
                                  }}
                                  className="h-7"
                                >
                                  {snapshot.isHealthy
                                    ? t('state.actions.unmarkHealthy')
                                    : t('state.actions.markHealthy')}
                                </Button>
                                <Button
                                  type="button"
                                  size="sm"
                                  variant="outline"
                                  disabled={actionDisabled || snapshot.templateReferenced || stateAction === 'deleteSnapshot'}
                                  onClick={(event) => {
                                    event.stopPropagation();
                                    setSnapshotToDelete(snapshot);
                                  }}
                                  className="h-7"
                                >
                                  {stateAction === 'deleteSnapshot' ? t('state.actions.deleting') : t('state.actions.deleteSnapshot')}
                                </Button>
                              </div>
                            )}
                          </div>
                        </div>
                      );
                    })}
                  </div>
                )}
              </div>
              <div className="space-y-3">
                <div className="rounded-xl border border-border/60 bg-background p-3">
                  <div className="text-sm font-medium">{t('state.actionsPanel.title')}</div>
                  <div className="mt-3 space-y-2">
                    <Button type="button" size="sm" variant="outline" disabled={actionDisabled || !selectedSnapshotId} onClick={() => setRollbackConfirmOpen(true)} className="h-8 w-full">
                      {stateAction === 'rollback' ? t('state.actions.rollbacking') : t('state.actions.rollback')}
                    </Button>
                    <div className="flex gap-2">
                      <Input value={cloneName} disabled={actionDisabled} onChange={(e) => setCloneName(e.target.value)} placeholder={t('state.placeholders.cloneName')} className="h-8 flex-1 text-xs" />
                      <Input
                        type="number"
                        min={1}
                        max={10}
                        value={cloneCount}
                        disabled={actionDisabled}
                        onChange={(e) => setCloneCount(Math.min(Math.max(Math.trunc(Number(e.target.value)) || 1, 1), 10))}
                        title={t('state.placeholders.cloneCount')}
                        aria-label={t('state.placeholders.cloneCount')}
                        className="h-8 w-16 text-center text-xs"
                      />
                    </div>
                    <Button type="button" size="sm" variant="outline" disabled={actionDisabled} onClick={() => setCloneConfirmOpen(true)} className="h-8 w-full">
                      {stateAction === 'clone'
                        ? t('state.actions.cloning')
                        : cloneCount > 1
                          ? t('state.actions.cloneN', { count: cloneCount })
                          : t('state.actions.clone')}
                    </Button>
                    <Input value={templateName} disabled={actionDisabled} onChange={(e) => setTemplateName(e.target.value)} placeholder={t('state.placeholders.templateName')} className="h-8 text-xs" />
                    <Button type="button" size="sm" disabled={actionDisabled} onClick={() => setPublishConfirmOpen(true)} className="h-8 w-full">
                      {stateAction === 'publish' ? t('state.actions.publishing') : t('state.actions.publishAssistantTemplate')}
                    </Button>
                    {publishResult && (
                      <div className="rounded-lg bg-emerald-500/10 px-2 py-1 text-[11px] text-emerald-600">
                        {t('state.publishResult', { templateId: publishResult })}
                      </div>
                    )}
                    {!HIDE_AGENT_RECOVER && (
                      <div className="border-t border-border/60 pt-2">
                        <Button
                          type="button"
                          size="sm"
                          variant="outline"
                          disabled={actionDisabled || healthySnapshotCount === 0}
                          onClick={() => setRecoverConfirmOpen(true)}
                          className="h-8 w-full"
                        >
                          <HeartPulse size={14} className="mr-1" />
                          {stateAction === 'recover' ? t('state.recover.recovering') : t('state.recover.action')}
                        </Button>
                        <div className="mt-1 text-[11px] text-muted-foreground">
                          {t('state.recover.panelHint', { count: healthySnapshotCount })}
                        </div>
                        {recoverResult && (
                          <div className="mt-1 rounded-lg bg-emerald-500/10 px-2 py-1 text-[11px] text-emerald-600">
                            {recoverResult === 'rollback'
                              ? t('state.recover.resultRollback')
                              : t('state.recover.resultRestart')}
                          </div>
                        )}
                      </div>
                    )}
                  </div>
                </div>
                <div className="rounded-xl border border-border/60 bg-background p-3">
                  <div className="text-sm font-medium">{t('state.operations.title')}</div>
                  <div className="mt-2 space-y-1.5">
                    {operations.length === 0 ? (
                      <div className="text-xs text-muted-foreground">{t('state.operations.empty')}</div>
                    ) : (
                      operations.map((operation) => (
                        <div key={operation.operationId} className="rounded-lg bg-muted/30 px-2 py-1.5 text-[11px]">
                          <div className="flex items-center justify-between gap-2">
                            <span className="truncate text-muted-foreground">
                              {operationTypeLabels[operation.operationType] || operation.operationType}
                            </span>
                            <span
                              className={cn(
                                'shrink-0 rounded-full px-1.5 py-0.5',
                                operation.status === 'succeeded' && 'bg-emerald-500/10 text-emerald-600',
                                operation.status === 'failed' && 'bg-destructive/10 text-destructive',
                                operation.status === 'running' && 'bg-primary/10 text-primary'
                              )}
                              title={operation.errorMessage || operation.targetId || undefined}
                            >
                              {operationStatusLabels[operation.status]}
                            </span>
                          </div>
                          <div className="mt-1 text-muted-foreground/70">{operation.updatedAt || operation.createdAt || '-'}</div>
                        </div>
                      ))
                    )}
                  </div>
                </div>
              </div>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>
      <ConfirmDialog
        open={rollbackConfirmOpen}
        title={t('state.dialogs.rollbackTitle')}
        description={t('state.confirmRollback')}
        confirming={stateAction === 'rollback'}
        confirmLabel={t('state.actions.rollback')}
        onOpenChange={(open) => {
          if (!open) setRollbackConfirmOpen(false);
        }}
        onConfirm={() => void handleRollback()}
      />
      <ConfirmDialog
        open={Boolean(snapshotToDelete)}
        title={t('state.dialogs.deleteSnapshotTitle')}
        description={t('state.archives.confirmDelete')}
        confirming={stateAction === 'deleteSnapshot'}
        confirmLabel={t('state.actions.deleteSnapshot')}
        onOpenChange={(open) => {
          if (!open) setSnapshotToDelete(null);
        }}
        onConfirm={() => {
          if (snapshotToDelete) void handleDeleteSnapshot(snapshotToDelete);
        }}
      />
      <SnapshotRenameDialog
        snapshot={snapshotToRename}
        saving={stateAction === 'renameSnapshot'}
        onOpenChange={(open) => {
          if (!open) setSnapshotToRename(null);
        }}
        onSubmit={(name) => void handleRenameSnapshot(name)}
      />
      <ActionErrorDialog
        message={actionError}
        onOpenChange={(open) => {
          if (!open) setActionError(null);
        }}
      />
      <DeleteConfirmDialog
        open={deleteConfirmOpen}
        deleting={deleting}
        onOpenChange={setDeleteConfirmOpen}
        onConfirm={handleConfirmDelete}
      />
      <ConfirmDialog
        open={pauseConfirmOpen}
        title={t('card.dialogs.pauseTitle')}
        description={t('card.prompts.pause')}
        confirming={pausing}
        confirmLabel={t('card.actions.pause')}
        onOpenChange={(open) => {
          if (!open) setPauseConfirmOpen(false);
        }}
        onConfirm={() => void handlePause()}
      />
      {!HIDE_AGENT_RECOVER && (
        <ConfirmDialog
          open={recoverConfirmOpen}
          title={t('state.dialogs.recoverTitle')}
          description={t('state.prompts.recover')}
          confirming={stateAction === 'recover'}
          confirmLabel={t('state.recover.action')}
          onOpenChange={(open) => {
            if (!open) setRecoverConfirmOpen(false);
          }}
          onConfirm={() => void handleRecover()}
        />
      )}
      <ConfirmDialog
        open={cloneConfirmOpen}
        title={t('state.dialogs.cloneTitle')}
        description={t('state.prompts.clone', { count: Math.min(Math.max(Math.trunc(cloneCount) || 1, 1), 10) })}
        confirming={stateAction === 'clone'}
        confirmLabel={cloneCount > 1 ? t('state.actions.cloneN', { count: cloneCount }) : t('state.actions.clone')}
        onOpenChange={(open) => {
          if (!open) setCloneConfirmOpen(false);
        }}
        onConfirm={() => void handleClone()}
      />
      <ConfirmDialog
        open={publishConfirmOpen}
        title={t('state.dialogs.publishTitle')}
        description={t('state.prompts.publish')}
        confirming={stateAction === 'publish'}
        confirmLabel={t('state.actions.publishAssistantTemplate')}
        onOpenChange={(open) => {
          if (!open) setPublishConfirmOpen(false);
        }}
        onConfirm={() => void handlePublishTemplate()}
      />
      <ConfirmDialog
        open={Boolean(healthyTarget)}
        title={healthyTarget?.isHealthy ? t('state.dialogs.unmarkHealthyTitle') : t('state.dialogs.markHealthyTitle')}
        description={healthyTarget?.isHealthy ? t('state.prompts.unmarkHealthy') : t('state.prompts.markHealthy')}
        confirming={stateAction === 'healthy'}
        confirmLabel={healthyTarget?.isHealthy ? t('state.actions.unmarkHealthy') : t('state.actions.markHealthy')}
        onOpenChange={(open) => {
          if (!open) setHealthyTarget(null);
        }}
        onConfirm={() => {
          if (healthyTarget) void handleToggleHealthy(healthyTarget);
        }}
      />

      {/* Actions */}
      <div className="mt-5 grid grid-cols-2 gap-2">
        <Button
          size="sm"
          className="gap-1.5"
          disabled={!isRunning || !agent.sandboxId || !gatewayReady}
          onClick={() => {
            const url = buildCubeProxyUrl(agent, OPENCLAW_GATEWAY_PORT, agent.gatewayUrl, {
              gateway: true,
              gatewayDomain,
            });
            if (url) {
              // Subdomain origins isolate localStorage on their own, so the
              // same-origin clearing workaround is only needed for the proxy path.
              if (!gatewayDomain.trim()) clearOpenclawClientState();
              window.open(url, '_blank', 'noopener,noreferrer');
            }
          }}
          title={!isRunning ? t('card.status.stopped') : undefined}
        >
          <ExternalLink size={13} />
          {t('card.actions.gatewayManage')}
        </Button>
        <Button
          size="sm"
          variant="outline"
          className="gap-1.5"
          disabled={!isRunning || !agent.sandboxId}
          onClick={() => {
            const url = buildCubeProxyUrl(agent, LOGIN_ENV_PORT, agent.envUrl);
            if (url) window.open(url, '_blank', 'noopener,noreferrer');
          }}
          title={!isRunning ? t('card.status.stopped') : undefined}
        >
          <Terminal size={13} />
          {t('card.actions.loginEnv')}
        </Button>
      </div>
    </div>
  );
}

function TemplateRenameDialog({
  template,
  saving,
  onOpenChange,
  onSubmit,
}: {
  template: AgentTemplateDto | null;
  saving: boolean;
  onOpenChange: (open: boolean) => void;
  onSubmit: (name: string) => void;
}) {
  const { t } = useTranslation('agentHub');
  const [name, setName] = useState('');

  useEffect(() => {
    setName(template?.name ?? '');
  }, [template]);

  const trimmed = name.trim();

  return (
    <Dialog.Root open={Boolean(template)} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-[60] bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-[70] w-[min(480px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-2xl border border-border/60 bg-card p-6 shadow-2xl">
          <Dialog.Title className="text-base font-semibold">
            {t('templates.dialogs.renameTitle')}
          </Dialog.Title>
          <Dialog.Description className="mt-2 text-sm text-muted-foreground">
            {t('templates.prompts.rename')}
          </Dialog.Description>
          <Input
            value={name}
            disabled={saving}
            onChange={(event) => setName(event.target.value)}
            className="mt-4"
            autoFocus
          />
          <div className="mt-6 flex justify-end gap-2">
            <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
              {t('deleteDialog.actions.cancel')}
            </Button>
            <Button type="button" disabled={saving || !trimmed} onClick={() => onSubmit(trimmed)}>
              {saving ? t('templates.actions.saving') : t('templates.actions.rename')}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function SnapshotRenameDialog({
  snapshot,
  saving,
  onOpenChange,
  onSubmit,
}: {
  snapshot: AgentSnapshotDto | null;
  saving: boolean;
  onOpenChange: (open: boolean) => void;
  onSubmit: (name: string) => void;
}) {
  const { t } = useTranslation('agentHub');
  const [name, setName] = useState('');

  useEffect(() => {
    setName(snapshot?.names[0] ?? '');
  }, [snapshot]);

  const trimmed = name.trim();

  return (
    <Dialog.Root open={Boolean(snapshot)} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-[60] bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-[70] w-[min(480px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-2xl border border-border/60 bg-card p-6 shadow-2xl">
          <Dialog.Title className="text-base font-semibold">
            {t('state.dialogs.renameSnapshotTitle')}
          </Dialog.Title>
          <Dialog.Description className="mt-2 text-sm text-muted-foreground">
            {t('state.dialogs.renameSnapshotDescription')}
          </Dialog.Description>
          <Input
            value={name}
            disabled={saving}
            onChange={(event) => setName(event.target.value)}
            className="mt-4"
            autoFocus
          />
          <div className="mt-6 flex justify-end gap-2">
            <Button type="button" variant="outline" disabled={saving} onClick={() => onOpenChange(false)}>
              {t('deleteDialog.actions.cancel')}
            </Button>
            <Button type="button" disabled={saving || !trimmed} onClick={() => onSubmit(trimmed)}>
              {saving ? t('templates.actions.saving') : t('templates.actions.rename')}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function ConfirmDialog({
  open,
  title,
  description,
  confirming,
  confirmLabel,
  onOpenChange,
  onConfirm,
}: {
  open: boolean;
  title: string;
  description: string;
  confirming: boolean;
  confirmLabel: string;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => void;
}) {
  const { t } = useTranslation('agentHub');

  const handleOpenChange = (nextOpen: boolean) => {
    if (!confirming) onOpenChange(nextOpen);
  };

  return (
    <Dialog.Root open={open} onOpenChange={handleOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-[60] bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-[70] w-[min(460px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-2xl border border-border/60 bg-card p-6 shadow-2xl">
          <Dialog.Title className="text-base font-semibold">{title}</Dialog.Title>
          <Dialog.Description className="mt-2 text-sm text-muted-foreground">
            {description}
          </Dialog.Description>
          <div className="mt-6 flex justify-end gap-2">
            <Button type="button" variant="outline" disabled={confirming} onClick={() => handleOpenChange(false)}>
              {t('deleteDialog.actions.cancel')}
            </Button>
            <Button type="button" disabled={confirming} onClick={onConfirm}>
              {confirming ? t('state.actions.processing') : confirmLabel}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function DeleteConfirmDialog({
  open,
  deleting,
  onOpenChange,
  onConfirm,
}: {
  open: boolean;
  deleting: boolean;
  onOpenChange: (open: boolean) => void;
  onConfirm: () => void;
}) {
  const { t } = useTranslation('agentHub');

  const handleOpenChange = (nextOpen: boolean) => {
    if (!deleting) onOpenChange(nextOpen);
  };

  return (
    <Dialog.Root open={open} onOpenChange={handleOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-[min(440px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-2xl border border-border/60 bg-card p-6 shadow-2xl">
          <Dialog.Title className="text-base font-semibold">
            {t('deleteDialog.title')}
          </Dialog.Title>
          <Dialog.Description className="mt-2 text-sm text-muted-foreground">
            {t('card.actions.deleteConfirm')}
          </Dialog.Description>
          <div className="mt-6 flex justify-end gap-2">
            <Button
              type="button"
              variant="outline"
              disabled={deleting}
              onClick={() => handleOpenChange(false)}
            >
              {t('deleteDialog.actions.cancel')}
            </Button>
            <Button type="button" disabled={deleting} onClick={onConfirm}>
              {deleting ? t('card.actions.deleting') : t('deleteDialog.actions.confirm')}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function ActionErrorDialog({
  message,
  onOpenChange,
}: {
  message: string | null;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useTranslation('agentHub');

  return (
    <Dialog.Root open={Boolean(message)} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-[min(560px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-2xl border border-border/60 bg-card p-6 shadow-2xl">
          <Dialog.Title className="text-base font-semibold">
            {t('errorDialog.title')}
          </Dialog.Title>
          <Dialog.Description className="mt-1 text-sm text-muted-foreground">
            {t('errorDialog.description')}
          </Dialog.Description>
          <div className="mt-4 h-40 overflow-y-auto rounded-xl border border-rose-500/20 bg-rose-500/10 p-3 text-xs leading-5 text-rose-500">
            <pre className="whitespace-pre-wrap break-words font-sans">{message}</pre>
          </div>
          <div className="mt-5 flex justify-end">
            <Button type="button" onClick={() => onOpenChange(false)}>
              {t('errorDialog.actions.close')}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function AgentActionMenu({
  disabled,
  restarting,
  pausing,
  resuming,
  upgrading,
  deleting,
  onRestart,
  onPause,
  onResume,
  onUpgrade,
  onDelete,
}: {
  disabled: boolean;
  restarting: boolean;
  pausing: boolean;
  resuming: boolean;
  upgrading: boolean;
  deleting: boolean;
  onRestart: () => void;
  onPause: () => void;
  onResume: () => void;
  onUpgrade: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation('agentHub');

  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <button
          type="button"
          disabled={disabled}
          className="rounded-xl bg-primary/5 p-2 text-primary transition-colors hover:bg-primary/10 disabled:cursor-not-allowed disabled:opacity-50"
          aria-label={t('card.actions.more')}
        >
          <MoreHorizontal size={16} />
        </button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content
          align="end"
          sideOffset={8}
          className="z-50 min-w-44 rounded-2xl border border-border/70 bg-popover p-2 text-sm text-popover-foreground shadow-xl"
        >
          <DropdownMenu.Item
            disabled={disabled}
            onSelect={onRestart}
            className="flex cursor-pointer select-none items-center gap-3 rounded-lg px-3 py-2.5 text-muted-foreground outline-none transition-colors hover:bg-muted hover:text-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50"
          >
            <RotateCcw size={16} className={restarting ? 'animate-spin' : undefined} />
            <span>{restarting ? t('card.actions.restarting') : t('card.actions.restartEnv')}</span>
          </DropdownMenu.Item>
          <DropdownMenu.Item
            disabled={disabled}
            onSelect={onPause}
            className="flex cursor-pointer select-none items-center gap-3 rounded-lg px-3 py-2.5 text-muted-foreground outline-none transition-colors hover:bg-muted hover:text-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50"
          >
            <Pause size={16} />
            <span>{pausing ? t('card.actions.pausing') : t('card.actions.pause')}</span>
          </DropdownMenu.Item>
          <DropdownMenu.Item
            disabled={disabled}
            onSelect={onResume}
            className="flex cursor-pointer select-none items-center gap-3 rounded-lg px-3 py-2.5 text-muted-foreground outline-none transition-colors hover:bg-muted hover:text-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50"
          >
            <Play size={16} />
            <span>{resuming ? t('card.actions.resuming') : t('card.actions.resume')}</span>
          </DropdownMenu.Item>
          <DropdownMenu.Item
            disabled={disabled}
            onSelect={onUpgrade}
            className="flex cursor-pointer select-none items-center gap-3 rounded-lg px-3 py-2.5 text-muted-foreground outline-none transition-colors hover:bg-muted hover:text-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50"
          >
            <Download size={16} className={upgrading ? 'animate-pulse' : undefined} />
            <span>{upgrading ? t('card.actions.upgrading') : t('card.fields.upgrade')}</span>
          </DropdownMenu.Item>
          <DropdownMenu.Separator className="my-1 h-px bg-border/70" />
          <DropdownMenu.Item
            disabled={disabled}
            onSelect={onDelete}
            className="flex cursor-pointer select-none items-center gap-3 rounded-lg px-3 py-2.5 text-rose-500 outline-none transition-colors hover:bg-rose-500/10 data-[disabled]:pointer-events-none data-[disabled]:opacity-50"
          >
            <Trash2 size={16} />
            <span>{deleting ? t('card.actions.deleting') : t('card.actions.delete')}</span>
          </DropdownMenu.Item>
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  );
}

function isSupportedRobotChannel(channel: string): channel is RobotChannel {
  return channel in ROBOT_CHANNELS;
}

function Row({
  label,
  value,
  action,
  actionDisabled = false,
  onAction,
}: {
  label: string;
  value: string;
  action?: string;
  actionDisabled?: boolean;
  onAction?: () => void;
}) {
  return (
    <div className="flex items-center gap-2">
      <span className="w-12 shrink-0 text-muted-foreground">{label}</span>
      <span className="truncate text-foreground/90">{value}</span>
      {action && (
        <button
          type="button"
          disabled={actionDisabled}
          onClick={onAction}
          className="ml-auto shrink-0 text-primary hover:underline disabled:cursor-not-allowed disabled:opacity-50"
        >
          {action}
        </button>
      )}
    </div>
  );
}

function EngineChip({ engine }: { engine: Agent['engine'] }) {
  const label = engine === 'openclaw' ? 'OpenClaw' : 'Hermes';
  return (
    <span className="inline-flex items-center gap-1 rounded-full bg-rose-500/10 px-2 py-0.5 text-[10px] font-medium text-rose-600 ring-1 ring-rose-500/20 dark:text-rose-300">
      <span className="inline-block h-1.5 w-1.5 rounded-full bg-rose-500" />
      {label}
    </span>
  );
}

function EnvChip({ env }: { env: Agent['env'] }) {
  const label = env === 'linux' ? 'Linux' : '个人 Mac 设备';
  return (
    <span className="inline-flex items-center gap-1 rounded-full bg-muted px-2 py-0.5 text-[10px] font-medium text-muted-foreground ring-1 ring-border/60">
      {label}
    </span>
  );
}

function BotChip({
  channel,
  bound = false,
  onClick,
}: {
  channel: RobotChannel;
  bound?: boolean;
  onClick?: () => void;
}) {
  const label = ROBOT_CHANNELS[channel].label;
  if (bound) {
    return (
      <button
        type="button"
        onClick={onClick}
        className="inline-flex items-center rounded-full bg-emerald-500/10 px-2 py-0.5 text-[10px] font-medium text-emerald-600 ring-1 ring-emerald-500/30 transition-colors hover:bg-emerald-500/15 dark:text-emerald-300"
      >
        {label}
      </button>
    );
  }

  return (
    <button
      type="button"
      onClick={onClick}
      className="inline-flex items-center gap-0.5 rounded-full bg-transparent px-2 py-0.5 text-[10px] font-medium text-muted-foreground ring-1 ring-dashed ring-border hover:text-foreground hover:ring-border/80"
    >
      <Plus size={9} />
      {label}
    </button>
  );
}

function ModelDialog({
  agent,
  onOpenChange,
  onSaved,
}: {
  agent: Agent | null;
  onOpenChange: (open: boolean) => void;
  onSaved: (agent: Agent) => void;
}) {
  const { t } = useTranslation('agentHub');
  const [model, setModel] = useState<(typeof MODEL_OPTIONS)[number]['value']>('DeepSeek V4 Flash');
  const [modelPickerOpen, setModelPickerOpen] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const selectedModel = MODEL_OPTIONS.find((option) => option.value === model) ?? MODEL_OPTIONS[0];

  useEffect(() => {
    if (!agent) return;
    const current = MODEL_OPTIONS.some((option) => option.value === agent.model)
      ? (agent.model as (typeof MODEL_OPTIONS)[number]['value'])
      : 'DeepSeek V4 Flash';
    setModel(current);
    setModelPickerOpen(false);
    setSubmitting(false);
    setError(null);
  }, [agent]);

  const handleOpenChange = (open: boolean) => {
    if (!open && !submitting) {
      setModelPickerOpen(false);
      onOpenChange(false);
    }
  };

  const handleSave = async () => {
    if (!agent) return;
    setSubmitting(true);
    setError(null);
    try {
      const updated = await agentHubApi.updateModel(agent.id, { model });
      onSaved(updated);
      onOpenChange(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setSubmitting(false);
    }
  };

  return (
    <Dialog.Root open={Boolean(agent)} onOpenChange={handleOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-[min(440px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-xl border border-border/60 bg-card p-6 shadow-2xl">
          <div className="flex items-start justify-between gap-4">
            <Dialog.Title className="text-xl font-semibold tracking-tight">
              {t('modelDialog.title')}
            </Dialog.Title>
            <Dialog.Close asChild>
              <button
                type="button"
                disabled={submitting}
                className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground disabled:cursor-not-allowed disabled:opacity-50"
              >
                <span className="sr-only">{t('wecomDialog.actions.cancel')}</span>
                <X size={18} />
              </button>
            </Dialog.Close>
          </div>

          <div className="mt-4 space-y-2">
            <div className="flex items-center justify-between gap-4">
              <label className="text-sm font-semibold text-muted-foreground">
                <span className="mr-1 text-rose-500">*</span>
                {t('modelDialog.label')}
              </label>
              <button
                type="button"
                className="inline-flex items-center gap-1 text-sm text-muted-foreground underline underline-offset-4 hover:text-foreground"
              >
                <HelpCircle size={14} />
                {t('modelDialog.help')}
              </button>
            </div>
            <div className="relative">
              <button
                type="button"
                disabled={submitting}
                onClick={() => setModelPickerOpen((open) => !open)}
                className="flex h-10 w-full items-center justify-between rounded-xl border border-primary/40 bg-background px-4 text-left text-sm font-medium shadow-sm outline-none ring-2 ring-primary/10 transition-colors hover:border-primary/60 focus:border-primary/60 disabled:cursor-not-allowed disabled:opacity-60"
              >
                <span>{t(selectedModel.labelKey)}</span>
                <ChevronDown
                  size={16}
                  className={cn(
                    'shrink-0 text-muted-foreground transition-transform',
                    modelPickerOpen && 'rotate-180'
                  )}
                />
              </button>
              {modelPickerOpen && (
                <div className="absolute left-0 right-0 top-[calc(100%+0.5rem)] z-50 max-h-40 overflow-y-auto rounded-xl border border-border/80 bg-card py-2 shadow-2xl">
                  {MODEL_OPTIONS.map((option) => {
                    const selected = option.value === model;
                    return (
                      <button
                        key={option.value}
                        type="button"
                        disabled={submitting}
                        onClick={() => {
                          setModel(option.value);
                          setModelPickerOpen(false);
                        }}
                        className={cn(
                          'flex w-full items-center justify-between px-4 py-2 text-left text-sm text-muted-foreground transition-colors hover:bg-muted/60 hover:text-foreground disabled:cursor-not-allowed disabled:opacity-60',
                          selected && 'bg-primary/10 text-foreground'
                        )}
                      >
                        <span>{t(option.labelKey)}</span>
                        {selected && <Check size={14} className="text-primary" />}
                      </button>
                    );
                  })}
                </div>
              )}
            </div>
            <p className="text-xs text-rose-500">{t('modelDialog.warning')}</p>
            {error && <p className="text-xs text-rose-500">{error}</p>}
          </div>

          <div className="mt-4 flex justify-end gap-2">
            <Button
              type="button"
              variant="outline"
              disabled={submitting}
              onClick={() => handleOpenChange(false)}
            >
              {t('modelDialog.actions.cancel')}
            </Button>
            <Button type="button" disabled={submitting} onClick={handleSave}>
              {submitting ? t('modelDialog.actions.saving') : t('modelDialog.actions.save')}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function WeComConfigDialog({
  agent,
  onOpenChange,
  onSaved,
}: {
  agent: Agent | null;
  onOpenChange: (open: boolean) => void;
  onSaved: (agent: Agent) => void;
}) {
  const { t } = useTranslation('agentHub');
  const [botId, setBotId] = useState('');
  const [botSecret, setBotSecret] = useState('');
  const [showSecret, setShowSecret] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!agent) return;
    let cancelled = false;
    setBotId(agent.wecomConfig?.botId ?? '');
    setBotSecret(agent.wecomConfig?.botSecret ?? '');
    setShowSecret(false);
    setError(null);
    setSubmitting(false);

    agentHubApi
      .getWecomConfig(agent.id)
      .then((config) => {
        if (cancelled || !config) return;
        setBotId(config.botId);
        setBotSecret(config.botSecret);
      })
      .catch(() => {
        // Existing unbound agents simply keep empty fields for first-time setup.
      });

    return () => {
      cancelled = true;
    };
  }, [agent]);

  const handleOpenChange = (open: boolean) => {
    if (!open && !submitting) onOpenChange(false);
  };

  const handleSave = async () => {
    if (!agent) return;
    const nextBotId = botId.trim();
    const nextBotSecret = botSecret.trim();
    if (!nextBotId || !nextBotSecret) {
      setError(t('wecomDialog.errors.required'));
      return;
    }

    setSubmitting(true);
    setError(null);
    try {
      const updated = await agentHubApi.updateWecomConfig(agent.id, {
        botId: nextBotId,
        botSecret: nextBotSecret,
      });
      onSaved(updated);
      onOpenChange(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setSubmitting(false);
    }
  };

  return (
    <Dialog.Root open={Boolean(agent)} onOpenChange={handleOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-background/70 backdrop-blur-sm data-[state=open]:animate-fade-in" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-[min(520px,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 rounded-2xl border border-border/60 bg-card p-6 shadow-2xl">
          <Dialog.Title className="text-base font-semibold">
            {t('wecomDialog.title')}
          </Dialog.Title>
          <Dialog.Description className="mt-1 text-sm text-muted-foreground">
            {t('wecomDialog.description')}
          </Dialog.Description>

          <div className="mt-5 space-y-4">
            <label className="block space-y-1.5 text-sm">
              <span className="font-medium">{t('wecomDialog.fields.botId')}</span>
              <Input
                value={botId}
                onChange={(e) => setBotId(e.target.value)}
                placeholder={t('wecomDialog.placeholders.botId')}
              />
            </label>
            <label className="block space-y-1.5 text-sm">
              <span className="font-medium">{t('wecomDialog.fields.botSecret')}</span>
              <div className="flex gap-2">
                <Input
                  type={showSecret ? 'text' : 'password'}
                  value={botSecret}
                  onChange={(e) => setBotSecret(e.target.value)}
                  placeholder={t('wecomDialog.placeholders.botSecret')}
                />
                <Button
                  type="button"
                  variant="outline"
                  disabled={submitting}
                  onClick={() => setShowSecret((v) => !v)}
                >
                  {showSecret
                    ? t('wecomDialog.actions.hideSecret')
                    : t('wecomDialog.actions.showSecret')}
                </Button>
              </div>
            </label>
            {error && <p className="text-xs text-rose-500">{error}</p>}
          </div>

          <div className="mt-6 flex justify-end gap-2">
            <Button
              type="button"
              variant="outline"
              disabled={submitting}
              onClick={() => handleOpenChange(false)}
            >
              {t('wecomDialog.actions.cancel')}
            </Button>
            <Button type="button" disabled={submitting} onClick={handleSave}>
              {submitting ? t('wecomDialog.actions.saving') : t('wecomDialog.actions.save')}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

function CreateAgentCard({ onClick }: { onClick: () => void }) {
  const { t } = useTranslation('agentHub');
  return (
    <button
      type="button"
      onClick={onClick}
      className="panel flex min-h-[360px] flex-col items-center justify-center gap-3 border-dashed text-center transition-colors hover:border-primary/40 hover:bg-primary/5"
      style={{ borderStyle: 'dashed' }}
    >
      <div className="flex h-14 w-14 items-center justify-center rounded-full bg-primary/10 text-primary ring-1 ring-primary/30">
        <Plus size={28} strokeWidth={1.5} />
      </div>
      <div className="mt-1 text-base font-semibold">{t('newCard.title')}</div>
      <div className="text-xs text-muted-foreground">{t('newCard.subtitle')}</div>
      <div className="mt-2 flex flex-wrap items-center justify-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
        <span className="inline-flex items-center gap-1">
          <Check size={12} className="text-emerald-500" />
          {t('newCard.engines.openclaw')}
        </span>
        <span className="inline-flex items-center gap-1">
          <Check size={12} className="text-emerald-500" />
          {t('newCard.engines.hermes')}
        </span>
      </div>
    </button>
  );
}

function TeamComingSoon() {
  const { t } = useTranslation('agentHub');
  return (
    <div className="panel flex min-h-[280px] flex-col items-center justify-center gap-2 p-10 text-center">
      <div className="rounded-full bg-primary/10 px-3 py-1 text-xs font-medium text-primary ring-1 ring-primary/30">
        {t('tabs.comingSoonBadge')}
      </div>
      <h2 className="mt-3 text-lg font-semibold">{t('teamPlaceholder.title')}</h2>
      <p className="max-w-md text-sm text-muted-foreground">
        {t('teamPlaceholder.description')}
      </p>
    </div>
  );
}
