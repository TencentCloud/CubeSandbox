// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

const http = require("http");

const host = process.env.HOST || "0.0.0.0";
const port = Number.parseInt(process.env.PORT || "3000", 10);

const startedAt = new Date().toISOString();

function sendJson(res, statusCode, payload) {
  const body = `${JSON.stringify(payload, null, 2)}\n`;
  res.writeHead(statusCode, {
    "content-type": "application/json; charset=utf-8",
    "cache-control": "no-store",
  });
  res.end(body);
}

function sendText(res, statusCode, body) {
  res.writeHead(statusCode, {
    "content-type": "text/plain; charset=utf-8",
    "cache-control": "no-store",
  });
  res.end(body);
}

const server = http.createServer((req, res) => {
  if (req.method !== "GET") {
    sendJson(res, 405, {
      ok: false,
      error: "method_not_allowed",
      allowed: ["GET"],
    });
    return;
  }

  const url = new URL(req.url, `http://${req.headers.host || "localhost"}`);

  if (url.pathname === "/health") {
    sendText(res, 200, "ok\n");
    return;
  }

  if (url.pathname === "/api/hello") {
    sendJson(res, 200, {
      ok: true,
      message: "hello from CubeSandbox Node.js",
      runtime: process.version,
      started_at: startedAt,
    });
    return;
  }

  if (url.pathname === "/") {
    sendText(
      res,
      200,
      [
        "CubeSandbox Node.js web sandbox",
        "GET /health -> ok",
        "GET /api/hello -> JSON payload",
        "",
      ].join("\n"),
    );
    return;
  }

  sendJson(res, 404, {
    ok: false,
    error: "not_found",
    path: url.pathname,
  });
});

server.listen(port, host, () => {
  console.log(`node-web-sandbox listening on ${host}:${port}`);
});

process.on("SIGTERM", () => {
  server.close(() => process.exit(0));
});
