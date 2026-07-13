// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { fetch, type Dispatcher } from "undici";

import { Commands } from "./commands.js";
import { Config, DEFAULT_SANDBOX_TIMEOUT_S, resolveConfig, type ConfigOptions } from "./config.js";
import {
  ApiError,
  AuthenticationError,
  CubeSandboxError,
  SandboxNotFoundError,
  TemplateNotFoundError,
} from "./exceptions.js";
import { Filesystem } from "./filesystem.js";
import { Execution, SnapshotInfo } from "./models.js";
import {
  normalizeRulesArg,
  serializeRule,
  validateAllowOutDomainsRequireDenyAll,
  type NetworkRules,
} from "./policy.js";
import { Pty } from "./pty.js";
import { createIdleTimeout, parseNdjsonStream, type RunCodeCallbacks } from "./stream.js";
import { buildDataDispatcher, controlFetch, dataScheme } from "./transport.js";

export const JUPYTER_PORT = 49999;

/** Egress network policy passed to {@link Sandbox.create}. */
export interface NetworkOptions {
  allowOut?: string[];
  denyOut?: string[];
  allowPublicTraffic?: boolean;
  rules?: NetworkRules;
}

/** Sandbox auto-resume lifecycle, mirroring the e2b SDK's ``lifecycle``. */
export interface LifecycleOptions {
  onTimeout?: "kill" | "pause";
  autoResume?: boolean;
}

/** Options for {@link Sandbox.create}. */
export interface CreateOptions {
  template?: string;
  /** Alias for {@link CreateOptions.template}, matching the Issue #760 / E2B shape. */
  templateId?: string;
  timeout?: number;
  envVars?: Record<string, string>;
  metadata?: Record<string, string>;
  allowInternetAccess?: boolean;
  network?: NetworkOptions;
  lifecycle?: LifecycleOptions;
  config?: Config | ConfigOptions;
  /** Extra fields forwarded verbatim into the create request body. */
  extra?: Record<string, unknown>;

  // ── Inline config overrides ───────────────────────────────────────────────
  // Convenience fields so callers can pass connection settings directly on
  // `create` (as shown in Issue #760) instead of nesting them under `config`.
  // Precedence: these flat fields > `config` > `CUBE_*` env vars.
  /** CubeAPI management-plane address. Overrides ``config``/``CUBE_API_URL``. */
  apiUrl?: string;
  /** CubeProxy node IP; bypasses DNS for ``*.cube.app``. Overrides ``config``/``CUBE_PROXY_NODE_IP``. */
  proxyNodeIp?: string | null;
  /** CubeProxy HTTP port. Overrides ``config``/``CUBE_PROXY_PORT_HTTP``. */
  proxyPort?: number;
  /** Data-plane scheme (``http`` / ``https``). Overrides ``config``/``CUBE_PROXY_SCHEME``. */
  proxyScheme?: string;
  /** Sandbox domain suffix. Overrides ``config``/``CUBE_SANDBOX_DOMAIN``. */
  sandboxDomain?: string;
  /** Per-request connect timeout in milliseconds. Overrides ``config``. */
  requestTimeoutMs?: number;
}

/** Options for {@link Sandbox.runCode}. */
export interface RunCodeOptions extends RunCodeCallbacks {
  language?: string;
  envs?: Record<string, string>;
  /**
   * Idle (read) timeout in milliseconds. The timer resets on every chunk
   * received, so long-running executions that keep producing output are not
   * aborted — only a stream that goes silent for longer than this is. Mirrors
   * the Python SDK's read timeout. Default: no timeout.
   */
  timeoutMs?: number;
  /**
   * Alias for {@link RunCodeOptions.timeoutMs} (also milliseconds), matching
   * the Issue #760 / E2B shape. {@link RunCodeOptions.timeoutMs} wins when both
   * are set.
   */
  timeout?: number;
}

/** Options for {@link Sandbox.pause}. */
export interface PauseOptions {
  wait?: boolean;
  timeoutMs?: number;
  intervalMs?: number;
}

/** Options for {@link Sandbox.listSnapshots}. */
export interface ListSnapshotsOptions {
  sandboxId?: string;
  limit?: number;
  nextToken?: string;
  config?: Config | ConfigOptions;
}

const VALID_ON_TIMEOUT = ["kill", "pause"] as const;

