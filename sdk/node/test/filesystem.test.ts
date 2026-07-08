// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { createServer, type IncomingHttpHeaders, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { Config } from "../src/config.js";
import { FilesystemNotFoundError, PartialWriteError } from "../src/exceptions.js";
import { Sandbox } from "../src/index.js";

const SANDBOX_ID = "sb-test-001";
const DOMAIN = "cube.app";
const SANDBOX_DATA = {
  sandboxID: SANDBOX_ID,
  templateID: "tpl-test",
  domain: DOMAIN,
  state: "running",
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
  headers?: Record<string, string>;
}

type Handler = (req: RecordedRequest) => MockResponse;

let server: Server;
let port: number;
let requests: RecordedRequest[] = [];
let handler: Handler = () => ({ status: 201, json: SANDBOX_DATA });

function makeConfig(): Config {
  return new Config({
    apiUrl: `http://127.0.0.1:${port}`,
    templateId: "tpl-test",
    proxyNodeIp: "127.0.0.1",
    proxyPort: port,
    sandboxDomain: DOMAIN,
  });
}

async function createSandbox(): Promise<Sandbox> {
  handler = () => ({ status: 201, json: SANDBOX_DATA });
  const sb = await Sandbox.create({ config: makeConfig() });
  requests = [];
  return sb;
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
        url,
        headers: req.headers,
        body: Buffer.concat(chunks),
      };
      requests.push(record);
      const out = handler(record);
      const headers: Record<string, string> = { ...(out.headers ?? {}) };
      let payload = "";
      if (out.text !== undefined) {
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

describe("Filesystem.read", () => {
  it("GETs /files with path + username and returns the raw body", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.method).toBe("GET");
      expect(req.pathname).toBe("/files");
      expect(req.headers.host).toBe(`49983-${SANDBOX_ID}.${DOMAIN}`);
      expect(req.url.searchParams.get("path")).toBe("/tmp/foo.txt");
      expect(req.url.searchParams.get("username")).toBe("root");
      return { status: 200, text: "file content" };
    };
    expect(await sb.files.read("/tmp/foo.txt")).toBe("file content");
    sb.close();
  });
});

describe("Filesystem.write", () => {
  it("writes as application/octet-stream on the first attempt", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.method).toBe("POST");
      expect(req.pathname).toBe("/files");
      expect(req.headers["content-type"]).toBe("application/octet-stream");
      expect(req.body.toString()).toBe("file content");
      return { status: 200, json: [{ path: "/tmp/foo.txt" }] };
    };
    await sb.files.write("/tmp/foo.txt", "file content");
    expect(requests).toHaveLength(1);
    sb.close();
  });

  it("falls back to multipart form-data when octet-stream is rejected", async () => {
    const sb = await createSandbox();
    let calls = 0;
    handler = (req) => {
      calls += 1;
      if (calls === 1) {
        return { status: 415, text: "unsupported media type" };
      }
      expect(req.headers["content-type"]).toMatch(/multipart\/form-data/);
      expect(req.body.toString()).toContain("file content");
      return { status: 200, json: [{ path: "/tmp/foo.txt" }] };
    };
    await sb.files.write("/tmp/foo.txt", "file content");
    expect(calls).toBe(2);
    sb.close();
  });
});

describe("Filesystem.writeFiles", () => {
  it("returns the number of files written", async () => {
    const sb = await createSandbox();
    const paths: Array<string | null> = [];
    handler = (req) => {
      paths.push(req.url.searchParams.get("path"));
      return { status: 200, json: [] };
    };
    const n = await sb.files.writeFiles([
      { path: "/tmp/a.txt", data: "aaa" },
      { path: "/tmp/b.txt", data: "bbb" },
      { path: "/tmp/c.txt", data: "ccc" },
    ]);
    expect(n).toBe(3);
    expect(paths).toEqual(["/tmp/a.txt", "/tmp/b.txt", "/tmp/c.txt"]);
    sb.close();
  });

  it("throws PartialWriteError recording the successful count", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      if (req.url.searchParams.get("path") === "/tmp/b.txt") {
        return { status: 500, json: { message: "disk full" } };
      }
      return { status: 200, json: [] };
    };
    let thrown: unknown;
    try {
      await sb.files.writeFiles([
        { path: "/tmp/a.txt", data: "ok" },
        { path: "/tmp/b.txt", data: "fail" },
        { path: "/tmp/c.txt", data: "skip" },
      ]);
    } catch (err) {
      thrown = err;
    }
    expect(thrown).toBeInstanceOf(PartialWriteError);
    expect((thrown as PartialWriteError).written).toBe(1);
    sb.close();
  });
});

