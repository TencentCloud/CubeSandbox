---
title: "Node.js Fastify Web Template for Stateful Sandbox Apps"
author: Alpaca233114514
date: 2026-07-03
tags:
  - nodejs
  - fastify
  - web-template
  - snapshot
lang: en-US
---

# Node.js Fastify Web Template for Stateful Sandbox Apps

## Business Context

Many agent and developer-tool workflows need more than a one-shot code interpreter. They need a sandbox that can run a long-lived web service, expose an HTTP port, keep workspace state, and still support CubeSandbox lifecycle controls such as pause and resume.

The `examples/node-fastify-web-template` example shows how to package that pattern as a reusable Node.js template. It runs a TypeScript Fastify API on port `3000` while keeping CubeSandbox `envd` available on port `49983` for SDK access.

## Key Challenges

- **Dual-process runtime**: the template must keep `envd` running for SDK operations while also starting the user web server.
- **Port exposure**: both the SDK control port and application port need to be exposed when the template is registered.
- **State persistence**: application data should live in the sandbox workspace so snapshot and resume flows can preserve it.
- **Operational reproducibility**: the template should be buildable from Docker and testable before it is registered in CubeSandbox.
- **Failure handling**: malformed JSON, invalid request bodies, and corrupted state files need deterministic behavior instead of only testing the happy path.

## Solution with Cube Sandbox

The example builds a Docker image from `node:24-bookworm-slim` and copies `envd` from `ghcr.io/tencentcloud/cubesandbox-base:2026.16`. The container entrypoint starts `envd` on `49983` and then runs the compiled Fastify service:

```bash
docker build -t cube-node-fastify-web:latest .
docker tag cube-node-fastify-web:latest <your-registry>/cube-node-fastify-web:latest
docker push <your-registry>/cube-node-fastify-web:latest

cubemastercli -a 127.0.0.1 -p 8089 tpl create-from-image \
  --image <your-registry>/cube-node-fastify-web:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port 3000 \
  --probe 49983 \
  --probe-path /health
```

Inside the app, runtime state is written under `/workspace/state`:

- `POST /api/counter` increments a persisted counter file.
- `POST /api/write-note` appends JSONL notes.
- `GET /api/info` reports Node.js runtime and workspace paths.

The Python demos use the E2B-compatible SDK endpoint exposed by CubeSandbox. `run_demo.py` creates a sandbox, resolves the public web host for port `3000`, calls the Fastify API, and writes state. `snapshot_resume.py` pauses and reconnects to the same sandbox, then verifies the counter continues from the previous value.

## Results and Benefits

- **Reusable web template pattern**: teams can start from a minimal Node.js + Fastify example instead of wiring `envd`, ports, and SDK access from scratch.
- **Stateful web app validation**: the counter and note endpoints make workspace persistence visible and easy to test.
- **Snapshot/resume coverage**: the demo verifies that web-service state survives pause and resume flows.
- **Non-happy-path tests**: local tests cover corrupted state, invalid bodies, malformed JSON, and a real listener on an ephemeral port.
- **Verified on a real CubeSandbox environment**: the template was registered as `tpl-03f52e94be8c48ca8ef68dee` and validated end to end in WSL with Kimi K2.7-assisted semi-automated real-machine testing.

## References

- Example source: `examples/node-fastify-web-template`
- Example index: [CubeSandbox examples](../tutorials/examples.md)
- Compatible protocol: [E2B Sandbox](https://e2b.dev)