function serializeLifecycle(lifecycle: LifecycleOptions): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (lifecycle.onTimeout !== undefined) {
    if (!VALID_ON_TIMEOUT.includes(lifecycle.onTimeout)) {
      throw new Error(
        `lifecycle.onTimeout must be one of ${JSON.stringify(VALID_ON_TIMEOUT)}, ` +
          `got ${JSON.stringify(lifecycle.onTimeout)}`,
      );
    }
    out.onTimeout = lifecycle.onTimeout;
  }
  if (lifecycle.autoResume !== undefined) {
    out.autoResume = Boolean(lifecycle.autoResume);
  }
  return out;
}

async function errorMessageFromResponse(resp: {
  status: number;
  text: () => Promise<string>;
}): Promise<string> {
  let text = "";
  try {
    text = await resp.text();
  } catch {
    text = "";
  }
  if (text) {
    try {
      const body = JSON.parse(text);
      const msg = body?.message ?? body?.detail;
      if (typeof msg === "string" && msg) {
        return msg;
      }
    } catch {
      // not JSON — fall through to raw (truncated) text
    }
    // Cap non-JSON bodies (HTML error pages, stack traces, internal IPs) so an
    // opaque upstream response can't bloat or leak into the thrown message.
    const trimmed = text.trim();
    const capped = trimmed.length > 500 ? `${trimmed.slice(0, 500)}… (truncated)` : trimmed;
    return capped ? `HTTP ${resp.status}: ${capped}` : `HTTP ${resp.status}`;
  }
  return `HTTP ${resp.status}`;
}

