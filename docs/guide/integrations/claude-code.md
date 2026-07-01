---
title: Claude Code Integration Guide
author: LangQi99
date: 2026-07-01
tags:
  - integration
  - claude-code
  - anthropic
  - agent
lang: en-US
---

# Claude Code Integration Guide

[中文文档](../../zh/guide/integrations/claude-code.md)

Run [Anthropic Claude Code](https://docs.anthropic.com/en/docs/claude-code) —
the terminal-native AI coding agent — inside CubeSandbox MicroVMs. This guide
covers everything from image build to production egress control, and pairs
with the runnable [`examples/claude-code-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration)
project.

## Integration Target and Version

| Component | Version |
|---|---|
| Claude Code CLI | `@anthropic-ai/claude-code` (latest at build time; pinned via `--build-arg CLAUDE_CODE_VERSION=x.y.z`) |
| Node.js | 20 LTS |
| CubeSandbox base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| E2B SDK (host driver) | `e2b >= 2.4.1` |
| CubeSandbox platform | `>= 0.3.0` (pause/resume) / `>= 0.4.0` (credential vault via CubeEgress) |

## Prerequisites

- CubeSandbox deployment reachable at CubeAPI (`http://<node>:3000`)
- `cubemastercli` on `$PATH` and connected to the cluster
- Docker on the workstation used to build the template image, plus a registry
  the Cube nodes can pull from
- Anthropic API key (`sk-ant-...`) — or a compatible gateway (Bedrock, Vertex,
  in-house Anthropic proxy)
- Python 3.10+ for the host driver scripts

## Why Run Claude Code Inside a Sandbox

Claude Code is a terminal agent that edits files, runs commands, and installs
packages. Running it directly on your workstation blends the agent's blast
radius with your dev environment. Running it inside CubeSandbox gives you:

| Concern | CubeSandbox provides |
|---|---|
| **Isolation** | KVM MicroVM per session, dedicated guest kernel |
| **Reproducibility** | Every session boots from the same template snapshot |
| **Fast spin-up** | Sub-60 ms cold start, so N-parallel agents are cheap |
| **Long tasks** | `sandbox.pause()` snapshots VM + rootfs; resume later |
| **Key hygiene** | CubeEgress injects `x-api-key` on the wire — the VM never sees it |
| **Egress audit** | Every request to `api.anthropic.com` lands in a JSONL audit log |

## Architecture

```
┌────────────────────────┐        ┌───────────────────────┐
│  Host driver script     │  E2B  │  CubeSandbox MicroVM  │
│  (run_claude.py)        │       │                       │
│                         │──────►│  envd (:49983)        │
│  ANTHROPIC_API_KEY      │       │  claude CLI (Node 20) │
│  optional               │       │  git / python / rg    │
│                         │       │  /workspace           │
└──────────┬──────────────┘       └───────────┬───────────┘
           │                                  │ HTTPS
           │                                  ▼
           │                           ┌───────────────┐
           └── inject rule ──────────► │  CubeEgress   │───► api.anthropic.com
                                       └───────────────┘
                                              │
                                              ▼
                                  /data/log/cube-egress/access.jsonl
```

Two integration flavors are supported and interchangeable at the SDK level:

| Flavor | Key flow | Best for |
|---|---|---|
| **Direct** | `envs={"ANTHROPIC_API_KEY": key}` per exec call | Single-tenant dev machines, quick trials |
| **Vault** | `Sandbox.create(network={"rules": [inject rule]})` | Shared clusters, hosted services, audit-heavy environments |

Both flavors share the same template — the only difference is whether the key
is forwarded into the sandbox or attached in-flight by CubeEgress.

## Integration Steps

### 1. Build the template image

The image stacks Node.js 20 and Anthropic's official CLI on top of
`cubesandbox-base`, so envd is already listening on `:49983`.

```dockerfile
# examples/claude-code-integration/Dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl gnupg git jq ripgrep less \
      python3 python3-pip build-essential \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @anthropic-ai/claude-code \
    && claude --version

ENV CLAUDE_CODE_HOME=/root/.claude
WORKDIR /workspace
EXPOSE 49983
```

Build and push:

```bash
docker build -t <your-registry>/claude-code-cube:latest \
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

Once the job reaches `READY`, note the `template_id` — you'll pass it to every
`Sandbox.create()` call. `4G` writable layer is enough for medium tasks; bump
it to `8G+` if the agent is expected to install large toolchains.

### 3. Wire up the host driver

```bash
cd examples/claude-code-integration
cp env.example .env
# fill in E2B_API_URL, CUBE_TEMPLATE_ID, ANTHROPIC_API_KEY
pip install -r requirements.txt
```

Direct-flavor smoke test:

```bash
python run_claude.py --prompt "Create hello.py that prints 'Hello from CubeSandbox!' and run it."
```

### 4. Enable pause / resume for long tasks

```bash
python resume_claude.py
```

This mirrors the [snapshot / clone / rollback](../snapshot-rollback-clone.md)
engine at the SDK layer:

- `sandbox.pause()` snapshots the running VM (memory + rootfs) to disk and
  frees compute resources.
- `Sandbox.connect(sandbox_id)` resumes from the snapshot with `/workspace`,
  `/root/.claude/`, and every other file intact.
- Multiple resumes off the same snapshot let you fork task variants without
  rerunning the setup.

### 5. Vault-flavor: keep the key out of the VM

`network_policy.py` demonstrates the recommended pattern for shared clusters:

```python
rules = [
    {
        "name": "allow_anthropic_api",
        "match": {
            "scheme": "https",
            "sni": "api.anthropic.com",
            "host": "api.anthropic.com",
        },
        "action": {
            "allow": True,
            "audit": "metadata",
            "inject": [
                {"header": "x-api-key",         "format": "${SECRET}",
                 "secret": ANTHROPIC_API_KEY},
                {"header": "anthropic-version", "format": "2023-06-01"},
            ],
        },
    },
]

with Sandbox.create(
    template=CUBE_TEMPLATE_ID,
    allow_internet_access=True,
    network={"rules": rules},
) as sandbox:
    ...
```

Effect:

- `printenv | grep ANTHROPIC_API_KEY` inside the sandbox returns nothing.
- Every request to `api.anthropic.com` gets `x-api-key` attached on the wire.
- Anything else is default-denied and returned as `403 Forbidden - CubeEgress`
  before touching the network.
- Every allow / deny decision lands in `/data/log/cube-egress/access.jsonl`.

## Key Code Snippets

### Headless `claude` invocation

Use `--print` (disables the interactive TUI) plus `--allowedTools` (skips the
per-tool permission prompt for whitelisted commands):

```python
cmd = (
    "cd /workspace && claude --print "
    "--allowedTools 'Bash(npm:*),Bash(node:*),Bash(python3:*),Edit,Write,Read' "
    f"-- {shlex.quote(prompt)}"
)
result = sandbox.commands.run(
    cmd, envs={"ANTHROPIC_API_KEY": key}, user="root", timeout=300,
    on_stdout=lambda m: sys.stdout.write(m),
    on_stderr=lambda m: sys.stderr.write(m),
)
```

Add `--verbose --output-format stream-json` when the caller wants a
machine-readable event stream (one JSON object per turn).

### Passing secrets via `envs` (direct flavor)

`e2b`'s `commands.run(envs=...)` puts the environment into the exec envelope,
not into a persistent env file inside the VM — the key lives only for the
lifetime of that command:

```python
sandbox.commands.run(
    "claude --print -- 'do something'",
    envs={"ANTHROPIC_API_KEY": key, "ANTHROPIC_MODEL": "claude-sonnet-4-5"},
    user="root",
)
```

### Uploading a seed project

```python
sandbox.files.write(
    f"{workspace}/{Path(seed).name}",
    Path(seed).read_bytes(),
    user="root",
)
```

### Pause / resume around a task

```python
sandbox = Sandbox.create(template=template_id, timeout=1800)
run_claude(sandbox, prompt_1)
sandbox.pause()

# ... hours later ...

sandbox = Sandbox.connect(sandbox_id)
run_claude(sandbox, prompt_2)  # /workspace + /root/.claude are intact
```

## Caveats

- **Node.js version.** Claude Code needs Node ≥ 18. The base image ships
  Ubuntu 22.04, whose apt Node is too old — always install via NodeSource.
- **Agent state directory.** `/root/.claude/` holds Claude Code's local
  session cache. Baking a stale directory into the image can leak previous
  sessions across tenants; the Dockerfile deliberately keeps it empty.
- **`--dangerously-skip-permissions`.** Skipping tool permissions is only
  reasonable inside a sandbox you're OK to lose. Prefer explicit
  `--allowedTools` whitelists.
- **CubeEgress CA.** For the vault flavor to work, the sandbox must trust
  CubeEgress's root CA. This is on by default for templates built via
  `cubemastercli tpl create-from-image`. If you set `--with-cube-ca=false`,
  set `SSL_CERT_FILE` inside the agent env to the correct bundle instead.
- **Egress side-effects.** Some `claude` tasks want to `npm install` or fetch
  MCP tools — either preinstall them into the template or add
  `registry.npmjs.org` (and, for MCP servers, the specific hosts) to your
  allow rules.
- **Interactive TTY features.** The Claude Code TUI (multi-line editor,
  `/` slash commands) is not available over the E2B protocol. Use `--print`
  headless mode and drive multi-turn conversations from the host script.
- **Network-agent auto-resume.** If you enable `on_timeout: pause,
  auto_resume: True`, the platform pauses idle sandboxes for you and wakes
  them up on the next request — see the [`auto-resume.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/code-sandbox-quickstart/auto-resume.py)
  demo for the pattern.
- **Template writable layer.** Node's `npm` cache alone can hit hundreds of
  MB. `4G` writable layer is a safe default; long refactor sessions with
  large dependencies may need `8G` or more.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `claude: command not found` in preflight | Template not rebuilt after CLI upgrade | Rebuild the image, re-register template |
| `Invalid API key · Please run /login` | Key not forwarded (direct flavor) or missing inject rule (vault flavor) | Add `envs={"ANTHROPIC_API_KEY": ...}` or fix the rule's `sni` / `host` |
| `403 Forbidden — CubeEgress` | Default-deny with no matching allow | Add `Match(sni="api.anthropic.com", scheme="https")` |
| SSL handshake failure to `api.anthropic.com` | Sandbox doesn't trust CubeEgress CA | Rebuild template with `--with-cube-ca=true` (default), or wire `SSL_CERT_FILE` correctly |
| Template creation stuck in `PULLING` | Registry unreachable from Cube nodes | Push to a registry the cluster can reach; supply auth if needed |
| Readiness probe timeout | Base image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| Agent hangs waiting on Bash tool | No `--allowedTools`, no TTY | Always run headless with `--print --allowedTools '...'` |
| `sandbox.pause()` errors on 0.2.x | Snapshot engine requires 0.3.0+ | Upgrade CubeSandbox platform |

## References

- Runnable example: [`examples/claude-code-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/claude-code-integration)
- Bring Your Own Image: [`docs/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- Template pipeline: [`docs/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- Credential vault + egress control: [`docs/guide/security-proxy.md`](../security-proxy.md)
- Claude Code CLI reference: <https://docs.anthropic.com/en/docs/claude-code/cli-reference>
- Claude Code SDK / headless mode: <https://docs.anthropic.com/en/docs/claude-code/sdk>
