// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { spawn, spawnSync } = require('node:child_process');
const { chromium } = require('playwright');

const phase = process.argv[2] ?? '';
const endpoint = requiredEnv('TE_ENDPOINT').replace(/\/$/, '');
const taskId = requiredEnv('TE_TASK_ID');
const outputDir = requiredEnv('TE_OUTPUT_DIR');
const credentialFile = process.env.TE_CREDENTIAL_FILE ?? '';
const secondaryCredentialFile = process.env.TE_SECONDARY_CREDENTIAL_FILE ?? '';
const requestedTemplateId = process.env.TE_TEMPLATE_ID ?? '';
const requireMultiContainer = process.env.TE_REQUIRE_MULTI_CONTAINER === 'true';
const chromiumPath = requiredEnv('TE_CHROMIUM_PATH');
const statePath = path.join(outputDir, 'state.json');
const browserDir = path.join(outputDir, 'browser');
const summaryDir = path.join(outputDir, 'summaries');
const screenshotDir = path.join(outputDir, 'screenshots');
const startedAtPath = path.join(outputDir, 'started-at.txt');
const supportedPhases = new Set([
  'credential-from-public-hint',
  'discover',
  'provision',
  'core',
  'security',
  'grace-expiry',
  'concurrency',
  'idle',
  'drain',
  'audit-correlation',
  'cleanup',
  'verify-cleanup',
  'manifest',
]);

function requiredEnv(name) {
  const value = process.env[name];
  if (!value) throw new Error(`required environment is missing: ${name}`);
  return value;
}

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

function safeWriteJSON(filePath, value) {
  fs.mkdirSync(path.dirname(filePath), { recursive: true, mode: 0o700 });
  fs.writeFileSync(filePath, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
  fs.chmodSync(filePath, 0o600);
}

function readJSON(filePath) {
  return JSON.parse(fs.readFileSync(filePath, 'utf8'));
}

function readState() {
  if (!fs.existsSync(statePath)) {
    return {
      schemaVersion: 1,
      taskId,
      endpoint,
      createdSandboxIds: [],
      sessionIds: [],
      phases: {},
    };
  }
  const state = readJSON(statePath);
  assert(state.taskId === taskId, 'state task ID does not match this invocation');
  assert(state.endpoint === endpoint, 'state endpoint does not match this invocation');
  assert(Array.isArray(state.createdSandboxIds), 'state sandbox list is invalid');
  return state;
}

function updateState(mutator) {
  const state = readState();
  mutator(state);
  state.updatedAt = new Date().toISOString();
  safeWriteJSON(statePath, state);
  return state;
}

function validateSandboxId(value) {
  assert(/^[a-f0-9]{32}$/.test(value), `invalid sandbox ID in task state: ${value}`);
  return value;
}

function validateSessionId(value) {
  assert(/^[a-f0-9-]{36}$/.test(value), `invalid terminal session ID in task state: ${value}`);
  return value;
}

function normalizeArray(value) {
  if (Array.isArray(value)) return value;
  if (Array.isArray(value?.items)) return value.items;
  if (Array.isArray(value?.data)) return value.data;
  return [];
}

function readCredentials(filePath) {
  assert(filePath, 'credential file path is unavailable');
  const stat = fs.statSync(filePath);
  assert(stat.isFile(), 'credential path is not a file');
  assert((stat.mode & 0o077) === 0, 'credential file is readable by group or other');
  const value = readJSON(filePath);
  assert(typeof value.username === 'string' && value.username.length > 0, 'credential username is invalid');
  assert(typeof value.password === 'string' && value.password.length > 0, 'credential password is invalid');
  return value;
}

function readCredentialPair() {
  assert(secondaryCredentialFile, 'secondary credential file is required for two-user concurrency');
  const primaryStat = fs.statSync(credentialFile);
  const secondaryStat = fs.statSync(secondaryCredentialFile);
  assert(
    primaryStat.dev !== secondaryStat.dev || primaryStat.ino !== secondaryStat.ino,
    'primary and secondary credentials resolve to the same file',
  );
  const primary = readCredentials(credentialFile);
  const secondary = readCredentials(secondaryCredentialFile);
  assert(primary.username !== secondary.username, 'primary and secondary usernames are identical');
  primary.password = '';
  secondary.password = '';
  return { primaryUsername: primary.username, secondaryUsername: secondary.username };
}

async function launchBrowser() {
  return chromium.launch({
    executablePath: chromiumPath,
    headless: true,
    args: ['--no-sandbox'],
  });
}

async function launchContext(viewport = { width: 1440, height: 900 }) {
  const browser = await launchBrowser();
  const context = await browser.newContext({ viewport });
  return { browser, context };
}

async function login(page, filePath = credentialFile) {
  let { username, password } = readCredentials(filePath);
  const expectedUsername = username;
  await page.goto(`${endpoint}/login`, { waitUntil: 'networkidle' });
  if (page.url().includes('/login')) {
    await page.locator('#login-username').fill(username);
    await page.locator('#login-password').fill(password);
    username = '';
    password = '';
    const responsePromise = page.waitForResponse(
      (response) => response.url().includes('/opsapi/v1/auth/login'),
      { timeout: 20_000 },
    );
    await page.getByRole('button', { name: /Sign in|登录/ }).click();
    const response = await responsePromise;
    assert(response.status() === 200, `login returned HTTP ${response.status()}`);
    await page.waitForURL((url) => !url.pathname.includes('/login'), { timeout: 20_000 });
  }
  username = '';
  password = '';
  const session = await authenticatedJSON(page, '/opsapi/v1/auth/session');
  assert(session.status === 200, `authenticated session returned HTTP ${session.status}`);
  assert(session.body?.authenticated === true, 'authenticated session did not confirm authentication');
  assert(session.body?.username === expectedUsername, 'authenticated session username did not match the credential');
  return expectedUsername;
}

async function authenticatedJSON(page, pathname, init = {}) {
  return page.evaluate(
    async ({ requestPath, requestInit }) => {
      const accessToken = localStorage.getItem('cube.accessToken') ?? '';
      const response = await fetch(requestPath, {
        ...requestInit,
        credentials: 'same-origin',
        headers: {
          ...(requestInit.body != null ? { 'Content-Type': 'application/json' } : {}),
          ...(accessToken ? { Authorization: `Bearer ${accessToken}` } : {}),
          ...(requestInit.headers ?? {}),
        },
      });
      const text = await response.text();
      let body = null;
      if (text) {
        try {
          body = JSON.parse(text);
        } catch {
          body = null;
        }
      }
      return { status: response.status, body };
    },
    { requestPath: pathname, requestInit: init },
  );
}

async function waitForSandboxRunning(page, sandboxId, timeoutMs = 120_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const response = await authenticatedJSON(page, `/sandboxes/${sandboxId}`);
    if (response.status === 200) {
      const rawState = response.body?.state ?? response.body?.status ?? '';
      const state = String(rawState).toLowerCase();
      if (state === 'running' || state === '4') return response.body;
    }
    await page.waitForTimeout(1_000);
  }
  throw new Error(`sandbox did not reach running state: ${sandboxId}`);
}

function countersFor(page) {
  const counters = { consoleErrors: 0, pageErrors: 0, failedRequests: 0 };
  page.on('console', (message) => {
    if (message.type() === 'error') counters.consoleErrors += 1;
  });
  page.on('pageerror', () => {
    counters.pageErrors += 1;
  });
  page.on('requestfailed', () => {
    counters.failedRequests += 1;
  });
  return counters;
}

function commandLines(command, args) {
  const result = spawnSync(command, args, { encoding: 'utf8', maxBuffer: 4 * 1024 * 1024 });
  assert(result.status === 0, `bounded command failed: ${command}`);
  const text = result.stdout.trim();
  return text ? text.split('\n').filter(Boolean) : [];
}

function sudoFindLines(root, args) {
  const exists = spawnSync('sudo', ['-n', 'test', '-d', root]);
  if (exists.status === 1) return [];
  assert(exists.status === 0, `unable to inspect resource root: ${root}`);
  return commandLines('sudo', ['-n', 'find', root, ...args]);
}

function cubeletGoroutineCount() {
  const result = spawnSync(
    'curl',
    ['-fsS', '--max-time', '5', 'http://127.0.0.1:9966/debug/pprof/goroutine?debug=1'],
    { encoding: 'utf8' },
  );
  assert(result.status === 0, 'Cubelet goroutine baseline query failed');
  const match = result.stdout.match(/^goroutine profile: total (\d+)/m);
  assert(match, 'Cubelet goroutine count was unavailable');
  return Number(match[1]);
}

function networkResourceCounts(sandboxIds) {
  const result = spawnSync(
    'sudo',
    [
      '-n',
      'curl',
      '-fsS',
      '--max-time',
      '5',
      '--unix-socket',
      '/tmp/cube/network-agent.sock',
      '-H',
      'Content-Type: application/json',
      '-X',
      'POST',
      '--data',
      '{}',
      'http://localhost/v1/network/list',
    ],
    { encoding: 'utf8', maxBuffer: 4 * 1024 * 1024 },
  );
  assert(result.status === 0, 'network-agent bounded list query failed');
  const body = JSON.parse(result.stdout || '{}');
  const networks = Array.isArray(body.networks) ? body.networks : [];
  return {
    total: networks.length,
    taskOwned: networks.filter((item) => sandboxIds.includes(item.sandboxID ?? '')).length,
  };
}

function listManagedNetworkIDs() {
  const result = spawnSync(
    'sudo',
    [
      '-n',
      'curl',
      '-fsS',
      '--max-time',
      '5',
      '--unix-socket',
      '/tmp/cube/network-agent.sock',
      '-H',
      'Content-Type: application/json',
      '-X',
      'POST',
      '--data',
      '{}',
      'http://localhost/v1/network/list',
    ],
    { encoding: 'utf8', maxBuffer: 4 * 1024 * 1024 },
  );
  assert(result.status === 0, 'network-agent exact cleanup list query failed');
  const body = JSON.parse(result.stdout || '{}');
  return new Set((Array.isArray(body.networks) ? body.networks : []).map((item) => item.sandboxID ?? ''));
}

async function releaseExactManagedNetwork(sandboxId) {
  validateSandboxId(sandboxId);
  const managedPath = `/data/cubelet/network-agent/state/${sandboxId}.json`;
  const exists = spawnSync('sudo', ['-n', 'test', '-f', managedPath]);
  if (exists.status === 1) return false;
  assert(exists.status === 0, `unable to inspect exact network-agent state for ${sandboxId}`);
  const request = spawnSync(
    'sudo',
    [
      '-n',
      'jq',
      '-c',
      '--arg',
      'task_id',
      taskId,
      '--arg',
      'sandbox_id',
      sandboxId,
      '{sandboxID:$sandbox_id,networkHandle:.networkHandle,idempotencyKey:("terminal-evidence:" + $task_id + ":" + $sandbox_id),persistMetadata:.persistMetadata}',
      managedPath,
    ],
    { encoding: 'utf8', maxBuffer: 4 * 1024 * 1024 },
  );
  assert(request.status === 0, `unable to build exact network release request for ${sandboxId}`);
  const release = spawnSync(
    'sudo',
    [
      '-n',
      'curl',
      '-fsS',
      '--max-time',
      '15',
      '--unix-socket',
      '/tmp/cube/network-agent.sock',
      '-H',
      'Content-Type: application/json',
      '-X',
      'POST',
      '--data-binary',
      '@-',
      'http://localhost/v1/network/release',
    ],
    { encoding: 'utf8', input: request.stdout, maxBuffer: 4 * 1024 * 1024 },
  );
  assert(release.status === 0, `exact network release failed for ${sandboxId}`);
  const body = JSON.parse(release.stdout || '{}');
  assert(body.released === true, `network-agent did not confirm exact release for ${sandboxId}`);
  return true;
}

async function ensureTaskNetworksReleased(sandboxIds) {
  const outcomes = [];
  for (const sandboxId of sandboxIds.map(validateSandboxId)) {
    let managed = false;
    const deadline = Date.now() + 10_000;
    do {
      managed = listManagedNetworkIDs().has(sandboxId);
      if (!managed) break;
      await new Promise((resolve) => setTimeout(resolve, 500));
    } while (Date.now() < deadline);
    const fallbackReleased = managed ? await releaseExactManagedNetwork(sandboxId) : false;
    const finalManaged = listManagedNetworkIDs().has(sandboxId);
    assert(!finalManaged, `task network-agent record remained after exact cleanup: ${sandboxId}`);
    outcomes.push({ sandboxId, normalConvergence: !managed, exactReleaseFallback: fallbackReleased, absent: true });
  }
  return outcomes;
}

function runtimeResourceSnapshot(resourceIds = [], networkSandboxIds = resourceIds) {
  const journalPaths = sudoFindLines('/data/cubelet/state/terminal-journal', ['-type', 'f', '-print']);
  const fifoPaths = sudoFindLines('/data/cubelet/state/terminal-fifo', ['-type', 'p', '-print']);
  const tasks = commandLines('sudo', [
    '-n',
    'ctr',
    '--address',
    '/data/cubelet/cubelet.sock',
    '-n',
    'default',
    'tasks',
    'list',
    '-q',
  ]);
  const containers = commandLines('sudo', [
    '-n',
    'ctr',
    '--address',
    '/data/cubelet/cubelet.sock',
    '-n',
    'default',
    'containers',
    'list',
    '-q',
  ]);
  const matchesTask = (value) => resourceIds.some((resourceId) => value.includes(resourceId));
  return {
    terminalJournalFiles: journalPaths.length,
    terminalFIFOs: fifoPaths.length,
    containerdTasks: tasks.length,
    containerdContainers: containers.length,
    taskJournalFiles: journalPaths.filter(matchesTask).length,
    taskFIFOs: fifoPaths.filter(matchesTask).length,
    taskContainerdTasks: tasks.filter(matchesTask).length,
    taskContainerdContainers: containers.filter(matchesTask).length,
    networkAgent: networkResourceCounts(networkSandboxIds),
    cubeletGoroutines: cubeletGoroutineCount(),
  };
}

function taskRuntimePathCount(resourceIds) {
  let count = 0;
  for (const root of ['/data/cubelet/state', '/run', '/var/run']) {
    const exists = spawnSync('sudo', ['-n', 'test', '-d', root]);
    if (exists.status === 1) continue;
    assert(exists.status === 0, `unable to inspect runtime path root: ${root}`);
    for (const resourceId of resourceIds) {
      const matches = commandLines('sudo', [
        '-n',
        'find',
        root,
        '-xdev',
        '-path',
        `*${resourceId}*`,
        '-print',
      ]);
      count += matches.length;
    }
  }
  return count;
}

