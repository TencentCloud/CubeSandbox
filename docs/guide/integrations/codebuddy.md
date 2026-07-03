---
title: CodeBuddy CLI Integration Guide
author: Mizup79
date: 2026-07-03
tags:
  - integration
  - codebuddy
  - cli
lang: en-US
---

# CodeBuddy CLI Integration Guide

## Integration Target and Version

**CodeBuddy CLI** is Tencent Cloud's AI coding assistant for the command line. It helps developers understand, refactor, and generate code through natural language conversations and tool-calling capabilities.

- **npm package**: `@tencent-ai/codebuddy-code`
- **Commands**: `codebuddy` / `cbc`
- **Version tested**: v2.110.0 (July 2026)
- **Runtime**: Node.js v18+ (v22 LTS recommended)
- **Architecture**: Monolithic Node.js process (not client/server)
- **Operation modes**:
  - **Native sandbox** (`codebuddy --sandbox <url>`) — CodeBuddy runs on your host; tool calls routed to a CubeSandbox MicroVM. Recommended for personal use, no enterprise account needed.
  - **In-sandbox** (`codebuddy -p`) — run a single prompt inside the MicroVM and exit, useful for CI/CD pipelines
  - **HTTP API** (`codebuddy --serve`) — start a REST server for interactive consumption
  - **SDK** (`@tencent-ai/agent-sdk`) — programmatic control from Node.js
