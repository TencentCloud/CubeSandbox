// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { Config, resolveConfig, type ConfigOptions } from "./config.js";
import { ApiError, AuthenticationError, TemplateNotFoundError } from "./exceptions.js";
import { validateAllowOutDomainsRequireDenyAll } from "./policy.js";
import { controlFetch } from "./transport.js";

async function checkTemplateResponse(resp: {
  ok: boolean;
  status: number;
  text: () => Promise<string>;
}): Promise<void> {
  if (resp.ok) {
    return;
  }
  let text = "";
  try {
    text = await resp.text();
  } catch {
    text = "";
  }
  let msg = text || `HTTP ${resp.status}`;
  try {
    const body = JSON.parse(text);
    msg = body?.message || body?.detail || msg;
  } catch {
    // raw text
  }
  const code = resp.status;
  if (code === 401 || code === 403) {
    throw new AuthenticationError(msg, code);
  }
  if (code === 404) {
    throw new TemplateNotFoundError(msg, code);
  }
  throw new ApiError(msg, code);
}

/** A template create/rebuild job or build status record. */
export class TemplateBuild {
  buildId: string;
  templateId: string;
  status: string;
  phase: string;
  progress: number;
  errorMessage: string;
  message: string;
  createdAt: string;
  finishedAt: string;
  logs: string[];

  constructor(fields: Partial<TemplateBuild> = {}) {
    this.buildId = fields.buildId ?? "";
    this.templateId = fields.templateId ?? "";
    this.status = fields.status ?? "";
    this.phase = fields.phase ?? "";
    this.progress = fields.progress ?? 0;
    this.errorMessage = fields.errorMessage ?? "";
    this.message = fields.message ?? "";
    this.createdAt = fields.createdAt ?? "";
    this.finishedAt = fields.finishedAt ?? "";
    this.logs = fields.logs ?? [];
  }

  /** Alias for create/rebuild responses that use ``jobID``. */
  get jobId(): string {
    return this.buildId;
  }

  static fromDict(data: Record<string, any>): TemplateBuild {
    return new TemplateBuild({
      buildId: data.buildID ?? data.jobID ?? data.build_id ?? "",
      templateId: data.templateID ?? data.template_id ?? "",
      status: data.status ?? "",
      phase: data.phase ?? "",
      progress: data.progress ?? 0,
      errorMessage: data.errorMessage ?? data.error_message ?? "",
      message: data.message ?? "",
      createdAt: data.createdAt ?? data.created_at ?? "",
      finishedAt: data.finishedAt ?? data.finished_at ?? "",
      logs: data.logs ?? [],
    });
  }
}

/** Metadata for a CubeSandbox template. */
export class TemplateInfo {
  templateId: string;
  name: string;
  instanceType: string;
  version: string;
  status: string;
  lastError: string;
  createdAt: string;
  imageInfo: string;
  jobId: string;
  public: boolean;
  cpuCount: number;
  memoryMb: number;
  replicas: Record<string, any>[];
  createRequest: Record<string, any> | null;
  networkType: string | null;
  allowInternetAccess: boolean | null;
  builds: TemplateBuild[];

  constructor(fields: Partial<TemplateInfo> = {}) {
    this.templateId = fields.templateId ?? "";
    this.name = fields.name ?? "";
    this.instanceType = fields.instanceType ?? "";
    this.version = fields.version ?? "";
    this.status = fields.status ?? "";
    this.lastError = fields.lastError ?? "";
    this.createdAt = fields.createdAt ?? "";
    this.imageInfo = fields.imageInfo ?? "";
    this.jobId = fields.jobId ?? "";
    this.public = fields.public ?? false;
    this.cpuCount = fields.cpuCount ?? 0;
    this.memoryMb = fields.memoryMb ?? 0;
    this.replicas = fields.replicas ?? [];
    this.createRequest = fields.createRequest ?? null;
    this.networkType = fields.networkType ?? null;
    this.allowInternetAccess = fields.allowInternetAccess ?? null;
    this.builds = fields.builds ?? [];
  }

