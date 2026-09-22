// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from "vitest";

import { ApiError } from "../src/exceptions.js";
import {
  convertE2BPerHostRules,
  normalizeRulesArg,
  renderInject,
  SECRET_MAX_BYTES,
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

  it("accepts an inject secret at the byte cap", () => {
    const secret = "x".repeat(SECRET_MAX_BYTES);
    const wire = serializeRule({
      name: "r1",
      match: { host: "api.example.com" },
      action: { allow: true, inject: [{ header: "Authorization", secret }] },
    });
    expect((wire.action as Record<string, unknown>).inject).toEqual([
      { header: "Authorization", secret },
    ]);
  });

  it("rejects an inject secret over the byte cap", () => {
    expect(() =>
      serializeRule({
        name: "r1",
        match: { host: "api.example.com" },
        action: {
          allow: true,
          inject: [{ header: "Authorization", secret: "x".repeat(SECRET_MAX_BYTES + 1) }],
        },
      }),
    ).toThrow("exceeds 2048 bytes");
  });

  it("emits port and normalizes scheme casing on the wire", () => {
    const wire = serializeRule({
      name: "r1",
      match: { host: "api.example.com", port: 8443, scheme: "HTTPS" as "https" },
      action: { allow: true },
    });
    expect(wire.match).toEqual({ host: "api.example.com", port: 8443, scheme: "https" });
  });

  it("rejects port without scheme", () => {
    expect(() =>
      serializeRule({
        name: "r1",
        match: { host: "api.example.com", port: 8443 },
        action: { allow: true },
      }),
    ).toThrow("match.port requires match.scheme to be set");
  });

  it("rejects out-of-range ports", () => {
    for (const port of [0, -1, 65536, 99999]) {
      expect(() =>
        serializeRule({
          name: "r1",
          match: { host: "api.example.com", port, scheme: "https" },
          action: { allow: true },
        }),
      ).toThrow("match.port must be in [1, 65535]");
    }
  });

  it("rejects non-integer ports", () => {
    expect(() =>
      serializeRule({
        name: "r1",
        match: { host: "api.example.com", port: 443.5, scheme: "https" },
        action: { allow: true },
      }),
    ).toThrow("match.port must be an int");
  });

  it("rejects unknown schemes", () => {
    expect(() =>
      serializeRule({
        name: "r1",
        match: { host: "api.example.com", scheme: "gopher" as "http" },
        action: { allow: true },
      }),
    ).toThrow("match.scheme must be 'http' or 'https'");
  });

  it("accepts scheme alone (legacy default-set filter)", () => {
    const wire = serializeRule({
      name: "r1",
      match: { host: "api.example.com", scheme: "https" },
      action: { allow: true },
    });
    expect(wire.match).toEqual({ host: "api.example.com", scheme: "https" });
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

  // CubeEgress substitutes only the first ${SECRET}
  // (`string.gsub(fmt, "%${SECRET}", escaped, 1)` in
  // CubeEgress/lua/access_phase.lua), so the preview must do the same. This
  // pins the string-pattern `.replace()` call — switching it to a global
  // regex or `replaceAll` would silently make the preview disagree with what
  // the sandbox's upstream receives.
  it("substitutes only the first placeholder, like the data plane", () => {
    expect(
      renderInject({ header: "Authorization", format: "Basic ${SECRET}:${SECRET}", secret: "tok" }),
    ).toBe("Basic tok:${SECRET}");
    expect(
      renderInject({ header: "Authorization", format: "${SECRET}-${SECRET}-${SECRET}", secret: "tok" }),
    ).toBe("tok-${SECRET}-${SECRET}");
  });

  // `String.prototype.replace` applies JS's `$` substitution rules to a *string*
  // replacement (`$$` becomes `$`, `$&` becomes the match, and the backtick
  // forms splice in surrounding text). CubeEgress builds its replacement with
  // Lua's `string.gsub`, which has no such rules, so a secret containing `$`
  // previews differently from what the upstream receives - and `$&` leaks the
  // literal `${SECRET}` placeholder into a header that is supposed to carry a
  // secret. A function replacer keeps the replace-once behaviour above while
  // neutering the expansion.
  it("does not apply $ substitution rules to the secret", () => {
    expect(renderInject({ header: "Authorization", secret: "pa$$word" })).toBe("pa$$word");
    expect(renderInject({ header: "Authorization", secret: "a$&b" })).toBe("a$&b");
    // The backtick form is spelled by concatenation so it cannot terminate the
    // surrounding template literal in this file.
    const dollarBacktick = "$`";
    expect(renderInject({ header: "Authorization", secret: "x" + dollarBacktick + "y" })).toBe(
      "x" + dollarBacktick + "y",
    );
  });

  // Lua falls back to `${SECRET}` when the format is absent *or empty*
  // (access_phase.lua), and the Python and Go renderers do the same. `??` only
  // covers null/undefined, so an empty string previewed as "" while the
  // upstream received the raw secret.
  it("falls back to the bare secret for an empty format", () => {
    expect(renderInject({ header: "Authorization", secret: "tok", format: "" })).toBe("tok");
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

  it("rejects a header value over the byte cap", () => {
    expect(() =>
      convertE2BPerHostRules({
        "api.example.com": [
          { transform: { headers: { Authorization: "x".repeat(SECRET_MAX_BYTES + 1) } } },
        ],
      }),
    ).toThrow("exceeds 2048 bytes");
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
