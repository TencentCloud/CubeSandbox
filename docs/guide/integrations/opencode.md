---
title: OpenCode Integration Guide
author: chaojixinren
date: 2026-07-10
tags:
  - integration
  - opencode
  - coding-agent
  - agent
lang: en-US
---

# OpenCode Integration Guide

[中文文档](../../zh/guide/integrations/opencode.md)

Run [OpenCode](https://www.npmjs.com/package/opencode-ai) — a terminal-native AI
coding agent — inside CubeSandbox MicroVMs. This guide covers image build, key
injection, egress control, and snapshot-based session persistence, and pairs with
the runnable
[`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
project.

## Integration Target and Version

| Component | Version |
|---|---|
| OpenCode CLI | `opencode-ai@1.17.18` (pinned via `ARG OPENCODE_VERSION`) |
| Node.js | 24 (installed via NodeSource) |
| CubeSandbox base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| CubeSandbox SDK (host driver) | `cubesandbox==0.3.0` |
| CubeSandbox platform | `>= 0.3.0` (pause/resume) / `>= 0.4.0` (CubeEgress credential vault) |

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- An LLM provider API key. Supported providers: `anthropic`, `openai`, `deepseek`,
  and `openrouter`.
- Python 3.10+ for the host driver scripts.

## Why Run OpenCode Inside a Sandbox

OpenCode is a terminal agent that edits files, runs commands, and installs
packages. Running it directly on a workstation blends the agent's blast radius
with your dev environment. Running it inside CubeSandbox gives you:

| Concern | CubeSandbox provides |
|---|---|
| **Isolation** | KVM MicroVM per session, dedicated guest kernel |
| **Reproducibility** | Every session boots from the same template snapshot |
| **Fast spin-up** | Sub-60 ms cold start, so N-parallel agents are cheap |
| **Long tasks** | `sandbox.pause()` snapshots VM + rootfs; resume later |
| **Key hygiene** | CubeEgress injects the auth header on the wire — the VM never sees the real key |
| **Egress audit** | Every request to the LLM API is recorded in the egress audit log |

## Integration Steps

### 1. Build the template image

The image stacks Node.js 24 and the OpenCode CLI on top of `cubesandbox-base`, so
envd is already listening on `:49983`.

```dockerfile
# examples/opencode-integration/Dockerfile (excerpt)
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG NODE_MAJOR=24
ARG OPENCODE_VERSION=1.17.18

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash ca-certificates curl git gnupg jq less procps \
        python3 python3-pip ripgrep \
    && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g "opencode-ai@${OPENCODE_VERSION}" \
    && opencode --version \
    && npm cache clean --force \
    && rm -rf /root/.npm

WORKDIR /workspace
EXPOSE 49983
```

Build and push:

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/opencode-cube:latest \
  examples/opencode-integration
docker push <your-registry>/opencode-cube:latest
```

### 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/opencode-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

Once the job reaches `READY`, note the `template_id` — you pass it to every
`Sandbox.create()` call. `4G` writable layer suits medium tasks; bump to `8G+`
if the agent installs large toolchains.

### 3. Wire up the host driver

```bash
cd examples/opencode-integration
cp .env.example .env
# fill in CUBE_API_URL, CUBE_TEMPLATE_ID, and your provider key
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `CUBE_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `OPENCODE_PROVIDER` | Provider selection | `anthropic`, `openai`, `deepseek`, or `openrouter` |
| `OPENCODE_MODEL` | OpenCode `--model` flag | Must use `provider/model` form |
| `ANTHROPIC_API_KEY` (or provider key) | `envs=...` (direct) or CubeEgress inject (vault) | Provider key |
| `OPENCODE_LLM_HOST` | `network_policy.py` | Override LLM host for CubeEgress rules |
| `OPENCODE_WORKSPACE` | `--dir` flag | Workspace inside the sandbox |
| `OPENCODE_SANDBOX_TIMEOUT` | `Sandbox.create(timeout=...)` | Sandbox lifetime in seconds |
| `OPENCODE_EXEC_TIMEOUT` | `commands.run(timeout=...)` | Per-command timeout |
| `OPENCODE_NODE_CA_BUNDLE` | `NODE_EXTRA_CA_CERTS` | CA bundle for CubeEgress TLS (vault) |

### 4. Runtime Configuration and API Key Injection

OpenCode is invoked non-interactively with `opencode run --model <model>
--dir <workspace> --auto --format json <prompt>`. The `--auto` flag approves
tool calls without user confirmation, `--format json` emits machine-readable
JSON events, and the prompt is the trailing positional argument. Two key-flow
flavors share the same template:

**Direct flavor** — forward the key per command. The `cubesandbox` SDK's
`commands.run(envs=...)` puts the environment into the exec envelope, not into
a persistent file inside the VM, so the key lives only for the lifetime of that
command:

```python
result = sandbox.commands.run(
    "opencode run --model anthropic/claude-sonnet-4-6 "
    "--dir /workspace --auto --format json 'do something'",
    envs={"ANTHROPIC_API_KEY": key},
    user="root",
    timeout=900,
)
```

**Vault flavor** — keep the key out of the VM entirely (see step 6).

The example scripts parse the JSON event stream and print a concise transcript
by default; the raw output is available through `result.stdout`.

### 5. Session Persistence (pause / resume)

```bash
python snapshot_restore.py
```

This mirrors the [snapshot / clone / rollback](../snapshot-rollback-clone.md)
engine at the SDK layer:

- `sandbox.pause()` snapshots the running VM (memory + rootfs) and frees compute.
- `Sandbox.connect(sandbox_id)` resumes with `/workspace`, OpenCode's state
  directories (`/root/.config/opencode`, `/root/.local/share/opencode`), and
  every other file intact.

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
    assert_state_survived(sandbox)       # /workspace + OpenCode state intact
    run_turn(sandbox, prompt_2, continue_last=True)  # continues the work
finally:
    sandbox.kill()
```

The resumed turn passes `--continue` so OpenCode picks up the previous session
context rather than starting a new one.

### 6. Network and Egress Policy (credential vault)

`network_policy.py` demonstrates the recommended pattern for shared clusters:
default-deny egress plus on-the-wire key injection.

```python
# Credential injection uses the native cubesandbox SDK (see security-proxy.md).
from cubesandbox import Sandbox, Rule, Match, Action, Inject

host = "api.anthropic.com"
rules = [
    Rule(
        name="allow_anthropic_llm",
        match=Match(scheme="https", sni=host, host=host),
        action=Action(allow=True, audit="metadata", inject=[
            Inject(header="x-api-key", secret=ANTHROPIC_API_KEY, format="${SECRET}"),
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
- Every request to the LLM host gets the auth header attached on the wire.
- Anything else is dropped by CubeVS at L3/L4 (`allow_internet_access=False`) and
  never leaves the sandbox.
- Every allow / deny decision lands in the egress audit log.

For non-Anthropic providers the example injects an `Authorization: Bearer` header
instead. If a provider does not accept a header-injected key, fall back to the
direct flavor (`envs=...`) — but never write the key into a persistent file
inside the sandbox.

## Use Cases and Best Practices

- **Isolated development.** Run the coding agent inside the sandbox so its file
  edits and shell commands cannot touch the host.
- **Execute agent-generated code and collect results.** Have the agent write to
  `/workspace`, then read artifacts back via `sandbox.files` or `commands.run`.
- **Checkpoint / resume long tasks.** Use `pause()` + `connect()` to snapshot a
  long refactor and resume later, or fork multiple task variants off one snapshot.
- **Preinstall heavy dependencies** into the template rather than fetching them
  at runtime, especially under a default-deny egress policy.

## Key Code Snippets

### Headless OpenCode invocation

```python
cmd = (
    "opencode run --model anthropic/claude-sonnet-4-6 "
    "--dir /workspace --auto --format json "
    "'Inspect the project, run app.py, and summarize the result.'"
)
result = sandbox.commands.run(cmd, envs=opencode_env, user="root", timeout=900)
```

### Preflight version check

```python
version = sandbox.commands.run("opencode --version", timeout=60)
```

## Caveats

- **Node.js version.** OpenCode needs a recent Node runtime; the base image
  ships an older apt Node, so always install via NodeSource (the Dockerfile does).
- **OpenCode state directories.** `/root/.config/opencode` and
  `/root/.local/share/opencode` hold OpenCode's configuration and session data.
  Keep them empty in the image to avoid leaking sessions across tenants; they are
  created at build time but not populated with any credentials.
- **Direct-flavor key persistence.** With the direct flavor (`envs=`) the key is
  scoped to the exec call, but OpenCode may cache provider credentials under its
  state directories, which survive `pause()` / `resume()`. For strict isolation
  prefer the vault flavor (`network_policy.py`), where the key never enters the
  VM.
- **CubeEgress CA (Node).** For the vault flavor the sandbox must trust the
  CubeEgress root CA, which the base image installs into the system bundle.
  OpenCode runs on Node.js, which ignores the system store, so
  `network_policy.py` also sets `NODE_EXTRA_CA_CERTS` (override via
  `OPENCODE_NODE_CA_BUNDLE`) — without it the vault path fails with
  `Connection error`.
- **Egress side-effects.** Tasks that `npm install` or fetch MCP tools need those
  hosts allowed or preinstalled into the template.
- **Interactive TTY features.** The OpenCode TUI is not available over the
  CubeSandbox protocol. Use headless `opencode run --format json` and drive
  multi-turn conversations from the host script.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `opencode: command not found` in preflight | Template not rebuilt after CLI change | Rebuild the image, re-register the template |
| Provider auth failure | Key not forwarded (direct) or missing inject rule (vault) | Pass `envs={...}` or fix the rule's `sni`/`host` |
| `403 Forbidden - CubeEgress` | Default-deny with no matching allow rule | Add the LLM host (and any extra hosts) to the rules |
| `Connection error` / TLS failure from OpenCode (vault) | OpenCode's Node runtime ignores the system CA store, so it won't trust the CubeEgress CA | The example sets `NODE_EXTRA_CA_CERTS`; override with `OPENCODE_NODE_CA_BUNDLE` if the CA lives elsewhere |
| Template creation stuck in `PULLING` | Registry unreachable from Cube nodes | Push to a registry the cluster can reach; supply auth if needed |
| Readiness probe timeout | Base image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `pause()` / `connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |
| `OPENCODE_MODEL` validation error | Model string missing `provider/model` form | Use the format `anthropic/claude-sonnet-4-6` |

## References

- Runnable example: [`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
- Bring Your Own Image: [`docs/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- Template from image: [`docs/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- Credential vault + egress control: [`docs/guide/security-proxy.md`](../security-proxy.md)
- OpenCode CLI: <https://www.npmjs.com/package/opencode-ai>
