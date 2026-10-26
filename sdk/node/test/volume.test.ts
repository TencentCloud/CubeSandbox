// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { createServer, type IncomingHttpHeaders, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { inspect } from "node:util";

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";

import { Config } from "../src/config.js";
import {
  ApiError,
  AuthenticationError,
  VolumeInUseError,
  VolumeNotFoundError,
} from "../src/exceptions.js";
import {
  MAX_VOLUME_NAME_LEN,
  Sandbox,
  Volume,
  VolumeInfo,
  VolumeMount,
} from "../src/index.js";
import { stubCubeEnv } from "./_env.js";

stubCubeEnv();

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

type Handler = (req: RecordedRequest) => MockResponse | Promise<MockResponse>;

let server: Server;
let port: number;
let requests: RecordedRequest[] = [];
let handler: Handler = () => ({ status: 200, json: {} });

function setHandler(h: Handler): void {
  handler = h;
}

function makeConfig(): Config {
  return new Config({
    apiUrl: `http://127.0.0.1:${port}`,
    templateId: "tpl-test",
    sandboxDomain: "cube.app",
  });
}

beforeAll(async () => {
  server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (chunk) => chunks.push(chunk as Buffer));
    req.on("end", () => {
      const url = new URL(req.url ?? "/", `http://127.0.0.1:${port}`);
      const recorded: RecordedRequest = {
        method: req.method ?? "",
        pathname: url.pathname,
        headers: req.headers,
        body: Buffer.concat(chunks),
      };
      requests.push(recorded);
      Promise.resolve(handler(recorded)).then((mock) => {
        const status = mock.status ?? 200;
        if (mock.json !== undefined) {
          res.writeHead(status, { "Content-Type": "application/json" });
          res.end(JSON.stringify(mock.json));
        } else {
          res.writeHead(status, { "Content-Type": "text/plain" });
          res.end(mock.text ?? "");
        }
      });
    });
  });
  await new Promise<void>((resolve) => {
    server.listen(0, "127.0.0.1", resolve);
  });
  port = (server.address() as AddressInfo).port;
});

afterAll(async () => {
  await new Promise((resolve) => server.close(resolve));
});

afterEach(() => {
  requests = [];
  handler = () => ({ status: 200, json: {} });
});

describe("Volume CRUD", () => {
  it("create sends the payload and parses the response", async () => {
    setHandler(() => ({
      status: 201,
      json: { volumeID: "my-data", name: "my-data", token: "tok-123" },
    }));
    const volume = await Volume.create({
      name: "my-data",
      driver: "cos",
      config: makeConfig(),
    });
    expect(volume.volumeId).toBe("my-data");
    expect(volume.name).toBe("my-data");
    expect(volume.token).toBe("tok-123");

    expect(requests).toHaveLength(1);
    expect(requests[0].method).toBe("POST");
    expect(requests[0].pathname).toBe("/volumes");
    expect(JSON.parse(requests[0].body.toString())).toEqual({
      name: "my-data",
      driver: "cos",
    });
  });

  it("create omits driver and allows an empty name", async () => {
    setHandler(() => ({
      status: 201,
      json: { volumeID: "3e9b6f2a-0000-4000-8000-000000000000", name: "3e9b6f2a-0000-4000-8000-000000000000", token: "" },
    }));
    const volume = await Volume.create({ config: makeConfig() });
    expect(volume.volumeId).not.toBe("");
    expect(volume.token).toBe("");

    // Mirror the Python/Go SDK wire format: name is always sent (empty string
    // requests a server-generated UUID), driver is omitted when unset.
    const payload = JSON.parse(requests[0].body.toString());
    expect(payload).toEqual({ name: "" });
  });

  it("create validates the name without sending a request", async () => {
    for (const name of ["a".repeat(MAX_VOLUME_NAME_LEN + 1), "my volume!", "a/b"]) {
      await expect(Volume.create({ name, config: makeConfig() })).rejects.toThrow(
        /volume name/,
      );
    }
    expect(requests).toHaveLength(0);
  });

  it("list returns VolumeInfo entries without tokens", async () => {
    setHandler(() => ({
      status: 200,
      json: [
        { volumeID: "vol-a", name: "vol-a" },
        { volumeID: "vol-b", name: "vol-b" },
      ],
    }));
    const volumes = await Volume.list({ config: makeConfig() });
    expect(volumes).toHaveLength(2);
    expect(volumes[0]).toBeInstanceOf(VolumeInfo);
    expect(volumes[0].volumeId).toBe("vol-a");
    expect(volumes[1].volumeId).toBe("vol-b");
    expect(volumes[0].token).toBe("");
    expect(requests[0].method).toBe("GET");
    expect(requests[0].pathname).toBe("/volumes");
  });

  it("getInfo returns the token and connect wraps it in a live Volume", async () => {
    setHandler(() => ({
      status: 200,
      json: { volumeID: "my-data", name: "my-data", token: "tok-456" },
    }));
    const info = await Volume.getInfo("my-data", { config: makeConfig() });
    expect(info.token).toBe("tok-456");
    expect(requests[0].pathname).toBe("/volumes/my-data");

    const volume = await Volume.connect("my-data", { config: makeConfig() });
    expect(volume).toBeInstanceOf(Volume);
    expect(volume.volumeId).toBe("my-data");
    expect(volume.token).toBe("tok-456");
  });

  it("destroy resolves true on 204 and false on 404", async () => {
    setHandler(() => ({ status: 204, text: "" }));
    await expect(Volume.destroy("my-data", { config: makeConfig() })).resolves.toBe(true);
    expect(requests[0].method).toBe("DELETE");
    expect(requests[0].pathname).toBe("/volumes/my-data");

    setHandler(() => ({ status: 404, json: { message: "volume not found: my-data" } }));
    await expect(Volume.destroy("my-data", { config: makeConfig() })).resolves.toBe(false);
  });

  it("rejects unsafe volume IDs without sending a request", async () => {
    for (const volumeId of ["", "../other", "a/b", "a b", "%2e%2e"]) {
      await expect(Volume.getInfo(volumeId, { config: makeConfig() })).rejects.toThrow(
        /volumeId/,
      );
      await expect(Volume.destroy(volumeId, { config: makeConfig() })).rejects.toThrow(
        /volumeId/,
      );
    }
    expect(requests).toHaveLength(0);
  });
});