async function checkControlResponse(resp: {
  ok: boolean;
  status: number;
  text: () => Promise<string>;
}): Promise<void> {
  if (resp.ok) {
    return;
  }
  const msg = await errorMessageFromResponse(resp);
  const code = resp.status;
  if (code === 401 || code === 403) {
    throw new AuthenticationError(msg, code);
  }
  if (code === 404) {
    if (msg.toLowerCase().includes("template")) {
      throw new TemplateNotFoundError(msg, code);
    }
    throw new SandboxNotFoundError(msg, code);
  }
  throw new ApiError(msg, code);
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/**
 * Resolve the effective {@link Config} for {@link Sandbox.create}, merging the
 * inline config overrides on {@link CreateOptions} over ``options.config`` over
 * the ``CUBE_*`` environment defaults. Flat fields win when defined.
 */
function resolveCreateConfig(options: CreateOptions): Config {
  const base: ConfigOptions =
    options.config instanceof Config
      ? {
          apiUrl: options.config.apiUrl,
          templateId: options.config.templateId,
          proxyNodeIp: options.config.proxyNodeIp,
          proxyPort: options.config.proxyPort,
          proxyScheme: options.config.proxyScheme,
          sandboxDomain: options.config.sandboxDomain,
          timeout: options.config.timeout,
          requestTimeoutMs: options.config.requestTimeoutMs,
        }
      : { ...(options.config ?? {}) };

  if (options.apiUrl !== undefined) base.apiUrl = options.apiUrl;
  if (options.proxyNodeIp !== undefined) base.proxyNodeIp = options.proxyNodeIp;
  if (options.proxyPort !== undefined) base.proxyPort = options.proxyPort;
  if (options.proxyScheme !== undefined) base.proxyScheme = options.proxyScheme;
  if (options.sandboxDomain !== undefined) base.sandboxDomain = options.sandboxDomain;
  if (options.requestTimeoutMs !== undefined) base.requestTimeoutMs = options.requestTimeoutMs;

  return new Config(base);
}

/**
 * A CubeSandbox code-execution environment.
 *
 * Example:
 * ```ts
 * const sb = await Sandbox.create();
 * await sb.runCode("x = 1");
 * const result = await sb.runCode("x + 1");
 * console.log(result.text); // "2"
 * await sb.kill();
 * ```
 */
export class Sandbox {
  /** Raw create/connect response payload (backend wire shape). */
  private readonly _data: Record<string, any>;
  readonly config: Config;

  private _dataDispatcher: Dispatcher | undefined;
  private _dataDispatcherBuilt = false;
  private readonly _commands: Commands;
  private readonly _files: Filesystem;
  private readonly _pty: Pty;

  constructor(data: Record<string, any>, config: Config) {
    this._data = data;
    this.config = config;
    this._commands = new Commands(this);
    this._files = new Filesystem(this);
    this._pty = new Pty(this);
  }

  get sandboxId(): string {
    return this._data.sandboxID;
  }

  get templateId(): string {
    return this._data.templateID;
  }

  get domain(): string {
    return this._data.domain || this.config.sandboxDomain;
  }

  /**
   * Per-sandbox token returned when ``network.allowPublicTraffic=false``. Sent
   * as both the ``e2b-traffic-access-token`` and ``cube-traffic-access-token``
   * headers on every request to the sandbox's public URL (CubeProxy accepts
   * either). ``null`` for publicly reachable sandboxes.
   */
  get trafficAccessToken(): string | null {
    return this._data.trafficAccessToken || null;
  }

  get envdAccessToken(): string | undefined {
    return this._data.envdAccessToken;
  }

  get commands(): Commands {
    return this._commands;
  }

  get files(): Filesystem {
    return this._files;
  }

  /** PTY (pseudo-terminal) namespace: ``create``/``connect``/``kill``/… */
  get pty(): Pty {
    return this._pty;
  }

  /** Return the virtual hostname for a sandbox port, e.g. ``49999-<id>.cube.app``. */
  getHost(port: number): string {
    return `${port}-${this.sandboxId}.${this.domain}`;
  }

  /** Data-plane URL for an envd/jupyter port and path. */
  dataUrl(port: number, path: string): string {
    return `${dataScheme(this.config)}://${this.getHost(port)}${path}`;
  }

  /** Lazily-built undici dispatcher for data-plane (CubeProxy) requests. */
  get dataDispatcher(): Dispatcher | undefined {
    if (!this._dataDispatcherBuilt) {
      this._dataDispatcher = buildDataDispatcher(this.config);
      this._dataDispatcherBuilt = true;
    }
    return this._dataDispatcher;
  }

  /** Headers required on every CubeProxy data-plane request. */
  trafficTokenHeaders(): Record<string, string> {
    const token = this.trafficAccessToken;
    return token
      ? { "e2b-traffic-access-token": token, "cube-traffic-access-token": token }
      : {};
  }

  // ── Class methods ─────────────────────────────────────────────────────────

  /** POST /sandboxes — create a new sandbox. */
  static async create(options: CreateOptions = {}): Promise<Sandbox> {
    const cfg = resolveCreateConfig(options);
    const tpl = options.template || options.templateId || cfg.templateId;
    if (!tpl) {
      throw new Error(
        "template is required. Set CUBE_TEMPLATE_ID or pass template/templateId",
      );
    }

    const payload: Record<string, unknown> = {
      templateID: tpl,
      timeout: options.timeout ?? cfg.timeout,
    };
    if (options.envVars) {
      payload.envVars = options.envVars;
    }
    if (options.metadata) {
      payload.metadata = options.metadata;
    }
    if (options.allowInternetAccess === false) {
      payload.allow_internet_access = false;
    }
    if (options.network) {
      const net = options.network;
      validateAllowOutDomainsRequireDenyAll(
        net.allowOut,
        net.denyOut,
        options.allowInternetAccess === false,
      );
      const wire: Record<string, unknown> = {};
      if (net.allowOut !== undefined) wire.allowOut = net.allowOut;
      if (net.denyOut !== undefined) wire.denyOut = net.denyOut;
      if (net.allowPublicTraffic !== undefined) {
        wire.allowPublicTraffic = net.allowPublicTraffic;
      }
      if (net.rules) {
        const normalized = normalizeRulesArg(net.rules);
        if (normalized.length > 0) {
          wire.rules = normalized.map(serializeRule);
        }
      }
      if (Object.keys(wire).length > 0) {
        payload.network = wire;
      }
    }
    if (options.lifecycle) {
      payload.lifecycle = serializeLifecycle(options.lifecycle);
    }
    if (options.extra) {
      Object.assign(payload, options.extra);
    }

    const resp = await controlFetch(cfg, `${cfg.apiUrl}/sandboxes`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    await checkControlResponse(resp);
    return new Sandbox((await resp.json()) as Record<string, any>, cfg);
  }

  /** POST /sandboxes/:id/connect — connect (auto-resumes if paused). */
  static async connect(
    sandboxId: string,
    options: { config?: Config | ConfigOptions } = {},
  ): Promise<Sandbox> {
    const cfg = resolveConfig(options.config);
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/sandboxes/${sandboxId}/connect`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ timeout: cfg.timeout }),
    });
    await checkControlResponse(resp);
    return new Sandbox((await resp.json()) as Record<string, any>, cfg);
  }

  /** GET /sandboxes — list all running sandboxes (v1). */
  static async list(config?: Config | ConfigOptions): Promise<Record<string, any>[]> {
    const cfg = resolveConfig(config);
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/sandboxes`);
    await checkControlResponse(resp);
    return (await resp.json()) as Record<string, any>[];
  }

  /** GET /v2/sandboxes — list all running sandboxes (v2). */
  static async listV2(config?: Config | ConfigOptions): Promise<Record<string, any>[]> {
    const cfg = resolveConfig(config);
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/v2/sandboxes`);
    await checkControlResponse(resp);
    return (await resp.json()) as Record<string, any>[];
  }

  /** GET /health — check the health of the CubeAPI service. */
  static async health(config?: Config | ConfigOptions): Promise<Record<string, any>> {
    const cfg = resolveConfig(config);
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/health`);
    await checkControlResponse(resp);
    return (await resp.json()) as Record<string, any>;
  }

  /** GET /snapshots — list snapshots with pagination. */
  static async listSnapshots(
    options: ListSnapshotsOptions = {},
  ): Promise<{ snapshots: SnapshotInfo[]; nextToken: string | null }> {
    const cfg = resolveConfig(options.config);
    const params = new URLSearchParams();
    if (options.sandboxId !== undefined) params.set("sandboxID", options.sandboxId);
    if (options.limit !== undefined) params.set("limit", String(options.limit));
    if (options.nextToken !== undefined) params.set("nextToken", options.nextToken);
    const query = params.toString();
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/snapshots${query ? `?${query}` : ""}`);
    await checkControlResponse(resp);
    const items = ((await resp.json()) as Record<string, any>[]) || [];
    const snapshots = items.map((d) => SnapshotInfo.fromDict(d));
    const nextToken = resp.headers.get("x-next-token") || null;
    return { snapshots, nextToken };
  }

  /** DELETE /templates/:id — delete a snapshot (stored as a template). */
  static async deleteSnapshot(
    snapshotId: string,
    options: { config?: Config | ConfigOptions } = {},
  ): Promise<void> {
    const cfg = resolveConfig(options.config);
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/templates/${snapshotId}`, {
      method: "DELETE",
    });
    await checkControlResponse(resp);
  }

  // ── Instance methods ────────────────────────────────────────────────────────

  /** POST /execute — execute code inside the sandbox (streams ndjson). */
  async runCode(code: string, options: RunCodeOptions = {}): Promise<Execution> {
    const url = this.dataUrl(JUPYTER_PORT, "/execute");
    const payload = {
      code,
      language: options.language ?? null,
      env_vars: options.envs ?? null,
    };
    const execution = new Execution();

    const idleMs = options.timeoutMs ?? options.timeout;
    const idle = createIdleTimeout(idleMs);
    let resp: Awaited<ReturnType<typeof fetch>>;
    try {
      resp = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json", ...this.trafficTokenHeaders() },
        body: JSON.stringify(payload),
        dispatcher: this.dataDispatcher,
        signal: idle.signal,
      });
    } catch (err) {
      idle.clear();
      if (idle.firedRef.current) {
        throw new ApiError(`runCode timed out after ${idleMs}ms of inactivity`);
      }
      throw err;
    }
    if (resp.status >= 400) {
      idle.clear();
      void resp.body?.cancel().catch(() => undefined);
      throw new ApiError(`execute failed: HTTP ${resp.status}`, resp.status);
    }
    try {
      if (resp.body) {
        await parseNdjsonStream(
          resp.body as ReadableStream<Uint8Array>,
          execution,
          options,
          idle.reset,
        );
      }
    } catch (err) {
      if (idle.firedRef.current) {
        throw new ApiError(`runCode timed out after ${idleMs}ms of inactivity`);
      }
      throw err;
    } finally {
      idle.clear();
    }
    return execution;
  }

  /** GET /sandboxes/:id — get sandbox detail. */
  async getInfo(): Promise<Record<string, any>> {
    const resp = await controlFetch(this.config, `${this.config.apiUrl}/sandboxes/${this.sandboxId}`);
    await checkControlResponse(resp);
    return (await resp.json()) as Record<string, any>;
  }

  /** POST /sandboxes/:id/pause — pause a sandbox (preserves memory snapshot). */
  async pause(options: PauseOptions = {}): Promise<void> {
    const wait = options.wait ?? true;
    const timeoutMs = options.timeoutMs ?? 30000;
    const intervalMs = options.intervalMs ?? 1000;

    const resp = await controlFetch(this.config, `${this.config.apiUrl}/sandboxes/${this.sandboxId}/pause`, {
      method: "POST",
    });
    await checkControlResponse(resp);

    if (!wait) {
      return;
    }
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      const info = await this.getInfo();
      if (info.state === "paused") {
        return;
      }
      await sleep(intervalMs);
    }
    throw new Error(
      `Sandbox ${this.sandboxId} did not reach 'paused' state within ${timeoutMs}ms`,
    );
  }

  /**
   * POST /sandboxes/:id/resume — resume a paused sandbox.
   * @deprecated Use {@link Sandbox.connect} which auto-resumes and returns a
   * fresh instance.
   */
  async resume(timeout = DEFAULT_SANDBOX_TIMEOUT_S): Promise<void> {
    const resp = await controlFetch(this.config, `${this.config.apiUrl}/sandboxes/${this.sandboxId}/resume`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ timeout }),
    });
    await checkControlResponse(resp);
  }

  /** DELETE /sandboxes/:id — destroy a sandbox. */
  async kill(): Promise<void> {
    const resp = await controlFetch(this.config, `${this.config.apiUrl}/sandboxes/${this.sandboxId}`, {
      method: "DELETE",
    });
    await checkControlResponse(resp);
  }

  /** POST /sandboxes/:id/snapshots — create a snapshot. */
  async createSnapshot(name?: string): Promise<SnapshotInfo> {
    const payload: Record<string, unknown> = {};
    if (name !== undefined) {
      payload.name = name;
    }
    const resp = await controlFetch(this.config, `${this.config.apiUrl}/sandboxes/${this.sandboxId}/snapshots`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    await checkControlResponse(resp);
    return SnapshotInfo.fromDict((await resp.json()) as Record<string, any>);
  }

  /** POST /sandboxes/:id/rollback — roll back a sandbox to a snapshot. */
  async rollback(snapshotId: string): Promise<Record<string, any>> {
    const resp = await controlFetch(this.config, `${this.config.apiUrl}/sandboxes/${this.sandboxId}/rollback`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ snapshotID: snapshotId }),
    });
    await checkControlResponse(resp);
    const result = (await resp.json()) as Record<string, any>;
    // Rollback restarts the sandbox process — drop the pooled data-plane
    // dispatcher so the next call reconnects.
    this._resetConnections();
    return result;
  }

  /**
   * Clone this sandbox ``n`` times: snapshot -> create ×n -> delete snapshot.
   *
   * ``concurrency`` caps how many ``Sandbox.create`` calls run in parallel at
   * ``min(n, concurrency)`` (default 1 = sequential), mirroring the Python
   * (``ThreadPoolExecutor``) and Go (semaphore) SDKs. If any create fails, all
   * successful siblings are killed and the first error is re-thrown.
   */
  async clone(n = 1, options: { concurrency?: number } = {}): Promise<Sandbox[]> {
    const concurrency = options.concurrency ?? 1;
    const snapshot = await this.createSnapshot();
    const snapId = snapshot.snapshotId;
    const cfg = this.config;

    const createOne = (): Promise<Sandbox> => Sandbox.create({ template: snapId, config: cfg });

    const sandboxes: Sandbox[] = [];
    let firstError: unknown = null;

    try {
      if (concurrency <= 1 || n <= 1) {
        for (let i = 0; i < n; i++) {
          try {
            sandboxes.push(await createOne());
          } catch (err) {
            firstError = err;
            break;
          }
        }
      } else {
        // Bounded fan-out: at most min(n, concurrency) create calls are in
        // flight at once (workers pull from a shared cursor), matching the
        // Python (ThreadPoolExecutor) and Go (semaphore) SDKs. Every task is
        // drained so a mid-fan-out failure never leaks a created sibling.
        const limit = Math.min(n, concurrency);
        let next = 0;
        const worker = async (): Promise<void> => {
          for (;;) {
            const index = next++;
            if (index >= n) {
              return;
            }
            try {
              sandboxes.push(await createOne());
            } catch (err) {
              if (firstError === null) {
                firstError = err;
              }
            }
          }
        };
        await Promise.all(Array.from({ length: limit }, () => worker()));
      }
    } finally {
      try {
        await Sandbox.deleteSnapshot(snapId, { config: cfg });
      } catch {
        // best-effort cleanup
      }
    }

    if (firstError !== null) {
      await Promise.allSettled(sandboxes.map((sb) => sb.kill()));
      throw firstError;
    }
    return sandboxes;
  }

  /** Close pooled HTTP connections without destroying the sandbox. */
  close(): void {
    this._resetConnections();
  }

  private _resetConnections(): void {
    if (this._dataDispatcher) {
      void this._dataDispatcher.close().catch(() => undefined);
    }
    this._dataDispatcher = undefined;
    this._dataDispatcherBuilt = false;
  }

  async [Symbol.asyncDispose](): Promise<void> {
    try {
      await this.kill();
    } catch (err) {
      if (!(err instanceof CubeSandboxError)) {
        throw err;
      }
    } finally {
      this.close();
    }
  }
}
