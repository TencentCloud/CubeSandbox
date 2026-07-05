---
title: Claude Code Integration Guide
author: shsaihdsaiudh
date: 2026-07-05
tags:
  - integration
  - claude-code
  - coding-agent
lang: en-US
---

# Claude Code

[Claude Code](https://docs.anthropic.com/en/docs/claude-code) is a terminal-based AI coding agent developed by Anthropic. It runs code, edits files, and executes commands in a terminal environment — making CubeSandbox a natural execution backend for isolated, reproducible development workflows.

This guide covers integrating Claude Code as a **sandboxed coding agent** running inside CubeSandbox MicroVMs.

## Architecture

```
Host (orchestrator)
    │  e2b-code-interpreter / E2B SDK
    │  Python scripts (run_claude_code.py, network_policy.py, etc.)
    ▼
CubeAPI (:3000) ──► CubeMaster ──► Cubelet ──► KVM MicroVM
                                                    │
                                                Claude Code CLI (Node.js)
                                                    │  -p / --print (headless)
                                                    │  ANTHROPIC_AUTH_TOKEN
                                                    │  ANTHROPIC_BASE_URL
                                                    ▼
                                                LLM API (DeepSeek / Anthropic / OpenAI)
```

## Prerequisites

- Running [CubeSandbox deployment](/guide/quickstart)
- Python 3.8+ with `e2b-code-interpreter`
- A CubeSandbox code template (see [Template Creation](#template-creation) below)
- API key for your LLM provider (DeepSeek, Anthropic, or OpenAI-compatible)

## Template Creation

### Option 1: Build a custom image (recommended)

Create a Dockerfile that extends the CubeSandbox base image with Claude Code:

```dockerfile
FROM cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest

ENV NPM_CONFIG_PREFIX=/usr/local

RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs \
    && npm install -g @anthropic-ai/claude-code
```

```bash
docker build -t claude-code-sandbox:v1 .
docker push <your-registry>/claude-code-sandbox:v1

cubemastercli tpl create-from-image \
  --image <your-registry>/claude-code-sandbox:v1 \
  --writable-layer-size 2G \
  --expose-port 49999 \
  --probe 49999
```

### Option 2: Use sandbox-code with runtime install

::: warning Startup overhead
Installing Node.js + Claude Code at sandbox creation adds ~30-60 seconds to cold start. Prefer Option 1 for production.
:::

```bash
# Create template from the base code image
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 2G \
  --expose-port 49999 \
  --probe 49999
```

Then install Claude Code in your sandbox init script:

```python
sandbox = Sandbox.create(template_id)

# Install Node.js + Claude Code
sandbox.commands.run("curl -fsSL https://deb.nodesource.com/setup_22.x | bash -")
sandbox.commands.run("apt-get install -y nodejs")
sandbox.commands.run("npm install -g @anthropic-ai/claude-code")
```

## Environment Configuration

Claude Code requires these environment variables inside the sandbox:

| Variable | Description | Example |
|----------|-------------|---------|
| `CC_PROVIDER` | LLM provider: `deepseek`, `anthropic`, or `openai` | `deepseek` |
| `ANTHROPIC_AUTH_TOKEN` | API key (DeepSeek / Anthropic) | `sk-a1b2c3d4...` (DeepSeek) or `sk-ant-...` (Anthropic) |
| `ANTHROPIC_BASE_URL` | API endpoint URL (DeepSeek / Anthropic) | `https://api.deepseek.com/anthropic` |
| `ANTHROPIC_MODEL` | Model name | `deepseek-v4-pro` |
| `OPENAI_API_KEY` | API key (OpenAI-compatible providers) | `sk-...` |
| `OPENAI_BASE_URL` | API endpoint URL (OpenAI-compatible providers) | `https://api.openai.com/v1` |

Set `CC_PROVIDER=anthropic` to switch to the Anthropic API, or `CC_PROVIDER=openai` for OpenAI-compatible providers (which require `OPENAI_API_KEY` and `OPENAI_BASE_URL`).

::: tip Using DeepSeek?
Set `ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic` and use DeepSeek model names like `deepseek-v4-pro`.
:::

## Usage Modes

### 1. One-shot Execution

Run a single prompt and collect the result. Suitable for CI/CD and code generation.

```bash
python run_claude_code.py "Write a Python function to sort a list of integers"
```

**How it works:**

1. Creates a sandbox from the template
2. Sets up environment variables
3. Runs `claude --print '<prompt>'` headlessly
4. Streams stdout back and destroys the sandbox

### 2. Session Persistence (Pause/Resume)

Start a session, pause the sandbox, and resume later with full context.

```bash
# Start a session
python resume_claude_code.py "Set up a FastAPI project skeleton"

# Output: Sandbox paused. Resume with:
#   python resume_claude_code.py --resume-from <sandbox-id>

# Later: resume and continue
python resume_claude_code.py --resume-from <sandbox-id> "Add a /health endpoint"
```

**How it works:**

1. Creates a sandbox, runs Claude Code, then calls `sandbox.pause()`
2. The entire VM state (memory, disk, process tree) is snapshotted
3. Later, `Sandbox.connect(sandbox_id)` restores the VM
4. Claude Code resumes with all conversation context and file changes intact

### 3. Secure Egress with API Key Injection

The sandbox has **no internet access** by default. Only the LLM API host is allowed through CubeEgress, which injects real credentials on the wire.

```bash
# Production (default): sandbox has no internet, needs prebuilt image
python network_policy.py "Analyze this code for security vulnerabilities"

# First-time setup: allow internet to install Claude Code at runtime
python network_policy.py --allow-internet "Analyze this code for security vulnerabilities"
```

**How it works:**

1. Sandbox created with `allow_internet_access=False` (default; use `--allow-internet` for first-time install)
2. CubeEgress rules allow only the LLM API host (e.g., `api.deepseek.com`)
3. Placeholder `ANTHROPIC_AUTH_TOKEN=sk-placeholder` is set in the sandbox
4. CubeEgress intercepts TLS traffic and replaces `Authorization: Bearer sk-placeholder` with the real key
5. The sandbox never sees the real API key — it can't exfiltrate it even if compromised

## Provider Support

| Provider | `ANTHROPIC_BASE_URL` | Default Model |
|----------|---------------------|---------------|
| DeepSeek | `https://api.deepseek.com/anthropic` | `deepseek-v4-pro` |
| Anthropic | `https://api.anthropic.com` | `claude-sonnet-4-6` |
| OpenAI | *(set `OPENAI_BASE_URL`)* | `gpt-4o` |

Set via environment variables or `.env` file:

```bash
# .env
CC_PROVIDER=deepseek
ANTHROPIC_AUTH_TOKEN=sk-...
ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic
ANTHROPIC_MODEL=deepseek-v4-pro
```

## Best Practices

### Isolation Development

```
                   ┌─────────────────────────┐
                   │  CubeSandbox MicroVM     │
                   │                         │
  Host             │  /workspace/            │
  git clone ──────►│  ├── src/               │
                   │  ├── tests/             │
                   │  └── ...                │
                   │                         │
                   │  Claude Code edits here │
                   │  (safe, disposable)     │
                   └─────────────────────────┘
```

1. **Clone to sandbox**: Mount host directories via `metadata.host-mount` or copy files in at creation time
2. **Edit inside sandbox**: Claude Code modifies files in the isolated VM
3. **Review + extract**: Read results back via `sandbox.files.read()` before destroying

### Long-running Tasks with Snapshots

For tasks that take hours (large refactors, multi-file changes):

1. Start Claude Code with `resume_claude_code.py`
2. Periodically pause: `sandbox.pause()` creates a point-in-time snapshot
3. If something goes wrong, rollback to the last snapshot
4. Resume from the snapshot to continue

### Code Execution and Result Collection

```
Orchestrator
    │ 1. Create sandbox
    ▼
Sandbox (Claude Code generates code)
    │ 2. Claude Code writes solution.py
    │ 3. Execute: python solution.py
    │ 4. Read output via sandbox.files
    ▼
Host (collect results, destroy sandbox)
```

## Troubleshooting

### `claude: command not found`

Claude Code is not installed in the template. Rebuild using Option 1 (Dockerfile), or install at runtime (Option 2).

### `ANTHROPIC_AUTH_TOKEN not set`

The sandbox doesn't have the API key configured. Check your `.env` file and verify `build_claude_env()` returns the expected values.

### SSL Certificate Error

If connecting to CubeProxy over HTTPS:

```bash
# In sandbox
export SSL_CERT_FILE=/usr/local/share/ca-certificates/cube-ca.crt
export NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/cube-ca.crt
```

Or set in the Python script via env vars.

### Connection Timeout to LLM API

The sandbox can't reach the LLM API. Check:
1. Network policy allows the host (use `network_policy.py` for a working example)
2. The API hostname resolves correctly inside the sandbox
3. If using CubeEgress, verify the rules are being applied

### Template Not Found

Verify the template exists and is in READY state:

```bash
cubemastercli tpl list
```

Make sure `CUBE_TEMPLATE_ID` matches a template with `STATUS: READY`.

## Example Repository

See the full runnable example at [`examples/claude-code-integration/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration), which includes:

- `Dockerfile` — Build sandbox image with Claude Code
- `run_claude_code.py` — One-shot execution
- `resume_claude_code.py` — Pause/resume session persistence
- `network_policy.py` — Secure egress with key injection
- `env_utils.py` — Environment and credential helpers
