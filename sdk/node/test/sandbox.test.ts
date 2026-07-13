// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { createServer, type IncomingHttpHeaders, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { Config } from "../src/config.js";
import {
  ApiError,
  AuthenticationError,
  SandboxNotFoundError,
  TemplateNotFoundError,
} from "../src/exceptions.js";
import { Sandbox } from "../src/index.js";
import { stubCubeEnv } from "./_env.js";

stubCubeEnv();

const SANDBOX_ID = "sb-test-001";
const DOMAIN = "cube.app";
const SANDBOX_DATA = {
  sandboxID: SANDBOX_ID,
  templateID: "tpl-test",
  domain: DOMAIN,
  state: "running",
  cpuCount: 2,
  memoryMB: 512,
};

interface RecordedRequest {
  method: string;
  pathname: string;
  url: URL;
  headers: IncomingHttpHeaders;
  body: Buffer;
}

interface MockResponse {
  status?: number;
  json?: unknown;
  text?: string;
  ndjson?: string;
  headers?: Record<string, string>;
}

type Handler = (req: RecordedRequest) => MockResponse | Promise<MockResponse>;

let server: Server;
let port: number;
let requests: RecordedRequest[] = [];
let handler: Handler = () => ({ status: 201, json: SANDBOX_DATA });

function setHandler(h: Handler): void {
  handler = h;
}

function makeConfig(): Config {
  return new Config({
    apiUrl: `http://127.0.0.1:${port}`,
    templateId: "tpl-test",
    proxyNodeIp: "127.0.0.1",
    proxyPort: port,
    sandboxDomain: DOMAIN,
  });
}

beforeAll(async () => {
  server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", async () => {
      const url = new URL(req.url ?? "/", "http://localhost");
      const record: RecordedRequest = {
        method: req.method ?? "",
        pathname: url.pathname,
        url,
        headers: req.headers,
        body: Buffer.concat(chunks),
      };
      requests.push(record);
      let out: MockResponse;
      try {
        out = await handler(record);
      } catch {
        out = { status: 500, json: { message: "handler threw" } };
      }
      const headers: Record<string, string> = { ...(out.headers ?? {}) };
      let payload = "";
      if (out.ndjson !== undefined) {
        payload = out.ndjson;
      } else if (out.text !== undefined) {
        payload = out.text;
      } else if (out.json !== undefined) {
        payload = JSON.stringify(out.json);
        headers["content-type"] ??= "application/json";
      }
      res.writeHead(out.status ?? 200, headers);
      res.end(payload);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, resolve));
  port = (server.address() as AddressInfo).port;
});

afterAll(() => {
  server.close();
});

afterEach(() => {
  requests = [];
});

