// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { api } from '@/lib/api';
import type { components } from './generated/schema';

export type ClusterOverviewDto = components['schemas']['ClusterOverview'];
export type ListedSandboxDto = components['schemas']['ListedSandbox'];
export type SandboxDetailDto = components['schemas']['SandboxDetail'];
export type SandboxSessionDto = components['schemas']['Sandbox'];
export type SandboxLogsDto = components['schemas']['SandboxLogsV2Response'];
export type SandboxLogEntry = components['schemas']['SandboxLogEntry'];
export type SandboxResumeRequest = components['schemas']['ResumedSandbox'];
export type TemplateSummaryDto = components['schemas']['TemplateSummary'];
export type TemplateDetailDto = components['schemas']['TemplateDetail'];
export type ApiNodeView = components['schemas']['NodeView'];
export type VersionMatrixDto = components['schemas']['VersionMatrixView'];
export type ComponentVersionDto = components['schemas']['ComponentVersionView'];

export interface RunningSandbox extends ListedSandboxDto {}

export interface SandboxDetail extends SandboxDetailDto {}

export interface TemplateSummary {
  templateID: string;
  instanceType?: string | null;
  version?: string | null;
  status: string;
  lastError?: string | null;
  createdAt?: string | null;
  imageInfo?: string | null;
  networkType?: string | null;
  allowInternetAccess?: boolean | null;
}

export interface TemplateDetail extends TemplateSummary {
  replicas: unknown[];
  createRequest?: unknown;
  networkType?: string | null;
  allowInternetAccess?: boolean | null;
}

export interface TemplateCompatSummary {
  staleTemplates: number;
  staleReplicas: number;
  affectedNodes: number;
  missingReplicas: number;
  unknownReplicas: number;
}

export interface TemplateNodeCompat {
  nodeID: string;
  nodeIP?: string | null;
  compatStatus: 'OK' | 'STALE' | 'UNKNOWN' | 'MISSING' | string;
  boundGuestImageVersion?: string | null;
  currentGuestImageVersion?: string | null;
  boundAgentVersion?: string | null;
  currentAgentVersion?: string | null;
  boundKernelVersion?: string | null;
  currentKernelVersion?: string | null;
}

export interface TemplateCompatRow {
  templateID: string;
  instanceType?: string | null;
  overall: 'OK' | 'STALE' | 'UNKNOWN' | 'MISSING' | string;
  nodes: TemplateNodeCompat[];
}

export interface TemplateCompatMatrix {
  summary: TemplateCompatSummary;
  templates: TemplateCompatRow[];
}

export interface ClusterNodeResourcesView {
  totalCpuMilli: number;
  allocatableCpuMilli: number;
  totalMemoryMB: number;
  allocatableMemoryMB: number;
  maxMvmSlots: number;
  quotaCpu: number;
  quotaMemMB: number;
  createConcurrentNum: number;
}

export interface ClusterNodeConditionView {
  type: string;
  status: string;
  lastTransitionTime?: string | null;
  reason?: string;
  message?: string;
}

export interface ClusterNodeView {
  nodeID: string;
  hostname?: string;
  status: string;
  role?: string;
  address?: string;
  resources: ClusterNodeResourcesView;
  conditions?: ClusterNodeConditionView[];
  saturationPct: number;
  memorySaturationPct: number;
  heartbeatTime?: string | null;
  healthy: boolean;
  isolated: boolean;
  isolatedAt?: number | null;
  isolatedBy?: string;
  isolatedReason?: string;
  localTemplates: string[];
  versions: ComponentVersionDto[];
}

export interface ClusterOverview extends ClusterOverviewDto {}

function mapSandbox(dto: ListedSandboxDto): RunningSandbox {
  return dto;
}

function mapSandboxDetail(dto: SandboxDetailDto): SandboxDetail {
  return dto;
}

function mapTemplateSummary(dto: TemplateSummaryDto): TemplateSummary {
  return {
    templateID: dto.templateID,
    instanceType: dto.instanceType,
    version: dto.version,
    status: dto.status,
    lastError: dto.lastError,
    createdAt: dto.createdAt,
    imageInfo: dto.imageInfo,
    networkType: (dto as unknown as { networkType?: string }).networkType ?? null,
    allowInternetAccess: (dto as unknown as { allowInternetAccess?: boolean }).allowInternetAccess ?? null,
  };
}

