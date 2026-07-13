// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { createServer, type IncomingHttpHeaders, type Server } from "node:http";
import type { AddressInfo } from "node:net";

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { Config } from "../src/config.js";
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

async function createSandbox(): Promise<Sandbox> {
  setHandler(() => ({ status: 201, json: SANDBOX_DATA }));
  const sb = await Sandbox.create({ config: makeConfig() });
  requests = [];
  return sb;
}

describe("Sandbox.createSnapshot", () => {
  it("POSTs a name and parses the SnapshotInfo response", async () => {
    const sb = await createSandbox();
    setHandler((req) => {
      expect(req.method).toBe("POST");
      expect(req.pathname).toBe(`/sandboxes/${SANDBOX_ID}/snapshots`);
      expect(JSON.parse(req.body.toString())).toEqual({ name: "snap-1" });
      return { status: 200, json: { snapshotID: "snap-abc", names: ["snap-1"] } };
    });
    const snap = await sb.createSnapshot("snap-1");
    expect(snap.snapshotId).toBe("snap-abc");
    expect(snap.names).toEqual(["snap-1"]);
    sb.close();
  });

  it("sends an empty body when no name is given", async () => {
    const sb = await createSandbox();
    setHandler((req) => {
      expect(JSON.parse(req.body.toString())).toEqual({});
      return { status: 200, json: { snapshotID: "snap-noname" } };
    });
    const snap = await sb.createSnapshot();
    expect(snap.snapshotId).toBe("snap-noname");
    expect(snap.names).toEqual([]);
    sb.close();
  });
});

describe("Sandbox.listSnapshots", () => {
  it("forwards query params and reads the next-token header", async () => {
    setHandler((req) => {
      expect(req.pathname).toBe("/snapshots");
      expect(req.url.searchParams.get("sandboxID")).toBe(SANDBOX_ID);
      expect(req.url.searchParams.get("limit")).toBe("10");
      expect(req.url.searchParams.get("nextToken")).toBe("tok-in");
      return {
        status: 200,
        json: [{ snapshotID: "s1" }, { snapshotID: "s2", names: ["b"] }],
        headers: { "x-next-token": "tok-out" },
      };
    });
    const { snapshots, nextToken } = await Sandbox.listSnapshots({
      sandboxId: SANDBOX_ID,
      limit: 10,
      nextToken: "tok-in",
      config: makeConfig(),
    });
    expect(snapshots.map((s) => s.snapshotId)).toEqual(["s1", "s2"]);
    expect(snapshots[1].names).toEqual(["b"]);
    expect(nextToken).toBe("tok-out");
  });

  it("returns a null next token when the header is absent", async () => {
    setHandler(() => ({ status: 200, json: [] }));
    const { snapshots, nextToken } = await Sandbox.listSnapshots({ config: makeConfig() });
    expect(snapshots).toEqual([]);
    expect(nextToken).toBeNull();
  });
});

describe("Sandbox.deleteSnapshot", () => {
  it("DELETEs the snapshot's template id", async () => {
    setHandler((req) => {
      expect(req.method).toBe("DELETE");
      expect(req.pathname).toBe("/templates/snap-abc");
      return { status: 204 };
    });
    await Sandbox.deleteSnapshot("snap-abc", { config: makeConfig() });
    expect(requests).toHaveLength(1);
  });
});

describe("Sandbox.rollback", () => {
  it("POSTs the snapshot id and returns the result body", async () => {
    const sb = await createSandbox();
    setHandler((req) => {
      expect(req.method).toBe("POST");
      expect(req.pathname).toBe(`/sandboxes/${SANDBOX_ID}/rollback`);
      expect(JSON.parse(req.body.toString())).toEqual({ snapshotID: "snap-abc" });
      return { status: 200, json: { ok: true } };
    });
    const result = await sb.rollback("snap-abc");
    expect(result.ok).toBe(true);
    sb.close();
  });
});

describe("Sandbox.clone", () => {
  it("snapshots, creates n siblings, then deletes the snapshot", async () => {
    const sb = await createSandbox();
    const createdIds: string[] = [];
    let snapshotDeleted = false;
    setHandler((req) => {
      if (req.method === "POST" && req.pathname.endsWith("/snapshots")) {
        return { status: 200, json: { snapshotID: "snap-clone" } };
      }
      if (req.method === "POST" && req.pathname === "/sandboxes") {
        expect(JSON.parse(req.body.toString()).templateID).toBe("snap-clone");
        const id = `sb-clone-${createdIds.length}`;
        createdIds.push(id);
        return { status: 201, json: { ...SANDBOX_DATA, sandboxID: id } };
      }
      if (req.method === "DELETE" && req.pathname === "/templates/snap-clone") {
        snapshotDeleted = true;
        return { status: 204 };
      }
      return { status: 500, json: { message: "unexpected" } };
    });

    const clones = await sb.clone(2);
    expect(clones.map((c) => c.sandboxId)).toEqual(["sb-clone-0", "sb-clone-1"]);
    expect(snapshotDeleted).toBe(true);
    clones.forEach((c) => c.close());
    sb.close();
  });

  it("caps in-flight creates at `concurrency`", async () => {
    const sb = await createSandbox();
    let inFlight = 0;
    let maxInFlight = 0;
    let created = 0;
    setHandler(async (req) => {
      if (req.method === "POST" && req.pathname.endsWith("/snapshots")) {
        return { status: 200, json: { snapshotID: "snap-clone" } };
      }
      if (req.method === "POST" && req.pathname === "/sandboxes") {
        inFlight += 1;
        maxInFlight = Math.max(maxInFlight, inFlight);
        await new Promise((r) => setTimeout(r, 50));
        inFlight -= 1;
        return { status: 201, json: { ...SANDBOX_DATA, sandboxID: `sb-clone-${created++}` } };
      }
      if (req.method === "DELETE" && req.pathname === "/templates/snap-clone") {
        return { status: 204 };
      }
      return { status: 500, json: { message: "unexpected" } };
    });

    const clones = await sb.clone(6, { concurrency: 2 });
    expect(clones).toHaveLength(6);
    // Never more than the requested concurrency in flight, but actually parallel.
    expect(maxInFlight).toBeLessThanOrEqual(2);
    expect(maxInFlight).toBeGreaterThan(1);
    clones.forEach((c) => c.close());
    sb.close();
  });

  it("kills successful siblings and rethrows when a create fails", async () => {
    const sb = await createSandbox();
    let createCount = 0;
    const killed: string[] = [];
    let snapshotDeleted = false;
    setHandler((req) => {
      if (req.method === "POST" && req.pathname.endsWith("/snapshots")) {
        return { status: 200, json: { snapshotID: "snap-clone" } };
      }
      if (req.method === "POST" && req.pathname === "/sandboxes") {
        createCount += 1;
        if (createCount === 1) {
          return { status: 201, json: { ...SANDBOX_DATA, sandboxID: "sb-clone-0" } };
        }
        return { status: 500, json: { message: "boom" } };
      }
      if (req.method === "DELETE" && req.pathname.startsWith("/sandboxes/")) {
        killed.push(req.pathname);
        return { status: 204 };
      }
      if (req.method === "DELETE" && req.pathname === "/templates/snap-clone") {
        snapshotDeleted = true;
        return { status: 204 };
      }
      return { status: 500, json: { message: "unexpected" } };
    });

    await expect(sb.clone(2)).rejects.toThrow(/boom/);
    expect(killed).toContain("/sandboxes/sb-clone-0");
    expect(snapshotDeleted).toBe(true);
    sb.close();
  });
});
