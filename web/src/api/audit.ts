// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.
//
// Sandbox audit trail. CubeSandbox itself keeps no record of destroyed
// sandboxes or of the commands run inside them (envd executes them in the
// guest; CubeShim only sees the VM lifecycle). The e2b-compatible gateway
// (e2b-shim) that fronts CubeAPI/CubeProxy writes one JSONL line per SDK
// request; this module reads the aggregated view it serves.
//
// Wire path: WebUI nginx `/auditapi/*` -> e2b-shim `/_audit/api/*`.

const AUDIT_BASE = '/auditapi';

export interface AuditSandboxRow {
  id: string;
  createdAt?: string | null;
  firstSeen: string;
  lastSeen: string;
  killedAt?: string | null;
  state: 'killed' | 'seen';
  template?: string | null;
  client?: string | null;
  metadata?: Record<string, string> | null;
  durationSec?: number | null;
  commands: number;
  files: number;
  requests: number;
  firstCommand?: string | null;
  allowInternetAccess?: boolean | null;
}

export interface AuditListResponse {
  days: number;
  files: string[];
  events: number;
  sandboxes: number;
  killed: number;
  templates: string[];
  total: number;
  page: number;
  size: number;
  items: AuditSandboxRow[];
}

export type AuditTimelineItem =
  | { ts: string; kind: 'create'; status: number; latencyMs: number; template?: string; metadata?: Record<string, string>; timeout?: number }
  | { ts: string; kind: 'kill'; status: number; latencyMs: number }
  | { ts: string; kind: 'api'; method: string; path: string; query?: string; status: number; latencyMs: number }
  | { ts: string; kind: 'command'; cmd: string; cwd?: string; user?: string; envKeys: string[]; pty: boolean; status: number; latencyMs: number }
  | { ts: string; kind: 'stdin'; body?: string; status: number }
  | { ts: string; kind: 'signal'; status: number }
  | { ts: string; kind: 'file'; op: string; path?: string; user?: string; status: number; reqBytes?: number; respBytes?: number; latencyMs?: number }
  | { ts: string; kind: 'process'; op: string; status: number; latencyMs: number };

export interface AuditSandboxDetail extends AuditSandboxRow {
  timeout?: number | null;
  network?: unknown;
  apiKey?: string | null;
  timeline: AuditTimelineItem[];
}

export interface AuditListParams {
  days: number;
  q?: string;
  template?: string;
  page?: number;
  size?: number;
}

async function get<T>(path: string, params: Record<string, string | number | undefined>): Promise<T> {
  const qs = Object.entries(params)
    .filter(([, v]) => v !== undefined && v !== '')
    .map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`)
    .join('&');
  const resp = await fetch(`${AUDIT_BASE}${path}${qs ? `?${qs}` : ''}`, {
    headers: { Accept: 'application/json' },
  });
  if (!resp.ok) {
    let msg = `${resp.status} ${resp.statusText}`;
    try {
      const body = (await resp.json()) as { error?: string };
      if (body?.error) msg = body.error;
    } catch {
      /* ignore */
    }
    throw new Error(msg);
  }
  return (await resp.json()) as T;
}

export const auditApi = {
  list: (p: AuditListParams) =>
    get<AuditListResponse>('/sandboxes', { days: p.days, q: p.q, template: p.template, page: p.page, size: p.size }),
  detail: (id: string, days: number) => get<AuditSandboxDetail>(`/sandboxes/${encodeURIComponent(id)}`, { days }),
};
