---
title: CodeBuddy Code Integration Guide
author: pei-pei45
date: 2026-07-16
tags:
  - integration
  - codebuddy
  - coding-agent
  - agent
lang: en-US
---

# CodeBuddy Code Integration Guide

[中文文档](../../zh/guide/integrations/codebuddy.md)

Run the [Tencent CodeBuddy Code CLI](https://www.codebuddy.ai/docs/cli/README)
— a terminal-native AI coding agent — inside CubeSandbox MicroVMs. This guide
covers image build, key injection, egress control, and snapshot-based session
persistence, and pairs with the runnable
[`examples/codebuddy-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/codebuddy-integration)
project.

## Integration Target and Version

| Component | Version |
|---|---|
| CodeBuddy Code | `@tencent-ai/codebuddy-code` (pinned via `--build-arg CODEBUDDY_VERSION=x.y.z`) |
| Node.js | 20 (installed via NodeSource) |
| CubeSandbox base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| E2B SDK (host driver) | `e2b` (latest) |
| CubeSandbox platform | `>= 0.3.0` (pause/resume) / `>= 0.4.0` (CubeEgress credential vault) |

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- A CodeBuddy Code account, or a custom upstream API key. CodeBuddy Code can be
  pointed at the international CodeBuddy platform (`CODEBUDDY_INTERNET_ENVIRONMENT=io`),
  the China platform (`internal`), the iOA enterprise platform (`ioa`), or any
  Anthropic- / OpenAI-compatible endpoint via `CODEBUDDY_BASE_URL` /
  `ANTHROPIC_BASE_URL`.
- Python 3.10+ for the host driver scripts.

## Why Run CodeBuddy Inside a Sandbox

CodeBuddy Code is a terminal agent that edits files, runs commands, and installs
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

The image stacks Node.js 20 and the CodeBuddy CLI on top of `cubesandbox-base`,
so envd is already listening on `:49983`.

```dockerfile
# examples/codebuddy-integration/Dockerfile (excerpt)
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG NODE_MAJOR=20
ARG CODEBUDDY_VERSION=2.117.1

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
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g --omit=dev --ignore-scripts \
        "@tencent-ai/codebuddy-code@${CODEBUDDY_VERSION}" \
    && codebuddy --version \
    && npm cache clean --force \
    && rm -rf /root/.npm

# CodeBuddy runs as an unprivileged user. UID/GID are auto-assigned because the
# base image already owns uid=1000 as the ``user`` exec account; pause/resume
# snapshots preserve identity by username, not by numeric id. The MicroVM
# provides the outer isolation; this user drop is defense-in-depth for
# prompt-injection scenarios where the LLM agent is tricked into shell commands.
#
# /workspace is world-writable because the e2b exec channel constrains us to
# the SDK's allowed-users list (``root``, ``user``) and ``user`` cannot write
# to a codebuddy-owned directory. The CodeBuddy state dir is rooted under
# /workspace/.codebuddy so the same permission model applies and pause/resume
# snapshots capture it alongside the project tree.
RUN groupadd --system codebuddy \
    && useradd  --system --gid codebuddy \
                --home-dir /home/codebuddy --shell /bin/bash \
                --no-create-home codebuddy \
    && install -d -o codebuddy -g codebuddy -m 0700 /home/codebuddy \
    && install -d -o codebuddy -g codebuddy -m 0777 /workspace

ENV CODEBUDDY_CONFIG_DIR=/workspace/.codebuddy \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    DISABLE_AUTOUPDATER=1 \
    DISABLE_FEEDBACK_COMMAND=1 \
    CODEBUDDY_INTERNET_ENVIRONMENT=io

WORKDIR /workspace
USER codebuddy
```

Build and push (from the repository root, so the relative build context
`examples/codebuddy-integration` resolves correctly):

```bash
docker build --pull --platform linux/amd64 \
  -t <your-registry>/codebuddy-cube:latest \
  examples/codebuddy-integration
docker push <your-registry>/codebuddy-cube:latest
```

### 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/codebuddy-cube:latest \
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
cd examples/codebuddy-integration
cp .env.example .env
# fill in E2B_API_URL, CUBE_TEMPLATE_ID, CODEBUDDY_INTERNET_ENVIRONMENT, and your provider key
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |
| `CODEBUDDY_INTERNET_ENVIRONMENT` | CodeBuddy CLI | `io` (default, international), `internal` (China), `ioa` (Tencent enterprise) |
| `CODEBUDDY_MODEL` / `CODEBUDDY_BASE_URL` | CodeBuddy CLI flags | Model id and optional custom upstream endpoint |
| `CODEBUDDY_API_KEY` / `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / ... | `envs=...` (direct) or CubeEgress inject (vault) | Provider key |
| `CODEBUDDY_LLM_HOST` | `network_policy.py` | LLM host allowed under default-deny egress |

### 4. Runtime Configuration and API Key Injection

CodeBuddy is built headlessly with `-p` (process the prompt and exit, no TUI)
and `-y` / `--dangerously-skip-permissions` (required for any non-interactive
run that touches files or runs commands — without it the CLI blocks on a
permission prompt that cannot be answered over the exec channel). The prompt is
the trailing positional argument. Two key-flow flavors share the same template:

**Direct flavor** — forward the key per command. `e2b`'s `commands.run(envs=...)`
puts the environment into the exec envelope, not into a persistent file inside
the VM, so the key lives only for the lifetime of that command:

```python
result = sandbox.commands.run(
    "cd /workspace && codebuddy -p -y --model claude-sonnet-4-6 'do something'",
    envs={"ANTHROPIC_API_KEY": key},
    user="codebuddy",
    timeout=900,
)
```

**Vault flavor** — keep the key out of the VM entirely (see step 6).

### 5. Session Persistence (pause / resume)

```bash
cd examples/codebuddy-integration
python resume_codebuddy.py
```

This mirrors the [snapshot / clone / rollback](../snapshot-rollback-clone.md)
engine at the SDK layer:

- `sandbox.pause()` snapshots the running VM (memory + rootfs) and frees compute.
- `Sandbox.connect(sandbox_id)` resumes with `/workspace`, CodeBuddy's state
  directory (`/workspace/.codebuddy`), and every other file intact. Turn 2 then
  calls `codebuddy -p -y -c` to continue the most recent session CodeBuddy
  recorded under `$CODEBUDDY_CONFIG_DIR/projects/`.

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
    assert_state_survived(sandbox)       # /workspace + /workspace/.codebuddy intact
    run_turn(sandbox, prompt_2, continue_session=True)
finally:
    sandbox.kill()
```

### 6. Network and Egress Policy (credential vault)

Run `network_policy.py` from `examples/codebuddy-integration/` (after step 3):

```bash
cd examples/codebuddy-integration
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
- **Switch LLM providers without rebuilding the image.** CodeBuddy itself keys
  off `CODEBUDDY_INTERNET_ENVIRONMENT` + `CODEBUDDY_API_KEY`; override the
  upstream via `CODEBUDDY_BASE_URL` to point at Anthropic / OpenAI / DeepSeek /
  Gemini while keeping the same template.
- **Preinstall heavy dependencies** into the template rather than fetching them
  at runtime, especially under a default-deny egress policy.

## Key Code Snippets

### Headless CodeBuddy invocation

```python
cmd = (
    "cd /workspace && codebuddy -p -y --model claude-sonnet-4-6 "
    "'Inspect the project, run app.py, and summarize the result.'"
)
result = sandbox.commands.run(cmd, envs=codebuddy_env, user="codebuddy", timeout=900)
```

### Preflight version check

```python
version = sandbox.commands.run("codebuddy --version", timeout=60)
```

## Caveats

- **Node.js version.** CodeBuddy requires Node 18.20+; the base image ships an
  older apt Node, so always install via NodeSource (the Dockerfile does).
- **Non-interactive mode needs a pre-set key.** `codebuddy -p` never falls back
  to a browser login flow — the run will block on the auth popup if you forget
  to set `CODEBUDDY_API_KEY` (or the matching provider env). `run_codebuddy.py`
  raises before booting the sandbox if none is set.
- **Permission mode.** `-y` skips every tool-call prompt; required because
  permission prompts cannot be answered over the non-interactive exec channel.
  In higher-security workflows tighten the allow-list via `settings.json`
  (`permissions.defaultMode`, `permissions.allow`, ...) rather than passing `-y`.
- **Agent state directory.** `/workspace/.codebuddy` holds CodeBuddy's
  session cache (config, history, sessions, plans, file-history). Keep it empty
  in the image to avoid leaking sessions across tenants; the Dockerfile creates
  it but does not populate it with any credentials.
- **Direct-flavor key persistence.** With the direct flavor (`envs=`) the key
  is scoped to the exec call, but CodeBuddy may cache provider credentials
  under its state dir (`/workspace/.codebuddy/`), which survives `pause()` /
  `resume()`. For strict isolation prefer the vault flavor (`network_policy.py`),
  where the key never enters the VM.
- **CubeEgress CA (Node).** For the vault flavor the sandbox must trust the
  CubeEgress root CA, which the base image installs into the system bundle.
  CodeBuddy ships as a Node.js bundle that ignores the system store, so
  `network_policy.py` also sets `NODE_EXTRA_CA_CERTS` (override via
  `CODEBUDDY_NODE_EXTRA_CA_CERTS`) — without it the vault path fails with
  "unable to verify the first certificate".
- **Egress side-effects.** Tasks that `npm install` or fetch MCP tools need
  those hosts allowed or preinstalled into the template.
- **Interactive TTY features.** The CodeBuddy TUI is not available over the
  E2B protocol. Use headless `-p -y` and drive multi-turn conversations from
  the host script (`-c` / `--resume` for continuation, `--session-id` to pin).

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `codebuddy: command not found` in preflight | Template not rebuilt after CLI change | Rebuild the image, re-register the template |
| Browser login popup blocks the run | `-p` mode requires a pre-set API key, never falls back to interactive login | Set `CODEBUDDY_API_KEY` (or the matching provider env) before launching |
| Permission prompt hangs the run | Forgot `-y` / `--dangerously-skip-permissions` on a run that touches files or commands | Add `-y`, or tighten `settings.json` permissions for non-`y` runs |
| Provider auth failure | Key not forwarded (direct) or missing inject rule (vault) | Pass `envs={...}` or fix the rule's `sni`/`host` |
| `403 Forbidden - CubeEgress` | Default-deny with no matching allow rule | Add the LLM host (and any extra hosts) to the rules |
| `Connection error` / TLS failure from CodeBuddy (vault) | CodeBuddy's Node runtime ignores the system CA store, so it won't trust the CubeEgress CA | The example sets `NODE_EXTRA_CA_CERTS`; override with `CODEBUDDY_NODE_EXTRA_CA_CERTS` if the CA lives elsewhere |
| Template creation stuck in `PULLING` | Registry unreachable from Cube nodes | Push to a registry the cluster can reach; supply auth if needed |
| Readiness probe timeout | Base image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| `pause()` / `connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |

## References

- Runnable example: [`examples/codebuddy-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/codebuddy-integration)
- Bring Your Own Image: [`docs/guide/tutorials/bring-your-own-image.md`](../tutorials/bring-your-own-image.md)
- Template from image: [`docs/guide/tutorials/template-from-image.md`](../tutorials/template-from-image.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../snapshot-rollback-clone.md)
- Credential vault + egress control: [`docs/guide/security-proxy.md`](../security-proxy.md)
- CodeBuddy Code CLI: <https://www.codebuddy.ai/docs/cli/README>
- CodeBuddy Code installation: <https://www.codebuddy.ai/docs/cli/installation>
- CodeBuddy Code environment variables: <https://www.codebuddy.ai/docs/cli/env-vars>
- CodeBuddy Code directory layout (`~/.codebuddy`): <https://www.codebuddy.ai/docs/cli/codebuddy-dir>
