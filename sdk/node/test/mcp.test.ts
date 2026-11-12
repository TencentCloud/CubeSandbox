// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { Commands } from "../src/commands.js";
import { Config } from "../src/config.js";
import { ApiError, CubeSandboxError, TemplateNotFoundError } from "../src/exceptions.js";
import { Filesystem } from "../src/filesystem.js";
import { MCP_PORT, Sandbox, type McpServer } from "../src/index.js";
import { stubCubeEnv } from "./_env.js";

stubCubeEnv();

const SANDBOX_DATA = { sandboxID: "sb-mcp", templateID: "mcp-gateway", domain: "cube.app" };
const MCP: McpServer = {
  duckduckgo: {},
  "github/acme/tools": { runCmd: "echo 'hi'", envs: { K: "v" } },
};

interface Recorded {
  method: string;
  path: string;
  body: string;
}

let server: Server;
let port: number;
let requests: Recorded[] = [];
let createStatus = 201;
let createBody: unknown = SANDBOX_DATA;
let missingTemplates = new Set<string>();
let legacyNotFound = false;

beforeAll(async () => {
  server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      requests.push({ method: req.method ?? "", path: req.url ?? "", body: Buffer.concat(chunks).toString() });
      if (req.method === "DELETE") {
        res.writeHead(204).end();
        return;
      }
      const tpl = req.method === "POST" ? JSON.parse(Buffer.concat(chunks).toString() || "{}").templateID : "";
      if (missingTemplates.has(tpl)) {
        const message = legacyNotFound
          ? `CubeMaster returned error code 130404: failed to resolve template identifier "${tpl}": template not found`
          : `template ${tpl} not found: template not found`;
        res.writeHead(legacyNotFound ? 500 : 404, { "content-type": "application/json" });
        res.end(JSON.stringify({ code: legacyNotFound ? 500 : 404, message }));
        return;
      }
      res.writeHead(createStatus, { "content-type": "application/json" });
      res.end(JSON.stringify(createBody));
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
  createStatus = 201;
  createBody = SANDBOX_DATA;
  missingTemplates = new Set();
  legacyNotFound = false;
  vi.restoreAllMocks();
});

function makeConfig(extra: Record<string, unknown> = {}): Config {
  return new Config({ apiUrl: `http://127.0.0.1:${port}`, templateId: "tpl-default", ...extra });
}

function createBodyOf(): Record<string, any> {
  return JSON.parse(requests.find((r) => r.method === "POST")!.body);
}

function mockRun(result = { stdout: "ready", stderr: "", exitCode: 0 }) {
  return vi.spyOn(Commands.prototype, "run").mockResolvedValue(result);
}

