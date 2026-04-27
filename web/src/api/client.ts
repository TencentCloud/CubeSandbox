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
export type NodeDto = components['schemas']['NodeView'];

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
}

export interface TemplateDetail extends TemplateSummary {
  replicas: unknown[];
  createRequest?: unknown;
}

export interface NodeResourcesView {
  totalCpuMilli: number;
  allocatableCpuMilli: number;
  totalMemoryMB: number;
  allocatableMemoryMB: number;
  maxMvmSlots: number;
}

export interface NodeConditionView {
  type: string;
  status: string;
  lastTransitionTime?: string | null;
  reason?: string;
  message?: string;
}

export interface NodeView {
  nodeID: string;
  hostname?: string;
  status: string;
  role?: string;
  address?: string;
  resources: NodeResourcesView;
  conditions?: NodeConditionView[];
  saturationPct: number;
  memorySaturationPct: number;
  heartbeatTime?: string | null;
  healthy: boolean;
  localTemplates: string[];
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
  };
}

function mapNode(dto: NodeDto): NodeView {
  return {
    nodeID: dto.nodeID,
    hostname: undefined,
    status: dto.healthy ? 'Ready' : 'Degraded',
    role: dto.instanceType ?? undefined,
    address: dto.hostIP,
    resources: {
      totalCpuMilli: dto.capacity.cpuCores * 1000,
      allocatableCpuMilli: dto.allocatable.cpuCores * 1000,
      totalMemoryMB: dto.capacity.memoryMB,
      allocatableMemoryMB: dto.allocatable.memoryMB,
      maxMvmSlots: dto.maxMvmSlots,
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
    localTemplates: dto.localTemplates ?? [],
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
};

export const templateApi = {
  list: () => api<TemplateSummaryDto[]>('/templates').then((items) => items.map(mapTemplateSummary)),
  get: (id: string) => api<TemplateDetailDto>(`/templates/${id}`).then(mapTemplateDetail),
  remove: (id: string) => api<void>(`/templates/${id}`, { method: 'DELETE' }),
};

export const clusterApi = {
  overview: () => api<ClusterOverviewDto>('/cluster/overview'),
  nodes: () => api<NodeDto[]>('/nodes').then((items) => items.map(mapNode)),
  node: (id: string) => api<NodeDto>(`/nodes/${id}`).then(mapNode),
};