describe("Sandbox.create", () => {
  it("returns a sandbox with fields parsed from the response", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb = await Sandbox.create({ config: makeConfig() });
    expect(sb.sandboxId).toBe(SANDBOX_ID);
    expect(sb.templateId).toBe("tpl-test");
    expect(sb.domain).toBe(DOMAIN);
    sb.close();
  });

  it("rejects when no template can be resolved", async () => {
    const cfg = makeConfig();
    cfg.templateId = null;
    await expect(Sandbox.create({ config: cfg })).rejects.toThrow(/template is required/);
  });

  it("sends templateID and timeout on the wire", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb = await Sandbox.create({ template: "tpl-foo", timeout: 600, config: makeConfig() });
    const body = JSON.parse(requests[0].body.toString());
    expect(body.templateID).toBe("tpl-foo");
    expect(body.timeout).toBe(600);
    sb.close();
  });

  it("sends envVars and metadata", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb = await Sandbox.create({
      envVars: { FOO: "bar" },
      metadata: { "network-policy": "deny-all" },
      config: makeConfig(),
    });
    const body = JSON.parse(requests[0].body.toString());
    expect(body.envVars).toEqual({ FOO: "bar" });
    expect(body.metadata).toEqual({ "network-policy": "deny-all" });
    sb.close();
  });

  it("emits allow_internet_access only when false", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb1 = await Sandbox.create({ allowInternetAccess: false, config: makeConfig() });
    expect(JSON.parse(requests[0].body.toString()).allow_internet_access).toBe(false);
    sb1.close();

    requests = [];
    const sb2 = await Sandbox.create({ config: makeConfig() });
    expect("allow_internet_access" in JSON.parse(requests[0].body.toString())).toBe(false);
    sb2.close();
  });

  it("maps network allowOut / denyOut", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb = await Sandbox.create({
      network: { allowOut: ["8.8.8.8/32"], denyOut: ["0.0.0.0/0"] },
      config: makeConfig(),
    });
    const body = JSON.parse(requests[0].body.toString());
    expect(body.network.allowOut).toEqual(["8.8.8.8/32"]);
    expect(body.network.denyOut).toEqual(["0.0.0.0/0"]);
    sb.close();
  });

  it("omits the network block for empty network options", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb = await Sandbox.create({ network: {}, config: makeConfig() });
    expect("network" in JSON.parse(requests[0].body.toString())).toBe(false);
    sb.close();
  });

  it("serializes typed L7 rules under network.rules", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb = await Sandbox.create({
      network: {
        rules: [
          {
            name: "github_api",
            match: { host: "api.github.com", path: "/repos/*" },
            action: { allow: true, audit: "metadata" },
          },
        ],
      },
      config: makeConfig(),
    });
    const body = JSON.parse(requests[0].body.toString());
    expect(body.network.rules).toEqual([
      {
        name: "github_api",
        match: { host: "api.github.com", path: "/repos/*" },
        action: { allow: true, audit: "metadata" },
      },
    ]);
    sb.close();
  });

  it("maps lifecycle to camelCase wire keys", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb1 = await Sandbox.create({
      lifecycle: { onTimeout: "pause" },
      config: makeConfig(),
    });
    expect(JSON.parse(requests[0].body.toString()).lifecycle).toEqual({ onTimeout: "pause" });
    sb1.close();

    requests = [];
    const sb2 = await Sandbox.create({
      lifecycle: { onTimeout: "pause", autoResume: true },
      config: makeConfig(),
    });
    expect(JSON.parse(requests[0].body.toString()).lifecycle).toEqual({
      onTimeout: "pause",
      autoResume: true,
    });
    sb2.close();
  });

  it("omits lifecycle when not provided", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb = await Sandbox.create({ config: makeConfig() });
    expect("lifecycle" in JSON.parse(requests[0].body.toString())).toBe(false);
    sb.close();
  });

  it("accepts inline config fields (templateId + apiUrl) per Issue #760", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    // No `config` and no `template` — only the flat Issue #760 shape.
    const sb = await Sandbox.create({
      apiUrl: `http://127.0.0.1:${port}`,
      templateId: "tpl-inline",
      proxyNodeIp: "127.0.0.1",
      proxyPort: port,
      sandboxDomain: DOMAIN,
    });
    expect(JSON.parse(requests[0].body.toString()).templateID).toBe("tpl-inline");
    expect(sb.config.apiUrl).toBe(`http://127.0.0.1:${port}`);
    expect(sb.config.proxyNodeIp).toBe("127.0.0.1");
    sb.close();
  });

  it("lets inline fields override a passed config object", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb = await Sandbox.create({
      config: makeConfig(),
      apiUrl: `http://127.0.0.1:${port}`,
      proxyScheme: "https",
      sandboxDomain: "override.app",
    });
    // Flat fields win over the config object.
    expect(sb.config.proxyScheme).toBe("https");
    expect(sb.config.sandboxDomain).toBe("override.app");
    sb.close();
  });

  it("prefers explicit template over templateId over config.templateId", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const cfg = makeConfig();
    cfg.templateId = "tpl-from-config";
    const sb = await Sandbox.create({
      template: "tpl-explicit",
      templateId: "tpl-alias",
      config: cfg,
    });
    expect(JSON.parse(requests[0].body.toString()).templateID).toBe("tpl-explicit");
    sb.close();
  });

  it("exposes the traffic access token when returned", async () => {
    setHandler(() => ({
      status: 201,
      json: { ...SANDBOX_DATA, trafficAccessToken: "tok-123" },
    }));
    const sb = await Sandbox.create({ config: makeConfig() });
    expect(sb.trafficAccessToken).toBe("tok-123");
    sb.close();
  });

  it("returns a null traffic access token by default", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb = await Sandbox.create({ config: makeConfig() });
    expect(sb.trafficAccessToken).toBeNull();
    sb.close();
  });
});

