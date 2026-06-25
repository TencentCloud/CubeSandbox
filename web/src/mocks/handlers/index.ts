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

export const handlers = [
  http.get('/cubeapi/v1/health', async () => {
    await mockDelay();
    return HttpResponse.json({ status: 'ok', sandboxes: listSandboxes().length });
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

  http.post('/cubeapi/v1/sandboxes', async ({ request }) => {
    await mockDelay();
    const body = await request.json() as {
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

  // ── SandboxCases mock endpoints ─────────────────────────────────────
  // The real backend returns a richer payload (steps + topology); the mock
  // mirrors the shape so the UI renders correctly under MSW without
  // spawning a subprocess.
  http.get('/cubeapi/v1/examples', async () => {
    await mockDelay();
    return HttpResponse.json([
      { id: 'code-sandbox-quickstart:create', scenario: 'code-sandbox-quickstart',
        filename: 'create.py', title: 'Create Sandbox',
        description: 'Create a sandbox from a template and read its metadata.',
        category: 'basics', language: 'python' },
      { id: 'code-sandbox-quickstart:exec_code', scenario: 'code-sandbox-quickstart',
        filename: 'exec_code.py', title: 'Execute Code',
        description: 'Run Python code inside the sandbox through the Jupyter kernel.',
        category: 'basics', language: 'python' },
      { id: 'code-sandbox-quickstart:cmd', scenario: 'code-sandbox-quickstart',
        filename: 'cmd.py', title: 'Run Shell Command',
        description: 'Execute a shell command inside the sandbox.',
        category: 'basics', language: 'python' },
      { id: 'snapshot-rollback-clone:01_create_snapshot', scenario: 'snapshot-rollback-clone',
        filename: '01_create_snapshot.py', title: '01 Create Snapshot',
        description: 'Capture a snapshot from a running sandbox.',
        category: 'lifecycle', language: 'python' },
      { id: 'snapshot-rollback-clone:09_rollback', scenario: 'snapshot-rollback-clone',
        filename: '09_rollback.py', title: '09 Rollback',
        description: 'Roll the sandbox back to a previous snapshot.',
        category: 'lifecycle', language: 'python' },
      { id: 'network-policy:network_allowlist', scenario: 'network-policy',
        filename: 'network_allowlist.py', title: 'Network Allowlist',
        description: 'Restrict egress to an explicit list of IPs.',
        category: 'network', language: 'python' },
      { id: 'host-mount:create_with_mount', scenario: 'host-mount',
        filename: 'create_with_mount.py', title: 'Create With Mount',
        description: 'Create a sandbox with a host directory mounted at /mnt.',
        category: 'filesystem', language: 'python' },
      { id: 'browser-sandbox:browser', scenario: 'browser-sandbox',
        filename: 'browser.py', title: 'Playwright + Chromium',
        description: 'Boot a sandbox with Chromium and run a Playwright script.',
        category: 'browser', language: 'python' },
    ]);
  }),

  http.get('/cubeapi/v1/examples/:scenario/:file', async ({ params }) => {
    await mockDelay();
    const scenario = String(params.scenario);
    const file = String(params.file);
    const id = `${scenario}:${file}`;
    // Tiny embedded source so the mock has a runnable preview.
    const stub =
      `# ${id}\n` +
      `# mock source — backend would read this from examples/<scenario>/<filename>\n` +
      `print("hello from ${id}")\n`;
    return HttpResponse.json({
      id,
      filename: `${file}.py`,
      scenario,
      language: 'python',
      source: stub,
    });
  }),

  http.post('/cubeapi/v1/examples/run', async ({ request }) => {
    await mockDelay();
    const body = (await request.json().catch(() => ({}))) as {
      id?: string;
      template_id?: string;
      code?: string;
    };
    const id = body.id ?? 'unknown';
    return HttpResponse.json({
      stdout: `[mock] ran ${id} with template=${body.template_id ?? '<default>'}\n${body.code ? 'code length=' + body.code.length : 'on-disk source'}`,
      stderr: '',
      exit_code: 0,
      success: true,
      elapsed_ms: 820,
      ran_edited: !!body.code,
      topology: {
        nodes: [
          { id: 'user', label: 'User Script', plane: 'control', kind: 'user', description: 'mock user' },
          { id: 'cubeapi', label: 'CubeAPI :3000', plane: 'control', kind: 'control', description: 'HTTP gateway' },
          { id: 'cubemaster', label: 'CubeMaster', plane: 'control', kind: 'control', description: 'Scheduler' },
          { id: 'cubelet', label: 'Cubelet', plane: 'control', kind: 'control', description: 'Per-node agent' },
          { id: 'cubeproxy', label: 'CubeProxy', plane: 'data', kind: 'control', description: 'TLS reverse proxy' },
          { id: 'microvm', label: 'KVM MicroVM', plane: 'data', kind: 'vm', description: 'Sandbox boundary' },
          { id: 'envd', label: 'envd :49983', plane: 'data', kind: 'data', description: 'In-sandbox daemon' },
          { id: 'runner', label: 'Python / Shell', plane: 'data', kind: 'data', description: 'Interpreter' },
        ],
        edges: [
          { from: 'user', to: 'cubeapi', label: 'HTTPS', plane: 'control' },
          { from: 'cubeapi', to: 'cubemaster', label: 'gRPC', plane: 'control' },
          { from: 'cubemaster', to: 'cubelet', label: 'gRPC', plane: 'control' },
          { from: 'cubelet', to: 'microvm', label: 'QMP / boot', plane: 'control' },
          { from: 'cubeapi', to: 'cubeproxy', label: 'HTTPS', plane: 'data' },
          { from: 'cubeproxy', to: 'envd', label: 'WSS tunnel', plane: 'data' },
          { from: 'envd', to: 'runner', label: 'fork+exec', plane: 'data' },
        ],
      },
    });
  }),
];