- **Authentication**: `CODEBUDDY_API_KEY` environment variable or interactive browser OAuth
- **Official docs**: [https://www.codebuddy.ai/docs/cli/overview](https://www.codebuddy.ai/docs/cli/overview)

The following diagram shows how CodeBuddy runs inside a CubeSandbox MicroVM:

```
User / CI Pipeline
    │
    ▼
e2b-code-interpreter SDK (Python)
    │  REST API
    ▼
CubeAPI (port 3000)
    │
    ▼
CubeMaster ──► Cubelet ──► KVM MicroVM
                               │
                           envd (PID 1)
                               │
                           codebuddy CLI
                               │
                           LLM API (egress via CubeEgress)
```

## Prerequisites

- **CubeSandbox deployment** — CubeAPI must be reachable at `http://<host>:3000`
- **Docker** — for building the template image (only needed for in-sandbox/HTTP API modes)
- **`cubemastercli` CLI tool** — installed with CubeSandbox
- **Python 3.10+** with the `e2b-code-interpreter` SDK (`pip install e2b-code-interpreter`)
- **CodeBuddy API key** — generate at [https://www.codebuddy.ai/profile/keys](https://www.codebuddy.ai/profile/keys)
- **Node.js v22 LTS** — only needed if testing CodeBuddy locally outside the sandbox

### Required Environment Variables

| Variable | Description | Example |
|----------|-------------|---------|
| `E2B_API_URL` | CubeAPI address | `http://192.168.1.100:3000` |
| `E2B_API_KEY` | CubeAPI auth key (any non-empty string) | `e2b_000000` |
| `CUBE_TEMPLATE_ID` | CodeBuddy sandbox template ID | (from `cubemastercli`) |
| `CODEBUDDY_API_KEY` | CodeBuddy API key | `ck_xxxxxxxx` |

## Architecture Overview

CubeSandbox supports two integration modes. Both use the same template image;
the difference is **where codebuddy runs**.

### Native Sandbox Mode (Recommended)

CodeBuddy runs on **your machine** (already logged in), and tool calls
(Bash, Read, Write) are routed to the sandbox via the `--sandbox` flag.
LLM API calls go through your local network directly.

```
User / CI Pipeline
    │
    ▼
codebuddy CLI (on host, authenticated)
    │
    ├──► LLM API (host network, direct)
    │
    └──► Tool calls via --sandbox flag
              │
              ▼
         CubeAPI (port 3000)
              │
              ▼
    CubeMaster ──► Cubelet ──► KVM MicroVM
                                   │
                               envd (PID 1)
                                   │
                               Run tools
                                   │
                               Result → codebuddy on host
```

**Auth requirement**: `CODEBUDDY_API_KEY` only. No enterprise account needed.

### In-Sandbox Mode

CodeBuddy runs **inside the MicroVM**, fully isolated. Everything —
codebuddy, tool execution, file access — happens within the sandbox.
LLM API calls go through CubeEgress for network control.

```
User / CI Pipeline
    │
    ▼
e2b-code-interpreter SDK (Python)
    │  REST API
    ▼
CubeAPI (port 3000)
    │
    ▼
CubeMaster ──► Cubelet ──► KVM MicroVM
                               │
                           envd (PID 1)
                               │
                           codebuddy CLI
                               │
                           LLM API (via CubeEgress)
```

**Auth requirement**: `CODEBUDDY_API_KEY` + `CODEBUDDY_AUTH_TOKEN` (enterprise/CI accounts only). The sandbox infrastructure has been verified; the conversation step could not be verified without this credential.

## Which Mode Should I Use?

| Use Case | Mode | Needs AUTH_TOKEN? |
|----------|------|-------------------|
| Quick test, personal use | Native sandbox | No — just `CODEBUDDY_API_KEY` |
| CI/CD pipeline, enterprise | In-sandbox | Yes — requires `CODEBUDDY_AUTH_TOKEN` |
| HTTP API service | HTTP API | Yes — requires `CODEBUDDY_AUTH_TOKEN` |

## Integration Steps

### 1. Quick Start — Native Sandbox Mode (Recommended)

This is the simplest integration path. CodeBuddy CLI has built-in `--sandbox` support that speaks the E2B protocol. Since CubeSandbox is E2B-compatible, you can connect CodeBuddy directly to CubeSandbox **without building a custom image or using the Python SDK**.

**Step 1: Set environment variables**

```bash
export E2B_API_URL=http://<cube-host>:3000
export E2B_API_KEY=e2b_000000
export CODEBUDDY_API_KEY=ck_xxxxxxxx
```

> No `CUBE_TEMPLATE_ID` needed — native sandbox uses the default `sandbox-code` template.

**Step 2: Run CodeBuddy with native sandbox routing**

```bash
# CodeBuddy runs on YOUR machine (already authenticated);
# tool calls (bash, file ops) are routed into a CubeSandbox MicroVM.
# This avoids the authentication issue inside the sandbox.
codebuddy --sandbox http://<cube-host>:3000 --sandbox-new \
  -p "List files in /workspace" --output-format json -y
```

Expected output (example):

```json
{
  "result": "Here are the files in /workspace:\n\n- README.md\n- src/\n- package.json\n",
  "session_id": "cb_sess_abc123",
  "usage": {
    "prompt_tokens": 45,
    "completion_tokens": 32,
    "total_tokens": 77
  }
}
```

In this mode:
- CodeBuddy itself runs on your host machine
- The `CODEBUDDY_API_KEY` stays on your host (never enters the sandbox)
- Tool calls (bash, read, write, edit) are executed inside the MicroVM
- `--sandbox-new` creates a fresh sandbox for each invocation
- `--sandbox-id <id>` reconnects to an existing sandbox
- `--sandbox-kill` destroys the sandbox when done
- `--sandbox-upload-dir <dir>` uploads a host directory into the sandbox

This is the recommended path when you don't need CodeBuddy pre-installed in the image. Use the default `sandbox-code` template (or any template with envd on port 49983).

### 2. Full Integration Path — In-sandbox / HTTP API Modes

Use this path when you need CodeBuddy installed **inside** the sandbox (for in-sandbox CI/CD or HTTP API service).

#### Step 1 — Build the Template Image

The template image layers Node.js 22 LTS and CodeBuddy CLI on top of the official `sandbox-code` base image (which includes envd, the CubeSandbox sandbox agent):

```dockerfile
FROM cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

# Install Node.js 22 LTS
# Try official source first, fall back to China mirror for users behind GFW.
# Uses .tar.gz because the base image may not have xz-utils installed.
ENV NODE_VERSION=22.11.0
RUN (curl -fsSL --connect-timeout 10 -o /tmp/node.tar.gz https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz || \
     curl -fsSL -o /tmp/node.tar.gz https://npmmirror.com/mirrors/node/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.gz) && \
    tar -xzf /tmp/node.tar.gz -C /usr/local --strip-components=1 && \
    rm /tmp/node.tar.gz && \
    node --version && npm --version

# Install CodeBuddy CLI globally
# Try default npm registry first, fall back to China mirror.
RUN npm install -g @tencent-ai/codebuddy-code@latest || \
    npm install -g @tencent-ai/codebuddy-code@latest \
        --registry=https://registry.npmmirror.com

# Create workspace directory
RUN mkdir -p /workspace
WORKDIR /workspace

# envd is the existing ENTRYPOINT from sandbox-code base image
# CodeBuddy runs on demand via sb.commands.run()
```

#### Step 2 — Register the Template

Build and push the image, then register it as a CubeSandbox template:

```bash
docker build -t <registry>/codebuddy-sandbox:latest .
docker push <registry>/codebuddy-sandbox:latest

cubemastercli tpl create-from-image \
  --image <registry>/codebuddy-sandbox:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --expose-port 8080 \
  --cpu 4000 --memory 4096 \
  --probe 49983
```

Note the `template_id` from the output.

- **Port 49983**: envd (CubeSandbox sandbox agent, required)
- **Port 8080**: CodeBuddy HTTP API (`codebuddy --serve`, optional — only needed for HTTP API mode)

#### Step 3 — Configure Environment Variables

```bash
export E2B_API_URL=http://<cube-host>:3000
export E2B_API_KEY=e2b_000000
export CUBE_TEMPLATE_ID=<template-id>
export CODEBUDDY_API_KEY=ck_xxxxxxxx
```

> **Authentication limitation**: In-sandbox and HTTP API modes require `CODEBUDDY_AUTH_TOKEN` (enterprise/CI accounts only). Sandbox infrastructure (creation, file upload, script execution) has been verified, but the actual codebuddy conversation inside the sandbox could not be verified without this credential. Personal accounts should use native sandbox mode instead, which works with just `CODEBUDDY_API_KEY`.

#### Step 4 — Run CodeBuddy Inside the Sandbox

In-sandbox mode (simplest):

```python
from e2b_code_interpreter import Sandbox

sb = Sandbox.create(template="your-template-id")
result = sb.commands.run(
    'codebuddy -p "List files in /workspace" --output-format json --max-turns 10',
    user="root"
)
print(result.stdout)
sb.kill()
```

Expected output (example):

```json
{
  "result": "The /workspace directory contains: README.md, src/, package.json",
  "session_id": "cb_sess_def456",
  "usage": {
    "prompt_tokens": 45,
    "completion_tokens": 28,
    "total_tokens": 73
  }
}
```

## Key Code Snippets

### 1. In-Sandbox Mode

The simplest integration — run a single CodeBuddy invocation:

```python
from e2b_code_interpreter import Sandbox
import json
import os

sb = Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"])

cmd = (
    f'codebuddy -p "Analyze the code in /workspace" '
    f'--output-format json --max-turns 10 '
    f'-y'
)
result = sb.commands.run(cmd, user="root")

output = json.loads(result.stdout)
print(output["result"])
print(f"Session: {output['session_id']}")
print(f"Tokens: {output.get('usage', {})}")

sb.kill()
```

### 2. HTTP API Mode

For interactive or long-running scenarios:

```python
from e2b_code_interpreter import Sandbox
import time
import os

sb = Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"])

# Start codebuddy --serve in background
sb.commands.run(
    'nohup codebuddy --serve --port 8080 --hostname 0.0.0.0 '
    '> /tmp/codebuddy.log 2>&1 &',
    user="root"
)

# Wait for server to be ready
for _ in range(30):
    time.sleep(1)
    health = sb.commands.run('curl -s http://localhost:8080/health', user="root")
    if "ok" in health.stdout:
        break

# Call the chat API
result = sb.commands.run(
    'curl -s -X POST http://localhost:8080/api/chat '
    '-H "Content-Type: application/json" '
    '-d \'{"message": "Hello!"}\'',
    user="root"
)
print(result.stdout)
sb.kill()
```

## Caveats

- **envd user**: CubeSandbox's envd only serves the `root` user. Always pass `user="root"` in `sb.commands.run()` and `sb.files.*` calls.
- **API key injection**: The `CODEBUDDY_API_KEY` must be available inside the sandbox. Either bake it into the image (not recommended for production) or inject it via CubeEgress Credential Vault (recommended — keys never enter the sandbox filesystem).
- **Network egress**: CodeBuddy needs outbound HTTPS to LLM API endpoints. Configure CubeEgress allowlist for `api.codebuddy.ai` (or your custom LLM endpoint). Use `Sandbox.create(allow_internet_access=False)` + CIDR allowlist for stricter control.
- **SSL certificates**: If using CubeSandbox's self-signed certificate, set `CUBE_SSL_CERT_FILE` to the CA bundle path. CodeBuddy's Node.js runtime uses the system CA store.
- **Resource limits**: CodeBuddy with large codebases may need >2 GB RAM. Set `--memory 4096` (or higher) when creating the template.
- **Max turns**: Always set `--max-turns` on the CLI to prevent infinite tool-call loops in in-sandbox mode.
- **Model availability**: CodeBuddy has separate model catalogs for the Chinese edition (`codebuddy.cn`, includes GLM/MiniMax/Kimi/Hunyuan models) and international edition (`codebuddy.ai`, includes GPT/Claude/Gemini models). `hy3-preview` is available on `codebuddy.cn`. Override with `--model`.
- **Image registry**: Use `cube-sandbox-int.tencentcloudcr.com` for international access, `cube-sandbox-cn.tencentcloudcr.com` for mainland China.
- **Sandbox-side authentication**: In-Sandbox and HTTP API modes require `CODEBUDDY_AUTH_TOKEN` for codebuddy to authenticate inside the sandbox. This token is currently available to enterprise and CI accounts only. The sandbox infrastructure (creation, file upload, script execution) has been verified. The actual codebuddy conversation inside the sandbox could not be verified due to this credential requirement.

## References

- Related docs:
  - [CubeSandbox Quick Start](../quickstart.md)
  - [CubeSandbox Templates](../templates.md)
  - [CubeSandbox Snapshot & Rollback](../snapshot-rollback-clone.md)
  - [CubeSandbox Network Policy](../network-policy.md)
- Sample repository: [`examples/codebuddy-integration/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/codebuddy-integration)
- Upstream project:
  - [CodeBuddy CLI Documentation](https://www.codebuddy.ai/docs/cli/overview)
  - [CodeBuddy API Keys](https://www.codebuddy.ai/profile/keys)
  - [CubeSandbox on GitHub](https://github.com/TencentCloud/CubeSandbox)
