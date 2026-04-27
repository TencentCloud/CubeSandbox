// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import type { components } from '@/api/generated/schema';

type ClusterOverviewDto = components['schemas']['ClusterOverview'];
type ListedSandboxDto = components['schemas']['ListedSandbox'];
type SandboxDetailDto = components['schemas']['SandboxDetail'];
type SandboxLogsDto = components['schemas']['SandboxLogsV2Response'];
type SandboxSessionDto = components['schemas']['Sandbox'];
type TemplateDetailDto = components['schemas']['TemplateDetail'];
type TemplateSummaryDto = components['schemas']['TemplateSummary'];
type NodeDto = components['schemas']['NodeView'];

const ago = (secs: number) => new Date(Date.now() - secs * 1000).toISOString();
const later = (secs: number) => new Date(Date.now() + secs * 1000).toISOString();
const clone = <T>(value: T): T => JSON.parse(JSON.stringify(value)) as T;

function buildSandboxes(): ListedSandboxDto[] {
  return [
    {
      templateID: 'python-3.11-ai',
      alias: 'pyai-analyst-03',
      sandboxID: 'isb_9f2e4c7a1b0d83e6',
      clientID: 'ops-east-1',
      startedAt: ago(137),
      endAt: later(3200),
      cpuCount: 4,
      memoryMB: 8192,
      diskSizeMB: 10_240,
      metadata: { project: 'data-pipeline', owner: 'ops@cube.dev', region: 'cn-shanghai' },
      state: 'running',
      envdVersion: '0.1.7',
      volumeMounts: [{ name: 'workspace', path: '/workspace' }],
    },
    {
      templateID: 'nodejs-20-web',
      alias: 'web-preview-21',
      sandboxID: 'isb_7711bb32e8ad4c90',
      clientID: 'frontend-ci',
      startedAt: ago(32),
      endAt: later(1700),
      cpuCount: 2,
      memoryMB: 4096,
      diskSizeMB: 8192,
      metadata: { branch: 'feat/dashboard-ui' },
      state: 'running',
      envdVersion: '0.1.7',
    },
    {
      templateID: 'ubuntu-24.04',
      alias: 'debug-session-17',
      sandboxID: 'isb_5a04c1f7b82039e1',
      clientID: 'research',
      startedAt: ago(6200),
      endAt: later(800),
      cpuCount: 2,
      memoryMB: 2048,
      diskSizeMB: 4096,
      metadata: { paused_reason: 'manual' },
      state: 'paused',
      envdVersion: '0.1.6',
    },
    {
      templateID: 'go-1.22',
      alias: 'go-api-stage',
      sandboxID: 'isb_0e41aa9c0b8f2d3f',
      clientID: 'stage-cluster',
      startedAt: ago(48),
      endAt: later(3400),
      cpuCount: 2,
      memoryMB: 4096,
      diskSizeMB: 8192,
      metadata: { deployment: 'canary-0.3' },
      state: 'running',
      envdVersion: '0.1.7',
    },
  ];
}

function buildTemplates(): TemplateSummaryDto[] {
  return [
    {
      templateID: 'python-3.11-ai',
      instanceType: 'standard',
      version: '2024.11.02',
      status: 'ready',
      createdAt: ago(86_400 * 18),
      imageInfo: 'registry.cube.dev/templates/python-3.11-ai:2024.11.02',
    },
    {
      templateID: 'nodejs-20-web',
      instanceType: 'standard',
      version: '2024.10.21',
      status: 'ready',
      createdAt: ago(86_400 * 34),
      imageInfo: 'registry.cube.dev/templates/nodejs-20-web:20.18.0',
    },
    {
      templateID: 'cuda-12-pytorch',
      instanceType: 'gpu',
      version: '2.4.0',
      status: 'building',
      createdAt: ago(86_400 * 8),
      imageInfo: 'registry.cube.dev/templates/cuda12-torch:2.4.0',
    },
    {
      templateID: 'playwright-chromium',
      instanceType: 'standard',
      version: '1.47.0',
      status: 'failed',
      lastError: 'image pull backoff: 429 Too Many Requests from registry',
      createdAt: ago(3600 * 4),
      imageInfo: 'registry.cube.dev/templates/playwright:1.47.0',
    },
  ];
}

