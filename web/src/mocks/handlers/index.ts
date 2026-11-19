// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { http, HttpResponse } from 'msw';
import {
  createSandbox,
  clearNodeMachineType,
  createMachineType,
  deleteMachineType,
  deleteNodeLabel,
  deleteSandbox,
  getClusterOverview,
  getNode,
  getVersionMatrix,
  getSandboxDetail,
  getSandboxLogs,
  getTemplate,
  getTemplateCompat,
  listMachineTypes,
  listNodes,
  listSandboxes,
  listTemplates,
  mockDelay,
  patchNodeLabels,
  pauseSandbox,
  replaceMachineTypeNodes,
  replaceNodeLabels,
  resetMockState,
  resumeSandbox,
  setNodeMachineType,
} from '../fixtures';
import {
  createImportJobs,
  createPreinstallJobs,
  createUpload,
  deleteWarehouseVersion,
  getImportJob,
  listImportJobs,
  getWarehouseComponent,
  listPreinstallJobs,
  listWarehouseComponents,
} from '../fixtures/warehouse';

function notFound(message: string) {
  return HttpResponse.json({ code: 404, message }, { status: 404 });
}

function opsError(status: number, error: string, code?: string) {
  return HttpResponse.json(code ? { error, code } : { error }, { status });
}