  static fromDict(data: Record<string, any>): TemplateInfo {
    const aliases: string[] = data.aliases ?? [];
    return new TemplateInfo({
      templateId: data.templateID ?? data.template_id ?? "",
      name: data.name || (aliases.length ? aliases[0] : "") || "",
      instanceType: data.instanceType ?? data.instance_type ?? "",
      version: data.version ?? "",
      status: data.status ?? "",
      lastError: data.lastError ?? data.last_error ?? "",
      createdAt: data.createdAt ?? data.created_at ?? "",
      imageInfo: data.imageInfo ?? data.image_info ?? "",
      jobId: data.jobID ?? data.job_id ?? "",
      public: Boolean(data.public ?? false),
      cpuCount: data.cpuCount ?? data.cpu_count ?? 0,
      memoryMb: data.memoryMB ?? data.memory_mb ?? 0,
      replicas: data.replicas ?? [],
      createRequest: data.createRequest ?? data.create_request ?? null,
      networkType: data.networkType ?? data.network_type ?? null,
      allowInternetAccess:
        "allowInternetAccess" in data
          ? data.allowInternetAccess
          : (data.allow_internet_access ?? null),
      builds: (data.builds ?? []).map((b: Record<string, any>) => TemplateBuild.fromDict(b)),
    });
  }
}

/** Options for {@link Template.build}. */
export interface TemplateBuildOptions {
  image?: string;
  dockerfile?: string;
  startCmd?: string;
  instanceType?: string;
  writableLayerSize?: string;
  exposedPorts?: number[];
  probePort?: number;
  probePath?: string;
  cpuCount?: number;
  memoryMb?: number;
  envs?: Record<string, string>;
  allowInternetAccess?: boolean;
  networkType?: string;
  nodes?: string[];
  registryUsername?: string;
  registryPassword?: string;
  command?: string[];
  args?: string[];
  dns?: string[];
  allowOut?: string[];
  denyOut?: string[];
  config?: Config | ConfigOptions;
  extra?: Record<string, unknown>;
}

