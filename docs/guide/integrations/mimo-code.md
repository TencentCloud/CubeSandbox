---
title: MiMo Code Integration Guide
author: Young-Allen
date: 2026-07-22
tags:
  - integration
  - mimo-code
  - coding-agent
  - agent
lang: en-US
---

# MiMo Code Integration Guide

[中文文档](../../zh/guide/integrations/mimo-code.md)

Run [MiMo Code](https://github.com/XiaomiMiMo/MiMo-Code) inside a
CubeSandbox MicroVM. This guide covers a reproducible template, headless agent
execution, MiMo Platform authentication, restricted egress, and conversation
continuation across a CubeSandbox snapshot.

The runnable implementation is in
[`examples/mimo-code-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/mimo-code-integration).

## Integration Target and Version

| Component | Tested version |
| --- | --- |
| MiMo Code | `@mimo-ai/cli@0.1.7` |
| MiMo model | `mimo/mimo-v2.5-pro` |
| Node.js | 24 (npm installation runtime) |
| CubeSandbox base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| E2B SDK | `e2b>=2.4.1` |
| CubeSandbox SDK | `cubesandbox>=0.3.0` |
| CubeSandbox platform | `>= 0.3.0` for pause/resume; `>= 0.4.0` for CubeEgress |

MiMo Code is derived from OpenCode but adds persistent memory, context
checkpoints, subagent orchestration, and Compose workflows. This integration
uses MiMo's own CLI, unified profile, NDJSON event stream, session IDs, and
Compose mode rather than reproducing a generic OpenCode executor or plugin.

## Prerequisites

- A running CubeSandbox deployment and a reachable CubeAPI.
- `cubemastercli`, Docker, and a node-reachable image registry.
- Python 3.10+ for the host runners.
- A MiMo Platform API key from <https://platform.xiaomimimo.com>.

## Why Run MiMo Code in CubeSandbox

MiMo can edit files, execute shell commands, install dependencies, and spawn
subagents. A MicroVM limits those capabilities to a disposable environment:

| Concern | CubeSandbox mechanism |
| --- | --- |
| Agent command isolation | Dedicated KVM MicroVM and guest kernel |
| Reproducible tools | Version-pinned template |
| Long task continuity | `pause()` snapshots VM memory and rootfs |
| MiMo state continuity | `$MIMOCODE_HOME` and `/workspace` survive reconnect |
| Key isolation | CubeEgress injects the real `api-key` outside the VM |
| Network control | Exact-host allow rule with default-deny egress |

## Integration Steps

### 1. Build the template

```bash
export MIMO_IMAGE="<your-registry>/mimo-code-cube:0.1.7"
./examples/mimo-code-integration/build-template.sh
cubemastercli tpl watch --job-id <job_id>
```

The Dockerfile pins the CLI and verifies the platform binary during the build:

```dockerfile
ARG CUBE_BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16
FROM ${CUBE_BASE_IMAGE}

ARG MIMO_VERSION=0.1.7
RUN npm install -g --no-audit --no-fund \
      "@mimo-ai/cli@${MIMO_VERSION}" \
      --registry https://registry.npmjs.org \
    && mimo --version

ENV MIMOCODE_HOME=/root/.mimocode
WORKDIR /workspace
EXPOSE 49983
```

The full Dockerfile also installs development tools and disables unrelated
MiMo network features.

### 2. Configure the host

```bash
cd examples/mimo-code-integration
install -m 600 .env.example .env
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt
```

Set `E2B_API_URL`, `E2B_API_KEY`, `CUBE_TEMPLATE_ID`, and `MIMO_API_KEY`.
Use HTTPS when a remote CubeAPI requires a real API key; plain HTTP is suitable
only for a trusted local deployment.
The initial integration deliberately targets MiMo Platform only:

- base URL: `https://api.xiaomimimo.com/v1`;
- model: `mimo/mimo-v2.5-pro`;
- authentication header: `api-key`.

Keeping this contract explicit avoids sending credentials to a host inferred
from an untrusted URL. Other OpenAI-compatible providers can be added later as
an explicit mode with their own host and authentication scheme.

### 3. Run a headless task

```bash
python run_mimo_code.py
```

The host invokes:

```bash
mimo run --format json --dir /workspace \
  --model mimo/mimo-v2.5-pro \
  --agent build \
  --dangerously-skip-permissions "<prompt>"
```

`--format json` produces newline-delimited events such as `tool_use`, `text`,
`error`, and `step_finish`. Every event carries a `sessionID`; the example
buffers arbitrary SDK stdout chunks before parsing complete JSON lines.

The direct runner forwards the key only in the command environment. Treat this
as a development flow: a tool with open egress can still exfiltrate a key that
exists in its process environment.

### 4. Keep all MiMo state under one profile

The template uses the absolute path:

```text
/root/.mimocode/
├── config/
├── data/    # session database, auth (if used), memory, checkpoints
├── state/
└── cache/
```

`MIMOCODE_HOME` is a MiMo-specific integration advantage: the profile can be
inspected, retained, or destroyed as one unit. Never pre-populate this
directory with developer sessions or credentials in a shared template.

### 5. Pause and continue the exact conversation

```bash
python resume_mimo_code.py
```

The runner captures the first turn's session ID, pauses the VM, reconnects, and
continues explicitly:

```python
first_result, events = run_turn(
    sandbox,
    workspace=workspace,
    prompt=first_prompt,
    envs=mimo_env,
    timeout=900,
)
session_id = session_id_from_events(events)

sandbox_id = sandbox.sandbox_id
paused = sandbox.pause()
if isinstance(paused, str) and paused:
    sandbox_id = paused
sandbox = Sandbox.connect(sandbox_id=sandbox_id)

second_result, events = run_turn(
    sandbox,
    workspace=workspace,
    prompt=second_prompt,
    envs=mimo_env,
    timeout=900,
    session_id=session_id,
)
```

The actual implementation handles SDK return-type differences and does not use
a `Sandbox.create()` context manager, whose `__exit__` would kill the paused
sandbox.

The test token is deliberately absent from `/workspace`; successful recall
therefore proves MiMo conversation continuity. The runner also verifies the
workspace, profile data, and `mimo session list --format json`.

MiMo checkpoints and CubeSandbox snapshots are complementary:

- MiMo checkpoints reconstruct long model context and persistent memory.
- CubeSandbox snapshots preserve the complete VM, including process memory,
  rootfs, workspace, database, and MiMo profile.

### 6. Apply default-deny egress and credential injection

```bash
python network_policy.py
```

The native CubeSandbox SDK attaches one exact MiMo Platform rule:

```python
Rule(
    name="allow_mimo_platform",
    match=Match(
        scheme="https",
        sni="api.xiaomimimo.com",
        host="api.xiaomimimo.com",
    ),
    action=Action(
        allow=True,
        audit="metadata",
        inject=[
            Inject(
                header="api-key",
                secret=MIMO_API_KEY,
                format="${SECRET}",
            )
        ],
    ),
)
```

The sandbox is created with `allow_internet_access=False`. It sees only a
placeholder `MIMO_API_KEY`; the real value exists in the host-side CubeEgress
rule. MiMo's runtime must trust the interception CA, so the example sets
`NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt`.

Sharing, telemetry, auto-update, model-manifest fetches, LSP downloads, and
external skills/plugins are disabled. These settings reduce auxiliary requests;
the CubeEgress rule remains the enforcement boundary for the allowlist.

### 7. Use MiMo Compose when the task benefits from subagents

```bash
python run_mimo_code.py --agent compose --prompt \
  "Inspect the project, implement the change, test it, and write result.md containing CUBE_MIMO_RUN_OK."
```

Compose is a MiMo primary agent and works in headless mode. Delegation is
model-driven, so production workflows should validate final artifacts and
tests rather than require a fixed subagent trace.

## Use Cases and Best Practices

- **Isolated autonomous development:** give MiMo a disposable repository copy,
  not host filesystem access.
- **Execute and collect:** keep outputs under `/workspace`, then retrieve them
  through `sandbox.files` or a controlled command.
- **Long task checkpointing:** pause after a completed MiMo turn, record both
  sandbox ID and MiMo session ID, then reconnect and resume explicitly.
- **Parallel variants:** fork MiMo sessions or clone CubeSandbox snapshots only
  when each branch has a clear ownership and cleanup policy.
- **Preinstall dependencies:** package required toolchains into the template so
  a narrow egress policy does not need npm, PyPI, or arbitrary download hosts.
- **Treat profile data as sensitive:** memory and session databases can contain
  prompts, code, paths, and command output.

## Caveats

- `--dangerously-skip-permissions` removes interactive approvals. Use it only
  inside a disposable sandbox and retain explicit deny rules when needed.
- The E2B command channel does not expose the MiMo TUI. Use `mimo run`.
- OAuth writes access and refresh tokens to `auth.json`; snapshots would retain
  them. The example therefore uses API-key injection instead.
- Pausing drops outbound connections. A resumed MiMo command opens a new model
  connection while retaining session state.
- `MIMOCODE_HOME` must be absolute.
- Compose delegation and automatic memory consolidation are model-dependent;
  do not use their exact trace as a deterministic health check.

## Troubleshooting

| Symptom | Cause | Resolution |
| --- | --- | --- |
| `mimo: command not found` | Old template | Rebuild and register the pinned image |
| Platform binary cannot execute | Wrong image architecture | Build for the Cube node architecture |
| Authentication failed | Invalid key or Bearer header used | MiMo Platform requires the `api-key` header |
| `403 Forbidden - CubeEgress` | Request host does not match | Use the exact MiMo endpoint and inspect egress audit logs |
| TLS verification failed | Runtime does not trust CubeEgress CA | Set `MIMO_NODE_EXTRA_CA_CERTS` correctly |
| Unexpected models.dev/update errors | Auxiliary network feature enabled | Keep the supplied disable switches |
| Template remains `PULLING` | Registry unavailable | Use a node-reachable registry and pull credentials |
| Probe timeout | Missing Cube entrypoint/envd | Inherit the CubeSandbox base image |
| No session ID | CLI/output changed | Keep MiMo Code pinned and use `--format json` |
| Session missing after resume | Different profile or workspace | Reuse the same absolute paths and sandbox ID |
| Task timeout | Model/tool run exceeded the budget | Increase both exec and sandbox timeouts |

## References

- Runnable example: [`examples/mimo-code-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/mimo-code-integration)
- [MiMo Code repository](https://github.com/XiaomiMiMo/MiMo-Code)
- [MiMo Code models](https://mimo.xiaomi.com/mimocode/models-provider)
- [MiMo Code sessions](https://mimo.xiaomi.com/mimocode/sessions)
- [Bring Your Own Image](../tutorials/bring-your-own-image.md)
- [Snapshot / Clone / Rollback](../snapshot-rollback-clone.md)
- [CubeEgress Security Proxy](../security-proxy.md)