describe("Filesystem RPCs", () => {
  it("lists a directory via ListDir", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.method).toBe("POST");
      expect(req.pathname).toBe("/filesystem.Filesystem/ListDir");
      expect(JSON.parse(req.body.toString())).toEqual({ path: "/tmp" });
      return {
        status: 200,
        json: {
          entries: [
            { name: "a.txt", type: "FILE_TYPE_FILE", path: "/tmp/a.txt" },
            { name: "sub", type: "FILE_TYPE_DIRECTORY", path: "/tmp/sub" },
          ],
        },
      };
    };
    const entries = await sb.files.list("/tmp");
    expect(entries).toHaveLength(2);
    expect(entries[0].name).toBe("a.txt");
    expect(entries[1].type).toBe("FILE_TYPE_DIRECTORY");
    sb.close();
  });

  it("stats a path and returns the entry", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.pathname).toBe("/filesystem.Filesystem/Stat");
      expect(JSON.parse(req.body.toString())).toEqual({ path: "/tmp/hello.txt" });
      return {
        status: 200,
        json: { entry: { name: "hello.txt", type: "FILE_TYPE_FILE", size: "30" } },
      };
    };
    const entry = await sb.files.stat("/tmp/hello.txt");
    expect(entry.name).toBe("hello.txt");
    expect(entry.size).toBe("30");
    sb.close();
  });

  it("maps a 404 stat to FilesystemNotFoundError", async () => {
    const sb = await createSandbox();
    handler = () => ({ status: 404, json: { code: "not_found", message: "file not found" } });
    await expect(sb.files.stat("/tmp/missing.txt")).rejects.toBeInstanceOf(
      FilesystemNotFoundError,
    );
    sb.close();
  });

  it("exists returns true when the path is present", async () => {
    const sb = await createSandbox();
    handler = () => ({ status: 200, json: { entry: { name: "f.txt" } } });
    expect(await sb.files.exists("/tmp/f.txt")).toBe(true);
    sb.close();
  });

  it("exists returns false on a 404", async () => {
    const sb = await createSandbox();
    handler = () => ({ status: 404, json: { code: "not_found", message: "missing" } });
    expect(await sb.files.exists("/tmp/missing.txt")).toBe(false);
    sb.close();
  });

  it("makes a directory via MakeDir", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.pathname).toBe("/filesystem.Filesystem/MakeDir");
      expect(JSON.parse(req.body.toString())).toEqual({ path: "/tmp/newdir" });
      return { status: 200, json: { entry: { name: "newdir", type: "FILE_TYPE_DIRECTORY" } } };
    };
    const entry = await sb.files.makeDir("/tmp/newdir");
    expect(entry.type).toBe("FILE_TYPE_DIRECTORY");
    sb.close();
  });

  it("renames via Move with source/destination", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.pathname).toBe("/filesystem.Filesystem/Move");
      expect(JSON.parse(req.body.toString())).toEqual({
        source: "/tmp/old.txt",
        destination: "/tmp/new.txt",
      });
      return { status: 200, json: { entry: { name: "new.txt" } } };
    };
    const entry = await sb.files.rename("/tmp/old.txt", "/tmp/new.txt");
    expect(entry.name).toBe("new.txt");
    sb.close();
  });

  it("removes a path via Remove", async () => {
    const sb = await createSandbox();
    handler = (req) => {
      expect(req.pathname).toBe("/filesystem.Filesystem/Remove");
      expect(JSON.parse(req.body.toString())).toEqual({ path: "/tmp/old.txt" });
      return { status: 200, json: {} };
    };
    await sb.files.remove("/tmp/old.txt");
    expect(requests).toHaveLength(1);
    sb.close();
  });
});