function mapTemplateDetail(dto: TemplateDetailDto): TemplateDetail {
  return {
    templateID: dto.templateID,
    instanceType: dto.instanceType,
    version: dto.version,
    status: dto.status,
    lastError: dto.lastError,
    createdAt: undefined,
    imageInfo: undefined,
    replicas: dto.replicas,
    createRequest: dto.createRequest,
    networkType: (dto as unknown as { networkType?: string }).networkType ?? null,
    allowInternetAccess: (dto as unknown as { allowInternetAccess?: boolean }).allowInternetAccess ?? null,
  };
}

function mapNode(dto: ApiNodeView): ClusterNodeView {
  return {
    nodeID: dto.nodeID,
    hostname: undefined,
    status: dto.healthy ? 'Ready' : 'Degraded',
    role: dto.instanceType ?? undefined,
    address: dto.hostIP,
    resources: {
      totalCpuMilli: dto.capacity.cpuMilli,
      allocatableCpuMilli: dto.allocatable.cpuMilli,
      totalMemoryMB: dto.capacity.memoryMB,
      allocatableMemoryMB: dto.allocatable.memoryMB,
      maxMvmSlots: dto.maxMvmSlots,
      quotaCpu: (dto as unknown as { quotaCpu?: number }).quotaCpu ?? 0,
      quotaMemMB: (dto as unknown as { quotaMemMB?: number }).quotaMemMB ?? 0,
      createConcurrentNum: (dto as unknown as { createConcurrentNum?: number }).createConcurrentNum ?? 0,
    },
    conditions: dto.conditions?.map((condition) => ({
      type: condition.type,
      status: condition.status,
      lastTransitionTime: condition.lastHeartbeatTime,
      reason: condition.reason,
      message: condition.message,
    })),
    saturationPct: Math.round(dto.cpuSaturation),
    memorySaturationPct: Math.round(dto.memorySaturation),
    heartbeatTime: dto.heartbeatTime,
    healthy: dto.healthy,
    isolated: dto.isolated ?? false,
    isolatedAt: dto.isolatedAt ?? null,
    isolatedBy: dto.isolatedBy ?? undefined,
    isolatedReason: dto.isolatedReason ?? undefined,
    localTemplates: dto.localTemplates ?? [],
    versions: dto.versions ?? [],
  };
}

const DEFAULT_RESUME_BODY: SandboxResumeRequest = {
  timeout: 15,
  autoPause: false,
};

export const sandboxApi = {
  list: (params?: { metadata?: string; state?: RunningSandbox['state']; nextToken?: string; limit?: number }) =>
    api<ListedSandboxDto[]>('/v2/sandboxes', { params }).then((items) => items.map(mapSandbox)),
  get: (id: string) => api<SandboxDetailDto>(`/sandboxes/${id}`).then(mapSandboxDetail),
  kill: (id: string) => api<void>(`/sandboxes/${id}`, { method: 'DELETE' }),
  pause: (id: string) => api<void>(`/sandboxes/${id}/pause`, { method: 'POST' }),
  resume: (id: string, body: SandboxResumeRequest = DEFAULT_RESUME_BODY) =>
    api<SandboxSessionDto>(`/sandboxes/${id}/resume`, {
      method: 'POST',
      body: JSON.stringify(body),
    }).then(() => undefined),
  setTimeout: (id: string, seconds: number) =>
    api<void>(`/sandboxes/${id}/timeout`, { method: 'POST', body: JSON.stringify({ timeout: seconds }) }),
  logs: (id: string, params?: { cursor?: number; limit?: number; direction?: string }) =>
    api<SandboxLogsDto>(`/v2/sandboxes/${id}/logs`, { params }),
  create: (body: {
    templateID: string;
    metadata?: Record<string, string>;
  }) =>
    api<SandboxSessionDto>('/sandboxes', {
      method: 'POST',
      body: JSON.stringify(body),
    }),
};