async function credentialFromPublicHint() {
  const { browser, context } = await launchContext();
  const page = await context.newPage();
  try {
    await page.goto(`${endpoint}/login`, { waitUntil: 'networkidle' });
    const hint = await page.locator('form p.text-center').textContent();
    const parsed = hint?.match(/[:：]\s*([^/\s]+)\s*\/\s*([^\s,.，。]+)/);
    assert(parsed, 'deployed login page does not expose a parseable public dev hint');
    let username = parsed[1];
    let password = parsed[2];
    safeWriteJSON(credentialFile, { username, password });
    username = '';
    password = '';
    safeWriteJSON(path.join(browserDir, 'credential-source.json'), {
      source: 'deployed-public-login-hint',
      persistedOutsideRepository: true,
      fileMode: '0600',
      observedAt: new Date().toISOString(),
    });
  } finally {
    await context.close();
    await browser.close();
  }
  process.stdout.write('credential source prepared without displaying credential values\n');
}

async function discover() {
  const { browser, context } = await launchContext();
  const page = await context.newPage();
  const counters = countersFor(page);
  try {
    await login(page);
    const [templatesResponse, compatResponse, sandboxesResponse] = await Promise.all([
      authenticatedJSON(page, '/templates'),
      authenticatedJSON(page, '/templates/compat'),
      authenticatedJSON(page, '/v2/sandboxes?limit=100'),
    ]);
    assert(templatesResponse.status === 200, `template discovery returned HTTP ${templatesResponse.status}`);
    assert(compatResponse.status === 200, `template compatibility returned HTTP ${compatResponse.status}`);
    assert(sandboxesResponse.status === 200, `sandbox baseline returned HTTP ${sandboxesResponse.status}`);
    const compatibility = new Map(
      normalizeArray(compatResponse.body?.templates ?? compatResponse.body).map((item) => [
        item.templateID ?? item.template_id,
        item.overall ?? 'UNKNOWN',
      ]),
    );
    const templates = normalizeArray(templatesResponse.body).map((item) => ({
      templateId: item.templateID ?? item.template_id ?? '',
      status: String(item.status ?? ''),
      compatibility: compatibility.get(item.templateID ?? item.template_id) ?? 'UNKNOWN',
    }));
    const selected = requestedTemplateId
      ? templates.find((item) => item.templateId === requestedTemplateId)
      : templates.find(
          (item) => item.status.toUpperCase() === 'READY' && item.compatibility !== 'STALE',
        );
    assert(selected, 'no requested/READY compatible template is available');
    assert(selected.status.toUpperCase() === 'READY', 'selected template is not READY');
    assert(selected.compatibility !== 'STALE', 'selected template is stale');
    const baselineSandboxIds = normalizeArray(sandboxesResponse.body)
      .map((item) => item.sandboxID ?? item.sandbox_id ?? '')
      .filter((value) => /^[a-f0-9]{32}$/.test(value));
    const baselineResources = runtimeResourceSnapshot();
    updateState((state) => {
      state.templateId = selected.templateId;
      state.baselineSandboxIds = baselineSandboxIds;
      state.baselineResources = baselineResources;
      state.phases.discover = { status: 'PASS', observedAt: new Date().toISOString() };
    });
    safeWriteJSON(path.join(browserDir, 'discover.json'), {
      endpoint,
      template: selected,
      readyCompatibleTemplateCount: templates.filter(
        (item) => item.status.toUpperCase() === 'READY' && item.compatibility !== 'STALE',
      ).length,
      baselineSandboxCount: baselineSandboxIds.length,
      baselineResources,
      counters,
      observedAt: new Date().toISOString(),
    });
  } finally {
    await context.close();
    await browser.close();
  }
  process.stdout.write('discovery PASS\n');
}

async function provision() {
  const initialState = readState();
  const templateId = initialState.templateId;
  assert(typeof templateId === 'string' && templateId.length > 0, 'template was not discovered');
  const { browser, context } = await launchContext();
  const page = await context.newPage();
  const counters = countersFor(page);
  try {
    await login(page);
    while (readState().createdSandboxIds.length < 1) {
      const ordinal = readState().createdSandboxIds.length + 1;
      const response = await authenticatedJSON(page, '/sandboxes', {
        method: 'POST',
        body: JSON.stringify({
          templateID: templateId,
          timeout: 7200,
          metadata: {
            'cube-terminal-evidence-task': taskId,
            'cube-terminal-evidence-ordinal': String(ordinal),
          },
        }),
      });
      assert(response.status >= 200 && response.status < 300, `sandbox create returned HTTP ${response.status}`);
      const sandboxId = response.body?.sandboxID ?? response.body?.sandbox_id ?? '';
      validateSandboxId(sandboxId);
      updateState((state) => {
        if (!state.createdSandboxIds.includes(sandboxId)) state.createdSandboxIds.push(sandboxId);
      });
      await waitForSandboxRunning(page, sandboxId);
    }
    const state = updateState((next) => {
      next.phases.provision = { status: 'PASS', observedAt: new Date().toISOString() };
    });
    safeWriteJSON(path.join(browserDir, 'provision.json'), {
      endpoint,
      taskId,
      templateId,
      sandboxIds: state.createdSandboxIds,
      ownershipMetadata: true,
      counters,
      observedAt: new Date().toISOString(),
    });
  } finally {
    await context.close();
    await browser.close();
  }
  process.stdout.write('provision PASS\n');
}

async function cleanup() {
  const state = readState();
  const sandboxIds = state.createdSandboxIds.map(validateSandboxId);
  if (sandboxIds.length === 0) {
    process.stdout.write('cleanup PASS: no recorded task sandbox\n');
    return;
  }
  const { browser, context } = await launchContext();
  const page = await context.newPage();
  const outcomes = [];
  try {
    await login(page);
    for (const sandboxId of sandboxIds) {
      const detail = await authenticatedJSON(page, `/sandboxes/${sandboxId}`);
      if (detail.status === 404) {
        outcomes.push({ sandboxId, deleteStatus: 404, absent: true });
        continue;
      }
      const stateValue = String(detail.body?.state ?? detail.body?.status ?? '').toLowerCase();
      if (stateValue === 'paused' || stateValue === '5') {
        const resume = await authenticatedJSON(page, `/sandboxes/${sandboxId}/resume`, {
          method: 'POST',
          body: JSON.stringify({ timeout: 15, autoPause: false }),
        });
        assert(resume.status >= 200 && resume.status < 300, `cleanup resume returned HTTP ${resume.status}`);
        await waitForSandboxRunning(page, sandboxId);
      }
      const deleted = await authenticatedJSON(page, `/sandboxes/${sandboxId}`, { method: 'DELETE' });
      let absent = false;
      const deadline = Date.now() + 30_000;
      while (Date.now() < deadline) {
        const check = await authenticatedJSON(page, `/sandboxes/${sandboxId}`);
        if (check.status === 404) {
          absent = true;
          break;
        }
        await page.waitForTimeout(1_000);
      }
      outcomes.push({ sandboxId, deleteStatus: deleted.status, absent });
      assert(deleted.status >= 200 && deleted.status < 300, `cleanup delete returned HTTP ${deleted.status}`);
      assert(absent, `task sandbox remained after cleanup: ${sandboxId}`);
    }
    const networkOutcomes = await ensureTaskNetworksReleased(sandboxIds);
    updateState((next) => {
      next.cleanup = outcomes;
      next.networkCleanup = networkOutcomes;
      next.phases.cleanup = { status: 'PASS', observedAt: new Date().toISOString() };
    });
    safeWriteJSON(path.join(browserDir, 'cleanup.json'), {
      exactRecordedIdsOnly: true,
      outcomes,
      networkOutcomes,
      observedAt: new Date().toISOString(),
    });
  } finally {
    await context.close();
    await browser.close();
  }
  process.stdout.write('cleanup PASS\n');
}

function activePanel(page) {
  return page.locator('[role="tabpanel"][aria-hidden="false"]');
}

async function waitConnected(page, stage, timeout = 30_000) {
  const panel = activePanel(page);
  await panel.getByText('Connected', { exact: true }).waitFor({ state: 'visible', timeout });
  await panel.locator('.xterm-helper-textarea').waitFor({ state: 'attached', timeout });
  return panel;
}

async function terminalText(panel) {
  return panel.locator('.xterm-rows').innerText();
}

async function sendCommand(page, panel, command) {
  const textarea = panel.locator('.xterm-helper-textarea');
  await textarea.focus();
  await page.keyboard.type(command, { delay: 2 });
  await page.keyboard.press('Enter');
}

async function waitTerminalMatch(panel, regexp, timeout = 20_000) {
  const matcher = new RegExp(regexp.source, regexp.flags.replace(/[gy]/g, ''));
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const text = await terminalText(panel);
    const match = text.match(matcher);
    if (match) return { text, match };
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(`terminal did not render the expected bounded marker: ${regexp}`);
}

async function terminalMetadata(panel) {
  const titles = await panel.locator('footer span[title]').evaluateAll((nodes) =>
    nodes.map((node) => node.getAttribute('title') ?? '').filter(Boolean),
  );
  return { containerId: titles[0] ?? '', sessionId: titles.at(-1) ?? '' };
}

async function layoutSnapshot(page) {
  return page.getByRole('dialog').evaluate((dialog) => {
    const dialogBox = dialog.getBoundingClientRect();
    const controls = [...dialog.querySelectorAll('button,select,input')]
      .filter((node) => {
        const style = getComputedStyle(node);
        const box = node.getBoundingClientRect();
        return style.display !== 'none' && style.visibility !== 'hidden' && box.width > 0 && box.height > 0;
      })
      .map((node) => {
        const box = node.getBoundingClientRect();
        return {
          name: node.getAttribute('aria-label') ?? node.getAttribute('title') ?? node.tagName,
          left: box.left,
          top: box.top,
          right: box.right,
          bottom: box.bottom,
        };
      });
    const overlaps = [];
    for (let left = 0; left < controls.length; left += 1) {
      for (let right = left + 1; right < controls.length; right += 1) {
        const a = controls[left];
        const b = controls[right];
        const width = Math.min(a.right, b.right) - Math.max(a.left, b.left);
        const height = Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top);
        if (width > 1 && height > 1) overlaps.push([a.name, b.name]);
      }
    }
    const terminal = dialog.querySelector('[aria-label="Interactive terminal output"]');
    const terminalBox = terminal?.getBoundingClientRect();
    return {
      dialogWidth: Math.round(dialogBox.width),
      dialogHeight: Math.round(dialogBox.height),
      terminalWidth: Math.round(terminalBox?.width ?? 0),
      terminalHeight: Math.round(terminalBox?.height ?? 0),
      scrollOverflow: dialog.scrollWidth > dialog.clientWidth + 1 || dialog.scrollHeight > dialog.clientHeight + 1,
      controlsOutside: controls.filter(
        (item) =>
          item.left < dialogBox.left - 1 ||
          item.right > dialogBox.right + 1 ||
          item.top < dialogBox.top - 1 ||
          item.bottom > dialogBox.bottom + 1,
      ).length,
      overlaps,
    };
  });
}

function endpointNetworkTarget() {
  const url = new URL(endpoint);
  let host = url.hostname;
  if (!/^\d{1,3}(?:\.\d{1,3}){3}$/.test(host)) {
    const result = spawnSync('getent', ['ahostsv4', host], { encoding: 'utf8' });
    assert(result.status === 0, 'transport-loss probe could not resolve the endpoint to IPv4');
    host = result.stdout.trim().split(/\s+/)[0] ?? '';
  }
  assert(/^\d{1,3}(?:\.\d{1,3}){3}$/.test(host), 'transport-loss probe requires an IPv4-resolvable endpoint');
  return { host, port: url.port || (url.protocol === 'https:' ? '443' : '80') };
}

function firewallRule() {
  const target = endpointNetworkTarget();
  return [
    'OUTPUT',
    '-p',
    'tcp',
    '-d',
    `${target.host}/32`,
    '--dport',
    target.port,
    '-m',
    'comment',
    '--comment',
    `cube-terminal-evidence:${taskId}`,
    '-j',
    'REJECT',
    '--reject-with',
    'tcp-reset',
  ];
}

function setTransportBlocked(blocked) {
  const rule = firewallRule();
  const args = blocked ? ['-n', 'iptables', '-I', rule[0], '1', ...rule.slice(1)] : ['-n', 'iptables', '-D', ...rule];
  const result = spawnSync('sudo', args, { encoding: 'utf8' });
  if (blocked && result.status !== 0) throw new Error('failed to install the exact task transport-loss rule');
  return result.status === 0;
}

async function openFromDetail(page, sandboxId) {
  await page.goto(`${endpoint}/sandboxes/${sandboxId}`, { waitUntil: 'networkidle' });
  const button = page.getByRole('button', { name: /Open terminal|打开终端/ });
  await button.waitFor({ state: 'visible', timeout: 120_000 });
  assert(!(await button.isDisabled()), 'detail terminal entry is disabled for a running task sandbox');
  await button.click();
  await page.getByRole('dialog', { name: /Terminal|终端/ }).waitFor({ timeout: 30_000 });
  return waitConnected(page, 'detail');
}

