// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";

import { ApiError } from "../src/exceptions.js";
import {
  convertE2BPerHostRules,
  normalizeRulesArg,
  renderInject,
  serializeRule,
  validateAllowOutDomainsRequireDenyAll,
  type Rule,
} from "../src/policy.js";

describe("serializeRule", () => {
  it("drops undefined match fields and always includes allow", () => {
    const wire = serializeRule({
      name: "r1",
      match: { host: "example.com" },
      action: { allow: false },
    });
    expect(wire).toEqual({
      name: "r1",
      match: { host: "example.com" },
      action: { allow: false },
    });
    expect((wire.match as Record<string, unknown>).scheme).toBeUndefined();
  });

  it("serializes a fully populated match", () => {
    const wire = serializeRule({
      name: "deepseek_api",
      match: {
        scheme: "https",
        host: "api.deepseek.com",
        method: ["POST"],
        path: "/v1/chat",
        sni: "api.deepseek.com",
      },
      action: { allow: true, audit: "metadata" },
    });
    expect(wire).toEqual({
      name: "deepseek_api",
      match: {
        scheme: "https",
        host: "api.deepseek.com",
        method: ["POST"],
        path: "/v1/chat",
        sni: "api.deepseek.com",
      },
      action: { allow: true, audit: "metadata" },
    });
  });

  it("serializes injected credentials", () => {
    const wire = serializeRule({
      name: "r1",
      match: { host: "api.example.com" },
      action: {
        allow: true,
        inject: [{ header: "Authorization", format: "Bearer ${SECRET}", secret: "sk_xxx" }],
      },
    });
    expect((wire.action as Record<string, unknown>).inject).toEqual([
      { header: "Authorization", secret: "sk_xxx", format: "Bearer ${SECRET}" },
    ]);
  });
});

describe("renderInject", () => {
  it("substitutes the secret into a custom format", () => {
    expect(
      renderInject({ header: "Authorization", format: "Bearer ${SECRET}", secret: "sk_xxx" }),
    ).toBe("Bearer sk_xxx");
  });

  it("defaults to the bare secret when no format is provided", () => {
    expect(renderInject({ header: "X-Token", secret: "abc" })).toBe("abc");
  });
});

describe("convertE2BPerHostRules", () => {
  it("maps a single host + single header to one rule", () => {
    const rules = convertE2BPerHostRules({
      "api.example.com": [{ transform: { headers: { "X-Header": "Content" } } }],
    });
    expect(rules).toEqual([
      {
        name: "e2b-transform-api.example.com",
        match: { host: "api.example.com" },
        action: { allow: true, inject: [{ header: "X-Header", secret: "Content" }] },
      },
    ]);
  });

  it("preserves header insertion order for a single entry", () => {
    const rules = convertE2BPerHostRules({
      "api.example.com": [
        { transform: { headers: { Authorization: "Bearer sk_xxx", "X-Trace": "on" } } },
      ],
    });
    expect(rules[0].action.inject).toEqual([
      { header: "Authorization", secret: "Bearer sk_xxx" },
      { header: "X-Trace", secret: "on" },
    ]);
  });

  it("indexes rule names when a host has multiple entries", () => {
    const rules = convertE2BPerHostRules({
      "api.example.com": [
        { transform: { headers: { "X-A": "1" } } },
        { transform: { headers: { "X-B": "2" } } },
      ],
    });
    expect(rules.map((r) => r.name)).toEqual([
      "e2b-transform-api.example.com-0",
      "e2b-transform-api.example.com-1",
    ]);
  });

  it("fans out multiple hosts preserving order", () => {
    const rules = convertE2BPerHostRules({
      "api.example.com": [{ transform: { headers: { "X-A": "1" } } }],
      "api.other.com": [{ transform: { headers: { "X-B": "2" } } }],
    });
    expect(rules.map((r) => r.match.host)).toEqual(["api.example.com", "api.other.com"]);
  });
});

describe("normalizeRulesArg", () => {
  it("returns an empty array for undefined input", () => {
    expect(normalizeRulesArg(undefined)).toEqual([]);
  });

  it("passes a typed Rule array through unchanged", () => {
    const rules: Rule[] = [
      { name: "r1", match: { host: "x.com" }, action: { allow: true } },
    ];
    expect(normalizeRulesArg(rules)).toEqual(rules);
  });

  it("converts an E2B per-host mapping into rules", () => {
    const rules = normalizeRulesArg({
      "api.example.com": [{ transform: { headers: { "X-Header": "Content" } } }],
    });
    expect(rules).toEqual([
      {
        name: "e2b-transform-api.example.com",
        match: { host: "api.example.com" },
        action: { allow: true, inject: [{ header: "X-Header", secret: "Content" }] },
      },
    ]);
  });
});

describe("validateAllowOutDomainsRequireDenyAll", () => {
  it("throws ApiError(400) when a domain is allowed without deny-all", () => {
    let thrown: unknown;
    try {
      validateAllowOutDomainsRequireDenyAll(["api.example.com"], ["203.0.113.0/24"]);
    } catch (err) {
      thrown = err;
    }
    expect(thrown).toBeInstanceOf(ApiError);
    expect((thrown as ApiError).statusCode).toBe(400);
  });

  it("passes when 0.0.0.0/0 is present in denyOut", () => {
    expect(() =>
      validateAllowOutDomainsRequireDenyAll(["api.example.com"], ["0.0.0.0/0"]),
    ).not.toThrow();
  });

  it("passes when defaultDenyAll is true", () => {
    expect(() =>
      validateAllowOutDomainsRequireDenyAll(["api.example.com"], undefined, true),
    ).not.toThrow();
  });

  it("ignores pure-IP / CIDR allowOut targets", () => {
    expect(() => validateAllowOutDomainsRequireDenyAll(["8.8.8.8/32"])).not.toThrow();
    expect(() => validateAllowOutDomainsRequireDenyAll(["8.8.8.8"])).not.toThrow();
    expect(() => validateAllowOutDomainsRequireDenyAll([])).not.toThrow();
  });

  it("accepts wildcard domains when deny-all is present", () => {
    expect(() =>
      validateAllowOutDomainsRequireDenyAll(["*.example.com"], ["0.0.0.0/0"]),
    ).not.toThrow();
  });
});
