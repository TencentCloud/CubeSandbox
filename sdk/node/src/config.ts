// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

/**
 * SDK configuration. All fields can be sourced from environment variables.
 *
 * Mirrors the Python SDK's ``Config`` dataclass. Pass a partial object to
 * override individual fields; anything omitted falls back to the matching
 * ``CUBE_*`` environment variable (evaluated at construction time).
 */
export interface ConfigOptions {
  /** CubeAPI management-plane address. Env: ``CUBE_API_URL``. */
  apiUrl?: string;
  /**
   * API key for management-plane authentication, sent as
   * ``Authorization: Bearer <key>`` on every control-plane request. Env:
   * ``CUBE_API_KEY`` (falling back to ``E2B_API_KEY``). When unset no
   * ``Authorization`` header is sent — matching an unauthenticated localhost
   * deployment.
   */
  apiKey?: string | null;
  /** Template ID for sandbox creation. Env: ``CUBE_TEMPLATE_ID``. */
  templateId?: string | null;
  /** CubeProxy node IP; bypasses DNS for ``*.cube.app``. Env: ``CUBE_PROXY_NODE_IP``. */
  proxyNodeIp?: string | null;
  /** CubeProxy HTTP port. Env: ``CUBE_PROXY_PORT_HTTP`` (default 80). */
  proxyPort?: number;
  /** Data-plane scheme (``http`` / ``https``). Env: ``CUBE_PROXY_SCHEME`` (default http). */
  proxyScheme?: string;
  /** Sandbox domain suffix. Env: ``CUBE_SANDBOX_DOMAIN`` (default cube.app). */
  sandboxDomain?: string;
  /** Default sandbox TTL in seconds (default 300). */
  timeout?: number;
  /** Per-request connect timeout in milliseconds (default 30000). */
  requestTimeoutMs?: number;
}

/**
 * Parse a port from an env var, falling back to ``fallback`` when the value is
 * unset, non-numeric (``NaN``), or outside the valid TCP port range
 * (1–65535). Mirrors the Go SDK's ``parseIntEnv`` graceful-fallback behavior
 * (``parseInt`` returns ``NaN`` — not ``null`` — so ``??`` alone would let it
 * slip through), with an added upper bound so an out-of-range value doesn't
 * surface later as an opaque connection failure.
 */
function parsePort(value: string | undefined, fallback: number): number {
  if (value === undefined) {
    return fallback;
  }
  const parsed = parseInt(value, 10);
  return Number.isInteger(parsed) && parsed > 0 && parsed <= 65535 ? parsed : fallback;
}

/**
 * Restrict a data-plane scheme to ``http`` / ``https``. Mirrors the Go SDK's
 * ``normalizeProxyScheme``: an unknown/empty value falls back to ``https`` when
 * the proxy port is 443, otherwise ``http`` — so a typo can't slip an invalid
 * scheme into the data-plane URL and surface as an opaque connection error.
 */
function normalizeProxyScheme(scheme: string | undefined, port: number): string {
  const normalized = (scheme ?? "").trim().toLowerCase();
  if (normalized === "http" || normalized === "https") {
    return normalized;
  }
  return port === 443 ? "https" : "http";
}

/** Default sandbox TTL in seconds, shared by {@link Config} and ``resume``. */
export const DEFAULT_SANDBOX_TIMEOUT_S = 300;

export class Config {
  apiUrl: string;
  apiKey: string | null;
  templateId: string | null;
  proxyNodeIp: string | null;
  proxyPort: number;
  proxyScheme: string;
  sandboxDomain: string;
  timeout: number;
  requestTimeoutMs: number;

  constructor(options: ConfigOptions = {}) {
    const env = process.env;
    this.apiUrl = (options.apiUrl ?? env.CUBE_API_URL ?? "http://127.0.0.1:3000").replace(
      /\/+$/,
      "",
    );
    const rawKey = options.apiKey ?? env.CUBE_API_KEY ?? env.E2B_API_KEY ?? null;
    this.apiKey = rawKey && rawKey.trim() ? rawKey.trim() : null;
    this.templateId = options.templateId ?? env.CUBE_TEMPLATE_ID ?? null;
    this.proxyNodeIp = options.proxyNodeIp ?? env.CUBE_PROXY_NODE_IP ?? null;
    this.proxyPort = options.proxyPort ?? parsePort(env.CUBE_PROXY_PORT_HTTP, 80);
    this.proxyScheme = normalizeProxyScheme(
      options.proxyScheme ?? env.CUBE_PROXY_SCHEME,
      this.proxyPort,
    );
    this.sandboxDomain = options.sandboxDomain ?? env.CUBE_SANDBOX_DOMAIN ?? "cube.app";
    this.timeout = options.timeout ?? DEFAULT_SANDBOX_TIMEOUT_S;
    this.requestTimeoutMs = options.requestTimeoutMs ?? 30000;
  }
}

/** Coerce an optional ``Config | ConfigOptions`` into a concrete ``Config``. */
export function resolveConfig(config?: Config | ConfigOptions): Config {
  if (config instanceof Config) {
    return config;
  }
  return new Config(config);
}