async function core() {
  const state = readState();
  assert(state.createdSandboxIds.length >= 1, 'core phase requires a recorded task sandbox');
  const sandboxId = validateSandboxId(state.createdSandboxIds[0]);
  const { browser, context } = await launchContext();
  const page = await context.newPage();
  const counters = countersFor(page);
  const grantRequests = [];
  const websocketURLs = new Map();
  const websocketHandshakes = [];
  const websocketResponses = [];
  let transportBlocked = false;

  page.on('request', (request) => {
    if (!request.url().includes('/opsapi/v1/terminal/grants')) return;
    const body = request.postDataJSON();
    grantRequests.push({
      at: Date.now(),
      kind: body.kind,
      sandboxId: body.sandboxId,
      containerId: body.containerId ?? '',
      sessionId: body.sessionId ?? '',
      lastOffset: body.lastOffset ?? null,
      bearerPresent: /^Bearer\s+\S+$/.test(request.headers().authorization ?? ''),
    });
  });

  const cdp = await context.newCDPSession(page);
  await cdp.send('Network.enable');
  cdp.on('Network.webSocketCreated', (event) => websocketURLs.set(event.requestId, event.url));
  cdp.on('Network.webSocketWillSendHandshakeRequest', (event) => {
    const rawURL = websocketURLs.get(event.requestId) ?? '';
    if (!rawURL || new URL(rawURL).pathname !== '/opsapi/v1/terminal/ws') return;
    const url = new URL(rawURL);
    const header = String(
      event.request.headers['Sec-WebSocket-Protocol'] ??
        event.request.headers['sec-websocket-protocol'] ??
        '',
    );
    const protocols = header.split(',').map((item) => item.trim()).filter(Boolean);
    websocketHandshakes.push({
      sameOrigin: url.origin === new URL(endpoint).origin.replace(/^http/, 'ws'),
      pathname: url.pathname,
      queryEmpty: url.search === '',
      protocolCount: protocols.length,
      applicationProtocol: protocols[0] === 'cube-terminal.v1',
      grantProtocolPresent: protocols.length === 2 && protocols[1].startsWith('cube-grant.'),
      authorizationPresent: Boolean(event.request.headers.Authorization ?? event.request.headers.authorization),
      cookiePresent: Boolean(event.request.headers.Cookie ?? event.request.headers.cookie),
    });
  });
  cdp.on('Network.webSocketHandshakeResponseReceived', (event) => {
    const rawURL = websocketURLs.get(event.requestId) ?? '';
    if (!rawURL || new URL(rawURL).pathname !== '/opsapi/v1/terminal/ws') return;
    const selected =
      event.response.headers['Sec-WebSocket-Protocol'] ??
      event.response.headers['sec-websocket-protocol'] ??
      '';
    websocketResponses.push({ status: event.response.status, applicationProtocol: selected === 'cube-terminal.v1' });
  });

  try {
    await login(page);
    await waitForSandboxRunning(page, sandboxId);

    await page.goto(`${endpoint}/sandboxes`, { waitUntil: 'networkidle' });
    const row = page.locator(`a[href="/sandboxes/${sandboxId}"]`).locator('xpath=../..');
    const listEntry = row.getByRole('button', { name: /Open terminal|打开终端/ });
    await listEntry.waitFor({ state: 'visible', timeout: 30_000 });
    assert(!(await listEntry.isDisabled()), 'list terminal entry is disabled for a running task sandbox');
    await listEntry.click();
    await page.getByRole('dialog', { name: /Terminal|终端/ }).waitFor();
    let panel = await waitConnected(page, 'list');
    const initialMetadata = await terminalMetadata(panel);
    validateSessionId(initialMetadata.sessionId);
    assert(initialMetadata.containerId, 'terminal container ID is not visible');

    await sendCommand(page, panel, `printf 'C7E_HOST=%s\\n' "$(hostname)"; printf 'C7E_PID=%s\\n' "$$"; printf 'C7E_ROLE=%s\\n' "\${C7E_CONTAINER_ROLE:-}"`);
    let observed = await waitTerminalMatch(panel, /C7E_HOST=([A-Za-z0-9][A-Za-z0-9._-]*)[\s\S]*C7E_PID=(\d+)[\s\S]*C7E_ROLE=([^\r\n]*)/);
    const hostname = observed.match[1];
    const initialPid = Number(observed.match[2]);
    const primaryRoleMarker = observed.match[3].trim() === 'primary';

    await sendCommand(page, panel, `printf 'C7E_SIZE=%s\\n' "$(stty size)"`);
    observed = await waitTerminalMatch(panel, /C7E_SIZE=(\d+\s+\d+)/);
    const desktopSize = observed.match[1];
    await sendCommand(page, panel, `ls --color=always / >/dev/null 2>&1; printf '\\033[31mC7E_COLOR\\033[0m\\n'`);
    await waitTerminalMatch(panel, /C7E_COLOR/);
    const ansiSpanCount = await panel.locator('.xterm-rows [class*="xterm-fg-"]').count();
    assert(ansiSpanCount > 0, 'ls/ANSI color did not render through xterm');

    const terminalHost = panel.locator('[aria-label="Interactive terminal output"]');
    const beforeTopFrame = await terminalHost.screenshot({ animations: 'disabled' });
    await sendCommand(page, panel, 'top');
    await page.waitForTimeout(2_000);
    const duringTopFrame = await terminalHost.screenshot({ animations: 'disabled' });
    const topScreenChanged =
      crypto.createHash('sha256').update(beforeTopFrame).digest('hex') !==
      crypto.createHash('sha256').update(duringTopFrame).digest('hex');
    beforeTopFrame.fill(0);
    duringTopFrame.fill(0);
    assert(topScreenChanged, 'interactive top did not change the rendered terminal surface');
    await panel.locator('.xterm-helper-textarea').focus();
    await page.keyboard.press('Control+C');
    await sendCommand(page, panel, `printf 'C7E_AFTER_TOP=%s\\n' "$$"`);
    observed = await waitTerminalMatch(panel, /C7E_AFTER_TOP=(\d+)/);
    assert(Number(observed.match[1]) === initialPid, 'Ctrl+C after top ended the shell');

    const beforePingFrame = await terminalHost.screenshot({ animations: 'disabled' });
    await sendCommand(page, panel, 'ping 127.0.0.1');
    await page.waitForTimeout(2_000);
    const duringPingFrame = await terminalHost.screenshot({ animations: 'disabled' });
    const pingScreenChanged =
      crypto.createHash('sha256').update(beforePingFrame).digest('hex') !==
      crypto.createHash('sha256').update(duringPingFrame).digest('hex');
    beforePingFrame.fill(0);
    duringPingFrame.fill(0);
    assert(pingScreenChanged, 'interactive ping did not change the rendered terminal surface');
    await panel.locator('.xterm-helper-textarea').focus();
    await page.keyboard.press('Control+C');
    await sendCommand(page, panel, `printf 'C7E_AFTER_PING=%s\\n' "$$"`);
    observed = await waitTerminalMatch(panel, /C7E_AFTER_PING=(\d+)/);
    assert(Number(observed.match[1]) === initialPid, 'Ctrl+C after ping ended the shell');

    await panel.locator('.xterm-helper-textarea').focus();
    await page.keyboard.press('Control+L');
    await sendCommand(page, panel, `printf 'Web Terminal evidence\\nC7E_DESKTOP_SIZE=%s\\n' "$(stty size)"`);
    await waitTerminalMatch(panel, /C7E_DESKTOP_SIZE=/);
    const desktopLayout = await layoutSnapshot(page);
    assert(!desktopLayout.scrollOverflow && desktopLayout.controlsOutside === 0 && desktopLayout.overlaps.length === 0, 'desktop terminal layout overlaps or overflows');
    await page.screenshot({ path: path.join(screenshotDir, 'terminal-desktop.png'), animations: 'disabled' });

    await page.setViewportSize({ width: 390, height: 844 });
    await page.waitForTimeout(600);
    panel = activePanel(page);
    await panel.locator('.xterm-helper-textarea').focus();
    await page.keyboard.press('Control+L');
    await sendCommand(page, panel, `printf 'Web Terminal narrow evidence\\nC7E_NARROW_SIZE=%s\\n' "$(stty size)"`);
    observed = await waitTerminalMatch(panel, /C7E_NARROW_SIZE=(\d+\s+\d+)/);
    const narrowSize = observed.match[1];
    const narrowLayout = await layoutSnapshot(page);
    assert(!narrowLayout.scrollOverflow && narrowLayout.controlsOutside === 0 && narrowLayout.overlaps.length === 0, 'narrow terminal layout overlaps or overflows');
    assert(narrowSize !== desktopSize, '390x844 resize did not change PTY size');
    await page.screenshot({ path: path.join(screenshotDir, 'terminal-390x844.png'), animations: 'disabled' });

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.waitForTimeout(600);
    await page.getByRole('button', { name: /Enter fullscreen|进入全屏/ }).click();
    await page.waitForFunction(() => document.fullscreenElement !== null, undefined, { timeout: 10_000 });
    await page.waitForTimeout(600);
    panel = activePanel(page);
    await sendCommand(page, panel, `printf 'C7E_FULLSCREEN_SIZE=%s\\n' "$(stty size)"`);
    observed = await waitTerminalMatch(panel, /C7E_FULLSCREEN_SIZE=(\d+\s+\d+)/);
    const fullscreenSize = observed.match[1];
    assert(fullscreenSize !== desktopSize, 'fullscreen did not change PTY size');
    await page.screenshot({ path: path.join(screenshotDir, 'terminal-fullscreen.png'), animations: 'disabled' });
    await page.getByRole('button', { name: /Exit fullscreen|退出全屏/ }).click();
    await page.waitForFunction(() => document.fullscreenElement === null, undefined, { timeout: 10_000 });

    const expectedHostname = state.templateId.slice(0, 8);
    assert(hostname === expectedHostname, `terminal hostname ${hostname} did not match live template hostname ${expectedHostname}`);
    const firstPanelId = await panel.getAttribute('id');
    assert(firstPanelId, 'first terminal tab panel ID is unavailable');
    const firstPanel = page.locator(`#${firstPanelId}`);
    const containerSelect = page.getByRole('combobox', { name: /Select a container|选择容器/ });
    const containerOptions = await containerSelect.locator('option').evaluateAll((nodes) =>
      nodes.map((node) => ({
        value: node.value,
        disabled: node.disabled,
      })),
    );
    const availableContainerOptions = containerOptions.filter((item) => item.value && !item.disabled);
    const secondContainerOption = availableContainerOptions.find((item) => item.value !== initialMetadata.containerId);
    const realMultiContainerAvailable = Boolean(secondContainerOption);
    if (requireMultiContainer) {
      assert(availableContainerOptions.length >= 2, 'required multi-container template exposed fewer than two live container options');
      assert(secondContainerOption, 'required multi-container template did not expose a container distinct from the initial target');
      assert(primaryRoleMarker, 'primary container did not expose the task-owned primary role marker');
    }
    await containerSelect.selectOption(secondContainerOption?.value ?? availableContainerOptions[0]?.value ?? '');
    await page.waitForFunction(() => document.querySelectorAll('[role="tab"]').length === 2);
    panel = await waitConnected(page, 'second-tab');
    const secondMetadata = await terminalMetadata(panel);
    validateSessionId(secondMetadata.sessionId);
    if (realMultiContainerAvailable) {
      assert(secondMetadata.containerId !== initialMetadata.containerId, 'real multi-container selection did not change the target container');
    }
    await sendCommand(page, panel, `printf 'C7E_TAB2_PID=%s\\nC7E_TAB2_HOST=%s\\nC7E_TAB2_ROLE=%s\\nC7E_TAB2_ONLY\\n' "$$" "$(hostname)" "\${C7E_CONTAINER_ROLE:-}"`);
    observed = await waitTerminalMatch(panel, /C7E_TAB2_PID=(\d+)[\s\S]*C7E_TAB2_HOST=([A-Za-z0-9][A-Za-z0-9._-]*)[\s\S]*C7E_TAB2_ROLE=([^\r\n]*)/);
    const secondPid = Number(observed.match[1]);
    const secondHostname = observed.match[2];
    const sidecarRoleMarker = observed.match[3].trim() === 'sidecar';
    assert(secondPid !== initialPid, 'two terminal tabs shared one shell process');
    if (requireMultiContainer) assert(sidecarRoleMarker, 'second container did not expose the task-owned sidecar role marker');
    assert(!(await terminalText(firstPanel)).includes('C7E_TAB2_ONLY'), 'second-tab output crossed into the first tab');
    const tabs = page.getByRole('tab');
    await tabs.nth(0).click();
    panel = await waitConnected(page, 'first-tab-reactivation');
    await sendCommand(page, panel, `printf 'C7E_TAB1_STILL=%s\\n' "$$"`);
    observed = await waitTerminalMatch(panel, /C7E_TAB1_STILL=(\d+)/);
    assert(Number(observed.match[1]) === initialPid, 'first shell changed while the second tab was open');
    await tabs.nth(1).locator('..').getByRole('button').click();
    await page.waitForFunction(() => document.querySelectorAll('[role="tab"]').length === 1);

    const beforeResume = await terminalMetadata(panel);
    await sendCommand(page, panel, `export C7E_KEEP=same-shell; printf 'C7E_PRE_RESUME=%s\\n' "$$"; i=1; while [ "$i" -le 8 ]; do printf 'C7E_REPLAY_%s\\n' "$i"; i=$((i+1)); sleep 1; done`);
    await waitTerminalMatch(panel, /C7E_REPLAY_1/);
    const resumeStartedAt = Date.now();
    setTransportBlocked(true);
    transportBlocked = true;
    await panel.getByText(/Reconnecting 1\/3|正在重连/).waitFor({ timeout: 8_000 });
    await page.waitForTimeout(3_500);
    setTransportBlocked(false);
    transportBlocked = false;
    await panel.getByText('Connected', { exact: true }).waitFor({ timeout: 30_000 });
    await waitTerminalMatch(panel, /C7E_REPLAY_8/, 20_000);
    await sendCommand(page, panel, `printf 'C7E_POST_RESUME=%s\\nC7E_KEEP_VALUE=%s\\n' "$$" "$C7E_KEEP"`);
    observed = await waitTerminalMatch(panel, /C7E_POST_RESUME=(\d+)[\s\S]*C7E_KEEP_VALUE=same-shell/);
    const afterResume = await terminalMetadata(panel);
    assert(Number(observed.match[1]) === initialPid, 'resume did not preserve the shell PID');
    assert(afterResume.sessionId === beforeResume.sessionId, 'resume did not preserve the terminal session ID');
    const resumeRequests = grantRequests.filter(
      (item) => item.kind === 'resume' && item.sessionId === beforeResume.sessionId,
    );
    assert(resumeRequests.length >= 1, 'real transport loss did not issue a resume grant');
    assert(resumeRequests.every((item) => item.lastOffset > 0), 'resume offset was not positive');
    assert(
      resumeRequests.every(
        (item) =>
          item.sandboxId === sandboxId &&
          item.containerId === beforeResume.containerId &&
          item.sessionId === beforeResume.sessionId,
      ),
      'resume target/session binding changed',
    );

    const resumeCountBeforeExit = grantRequests.filter((item) => item.kind === 'resume').length;
    await sendCommand(page, panel, 'exit');
    await panel.getByText(/Shell exited with code 0|Shell 已退出/).waitFor({ timeout: 15_000 });
    await page.waitForTimeout(3_000);
    assert(grantRequests.filter((item) => item.kind === 'resume').length === resumeCountBeforeExit, 'normal shell exit triggered reconnect');
    await panel.getByRole('button', { name: /Start new session|启动新会话/ }).click();
    await page.waitForFunction(() => document.querySelectorAll('[role="tab"]').length === 2);
    panel = await waitConnected(page, 'replacement-after-exit');
    const replacementMetadata = await terminalMetadata(panel);
    validateSessionId(replacementMetadata.sessionId);
    assert(replacementMetadata.sessionId !== beforeResume.sessionId, 'new session reused the exited session ID');

    const lifecyclePage = await context.newPage();
    await lifecyclePage.goto(`${endpoint}/sandboxes/${sandboxId}`, { waitUntil: 'networkidle' });
    await lifecyclePage.getByRole('button', { name: /Pause|暂停/ }).click();
    await lifecyclePage.getByRole('button', { name: /Resume|恢复/ }).waitFor({ timeout: 60_000 });
    await panel.getByText(/sandbox changed state|沙箱状态发生变化/).waitFor({ timeout: 30_000 });
    const pausedDetailEntry = lifecyclePage.getByRole('button', { name: /Open terminal|打开终端/ });
    assert(await pausedDetailEntry.isDisabled(), 'paused detail terminal entry remained enabled');
    await pausedDetailEntry.locator('..').hover();
    await lifecyclePage.getByText(/available only while the sandbox is running|仅在沙箱运行时可用/).waitFor();
    await lifecyclePage.goto(`${endpoint}/sandboxes`, { waitUntil: 'networkidle' });
    const pausedRow = lifecyclePage.locator(`a[href="/sandboxes/${sandboxId}"]`).locator('xpath=../..');
    const pausedListEntry = pausedRow.getByRole('button', { name: /Open terminal|打开终端/ });
    assert(await pausedListEntry.isDisabled(), 'paused list terminal entry remained enabled');
    await pausedListEntry.locator('..').hover();
    await lifecyclePage.getByText(/available only while the sandbox is running|仅在沙箱运行时可用/).waitFor();
    const nonRunningGrant = await issueGrant(lifecyclePage, sandboxId);
    assert(nonRunningGrant.status === 409, `non-running grant returned HTTP ${nonRunningGrant.status}`);
    assert(nonRunningGrant.body?.error === 'TARGET_NOT_RUNNING', 'non-running grant did not return TARGET_NOT_RUNNING');
    await lifecyclePage.screenshot({ path: path.join(screenshotDir, 'terminal-paused-tooltip.png'), animations: 'disabled' });
    await lifecyclePage.goto(`${endpoint}/sandboxes/${sandboxId}`, { waitUntil: 'networkidle' });
    await lifecyclePage.getByRole('button', { name: /Resume|恢复/ }).click();
    await lifecyclePage.getByRole('button', { name: /Pause|暂停/ }).waitFor({ timeout: 60_000 });
    await lifecyclePage.close();

    await page.getByRole('button', { name: /Close terminal|关闭终端/ }).click();
    await page.getByRole('dialog', { name: /Terminal|终端/ }).waitFor({ state: 'detached' });
    panel = await openFromDetail(page, sandboxId);
    const finalMetadata = await terminalMetadata(panel);
    validateSessionId(finalMetadata.sessionId);
    const resumeCountBeforeClose = grantRequests.filter((item) => item.kind === 'resume').length;
    await page.getByRole('button', { name: /Close terminal|关闭终端/ }).click();
    await page.getByRole('dialog', { name: /Terminal|终端/ }).waitFor({ state: 'detached' });
    await page.waitForTimeout(3_000);
    assert(grantRequests.filter((item) => item.kind === 'resume').length === resumeCountBeforeClose, 'user close triggered reconnect');

    const sessionIds = [
      initialMetadata.sessionId,
      secondMetadata.sessionId,
      replacementMetadata.sessionId,
      finalMetadata.sessionId,
    ];
    updateState((next) => {
      for (const sessionId of sessionIds) {
        if (!next.sessionIds.includes(sessionId)) next.sessionIds.push(sessionId);
      }
      next.primaryContainerId = initialMetadata.containerId;
      next.primaryHostname = hostname;
      next.nonRunningTargetEvidence = {
        sandboxId,
        status: nonRunningGrant.status,
        error: nonRunningGrant.body.error,
      };
      next.phases.core = { status: 'PASS', observedAt: new Date().toISOString() };
    });
    const result = {
      endpoint,
      sandboxId,
      listEntry: { visible: true, enabled: true },
      detailEntry: { visible: true, enabled: true },
      targetBinding: {
        hostname,
        expectedHostname,
        hostnameMatchesLiveTemplate: true,
        containerId: initialMetadata.containerId,
        sessionId: initialMetadata.sessionId,
      },
      commands: { lsColor: true, topRenderedSurfaceChanged: true, topCtrlC: true, pingRenderedSurfaceChanged: true, pingCtrlC: true, shellPidPreserved: true },
      resize: { desktopSize, narrowSize, fullscreenSize },
      layout: { desktop: desktopLayout, narrow: narrowLayout },
      tabs: { independent: true, closedOneKeptOther: true, sessionIds: sessionIds.slice(0, 2) },
      transportLoss: {
        realFirewallFault: true,
        durationMs: Date.now() - resumeStartedAt,
        sameSession: true,
        sameShellPid: true,
        replayObserved: true,
        positiveResumeOffsets: true,
      },
      lifecycle: {
        shellExitNoReconnect: true,
        userCloseNoReconnect: true,
        sandboxTransition: true,
        pausedListDisabled: true,
        pausedDetailDisabled: true,
        resumedNewTerminal: true,
      },
      containers: {
        selectorOptionCount: containerOptions.length,
        liveContainerOptionCount: availableContainerOptions.length,
        realMultiContainerAvailable,
        required: requireMultiContainer,
        firstContainerId: initialMetadata.containerId,
        secondContainerId: secondMetadata.containerId,
        firstHostname: hostname,
        secondHostname,
        primaryRoleMarker,
        sidecarRoleMarker,
        distinctContainerIds: realMultiContainerAvailable,
        status: realMultiContainerAvailable && (!requireMultiContainer || (primaryRoleMarker && sidecarRoleMarker))
          ? 'PASS_REAL_MULTI_CONTAINER'
          : 'SKIP_SINGLE_CONTAINER_TEMPLATE',
      },
      websocket: { handshakes: websocketHandshakes, responses: websocketResponses },
      counters,
      screenshots: [
        'terminal-desktop.png',
        'terminal-390x844.png',
        'terminal-fullscreen.png',
        'terminal-paused-tooltip.png',
      ],
      observedAt: new Date().toISOString(),
    };
    assert(websocketHandshakes.length >= 1, 'no terminal WebSocket handshake was observed');
    assert(websocketHandshakes.every((item) => item.queryEmpty && item.protocolCount === 2 && item.applicationProtocol && item.grantProtocolPresent && !item.authorizationPresent && !item.cookiePresent), 'terminal WebSocket credential placement invariant failed');
    assert(websocketResponses.some((item) => item.status === 101 && item.applicationProtocol), 'terminal WebSocket did not negotiate HTTP 101/application protocol');
    safeWriteJSON(path.join(browserDir, 'core.json'), result);
  } finally {
    if (transportBlocked) setTransportBlocked(false);
    await context.close();
    await browser.close();
  }
  process.stdout.write('core PASS\n');
}

