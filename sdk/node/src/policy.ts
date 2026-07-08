// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

/**
 * L7 egress policy types — host/path/SNI matching, audit, credential
 * injection.
 *
 * These are pure data holders on the SDK side; matching and evaluation happen
 * server-side. {@link serializeRule} emits the camelCase JSON shape that nests
 * under ``network.rules`` in the ``POST /sandboxes`` payload.
 */

import { ApiError } from "./exceptions.js";

export type Scheme = "http" | "https";

export type Method =
  | "GET"
  | "HEAD"
  | "POST"
  | "PUT"
  | "PATCH"
  | "DELETE"
  | "OPTIONS"
  | "CONNECT"
  | "TRACE";

export type AuditLevel = "full" | "metadata" | "none";

/**
 * Rule match conditions. All fields optional; an empty match matches any
 * request. Semantics: AND across fields, OR within ``method``. Comparisons on
 * sni/host/scheme are case-insensitive (server-enforced).
 */
export interface Match {
  sni?: string;
  host?: string;
  method?: Method[];
  path?: string;
  scheme?: Scheme;
}

/**
 * Credential injection. Only honored when ``Action.allow`` is true and the
 * request is HTTPS with matching SNI/Host (server enforces).
 */
export interface Inject {
  header: string;
  secret: string;
  format?: string;
}

/**
 * Rule action.
 *
 * - ``allow: true`` — pass the request through; optional credential injection.
 * - ``allow: false`` — reject (HTTP 403); ``inject`` is ignored if set.
 * - ``audit`` defaults to ``"metadata"`` server-side when omitted.
 */
export interface Action {
  allow: boolean;
  inject?: Inject[];
  audit?: AuditLevel;
}

/** Egress rule. ``name`` is a human-readable label used for audit logging. */
export interface Rule {
  name: string;
  match: Match;
  action: Action;
}

/** E2B per-host request transform entry. */
export interface E2BTransformEntry {
  transform: { headers: Record<string, string> };
}

/** E2B per-host request transforms (host -> list of transform entries). */
export type E2BPerHostRules = Record<string, E2BTransformEntry[]>;

/** Accepted shapes for ``network.rules``. */
export type NetworkRules = Rule[] | E2BPerHostRules;

/** Render the final injected header value (preview helper). */
export function renderInject(inject: Inject): string {
  const fmt = inject.format ?? "${SECRET}";
  return fmt.replace("${SECRET}", inject.secret);
}

function serializeMatch(match: Match): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (match.sni !== undefined) out.sni = match.sni;
  if (match.host !== undefined) out.host = match.host;
  if (match.method !== undefined) out.method = [...match.method];
  if (match.path !== undefined) out.path = match.path;
  if (match.scheme !== undefined) out.scheme = match.scheme;
  return out;
}

function serializeInject(inject: Inject): Record<string, unknown> {
  const out: Record<string, unknown> = { header: inject.header, secret: inject.secret };
  if (inject.format !== undefined) out.format = inject.format;
  return out;
}

function serializeAction(action: Action): Record<string, unknown> {
  const out: Record<string, unknown> = { allow: action.allow };
  if (action.audit !== undefined) out.audit = action.audit;
  if (action.inject !== undefined) out.inject = action.inject.map(serializeInject);
  return out;
}

/** Serialize a {@link Rule} to the wire JSON shape used by CubeEgress. */
export function serializeRule(rule: Rule): Record<string, unknown> {
  return {
    name: rule.name,
    match: serializeMatch(rule.match),
    action: serializeAction(rule.action),
  };
}

function isE2BPerHostRules(rules: NetworkRules): rules is E2BPerHostRules {
  return !Array.isArray(rules) && typeof rules === "object" && rules !== null;
}

function convertE2BTransformToInject(transform: {
  headers?: Record<string, string>;
}): Inject[] {
  if (typeof transform !== "object" || transform === null) {
    throw new Error("network.rules transform must be an object");
  }
  const headers = transform.headers;
  if (headers === undefined) {
    throw new Error("network.rules transform requires a 'headers' field");
  }
  if (typeof headers !== "object" || headers === null) {
    throw new Error("network.rules transform.headers must be an object");
  }
  const unknown = Object.keys(transform).filter((k) => k !== "headers");
  if (unknown.length > 0) {
    throw new Error(
      `network.rules transform has unsupported keys: ${JSON.stringify(unknown)}; ` +
        "only 'headers' is supported by the CubeEgress compatibility layer",
    );
  }
  const injects: Inject[] = [];
  for (const [name, value] of Object.entries(headers)) {
    if (!name) {
      throw new Error("network.rules transform.headers keys must be non-empty strings");
    }
    if (typeof value !== "string") {
      throw new Error(`network.rules transform.headers[${name}] must be a string`);
    }
    injects.push({ header: name, secret: value });
  }
  return injects;
}

