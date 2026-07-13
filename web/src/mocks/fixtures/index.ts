// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import type { components } from '@/api/generated/schema';
import type { TemplateCompatMatrix } from '@/api/client';

type ClusterOverviewDto = components['schemas']['ClusterOverview'];
type ListedSandboxDto = components['schemas']['ListedSandbox'];
type SandboxDetailDto = components['schemas']['SandboxDetail'];
type SandboxLogsDto = components['schemas']['SandboxLogsV2Response'];
type SandboxSessionDto = components['schemas']['Sandbox'];
type TemplateDetailDto = components['schemas']['TemplateDetail'];
type TemplateSummaryDto = components['schemas']['TemplateSummary'];
type NodeDto = components['schemas']['NodeView'];
type VersionMatrixDto = components['schemas']['VersionMatrixView'];

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
      jobID: 'job-mock-python-ready-01',
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
      jobID: 'job-mock-cuda-build-01',
      createdAt: ago(86_400 * 8),
      imageInfo: 'registry.cube.dev/templates/cuda12-torch:2.4.0',
    },
    {
      templateID: 'playwright-chromium',
      instanceType: 'standard',
      version: '1.47.0',
      status: 'failed',
      jobID: 'job-mock-playwright-failed-01',
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
      quotaCpu: 64_000,
      quotaMemMB: 131_072,
      createConcurrentNum: 8,
      heartbeatTime: ago(12),
      conditions: [
        { type: 'Ready', status: 'True', lastHeartbeatTime: ago(12) },
        { type: 'KernelDeadlock', status: 'False', lastHeartbeatTime: ago(60) },
      ],
      localTemplates: ['python-3.11-ai', 'nodejs-20-web', 'ubuntu-24.04'],
      versions: [
        { component: 'cubelet', version: 'v0.5.0', commit: 'a1b2c3d4e5f6', source: 'binary' },
        { component: 'containerd-shim-cube-rs', version: 'v0.5.0', source: 'manifest' },
        { component: 'cube-runtime', version: 'v0.5.0', source: 'manifest' },
        { component: 'cube-agent', version: 'agent-1.2.3', source: 'manifest' },
        { component: 'guest-image', version: 'cube-image/2026.01', source: 'file' },
        { component: 'kernel', version: '5.10.0-100', source: 'manifest' },
      ],
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
      quotaCpu: 48_000,
      quotaMemMB: 98_304,
      createConcurrentNum: 6,
      heartbeatTime: ago(9),
      conditions: [{ type: 'Ready', status: 'True', lastHeartbeatTime: ago(9) }],
      localTemplates: ['nodejs-20-web', 'go-1.22', 'ubuntu-24.04'],
      // edge-02 is on a different cubelet version, which demonstrates normal
      // multi-version distribution plus an undeclared version marker.
      versions: [
        { component: 'cubelet', version: 'v0.4.9', commit: 'f6e5d4c3b2a1', source: 'binary' },
        { component: 'containerd-shim-cube-rs', version: 'v0.5.0', source: 'manifest' },
        { component: 'cube-runtime', version: 'v0.5.0', source: 'manifest' },
        { component: 'cube-agent', version: 'agent-1.2.2', source: 'manifest' },
        { component: 'guest-image', version: 'cube-image/2026.01', source: 'file' },
        { component: 'kernel', version: '5.10.0-100', source: 'manifest' },
      ],
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
      quotaCpu: 32_000,
      quotaMemMB: 65_536,
      createConcurrentNum: 4,
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
      // edge-03 is unhealthy AND running a guest-image outside the release
      // declaration, covering "not ready" + undeclared in the matrix table.
      versions: [
        { component: 'cubelet', version: 'v0.5.0', commit: 'a1b2c3d4e5f6', source: 'binary' },
        { component: 'containerd-shim-cube-rs', version: 'v0.5.0', source: 'manifest' },
        { component: 'cube-runtime', version: 'v0.5.0', source: 'manifest' },
        { component: 'cube-agent', version: 'agent-1.2.3', source: 'manifest' },
        { component: 'guest-image', version: 'cube-image/2025.12', source: 'file' },
        { component: 'kernel', version: '5.10.0-100', source: 'manifest' },
      ],
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

function buildMockCreateRequest(base: TemplateSummaryDto) {
  const containerBase = {
    image: { writable_layer_size: '1G' },
    resources: { cpu: '2000m', mem: '2048Mi' },
    probe: { probe_handler: { http_get: { path: '/health', port: 8080 } } },
  };

  const common = {
    templateID: base.templateID,
    instanceType: base.instanceType ?? 'standard',
    image: base.imageInfo,
    annotations: { 'com.exposed_ports': '8080' },
    containers: [containerBase],
  };

  switch (base.templateID) {
    case 'python-3.11-ai':
      return {
        ...common,
        network_type: 'tap',
        cubevs_context: {
          allowInternetAccess: true,
          allowOut: ['172.67.0.0/16'],
          denyOut: ['10.0.0.0/8'],
        },
        containers: [{
          ...containerBase,
          envs: [
            { key: 'APP_ENV', value: 'production' },
            { key: 'DEBUG', value: 'false' },
          ],
          dns_config: { servers: ['8.8.8.8', '1.1.1.1'] },
        }],
      };
    case 'nodejs-20-web':
      return {
        ...common,
        network_type: 'tap',
        cubevs_context: { allowInternetAccess: false },
        containers: [{
          ...containerBase,
          envs: [{ key: 'NODE_ENV', value: 'production' }],
          dns_config: { servers: ['114.114.114.114'] },
        }],
      };
    default:
      return common;
  }
}

function mockTemplateNetworkFields(templateID: string) {
  switch (templateID) {
    case 'python-3.11-ai':
      return { networkType: 'tap', allowInternetAccess: true };
    case 'nodejs-20-web':
      return { networkType: 'tap', allowInternetAccess: false };
    default:
      return { networkType: null, allowInternetAccess: null };
  }
}

export function getTemplate(templateID: string): TemplateDetailDto | undefined {
  const base = templates.find((item) => item.templateID === templateID);
  if (!base) return undefined;
  const network = mockTemplateNetworkFields(base.templateID);
  return {
    templateID: base.templateID,
    instanceType: base.instanceType,
    version: base.version,
    status: base.status,
    lastError: base.lastError,
    replicas: [
      {
        node_id: 'cube-edge-01',
        node_ip: '10.0.2.11',
        phase: 'READY',
        status: 'READY',
        spec: 'cpu=2000m,mem=4096Mi',
        artifact_id: 'rfs-mock-edge-01',
        last_job_id: 'job-mock-edge-01',
        compat_status: base.templateID === 'python-3.11-ai' ? 'STALE' : 'OK',
        guest_image_version: base.templateID === 'python-3.11-ai' ? 'guest-image@2024.11.02' : 'guest-image@2024.12.01',
        agent_version: base.templateID === 'python-3.11-ai' ? 'cube-agent@0.1.7' : 'cube-agent@0.1.8',
      },
      {
        node_id: 'cube-edge-02',
        node_ip: '10.0.2.12',
        phase: base.status === 'failed' ? 'FAILED' : 'READY',
        status: base.status === 'failed' ? 'FAILED' : 'READY',
        spec: 'cpu=2000m,mem=4096Mi',
        artifact_id: 'rfs-mock-edge-02',
        last_job_id: 'job-mock-edge-02',
        compat_status: base.templateID === 'nodejs-20-web' ? 'UNKNOWN' : 'OK',
        guest_image_version: 'guest-image@2024.12.01',
        agent_version: 'cube-agent@0.1.8',
      },
    ],
    createRequest: buildMockCreateRequest(base),
    ...network,
  } as TemplateDetailDto;
}

export function getTemplateCompat(): TemplateCompatMatrix {
  return {
    summary: {
      staleTemplates: 1,
      staleReplicas: 1,
      affectedNodes: 1,
      missingReplicas: 1,
      unknownReplicas: 1,
    },
    templates: [
      {
        templateID: 'python-3.11-ai',
        instanceType: 'standard',
        overall: 'STALE',
        nodes: [
          {
            nodeID: 'cube-edge-01',
            nodeIP: '10.0.2.11',
            compatStatus: 'STALE',
            boundGuestImageVersion: 'guest-image@2024.11.02',
            currentGuestImageVersion: 'guest-image@2024.12.01',
            boundAgentVersion: 'cube-agent@0.1.7',
            currentAgentVersion: 'cube-agent@0.1.8',
            boundKernelVersion: 'kernel@6.6.32-cube',
            currentKernelVersion: 'kernel@6.6.32-cube',
          },
          {
            nodeID: 'cube-edge-02',
            nodeIP: '10.0.2.12',
            compatStatus: 'OK',
            boundGuestImageVersion: 'guest-image@2024.11.02',
            currentGuestImageVersion: 'guest-image@2024.11.02',
            boundAgentVersion: 'cube-agent@0.1.7',
            currentAgentVersion: 'cube-agent@0.1.7',
            boundKernelVersion: 'kernel@6.6.32-cube',
            currentKernelVersion: 'kernel@6.6.32-cube',
          },
        ],
      },
      {
        templateID: 'nodejs-20-web',
        instanceType: 'standard',
        overall: 'UNKNOWN',
        nodes: [
          { nodeID: 'cube-edge-01', nodeIP: '10.0.2.11', compatStatus: 'UNKNOWN' },
          { nodeID: 'cube-edge-02', nodeIP: '10.0.2.12', compatStatus: 'MISSING' },
        ],
      },
    ],
  };
}

export function listNodes() {
  return clone(nodes);
}

export function getNode(nodeID: string) {
  const node = nodes.find((item) => item.nodeID === nodeID);
  return node ? clone(node) : undefined;
}

export function getVersionMatrix(): VersionMatrixDto {
  const declared: Record<string, string> = {
    cubelet: 'v0.5.0',
    'containerd-shim-cube-rs': 'v0.5.0',
    'cube-runtime': 'v0.5.0',
    'cube-agent': 'agent-1.2.3',
    'guest-image': 'cube-image/2026.01',
    kernel: '5.10.0-100',
  };

  const componentNodes = new Map<string, Map<string, string[]>>();
  for (const node of nodes) {
    for (const v of node.versions ?? []) {
      const version = v.version ?? '';
      if (!componentNodes.has(v.component)) componentNodes.set(v.component, new Map());
      const byVersion = componentNodes.get(v.component)!;
      const list = byVersion.get(version) ?? [];
      list.push(node.nodeID);
      byVersion.set(version, list);
    }
  }

  const components = Array.from(componentNodes.entries())
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([component, byVersion]) => {
      const versions = Array.from(byVersion.entries())
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([version, nodeIds]) => ({ version, nodes: nodeIds }));
      return {
        component,
        declaredVersion: declared[component] ?? '',
        declaredVersions: declared[component] ? [declared[component]] : [],
        consistent: versions.length <= 1,
        versions,
      };
    });

  const matrixNodes = nodes.map((node) => ({
    nodeID: node.nodeID,
    healthy: node.healthy,
    components: (node.versions ?? [])
      .slice()
      .sort((a, b) => a.component.localeCompare(b.component))
      .map((v) => ({
        component: v.component,
        version: v.version ?? '',
        declared: !!declared[v.component] && v.version === declared[v.component],
      })),
  }));

  return {
    controlPlane: { version: 'v0.5.0', commit: 'a1b2c3d4e5f6a1b2', buildTime: '2026-01-15T08:00:00Z' },
    components,
    nodes: matrixNodes,
  };
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

export function createSandbox(body: {
  templateID: string;
  timeout?: number;
  alias?: string;
  autoPause?: boolean;
  metadata?: Record<string, string>;
}): SandboxSessionDto {
  const sandboxID = 'isb_' + Math.random().toString(36).slice(2, 18).padEnd(16, '0');
  const newSandbox: ListedSandboxDto = {
    templateID: body.templateID,
    sandboxID,
    alias: body.alias,
    clientID: 'dashboard',
    startedAt: new Date().toISOString(),
    endAt: later((body.timeout ?? 300)),
    cpuCount: 2,
    memoryMB: 4096,
    diskSizeMB: 8192,
    metadata: body.metadata ?? {},
    state: 'running',
    envdVersion: '0.1.7',
  };
  sandboxes.push(newSandbox);
  return {
    templateID: newSandbox.templateID,
    sandboxID: newSandbox.sandboxID,
    alias: newSandbox.alias,
    clientID: newSandbox.clientID,
    envdVersion: newSandbox.envdVersion,
    envdAccessToken: `eat_${sandboxID.slice(-8)}`,
    trafficAccessToken: undefined,
    domain: 'cube.local',
  };
}