describe("Sandbox control-plane error mapping", () => {
  it("maps 401 to AuthenticationError", async () => {
    setHandler(() => ({ status: 401, json: { message: "unauthorized" } }));
    await expect(Sandbox.create({ config: makeConfig() })).rejects.toBeInstanceOf(
      AuthenticationError,
    );
  });

  it("maps 404 with a template message to TemplateNotFoundError", async () => {
    setHandler(() => ({ status: 404, json: { message: "template not found" } }));
    await expect(Sandbox.create({ config: makeConfig() })).rejects.toBeInstanceOf(
      TemplateNotFoundError,
    );
  });

  it("maps 404 without a template message to SandboxNotFoundError", async () => {
    setHandler(() => ({ status: 404, json: { message: "sandbox gone" } }));
    await expect(Sandbox.connect(SANDBOX_ID, { config: makeConfig() })).rejects.toBeInstanceOf(
      SandboxNotFoundError,
    );
  });

  it("maps other non-2xx responses to ApiError with a status code", async () => {
    setHandler(() => ({ status: 500, json: { message: "internal error" } }));
    let thrown: unknown;
    try {
      await Sandbox.create({ config: makeConfig() });
    } catch (err) {
      thrown = err;
    }
    expect(thrown).toBeInstanceOf(ApiError);
    expect((thrown as ApiError).statusCode).toBe(500);
  });
});

describe("Sandbox list / health", () => {
  it("lists sandboxes (v1)", async () => {
    setHandler((req) => {
      expect(req.pathname).toBe("/sandboxes");
      expect(req.method).toBe("GET");
      return { status: 200, json: [SANDBOX_DATA] };
    });
    const result = await Sandbox.list(makeConfig());
    expect(result).toHaveLength(1);
    expect(result[0].sandboxID).toBe(SANDBOX_ID);
  });

  it("lists sandboxes (v2) from the /v2 endpoint", async () => {
    setHandler((req) => {
      expect(req.pathname).toBe("/v2/sandboxes");
      return { status: 200, json: [] };
    });
    expect(await Sandbox.listV2(makeConfig())).toEqual([]);
  });

  it("reports health", async () => {
    setHandler((req) => {
      expect(req.pathname).toBe("/health");
      return { status: 200, json: { status: "ok", sandboxes: 2 } };
    });
    const health = await Sandbox.health(makeConfig());
    expect(health.status).toBe("ok");
    expect(health.sandboxes).toBe(2);
  });
});

describe("Sandbox instance operations", () => {
  async function createSandbox(): Promise<Sandbox> {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb = await Sandbox.create({ config: makeConfig() });
    requests = [];
    return sb;
  }

  it("builds virtual hostnames from port + id + domain", async () => {
    const sb = await createSandbox();
    expect(sb.getHost(49999)).toBe(`49999-${SANDBOX_ID}.${DOMAIN}`);
    expect(sb.getHost(8080)).toBe(`8080-${SANDBOX_ID}.${DOMAIN}`);
    sb.close();
  });

  it("fetches info via GET /sandboxes/:id", async () => {
    const sb = await createSandbox();
    setHandler((req) => {
      expect(req.pathname).toBe(`/sandboxes/${SANDBOX_ID}`);
      expect(req.method).toBe("GET");
      return { status: 200, json: { ...SANDBOX_DATA, state: "paused" } };
    });
    const info = await sb.getInfo();
    expect(info.state).toBe("paused");
    sb.close();
  });

  it("kills via DELETE /sandboxes/:id", async () => {
    const sb = await createSandbox();
    setHandler((req) => {
      expect(req.method).toBe("DELETE");
      expect(req.pathname).toBe(`/sandboxes/${SANDBOX_ID}`);
      return { status: 204 };
    });
    await sb.kill();
    expect(requests).toHaveLength(1);
    sb.close();
  });

  it("pauses and polls getInfo until the sandbox is paused", async () => {
    const sb = await createSandbox();
    let getCalls = 0;
    setHandler((req) => {
      if (req.method === "POST" && req.pathname.endsWith("/pause")) {
        return { status: 204 };
      }
      // GET /sandboxes/:id
      getCalls += 1;
      return {
        status: 200,
        json: { ...SANDBOX_DATA, state: getCalls >= 2 ? "paused" : "running" },
      };
    });
    await sb.pause({ wait: true, intervalMs: 0 });
    expect(getCalls).toBeGreaterThanOrEqual(2);
    sb.close();
  });

  it("throws a paused-timeout error when the state never flips", async () => {
    const sb = await createSandbox();
    setHandler((req) => {
      if (req.method === "POST" && req.pathname.endsWith("/pause")) {
        return { status: 204 };
      }
      return { status: 200, json: { ...SANDBOX_DATA, state: "running" } };
    });
    await expect(sb.pause({ wait: true, timeoutMs: 20, intervalMs: 1 })).rejects.toThrow(
      /paused/,
    );
    sb.close();
  });
});

