// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { Config } from "../src/config.js";

const ENV_KEYS = [
  "CUBE_API_URL",
  "CUBE_API_KEY",
  "E2B_API_KEY",
  "CUBE_TEMPLATE_ID",
  "CUBE_PROXY_NODE_IP",
  "CUBE_PROXY_PORT_HTTP",
  "CUBE_PROXY_SCHEME",
  "CUBE_SANDBOX_DOMAIN",
] as const;

describe("Config", () => {
  let saved: Record<string, string | undefined>;

  beforeEach(() => {
    saved = {};
    for (const key of ENV_KEYS) {
      saved[key] = process.env[key];
      delete process.env[key];
    }
  });

  afterEach(() => {
    for (const key of ENV_KEYS) {
      const value = saved[key];
      if (value === undefined) {
        delete process.env[key];
      } else {
        process.env[key] = value;
      }
    }
  });

  it("uses documented defaults when nothing is provided", () => {
    const cfg = new Config();
    expect(cfg.apiUrl).toBe("http://127.0.0.1:3000");
    expect(cfg.apiKey).toBeNull();
    expect(cfg.templateId).toBeNull();
    expect(cfg.proxyNodeIp).toBeNull();
    expect(cfg.proxyPort).toBe(80);
    expect(cfg.proxyScheme).toBe("http");
    expect(cfg.sandboxDomain).toBe("cube.app");
    expect(cfg.timeout).toBe(300);
    expect(cfg.requestTimeoutMs).toBe(30000);
  });

  it("reads the API key from CUBE_API_KEY, then E2B_API_KEY", () => {
    process.env.E2B_API_KEY = "e2b-key";
    expect(new Config().apiKey).toBe("e2b-key");
    process.env.CUBE_API_KEY = "cube-key";
    expect(new Config().apiKey).toBe("cube-key"); // CUBE_ wins
    expect(new Config({ apiKey: "explicit" }).apiKey).toBe("explicit");
  });

  it("treats a blank API key as unset", () => {
    process.env.CUBE_API_KEY = "   ";
    expect(new Config().apiKey).toBeNull();
  });

  it("normalizes proxyScheme to http/https", () => {
    expect(new Config({ proxyScheme: "HTTPS" }).proxyScheme).toBe("https");
    expect(new Config({ proxyScheme: " http " }).proxyScheme).toBe("http");
    // Unknown scheme falls back by port: 443 → https, otherwise http.
    expect(new Config({ proxyScheme: "ftp" }).proxyScheme).toBe("http");
    expect(new Config({ proxyScheme: "ftp", proxyPort: 443 }).proxyScheme).toBe("https");
    expect(new Config({ proxyPort: 443 }).proxyScheme).toBe("https");
  });

  it("strips trailing slashes from apiUrl", () => {
    expect(new Config({ apiUrl: "http://localhost:3000/" }).apiUrl).toBe(
      "http://localhost:3000",
    );
    expect(new Config({ apiUrl: "http://localhost:3000///" }).apiUrl).toBe(
      "http://localhost:3000",
    );
  });

  it("reads CUBE_* environment variables", () => {
    process.env.CUBE_API_URL = "http://1.2.3.4:3000";
    process.env.CUBE_TEMPLATE_ID = "tpl-env";
    process.env.CUBE_PROXY_NODE_IP = "1.2.3.4";
    process.env.CUBE_PROXY_PORT_HTTP = "9090";
    process.env.CUBE_SANDBOX_DOMAIN = "mybox.io";

    const cfg = new Config();
    expect(cfg.apiUrl).toBe("http://1.2.3.4:3000");
    expect(cfg.templateId).toBe("tpl-env");
    expect(cfg.proxyNodeIp).toBe("1.2.3.4");
    expect(cfg.proxyPort).toBe(9090);
    expect(cfg.sandboxDomain).toBe("mybox.io");
  });

  it("falls back to port 80 when CUBE_PROXY_PORT_HTTP is invalid or out of range", () => {
    for (const bad of ["abc", "", "0", "-1", "65536", "99999"]) {
      process.env.CUBE_PROXY_PORT_HTTP = bad;
      const cfg = new Config();
      expect(cfg.proxyPort).toBe(80);
      expect(Number.isNaN(cfg.proxyPort)).toBe(false);
    }
  });

  it("accepts the maximum valid port from CUBE_PROXY_PORT_HTTP", () => {
    process.env.CUBE_PROXY_PORT_HTTP = "65535";
    expect(new Config().proxyPort).toBe(65535);
  });

  it("lets explicit options override environment variables", () => {
    process.env.CUBE_API_URL = "http://env-host:3000";
    process.env.CUBE_TEMPLATE_ID = "tpl-env";
    process.env.CUBE_PROXY_PORT_HTTP = "9090";

    const cfg = new Config({
      apiUrl: "http://explicit:4000",
      templateId: "tpl-explicit",
      proxyPort: 8080,
    });
    expect(cfg.apiUrl).toBe("http://explicit:4000");
    expect(cfg.templateId).toBe("tpl-explicit");
    expect(cfg.proxyPort).toBe(8080);
  });
});