describe("Sandbox.create with mcp", () => {
  it("defaults to the mcp-gateway template and sends mcp", async () => {
    mockRun();
    await Sandbox.create({ mcp: MCP, config: makeConfig() });
    const body = createBodyOf();
    expect(body.templateID).toBe("mcp-gateway");
    expect(body.mcp).toEqual(MCP);
  });

  it("honours mcpTemplateId and an explicit template", async () => {
    mockRun();
    await Sandbox.create({ mcp: {}, config: makeConfig({ mcpTemplateId: "tpl-mcp" }) });
    expect(createBodyOf().templateID).toBe("tpl-mcp");
    requests = [];
    await Sandbox.create({ mcp: {}, template: "tpl-explicit", config: makeConfig() });
    expect(createBodyOf().templateID).toBe("tpl-explicit");
  });

  const postedTemplates = () =>
    requests.filter((r) => r.method === "POST").map((r) => JSON.parse(r.body).templateID);

  it.each([false, true])("falls back to templateId when the mcp template is missing (legacy=%s)", async (legacy) => {
    const run = mockRun();
    missingTemplates = new Set(["mcp-gateway"]);
    legacyNotFound = legacy;
    await Sandbox.create({ mcp: MCP, config: makeConfig() });
    expect(postedTemplates()).toEqual(["mcp-gateway", "tpl-default"]);
    expect(run).toHaveBeenCalledOnce();
  });

  it("throws when no candidate template exists", async () => {
    const run = mockRun();
    missingTemplates = new Set(["mcp-gateway", "tpl-default"]);
    const err = await Sandbox.create({ mcp: MCP, config: makeConfig() }).catch((e) => e);
    expect(err).toBeInstanceOf(TemplateNotFoundError);
    expect(err.message).toContain('"mcp-gateway", "tpl-default"');
    expect(postedTemplates()).toEqual(["mcp-gateway", "tpl-default"]);
    expect(run).not.toHaveBeenCalled();
  });

  it("does not fall back from an explicit template or on other errors", async () => {
    mockRun();
    missingTemplates = new Set(["tpl-x"]);
    await expect(Sandbox.create({ mcp: MCP, template: "tpl-x", config: makeConfig() })).rejects.toBeInstanceOf(
      TemplateNotFoundError,
    );
    expect(postedTemplates()).toEqual(["tpl-x"]);
    requests = [];
    createStatus = 500;
    createBody = { message: "boom" };
    await expect(Sandbox.create({ mcp: MCP, config: makeConfig() })).rejects.toBeInstanceOf(ApiError);
    expect(postedTemplates()).toEqual(["mcp-gateway"]);
  });

  it("sends extra.templateID verbatim without falling back", async () => {
    mockRun();
    await Sandbox.create({ extra: { templateID: "tpl-extra" }, config: makeConfig() });
    expect(postedTemplates()).toEqual(["tpl-extra"]);
    requests = [];
    missingTemplates = new Set(["tpl-extra"]);
    await expect(
      Sandbox.create({ mcp: MCP, extra: { templateID: "tpl-extra" }, config: makeConfig() }),
    ).rejects.toBeInstanceOf(TemplateNotFoundError);
    expect(postedTemplates()).toEqual(["tpl-extra"]);
  });

  it("names the template when it has no mcp-gateway", async () => {
    mockRun({ stdout: "", stderr: "/bin/bash: mcp-gateway: command not found", exitCode: 127 });
    await expect(Sandbox.create({ mcp: MCP, config: makeConfig() })).rejects.toThrow(
      'template "mcp-gateway" does not provide mcp-gateway',
    );
  });

  it("does not start the gateway without mcp", async () => {
    const run = mockRun();
    await Sandbox.create({ config: makeConfig() });
    const body = createBodyOf();
    expect(body.templateID).toBe("tpl-default");
    expect(body.mcp).toBeUndefined();
    expect(run).not.toHaveBeenCalled();
  });

  it("starts mcp-gateway as root with a fresh token", async () => {
    const run = mockRun();
    const sb = await Sandbox.create({ mcp: MCP, config: makeConfig() });
    const [cmd, opts] = run.mock.calls[0];
    expect(cmd).toBe(
      `mcp-gateway --config '${JSON.stringify(MCP).replace(/'/g, `'\\''`)}'`,
    );
    expect(opts?.user).toBe("root");
    const token = opts?.envs?.GATEWAY_ACCESS_TOKEN;
    expect(token).toMatch(/^[0-9a-f-]{36}$/);
    expect(opts?.timeoutMs).toBe(60_000);
    await expect(sb.getMcpToken()).resolves.toBe(token);
    expect(requests.some((r) => r.method === "DELETE")).toBe(false);
  });

  it("kills the sandbox and throws when the gateway fails", async () => {
    mockRun({ stdout: "", stderr: 'mcp server "duckduckgo": boom', exitCode: 1 });
    await expect(Sandbox.create({ mcp: MCP, config: makeConfig() })).rejects.toThrow(
      new CubeSandboxError('Failed to start MCP gateway: mcp server "duckduckgo": boom'),
    );
    expect(requests.filter((r) => r.method === "DELETE").map((r) => r.path)).toEqual(["/sandboxes/sb-mcp"]);
  });

  it("kills the sandbox when starting the gateway throws", async () => {
    vi.spyOn(Commands.prototype, "run").mockRejectedValue(new Error("stream reset"));
    await expect(Sandbox.create({ mcp: MCP, config: makeConfig() })).rejects.toThrow("stream reset");
    expect(requests.some((r) => r.method === "DELETE")).toBe(true);
  });

  it("surfaces the API rejection of an invalid mcp shape", async () => {
    const run = mockRun();
    createStatus = 400;
    createBody = { code: 400, message: "mcp must be an object keyed by MCP server name" };
    await expect(
      Sandbox.create({ mcp: ["duckduckgo"] as unknown as McpServer, config: makeConfig() }),
    ).rejects.toBeInstanceOf(ApiError);
    expect(run).not.toHaveBeenCalled();
  });
});

describe("Sandbox MCP accessors", () => {
  const sandbox = (cfg = makeConfig()) => new Sandbox(SANDBOX_DATA, cfg);

  it("builds the gateway URL on the MCP port", () => {
    expect(MCP_PORT).toBe(50005);
    expect(sandbox().getMcpUrl()).toBe("http://50005-sb-mcp.cube.app/mcp");
    expect(sandbox(makeConfig({ proxyScheme: "https" })).getMcpUrl()).toBe(
      "https://50005-sb-mcp.cube.app/mcp",
    );
  });

  it("reads and caches the token from the sandbox", async () => {
    const exists = vi.spyOn(Filesystem.prototype, "exists").mockResolvedValue(true);
    const read = vi.spyOn(Filesystem.prototype, "read").mockResolvedValue("tok\n");
    const sb = sandbox();
    await expect(sb.getMcpToken()).resolves.toBe("tok");
    await expect(sb.getMcpToken()).resolves.toBe("tok");
    expect(exists).toHaveBeenCalledWith("/etc/mcp-gateway/.token");
    expect(read).toHaveBeenCalledTimes(1);
    expect(read).toHaveBeenCalledWith("/etc/mcp-gateway/.token", { user: "root" });
  });

  it("returns undefined when the gateway was never started", async () => {
    vi.spyOn(Filesystem.prototype, "exists").mockResolvedValue(false);
    await expect(sandbox().getMcpToken()).resolves.toBeUndefined();
  });
});