/**
 * Convert E2B's per-host transform mapping into CubeEgress rules. Each
 * ``host -> [entry, ...]`` pair fans out into one rule per entry, preserving
 * list order. Generated rule names use the ``e2b-transform-<host>[-<index>]``
 * convention so compat-layer output is identifiable in audit logs.
 */
export function convertE2BPerHostRules(rules: E2BPerHostRules): Rule[] {
  const converted: Rule[] = [];
  for (const [host, entries] of Object.entries(rules)) {
    if (!host) {
      throw new Error("network.rules host keys must be non-empty strings");
    }
    if (!Array.isArray(entries)) {
      throw new Error(`network.rules[${host}] must be a list of transform entries`);
    }
    entries.forEach((entry, index) => {
      if (typeof entry !== "object" || entry === null) {
        throw new Error(`network.rules[${host}][${index}] must be an object`);
      }
      const transform = (entry as E2BTransformEntry).transform;
      if (transform === undefined) {
        throw new Error(
          `network.rules[${host}][${index}] is missing the 'transform' field`,
        );
      }
      const unknown = Object.keys(entry).filter((k) => k !== "transform");
      if (unknown.length > 0) {
        throw new Error(
          `network.rules[${host}][${index}] has unsupported keys: ` +
            `${JSON.stringify(unknown)}; only 'transform' is supported`,
        );
      }
      const injects = convertE2BTransformToInject(transform);
      const suffix = entries.length === 1 ? "" : `-${index}`;
      converted.push({
        name: `e2b-transform-${host}${suffix}`,
        match: { host },
        action: { allow: true, inject: injects },
      });
    });
  }
  return converted;
}

/**
 * Normalize the ``network.rules`` argument to a list of {@link Rule} suitable
 * for {@link serializeRule}. Accepts either a CubeEgress rule array or an E2B
 * per-host transform mapping.
 */
export function normalizeRulesArg(rules?: NetworkRules): Rule[] {
  if (!rules) {
    return [];
  }
  if (isE2BPerHostRules(rules)) {
    return convertE2BPerHostRules(rules);
  }
  return rules;
}

const DENY_ALL_IPV4_CIDR = "0.0.0.0/0";
const ALLOW_OUT_DOMAIN_REQUIRES_DENY_ALL =
  "When specifying allowed domains in allow_out, you must disable public " +
  "outbound traffic or include '0.0.0.0/0' in deny_out to block all other traffic.";

function isIpAddress(target: string): boolean {
  // IPv4 dotted-decimal.
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(target)) {
    return target.split(".").every((p) => Number(p) <= 255);
  }
  // IPv6 (loose check — anything with a colon).
  return target.includes(":");
}

function isDottedDecimalLike(target: string): boolean {
  const parts = target.replace(/\.+$/, "").split(".");
  return parts.length === 4 && parts.every((p) => p.length > 0 && /^\d+$/.test(p));
}

function isValidDnsDomainName(domain: string): boolean {
  if (!domain || domain.length >= 255) {
    return false;
  }
  return domain.split(".").every(
    (label) =>
      label.length > 0 &&
      label.length <= 63 &&
      !label.startsWith("-") &&
      !label.endsWith("-") &&
      /^[a-zA-Z0-9-]+$/.test(label),
  );
}

function isDomainAllowOutTarget(target: unknown): boolean {
  if (typeof target !== "string") {
    return false;
  }
  const trimmed = target.trim();
  if (!trimmed || trimmed.includes("/")) {
    return false;
  }
  if (isIpAddress(trimmed)) {
    return false;
  }
  if (isDottedDecimalLike(trimmed)) {
    return false;
  }
  let domain = trimmed.replace(/\.+$/, "").toLowerCase();
  if (domain.startsWith("*.")) {
    domain = domain.slice(2);
  } else if (domain.includes("*")) {
    return false;
  }
  return isValidDnsDomainName(domain);
}

/**
 * Enforce that allowing specific domains in ``allowOut`` is only permitted
 * when all other egress is denied (public traffic off, or ``0.0.0.0/0`` in
 * ``denyOut``). Throws {@link ApiError} otherwise. Mirrors the Python SDK.
 */
export function validateAllowOutDomainsRequireDenyAll(
  allowOut?: string[],
  denyOut?: string[],
  defaultDenyAll = false,
): void {
  if (!(allowOut ?? []).some(isDomainAllowOutTarget)) {
    return;
  }
  if (
    defaultDenyAll ||
    (denyOut ?? []).some((target) => String(target).trim() === DENY_ALL_IPV4_CIDR)
  ) {
    return;
  }
  throw new ApiError(ALLOW_OUT_DOMAIN_REQUIRES_DENY_ALL, 400);
}
