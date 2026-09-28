// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import type {
  WarehouseComponentDetail,
  WarehouseComponentSummary,
  WarehouseImportJob,
  WarehousePreinstallJob,
  WarehouseVersionGroup,
} from '@/api/client';

const ago = (secs: number) => new Date(Date.now() - secs * 1000).toISOString();
const clone = <T>(value: T): T => JSON.parse(JSON.stringify(value)) as T;

function nid(prefix: string): string {
  if (typeof crypto !== 'undefined' && crypto.randomUUID) {
    return crypto.randomUUID();
  }
  return `${prefix}-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

type NodeInstall = { nodeId: string; arch: string; component: string; version: string };

type WarehouseItem = {
  arch: string;
  component: string;
  version: string;
  source: string;
  sourceRef: string;
  objectKey: string;
  sizeBytes: number;
  checksum: string;
  createdAt: string;
  updatedAt: string;
};

const CATALOG = ['cube-shim', 'cube-image', 'cube-agent', 'cube-kernel-scf'] as const;

function item(
  arch: string,
  component: string,
  version: string,
  source: string,
  sizeBytes: number,
  ageSec: number,
): WarehouseItem {
  const createdAt = ago(ageSec);
  return {
    arch,
    component,
    version,
    source,
    sourceRef: source === 'github' ? 'TencentCloud/CubeSandbox' : 'local-upload',
    objectKey: `warehouse/blobs/${arch}/${component}/${version}/component.tar.gz`,
    sizeBytes,
    checksum: `sha256:${component.slice(0, 8)}${version.replace(/[^a-z0-9]/gi, '').slice(0, 8)}deadbeef`,
    createdAt,
    updatedAt: createdAt,
  };
}

function buildItems(): WarehouseItem[] {
  return [
    item('amd64', 'cube-shim', 'v0.6.0', 'github', 82_345_216, 86_400 * 2),
    item('amd64', 'cube-image', 'v0.6.0', 'github', 412_221_440, 86_400 * 2),
    item('amd64', 'cube-agent', 'v0.6.0', 'github', 8_388_608, 86_400 * 2),
    item('amd64', 'cube-kernel-scf', 'sha256-b7c91a3e4f12', 'github', 61_472_768, 86_400 * 2),
    item('amd64', 'cube-shim', 'v0.5.0', 'github', 80_117_760, 86_400 * 18),
    item('arm64', 'cube-shim', 'v0.6.0', 'github', 79_691_776, 86_400),
    item('arm64', 'cube-image', 'v0.6.0', 'github', 398_458_880, 86_400),
  ];
}

function buildInstalls(): NodeInstall[] {
  const v06 = [
    ['cube-shim', 'v0.6.0'],
    ['cube-image', 'v0.6.0'],
    ['cube-agent', 'v0.6.0'],
    ['cube-kernel-scf', 'sha256-b7c91a3e4f12'],
  ] as const;
  const out: NodeInstall[] = [];
  for (const [component, version] of v06) {
    out.push({ nodeId: 'cube-edge-01', arch: 'amd64', component, version });
  }
  out.push({ nodeId: 'cube-edge-02', arch: 'amd64', component: 'cube-shim', version: 'v0.6.0' });
  out.push({ nodeId: 'cube-edge-02', arch: 'amd64', component: 'cube-image', version: 'v0.6.0' });
  return out;
}

const NODES = ['cube-edge-01', 'cube-edge-02', 'cube-edge-03'] as const;
const VERSIONS = ['v0.6.0', 'v0.5.0', 'v0.4.1'] as const;
/** Enough rows for 9 pages at PAGE_SIZE=20 so the pager shows numbers and ellipsis. */
const MOCK_JOB_PAGES = 9;
const MOCK_JOB_PAGE_SIZE = 20;
const MOCK_JOB_COUNT = MOCK_JOB_PAGES * MOCK_JOB_PAGE_SIZE;

function buildPreinstall(): WarehousePreinstallJob[] {
  const showcase: WarehousePreinstallJob[] = [
    {
      id: 'pre-mock-running',
      nodeId: 'cube-edge-02',
      arch: 'amd64',
      component: 'cube-agent',
      version: 'v0.6.0',
      status: 'running',
    },
    {
      id: 'pre-mock-failed',
      nodeId: 'cube-edge-03',
      arch: 'amd64',
      component: 'cube-shim',
      version: 'v0.5.0',
      status: 'failed',
      error: 'download timed out contacting CubeOps',
    },
  ];
  const extra: WarehousePreinstallJob[] = [];
  for (let i = 0; i < MOCK_JOB_COUNT - showcase.length; i++) {
    const failed = i % 9 === 8;
    const cancelled = i % 9 === 7;
    extra.push({
      id: `pre-mock-hist-${i + 1}`,
      nodeId: NODES[i % NODES.length],
      arch: i % 7 === 0 ? 'arm64' : 'amd64',
      component: CATALOG[i % CATALOG.length],
      version: VERSIONS[i % VERSIONS.length],
      status: failed ? 'failed' : cancelled ? 'cancelled' : 'succeeded',
      ...(failed ? { error: 'download timed out contacting CubeOps' } : {}),
    });
  }
  return [...showcase, ...extra];
}

function buildImportJobs(): WarehouseImportJob[] {
  const sources = ['github', 'cnb', 'upload'] as const;
  const jobs: WarehouseImportJob[] = [];
  for (let i = 0; i < MOCK_JOB_COUNT; i++) {
    const source = sources[i % sources.length];
    const failed = i % 17 === 16;
    jobs.push({
      id: `imp-mock-${i + 1}`,
      source,
      sourceRef: source === 'upload' ? `upl-mock-${i + 1}` : 'TencentCloud/CubeSandbox',
      tag: VERSIONS[i % VERSIONS.length],
      arch: i % 5 === 0 ? 'arm64' : 'amd64',
      status: failed ? 'failed' : 'succeeded',
      bytesTotal: failed ? 0 : 564_428_032,
      ...(failed ? { error: 'release asset not found for this tag' } : {}),
    });
  }
  return jobs;
}

let items = buildItems();
let installs = buildInstalls();
let importJobs: WarehouseImportJob[] = buildImportJobs();
const importPolls = new Map<string, number>();
let preinstallJobs = buildPreinstall();
const uploads = new Set<string>();

export function resetWarehouseState() {
  items = buildItems();
  installs = buildInstalls();
  importJobs = buildImportJobs();
  importPolls.clear();
  preinstallJobs = buildPreinstall();
  uploads.clear();
}

function keyOf(arch: string, component: string, version: string) {
  return `${arch}|${component}|${version}`;
}

function upsertItem(next: WarehouseItem) {
  const i = items.findIndex(
    (row) =>
      row.arch === next.arch && row.component === next.component && row.version === next.version,
  );
  if (i >= 0) {
    items[i] = { ...items[i], ...next, updatedAt: new Date().toISOString() };
    return;
  }
  items.push(next);
}

function markInstalled(nodeId: string, arch: string, component: string, version: string) {
  if (
    installs.some(
      (row) =>
        row.nodeId === nodeId &&
        row.arch === arch &&
        row.component === component &&
        row.version === version,
    )
  ) {
    return;
  }
  installs.push({ nodeId, arch, component, version });
}

function installIndex(): Map<string, Set<string>> {
  const have = new Map<string, Set<string>>();
  for (const inst of installs) {
    const k = keyOf(inst.arch, inst.component, inst.version);
    if (!have.has(k)) have.set(k, new Set());
    have.get(k)!.add(inst.nodeId);
  }
  return have;
}

function splitCoverage(
  arch: string,
  component: string,
  version: string,
  have: Map<string, Set<string>>,
  nodeIds: string[],
) {
  const installed = have.get(keyOf(arch, component, version)) ?? new Set<string>();
  const nodesInstalled: string[] = [];
  const nodesMissing: string[] = [];
  const seen = new Set<string>();
  for (const n of nodeIds) {
    seen.add(n);
    if (installed.has(n)) nodesInstalled.push(n);
    else nodesMissing.push(n);
  }
  for (const n of installed) {
    if (!seen.has(n)) nodesInstalled.push(n);
  }
  return { nodesInstalled, nodesMissing };
}

function nodesMissingAny(
  component: string,
  group: WarehouseItem[],
  have: Map<string, Set<string>>,
  nodeIds: string[],
): number {
  if (group.length === 0) return 0;
  let count = 0;
  for (const n of nodeIds) {
    for (const item of group) {
      if (item.component !== component) continue;
      const set = have.get(keyOf(item.arch, item.component, item.version));
      if (!set || !set.has(n)) {
        count++;
        break;
      }
    }
  }
  return count;
}

export function listWarehouseComponents(nodeIds: string[]): {
  components: WarehouseComponentSummary[];
} {
  tickPreinstallJobs();
  const have = installIndex();
  const byComp = new Map<string, WarehouseItem[]>();
  for (const item of items) {
    const cur = byComp.get(item.component) ?? [];
    cur.push(item);
    byComp.set(item.component, cur);
  }
  const components: WarehouseComponentSummary[] = CATALOG.map((name) => {
    const group = byComp.get(name) ?? [];
    const versions = new Set<string>();
    const arches = new Set<string>();
    let sizeBytes = 0;
    for (const item of group) {
      versions.add(item.version);
      arches.add(item.arch);
      sizeBytes += item.sizeBytes;
    }
    return {
      name,
      versionCount: versions.size,
      arches: [...arches].sort(),
      sizeBytes,
      nodesMissing: nodesMissingAny(name, group, have, nodeIds),
    };
  });
  return { components };
}

export function getWarehouseComponent(
  name: string,
  nodeIds: string[],
): { ok: true; detail: WarehouseComponentDetail } | { ok: false; status: number; error: string } {
  if (!CATALOG.includes(name as (typeof CATALOG)[number])) {
    return { ok: false, status: 400, error: `unsupported component ${JSON.stringify(name)}` };
  }
  tickPreinstallJobs();
  const have = installIndex();
  const order: string[] = [];
  const seen = new Map<string, WarehouseItem[]>();
  for (const item of items) {
    if (item.component !== name) continue;
    if (!seen.has(item.version)) {
      order.push(item.version);
      seen.set(item.version, []);
    }
    seen.get(item.version)!.push(item);
  }
  const versions: WarehouseVersionGroup[] = order.map((version) => {
    const arts = [...(seen.get(version) ?? [])].sort((a, b) => a.arch.localeCompare(b.arch));
    return {
      version,
      artifacts: arts.map((item) => {
        const cov = splitCoverage(item.arch, item.component, item.version, have, nodeIds);
        return {
          arch: item.arch,
          sizeBytes: item.sizeBytes,
          source: item.source,
          sourceRef: item.sourceRef,
          checksum: item.checksum,
          createdAt: item.createdAt,
          nodesInstalled: cov.nodesInstalled,
          nodesMissing: cov.nodesMissing,
        };
      }),
    };
  });
  return { ok: true, detail: { name, versions } };
}

export function deleteWarehouseVersion(
  component: string,
  version: string,
  arch: string,
): 'ok' | 'not_found' {
  const before = items.length;
  items = items.filter(
    (row) => !(row.arch === arch && row.component === component && row.version === version),
  );
  if (items.length === before) {
    return 'not_found';
  }
  preinstallJobs = preinstallJobs.map((job) => {
    if (
      job.arch === arch &&
      job.component === component &&
      job.version === version &&
      (job.status === 'pending' || job.status === 'running')
    ) {
      return { ...job, status: 'cancelled' };
    }
    return job;
  });
  return 'ok';
}

export function createUpload(
  filename: string,
): { ok: true; uploadId: string; filename: string } | { ok: false; error: string } {
  const lower = filename.toLowerCase();
  if (!lower.endsWith('.tar.gz') && !lower.endsWith('.tgz')) {
    return { ok: false, error: 'upload must be a .tar.gz one-click package' };
  }
  const uploadId = nid('upload');
  uploads.add(uploadId);
  return { ok: true, uploadId, filename };
}

export function createImportJobs(body: {
  source?: string;
  repo?: string;
  tag?: string;
  uploadId?: string;
  arch?: string[];
}): { ok: true; jobs: WarehouseImportJob[] } | { ok: false; status: number; error: string } {
  const source = (body.source ?? '').trim().toLowerCase();
  const arches = (body.arch ?? []).map((a) => a.trim().toLowerCase()).filter(Boolean);
  if (arches.length === 0) {
    return { ok: false, status: 400, error: 'arch is required' };
  }
  for (const a of arches) {
    if (a !== 'amd64' && a !== 'arm64' && a !== 'x86_64' && a !== 'aarch64') {
      return {
        ok: false,
        status: 400,
        error: `unsupported arch ${JSON.stringify(a)} (want amd64 or arm64)`,
      };
    }
  }
  const normArch = (a: string) => (a === 'x86_64' ? 'amd64' : a === 'aarch64' ? 'arm64' : a);
  const jobs: WarehouseImportJob[] = [];
  for (const raw of arches) {
    const arch = normArch(raw);
    const job: WarehouseImportJob = {
      id: nid('imp'),
      source,
      sourceRef: '',
      tag: (body.tag ?? '').trim(),
      arch,
      status: 'pending',
      bytesTotal: 0,
    };
    if (source === 'upload') {
      const uploadId = (body.uploadId ?? '').trim();
      if (!uploadId) return { ok: false, status: 400, error: 'uploadId is required' };
      if (!uploads.has(uploadId)) return { ok: false, status: 400, error: 'upload not found' };
      job.sourceRef = uploadId;
    } else if (source === 'github' || source === 'cnb') {
      if (!(body.repo ?? '').trim() || !(body.tag ?? '').trim()) {
        return { ok: false, status: 400, error: 'repo and tag are required' };
      }
      job.sourceRef = body.repo!.trim();
    } else {
      return { ok: false, status: 400, error: 'source must be github, cnb, or upload' };
    }
    importJobs.push(job);
    importPolls.set(job.id, 0);
    jobs.push(job);
  }
  return { ok: true, jobs: clone(jobs) };
}

function extractedFor(job: WarehouseImportJob): WarehouseItem[] {
  const tag = job.tag || 'v0.6.0';
  const source = job.source || 'github';
  const now = 0;
  return [
    item(job.arch, 'cube-shim', tag, source, 82_345_216, now),
    item(job.arch, 'cube-image', tag, source, 412_221_440, now),
    item(job.arch, 'cube-agent', tag, source, 8_388_608, now),
    item(job.arch, 'cube-kernel-scf', 'sha256-b7c91a3e4f12', source, 61_472_768, now),
  ];
}

export function getImportJob(id: string): WarehouseImportJob | undefined {
  const job = importJobs.find((row) => row.id === id);
  if (!job) return undefined;
  tickImportJob(job);
  return clone(job);
}

export function listImportJobs(opts: { limit?: string | null; offset?: string | null } = {}): {
  jobs: WarehouseImportJob[];
  total: number;
} {
  for (const job of importJobs) {
    tickImportJob(job);
  }
  return paginate(clone(importJobs).reverse(), opts.limit, opts.offset);
}

function tickImportJob(job: WarehouseImportJob) {
  const polls = (importPolls.get(job.id) ?? 0) + 1;
  importPolls.set(job.id, polls);
  if (job.status === 'pending' || job.status === 'running') {
    if (job.tag.trim().toLowerCase() === 'fail' && polls >= 2) {
      job.status = 'failed';
      job.error = 'release asset not found for this tag';
    } else if (polls === 1) {
      job.status = 'running';
    } else if (polls >= 2) {
      job.status = 'succeeded';
      job.bytesTotal = 564_428_032;
      for (const next of extractedFor(job)) {
        upsertItem(next);
      }
    }
  }
}

export function createPreinstallJobs(body: {
  nodeIds?: string[];
  arch?: string;
  component?: string;
  version?: string;
}):
  | { ok: true; jobs: WarehousePreinstallJob[] }
  | { ok: false; status: number; error: string; code?: string } {
  const arch = (body.arch ?? '').trim();
  const component = (body.component ?? '').trim();
  const version = (body.version ?? '').trim();
  if (!arch) return { ok: false, status: 400, error: 'arch query is required (amd64 or arm64)' };
  if (!component || !version)
    return { ok: false, status: 400, error: 'component and version are required' };
  const found = items.find(
    (row) => row.arch === arch && row.component === component && row.version === version,
  );
  if (!found) {
    return {
      ok: false,
      status: 404,
      error: 'warehouse version not found',
      code: 'warehouse_not_found',
    };
  }
  const nodeIds = (body.nodeIds ?? []).map((n) => n.trim()).filter(Boolean);
  if (nodeIds.length === 0) return { ok: false, status: 400, error: 'nodeIds is required' };
  const jobs: WarehousePreinstallJob[] = nodeIds.map((nodeId) => ({
    id: nid('pre'),
    nodeId,
    arch,
    component,
    version,
    status: 'running',
  }));
  preinstallJobs.push(...jobs);
  return { ok: true, jobs: clone(jobs) };
}

function tickPreinstallJobs() {
  for (const job of preinstallJobs) {
    if (job.status === 'pending') {
      job.status = 'running';
    } else if (job.status === 'running') {
      job.status = 'succeeded';
      markInstalled(job.nodeId, job.arch, job.component, job.version);
    }
  }
}

export function listPreinstallJobs(
  filters: {
    node_id?: string | null;
    status?: string | null;
    limit?: string | null;
    offset?: string | null;
  } = {},
) {
  tickPreinstallJobs();
  const jobs = preinstallJobs.filter((job) => {
    if (filters.node_id && job.nodeId !== filters.node_id) return false;
    if (filters.status && job.status !== filters.status) return false;
    return true;
  });
  return paginate(clone(jobs), filters.limit, filters.offset);
}

function paginate<T>(
  items: T[],
  rawLimit?: string | null,
  rawOffset?: string | null,
): { jobs: T[]; total: number } {
  const total = items.length;
  let offset = Number(rawOffset);
  if (!Number.isFinite(offset) || offset < 0) {
    offset = 0;
  }
  let limit = Number(rawLimit);
  if (!Number.isFinite(limit) || limit <= 0) {
    limit = 50;
  }
  if (limit > 200) {
    limit = 200;
  }
  return { jobs: items.slice(offset, offset + limit), total };
}
