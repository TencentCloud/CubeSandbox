// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import { execFileSync } from "node:child_process";
import { createServer as createHttpServer, type Server } from "node:http";
import { createServer as createHttpsServer, type Server as HttpsServer } from "node:https";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { TLSSocket } from "node:tls";

import { fetch } from "undici";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { Config } from "../src/config.js";
import { buildDataDispatcher, controlFetch } from "../src/transport.js";
import { stubCubeEnv } from "./_env.js";

stubCubeEnv();

describe("controlFetch auth", () => {
  let server: Server;
  let port: number;
  let seenAuth: string | undefined;

  beforeAll(async () => {
    server = createHttpServer((req, res) => {
      seenAuth = req.headers.authorization;
      res.writeHead(200);
      res.end("ok");
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    port = (server.address() as AddressInfo).port;
  });

  afterAll(() => server.close());

  it("injects Authorization: Bearer when an API key is set", async () => {
    const cfg = new Config({ apiKey: "secret-key" });
    await controlFetch(cfg, `http://127.0.0.1:${port}/health`);
    expect(seenAuth).toBe("Bearer secret-key");
  });

  it("sends no Authorization header when no API key is set", async () => {
    seenAuth = undefined;
    const cfg = new Config({ apiKey: null });
    await controlFetch(cfg, `http://127.0.0.1:${port}/health`);
    expect(seenAuth).toBeUndefined();
  });

  it("does not overwrite a caller-provided Authorization header", async () => {
    const cfg = new Config({ apiKey: "secret-key" });
    await controlFetch(cfg, `http://127.0.0.1:${port}/health`, {
      headers: { Authorization: "Bearer caller" },
    });
    expect(seenAuth).toBe("Bearer caller");
  });
});

describe("buildDataDispatcher", () => {
  it("returns undefined when no proxy node IP is configured", () => {
    const cfg = new Config({ proxyNodeIp: null });
    expect(buildDataDispatcher(cfg)).toBeUndefined();
  });

  it("routes plain http connections to the proxy node while keeping the Host header", async () => {
    let seenHost: string | undefined;
    let seenPath: string | undefined;
    const server: Server = createHttpServer((req, res) => {
      seenHost = req.headers.host;
      seenPath = req.url;
      res.writeHead(200);
      res.end("ok");
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const proxyPort = (server.address() as AddressInfo).port;

    try {
      const cfg = new Config({
        proxyNodeIp: "127.0.0.1",
        proxyPort,
        proxyScheme: "http",
        sandboxDomain: "cube.app",
      });
      const dispatcher = buildDataDispatcher(cfg);
      const virtualHost = "49983-sb-http.cube.app";
      const resp = await fetch(`http://${virtualHost}/files`, { dispatcher });
      expect(resp.status).toBe(200);
      // Dialed the proxy IP, but the virtual host is preserved for routing.
      expect(seenHost).toBe(virtualHost);
      expect(seenPath).toBe("/files");
      await dispatcher?.close();
    } finally {
      server.close();
    }
  });
});

describe("buildDataDispatcher (https SNI preservation)", () => {
  let tmp: string;
  let server: HttpsServer;
  let proxyPort: number;
  let capturedServername: string | undefined;
  const prevReject = process.env.NODE_TLS_REJECT_UNAUTHORIZED;

  beforeAll(async () => {
    tmp = mkdtempSync(join(tmpdir(), "cube-tls-"));
    const keyPath = join(tmp, "key.pem");
    const certPath = join(tmp, "cert.pem");
    // Self-signed cert; SAN is set to the virtual host but not actually
    // verified here (see NODE_TLS_REJECT_UNAUTHORIZED below) — the point of
    // this test is purely that the SNI servername reaches the server.
    execFileSync(
      "openssl",
      [
        "req", "-x509", "-newkey", "rsa:2048", "-nodes",
        "-keyout", keyPath, "-out", certPath, "-days", "1",
        "-subj", "/CN=cube-test",
        "-addext", "subjectAltName=DNS:49999-sb-tls.cube.app",
      ],
      { stdio: "ignore" },
    );

    // Self-signed cert would otherwise fail verification; we only assert on
    // the presented SNI, so relax verification for this test process.
    process.env.NODE_TLS_REJECT_UNAUTHORIZED = "0";

    server = createHttpsServer(
      { key: readFileSync(keyPath), cert: readFileSync(certPath) },
      (_req, res) => {
        res.writeHead(200);
        res.end("ok");
      },
    );
    server.on("secureConnection", (socket: TLSSocket) => {
      capturedServername = socket.servername || undefined;
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    proxyPort = (server.address() as AddressInfo).port;
  });

  afterAll(() => {
    server.close();
    if (prevReject === undefined) {
      delete process.env.NODE_TLS_REJECT_UNAUTHORIZED;
    } else {
      process.env.NODE_TLS_REJECT_UNAUTHORIZED = prevReject;
    }
    rmSync(tmp, { recursive: true, force: true });
  });

  it("presents the virtual sandbox host as the TLS SNI, not the proxy IP", async () => {
    const cfg = new Config({
      proxyNodeIp: "127.0.0.1",
      proxyPort,
      proxyScheme: "https",
      sandboxDomain: "cube.app",
    });
    const dispatcher = buildDataDispatcher(cfg);
    const virtualHost = "49999-sb-tls.cube.app";
    const resp = await fetch(`https://${virtualHost}/execute`, { dispatcher });
    expect(resp.status).toBe(200);
    // SNI must be the virtual host so CubeProxy TLS routing / cert checks work,
    // even though the TCP socket landed on 127.0.0.1 (the proxy IP).
    expect(capturedServername).toBe(virtualHost);
    await dispatcher?.close();
  });
});
