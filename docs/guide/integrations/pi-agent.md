---
title: Pi Agent Integration Guide
author: chaojixinren
date: 2026-07-01
tags:
  - integration
  - pi-agent
  - coding-agent
  - agent
lang: en-US
---

# Pi Agent Integration Guide

[中文文档](../../zh/guide/integrations/pi-agent.md)

Run the [Pi coding agent](https://www.npmjs.com/package/@earendil-works/pi-coding-agent)
— a terminal-native AI coding agent — inside CubeSandbox MicroVMs. This guide
covers image build, key injection, egress control, and snapshot-based session
persistence, and pairs with the runnable
[`examples/pi-agent-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/pi-agent-integration)
project.

## Integration Target and Version

| Component | Version |
|---|---|
| Pi coding agent | `@earendil-works/pi-coding-agent` (pinned via `--build-arg PI_VERSION=x.y.z`) |
| Node.js | 24 (installed via NodeSource) |
| CubeSandbox base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| E2B SDK (host driver) | `e2b` (latest) |
| CubeSandbox platform | `>= 0.3.0` (pause/resume) / `>= 0.4.0` (CubeEgress credential vault) |

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- An LLM provider API key. Anthropic is the default; any Anthropic-compatible or
  OpenAI-compatible endpoint works via `ANTHROPIC_BASE_URL` / provider env.
- Python 3.10+ for the host driver scripts.

## Why Run Pi Inside a Sandbox

Pi is a terminal agent that edits files, runs commands, and installs packages.
Running it directly on a workstation blends the agent's blast radius with your
dev environment. Running it inside CubeSandbox gives you:

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

The image stacks Node.js 24 and the Pi CLI on top of `cubesandbox-base`, so
envd is already listening on `:49983`.

```dockerfile
# examples/pi-agent-integration/Dockerfile (excerpt)
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG NODE_MAJOR=24
ARG PI_VERSION=0.80.3

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl git gnupg jq less procps python3 python3-pip ripgrep \
    && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g --ignore-scripts "@earendil-works/pi-coding-agent@${PI_VERSION}" \
    && pi --version \
    && npm cache clean --force \
    && rm -rf /root/.npm /var/lib/apt/lists/*

WORKDIR /workspace
EXPOSE 49983
```

Build and push:

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/pi-agent-cube:latest \
  examples/pi-agent-integration
docker push <your-registry>/pi-agent-cube:latest
```

### 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/pi-agent-cube:latest \
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
cd examples/pi-agent-integration
cp .env.example .env
# fill in E2B_API_URL, CUBE_TEMPLATE_ID, and your provider key
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `PI_PROVIDER` / `PI_MODEL` | Pi CLI flags | Provider and model selection |
| `ANTHROPIC_API_KEY` | `envs=...` (direct) or CubeEgress inject (vault) | Provider key |
| `ANTHROPIC_BASE_URL` | Passed into the exec env | Anthropic-compatible gateways (e.g. DeepSeek) |
| `PI_LLM_HOST` | `network_policy.py` | LLM host allowed under default-deny egress |

### 4. Runtime Configuration and API Key Injection

The Pi command is built headlessly (no TUI) with an explicit provider/model and
`--mode json` so the host can capture a machine-readable result. Two key-flow
flavors share the same template:

**Direct flavor** — forward the key per command. `e2b`'s `commands.run(envs=...)`
puts the environment into the exec envelope, not into a persistent file inside
the VM, so the key lives only for the lifetime of that command:

```python
result = sandbox.commands.run(
    "cd /workspace && pi --mode json --provider anthropic "
    "--model claude-sonnet-4-6 --approve 'do something'",
    envs={"ANTHROPIC_API_KEY": key},
    user="root",
    timeout=900,
)
```

**Vault flavor** — keep the key out of the VM entirely (see step 6).

### 5. Session Persistence (pause / resume)

```bash
python resume_pi_agent.py
```

This mirrors the [snapshot / clone / rollback](../snapshot-rollback-clone.md)
engine at the SDK layer:

- `sandbox.pause()` snapshots the running VM (memory + rootfs) and frees compute.
- `Sandbox.connect(sandbox_id)` resumes with `/workspace`, Pi's state directory
  (`/root/.pi/agent`), and every other file intact.

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
    assert_state_survived(sandbox)       # /workspace + /root/.pi/agent intact
    run_turn(sandbox, prompt_2)          # continues the work
finally:
    sandbox.kill()
```

### 6. Network and Egress Policy (credential vault)

`network_policy.py` demonstrates the recommended pattern for shared clusters:
default-deny egress plus on-the-wire key injection.

```python
rules = [
    {
        "name": "allow_anthropic_llm",
        "match": {"scheme": "https", "sni": "api.anthropic.com", "host": "api.anthropic.com"},
        "action": {
            "allow": True,
            "audit": "metadata",
            "inject": [
                {"header": "x-api-key", "secret": ANTHROPIC_API_KEY, "format": "${SECRET}"},
                {"header": "anthropic-version", "secret": "2023-06-01"},
            ],
        },
    },
]

sandbox = Sandbox.create(
    template=CUBE_TEMPLATE_ID,
    allow_internet_access=True,
    network={"rules": rules},
)
```

Effect:

- `printenv ANTHROPIC_API_KEY` inside the sandbox shows only a placeholder.
- Every request to the LLM host gets the auth header attached on the wire.
- Anything else is default-denied and returned as `403 Forbidden - CubeEgress`.
- Every allow / deny decision lands in the egress audit log.

For non-Anthropic providers the example injects `Authorization: Bearer ${SECRET}`
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

### Headless Pi invocation

```python
cmd = (
    "cd /workspace && pi --mode json "
    "--provider anthropic --model claude-sonnet-4-6 "
    "--approve 'Inspect the project, run app.py, and summarize the result.'"
)
result = sandbox.commands.run(cmd, envs=pi_env, user="root", timeout=900)
```

### Preflight version check

```python
version = sandbox.commands.run("pi --version", timeout=60)
```

## Caveats

- **Node.js version.** Pi needs a recent Node runtime; the base image ships an
  older apt Node, so always install via NodeSource (the Dockerfile does).
- **Agent state directory.** `/root/.pi/agent` holds Pi's session cache. Keep it
  empty in the image to avoid leaking sessions across tenants; it is created at
  build time but not populated with any credentials.
- **CubeEgress CA.** For the vault flavor to work, the sandbox must trust the
  CubeEgress root CA. This is on by default for templates built via
  `cubemastercli tpl create-from-image`.
- **Egress side-effects.** Tasks that `npm install` or fetch MCP tools need those
  hosts allowed or preinstalled into the template.
- **Interactive TTY features.** The Pi TUI is not available over the E2B
  protocol. Use headless `--mode json` and drive multi-turn conversations from
  the host script.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `pi: command not found` in preflight | Template not rebuilt after CLI change | Rebuild the image, re-register the template |
| Provider auth failure | Key not forwarded (direct) or missing inject rule (vault) | Pass `envs={...}` or fix the rule's `sni`/`host` |
| `403 Forbidden - CubeEgress` | Default-deny with no matching allow rule | Add the LLM host (and any extra hosts) to the rules |
| SSL handshake failure to the LLM host | Sandbox doesn't trust the CubeEgress CA | Rebuild with the Cube CA enabled, or set `SSL_CERT_FILE` |
| Template creation stuck in `PULLING` | Registry unreachable from Cube nodes | Push to a registry the cluster can reach; supply auth if needed |
| Readiness probe timeout | Base image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `pause()` / `connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |

## References

- Runnable example: [`examples/pi-agent-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/pi-agent-integration)
- Bring Your Own Image: [`docs/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- Template from image: [`docs/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- Credential vault + egress control: [`docs/guide/security-proxy.md`](../security-proxy.md)
- Pi coding agent: <https://www.npmjs.com/package/@earendil-works/pi-coding-agent>