export const templateApi = {
  list: () => api<TemplateSummaryDto[]>('/templates').then((items) => items.map(mapTemplateSummary)),
  get: (id: string) => api<TemplateDetailDto>(`/templates/${id}`).then(mapTemplateDetail),
  create: (body: { templateID?: string; image: string; instanceType?: string; writableLayerSize?: string; exposedPorts?: number[]; probePort?: number; probePath?: string; cpu?: number; memory?: number; env?: string[]; allowInternetAccess?: boolean }) =>
    api<unknown>('/templates', { method: 'POST', body: JSON.stringify(body) }),
  rebuild: (id: string) => api<unknown>(`/templates/${id}`, { method: 'POST', body: JSON.stringify({}) }),
  getBuildStatus: (id: string, buildID: string) =>
    api<unknown>(`/templates/${id}/builds/${buildID}/status`),
  getBuildLogs: (id: string, buildID: string) =>
    api<{ lines?: string[]; status?: string; progress?: number }>(`/templates/${id}/builds/${buildID}/logs`),
  remove: (id: string) => api<void>(`/templates/${id}`, { method: 'DELETE' }),
  compat: () => api<TemplateCompatMatrix>('/templates/compat'),
  adoptCompatBaseline: (id: string) =>
    api<{ updated: number }>(`/templates/compat/${id}/adopt-baseline`, { method: 'POST' }),
};

export const versionApi = {
  matrix: () => api<VersionMatrixDto>('/cluster/versions'),
};

export const clusterApi = {
  overview: () => api<ClusterOverviewDto>('/cluster/overview'),
  nodes: () => api<ApiNodeView[]>('/nodes').then((items) => items.map(mapNode)),
  node: (id: string) => api<ApiNodeView>(`/nodes/${id}`).then(mapNode),
  setNodeIsolation: (id: string, isolated: boolean, reason?: string) =>
    api<ApiNodeView>(`/nodes/${id}/isolation`, {
      method: 'POST',
      body: JSON.stringify({ isolated, ...(reason ? { reason } : {}) }),
    }).then(mapNode),
  config: () => api<{
    apiEndpoint: string;
    rateLimitPerSec: number;
    authEnabled: boolean;
    sandboxDomain: string;
    instanceType: string;
  }>('/config'),
};

export interface ImageMeta {
  image: string;
  size_bytes: number;
  size_mb: number;
  digest: string | null;
  digest_short: string | null;
}

export interface StoreMeta {
  images: ImageMeta[];
}

export const storeApi = {
  meta: () => api<StoreMeta>('/store/meta'),
  refresh: () => api<StoreMeta>('/store/refresh', { method: 'POST' }),
};

export interface AgentInstanceDto {
  id: string;
  name: string;
  status: 'running' | 'starting' | 'stopped' | 'error';
  engine: 'openclaw' | 'hermes';
  env: 'linux' | 'mac';
  model: string;
  version: string;
  bots: Array<'wecom'>;
  botsAvailable: Array<'wecom'>;
  avatar: string;
  avatarTone: 'sky' | 'amber' | 'emerald' | 'rose' | 'violet';
  sandboxId: string;
  templateId: string;
  gatewayUrl: string;
  envUrl: string;
  persistenceMode?: 'full_snapshot' | 'shared_files';
  rootfsSourceType?: 'template' | 'snapshot';
  rootfsSourceId?: string;
  openclawPersistId?: string;
  openclawStatePath?: string;
  wecomConfig?: {
    botId: string;
    botSecret: string;
  };
  setup?: {
    exitCode: number;
    stdout: string;
    stderr: string;
  };
}

export interface AgentWeComConfigDto {
  botId: string;
  botSecret: string;
}

export interface AgentGatewayHealthDto {
  ready: boolean;
}

export interface AgentSetupResultDto {
  exitCode: number;
  stdout: string;
  stderr: string;
}

export interface AgentSnapshotDto {
  snapshotID: string;
  names: string[];
  status?: string;
  snapshotKind?: 'sandbox' | 'agenthub_state';
  originSandboxID?: string;
  publishedTemplateId?: string;
  rootfsSourceType?: 'template' | 'snapshot';
  rootfsSourceId?: string;
  rootfsSnapshotId?: string;
  openclawStateSnapshotPath?: string;
  templateReferenced: boolean;
  isHealthy: boolean;
  parentSnapshotID?: string;
  createdAt?: string;
  updatedAt?: string;
}