describe("Volume error mapping", () => {
  it("maps 404 to VolumeNotFoundError", async () => {
    setHandler(() => ({ status: 404, json: { message: "volume not found: my-data" } }));
    await expect(Volume.getInfo("my-data", { config: makeConfig() })).rejects.toBeInstanceOf(
      VolumeNotFoundError,
    );
  });

  it("maps the in-use 409 to VolumeInUseError", async () => {
    setHandler(() => ({
      status: 409,
      json: {
        message:
          "conflict: volume my-data is in use by 2 node(s); destroy the sandboxes using it before deleting",
      },
    }));
    const err = await Volume.destroy("my-data", { config: makeConfig() }).catch((e) => e);
    expect(err).toBeInstanceOf(VolumeInUseError);
    expect(err.statusCode).toBe(409);
  });

  it("keeps the duplicate-name 409 a generic ApiError", async () => {
    setHandler(() => ({ status: 409, json: { message: "volume already exists: my-data" } }));
    const err = await Volume.create({ name: "my-data", config: makeConfig() }).catch(
      (e) => e,
    );
    expect(err).toBeInstanceOf(ApiError);
    expect(err).not.toBeInstanceOf(VolumeInUseError);
  });

  it("maps 401 to AuthenticationError", async () => {
    setHandler(() => ({ status: 401, json: { message: "bad key" } }));
    await expect(Volume.list({ config: makeConfig() })).rejects.toBeInstanceOf(
      AuthenticationError,
    );
  });
});

describe("token masking", () => {
  it("toString and util.inspect mask the token", () => {
    const info = new VolumeInfo("my-data", "my-data", "tok-secret-123");
    for (const formatted of [info.toString(), `${info}`, inspect(info)]) {
      expect(formatted).not.toContain("tok-secret-123");
      expect(formatted).toContain("***");
      expect(formatted).toContain("my-data");
    }
  });

  it("renders an absent token as empty, not as the mask", () => {
    const info = new VolumeInfo("no-token", "no-token");
    expect(inspect(info)).not.toContain("***");
  });
});

describe("Sandbox.create volumeMounts", () => {
  const SANDBOX_JSON = {
    sandboxID: "sb-test-001",
    templateID: "tpl-test",
    domain: "cube.app",
    state: "running",
  };

  it("serializes the e2b mapping into the shared wire format", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_JSON }));
    const volume = new VolumeInfo("my-data", "my-data");
    await Sandbox.create({
      config: makeConfig(),
      volumeMounts: {
        "/workspace": volume,
        "/cache": "shared-cache",
        "/dataset": new VolumeMount(volume, { readOnly: true }),
      },
    });
    const payload = JSON.parse(requests[0].body.toString());
    expect(payload.volumeMounts).toEqual([
      { name: "my-data", path: "/workspace" },
      { name: "shared-cache", path: "/cache" },
      { name: "my-data", path: "/dataset", readOnly: true },
    ]);
  });

  it("omits volumeMounts when not provided", async () => {
    setHandler(() => ({ status: 201, json: SANDBOX_JSON }));
    await Sandbox.create({ config: makeConfig() });
    const payload = JSON.parse(requests[0].body.toString());
    expect(payload).not.toHaveProperty("volumeMounts");
  });

  it("validates mount paths and volume names without sending a request", async () => {
    const cases: Array<Record<string, string>> = [
      { "": "my-data" },
      { relative: "my-data" },
      { "/workspace/../etc": "my-data" },
      { "/./workspace": "my-data" },
      { "/workspace": "" },
      { "/workspace": "../other" },
    ];
    for (const volumeMounts of cases) {
      await expect(
        Sandbox.create({ config: makeConfig(), volumeMounts }),
      ).rejects.toThrow(/volume/);
    }
    expect(requests).toHaveLength(0);
  });
});