async function issueGrant(page, sandboxId, kind = 'open', extra = {}) {
  const response = await authenticatedJSON(page, '/opsapi/v1/terminal/grants', {
    method: 'POST',
    body: JSON.stringify({ kind, sandboxId, cols: 80, rows: 24, ...extra }),
  });
  return response;
}

function normalizedWebSocketURL(rawURL) {
  const socketURL = new URL(rawURL, endpoint);
  const endpointURL = new URL(endpoint);
  socketURL.protocol = endpointURL.protocol === 'https:' ? 'wss:' : 'ws:';
  socketURL.host = endpointURL.host;
  socketURL.pathname = '/opsapi/v1/terminal/ws';
  socketURL.search = '';
  socketURL.hash = '';
  return socketURL.href;
}

async function attemptWebSocket(page, socketURL, oneTimeToken, action = 'close') {
  return page.evaluate(
    ({ url, token, socketAction }) =>
      new Promise((resolve) => {
        let settled = false;
        let opened = false;
        let errorObserved = false;
        const finish = (value) => {
          if (settled) return;
          settled = true;
          resolve(value);
        };
        const socket = new WebSocket(url, ['cube-terminal.v1', `cube-grant.${token}`]);
        socket.onopen = () => {
          opened = true;
          if (socketAction === 'oversized') {
            const frame = new Uint8Array(65_538);
            frame[0] = 0x00;
            socket.send(frame);
            return;
          }
          socket.close(1000, 'evidence probe');
        };
        socket.onerror = () => {
          errorObserved = true;
        };
        socket.onclose = (event) => finish({
          opened,
          outcome: opened ? 'closed' : 'rejected',
          closeCode: event.code,
          errorObserved,
        });
        setTimeout(() => finish({ opened, outcome: 'timeout', closeCode: null, errorObserved }), 15_000);
      }),
    { url: socketURL, token: oneTimeToken, socketAction: action },
  );
}

async function waitForSandboxState(page, sandboxId, expected, timeoutMs = 90_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const response = await authenticatedJSON(page, `/sandboxes/${sandboxId}`);
    if (response.status === 200) {
      const state = String(response.body?.state ?? response.body?.status ?? '').toLowerCase();
      if (expected.includes(state)) return state;
    }
    await page.waitForTimeout(1_000);
  }
  throw new Error(`sandbox ${sandboxId} did not reach expected state`);
}

async function security() {
  const state = readState();
  assert(state.createdSandboxIds.length >= 1, 'security phase requires a task sandbox');
  const sandboxId = validateSandboxId(state.createdSandboxIds[0]);
  const nonRunningSandboxId = sandboxId;
  const nonRunningEvidence = state.nonRunningTargetEvidence;
  assert(nonRunningEvidence?.sandboxId === sandboxId, 'security phase lacks the core non-running target binding');
  assert(nonRunningEvidence.status === 409 && nonRunningEvidence.error === 'TARGET_NOT_RUNNING', 'core non-running grant evidence is invalid');
  const probeStartedAt = new Date().toISOString();
  const journalSince = probeStartedAt.replace('T', ' ').replace(/\.\d{3}Z$/, ' UTC');
  const { browser, context } = await launchContext();
  const page = await context.newPage();
  const counters = countersFor(page);
  const consoleValues = [];
  const networkURLs = [];
  const grantTokens = [];
  page.on('console', (message) => consoleValues.push(message.text()));
  page.on('request', (request) => networkURLs.push(request.url()));

  try {
    await login(page);
    await waitForSandboxRunning(page, sandboxId);
    await waitForSandboxRunning(page, nonRunningSandboxId);

    const missingAuth = await page.evaluate(async (targetSandboxId) => {
      const response = await fetch('/opsapi/v1/terminal/grants', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ kind: 'open', sandboxId: targetSandboxId, cols: 80, rows: 24 }),
      });
      return response.status;
    }, sandboxId);
    assert(missingAuth === 401, `grant POST without authentication returned HTTP ${missingAuth}`);

    const normalGrant = await issueGrant(page, sandboxId);
    assert(normalGrant.status === 201, `normal grant returned HTTP ${normalGrant.status}`);
    let normalToken = normalGrant.body?.token ?? '';
    grantTokens.push(normalToken);
    const socketURL = normalizedWebSocketURL(normalGrant.body?.wsUrl ?? '/opsapi/v1/terminal/ws');
    normalGrant.body.token = '';
    const normal = await attemptWebSocket(page, socketURL, normalToken);
    assert(normal.opened && normal.closeCode === 1000, 'normal one-time grant did not open and close cleanly');
    const replay = await attemptWebSocket(page, socketURL, normalToken);
    assert(!replay.opened, 'consumed grant replay opened a WebSocket');

    let missingToken = crypto.randomBytes(16).toString('base64url');
    const missing = await attemptWebSocket(page, socketURL, missingToken);
    missingToken = '';
    assert(!missing.opened, 'unknown grant opened a WebSocket');

    const expiringGrant = await issueGrant(page, sandboxId);
    assert(expiringGrant.status === 201, `expiring grant returned HTTP ${expiringGrant.status}`);
    let expiringToken = expiringGrant.body?.token ?? '';
    grantTokens.push(expiringToken);
    const expiresAt = Date.parse(expiringGrant.body?.expiresAt ?? '');
    assert(Number.isFinite(expiresAt), 'grant expiry timestamp is invalid');
    expiringGrant.body.token = '';
    const expiryWaitMs = Math.max(0, expiresAt + 2_000 - Date.now());
    if (expiryWaitMs > 0) await page.waitForTimeout(expiryWaitMs);
    const expired = await attemptWebSocket(page, socketURL, expiringToken);
    expiringToken = '';
    assert(!expired.opened, 'expired grant opened a WebSocket');

    const forgedGrant = await issueGrant(page, sandboxId);
    assert(forgedGrant.status === 201, `forged-origin grant returned HTTP ${forgedGrant.status}`);
    let forgedToken = forgedGrant.body?.token ?? '';
    grantTokens.push(forgedToken);
    forgedGrant.body.token = '';
    const altPage = await context.newPage();
    await altPage.goto('http://127.0.0.1:12088/', { waitUntil: 'domcontentloaded' });
    const forgedOrigin = await attemptWebSocket(altPage, socketURL, forgedToken);
    await altPage.close();
    assert(!forgedOrigin.opened, 'forged Origin opened a WebSocket');
    const forgedCleanup = await attemptWebSocket(page, socketURL, forgedToken);
    assert(forgedCleanup.opened, 'same-origin cleanup did not consume the forged-origin grant');
    forgedToken = '';

    const oversizedGrant = await issueGrant(page, sandboxId);
    assert(oversizedGrant.status === 201, `oversized-frame grant returned HTTP ${oversizedGrant.status}`);
    let oversizedToken = oversizedGrant.body?.token ?? '';
    grantTokens.push(oversizedToken);
    oversizedGrant.body.token = '';
    const oversized = await attemptWebSocket(page, socketURL, oversizedToken, 'oversized');
    oversizedToken = '';
    assert(oversized.opened && oversized.closeCode === 1009, `oversized frame closed with ${oversized.closeCode}, expected 1009`);

    await page.waitForTimeout(1_000);
    const accessLog = spawnSync(
      'journalctl',
      ['-u', 'cube-sandbox-webui.service', '--since', journalSince, '--no-pager', '-o', 'cat'],
      { encoding: 'utf8' },
    );
    assert(accessLog.status === 0, 'bounded WebUI journal query failed');
    const statuses = [
      ...accessLog.stdout.matchAll(/"GET \/opsapi\/v1\/terminal\/ws HTTP\/1\.1" (\d{3})/g),
    ].map((match) => Number(match[1]));
    const observedStatuses = statuses.slice(-7);
    assert(
      observedStatuses.length === 7 &&
        observedStatuses[0] === 101 &&
        observedStatuses[1] === 401 &&
        observedStatuses[2] === 401 &&
        observedStatuses[3] === 401 &&
        observedStatuses[4] === 403 &&
        observedStatuses[5] === 101 &&
        observedStatuses[6] === 101,
      `bounded WebSocket status sequence was unexpected: ${observedStatuses.join(',')}`,
    );

    const exposure = await page.evaluate((tokens) => {
      const dom = document.documentElement.innerHTML;
      const localValues = Object.values(localStorage);
      const sessionValues = Object.values(sessionStorage);
      return {
        dom: tokens.some((token) => token && dom.includes(token)),
        localStorage: tokens.some((token) => token && localValues.some((value) => value.includes(token))),
        sessionStorage: tokens.some((token) => token && sessionValues.some((value) => value.includes(token))),
        currentURL: tokens.some((token) => token && location.href.includes(token)),
        localStorageKeys: Object.keys(localStorage).sort(),
        sessionStorageKeys: Object.keys(sessionStorage).sort(),
      };
    }, grantTokens);
    const networkURLExposure = grantTokens.some((token) => token && networkURLs.some((url) => url.includes(token)));
    const consoleExposure = grantTokens.some((token) => token && consoleValues.some((value) => value.includes(token)));
    assert(!exposure.dom && !exposure.localStorage && !exposure.sessionStorage && !exposure.currentURL && !networkURLExposure && !consoleExposure, 'grant value escaped the expected WebSocket subprotocol boundary');

    updateState((next) => {
      next.phases.security = { status: 'PASS', observedAt: new Date().toISOString() };
    });
    safeWriteJSON(path.join(browserDir, 'security.json'), {
      endpoint,
      sandboxId,
      unauthenticatedGrantPost: { status: missingAuth, result: 'PASS' },
      consumedGrant: { rejected: true, status: observedStatuses[1] },
      missingGrant: { rejected: true, status: observedStatuses[2] },
      expiredGrant: { rejected: true, status: observedStatuses[3], waitedPastExpiryMs: 2_000 },
      forgedOrigin: { rejected: true, status: observedStatuses[4], grantConsumedAfterProbe: true },
      oversizedFrame: { closeCode: oversized.closeCode, expected: 1009 },
      nonRunningTarget: {
        sandboxId: nonRunningSandboxId,
        status: nonRunningEvidence.status,
        error: nonRunningEvidence.error,
        capturedDuringCorePause: true,
      },
      boundedWebSocketStatuses: observedStatuses,
      grantExposure: {
        dom: exposure.dom,
        localStorage: exposure.localStorage,
        sessionStorage: exposure.sessionStorage,
        currentURL: exposure.currentURL,
        networkURL: networkURLExposure,
        console: consoleExposure,
      },
      browserStorageKeys: {
        local: exposure.localStorageKeys,
        session: exposure.sessionStorageKeys,
      },
      rawGrantPersisted: false,
      counters,
      observedAt: new Date().toISOString(),
    });
    normalToken = '';
    grantTokens.fill('');
    consoleValues.fill('');
    networkURLs.fill('');
  } finally {
    grantTokens.fill('');
    consoleValues.fill('');
    networkURLs.fill('');
    await context.close();
    await browser.close();
  }
  process.stdout.write('security PASS\n');
}

