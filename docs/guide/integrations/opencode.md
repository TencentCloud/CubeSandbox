---
title: OpenCode Integration Guide
author: pei-pei45
date: 2026-07-18
tags:
  - integration
  - opencode
  - coding-agent
  - agent
lang: en-US
---

# OpenCode Integration Guide

[中文文档](../../zh/guide/integrations/opencode.md)

Run the [OpenCode CLI](https://opencode.ai/) — an open-source, terminal-native
AI coding agent — inside CubeSandbox MicroVMs. This guide covers image build,
key injection, egress control, and snapshot-based session persistence, and pairs
with the runnable
[`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
project.

## Integration Target and Version

| Component | Version |
|---|---|
| OpenCode | `opencode-ai@1.17.20` (pinned via `--build-arg OPENCODE_VERSION=x.y.z`; relies on the `--dangerously-skip-permissions` flag that landed in 1.17.x) |
| Node.js | 20 (installed via NodeSource) |
| CubeSandbox base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| E2B SDK (host driver) | `e2b` (latest) |
| CubeSandbox platform | `>= 0.3.0` (pause/resume) / `>= 0.4.0` (CubeEgress credential vault) |

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- An OpenCode-compatible LLM provider key. OpenCode ships built-in presets for
  Anthropic, OpenAI, Google Gemini, Azure, AWS Bedrock, DeepSeek, Groq, Mistral,
  OpenRouter, and more; any provider preset works. Custom upstreams are
  reachable via `OPENCODE_BASE_URL` / `ANTHROPIC_BASE_URL` /
  `OPENAI_BASE_URL`.
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
| **Open ecosystem** | OpenCode's MCP support maps cleanly onto CubeSandbox's network policy |

## Integration Steps

### 1. Build the template image

The image stacks Node.js 20 and the OpenCode CLI on top of `cubesandbox-base`,
so envd is already listening on `:49983`.

```dockerfile
# examples/opencode-integration/Dockerfile (excerpt)
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG NODE_MAJOR=20
ARG OPENCODE_VERSION=1.17.20

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl git gnupg jq less procps python3 python3-pip ripgrep \
    && mkdir -p /etc/apt/keyrings \
    && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
        | gpg --dearmor --yes -o /etc/apt/keyrings/nodesource.gpg \
    && gpg --show-keys /etc/apt/keyrings/nodesource.gpg 2>/dev/null \
        | grep -q "6F71F525282841EEDAF851B42F59B5F99B1BE0B4" \
        || (echo "ERROR: NodeSource GPG fingerprint mismatch" && exit 1) \
    && echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
        > /etc/apt/sources.list.d/nodesource.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends nodejs \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g --omit=dev \
        "opencode-ai@${OPENCODE_VERSION}" \
    && opencode --version \
    && rm -rf /root/.npm

# OpenCode runs as an unprivileged user. UID/GID are auto-assigned because the
# base image already owns uid=1000 as the ``user`` exec account; pause/resume
# snapshots preserve identity by username, not by numeric id. The MicroVM
# provides the outer isolation; this user drop is defense-in-depth for
# prompt-injection scenarios where the LLM agent is tricked into shell commands.
#
# /workspace is world-writable because the e2b exec channel constrains us to
# the SDK's allowed-users list (``root``, ``user``) and ``user`` cannot write
# to an opencode-owned directory. OpenCode splits its on-disk state between
# ``$OPENCODE_CONFIG_DIR`` (config + agents) and ``$OPENCODE_DATA_DIR``
# (sessions, auth.json, storage); both are rooted under ``/workspace/.opencode``
# so pause/resume snapshots capture them alongside the project tree.
ARG OPENCODE_CONFIG_DIR=/workspace/.opencode/config
ARG OPENCODE_DATA_DIR=/workspace/.opencode/data

RUN groupadd --system opencode \
    && useradd  --system --gid opencode \
                --home-dir /home/opencode --shell /bin/bash \
                --no-create-home opencode \
    && install -d -o opencode -g opencode -m 0700 /home/opencode \
    && install -d -o opencode -g opencode -m 0777 /workspace

ENV OPENCODE_CONFIG_DIR=${OPENCODE_CONFIG_DIR} \
    OPENCODE_DATA_DIR=${OPENCODE_DATA_DIR} \
    XDG_CONFIG_HOME=/workspace \
    XDG_DATA_HOME=/workspace \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    OPENCODE_DISABLE_AUTOUPDATE=1

RUN install -d -o opencode -g opencode -m 0777 "${OPENCODE_CONFIG_DIR}" \
    && install -d -o opencode -g opencode -m 0777 "${OPENCODE_DATA_DIR}/storage" \
    && printf '{}\n' > "${OPENCODE_CONFIG_DIR}/opencode.json"

WORKDIR /workspace
USER opencode
```

Build and push (from the repository root, so the relative build context
`examples/opencode-integration` resolves correctly):

```bash
docker build --pull --platform linux/amd64 \
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
| `E2B_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `OPENCODE_PROVIDER` | `env_utils.provider()` | Optional — inferred from the active key when unset |
| `OPENCODE_MODEL` / `OPENCODE_BASE_URL` | OpenCode CLI flags | Model id and optional custom upstream endpoint |
| `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `GOOGLE_API_KEY` / ... | `envs=...` (direct) or CubeEgress inject (vault) | Provider key |
| `OPENCODE_LLM_HOST` | `network_policy.py` | LLM host allowed under default-deny egress |

### 4. Runtime Configuration and API Key Injection

OpenCode is invoked headlessly with `opencode run "..."` (process the prompt
and exit, no TUI). Tool-call permission is auto-approved for the lifetime of
the run via the `--dangerously-skip-permissions` flag (added in OpenCode
1.17.x; `--yolo` is an alias) — required because the exec channel cannot
answer permission prompts. The prompt is the trailing positional argument.
Two key-flow flavors share the same template:

**Direct flavor** — forward the key per command. `e2b`'s `commands.run(envs=...)`
puts the environment into the exec envelope, not into a persistent file inside
the VM, so the key lives only for the lifetime of that command. The exec
channel only accepts the usernames `root` and `user` (the same `user` that
`_opencode_common.run_command` defaults to); passing `user="opencode"`
raises `invalid username: 'opencode'`. Inside the image the agent still runs
unprivileged because the Dockerfile drops to `USER opencode`; the SDK-level
`user` argument only constrains the exec channel identity, not the container's
process identity:

```python
result = sandbox.commands.run(
    "cd /workspace && opencode run --dangerously-skip-permissions -m claude-sonnet-4-6 "
    "'Inspect the project, run app.py, and summarize the result.'",
    envs={"ANTHROPIC_API_KEY": key},
    user="user",
    timeout=900,
)
```

**Vault flavor** — keep the key out of the VM entirely (see step 6).

### 5. Session Persistence (pause / resume)

```bash
cd examples/opencode-integration
python resume_opencode.py
```

This mirrors the [snapshot / clone / rollback](../snapshot-rollback-clone.md)
engine at the SDK layer:

- `sandbox.pause()` snapshots the running VM (memory + rootfs) and frees compute.
- `Sandbox.connect(sandbox_id)` resumes with `/workspace`, OpenCode's config
  directory (`/workspace/.opencode/config/`), data directory
  (`/workspace/.opencode/data/`), and every other file intact. Turn 2 then
  calls `opencode run --dangerously-skip-permissions -c` to continue the
  most recent session OpenCode recorded under `$OPENCODE_DATA_DIR/storage/`.

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
    assert_state_survived(sandbox)       # /workspace + /workspace/.opencode/{config,data}/ intact
    run_turn(sandbox, prompt_2, continue_session=True)
finally:
    sandbox.kill()
```

### 6. Network and Egress Policy (credential vault)

Run `network_policy.py` from `examples/opencode-integration/` (after step 3):

```bash
cd examples/opencode-integration
python network_policy.py
```

The script demonstrates the recommended pattern for shared clusters:
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
- **Switch LLM providers without rebuilding the image.** OpenCode itself keys
  off the upstream env var (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, ...); override
  the upstream via `OPENCODE_BASE_URL` to point at any Anthropic- / OpenAI-
  compatible endpoint while keeping the same template.
- **Preinstall heavy dependencies** into the template rather than fetching them
  at runtime, especially under a default-deny egress policy.
- **Pair OpenCode with MCP servers.** OpenCode ships first-class MCP support;
  run an MCP server inside the same template and let OpenCode call it for
  filesystem, browser, or database access — the egress policy still applies.

## Key Code Snippets

### Headless OpenCode invocation

```python
cmd = (
    "cd /workspace && opencode run --dangerously-skip-permissions -m claude-sonnet-4-6 "
    "'Inspect the project, run app.py, and summarize the result.'"
)
result = sandbox.commands.run(cmd, envs=opencode_env, timeout=900)
```

### Preflight version check

```python
version = sandbox.commands.run("opencode --version", timeout=60)
```

## Caveats

- **Node.js version.** OpenCode requires Node 18.20+; the base image ships an
  older apt Node, so always install via NodeSource (the Dockerfile does).
- **Non-interactive mode needs `--dangerously-skip-permissions`.** `opencode run`
  prompts for tool-call permission by default; in non-interactive mode the
  prompt cannot be answered. `run_opencode.py` passes the flag for the
  duration of the run; in higher-security workflows tighten the allow-list
  via `opencode.json` instead. (Older OpenCode releases < 1.17 take
  `OPENCODE_PERMISSION='{"*":"allow"}'` in the env instead.)
- **Agent state directories.** OpenCode splits state between
  `/workspace/.opencode/config/` (config) and
  `/workspace/.opencode/data/` (sessions, auth.json, storage). The
  Dockerfile creates both but populates them with no credentials.
- **Direct-flavor key persistence.** With the direct flavor (`envs=`) the
  key is scoped to the exec call, but OpenCode caches provider credentials
  under its data dir (`/workspace/.opencode/data/auth.json`), which
  survives `pause()` / `resume()`. For strict isolation prefer the vault
  flavor (`network_policy.py`), where the key never enters the VM.
- **CubeEgress CA (Node).** For the vault flavor the sandbox must trust the
  CubeEgress root CA, which the base image installs into the system bundle.
  OpenCode ships as a Node.js bundle that ignores the system store, so
  `network_policy.py` also sets `NODE_EXTRA_CA_CERTS` (override via
  `OPENCODE_NODE_EXTRA_CA_CERTS`) — without it the vault path fails with
  "unable to verify the first certificate".
- **Egress side-effects.** Tasks that `npm install` or fetch MCP tools need
  those hosts allowed or preinstalled into the template.
- **Interactive TTY features.** The OpenCode TUI is not available over the
  E2B protocol. Use headless `opencode run` and drive multi-turn conversations
  from the host script (`-c` / `-s <id>` for continuation, `--session-id` to
  pin).
- **Provider resolution.** `env_utils.provider()` infers the active provider
  from whichever key env var is set. For custom gateways, set
  `OPENCODE_PROVIDER` explicitly to bypass the substring heuristic that maps
  `*_BASE_URL` to a provider.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `opencode: command not found` in preflight | Template not rebuilt after CLI change | Rebuild the image, re-register the template |
| Permission prompt hangs the run | `--dangerously-skip-permissions` was not passed for a run that touches files or commands | Add the flag (or tighten `opencode.json` permissions) |
| `unknown flag: --dangerously-skip-permissions` | OpenCode older than 1.17 | Rebuild the image with `--build-arg OPENCODE_VERSION=1.17.20`, or run with `OPENCODE_PERMISSION='{"*":"allow"}'` instead |
| `model not found` from OpenCode | `OPENCODE_MODEL` does not match the active provider | Set `OPENCODE_MODEL` explicitly, or use `provider/model` shorthand via `-m anthropic/claude-sonnet-4-6` |
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
- OpenCode CLI: <https://opencode.ai/>
- OpenCode docs: <https://opencode.ai/docs/>
- OpenCode repository: <https://github.com/anomalyco/opencode>