function buildNodes(): NodeDto[] {
  return [
    {
      nodeID: 'cube-edge-01',
      hostIP: '10.0.2.11',
      instanceType: 'standard',
      healthy: true,
      capacity: { cpuMilli: 64_000, memoryMB: 131_072 },
      allocatable: { cpuMilli: 19_000, memoryMB: 42_800 },
      cpuSaturation: 70.3,
      memorySaturation: 67.3,
      maxMvmSlots: 32,
      heartbeatTime: ago(12),
      conditions: [
        { type: 'Ready', status: 'True', lastHeartbeatTime: ago(12) },
        { type: 'KernelDeadlock', status: 'False', lastHeartbeatTime: ago(60) },
      ],
      localTemplates: ['python-3.11-ai', 'nodejs-20-web', 'ubuntu-24.04'],
    },
    {
      nodeID: 'cube-edge-02',
      hostIP: '10.0.2.12',
      instanceType: 'standard',
      healthy: true,
      capacity: { cpuMilli: 48_000, memoryMB: 98_304 },
      allocatable: { cpuMilli: 22_000, memoryMB: 55_000 },
      cpuSaturation: 54.2,
      memorySaturation: 44.0,
      maxMvmSlots: 24,
      heartbeatTime: ago(9),
      conditions: [{ type: 'Ready', status: 'True', lastHeartbeatTime: ago(9) }],
      localTemplates: ['nodejs-20-web', 'go-1.22', 'ubuntu-24.04'],
    },
    {
      nodeID: 'cube-edge-03',
      hostIP: '10.0.2.13',
      instanceType: 'standard',
      healthy: false,
      capacity: { cpuMilli: 32_000, memoryMB: 65_536 },
      allocatable: { cpuMilli: 3_000, memoryMB: 4_100 },
      cpuSaturation: 90.6,
      memorySaturation: 93.7,
      maxMvmSlots: 16,
      heartbeatTime: ago(48),
      conditions: [
        {
          type: 'Ready',
          status: 'False',
          lastHeartbeatTime: ago(48),
          reason: 'HighPressure',
          message: 'CPU saturation > 90% for 5m',
        },
        { type: 'MemoryPressure', status: 'True', lastHeartbeatTime: ago(60) },
      ],
      localTemplates: ['ubuntu-24.04'],
    },
  ];
}

let sandboxes = buildSandboxes();
let templates = buildTemplates();
let nodes = buildNodes();

export function resetMockState() {
  sandboxes = buildSandboxes();
  templates = buildTemplates();
  nodes = buildNodes();
}

export async function mockDelay() {
  const ms = 140 + Math.random() * 240;
  await new Promise((resolve) => setTimeout(resolve, ms));
}

export function listSandboxes(filters: { state?: string | null; metadata?: string | null } = {}) {
  const { state, metadata } = filters;
  return clone(
    sandboxes.filter((sandbox) => {
      if (state && sandbox.state !== state) return false;
      if (!metadata) return true;
      const pairs = new URLSearchParams(metadata);
      return Array.from(pairs.entries()).every(([key, value]) => sandbox.metadata?.[key] === value);
    }),
  );
}

export function getSandboxDetail(sandboxID: string): SandboxDetailDto | undefined {
  const sandbox = sandboxes.find((item) => item.sandboxID === sandboxID);
  if (!sandbox) return undefined;
  return {
    ...clone(sandbox),
    envdAccessToken: `eat_${sandbox.sandboxID.slice(-8)}`,
    domain: 'cube.local',
  };
}

