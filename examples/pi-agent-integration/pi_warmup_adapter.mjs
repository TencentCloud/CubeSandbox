#!/usr/bin/env node
// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

import http from "node:http";

import {
  AuthStorage,
  createAgentSession,
  ModelRegistry,
  SessionManager,
} from "./dist/index.js";

const host = process.env.PI_WARMUP_HOST || "0.0.0.0";
const port = parsePositiveInteger(process.env.PI_WARMUP_PORT, 8080);
const maxBodyBytes = parsePositiveInteger(
  process.env.PI_WARMUP_MAX_BODY_BYTES,
  1024 * 1024,
);
const cwd = process.env.PI_WORKSPACE || "/workspace";
const agentDir = process.env.PI_CODING_AGENT_DIR || "/root/.pi/agent";
const sessionDir =
  process.env.PI_CODING_AGENT_SESSION_DIR || `${agentDir}/sessions`;
const defaultProvider = (process.env.PI_PROVIDER || "deepseek").toLowerCase();
const defaultModel = process.env.PI_MODEL || "deepseek-v4-pro";

let ready = false;
let busy = false;
let session;
let authStorage;
let modelRegistry;

function parsePositiveInteger(value, fallback) {
  const parsed = Number.parseInt(value || "", 10);
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : fallback;
}

function sendJson(response, status, payload) {
  const body = JSON.stringify(payload);
  response.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "content-length": Buffer.byteLength(body),
  });
  response.end(body);
}

async function readJson(request) {
  const chunks = [];
  let size = 0;
  for await (const chunk of request) {
    size += chunk.length;
    if (size > maxBodyBytes) {
      const error = new Error(`request body exceeds ${maxBodyBytes} bytes`);
      error.statusCode = 413;
      throw error;
    }
    chunks.push(chunk);
  }
  if (size === 0) return {};
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    const error = new Error("request body must be valid JSON");
    error.statusCode = 400;
    throw error;
  }
}

async function handlePrompt(request, response) {
  if (!ready) return sendJson(response, 503, { error: "session is not ready" });
  if (busy) return sendJson(response, 409, { error: "session is busy" });

  busy = true;
  let unsubscribe;
  try {
    const body = await readJson(request);
    if (typeof body.prompt !== "string" || body.prompt.trim() === "") {
      return sendJson(response, 400, {
        error: "prompt must be a non-empty string",
      });
    }

    const provider = String(body.provider || defaultProvider).toLowerCase();
    const modelId = String(body.model || defaultModel);
    if (typeof body.baseUrl === "string" && body.baseUrl !== "") {
      modelRegistry.registerProvider(provider, { baseUrl: body.baseUrl });
    }
    const model = modelRegistry.find(provider, modelId);
    if (!model) {
      return sendJson(response, 400, {
        error: `unknown Pi model: ${provider}/${modelId}`,
      });
    }
    if (typeof body.apiKey === "string" && body.apiKey !== "") {
      // Runtime-only: never persist request credentials into the snapshot/session.
      authStorage.setRuntimeApiKey(provider, body.apiKey);
    }
    await session.setModel(model);

    let messages = [];
    unsubscribe = session.subscribe((event) => {
      if (event.type === "agent_end") messages = event.messages;
    });
    await session.prompt(body.prompt);
    return sendJson(response, 200, {
      sessionId: session.sessionId,
      model: `${provider}/${modelId}`,
      messages,
    });
  } catch (error) {
    const status = Number.isInteger(error?.statusCode) ? error.statusCode : 500;
    return sendJson(response, status, {
      error: error instanceof Error ? error.message : String(error),
    });
  } finally {
    unsubscribe?.();
    busy = false;
  }
}

const server = http.createServer(async (request, response) => {
  if (request.method === "GET" && request.url === "/readyz") {
    return sendJson(response, ready ? 200 : 503, { ready, busy });
  }
  if (request.method === "POST" && request.url === "/prompt") {
    return handlePrompt(request, response);
  }
  return sendJson(response, 404, { error: "not found" });
});

async function shutdown(signal) {
  ready = false;
  server.close(() => {
    session?.dispose();
    process.exit(0);
  });
  setTimeout(() => process.exit(1), 5000).unref();
  console.log(`Received ${signal}; shutting down`);
}

process.on("SIGTERM", () => void shutdown("SIGTERM"));
process.on("SIGINT", () => void shutdown("SIGINT"));

try {
  authStorage = AuthStorage.inMemory();
  modelRegistry = ModelRegistry.create(authStorage);
  const model = modelRegistry.find(defaultProvider, defaultModel);
  if (!model) {
    throw new Error(`unknown Pi model: ${defaultProvider}/${defaultModel}`);
  }

  ({ session } = await createAgentSession({
    cwd,
    agentDir,
    authStorage,
    modelRegistry,
    model,
    sessionManager: SessionManager.create(cwd, sessionDir),
  }));
  ready = true;
  server.listen(port, host, () => {
    console.log(
      `Pi AgentSession ${session.sessionId} ready on http://${host}:${port}`,
    );
  });
} catch (error) {
  console.error("Failed to initialize Pi AgentSession:", error);
  process.exit(1);
}