async function graceExpiry() {
  const state = readState();
  const sandboxId = validateSandboxId(state.createdSandboxIds[0]);
  const delayedResumeAtMs = 65_000;
  const { browser, context } = await launchContext();
  const page = await context.newPage();
  const counters = countersFor(page);
  const grantRequests = [];
  const grantResponses = [];
  const responseReaders = [];
  const states = [];
  let faultAt = 0;
  let resumeOrdinal = 0;
  let transportBlocked = false;

  page.on('request', (request) => {
    if (!request.url().includes('/opsapi/v1/terminal/grants')) return;
    const body = request.postDataJSON();
    grantRequests.push({
      at: Date.now(),
      kind: body.kind,
      sandboxId: body.sandboxId,
      containerId: body.containerId ?? '',
      sessionId: body.sessionId ?? '',
      lastOffset: body.lastOffset ?? null,
    });
  });
  page.on('response', (response) => {
    if (!response.url().includes('/opsapi/v1/terminal/grants')) return;
    const requestBody = response.request().postDataJSON();
    const entry = { kind: requestBody.kind, status: response.status(), error: '', at: Date.now() };
    grantResponses.push(entry);
    if (!response.ok()) {
      responseReaders.push(
        response.json().then((body) => {
          entry.error = typeof body?.error === 'string' ? body.error : '';
        }).catch(() => undefined),
      );
    }
  });
  await page.route('**/opsapi/v1/terminal/grants', async (route) => {
    const body = route.request().postDataJSON();
    if (body.kind === 'resume') {
      resumeOrdinal += 1;
      if (resumeOrdinal === 3 && faultAt > 0) {
        const delay = Math.max(0, faultAt + delayedResumeAtMs - Date.now());
        if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
      }
    }
    await route.continue();
  });

  try {
    await login(page);
    await waitForSandboxRunning(page, sandboxId);
    let panel = await openFromDetail(page, sandboxId);
    const beforeMetadata = await terminalMetadata(panel);
    validateSessionId(beforeMetadata.sessionId);
    await sendCommand(page, panel, `printf 'C7E_LOST_PID=%s\\n' "$$"`);
    let observed = await waitTerminalMatch(panel, /C7E_LOST_PID=(\d+)/);
    const beforePid = Number(observed.match[1]);

    faultAt = Date.now();
    setTransportBlocked(true);
    transportBlocked = true;
    await panel.locator('.xterm-helper-textarea').focus();
    await page.keyboard.press('Control+L');

    let networkRestored = false;
    const stateLabels = [
      'Reconnecting 1/3…',
      'Reconnecting 2/3…',
      'Reconnecting 3/3…',
      'The previous shell can no longer be resumed.',
    ];
    const deadline = Date.now() + 105_000;
    while (Date.now() < deadline) {
      const panelText = await panel.innerText().catch(() => '');
      const current = stateLabels.find((label) => panelText.includes(label));
      if (current && states.at(-1)?.state !== current) states.push({ state: current, at: Date.now() });
      if (!networkRestored && current === 'Reconnecting 3/3…') {
        setTransportBlocked(false);
        transportBlocked = false;
        networkRestored = true;
      }
      if (current === 'The previous shell can no longer be resumed.') break;
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    for (const expected of stateLabels) {
      assert(states.some((item) => item.state === expected), `grace-expiry UI state is missing: ${expected}`);
    }
    await Promise.all(responseReaders);
    const resumeRequests = grantRequests.filter((item) => item.kind === 'resume');
    const resumeResponses = grantResponses.filter((item) => item.kind === 'resume');
    assert(resumeRequests.length === 3, 'grace expiry did not issue exactly three resume requests');
    assert(resumeResponses.at(-1)?.status === 409, 'final expired resume grant did not return HTTP 409');
    assert(resumeResponses.at(-1)?.error === 'SESSION_LOST', 'final expired resume grant did not return SESSION_LOST');
    assert(
      resumeRequests.every(
        (item) =>
          item.sessionId === beforeMetadata.sessionId &&
          item.sandboxId === sandboxId &&
          item.containerId === beforeMetadata.containerId &&
          item.lastOffset > 0,
      ),
      'grace-expiry resume changed target/session or used a nonpositive offset',
    );
    await page.waitForTimeout(3_000);
    assert(grantRequests.filter((item) => item.kind === 'resume').length === 3, 'SESSION_LOST kept retrying');

    await panel.getByRole('button', { name: /Start new session|启动新会话/ }).click();
    await page.waitForFunction(() => document.querySelectorAll('[role="tab"]').length === 2);
    panel = await waitConnected(page, 'new-session-after-grace-expiry');
    const replacementMetadata = await terminalMetadata(panel);
    validateSessionId(replacementMetadata.sessionId);
    await sendCommand(page, panel, `printf 'C7E_AFTER_LOST_PID=%s\\n' "$$"`);
    observed = await waitTerminalMatch(panel, /C7E_AFTER_LOST_PID=(\d+)/);
    const afterPid = Number(observed.match[1]);
    assert(replacementMetadata.sessionId !== beforeMetadata.sessionId, 'new session reused the lost session ID');
    assert(afterPid !== beforePid, 'new session reused the lost shell process');

    updateState((next) => {
      for (const sessionId of [beforeMetadata.sessionId, replacementMetadata.sessionId]) {
        if (!next.sessionIds.includes(sessionId)) next.sessionIds.push(sessionId);
      }
      next.phases['grace-expiry'] = { status: 'PASS', observedAt: new Date().toISOString() };
    });
    safeWriteJSON(path.join(browserDir, 'grace-expiry.json'), {
      endpoint,
      sandboxId,
      original: {
        sessionId: beforeMetadata.sessionId,
        containerId: beforeMetadata.containerId,
        shellPid: beforePid,
      },
      uiStates: states.map((item) => ({ state: item.state, elapsedMs: item.at - faultAt })),
      resumeRequests: resumeRequests.map((item, index) => ({
        elapsedMs: item.at - faultAt,
        sessionBound: item.sessionId === beforeMetadata.sessionId,
        targetBound: item.sandboxId === sandboxId && item.containerId === beforeMetadata.containerId,
        positiveOffset: item.lastOffset > 0,
        finalStatus: index === resumeRequests.length - 1 ? resumeResponses.at(-1)?.status ?? null : null,
        finalError: index === resumeRequests.length - 1 ? resumeResponses.at(-1)?.error ?? '' : '',
      })),
      replacement: {
        sessionId: replacementMetadata.sessionId,
        shellPid: afterPid,
        sessionChanged: true,
        shellPidChanged: true,
      },
      retriesStopped: true,
      counters,
      observedAt: new Date().toISOString(),
    });
    await page.getByRole('button', { name: /Close terminal|关闭终端/ }).click();
    await page.getByRole('dialog', { name: /Terminal|终端/ }).waitFor({ state: 'detached' });
  } finally {
    if (transportBlocked) setTransportBlocked(false);
    await context.close();
    await browser.close();
  }
  process.stdout.write('grace-expiry PASS\n');
}

async function concurrency() {
  let state = readState();
  assert(state.createdSandboxIds.length >= 1, 'concurrency phase requires the primary task sandbox');
  const credentialPair = readCredentialPair();
  const browser = await launchBrowser();
  const contexts = [
    await browser.newContext({ viewport: { width: 1440, height: 900 } }),
    await browser.newContext({ viewport: { width: 1440, height: 900 } }),
  ];
  const pages = [
    await contexts[0].newPage(),
    await contexts[0].newPage(),
    await contexts[1].newPage(),
    await contexts[1].newPage(),
  ];
  const counters = pages.map((page) => countersFor(page));
  try {
    const primaryUsername = await login(pages[0], credentialFile);
    await login(pages[1], credentialFile);
    const secondaryUsername = await login(pages[2], secondaryCredentialFile);
    await login(pages[3], secondaryCredentialFile);
    assert(primaryUsername === credentialPair.primaryUsername, 'primary context identity changed after login');
    assert(secondaryUsername === credentialPair.secondaryUsername, 'secondary context identity changed after login');
    assert(primaryUsername !== secondaryUsername, 'concurrency contexts are not authenticated as distinct users');
    if (state.createdSandboxIds.length < 2) {
      const response = await authenticatedJSON(pages[0], '/sandboxes', {
        method: 'POST',
        body: JSON.stringify({
          templateID: state.templateId,
          timeout: 7200,
          metadata: {
            'cube-terminal-evidence-task': taskId,
            'cube-terminal-evidence-ordinal': '2',
          },
        }),
      });
      assert(response.status >= 200 && response.status < 300, `second sandbox create returned HTTP ${response.status}`);
      const secondSandboxId = response.body?.sandboxID ?? response.body?.sandbox_id ?? '';
      validateSandboxId(secondSandboxId);
      state = updateState((next) => {
        if (!next.createdSandboxIds.includes(secondSandboxId)) next.createdSandboxIds.push(secondSandboxId);
      });
      await waitForSandboxRunning(pages[0], secondSandboxId);
    }
    const sandboxIds = state.createdSandboxIds.slice(0, 2).map(validateSandboxId);
    assert(sandboxIds.length === 2, 'concurrency phase did not create two task sandboxes');
    const assignments = [sandboxIds[0], sandboxIds[1], sandboxIds[0], sandboxIds[1]];
    const assignedUsers = [primaryUsername, primaryUsername, secondaryUsername, secondaryUsername];
    for (const sandboxId of sandboxIds) await waitForSandboxRunning(pages[0], sandboxId);

    const panels = await Promise.all(
      pages.map((page, index) => openFromDetail(page, assignments[index])),
    );
    const metadata = await Promise.all(panels.map((panel) => terminalMetadata(panel)));
    metadata.forEach((item) => validateSessionId(item.sessionId));
    assert(new Set(metadata.map((item) => item.sessionId)).size === 4, 'parallel sessions did not receive four unique IDs');

    await Promise.all(
      panels.map((panel, index) =>
        sendCommand(
          pages[index],
          panel,
          `printf 'C7E_PARALLEL_${index}_PID=%s\\nC7E_PARALLEL_${index}_ONLY\\n' "$$"`,
        ),
      ),
    );
    const observed = await Promise.all(
      panels.map((panel, index) => waitTerminalMatch(panel, new RegExp(`C7E_PARALLEL_${index}_PID=(\\d+)`))),
    );
    const pids = observed.map((item) => Number(item.match[1]));
    assert(new Set(pids).size === 4, 'parallel sessions shared a shell process');
    for (let index = 0; index < panels.length; index += 1) {
      const text = await terminalText(panels[index]);
      for (let other = 0; other < panels.length; other += 1) {
        if (index === other) {
          assert(text.includes(`C7E_PARALLEL_${other}_ONLY`), 'parallel session did not render its own marker');
        } else {
          assert(!text.includes(`C7E_PARALLEL_${other}_ONLY`), 'parallel session output crossed a session boundary');
        }
      }
    }

    await pages[0].getByRole('button', { name: /Close terminal|关闭终端/ }).click();
    await pages[0].getByRole('dialog', { name: /Terminal|终端/ }).waitFor({ state: 'detached' });
    for (let index = 1; index < panels.length; index += 1) {
      await sendCommand(pages[index], panels[index], `printf 'C7E_PARALLEL_${index}_STILL=%s\\n' "$$"`);
      const live = await waitTerminalMatch(panels[index], new RegExp(`C7E_PARALLEL_${index}_STILL=(\\d+)`));
      assert(Number(live.match[1]) === pids[index], 'closing one parallel session changed another shell');
    }

    for (let index = 1; index < pages.length; index += 1) {
      await pages[index].getByRole('button', { name: /Close terminal|关闭终端/ }).click();
      await pages[index].getByRole('dialog', { name: /Terminal|终端/ }).waitFor({ state: 'detached' });
    }

    const secondaryDelete = await authenticatedJSON(pages[0], `/sandboxes/${sandboxIds[1]}`, { method: 'DELETE' });
    assert(secondaryDelete.status >= 200 && secondaryDelete.status < 300, `second sandbox cleanup returned HTTP ${secondaryDelete.status}`);
    let secondaryAbsent = false;
    const secondaryDeadline = Date.now() + 30_000;
    while (Date.now() < secondaryDeadline) {
      const check = await authenticatedJSON(pages[0], `/sandboxes/${sandboxIds[1]}`);
      if (check.status === 404) {
        secondaryAbsent = true;
        break;
      }
      await pages[0].waitForTimeout(1_000);
    }
    assert(secondaryAbsent, 'second sandbox remained after the concurrency phase');

    updateState((next) => {
      for (const item of metadata) {
        if (!next.sessionIds.includes(item.sessionId)) next.sessionIds.push(item.sessionId);
      }
      next.authenticatedUsers = [primaryUsername, secondaryUsername];
      next.concurrencySessions = metadata.map((item, index) => ({
        username: assignedUsers[index],
        sessionId: item.sessionId,
        sandboxId: assignments[index],
        containerId: item.containerId,
        shellPid: pids[index],
      }));
      next.phases.concurrency = { status: 'PASS', observedAt: new Date().toISOString() };
    });
    safeWriteJSON(path.join(browserDir, 'concurrency.json'), {
      endpoint,
      requestedMatrix: { users: 2, instances: 2, sessions: 4 },
      executedMatrix: { users: 2, instances: 2, sessions: 4 },
      status: 'PASS',
      isolatedBrowserContexts: 2,
      authenticatedUsers: [primaryUsername, secondaryUsername],
      distinctAuthenticatedUsers: true,
      sandboxIds,
      sessions: metadata.map((item, index) => ({
        username: assignedUsers[index],
        sessionId: item.sessionId,
        sandboxId: assignments[index],
        containerId: item.containerId,
        shellPid: pids[index],
      })),
      distinctSessionIds: true,
      distinctShellPids: true,
      outputIsolation: true,
      closeIsolation: true,
      secondarySandboxCleanup: {
        sandboxId: sandboxIds[1],
        deleteStatus: secondaryDelete.status,
        absent: secondaryAbsent,
      },
      counters,
      observedAt: new Date().toISOString(),
    });
  } finally {
    await Promise.all(contexts.map((context) => context.close()));
    await browser.close();
  }
  process.stdout.write('concurrency PASS: two real users x two instances x four isolated sessions\n');
}

async function idle() {
  const state = readState();
  const sandboxId = validateSandboxId(state.createdSandboxIds[0]);
  const { browser, context } = await launchContext();
  const page = await context.newPage();
  const counters = countersFor(page);
  const grantKinds = [];
  page.on('request', (request) => {
    if (!request.url().includes('/opsapi/v1/terminal/grants')) return;
    grantKinds.push(request.postDataJSON()?.kind ?? 'unknown');
  });
  try {
    await login(page);
    await waitForSandboxRunning(page, sandboxId);
    const openedAt = Date.now();
    const panel = await openFromDetail(page, sandboxId);
    const metadata = await terminalMetadata(panel);
    validateSessionId(metadata.sessionId);
    await panel.getByText(/terminal closed after being idle for too long|终端因长时间没有输入而关闭/).waitFor({
      timeout: 95_000,
    });
    const elapsedMs = Date.now() - openedAt;
    assert(elapsedMs >= 55_000 && elapsedMs < 95_000, `idle close occurred outside the one-minute window: ${elapsedMs}ms`);
    await page.waitForTimeout(3_000);
    assert(grantKinds.filter((kind) => kind === 'resume').length === 0, 'idle timeout triggered reconnect');
    updateState((next) => {
      if (!next.sessionIds.includes(metadata.sessionId)) next.sessionIds.push(metadata.sessionId);
      next.phases.idle = { status: 'PASS', observedAt: new Date().toISOString() };
    });
    safeWriteJSON(path.join(browserDir, 'idle.json'), {
      endpoint,
      sandboxId,
      sessionId: metadata.sessionId,
      containerId: metadata.containerId,
      configuredIdleMinutes: 1,
      noStdinAfterOpen: true,
      closeReason: 'IDLE_TIMEOUT',
      elapsedMs,
      reconnectAttempted: false,
      counters,
      observedAt: new Date().toISOString(),
    });
    await page.getByRole('button', { name: /Close terminal|关闭终端/ }).click();
    await page.getByRole('dialog', { name: /Terminal|终端/ }).waitFor({ state: 'detached' });
  } finally {
    await context.close();
    await browser.close();
  }
  process.stdout.write('idle PASS\n');
}

function systemdMainPID(unit) {
  const result = spawnSync('systemctl', ['show', unit, '-p', 'MainPID', '--value'], { encoding: 'utf8' });
  assert(result.status === 0, `unable to read MainPID for ${unit}`);
  const pid = Number(result.stdout.trim());
  assert(Number.isInteger(pid) && pid > 0, `invalid MainPID for ${unit}`);
  return pid;
}

async function waitProcess(child) {
  return new Promise((resolve, reject) => {
    child.once('error', reject);
    child.once('exit', (code, signal) => resolve({ code, signal }));
  });
}

async function drain() {
  const state = readState();
  const sandboxId = validateSandboxId(state.createdSandboxIds[0]);
  const beforePids = {
    cubeops: systemdMainPID('cube-sandbox-cubeops.service'),
    cubemaster: systemdMainPID('cube-sandbox-cubemaster.service'),
    cubelet: systemdMainPID('cube-sandbox-cubelet.service'),
    networkAgent: systemdMainPID('cube-sandbox-network-agent.service'),
  };
  const { browser, context } = await launchContext();
  const page = await context.newPage();
  const counters = countersFor(page);
  try {
    await login(page);
    await waitForSandboxRunning(page, sandboxId);
    let panel = await openFromDetail(page, sandboxId);
    const beforeMetadata = await terminalMetadata(panel);
    validateSessionId(beforeMetadata.sessionId);
    await sendCommand(page, panel, `printf 'C7E_DRAIN_READY=%s\\n' "$$"`);
    const ready = await waitTerminalMatch(panel, /C7E_DRAIN_READY=(\d+)/);
    const shellPid = Number(ready.match[1]);

    const restartStartedAt = Date.now();
    const restartChild = spawn(
      'sudo',
      ['-n', 'systemctl', 'restart', 'cube-sandbox-cubeops.service'],
      { stdio: 'ignore' },
    );
    const restartPromise = waitProcess(restartChild);
    await panel.getByText(/terminal service is restarting|终端服务正在重启/).waitFor({ timeout: 45_000 });
    const noticeElapsedMs = Date.now() - restartStartedAt;
    const restartResult = await restartPromise;
    assert(restartResult.code === 0 && restartResult.signal === null, 'CubeOps rolling restart command failed');

    const healthDeadline = Date.now() + 45_000;
    let restStatus = 0;
    while (Date.now() < healthDeadline) {
      const response = await authenticatedJSON(page, '/opsapi/v1/auth/session');
      restStatus = response.status;
      if (restStatus === 200) break;
      await page.waitForTimeout(1_000);
    }
    assert(restStatus === 200, `ordinary authenticated REST did not recover after drain: HTTP ${restStatus}`);
    const rootResponse = await context.request.get(`${endpoint}/`);
    assert(rootResponse.status() === 200, `WebUI root did not recover after drain: HTTP ${rootResponse.status()}`);
    const afterPids = {
      cubeops: systemdMainPID('cube-sandbox-cubeops.service'),
      cubemaster: systemdMainPID('cube-sandbox-cubemaster.service'),
      cubelet: systemdMainPID('cube-sandbox-cubelet.service'),
      networkAgent: systemdMainPID('cube-sandbox-network-agent.service'),
    };
    assert(afterPids.cubeops !== beforePids.cubeops, 'CubeOps PID did not change during the rolling restart');
    assert(afterPids.cubemaster === beforePids.cubemaster, 'CubeMaster was restarted during CubeOps drain');
    assert(afterPids.cubelet === beforePids.cubelet, 'Cubelet was restarted during CubeOps drain');
    assert(afterPids.networkAgent === beforePids.networkAgent, 'network-agent was restarted during CubeOps drain');

    await page.getByRole('button', { name: /Close terminal|关闭终端/ }).click();
    await page.getByRole('dialog', { name: /Terminal|终端/ }).waitFor({ state: 'detached' });
    panel = await openFromDetail(page, sandboxId);
    const replacementMetadata = await terminalMetadata(panel);
    validateSessionId(replacementMetadata.sessionId);
    await sendCommand(page, panel, `printf 'C7E_AFTER_DRAIN=%s\\n' "$$"`);
    await waitTerminalMatch(panel, /C7E_AFTER_DRAIN=\d+/);
    await page.getByRole('button', { name: /Close terminal|关闭终端/ }).click();
    await page.getByRole('dialog', { name: /Terminal|终端/ }).waitFor({ state: 'detached' });

    updateState((next) => {
      for (const sessionId of [beforeMetadata.sessionId, replacementMetadata.sessionId]) {
        if (!next.sessionIds.includes(sessionId)) next.sessionIds.push(sessionId);
      }
      next.phases.drain = { status: 'PASS', observedAt: new Date().toISOString() };
    });
    safeWriteJSON(path.join(browserDir, 'drain.json'), {
      endpoint,
      sandboxId,
      drainedSession: {
        sessionId: beforeMetadata.sessionId,
        containerId: beforeMetadata.containerId,
        shellPid,
        closeReason: 'SERVER_DRAINING',
      },
      replacementSessionId: replacementMetadata.sessionId,
      noticeElapsedMs,
      restartElapsedMs: Date.now() - restartStartedAt,
      beforePids,
      afterPids,
      onlyCubeOpsRestarted: true,
      webUIStatus: rootResponse.status(),
      ordinaryRESTStatus: restStatus,
      newTerminalAfterRestart: true,
      counters,
      observedAt: new Date().toISOString(),
    });
  } finally {
    await context.close();
    await browser.close();
  }
  process.stdout.write('drain PASS\n');
}

function mysqlQuery(sql) {
  assert(!/token_hash/i.test(sql), 'audit query must never reference token_hash');
  const containerScript = [
    'set -eu',
    'umask 077',
    'defaults_file=$(mktemp /tmp/c7e-mysql.XXXXXX)',
    'trap \'rm -f -- "$defaults_file"\' EXIT HUP INT TERM',
    'printf "[client]\\nuser=%s\\npassword=%s\\n" "${MYSQL_USER:?}" "${MYSQL_PASSWORD:?}" >"$defaults_file"',
    'mysql --defaults-extra-file="$defaults_file" --protocol=socket --database="${MYSQL_DATABASE:?}" --batch --skip-column-names --raw',
  ].join('; ');
  const result = spawnSync(
    'sudo',
    ['-n', 'docker', 'exec', '-i', 'cube-sandbox-mysql', 'sh', '-c', containerScript],
    { encoding: 'utf8', input: `${sql}\n`, maxBuffer: 4 * 1024 * 1024 },
  );
  assert(result.status === 0, `count/metadata-only MySQL query failed: exit ${result.status}`);
  return result.stdout.trim();
}

function parseTSV(text, columns) {
  if (!text) return [];
  return text.split('\n').filter(Boolean).map((line) => {
    const values = line.split('\t');
    const row = {};
    columns.forEach((column, index) => {
      row[column] = values[index] ?? '';
    });
    return row;
  });
}

async function waitForTaskSessionsClosed(quotedSandboxes, timeoutMs = 120_000) {
  const startedAt = Date.now();
  let openRows = Number.NaN;
  do {
    openRows = Number(mysqlQuery(
      'SELECT COUNT(*) FROM terminal_sessions WHERE sandbox_id IN (' + quotedSandboxes + ') AND closed_at IS NULL',
    ));
    assert(Number.isInteger(openRows) && openRows >= 0, 'task open-session count is invalid');
    if (openRows === 0) {
      return { elapsedMs: Date.now() - startedAt, openRows: 0 };
    }
    await new Promise((resolve) => setTimeout(resolve, 1_000));
  } while (Date.now() - startedAt < timeoutMs);
  throw new Error('task terminal audit rows did not close within ' + timeoutMs + 'ms: open=' + openRows);
}

function boundedJournal(unit, since) {
  const result = spawnSync(
    'journalctl',
    ['-u', unit, '--since', since, '--no-pager', '-o', 'cat'],
    { encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 },
  );
  assert(result.status === 0, `bounded journal query failed for ${unit}`);
  return result.stdout;
}

function boundedRuntimeLog(logPath, sinceISO) {
  const sinceSecond = sinceISO.slice(0, 19);
  const script = [
    '{',
    '  line=$0',
    '  timestamp=line',
    '  if (sub(/^.*"@timestamp":"/, "", timestamp) == 0) next',
    '  sub(/".*$/, "", timestamp)',
    '  if (substr(timestamp, 1, 19) >= since) print line',
    '}',
  ].join('\n');
  const result = spawnSync(
    'sudo',
    ['-n', 'awk', '-v', `since=${sinceSecond}`, script, logPath],
    { encoding: 'utf8', maxBuffer: 32 * 1024 * 1024 },
  );
  assert(result.status === 0, `bounded runtime log query failed for ${logPath}`);
  return result.stdout;
}

function countOccurrences(text, value) {
  if (!value) return 0;
  let count = 0;
  let offset = 0;
  while ((offset = text.indexOf(value, offset)) !== -1) {
    count += 1;
    offset += value.length;
  }
  return count;
}

function exactInternalTokenLogCount(sinceISO) {
  const script = String.raw`
import json
import subprocess
import sys
from datetime import datetime, timezone

since = datetime.fromisoformat(sys.argv[1].replace('Z', '+00:00'))
token = open('/usr/local/services/cubetoolbox/.terminal-internal-token', 'rb').read().strip()
count = 0
for log_path in (
    '/data/log/CubeOps/cubeops-req.log',
    '/data/log/CubeMaster/cubemaster-req.log',
    '/data/log/Cubelet/Cubelet-req.log',
):
    with open(log_path, 'rb') as stream:
        for line in stream:
            try:
                timestamp = json.loads(line).get('@timestamp', '')
                observed = datetime.fromisoformat(timestamp.replace('Z', '+00:00'))
            except Exception:
                continue
            if observed >= since and token and token in line:
                count += 1
since_journal = since.astimezone(timezone.utc).strftime('%Y-%m-%d %H:%M:%S UTC')
for unit in (
    'cube-sandbox-cubeops.service',
    'cube-sandbox-cubemaster.service',
    'cube-sandbox-cubelet.service',
    'cube-sandbox-webui.service',
):
    result = subprocess.run(
        ['journalctl', '-u', unit, '--since', since_journal, '--no-pager', '-o', 'cat'],
        check=True,
        stdout=subprocess.PIPE,
    )
    if token:
        count += result.stdout.count(token)
token = b''
print(count)
`;
  const result = spawnSync('sudo', ['-n', 'python3', '-c', script, sinceISO], { encoding: 'utf8' });
  assert(result.status === 0, 'exact internal-token bounded log scan failed');
  const count = Number(result.stdout.trim());
  assert(Number.isInteger(count) && count >= 0, 'internal-token scan count is invalid');
  return count;
}

async function auditCorrelation() {
  const state = readState();
  const sandboxIds = state.createdSandboxIds.map(validateSandboxId);
  const sessionIds = [...new Set(state.sessionIds.map(validateSessionId))];
  assert(sandboxIds.length === 2 && sessionIds.length > 0, 'audit phase lacks task-owned IDs');
  const quotedSandboxes = sandboxIds.map((value) => `'${value}'`).join(',');
  const sessionColumns = [
    'id',
    'userId',
    'sandboxId',
    'containerId',
    'cubeletHost',
    'openedAt',
    'lastSeenAt',
    'closedAt',
    'closeReason',
    'exitCode',
    'bytesIn',
    'bytesOut',
    'resumeCount',
  ];
  const sessionSQL = `SELECT id,user_id,sandbox_id,container_id,cubelet_host,DATE_FORMAT(opened_at,'%Y-%m-%dT%H:%i:%sZ'),DATE_FORMAT(last_seen_at,'%Y-%m-%dT%H:%i:%sZ'),COALESCE(DATE_FORMAT(closed_at,'%Y-%m-%dT%H:%i:%sZ'),''),COALESCE(close_reason,''),COALESCE(exit_code,''),bytes_in,bytes_out,resume_count FROM terminal_sessions WHERE sandbox_id IN (${quotedSandboxes}) ORDER BY opened_at,id`;
  const grantColumns = [
    'id',
    'kind',
    'userId',
    'sandboxId',
    'containerId',
    'sessionId',
    'cols',
    'rows',
    'resumeOffset',
    'createdAt',
    'expiresAt',
    'consumed',
  ];
  const grantSQL = `SELECT id,kind,user_id,sandbox_id,container_id,COALESCE(session_id,''),\`cols\`,\`rows\`,resume_offset,DATE_FORMAT(created_at,'%Y-%m-%dT%H:%i:%sZ'),DATE_FORMAT(expires_at,'%Y-%m-%dT%H:%i:%sZ'),CASE WHEN consumed_at IS NULL THEN 'false' ELSE 'true' END FROM terminal_grants WHERE sandbox_id IN (${quotedSandboxes}) ORDER BY created_at,id`;
  const closeConvergence = await waitForTaskSessionsClosed(quotedSandboxes);
  const sessions = parseTSV(mysqlQuery(sessionSQL), sessionColumns);
  const grants = parseTSV(mysqlQuery(grantSQL), grantColumns);
  assert(sessions.length >= sessionIds.length, 'audit store is missing recorded UI sessions');
  const storedSessionIds = new Set(sessions.map((row) => row.id));
  assert(sessionIds.every((value) => storedSessionIds.has(value)), 'UI session ID is missing from CubeOps audit store');
  assert(sessions.every((row) => row.closedAt && row.closeReason), 'task terminal audit row remained open or lacked close reason');
  assert(sessions.every((row) => Number(row.bytesIn) >= 0 && Number(row.bytesOut) >= 0 && Number(row.resumeCount) >= 0), 'terminal audit counters are invalid');

  const expectedConcurrency = Array.isArray(state.concurrencySessions) ? state.concurrencySessions : [];
  assert(expectedConcurrency.length === 4, 'audit phase lacks the four-session concurrency mapping');
  assert(new Set(expectedConcurrency.map((item) => item.username)).size === 2, 'audit phase lacks two distinct concurrency users');
  const sessionRowsByID = new Map(sessions.map((row) => [row.id, row]));
  const concurrencyUserMapping = expectedConcurrency.map((expected) => {
    const row = sessionRowsByID.get(expected.sessionId);
    assert(row, `concurrency session is missing from the audit store: ${expected.sessionId}`);
    assert(row.userId === expected.username, `audit user mismatch for concurrency session ${expected.sessionId}`);
    assert(row.sandboxId === expected.sandboxId, `audit sandbox mismatch for concurrency session ${expected.sessionId}`);
    assert(row.containerId === expected.containerId, `audit container mismatch for concurrency session ${expected.sessionId}`);
    return {
      username: expected.username,
      sessionId: expected.sessionId,
      sandboxId: expected.sandboxId,
      containerId: expected.containerId,
      userMatches: true,
      targetMatches: true,
    };
  });
  safeWriteJSON(path.join(summaryDir, 'audit.json'), {
    selectedColumns: sessionColumns,
    sensitiveColumnsSelected: 0,
    sessions,
    grantSelectedColumns: grantColumns,
    grants,
    closeConvergence,
    concurrencyUserMapping,
    distinctConcurrencyUsers: 2,
    payloadFree: true,
    sensitiveFieldsOmitted: true,
    observedAt: new Date().toISOString(),
  });
  updateState((next) => {
    next.phases['audit-database'] = { status: 'PASS', observedAt: new Date().toISOString() };
  });

  const sinceISO = fs.readFileSync(startedAtPath, 'utf8').trim();
  const sinceJournal = sinceISO.replace('T', ' ').replace('Z', ' UTC');
  const logs = {
    cubeops: boundedRuntimeLog('/data/log/CubeOps/cubeops-req.log', sinceISO),
    cubemaster: boundedRuntimeLog('/data/log/CubeMaster/cubemaster-req.log', sinceISO),
    cubelet: boundedRuntimeLog('/data/log/Cubelet/Cubelet-req.log', sinceISO),
    webui: boundedJournal('cube-sandbox-webui.service', sinceJournal),
  };
  const preferredSessionId = readJSON(path.join(browserDir, 'core.json')).targetBinding.sessionId;
  validateSessionId(preferredSessionId);
  const execId = `cubelet-term-${preferredSessionId.slice(0, 12)}`;
  const correlation = {
    sessionId: preferredSessionId,
    execId,
    ui: 1,
    cubeOpsLogMatches: countOccurrences(logs.cubeops, preferredSessionId),
    cubeMasterLogMatches: countOccurrences(logs.cubemaster, preferredSessionId),
    cubeletLogMatches: countOccurrences(logs.cubelet, preferredSessionId),
    cubeletExecIdMatches: countOccurrences(logs.cubelet, execId),
    databaseRows: sessions.filter((row) => row.id === preferredSessionId).length,
  };

  const patterns = {
    grantSubprotocol: /cube-grant\./g,
    authorization: /Authorization\s*:/gi,
    bearer: /Bearer\s+[A-Za-z0-9._~-]+/g,
    cookie: /Cookie\s*:/gi,
    jwtLike: /eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g,
    tokenHashName: /token_hash/gi,
    terminalMarker: /C7E_/g,
  };
  const sensitiveCounts = {};
  for (const [name, pattern] of Object.entries(patterns)) {
    sensitiveCounts[name] = Object.values(logs).reduce(
      (sum, text) => sum + (text.match(pattern) ?? []).length,
      0,
    );
  }
  sensitiveCounts.exactInternalToken = exactInternalTokenLogCount(sinceISO);
  safeWriteJSON(path.join(outputDir, 'raw', 'audit-correlation-attempt.json'), {
    ...correlation,
    sources: {
      cubeops: '/data/log/CubeOps/cubeops-req.log',
      cubemaster: '/data/log/CubeMaster/cubemaster-req.log',
      cubelet: '/data/log/Cubelet/Cubelet-req.log',
      webui: 'journal: cube-sandbox-webui.service',
    },
    checks: {
      cubeOps: correlation.cubeOpsLogMatches > 0,
      cubeMaster: correlation.cubeMasterLogMatches > 0,
      cubeletSession: correlation.cubeletLogMatches > 0,
      cubeletExecID: correlation.cubeletExecIdMatches > 0,
      database: correlation.databaseRows === 1,
    },
    rawLogLinesRecorded: false,
    observedAt: new Date().toISOString(),
  });
  safeWriteJSON(path.join(summaryDir, 'sensitive-log-scan.json'), {
    since: sinceISO,
    services: Object.keys(logs),
    countsOnly: true,
    counts: sensitiveCounts,
    result: Object.values(sensitiveCounts).every((count) => count === 0) ? 'PASS' : 'FAIL',
    observedAt: new Date().toISOString(),
  });
  assert(Object.values(sensitiveCounts).every((count) => count === 0), `bounded journal sensitive/payload counts were nonzero: ${JSON.stringify(sensitiveCounts)}`);
  assert(correlation.cubeOpsLogMatches > 0, 'five-point correlation lacks CubeOps session evidence');
  assert(correlation.cubeMasterLogMatches > 0, 'five-point correlation lacks CubeMaster session evidence');
  assert(correlation.cubeletLogMatches > 0, 'five-point correlation lacks Cubelet session evidence');
  assert(correlation.cubeletExecIdMatches > 0, 'five-point correlation lacks Cubelet/containerd exec ID evidence');
  assert(correlation.databaseRows === 1, 'five-point correlation lacks the unique audit row');

  updateState((next) => {
    next.phases['audit-correlation'] = { status: 'PASS', observedAt: new Date().toISOString() };
  });
  safeWriteJSON(path.join(summaryDir, 'correlation.json'), {
    ...correlation,
    points: ['WebUI', 'CubeOps', 'CubeMaster', 'Cubelet', 'containerd execID/audit store'],
    result: 'PASS',
    observedAt: new Date().toISOString(),
  });
  for (const key of Object.keys(logs)) logs[key] = '';
  process.stdout.write('audit-correlation PASS\n');
}

async function verifyCleanup() {
  const state = readState();
  const sandboxIds = state.createdSandboxIds.map(validateSandboxId);
  const sessionIds = [...new Set(state.sessionIds.map(validateSessionId))];
  const resourceIds = [
    ...sandboxIds,
    ...sessionIds,
    ...sessionIds.map((sessionId) => `cubelet-term-${sessionId.slice(0, 12)}`),
  ];
  const baselineResources = state.baselineResources;
  assert(baselineResources && typeof baselineResources === 'object', 'cleanup verification lacks the pre-run resource baseline');
  const { browser, context } = await launchContext();
  const page = await context.newPage();
  try {
    await login(page);
    const sandboxChecks = [];
    for (const sandboxId of sandboxIds) {
      const check = await authenticatedJSON(page, `/sandboxes/${sandboxId}`);
      sandboxChecks.push({ sandboxId, status: check.status, absent: check.status === 404 });
      assert(check.status === 404, `task sandbox still exists after cleanup: ${sandboxId}`);
    }
    const quotedSandboxes = sandboxIds.map((value) => `'${value}'`).join(',');
    const databaseCounts = parseTSV(
      mysqlQuery(`SELECT CONCAT('open_sessions=',COUNT(*)) FROM terminal_sessions WHERE sandbox_id IN (${quotedSandboxes}) AND closed_at IS NULL; SELECT CONCAT('live_grants=',COUNT(*)) FROM terminal_grants WHERE sandbox_id IN (${quotedSandboxes}) AND consumed_at IS NULL AND expires_at > UTC_TIMESTAMP()`),
      ['value'],
    ).map((row) => row.value);
    assert(databaseCounts.includes('open_sessions=0'), 'task terminal session remained open');
    assert(databaseCounts.includes('live_grants=0'), 'task terminal grant remained live');

    const finalResources = runtimeResourceSnapshot(resourceIds, sandboxIds);
    assert(finalResources.taskJournalFiles === 0 && finalResources.taskFIFOs === 0, 'task terminal journal/FIFO residue remained');
    assert(finalResources.taskContainerdTasks === 0 && finalResources.taskContainerdContainers === 0, 'task containerd residue remained');
    assert(finalResources.networkAgent.taskOwned === 0, 'task network-agent record remained');
    for (const key of ['terminalJournalFiles', 'terminalFIFOs', 'containerdTasks', 'containerdContainers']) {
      assert(finalResources[key] === baselineResources[key], `${key} did not return to its pre-run baseline`);
    }
    assert(finalResources.networkAgent.total === baselineResources.networkAgent.total, 'network-agent count did not return to its pre-run baseline');
    const goroutineDelta = finalResources.cubeletGoroutines - baselineResources.cubeletGoroutines;
    assert(goroutineDelta <= 10, `Cubelet goroutines remained above the bounded pre-run baseline: delta=${goroutineDelta}`);
    const runtimePathMatches = taskRuntimePathCount(resourceIds);
    assert(runtimePathMatches === 0, 'task runtime path remained after cleanup');

    let processResidue = 0;
    for (const entry of fs.readdirSync('/proc')) {
      if (!/^\d+$/.test(entry)) continue;
      try {
        const commandLine = fs.readFileSync(`/proc/${entry}/cmdline`, 'utf8');
        if (sandboxIds.some((sandboxId) => commandLine.includes(sandboxId))) processResidue += 1;
      } catch {
        // A process may exit between readdir and read; it is not residue.
      }
    }
    assert(processResidue === 0, 'task sandbox ID remained in a process command line');

    for (const unit of [
      'cube-sandbox-cubeops.service',
      'cube-sandbox-cubemaster.service',
      'cube-sandbox-cubelet.service',
      'cube-sandbox-network-agent.service',
      'cube-sandbox-webui.service',
      'cube-sandbox-mysql.service',
    ]) {
      const active = spawnSync('systemctl', ['is-active', '--quiet', unit]);
      assert(active.status === 0, `required service is not active: ${unit}`);
    }
    const health = await context.request.get(`${endpoint}/`);
    assert(health.status() === 200, `WebUI root returned HTTP ${health.status()}`);
    const rest = await authenticatedJSON(page, '/opsapi/v1/auth/session');
    assert(rest.status === 200, `ordinary REST returned HTTP ${rest.status}`);
    const oldResidual = await authenticatedJSON(page, '/sandboxes/941a61a65d694136a8f3de743177a2cd');
    const oldResidualState = oldResidual.status === 200
      ? String(oldResidual.body?.state ?? oldResidual.body?.status ?? '').toLowerCase()
      : `HTTP_${oldResidual.status}`;
    const coreDNS = spawnSync(
      'systemctl',
      ['show', 'cube-sandbox-coredns.service', '-p', 'ActiveState', '-p', 'SubState', '-p', 'NRestarts', '--no-pager'],
      { encoding: 'utf8' },
    );
    assert(coreDNS.status === 0, 'CoreDNS residual state query failed');

    updateState((next) => {
      next.phases['verify-cleanup'] = { status: 'PASS', observedAt: new Date().toISOString() };
    });
    safeWriteJSON(path.join(summaryDir, 'cleanup-health.json'), {
      exactTaskSandboxChecks: sandboxChecks,
      databaseCounts,
      baselineResources,
      finalResources,
      baselineComparison: {
        terminalJournalFiles: true,
        terminalFIFOs: true,
        containerdTasks: true,
        containerdContainers: true,
        networkAgent: true,
        cubeletGoroutineDelta: finalResources.cubeletGoroutines - baselineResources.cubeletGoroutines,
      },
      taskRuntimePathMatches: runtimePathMatches,
      processResidue,
      webUIStatus: health.status(),
      ordinaryRESTStatus: rest.status,
      oldPausedResidual: {
        sandboxId: '941a61a65d694136a8f3de743177a2cd',
        observedState: oldResidualState,
        modified: false,
      },
      coreDNSState: coreDNS.stdout.trim().split('\n').sort(),
      result: 'PASS',
      observedAt: new Date().toISOString(),
    });
  } finally {
    await context.close();
    await browser.close();
  }
  process.stdout.write('verify-cleanup PASS\n');
}

function commandOutput(command, args) {
  const result = spawnSync(command, args, { encoding: 'utf8' });
  assert(result.status === 0, `command failed while building manifest: ${command}`);
  return result.stdout.trim();
}

function sha256File(filePath) {
  const hash = crypto.createHash('sha256');
  hash.update(fs.readFileSync(filePath));
  return hash.digest('hex');
}

function collectFiles(root) {
  if (!fs.existsSync(root)) return [];
  const files = [];
  const visit = (current) => {
    for (const entry of fs.readdirSync(current, { withFileTypes: true })) {
      const absolute = path.join(current, entry.name);
      if (entry.isDirectory()) visit(absolute);
      else if (entry.isFile()) files.push(absolute);
    }
  };
  visit(root);
  return files.sort();
}

function staticTreeFingerprint(staticPath) {
  const resolvedPath = fs.realpathSync(staticPath);
  const files = collectFiles(resolvedPath).map((file) => ({
    path: path.relative(resolvedPath, file),
    bytes: fs.statSync(file).size,
    sha256: sha256File(file),
  }));
  const hash = crypto.createHash('sha256');
  for (const file of files) hash.update(`${file.path}\t${file.bytes}\t${file.sha256}\n`);
  return {
    configuredPath: staticPath,
    resolvedPath,
    fileCount: files.length,
    bytes: files.reduce((sum, file) => sum + file.bytes, 0),
    sha256: hash.digest('hex'),
  };
}

function exactInternalTokenArtifactCount(roots) {
  const script = String.raw`
import os
import sys

token = open('/usr/local/services/cubetoolbox/.terminal-internal-token', 'rb').read().strip()
count = 0
for root in sys.argv[1:]:
    for current, _, files in os.walk(root):
        for name in files:
            path = os.path.join(current, name)
            try:
                with open(path, 'rb') as stream:
                    if token and token in stream.read():
                        count += 1
            except OSError:
                continue
token = b''
print(count)
`;
  const result = spawnSync('sudo', ['-n', 'python3', '-c', script, ...roots], { encoding: 'utf8' });
  assert(result.status === 0, 'exact internal-token artifact scan failed');
  const count = Number(result.stdout.trim());
  assert(Number.isInteger(count) && count >= 0, 'exact internal-token artifact count is invalid');
  return count;
}

function manifest() {
  const state = readState();
  const repoRoot = path.resolve(__dirname, '..', '..');
  const reviewArtifactPaths = [
    'browser/discover.json',
    'browser/provision.json',
    'browser/core.json',
    'browser/security.json',
    'browser/grace-expiry.json',
    'browser/concurrency.json',
    'browser/idle.json',
    'browser/drain.json',
    'browser/cleanup.json',
    'summaries/audit.json',
    'summaries/correlation.json',
    'summaries/sensitive-log-scan.json',
    'summaries/cleanup-health.json',
    'screenshots/terminal-desktop.png',
    'screenshots/terminal-390x844.png',
    'screenshots/terminal-fullscreen.png',
    'screenshots/terminal-paused-tooltip.png',
  ];
  for (const optionalPath of [
    'summaries/task-users-provision.json',
    'summaries/task-users-cleanup.json',
  ]) {
    if (fs.existsSync(path.join(outputDir, optionalPath))) reviewArtifactPaths.push(optionalPath);
  }
  const candidateFiles = reviewArtifactPaths.map((relativePath) => path.join(outputDir, relativePath));
  for (const file of candidateFiles) assert(fs.statSync(file).isFile(), `required bounded review artifact is missing: ${path.relative(outputDir, file)}`);
  const artifactHashes = candidateFiles.map((file) => ({
    path: path.relative(outputDir, file),
    bytes: fs.statSync(file).size,
    sha256: sha256File(file),
  }));
  const textFiles = candidateFiles.filter((file) => !/\.(png|jpg|jpeg|webp)$/i.test(file));
  const text = textFiles.map((file) => fs.readFileSync(file, 'utf8')).join('\n');
  const scanCounts = {
    grantSubprotocol: (text.match(/cube-grant\./g) ?? []).length,
    authorizationHeader: (text.match(/Authorization\s*:/gi) ?? []).length,
    bearerValue: (text.match(/Bearer\s+[A-Za-z0-9._~-]+/g) ?? []).length,
    cookieHeader: (text.match(/Cookie\s*:/gi) ?? []).length,
    jwtLike: (text.match(/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g) ?? []).length,
    tokenHashName: (text.match(/token_hash/gi) ?? []).length,
    terminalPayloadMarker: (text.match(/C7E_/g) ?? []).length,
    credentialValueField: (text.match(/"(?:password|token|accessToken|refreshToken)"\s*:/g) ?? []).length,
    internalTokenAssignment: (text.match(/CUBE_TERMINAL_INTERNAL_TOKEN\s*=/g) ?? []).length,
    privateKeyBlock: (text.match(/-----BEGIN [A-Z ]*PRIVATE KEY-----/g) ?? []).length,
    websocketProtocolHeader: (text.match(/Sec-WebSocket-Protocol\s*:/gi) ?? []).length,
  };
  assert(Object.values(scanCounts).every((value) => value === 0), `review artifact scan counts are nonzero: ${JSON.stringify(scanCounts)}`);
  scanCounts.exactInternalToken = exactInternalTokenArtifactCount([browserDir, summaryDir, screenshotDir]);
  assert(scanCounts.exactInternalToken === 0, 'exact internal token appeared in a review artifact');

  const livePids = {
    cubeops: systemdMainPID('cube-sandbox-cubeops.service'),
    cubemaster: systemdMainPID('cube-sandbox-cubemaster.service'),
    cubelet: systemdMainPID('cube-sandbox-cubelet.service'),
  };
  const binaryHashes = {};
  for (const [name, pid] of Object.entries(livePids)) {
    binaryHashes[name] = commandOutput('sudo', ['-n', 'sha256sum', `/proc/${pid}/exe`]).split(/\s+/)[0];
  }
  const imageID = commandOutput('docker', ['inspect', 'cube-webui', '--format', '{{.Image}}']);
  const imageRepoDigests = JSON.parse(commandOutput('docker', ['image', 'inspect', imageID, '--format', '{{json .RepoDigests}}']) || '[]');
  const nginxHash = commandOutput('sha256sum', ['/usr/local/services/cubetoolbox/webui/nginx.generated.conf']).split(/\s+/)[0];
  const staticTree = staticTreeFingerprint('/usr/local/services/cubetoolbox/webui/dist');
  const sourcePaths = [
    'scripts/terminal-evidence/README.md',
    'scripts/terminal-evidence/browser.cjs',
    'scripts/terminal-evidence/lib.sh',
    'scripts/terminal-evidence/terminal-evidence.sh',
    'scripts/terminal-evidence/tests/test.sh',
  ];
  const sourceHashes = sourcePaths.map((relativePath) => {
    const file = path.join(repoRoot, relativePath);
    return { path: relativePath, bytes: fs.statSync(file).size, sha256: sha256File(file) };
  });
  const tokenFileMetadata = commandOutput('sudo', [
    '-n',
    'stat',
    '-c',
    'owner=%U:%G mode=%a bytes=%s',
    '/usr/local/services/cubetoolbox/.terminal-internal-token',
  ]);
  const steps = fs.readFileSync(path.join(outputDir, 'steps.tsv'), 'utf8').trim().split('\n').filter(Boolean).map((line) => {
    const [at, step, status, detail] = line.split('\t');
    return { at, step, status, detail };
  });
  const completedAt = new Date().toISOString();
  const concurrencyEvidence = readJSON(path.join(browserDir, 'concurrency.json'));
  const coreEvidence = readJSON(path.join(browserDir, 'core.json'));
  const twoRealUsersPassed =
    concurrencyEvidence.status === 'PASS' &&
    concurrencyEvidence.executedMatrix?.users === 2 &&
    concurrencyEvidence.distinctAuthenticatedUsers === true;
  const evidenceManifest = {
    schemaVersion: 1,
    taskId,
    gitSHA: commandOutput('git', ['-C', repoRoot, 'rev-parse', 'HEAD']),
    endpoint,
    startedAt: fs.readFileSync(startedAtPath, 'utf8').trim(),
    completedAt,
    topology: {
      webUI: 'systemd cube-sandbox-webui.service -> Docker cube-webui/OpenResty',
      backend: 'nginx -> CubeOps -> CubeMaster -> Cubelet -> containerd exec',
    },
    liveDigests: {
      webUIImageID: imageID,
      webUIImageRepoDigests: imageRepoDigests,
      nginxSHA256: nginxHash,
      staticTree,
      binaries: binaryHashes,
    },
    effectiveLocations: {
      nginx: '/usr/local/services/cubetoolbox/webui/nginx.generated.conf',
      static: '/usr/local/services/cubetoolbox/webui/dist',
      runtimeEnvironment: '/usr/local/services/cubetoolbox/.one-click.env',
      terminalInternalToken: {
        path: '/usr/local/services/cubetoolbox/.terminal-internal-token',
        metadata: tokenFileMetadata,
        valueRecorded: false,
      },
    },
    evidenceSource: sourceHashes,
    execution: steps,
    matrix: {
      environmentFingerprint: 'PASS',
      functionAndTargetBinding: 'PASS',
      security: 'PASS',
      lifecycle: 'PASS',
      concurrency: twoRealUsersPassed ? 'PASS_TWO_USERS_TWO_INSTANCES_FOUR_SESSIONS' : concurrencyEvidence.status,
      audit: 'PASS',
      upgradeDrain: 'PASS',
      realMultiContainer: coreEvidence.containers.status,
      twoRealUsers: twoRealUsersPassed ? 'PASS_TWO_DISTINCT_AUTHENTICATED_USERS' : 'FAIL_OR_NOT_RUN',
    },
    artifacts: artifactHashes,
    sensitiveScan: { countsOnly: true, counts: scanCounts, result: 'PASS' },
    cleanup: readJSON(path.join(summaryDir, 'cleanup-health.json')),
    firstFailurePreserved: fs.existsSync(path.join(outputDir, 'first-failure.tsv')),
    gaps: [
      ...(!twoRealUsersPassed
        ? ['Two distinct authenticated users were not completed; see browser/concurrency.json.']
        : []),
      ...(coreEvidence.containers.status === 'SKIP_SINGLE_CONTAINER_TEMPLATE'
        ? ['The available READY template is single-container; no safe real multi-container template was available.']
        : []),
      'This evidence run validates an existing one-click deployment and does not recreate or destroy the shared cube-dev stack.',
    ],
  };
  safeWriteJSON(path.join(summaryDir, 'manifest.json'), evidenceManifest);
  const realMultiContainerResult = evidenceManifest.matrix.realMultiContainer === 'PASS_REAL_MULTI_CONTAINER'
    ? 'PASS'
    : 'SKIP: no safe real multi-container template';
  const index = [
    '# Web Terminal real-cluster evidence index',
    '',
    `- Task ID: \`${taskId}\``,
    `- Git SHA: \`${evidenceManifest.gitSHA}\``,
    `- Endpoint: \`${endpoint}\``,
    `- Window: ${evidenceManifest.startedAt} to ${completedAt}`,
    '',
    '| Design section 9.2 category | Result | Review artifact |',
    '| --- | --- | --- |',
    '| 1. Environment fingerprint | PASS | `manifest.json` |',
    `| 2. Function and target binding | PASS, real multi-container ${realMultiContainerResult} | \`browser/core.json\`, \`correlation.json\`, screenshots |`,
    '| 3. Security | PASS | `browser/security.json`, `sensitive-log-scan.json` |',
    '| 4. Lifecycle | PASS | `browser/core.json`, `browser/grace-expiry.json`, `browser/idle.json` |',
    `| 5. Concurrency | ${twoRealUsersPassed ? 'PASS: two real users x two instances x four sessions' : concurrencyEvidence.status} | \`browser/concurrency.json\` |`,
    '| 6. Audit | PASS | `audit.json` (payload-free columns; token hash excluded) |',
    '| 7. Upgrade/drain | PASS | `browser/drain.json` |',
    '',
    'Raw logs, credentials, browser profiles, traces, videos, and terminal output remain outside the review set. Any unavailable coverage remains an explicit gap rather than a synthetic PASS.',
    '',
  ].join('\n');
  fs.writeFileSync(path.join(summaryDir, 'evidence-index.md'), index, { mode: 0o600 });
  fs.chmodSync(path.join(summaryDir, 'evidence-index.md'), 0o600);
  updateState((next) => {
    next.phases.manifest = { status: 'PASS', observedAt: completedAt };
  });
  process.stdout.write('manifest PASS\n');
}

const handlers = {
  'credential-from-public-hint': credentialFromPublicHint,
  discover,
  provision,
  core,
  security,
  'grace-expiry': graceExpiry,
  concurrency,
  idle,
  drain,
  'audit-correlation': auditCorrelation,
  cleanup,
  'verify-cleanup': verifyCleanup,
  manifest,
};

async function main() {
  assert(supportedPhases.has(phase), `unsupported browser phase: ${phase}`);
  const handler = handlers[phase];
  assert(typeof handler === 'function', `browser phase is not implemented: ${phase}`);
  await handler();
}

main().catch((error) => {
  const message = String(error?.message ?? error)
    .replace(/cube-grant\.[A-Za-z0-9_-]+/g, 'cube-grant.<redacted>')
    .replace(/Bearer\s+\S+/g, 'Bearer <redacted>')
    .replace(/Cookie:\s*\S+/gi, 'Cookie: <redacted>');
  process.stderr.write(`${phase || 'unknown'} failed: ${message}\n`);
  process.exitCode = 1;
});