describe("Sandbox.runCode", () => {
  async function createSandbox(): Promise<Sandbox> {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb = await Sandbox.create({ config: makeConfig() });
    requests = [];
    return sb;
  }

  it("streams ndjson into an Execution and fires callbacks", async () => {
    const sb = await createSandbox();
    const ndjson =
      '{"type":"stdout","text":"hello\\n","timestamp":123}\n' +
      '{"type":"result","text":"2","is_main_result":true}\n' +
      '{"type":"number_of_executions","execution_count":1}\n';
    setHandler((req) => {
      expect(req.method).toBe("POST");
      expect(req.pathname).toBe("/execute");
      expect(req.headers.host).toBe(`49999-${SANDBOX_ID}.${DOMAIN}`);
      const body = JSON.parse(req.body.toString());
      expect(body.code).toBe("1+1");
      return { status: 200, ndjson };
    });

    const stdout: string[] = [];
    const results: Array<string | undefined> = [];
    const execution = await sb.runCode("1+1", {
      onStdout: (m) => stdout.push(m.text),
      onResult: (r) => results.push(r.text),
    });

    expect(execution.text).toBe("2");
    expect(execution.logs.stdout).toEqual(["hello\n"]);
    expect(execution.executionCount).toBe(1);
    expect(stdout).toEqual(["hello\n"]);
    expect(results).toEqual(["2"]);
    sb.close();
  });

  it("captures execution errors from the stream", async () => {
    const sb = await createSandbox();
    const ndjson =
      '{"type":"error","name":"ValueError","value":"boom","traceback":["line1","line2"]}\n';
    setHandler(() => ({ status: 200, ndjson }));

    const errors: string[] = [];
    const execution = await sb.runCode("raise ValueError('boom')", {
      onError: (e) => errors.push(e.name),
    });

    expect(execution.error?.name).toBe("ValueError");
    expect(execution.error?.traceback).toBe("line1\nline2");
    expect(errors).toEqual(["ValueError"]);
    sb.close();
  });
});

describe("control-plane request timeout", () => {
  it("aborts a management call that exceeds requestTimeoutMs", async () => {
    setHandler(async () => {
      await new Promise((r) => setTimeout(r, 300));
      return { status: 200, json: [] };
    });
    const cfg = new Config({
      apiUrl: `http://127.0.0.1:${port}`,
      templateId: "tpl-test",
      requestTimeoutMs: 60,
    });
    await expect(Sandbox.list(cfg)).rejects.toThrow();
  });

  it("completes when the response returns within requestTimeoutMs", async () => {
    setHandler(() => ({ status: 200, json: [] }));
    const cfg = new Config({
      apiUrl: `http://127.0.0.1:${port}`,
      templateId: "tpl-test",
      requestTimeoutMs: 1000,
    });
    await expect(Sandbox.list(cfg)).resolves.toEqual([]);
  });
});

describe("Sandbox.trafficTokenHeaders", () => {
  it("sends both e2b- and cube- traffic tokens when the sandbox is restricted", async () => {
    setHandler(() => ({ status: 201, json: { ...SANDBOX_DATA, trafficAccessToken: "tok-123" } }));
    const sb = await Sandbox.create({ config: makeConfig() });
    expect(sb.trafficTokenHeaders()).toEqual({
      "e2b-traffic-access-token": "tok-123",
      "cube-traffic-access-token": "tok-123",
    });
    sb.close();
  });

  it("sends no traffic-token headers for a public sandbox", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
    const sb = await Sandbox.create({ config: makeConfig() });
    expect(sb.trafficTokenHeaders()).toEqual({});
    sb.close();
  });
});