export interface AgentRollbackResponseDto {
  sandboxID: string;
  snapshotID: string;
  operationID: string;
  status: string;
}

export interface AgentRecoverResponseDto {
  recovered: boolean;
  method: 'restart' | 'rollback';
  snapshotID?: string;
}

export interface AgentPublishTemplateResponseDto {
  templateId: string;
  snapshotId: string;
  name?: string;
}

export interface AgentTemplateDto {
  templateId: string;
  name: string;
  sourceAgentId: string;
  sourceSnapshotId: string;
  sourceSandboxId: string;
  model: string;
  version: string;
  persistenceMode?: 'full_snapshot' | 'shared_files';
  recommended: boolean;
  createdAt?: string;
}

export interface AgentOperationDto {
  operationId: string;
  agentId: string;
  operationType: string;
  status: 'running' | 'succeeded' | 'failed';
  targetId?: string;
  errorMessage?: string;
  createdAt?: string;
  updatedAt?: string;
}

// 存档改为异步：接口立即返回操作 ID，前端轮询操作流水获知完成状态。
export interface AgentSnapshotJobDto {
  operationId: string | null;
  status: string;
}

export interface SessionDto {
  authRequired: boolean;
  authenticated: boolean;
  username?: string;
}

export interface LoginResponseDto {
  token: string;
  username: string;
  expiresInSecs: number;
}

export const authApi = {
  session: () => api<SessionDto>('/auth/session'),
  login: (body: { username: string; password: string }) =>
    api<LoginResponseDto>('/auth/login', { method: 'POST', body: JSON.stringify(body) }),
  logout: () => api<void>('/auth/logout', { method: 'POST' }),
  changePassword: (body: { username: string; oldPassword: string; newPassword: string }) =>
    api<void>('/auth/change-password', { method: 'POST', body: JSON.stringify(body) }),
};

export interface AgentSettingsDto {
  /** Backward-compatible alias for the default LLM API key state. */
  deepseekApiKeyConfigured: boolean;
  /** Backward-compatible masked preview. Never the full key. */
  deepseekApiKeyMasked?: string;
  /** Backward-compatible key source. */
  source: 'database' | 'none';
  /** LLM provider id, e.g. "deepseek" or "custom". */
  llmProvider: string;
  /** OpenAI-compatible base URL. */
  llmBaseUrl: string;
  /** Default model id injected into OpenClaw. */
  llmModel: string;
  /** Whether a usable default LLM API key is available. */
  llmApiKeyConfigured: boolean;
  /** Masked preview of the default LLM API key. Never the full key. */
  llmApiKeyMasked?: string;
  /** Where the LLM API key comes from. */
  llmApiKeySource: 'database' | 'none';
  /** How the LLM credential is delivered to OpenClaw. */
  llmCredentialMode: 'egress' | 'env';
  /** Whether settings can be persisted (requires the AgentHub database). */
  persistenceEnabled: boolean;
  /**
   * Configured gateway domain (e.g. "cube.app"), or undefined when not set.
   * Assistants open their OpenClaw gateway via `<port>-<sandboxId>.<domain>`
   * (subdomain origin) when this is present.
   */
  gatewayDomain?: string;
}

