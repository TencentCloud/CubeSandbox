// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { createServer, type IncomingHttpHeaders, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { Config } from "../src/config.js";
import { TemplateNotFoundError } from "../src/exceptions.js";
import { Template } from "../src/index.js";

interface RecordedRequest {
  method: string;
  pathname: string;
  headers: IncomingHttpHeaders;
  body: Buffer;
}

interface MockResponse {
  status?: number;
  json?: unknown;
  text?: string;
}

type Handler = (req: RecordedRequest) => MockResponse;

let server: Server;
let port: number;
let requests: RecordedRequest[] = [];
let handler: Handler = () => ({ status: 200, json: {} });

function makeConfig(): Config {
  return new Config({ apiUrl: `http://127.0.0.1:${port}` });
}

beforeAll(async () => {
  server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const url = new URL(req.url ?? "/", "http://localhost");
      const record: RecordedRequest = {
        method: req.method ?? "",
        pathname: url.pathname,
        headers: req.headers,
        body: Buffer.concat(chunks),
      };
      requests.push(record);
      const out = handler(record);
      const headers: Record<string, string> = {};
      let payload = "";
      if (out.text !== undefined) {
        payload = out.text;
      } else if (out.json !== undefined) {
        payload = JSON.stringify(out.json);
        headers["content-type"] = "application/json";
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

describe("Template.list", () => {
  it("returns parsed TemplateInfo objects", async () => {
    handler = (req) => {
      expect(req.method).toBe("GET");
      expect(req.pathname).toBe("/templates");
      return {
        status: 200,
        json: [{ templateID: "tpl-a", status: "READY", aliases: ["alpha"] }],
      };
    };
    const templates = await Template.list({ config: makeConfig() });
    expect(templates).toHaveLength(1);
    expect(templates[0].templateId).toBe("tpl-a");
    expect(templates[0].status).toBe("READY");
  });
});

describe("Template.get", () => {
  it("fetches a single template and parses network fields", async () => {
    handler = (req) => {
      expect(req.pathname).toBe("/templates/tpl-network");
      return {
        status: 200,
        json: {
          templateID: "tpl-network",
          status: "READY",
          networkType: "tap",
          allowInternetAccess: false,
        },
      };
    };
    const info = await Template.get("tpl-network", { config: makeConfig() });
    expect(info.templateId).toBe("tpl-network");
    expect(info.networkType).toBe("tap");
    expect(info.allowInternetAccess).toBe(false);
  });
});

describe("Template.build", () => {
  it("rejects when no image is provided", async () => {
    await expect(Template.build({ config: makeConfig() })).rejects.toThrow(/image/);
  });

  it("POSTs to /templates with the image and returns a build", async () => {
    handler = (req) => {
      expect(req.method).toBe("POST");
      expect(req.pathname).toBe("/templates");
      const body = JSON.parse(req.body.toString());
      expect(body.image).toBe("python:3.11-slim");
      return {
        status: 200,
        json: {
          jobID: "job-001",
          templateID: "tpl-python",
          status: "running",
          phase: "Pulling",
          progress: 10,
        },
      };
    };
    const build = await Template.build({ image: "python:3.11-slim", config: makeConfig() });
    expect(build.buildId).toBe("job-001");
    expect(build.jobId).toBe("job-001");
    expect(build.templateId).toBe("tpl-python");
    expect(build.status).toBe("running");
  });
});

describe("Template.rebuild", () => {
  it("POSTs an empty body to /templates/:id by default", async () => {
    handler = (req) => {
      expect(req.method).toBe("POST");
      expect(req.pathname).toBe("/templates/tpl-python");
      expect(JSON.parse(req.body.toString())).toEqual({});
      return {
        status: 200,
        json: { jobID: "job-002", templateID: "tpl-python", status: "running" },
      };
    };
    const build = await Template.rebuild("tpl-python", { config: makeConfig() });
    expect(build.jobId).toBe("job-002");
    expect(build.templateId).toBe("tpl-python");
    expect(build.status).toBe("running");
  });

  it("forwards extra fields into the request body", async () => {
    handler = (req) => {
      expect(JSON.parse(req.body.toString())).toEqual({ force: true });
      return { status: 200, json: { jobID: "job-003" } };
    };
    const build = await Template.rebuild("tpl-x", { extra: { force: true }, config: makeConfig() });
    expect(build.jobId).toBe("job-003");
  });

  it("maps a 404 to TemplateNotFoundError", async () => {
    handler = () => ({ status: 404, json: { message: "template not found" } });
    await expect(Template.rebuild("tpl-missing", { config: makeConfig() })).rejects.toBeInstanceOf(
      TemplateNotFoundError,
    );
  });
});

describe("Template.getBuildLogs", () => {
  it("GETs the build logs endpoint and returns the payload", async () => {
    handler = (req) => {
      expect(req.method).toBe("GET");
      expect(req.pathname).toBe("/templates/tpl-python/builds/job-001/logs");
      return { status: 200, json: { logs: ["step 1", "step 2"] } };
    };
    const logs = await Template.getBuildLogs("tpl-python", "job-001", { config: makeConfig() });
    expect(logs.logs).toEqual(["step 1", "step 2"]);
  });
});

describe("Template.getBuildStatus", () => {
  it("GETs the build status endpoint", async () => {
    handler = (req) => {
      expect(req.pathname).toBe("/templates/tpl-python/builds/job-001/status");
      return { status: 200, json: { buildID: "job-001", status: "succeeded" } };
    };
    const build = await Template.getBuildStatus("tpl-python", "job-001", {
      config: makeConfig(),
    });
    expect(build.buildId).toBe("job-001");
    expect(build.status).toBe("succeeded");
  });
});

describe("Template.delete", () => {
  it("DELETEs the template", async () => {
    handler = (req) => {
      expect(req.method).toBe("DELETE");
      expect(req.pathname).toBe("/templates/tpl-old");
      return { status: 204 };
    };
    await Template.delete("tpl-old", { config: makeConfig() });
    expect(requests).toHaveLength(1);
  });
});
