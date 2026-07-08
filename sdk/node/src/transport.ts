// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { Agent, buildConnector, fetch, type Dispatcher } from "undici";

import type { Config } from "./config.js";

/**
 * Build an undici {@link Dispatcher} for sandbox data-plane requests.
 *
 * When ``config.proxyNodeIp`` is set, every TCP connection is routed directly
 * to that IP:port while the request URL retains the virtual ``*.cube.app``
 * hostname, so the ``Host`` header is preserved for CubeProxy routing. This is
 * the Node equivalent of the Python SDK's ``IPOverrideTransport`` and the Go
 * SDK's ``DialContext`` override (``curl --resolve host:port:ip``).
 *
 * Returns ``undefined`` when no proxy override is configured, in which case
 * callers fall back to the default global dispatcher (normal DNS resolution).
 */
export function buildDataDispatcher(config: Config): Dispatcher | undefined {
  if (!config.proxyNodeIp) {
    return undefined;
  }

  const proxyIp = config.proxyNodeIp;
  const proxyPort = config.proxyPort;
  const baseConnect = buildConnector({ timeout: config.requestTimeoutMs });

  return new Agent({
    connect(opts, callback) {
      // Ignore the virtual hostname/port from the URL and dial the proxy node
      // instead. The Host header (derived from the URL) is left untouched.
      //
      // For TLS (``https`` data plane) we must keep the SNI / cert-verification
      // servername pinned to the *virtual* sandbox host, not the proxy IP —
      // otherwise the handshake fails or is validated against the wrong name.
      // This mirrors ``curl --resolve host:port:ip`` (dial ip, present host).
      const virtualServername =
        (opts as { servername?: string }).servername ??
        (typeof opts.hostname === "string" ? opts.hostname : undefined);
      baseConnect(
        { ...opts, hostname: proxyIp, port: String(proxyPort), servername: virtualServername },
        callback,
      );
    },
  });
}

/** Scheme used for data-plane (CubeProxy-routed) URLs. */
export function dataScheme(config: Config): string {
  return config.proxyScheme || "http";
}

/**
 * Perform a management-plane (CubeAPI) request with an overall timeout derived
 * from ``config.requestTimeoutMs`` (default 30s). Mirrors the Go SDK's
 * control-plane ``http.Client{Timeout: RequestTimeout}`` so a hung or slow
 * management call fails fast instead of blocking the caller indefinitely.
 *
 * Data-plane calls (``runCode`` / ``commands`` / filesystem) do NOT go through
 * here — they carry their own idle/connect timeouts and data dispatcher. A
 * caller-supplied ``signal`` takes precedence and disables the default timeout.
 *
 * When ``config.apiKey`` is set, an ``Authorization: Bearer`` header is injected
 * (unless the caller already provided one), matching the Go SDK's control-plane
 * auth. With no key the request is sent unauthenticated (localhost default).
 */
export function controlFetch(
  config: Config,
  url: string,
  init: Parameters<typeof fetch>[1] = {},
): ReturnType<typeof fetch> {
  const signal = init?.signal ?? AbortSignal.timeout(config.requestTimeoutMs);
  const headers: Record<string, string> = { ...(init?.headers as Record<string, string>) };
  if (config.apiKey && headers.Authorization === undefined) {
    headers.Authorization = `Bearer ${config.apiKey}`;
  }
  return fetch(url, { ...init, headers, signal });
}
