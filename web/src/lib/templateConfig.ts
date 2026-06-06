// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.

interface KeyValue {
  key?: string;
  value?: string;
}

interface CreateRequestShape {
  annotations?: Record<string, string>;
  containers?: Array<{
    image?: { writable_layer_size?: string };
    resources?: { cpu?: string; mem?: string };
    probe?: { probe_handler?: { http_get?: { path?: string; port?: number } } };
    envs?: KeyValue[];
    dns_config?: { servers?: string[] };
  }>;
  cubevs_context?: {
    allowOut?: string[];
    denyOut?: string[];
  };
}

export interface TemplateRuntimeConfig {
  exposedPorts: string | null;
  writableLayerSize: string | null;
  cpu: string | null;
  mem: string | null;
  probePath: string | null;
  probePort: string | null;
  env: string | null;
}

export interface TemplateNetworkPolicy {
  dns: string | null;
  allowOut: string | null;
  denyOut: string | null;
}

function formatEnvList(envs?: KeyValue[]): string | null {
  if (!envs?.length) return null;
  const lines = envs
    .map(({ key, value }) => (key ? `${key}=${value ?? ''}` : null))
    .filter(Boolean) as string[];
  return lines.length > 0 ? lines.join('\n') : null;
}

function formatStringList(items?: string[]): string | null {
  const values = items?.map((item) => item.trim()).filter(Boolean) ?? [];
  return values.length > 0 ? values.join(', ') : null;
}

export function extractTemplateRuntimeConfig(cr: unknown): TemplateRuntimeConfig | null {
  if (!cr || typeof cr !== 'object') return null;
  const req = cr as CreateRequestShape;
  const ann = req.annotations ?? {};
  const c = (req.containers ?? [])[0] ?? {};
  const probe = c.probe?.probe_handler?.http_get;
  return {
    exposedPorts: ann['com.exposed_ports'] ?? null,
    writableLayerSize: c.image?.writable_layer_size ?? null,
    cpu: c.resources?.cpu ?? null,
    mem: c.resources?.mem ?? null,
    probePath: probe?.path ?? null,
    probePort: probe?.port != null ? String(probe.port) : null,
    env: formatEnvList(c.envs),
  };
}

export function extractTemplateNetworkPolicy(cr: unknown): TemplateNetworkPolicy {
  if (!cr || typeof cr !== 'object') {
    return { dns: null, allowOut: null, denyOut: null };
  }
  const req = cr as CreateRequestShape;
  const c = (req.containers ?? [])[0] ?? {};
  const ctx = req.cubevs_context;
  return {
    dns: formatStringList(c.dns_config?.servers),
    allowOut: formatStringList(ctx?.allowOut),
    denyOut: formatStringList(ctx?.denyOut),
  };
}
