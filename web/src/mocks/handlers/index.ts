// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { http, HttpResponse } from 'msw';
import {
  deleteSandbox,
  getClusterOverview,
  getNode,
  getSandboxDetail,
  getSandboxLogs,
  getSandboxSession,
  getTemplate,
  listNodes,
  listSandboxes,
  listTemplates,
  mockDelay,
  pauseSandbox,
  resetMockState,
  resumeSandbox,
} from '../fixtures';

function notFound(message: string) {
  return HttpResponse.json({ code: 404, message }, { status: 404 });
}

export const handlers = [
  http.get('/cubeapi/v1/health', async () => {
    await mockDelay();
    return HttpResponse.json({ status: 'ok', sandboxes: listSandboxes().length });
  }),

  http.get('/cubeapi/v1/cluster/overview', async () => {
    await mockDelay();
    return HttpResponse.json(getClusterOverview());
  }),

  http.get('/cubeapi/v1/nodes', async () => {
    await mockDelay();
    return HttpResponse.json(listNodes());
  }),

  http.get('/cubeapi/v1/nodes/:nodeID', async ({ params }) => {
    await mockDelay();
    const node = getNode(String(params.nodeID));
    return node ? HttpResponse.json(node) : notFound(`node ${params.nodeID} not found`);
  }),

  http.get('/cubeapi/v1/templates', async () => {
    await mockDelay();
    return HttpResponse.json(listTemplates());
  }),

  http.get('/cubeapi/v1/templates/:templateID', async ({ params }) => {
    await mockDelay();
    const template = getTemplate(String(params.templateID));
    return template ? HttpResponse.json(template) : notFound(`template ${params.templateID} not found`);
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

  http.post('/mock/reset', async () => {
    resetMockState();
    return HttpResponse.json({ ok: true });
  }),
];
