// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

/**
 * Exercises the mock's label HTTP handlers by invoking their resolvers
 * directly: request parsing, status codes and error shapes.
 */

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { handlers } from './handlers/index.ts';
import { resetMockState } from './fixtures/index.ts';

const MT = 'cube.cubesandbox.io%2Fmachine-type';

type Resolver = (info: {
  params: Record<string, string>;
  request: Request;
}) => Promise<Response> | Response;

function handlerFor(method: string, path: string): Resolver {
  for (const h of handlers as any[]) {
    if (h.info?.method === method && h.info?.path === path) return h.resolver;
  }
  throw new Error(`no handler for ${method} ${path}`);
}

const request = (method: string, body?: unknown, query = '') =>
  new Request(`http://localhost/opsapi/v1/labels${query}`, {
    method,
    headers: body ? { 'content-type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });

test('GET nodes returns labels for the UI', async () => {
  resetMockState();
  const res = await handlerFor('GET', '/opsapi/v1/nodes')({ params: {}, request: request('GET') });
  assert.equal(res.status, 200);
  const nodes = await res.json();
  assert.ok(nodes.some((n: any) => n.labels?.[decodeURIComponent(MT)]));
});

test('PATCH merges and keeps data-plane labels', async () => {
  resetMockState();
  const res = await handlerFor(
    'PATCH',
    '/opsapi/v1/nodes/:nodeID/labels',
  )({
    params: { nodeID: 'cube-edge-01' },
    request: request('PATCH', { labels: { team: 'merged' } }),
  });
  assert.equal(res.status, 204);
});

test('PUT rejects machine-type in the payload with 403', async () => {
  const res = await handlerFor(
    'PUT',
    '/opsapi/v1/nodes/:nodeID/labels',
  )({
    params: { nodeID: 'cube-edge-01' },
    request: request('PUT', { labels: { [decodeURIComponent(MT)]: 'amd' } }),
  });
  assert.equal(res.status, 403);
  assert.match((await res.json()).error, /machine-type|reserved/);
});

test('PUT without labels body is 400, not a silent clear', async () => {
  const res = await handlerFor(
    'PUT',
    '/opsapi/v1/nodes/:nodeID/labels',
  )({
    params: { nodeID: 'cube-edge-01' },
    request: request('PUT', {}),
  });
  assert.equal(res.status, 400);
});

test('DELETE machine-type through the label API is 403', async () => {
  resetMockState();
  const res = await handlerFor(
    'DELETE',
    '/opsapi/v1/nodes/:nodeID/labels',
  )({
    params: { nodeID: 'cube-edge-01' },
    request: request('DELETE', undefined, `?key=${MT}`),
  });
  assert.equal(res.status, 403);
  const detail = await handlerFor(
    'GET',
    '/opsapi/v1/nodes/:nodeID',
  )({
    params: { nodeID: 'cube-edge-01' },
    request: request('GET'),
  });
  const node = await detail.json();
  assert.equal(node.labels[decodeURIComponent(MT)], 'intel-e5-2680');
});

test('DELETE reads a slash key from ?key=', async () => {
  resetMockState();
  const key = 'example.com/team';
  const patched = await handlerFor(
    'PATCH',
    '/opsapi/v1/nodes/:nodeID/labels',
  )({
    params: { nodeID: 'cube-edge-01' },
    request: request('PATCH', { labels: { [key]: 'infra' } }),
  });
  assert.equal(patched.status, 204);
  const res = await handlerFor(
    'DELETE',
    '/opsapi/v1/nodes/:nodeID/labels',
  )({
    params: { nodeID: 'cube-edge-01' },
    request: request('DELETE', undefined, `?key=${encodeURIComponent(key)}`),
  });
  assert.equal(res.status, 204);
});

test('DELETE without key is 400', async () => {
  const res = await handlerFor(
    'DELETE',
    '/opsapi/v1/nodes/:nodeID/labels',
  )({
    params: { nodeID: 'cube-edge-01' },
    request: request('DELETE'),
  });
  assert.equal(res.status, 400);
});

test('data-plane label write is 403 with a readable error', async () => {
  const res = await handlerFor(
    'PATCH',
    '/opsapi/v1/nodes/:nodeID/labels',
  )({
    params: { nodeID: 'cube-edge-01' },
    request: request('PATCH', { labels: { 'kubernetes.io/arch': 'arm' } }),
  });
  assert.equal(res.status, 403);
  assert.match((await res.json()).error, /reserved/);
});

function api(method: string, path: string, body?: unknown) {
  return new Request(`http://localhost${path}`, {
    method,
    headers: body !== undefined ? { 'content-type': 'application/json' } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
}

test('machine type catalog create validates name and ignores a missing node list', async () => {
  resetMockState();
  const bad = await handlerFor(
    'POST',
    '/opsapi/v1/machine-types',
  )({
    params: {},
    request: api('POST', '/opsapi/v1/machine-types', { name: 'Intel' }),
  });
  assert.equal(bad.status, 400);

  const created = await handlerFor(
    'POST',
    '/opsapi/v1/machine-types',
  )({
    params: {},
    request: api('POST', '/opsapi/v1/machine-types', { name: 'amd-epyc', displayName: 'AMD' }),
  });
  assert.equal(created.status, 201);
  const view = await created.json();
  assert.equal(view.name, 'amd-epyc');
  assert.equal(view.nodeCount, 0);

  const dup = await handlerFor(
    'POST',
    '/opsapi/v1/machine-types',
  )({
    params: {},
    request: api('POST', '/opsapi/v1/machine-types', { name: 'amd-epyc' }),
  });
  assert.equal(dup.status, 409);
});

test('replace machine type nodes binds, moves, unbinds and warns on mixed CPU', async () => {
  resetMockState();
  const replaced = await handlerFor(
    'PUT',
    '/opsapi/v1/machine-types/:name/nodes',
  )({
    params: { name: 'intel-e5-2680' },
    request: api('PUT', '/opsapi/v1/machine-types/intel-e5-2680/nodes', {
      nodeIDs: ['cube-edge-02', 'cube-edge-03'],
    }),
  });
  assert.equal(replaced.status, 200);
  const body = await replaced.json();
  assert.equal(
    body.results.every((item: { ok: boolean }) => item.ok),
    true,
  );

  const listed = await handlerFor(
    'GET',
    '/opsapi/v1/machine-types',
  )({
    params: {},
    request: api('GET', '/opsapi/v1/machine-types'),
  });
  const types = await listed.json();
  const intel = types.find((item: { name: string }) => item.name === 'intel-e5-2680');
  assert.deepEqual(intel.nodes.map((node: { nodeID: string }) => node.nodeID).sort(), [
    'cube-edge-02',
    'cube-edge-03',
  ]);
  assert.ok(intel.warnings.includes('multiple_cpuid'));

  const origin = await handlerFor(
    'GET',
    '/opsapi/v1/nodes/:nodeID',
  )({
    params: { nodeID: 'cube-edge-01' },
    request: api('GET', '/opsapi/v1/nodes/cube-edge-01'),
  });
  assert.equal((await origin.json()).machineType ?? '', '');
});

test('node machine-type bind and unbind, including an unknown type', async () => {
  resetMockState();
  const missing = await handlerFor(
    'PUT',
    '/opsapi/v1/nodes/:nodeID/machine-type',
  )({
    params: { nodeID: 'cube-edge-02' },
    request: api('PUT', '/opsapi/v1/nodes/cube-edge-02/machine-type', { machineType: 'no-such' }),
  });
  assert.equal(missing.status, 404);

  const bound = await handlerFor(
    'PUT',
    '/opsapi/v1/nodes/:nodeID/machine-type',
  )({
    params: { nodeID: 'cube-edge-02' },
    request: api('PUT', '/opsapi/v1/nodes/cube-edge-02/machine-type', {
      machineType: 'intel-e5-2680',
    }),
  });
  assert.equal(bound.status, 204);

  const cleared = await handlerFor(
    'DELETE',
    '/opsapi/v1/nodes/:nodeID/machine-type',
  )({
    params: { nodeID: 'cube-edge-02' },
    request: api('DELETE', '/opsapi/v1/nodes/cube-edge-02/machine-type'),
  });
  assert.equal(cleared.status, 204);
});

test('deleting a machine type is 409 until unbind clears the label', async () => {
  resetMockState();
  const blocked = await handlerFor(
    'DELETE',
    '/opsapi/v1/machine-types/:name',
  )({
    params: { name: 'intel-e5-2680' },
    request: api('DELETE', '/opsapi/v1/machine-types/intel-e5-2680'),
  });
  assert.equal(blocked.status, 409);

  const removed = await handlerFor(
    'DELETE',
    '/opsapi/v1/machine-types/:name',
  )({
    params: { name: 'intel-e5-2680' },
    request: api('DELETE', '/opsapi/v1/machine-types/intel-e5-2680?unbind=true'),
  });
  assert.equal(removed.status, 204);

  const node = await handlerFor(
    'GET',
    '/opsapi/v1/nodes/:nodeID',
  )({
    params: { nodeID: 'cube-edge-01' },
    request: api('GET', '/opsapi/v1/nodes/cube-edge-01'),
  });
  const detail = await node.json();
  assert.equal(detail.labels?.['cube.cubesandbox.io/machine-type'], undefined);
});

test('unknown node is 404', async () => {
  const res = await handlerFor(
    'PATCH',
    '/opsapi/v1/nodes/:nodeID/labels',
  )({
    params: { nodeID: 'missing' },
    request: request('PATCH', { labels: { team: 'x' } }),
  });
  assert.equal(res.status, 404);
});