export const agentHubApi = {
  list: () => api<AgentInstanceDto[]>('/agenthub/instances'),
  listTemplates: () => api<AgentTemplateDto[]>('/agenthub/templates'),
  registerMarketTemplate: (body: {
    templateId: string;
    name?: string;
    model?: string;
    version?: string;
    recommended?: boolean;
  }) =>
    api<AgentTemplateDto>('/agenthub/templates/market', {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  getSettings: () => api<AgentSettingsDto>('/agenthub/settings'),
  updateSettings: (body: {
    deepseekApiKey?: string;
    llmProvider?: string;
    llmBaseUrl?: string;
    llmModel?: string;
    llmApiKey?: string;
    llmCredentialMode?: 'egress' | 'env';
    gatewayDomain?: string;
  }) =>
    api<AgentSettingsDto>('/agenthub/settings', {
      method: 'PUT',
      body: JSON.stringify(body),
    }),
  create: (body: {
    name: string;
    engine: 'openclaw';
    model?: string;
    templateId?: string;
    snapshotId?: string;
    persistenceMode?: 'full_snapshot' | 'shared_files';
    botId?: string;
    botSecret?: string;
  }) =>
    api<AgentInstanceDto>('/agenthub/instances', {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  delete: (id: string) =>
    api<void>(`/agenthub/instances/${encodeURIComponent(id)}`, {
      method: 'DELETE',
    }),
  restart: (id: string) =>
    api<AgentSetupResultDto>(`/agenthub/instances/${encodeURIComponent(id)}/restart`, {
      method: 'POST',
    }),
  pause: (id: string) =>
    api<AgentInstanceDto>(`/agenthub/instances/${encodeURIComponent(id)}/pause`, {
      method: 'POST',
    }),
  resume: (id: string) =>
    api<AgentInstanceDto>(`/agenthub/instances/${encodeURIComponent(id)}/resume`, {
      method: 'POST',
    }),
  upgrade: (id: string) =>
    api<AgentSetupResultDto>(`/agenthub/instances/${encodeURIComponent(id)}/upgrade`, {
      method: 'POST',
    }),
  updateModel: (id: string, body: { model: string }) =>
    api<AgentInstanceDto>(`/agenthub/instances/${encodeURIComponent(id)}/model`, {
      method: 'PUT',
      body: JSON.stringify(body),
    }),
  updateWecomConfig: (id: string, body: { botId: string; botSecret: string }) =>
    api<AgentInstanceDto>(`/agenthub/instances/${encodeURIComponent(id)}/wecom`, {
      method: 'PUT',
      body: JSON.stringify(body),
    }),
  getWecomConfig: (id: string) =>
    api<AgentWeComConfigDto | null>(`/agenthub/instances/${encodeURIComponent(id)}/wecom`),
  getGatewayHealth: (id: string) =>
    api<AgentGatewayHealthDto>(`/agenthub/instances/${encodeURIComponent(id)}/gateway/health`),
  listOperations: (id: string) =>
    api<AgentOperationDto[]>(`/agenthub/instances/${encodeURIComponent(id)}/operations`),
  listSnapshots: (id: string) =>
    api<AgentSnapshotDto[]>(`/agenthub/instances/${encodeURIComponent(id)}/snapshots`),
  createSnapshot: (id: string, body: { name?: string }) =>
    api<AgentSnapshotJobDto>(`/agenthub/instances/${encodeURIComponent(id)}/snapshots`, {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  deleteSnapshot: (id: string, snapshotId: string) =>
    api<void>(`/agenthub/instances/${encodeURIComponent(id)}/snapshots/${encodeURIComponent(snapshotId)}`, {
      method: 'DELETE',
    }),
  updateSnapshot: (id: string, snapshotId: string, body: { name?: string; isHealthy?: boolean }) =>
    api<void>(`/agenthub/instances/${encodeURIComponent(id)}/snapshots/${encodeURIComponent(snapshotId)}`, {
      method: 'PATCH',
      body: JSON.stringify(body),
    }),
  recover: (id: string) =>
    api<AgentRecoverResponseDto>(`/agenthub/instances/${encodeURIComponent(id)}/recover`, {
      method: 'POST',
    }),
  rollback: (id: string, body: { snapshotId: string }) =>
    api<AgentRollbackResponseDto>(`/agenthub/instances/${encodeURIComponent(id)}/rollback`, {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  clone: (id: string, body: { name?: string; snapshotId?: string }) =>
    api<AgentInstanceDto>(`/agenthub/instances/${encodeURIComponent(id)}/clone`, {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  publishTemplate: (id: string, body: { name?: string; snapshotId?: string }) =>
    api<AgentPublishTemplateResponseDto>(`/agenthub/instances/${encodeURIComponent(id)}/publish-template`, {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  updateTemplate: (templateId: string, body: { name?: string; recommended?: boolean }) =>
    api<void>(`/agenthub/templates/${encodeURIComponent(templateId)}`, {
      method: 'PATCH',
      body: JSON.stringify(body),
    }),
  deleteTemplate: (templateId: string) =>
    api<void>(`/agenthub/templates/${encodeURIComponent(templateId)}`, {
      method: 'DELETE',
    }),
};
