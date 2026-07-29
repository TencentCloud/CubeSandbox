---
title: Claude Code Integration Guide
author: dcdc4747
date: 2026-07-29
tags:
  - integration
  - claude-code
  - coding-agent
  - agent
lang: en-US
---

# Claude Code Integration Guide

[中文文档](../../zh/guide/integrations/claude-code.md)

Run [Claude Code](https://docs.anthropic.com/en/docs/claude-code) — Anthropic's
agentic coding CLI — inside CubeSandbox MicroVMs. This guide covers image build,
key injection, egress control, and snapshot-based session persistence, and pairs
with the runnable
[`examples/claude-code-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration)
project.

## Integration Target and Version

| Component | Version |
|---|---|
| Claude Code CLI | `@anthropic-ai/claude-code` (pinned via `--build-arg CLAUDE_CODE_VERSION=x.y.z`) |
| Node.js | 24 (installed via NodeSource) |
| CubeSandbox base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| E2B SDK (host driver) | `e2b` (latest) |
| CubeSandbox platform | `>= 0.3.0` (pause/resume) / `>= 0.4.0` (CubeEgress credential vault) |

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- An LLM provider API key. Anthropic is the default; any Anthropic-compatible
  endpoint (e.g., DeepSeek) works via `ANTHROPIC_BASE_URL` and
  `ANTHROPIC_AUTH_TOKEN`.
- Python 3.10+ for the host driver scripts.

## Why Run Claude Code Inside a Sandbox

Claude Code edits files, runs shell commands, installs packages, and can
autonomously chain tool calls. Running it directly on a workstation blends the
agent's blast radius with your dev environment. Running it inside CubeSandbox
gives you:

| Concern | CubeSandbox provides |
|---|---|
| **Isolation** | KVM MicroVM per session, dedicated guest kernel |
| **Reproducibility** | Every session boots from the same template snapshot |
| **Fast spin-up** | Sub-60 ms cold start, so N-parallel agents are cheap |
| **Long tasks** | `sandbox.pause()` snapshots VM + rootfs; resume later |
| **Key hygiene** | CubeEgress injects the auth header on the wire — the VM never sees the real key |
| **Egress audit** | Every request to the Anthropic API is recorded in the egress audit log |

## Integration Steps

### 1. Build the template image

The image stacks Node.js 24 and the Claude Code CLI on top of `cubesandbox-base`,
so envd is already listening on `:49983`.

```dockerfile
# examples/claude-code-integration/Dockerfile (excerpt)
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG NODE_MAJOR=24
ARG CLAUDE_CODE_VERSION=latest

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl git gnupg jq less procps python3 python3-pip ripgrep \
    && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && claude --version \
    && npm cache clean --force \
    && rm -rf /root/.npm /var/lib/apt/lists/*

WORKDIR /workspace
EXPOSE 49983
```

Build and push:

```bash
# For users in China, use the NJU mirror:
#   --build-arg CUBE_BASE_IMAGE=ghcr.nju.edu.cn/tencentcloud/cubesandbox-base:2026.16
docker build --platform linux/amd64 \
  -t <your-registry>/claude-code-cube:latest \
  examples/claude-code-integration
docker push <your-registry>/claude-code-cube:latest
```

### 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/claude-code-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

Once the job reaches `READY`, note the `template_id` — you pass it to every
`Sandbox.create()` call. `4G` writable layer suits medium tasks; bump to `8G+`
if the agent installs large toolchains (e.g., `claude` plugins, MCP servers).

### 3. Wire up the host driver

```bash
cd examples/claude-code-integration
cp .env.example .env
# fill in E2B_API_URL, CUBE_TEMPLATE_ID, and your API key
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `ANTHROPIC_API_KEY` | `envs=...` (direct) or CubeEgress inject (vault) | Provider key (standard) |
| `ANTHROPIC_AUTH_TOKEN` | `envs=...` (direct) | Provider key (third-party endpoints, e.g. DeepSeek) |
| `ANTHROPIC_BASE_URL` | Passed into the exec env | For API gateways / compatible endpoints |
| `CC_MODEL` | `--model` flag | Default: `claude-sonnet-4-6` |
| `CC_EFFORT` | `--effort` flag | Effort level: low, medium, high, xhigh, max |
| `CC_LLM_HOST` | `network_policy.py` | API host allowed under default-deny egress |

### 4. Runtime Configuration and API Key Injection

Claude Code is invoked headlessly with `-p` (process the prompt and exit, no
interactive TUI) and `--output-format json` for machine-readable output. Two
key-flow flavors share the same template:

**Direct flavor** — forward the key per command. `e2b`'s `commands.run(envs=...)`
puts the environment into the exec envelope, not into a persistent file inside
the VM, so the key lives only for the lifetime of that command:

```python
result = sandbox.commands.run(
    "cd /workspace && claude -p 'Refactor the auth module.' "
    "--output-format json --model claude-sonnet-4-6 "
    "--dangerously-skip-permissions",
    envs={"ANTHROPIC_API_KEY": key},
    user="user",
    timeout=900,
)
```

**Vault flavor** — keep the key out of the VM entirely (see step 6).

The example scripts parse the JSON stream and print a concise transcript by
default (assistant text, tool calls, and any errors); pass `--raw` (or set
`CC_STREAM_RAW=1`) to see the raw JSON event stream.

### 5. Session Persistence (pause / resume)

```bash
python resume_claude_code.py
```

This mirrors the [snapshot / clone / rollback](../snapshot-rollback-clone.md)
engine at the SDK layer:

- `sandbox.pause()` snapshots the running VM (memory + rootfs) and frees compute.
- `Sandbox.connect(sandbox_id)` resumes with `/workspace`, Claude Code's state
  directory (`/home/user/.claude` when running as non-root), and every other file intact.

> **Lifecycle caveat:** manage the sandbox lifecycle with `try/finally`, not a
> `with Sandbox.create(...)` context manager. On `__exit__` the context manager
> kills the sandbox, which would undo the pause. The example creates the sandbox
> explicitly and only calls `sandbox.kill()` in `finally`.

```python
sandbox = Sandbox.create(template=template_id, timeout=1800)
try:
    run_turn(sandbox, prompt_1)          # writes /workspace/plan.md
    sandbox_id = sandbox.pause() or sandbox.sandbox_id
    sandbox = Sandbox.connect(sandbox_id)
    assert_state_survived(sandbox)       # /workspace + state-dir intact
    run_turn(sandbox, prompt_2)          # continues the work
finally:
    sandbox.kill()
```

### 6. Network and Egress Policy (credential vault)

`network_policy.py` demonstrates the recommended pattern for shared clusters:
default-deny egress plus on-the-wire key injection.

```python
# Credential injection uses the native cubesandbox SDK (see security-proxy.md).
from cubesandbox import Sandbox, Rule, Match, Action, Inject

host = "api.anthropic.com"
rules = [
    Rule(
        name="allow_anthropic_api",
        match=Match(scheme="https", sni=host, host=host),
        action=Action(allow=True, audit="metadata", inject=[
            Inject(header="x-api-key", secret=ANTHROPIC_API_KEY, format="${SECRET}"),
            Inject(header="anthropic-version", secret="2023-06-01", format="${SECRET}"),
        ]),
    ),
]

sandbox = Sandbox.create(
    template=CUBE_TEMPLATE_ID,
    allow_internet_access=False,   # default-deny; the rule's host is auto-allowed
    network={"rules": rules},
)
```

Effect:

- `printenv ANTHROPIC_API_KEY` inside the sandbox shows only a placeholder.
- Every request to `api.anthropic.com` gets the auth headers attached on the wire.
- Anything else is dropped by CubeVS at L3/L4 (`allow_internet_access=False`) and
  never leaves the sandbox.
- Every allow / deny decision lands in the egress audit log.

For Anthropic-compatible gateways (e.g., DeepSeek), adjust the host in the rule
and set `ANTHROPIC_BASE_URL` in the environment.

## Use Cases and Best Practices

- **Isolated development.** Run Claude Code inside the sandbox so its file
  edits and shell commands cannot touch the host.
- **Execute agent-generated code and collect results.** Have Claude Code write to
  `/workspace`, then read artifacts back via `sandbox.files` or `commands.run`.
- **Checkpoint / resume long tasks.** Use `pause()` + `connect()` to snapshot a
  long refactor and resume later, or fork multiple task variants off one snapshot.
- **Preinstall heavy dependencies** into the template rather than fetching them
  at runtime, especially under a default-deny egress policy.
- **Batch processing.** Run multiple Claude Code instances in parallel sandboxes
  for code review, migration, or analysis pipelines.

## Key Code Snippets

### Headless Claude Code invocation

```python
cmd = (
    "cd /workspace && claude -p "
    "'Inspect the project, run app.py, and write a summary to result.md.' "
    "--output-format json --model claude-sonnet-4-6 "
    "--dangerously-skip-permissions"
)
result = sandbox.commands.run(cmd, envs=cc_env, user="user", timeout=900)
```

### Preflight version check

```python
version = sandbox.commands.run("claude --version", timeout=60)
```

### Using a specific effort level

```python
# Control reasoning depth: low, medium, high, xhigh, max
cmd = (
    "claude -p 'Audit this code for security issues.' "
    "--output-format json --effort high"
)
```

### Setting permission mode for automation

```python
# plan: ask before edits (default); acceptEdits: auto-approve edits;
# bypassPermissions: full auto (non-root required)
cmd = (
    "claude -p 'Fix all lint errors in src/' "
    "--output-format json --permission-mode acceptEdits"
)
```

### Dangerously skip all permissions (sandbox only)

```python
# Recommended for isolated sandboxes. Must run as non-root user;
# Claude Code rejects --dangerously-skip-permissions with root/sudo.
cmd = (
    "claude -p 'Do the task' "
    "--output-format json --dangerously-skip-permissions"
)
result = sandbox.commands.run(cmd, envs=cc_env, user="user", timeout=900)
```

## Caveats

- **Node.js version.** Claude Code needs a recent Node runtime; the base image
  ships an older apt Node, so always install via NodeSource (the Dockerfile does).
- **Agent state directory.** When running as `user` (recommended for headless
  automation), Claude Code stores its session cache at `/home/user/.claude`.
  When running as `root`, the path is `/root/.claude`. Keep it empty in the
  image to avoid leaking sessions across tenants; it is created at build time
  but not populated with any credentials.
- **Direct-flavor key persistence.** With the direct flavor (`envs=`) the key is
  scoped to the exec call, but sandbox snapshots may capture the in-VM environment.
  For strict isolation prefer the vault flavor (`network_policy.py`), where the
  key never enters the VM.
- **CubeEgress CA (Node).** For the vault flavor the sandbox must trust the
  CubeEgress root CA, which the base image installs into the system bundle.
  Claude Code runs on Node.js, which ignores the system store, so
  `network_policy.py` also sets `NODE_EXTRA_CA_CERTS` (override via
  `CC_NODE_EXTRA_CA_CERTS`) — without it the vault path fails with TLS errors.
- **Headless only.** Claude Code's interactive TUI is not available over the
  E2B protocol. Use `-p` / `--print` with `--output-format json` for
  machine-readable output, and drive multi-turn conversations from the host
  script.
- **Permission mode.** In automated sandbox environments, use
  `--dangerously-skip-permissions` (requires non-root user) or
  `--permission-mode acceptEdits` (works with root) for autonomous execution.
  `plan` mode (default) requires human approval for each tool call, which is
  impractical in a headless sandbox. Note: `--dangerously-skip-permissions`
  is rejected when Claude Code runs as root for security reasons.
- **Egress side-effects.** Tasks that `npm install` or fetch MCP tools need those
  hosts allowed or preinstalled into the template.
- **API rate limits.** Claude Code interacts with the Anthropic API; standard
  rate limits and token quotas apply. For high-throughput batch processing,
  distribute across multiple API keys and sandboxes.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `claude: command not found` in preflight | Template not rebuilt after CLI change | Rebuild the image, re-register the template |
| API auth failure | Key not forwarded (direct) or missing inject rule (vault) | Pass `envs={...}` or fix the rule's `sni`/`host` |
| `403 Forbidden - CubeEgress` | Default-deny with no matching allow rule | Add `api.anthropic.com` (and any extra hosts) to the rules |
| TLS / connection error from Claude Code (vault) | Node.js ignores system CA store | Set `NODE_EXTRA_CA_CERTS` as shown in `network_policy.py` |
| Template creation stuck in `PULLING` | Registry unreachable from Cube nodes | Push to a registry the cluster can reach; supply auth if needed |
| Readiness probe timeout | Base image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `pause()` / `connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |
| Claude Code hangs / no output | TUI mode launched over non-TTY channel | Always use `-p` / `--print` in headless mode |
| Token limit exceeded | Task too large for the configured thinking budget | Lower `--thinking` budget or split into smaller tasks |

## References

- Runnable example: [`examples/claude-code-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration)
- Bring Your Own Image: [`docs/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- Template from image: [`docs/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- Credential vault + egress control: [`docs/guide/security-proxy.md`](../security-proxy.md)
- Claude Code: <https://docs.anthropic.com/en/docs/claude-code>
- E2B Claude Code integration: <https://e2b.dev/docs/agents/claude-code>