/** Class-level helper for Cube template management. All methods are static. */
export class Template {
  /** GET /templates — list all templates. */
  static async list(options: { config?: Config | ConfigOptions } = {}): Promise<TemplateInfo[]> {
    const cfg = resolveConfig(options.config);
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/templates`);
    await checkTemplateResponse(resp);
    let data = (await resp.json()) as any;
    if (data && !Array.isArray(data)) {
      data = data.templates ?? data.items ?? [];
    }
    return ((data as Record<string, any>[]) || []).map((d) => TemplateInfo.fromDict(d));
  }

  /** GET /templates/:id — get a template and its build history. */
  static async get(
    templateId: string,
    options: { limit?: number; nextToken?: string; config?: Config | ConfigOptions } = {},
  ): Promise<TemplateInfo> {
    const cfg = resolveConfig(options.config);
    const params = new URLSearchParams();
    if (options.limit !== undefined) params.set("limit", String(options.limit));
    if (options.nextToken !== undefined) params.set("nextToken", options.nextToken);
    const query = params.toString();
    const resp = await controlFetch(
      cfg,
      `${cfg.apiUrl}/templates/${templateId}${query ? `?${query}` : ""}`,
    );
    await checkTemplateResponse(resp);
    return TemplateInfo.fromDict((await resp.json()) as Record<string, any>);
  }

  /** POST /templates — build (create) a new template from a container image. */
  static async build(options: TemplateBuildOptions): Promise<TemplateBuild> {
    if (options.dockerfile !== undefined) {
      throw new Error("dockerfile builds are not supported by CubeAPI /templates");
    }
    if (options.startCmd !== undefined) {
      throw new Error("startCmd is not supported by CubeAPI /templates");
    }
    if (!options.image || !options.image.trim()) {
      throw new Error("image is required");
    }
    validateAllowOutDomainsRequireDenyAll(
      options.allowOut,
      options.denyOut,
      options.allowInternetAccess === false,
    );

    const cfg = resolveConfig(options.config);
    const payload: Record<string, unknown> = { image: options.image.trim() };
    if (options.instanceType !== undefined) payload.instanceType = options.instanceType;
    if (options.writableLayerSize !== undefined) {
      payload.writableLayerSize = options.writableLayerSize;
    }
    if (options.exposedPorts !== undefined) payload.exposedPorts = options.exposedPorts;
    if (options.probePort !== undefined) payload.probePort = options.probePort;
    if (options.probePath !== undefined) payload.probePath = options.probePath;
    if (options.cpuCount !== undefined) payload.cpu = options.cpuCount;
    if (options.memoryMb !== undefined) payload.memory = options.memoryMb;
    if (options.envs !== undefined) {
      payload.env = Object.entries(options.envs).map(([k, v]) => `${k}=${v}`);
    }
    if (options.allowInternetAccess !== undefined) {
      payload.allowInternetAccess = options.allowInternetAccess;
    }
    if (options.networkType !== undefined) payload.networkType = options.networkType;
    if (options.nodes !== undefined) payload.nodes = options.nodes;
    if (options.registryUsername !== undefined) {
      payload.registryUsername = options.registryUsername;
    }
    if (options.registryPassword !== undefined) {
      payload.registryPassword = options.registryPassword;
    }
    if (options.command !== undefined) payload.command = options.command;
    if (options.args !== undefined) payload.args = options.args;
    if (options.dns !== undefined) payload.dns = options.dns;
    if (options.allowOut !== undefined) payload.allowOut = options.allowOut;
    if (options.denyOut !== undefined) payload.denyOut = options.denyOut;
    if (options.extra) Object.assign(payload, options.extra);

    const resp = await controlFetch(cfg, `${cfg.apiUrl}/templates`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    await checkTemplateResponse(resp);
    return TemplateBuild.fromDict((await resp.json()) as Record<string, any>);
  }

  /** POST /templates/:id — rebuild an existing template. */
  static async rebuild(
    templateId: string,
    options: { config?: Config | ConfigOptions; extra?: Record<string, unknown> } = {},
  ): Promise<TemplateBuild> {
    const cfg = resolveConfig(options.config);
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/templates/${templateId}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(options.extra ?? {}),
    });
    await checkTemplateResponse(resp);
    return TemplateBuild.fromDict((await resp.json()) as Record<string, any>);
  }

  /** GET /templates/:id/builds/:buildId/status. */
  static async getBuildStatus(
    templateId: string,
    buildId: string,
    options: { config?: Config | ConfigOptions } = {},
  ): Promise<TemplateBuild> {
    const cfg = resolveConfig(options.config);
    const resp = await controlFetch(
      cfg,
      `${cfg.apiUrl}/templates/${templateId}/builds/${buildId}/status`,
    );
    await checkTemplateResponse(resp);
    return TemplateBuild.fromDict((await resp.json()) as Record<string, any>);
  }

  /** GET /templates/:id/builds/:buildId/logs. */
  static async getBuildLogs(
    templateId: string,
    buildId: string,
    options: { config?: Config | ConfigOptions } = {},
  ): Promise<Record<string, any>> {
    const cfg = resolveConfig(options.config);
    const resp = await controlFetch(
      cfg,
      `${cfg.apiUrl}/templates/${templateId}/builds/${buildId}/logs`,
    );
    await checkTemplateResponse(resp);
    return (await resp.json()) as Record<string, any>;
  }

  /** DELETE /templates/:id — delete a template permanently. */
  static async delete(
    templateId: string,
    options: { config?: Config | ConfigOptions } = {},
  ): Promise<void> {
    const cfg = resolveConfig(options.config);
    const resp = await controlFetch(cfg, `${cfg.apiUrl}/templates/${templateId}`, {
      method: "DELETE",
    });
    await checkTemplateResponse(resp);
  }
}
