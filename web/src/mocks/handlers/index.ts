// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

import { http, HttpResponse } from 'msw';
import {
  createSandbox,
  deleteSandbox,
  getClusterOverview,
  getNode,
  getVersionMatrix,
  getSandboxDetail,
  getSandboxLogs,
  getSandboxSession,
  getTemplate,
  getTemplateCompat,
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

const MOCK_SESSION_TOKEN = 'mock-cube-session-token';

export const handlers = [
  http.get('/cubeapi/v1/health', async () => {
    await mockDelay();
    return HttpResponse.json({ status: 'ok', sandboxes: listSandboxes().length });
  }),

  http.get('/cubeapi/v1/auth/session', async ({ request }) => {
    await mockDelay();
    const token = request.headers.get('x-session-token');
    return HttpResponse.json({
      authRequired: true,
      authenticated: token === MOCK_SESSION_TOKEN,
      username: token === MOCK_SESSION_TOKEN ? 'admin' : undefined,
    });
  }),

  http.post('/cubeapi/v1/auth/login', async ({ request }) => {
    await mockDelay();
    const body = await request.json().catch(() => ({})) as { username?: string; password?: string };
    if (body.username !== 'admin' || body.password !== 'admin') {
      return HttpResponse.json({ code: 401, message: 'invalid username or password' }, { status: 401 });
    }
    return HttpResponse.json({
      token: MOCK_SESSION_TOKEN,
      username: 'admin',
      expiresInSecs: 86_400,
    });
  }),

  http.post('/cubeapi/v1/auth/logout', async () => {
    await mockDelay();
    return new HttpResponse(null, { status: 204 });
  }),

  http.post('/cubeapi/v1/auth/change-password', async () => {
    await mockDelay();
    return new HttpResponse(null, { status: 204 });
  }),

  http.get('/cubeapi/v1/cluster/overview', async () => {
    await mockDelay();
    return HttpResponse.json(getClusterOverview());
  }),

  http.get('/cubeapi/v1/cluster/versions', async () => {
    await mockDelay();
    return HttpResponse.json(getVersionMatrix());
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

  http.post('/opsapi/v1/terminal/sandboxes/:sandboxID/tickets', async ({ params, request }) => {
    await mockDelay();
    const sandboxID = String(params.sandboxID);
    const sandbox = getSandboxDetail(sandboxID);
    if (!sandbox) return notFound(`sandbox ${sandboxID} not found`);
    const body = await request.json().catch(() => ({})) as { containerID?: string };
    return HttpResponse.json(
      {
        ticket: `mock-${sandboxID}`,
        expiresAt: new Date(Date.now() + 60_000).toISOString(),
        websocketUrl: `/opsapi/v1/terminal/sandboxes/${sandboxID}/ws`,
        containerID: body.containerID,
      },
      { status: 201 },
    );
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
];