export const handlers = [
  http.get('/cubeapi/v1/health', async () => {
    await mockDelay();
    return HttpResponse.json({ status: 'ok' });
  }),

  http.get('/cubeapi/v1/cluster/overview', async () => {
    await mockDelay();
    return HttpResponse.json(getClusterOverview());
  }),

  http.get('/cubeapi/v1/cluster/versions', async () => {
    await mockDelay();
    return HttpResponse.json(getVersionMatrix());
  }),

  // Node inventory and labels are CubeOps routes (/opsapi/v1/...).
  http.get('/opsapi/v1/nodes', async () => {
    await mockDelay();
    return HttpResponse.json(listNodes());
  }),

  http.get('/opsapi/v1/nodes/:nodeID', async ({ params }) => {
    await mockDelay();
    const node = getNode(String(params.nodeID));
    return node ? HttpResponse.json(node) : notFound(`node ${params.nodeID} not found`);
  }),

  // PATCH merges, PUT replaces, DELETE removes one key (?key=, because keys
  // contain '/').
  http.patch('/opsapi/v1/nodes/:nodeID/labels', async ({ params, request }) => {
    await mockDelay();
    const body = (await request.json()) as { labels?: Record<string, string> };
    const result = patchNodeLabels(String(params.nodeID), body.labels ?? {});
    return result.ok
      ? new HttpResponse(null, { status: 204 })
      : opsError(result.status, result.error);
  }),

  http.put('/opsapi/v1/nodes/:nodeID/labels', async ({ params, request }) => {
    await mockDelay();
    const body = (await request.json().catch(() => null)) as {
      labels?: Record<string, string>;
    } | null;
    if (!body || body.labels == null) {
      return opsError(400, 'labels must not be empty; send {} to clear user labels');
    }
    const result = replaceNodeLabels(String(params.nodeID), body.labels);
    return result.ok
      ? new HttpResponse(null, { status: 204 })
      : opsError(result.status, result.error);
  }),

  http.get('/opsapi/v1/machine-types', async () => {
    await mockDelay();
    return HttpResponse.json(listMachineTypes());
  }),

  http.post('/opsapi/v1/machine-types', async ({ request }) => {
    await mockDelay();
    const body = (await request.json()) as {
      name?: string;
      displayName?: string;
      description?: string;
      nodeIDs?: string[];
    };
    const result = createMachineType(body);
    return result.ok
      ? HttpResponse.json(result.view, { status: 201 })
      : opsError(result.status, result.error);
  }),

  http.put('/opsapi/v1/machine-types/:name/nodes', async ({ params, request }) => {
    await mockDelay();
    const body = (await request.json()) as { nodeIDs?: string[] };
    if (!body || body.nodeIDs == null) return opsError(400, 'nodeIDs is required');
    const result = replaceMachineTypeNodes(String(params.name), body.nodeIDs);
    return result.ok ? HttpResponse.json(result.result) : opsError(result.status, result.error);
  }),

  http.delete('/opsapi/v1/machine-types/:name', async ({ params, request }) => {
    await mockDelay();
    const unbind = new URL(request.url).searchParams.get('unbind') === 'true';
    const result = deleteMachineType(String(params.name), unbind);
    return result.ok
      ? new HttpResponse(null, { status: 204 })
      : opsError(result.status, result.error);
  }),

  http.put('/opsapi/v1/nodes/:nodeID/machine-type', async ({ params, request }) => {
    await mockDelay();
    const body = (await request.json()) as { machineType?: string };
    const result = setNodeMachineType(String(params.nodeID), body.machineType ?? '');
    return result.ok
      ? new HttpResponse(null, { status: 204 })
      : opsError(result.status, result.error);
  }),

  http.delete('/opsapi/v1/nodes/:nodeID/machine-type', async ({ params }) => {
    await mockDelay();
    const result = clearNodeMachineType(String(params.nodeID));
    return result.ok
      ? new HttpResponse(null, { status: 204 })
      : opsError(result.status, result.error);
  }),

  http.delete('/opsapi/v1/nodes/:nodeID/labels', async ({ params, request }) => {
    await mockDelay();
    const key = new URL(request.url).searchParams.get('key') ?? '';
    if (!key) return opsError(400, 'query parameter key is required');
    const result = deleteNodeLabel(String(params.nodeID), key);
    return result.ok
      ? new HttpResponse(null, { status: 204 })
      : opsError(result.status, result.error);
  }),

  http.get('/cubeapi/v1/templates', async () => {
    await mockDelay();
    return HttpResponse.json(listTemplates());
  }),

  http.get('/cubeapi/v1/templates/compat', async () => {
    await mockDelay();
    return HttpResponse.json(getTemplateCompat());
  }),

  http.post('/cubeapi/v1/templates/compat/:templateID/adopt-baseline', async () => {
    await mockDelay();
    return HttpResponse.json({ updated: 1 });
  }),

  http.get('/cubeapi/v1/templates/:templateID', async ({ params }) => {
    await mockDelay();
    const template = getTemplate(String(params.templateID));
    return template
      ? HttpResponse.json(template)
      : notFound(`template ${params.templateID} not found`);
  }),

  http.get('/cubeapi/v1/v2/sandboxes', async ({ request }) => {
    await mockDelay();
    const url = new URL(request.url);
    return HttpResponse.json(
      listSandboxes({
        state: url.searchParams.get('state'),
        metadata: url.searchParams.get('metadata'),
      }),
    );
  }),

  http.get('/cubeapi/v1/sandboxes/:sandboxID', async ({ params }) => {
    await mockDelay();
    const sandbox = getSandboxDetail(String(params.sandboxID));
    return sandbox ? HttpResponse.json(sandbox) : notFound(`sandbox ${params.sandboxID} not found`);
  }),

  http.delete('/cubeapi/v1/sandboxes/:sandboxID', async ({ params }) => {
    await mockDelay();
    return deleteSandbox(String(params.sandboxID))
      ? new HttpResponse(null, { status: 204 })
      : notFound(`sandbox ${params.sandboxID} not found`);
  }),

  http.post('/cubeapi/v1/sandboxes/:sandboxID/pause', async ({ params }) => {
    await mockDelay();
    return pauseSandbox(String(params.sandboxID))
      ? new HttpResponse(null, { status: 204 })
      : notFound(`sandbox ${params.sandboxID} not found`);
  }),

  http.post('/cubeapi/v1/sandboxes/:sandboxID/resume', async ({ params }) => {
    await mockDelay();
    const sandbox = resumeSandbox(String(params.sandboxID));
    return sandbox
      ? HttpResponse.json(sandbox, { status: 201 })
      : notFound(`sandbox ${params.sandboxID} not found`);
  }),

  http.get('/cubeapi/v1/v2/sandboxes/:sandboxID/logs', async ({ params }) => {
    await mockDelay();
    const logs = getSandboxLogs(String(params.sandboxID));
    return logs ? HttpResponse.json(logs) : notFound(`sandbox ${params.sandboxID} not found`);
  }),

  http.post('/cubeapi/v1/sandboxes', async ({ request }) => {
    await mockDelay();
    const body = (await request.json()) as {
      templateID: string;
      timeout?: number;
      alias?: string;
      autoPause?: boolean;
      metadata?: Record<string, string>;
    };
    if (!body.templateID) {
      return HttpResponse.json({ code: 400, message: 'templateID is required' }, { status: 400 });
    }
    const sandbox = createSandbox(body);
    return HttpResponse.json(sandbox, { status: 201 });
  }),

  http.post('/mock/reset', async () => {
    resetMockState();
    return HttpResponse.json({ ok: true });
  }),

  // Mock mode has no CubeOps: keep the shell open so warehouse and other
  // ops-backed pages can be exercised locally.
  http.get('/opsapi/v1/auth/session', async () => {
    await mockDelay();
    return HttpResponse.json({ authRequired: false, authenticated: true, username: 'mock' });
  }),
  http.post('/opsapi/v1/auth/login', async () => {
    await mockDelay();
    return HttpResponse.json({
      accessToken: 'mock-access',
      refreshToken: 'mock-refresh',
      username: 'mock',
      expiresInSecs: 3600,
    });
  }),
  http.post('/opsapi/v1/auth/logout', async () => {
    await mockDelay();
    return new HttpResponse(null, { status: 204 });
  }),
  http.post('/opsapi/v1/auth/refresh', async () => {
    await mockDelay();
    return HttpResponse.json({ accessToken: 'mock-access', refreshToken: 'mock-refresh' });
  }),

  http.get('/opsapi/v1/warehouse/components', async () => {
    await mockDelay();
    const nodeIds = listNodes()
      .map((n) => n.nodeID)
      .filter(Boolean);
    return HttpResponse.json(listWarehouseComponents(nodeIds));
  }),

  http.get('/opsapi/v1/warehouse/components/:component', async ({ params }) => {
    await mockDelay();
    const nodeIds = listNodes()
      .map((n) => n.nodeID)
      .filter(Boolean);
    const result = getWarehouseComponent(String(params.component), nodeIds);
    if (!result.ok) {
      return opsError(result.status, result.error);
    }
    return HttpResponse.json(result.detail);
  }),

  http.delete(
    '/opsapi/v1/warehouse/components/:component/versions/:version',
    async ({ request, params }) => {
      await mockDelay();
      const url = new URL(request.url);
      const arch = url.searchParams.get('arch') ?? '';
      if (!arch) {
        return opsError(400, 'arch query is required (amd64 or arm64)');
      }
      const result = deleteWarehouseVersion(String(params.component), String(params.version), arch);
      if (result === 'not_found') {
        return opsError(404, 'warehouse version not found', 'warehouse_not_found');
      }
      return new HttpResponse(null, { status: 204 });
    },
  ),

  http.post('/opsapi/v1/warehouse/uploads', async ({ request }) => {
    await mockDelay();
    const form = await request.formData();
    const file = form.get('file');
    if (!(file instanceof File)) {
      return opsError(400, 'multipart field file is required');
    }
    const result = createUpload(file.name);
    if (!result.ok) {
      return opsError(400, result.error);
    }
    return HttpResponse.json(
      { uploadId: result.uploadId, filename: result.filename },
      { status: 201 },
    );
  }),

  http.post('/opsapi/v1/warehouse/imports', async ({ request }) => {
    await mockDelay();
    let body: {
      source?: string;
      repo?: string;
      tag?: string;
      uploadId?: string;
      arch?: string[];
    };
    try {
      body = (await request.json()) as typeof body;
    } catch {
      return opsError(400, 'invalid JSON body');
    }
    const result = createImportJobs(body);
    if (!result.ok) {
      return opsError(result.status, result.error);
    }
    return HttpResponse.json({ jobs: result.jobs }, { status: 202 });
  }),

  http.get('/opsapi/v1/warehouse/imports', async ({ request }) => {
    await mockDelay();
    const url = new URL(request.url);
    return HttpResponse.json(
      listImportJobs({
        limit: url.searchParams.get('limit'),
        offset: url.searchParams.get('offset'),
      }),
    );
  }),

  http.get('/opsapi/v1/warehouse/imports/:id', async ({ params }) => {
    await mockDelay();
    const job = getImportJob(String(params.id));
    if (!job) {
      return opsError(404, 'import job not found');
    }
    return HttpResponse.json(job);
  }),

  http.get('/opsapi/v1/warehouse/preinstall', async ({ request }) => {
    await mockDelay();
    const url = new URL(request.url);
    return HttpResponse.json(
      listPreinstallJobs({
        node_id: url.searchParams.get('node_id'),
        status: url.searchParams.get('status'),
        limit: url.searchParams.get('limit'),
        offset: url.searchParams.get('offset'),
      }),
    );
  }),

  http.post('/opsapi/v1/warehouse/preinstall', async ({ request }) => {
    await mockDelay();
    let body: { nodeIds?: string[]; arch?: string; component?: string; version?: string };
    try {
      body = (await request.json()) as typeof body;
    } catch {
      return opsError(400, 'invalid JSON body');
    }
    const result = createPreinstallJobs(body);
    if (!result.ok) {
      return opsError(result.status, result.error, result.code);
    }
    return HttpResponse.json({ jobs: result.jobs }, { status: 202 });
  }),
];
