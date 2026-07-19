---
title: OpenCode Integration Guide
author: zhangzherui
date: 2026-07-11
tags:
  - integration
  - opencode
  - coding-agent
  - agent
lang: en-US
---

# OpenCode Integration Guide

[中文文档](../../zh/guide/integrations/opencode.md)

Run [OpenCode](https://www.npmjs.com/package/opencode-ai)
— a terminal-native AI coding agent — inside CubeSandbox MicroVMs. This guide
covers image build, key injection, egress control, snapshot-based session
persistence, and both headless and server-mode integration, pairing with the
runnable
[`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
project.

## Integration Target and Version

| Component | Version |
|---|---|
| OpenCode coding agent | `opencode-ai` (pinned via `--build-arg OPENCODE_VERSION=x.y.z`) |
| Node.js | 24 (installed via NodeSource) |
| CubeSandbox base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| E2B SDK (host driver) | `e2b` (latest) |
| CubeSandbox platform | `>= 0.3.0` (pause/resume) / `>= 0.4.0` (CubeEgress credential vault) |

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `https://<node>:3000`.
  Use plain HTTP only for isolated local development on loopback.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- An LLM provider API key. Anthropic is the default; OpenAI and Google are
  supported via `OPENCODE_PROVIDER` / `OPENCODE_MODEL`.
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

The image stacks Node.js 24 and the OpenCode CLI on top of `cubesandbox-base`,
so envd is already listening on `:49983`.

The excerpt below highlights the main build steps. Use the checked-in
`Dockerfile` as the canonical source for the exact package list, install flags,
and environment settings.

```dockerfile
# examples/opencode-integration/Dockerfile (excerpt)
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG NODE_MAJOR=24
ARG OPENCODE_VERSION=0.5.0

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl git gnupg jq less procps python3 python3-pip ripgrep \
    && curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g "opencode-ai@${OPENCODE_VERSION}" \
    && opencode --version \
    && rm -rf /root/.npm /var/lib/apt/lists/*

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
# fill in E2B_API_URL, CUBE_TEMPLATE_ID, and your provider key
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | TLS-protected CubeAPI address (`https://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `OPENCODE_PROVIDER` / `OPENCODE_MODEL` | OpenCode CLI flags | Provider and model selection |
| `ANTHROPIC_API_KEY` | `envs=...` (direct) or CubeEgress inject (vault) | Provider key |
| `ANTHROPIC_BASE_URL` | Passed into the exec env | Anthropic-compatible gateways (e.g. DeepSeek) |
| `OPENCODE_LLM_HOST` | `network_policy.py` | LLM host allowed under default-deny egress |

### 4. Runtime Configuration and API Key Injection

OpenCode supports two runtime modes inside the sandbox:

**Headless mode** — `opencode run "prompt"` processes the prompt and exits,
ideal for one-shot tasks. Two key-flow flavors share the same template:

**Direct flavor** — forward the key per command. `e2b`'s `commands.run(envs=...)`
puts the environment into the exec envelope, not into a persistent file inside
the VM, so the key lives only for the lifetime of that command:

```python
result = sandbox.commands.run(
    "cd /workspace && opencode run --provider anthropic "
    "--model claude-sonnet-4-6 'do something'",
    envs={"ANTHROPIC_API_KEY": key},
    user="root",
    timeout=900,
)
```

**Vault flavor** — keep the key out of the VM entirely (see step 6).

**Server mode** — `opencode serve --hostname 0.0.0.0 --port 4096` starts an
HTTP server inside the sandbox. The host driver can then use the `@opencode-ai/sdk`
package to create sessions, send prompts, and retrieve results programmatically.
This mode is useful for multi-turn conversations or orchestrating several agents
from one host process.

```python
# Start the server
sandbox.commands.run(
    "opencode serve --hostname 0.0.0.0 --port 4096 "
    "--provider anthropic --model claude-sonnet-4-6",
    envs={"ANTHROPIC_API_KEY": key},
    user="root",
    background=True,
)

# Then interact via @opencode-ai/sdk from the host (or curl the HTTP API)
```

### 5. Session Persistence (pause / resume)

```bash
python resume_opencode.py
```

This mirrors the [snapshot / clone / rollback](../snapshot-rollback-clone.md)
engine at the SDK layer:

- `sandbox.pause()` snapshots the running VM (memory + rootfs) and frees compute.
- `Sandbox.connect(sandbox_id)` resumes with `/workspace`, OpenCode's state
  directory (`/root/.opencode`), and every other file intact.

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
    assert_state_survived(sandbox)       # /workspace + /root/.opencode intact
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
        name="allow_anthropic_llm",
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
- Every request to the LLM host gets the auth header attached on the wire.
- Anything else is dropped by CubeVS at L3/L4 (`allow_internet_access=False`) and never leaves the sandbox.
- Every allow / deny decision lands in the egress audit log.

For non-Anthropic providers the example injects an `Authorization: Bearer`
header instead. If a provider does not accept a header-injected key, fall back
to the direct flavor (`envs=...`) — but never write the key into a persistent
file inside the sandbox.

## Use Cases and Best Practices

- **Isolated development.** Run the coding agent inside the sandbox so its file
  edits and shell commands cannot touch the host.
- **Execute agent-generated code and collect results.** Have the agent write to
  `/workspace`, then read artifacts back via `sandbox.files` or `commands.run`.
- **Checkpoint / resume long tasks.** Use `pause()` + `connect()` to snapshot a
  long refactor and resume later, or fork multiple task variants off one snapshot.
- **Programmatic control with SDK.** Start OpenCode in server mode
  (`opencode serve`) and use `@opencode-ai/sdk` for fine-grained multi-turn
  orchestration.
- **Preinstall heavy dependencies** into the template rather than fetching them
  at runtime, especially under a default-deny egress policy.

## Key Code Snippets

### Headless OpenCode invocation

```python
cmd = (
    "cd /workspace && opencode run --provider anthropic "
    "--model claude-sonnet-4-6 "
    "'Inspect the project, run app.py, and summarize the result.'"
)
result = sandbox.commands.run(cmd, envs=opencode_env, user="root", timeout=900)
```

### Server mode with SDK

```python
# Start OpenCode server inside the sandbox
sandbox.commands.run(
    "opencode serve --hostname 0.0.0.0 --port 4096 "
    "--provider anthropic --model claude-sonnet-4-6",
    envs=opencode_env,
    user="root",
    background=True,
)
# Interact via @opencode-ai/sdk from the host driver
```

### Preflight version check

```python
version = sandbox.commands.run("opencode --version", timeout=60)
```

## Caveats

- **Node.js version.** OpenCode needs a recent Node runtime; the base image
  ships an older apt Node, so always install via NodeSource (the Dockerfile
  does).
- **Agent state directory.** `/root/.opencode` holds OpenCode's session cache.
  Keep it empty in the image to avoid leaking sessions across tenants; it is
  created at build time but not populated with any credentials.
- **Direct-flavor key persistence.** With the direct flavor (`envs=`) the key
  is scoped to the exec call, but OpenCode may cache provider credentials under
  its state dir (`/root/.opencode/`), which survives `pause()` / `resume()`.
  For strict isolation prefer the vault flavor (`network_policy.py`), where the
  key never enters the VM.
- **CubeEgress CA (Node).** For the vault flavor the sandbox must trust the
  CubeEgress root CA, which CubeMaster bakes as the dedicated
  `/usr/local/share/ca-certificates/cube-egress-root.crt` anchor.
  OpenCode runs on Node.js, which ignores the system store, so
  `network_policy.py` also sets `NODE_EXTRA_CA_CERTS` (override via
  `OPENCODE_NODE_EXTRA_CA_CERTS`) — without it the vault path fails with
  `Connection error`.
- **Egress side-effects.** Tasks that `npm install` or fetch MCP tools need
  those hosts allowed or preinstalled into the template.
- **Interactive TTY features.** The OpenCode interactive session is not
  available over the E2B protocol. Use headless `opencode run` or server mode
  (`opencode serve`) and drive multi-turn conversations from the host script or
  SDK.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `opencode: command not found` in preflight | Template not rebuilt after CLI change | Rebuild the image, re-register the template |
| Provider auth failure | Key not forwarded (direct) or missing inject rule (vault) | Pass `envs={...}` or fix the rule's `sni`/`host` |
| `403 Forbidden - CubeEgress` | Default-deny with no matching allow rule | Add the LLM host (and any extra hosts) to the rules |
| `Connection error` / TLS failure from OpenCode (vault) | OpenCode's Node runtime ignores the system CA store, so it won't trust the CubeEgress CA | The example sets `NODE_EXTRA_CA_CERTS`; override with `OPENCODE_NODE_EXTRA_CA_CERTS` if the CA lives elsewhere |
| Template creation stuck in `PULLING` | Registry unreachable from Cube nodes | Push to a registry the cluster can reach; supply auth if needed |
| Readiness probe timeout | Base image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `pause()` / `connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |

## References

- Runnable example: [`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
- Bring Your Own Image: [`docs/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- Template from image: [`docs/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- Credential vault + egress control: [`docs/guide/security-proxy.md`](../security-proxy.md)
- OpenCode coding agent: <https://www.npmjs.com/package/opencode-ai>
- OpenCode SDK: <https://www.npmjs.com/package/@opencode-ai/sdk>