export function getSandboxSession(sandboxID: string): SandboxSessionDto | undefined {
  const sandbox = sandboxes.find((item) => item.sandboxID === sandboxID);
  if (!sandbox) return undefined;
  return {
    templateID: sandbox.templateID,
    sandboxID: sandbox.sandboxID,
    alias: sandbox.alias,
    clientID: sandbox.clientID,
    envdVersion: sandbox.envdVersion,
    envdAccessToken: `eat_${sandbox.sandboxID.slice(-8)}`,
    trafficAccessToken: undefined,
    domain: 'cube.local',
  };
}

export function deleteSandbox(sandboxID: string) {
  const before = sandboxes.length;
  sandboxes = sandboxes.filter((sandbox) => sandbox.sandboxID !== sandboxID);
  return sandboxes.length !== before;
}

export function pauseSandbox(sandboxID: string) {
  const sandbox = sandboxes.find((item) => item.sandboxID === sandboxID);
  if (!sandbox) return undefined;
  sandbox.state = 'paused';
  return clone(sandbox);
}

export function resumeSandbox(sandboxID: string) {
  const sandbox = sandboxes.find((item) => item.sandboxID === sandboxID);
  if (!sandbox) return undefined;
  sandbox.state = 'running';
  sandbox.endAt = later(1800);
  return getSandboxSession(sandboxID);
}

export function listTemplates() {
  return clone(templates);
}

export function getTemplate(templateID: string): TemplateDetailDto | undefined {
  const base = templates.find((item) => item.templateID === templateID);
  if (!base) return undefined;
  return {
    templateID: base.templateID,
    instanceType: base.instanceType,
    version: base.version,
    status: base.status,
    lastError: base.lastError,
    replicas: [
      { node: 'cube-edge-01', ready: true, localVersion: base.version },
      { node: 'cube-edge-02', ready: base.status !== 'failed', localVersion: base.version },
    ],
    createRequest: {
      templateID: base.templateID,
      instanceType: base.instanceType ?? 'standard',
      image: base.imageInfo,
    },
  };
}

export function listNodes() {
  return clone(nodes);
}

export function getNode(nodeID: string) {
  const node = nodes.find((item) => item.nodeID === nodeID);
  return node ? clone(node) : undefined;
}

export function getClusterOverview(): ClusterOverviewDto {
  const totalCpuMilli = nodes.reduce((sum, node) => sum + node.capacity.cpuMilli, 0);
  const allocatableCpuMilli = nodes.reduce((sum, node) => sum + node.allocatable.cpuMilli, 0);
  const totalMemoryMB = nodes.reduce((sum, node) => sum + node.capacity.memoryMB, 0);
  const allocatableMemoryMB = nodes.reduce((sum, node) => sum + node.allocatable.memoryMB, 0);

  return {
    nodeCount: nodes.length,
    healthyNodes: nodes.filter((node) => node.healthy).length,
    totalCpuMilli,
    allocatableCpuMilli,
    totalMemoryMB,
    allocatableMemoryMB,
    maxMvmSlots: nodes.reduce((sum, node) => sum + node.maxMvmSlots, 0),
  };
}

export function getSandboxLogs(sandboxID: string): SandboxLogsDto | undefined {
  const sandbox = sandboxes.find((item) => item.sandboxID === sandboxID);
  if (!sandbox) return undefined;
  return {
    logs: [
      {
        timestamp: ago(120),
        level: 'info',
        message: 'sandbox booted',
        fields: { sandboxID, boot_ms: '612' },
      },
      {
        timestamp: ago(65),
        level: 'info',
        message: 'network attached',
        fields: { iface: 'eth0', ip: '10.244.1.37' },
      },
      {
        timestamp: ago(18),
        level: sandbox.state === 'paused' ? 'warn' : 'info',
        message: sandbox.state === 'paused' ? 'sandbox paused by operator' : 'client connected',
        fields: sandbox.state === 'paused' ? { actor: 'dashboard' } : { client: 'sdk/python@1.4.2' },
      },
    ],
  };
}
