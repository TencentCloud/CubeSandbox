---
title: OpenCode Integration Guide
author: blues-kun
date: 2026-07-29
tags:
  - integration
  - opencode
  - coding-agent
  - agent
lang: en-US
---

# OpenCode Integration Guide

[中文文档](../../zh/guide/integrations/opencode.md)

Run the OpenCode terminal coding agent inside a CubeSandbox MicroVM. This guide
covers a reproducible template, headless execution, sensitive configuration,
default-deny egress, and pause/resume of both the workspace and OpenCode session.
It pairs with the runnable
[`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
project.

## Integration Target and Version

| Component | Tested version |
|---|---|
| OpenCode | `1.18.9` |
| CubeSandbox base | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` |
| CubeSandbox platform | `>= 0.3.0` for pause/resume; `>= 0.4.0` for CubeEgress vault |
| Host SDK | `e2b 2.35.0`, `cubesandbox 0.6.0` |
| Example LLM | Tencent TokenHub Hy3 over OpenAI-compatible Chat Completions |

OpenCode 2 is currently beta and uses a different configuration format. The
example pins stable OpenCode 1, where custom providers live under the singular
`provider` key and use `npm` plus `options.baseURL`.

## Why Put the Coding Agent in CubeSandbox

OpenCode can read and edit files, execute shell commands, and invoke nested
tools. CubeSandbox places these actions in a dedicated KVM MicroVM:

| Requirement | CubeSandbox control |
|---|---|
| Host isolation | Separate guest kernel and writable layer |
| Reproducibility | Versioned template image and pinned agent binary |
| Long tasks | VM/rootfs snapshot through `pause()`, then `connect()` |
| Secret minimization | Per-process env injection or CubeEgress credential vault |
| Network control | Default-deny egress with host-level allow and audit |

The OpenCode permission file improves ergonomics but is not the security
boundary. Shell commands can have many equivalent forms; the MicroVM and egress
policy remain the containment controls.

## Prerequisites

- CubeSandbox is deployed and CubeAPI is reachable at `http://<node>:3000`.
- `cubemastercli` is connected to the cluster.
- Docker can push to a registry reachable by every Cube node.
- Python 3.10+ is available on the host.
- An OpenAI-compatible model key and base URL. The example defaults to TokenHub
  Hy3 (`HY3_MODEL=hy3`).

## Architecture

```text
Host driver
  |  create(template)
  v
CubeSandbox MicroVM
  |-- /workspace                      project and output
  |-- /root/.local/share/opencode     sessions and local state
  |-- opencode run --format json      headless event stream
  |
  +--> CubeEgress --> allowed LLM host
          |
          +-- inject Authorization header
          +-- audit metadata
```

## Integration Steps

### 1. Build the template

The Dockerfile uses the official Cube base image so envd remains available on
port `49983`. It reuses the base image's `bash`, CA bundle, and `curl`, while
installing only additional coding tools. The OpenCode x86-64 release tarball
and SHA256 are both pinned; a non-`amd64` build fails early rather than
installing an incompatible binary. A `.dockerignore` allowlist sends only the
Dockerfile and V1 configuration, preventing `.env`, virtual environments,
tests, and caches from entering the build context.

```bash
IMAGE=<your-registry>/opencode-cube:1.18.9 PUSH=1 \
  ./examples/opencode-integration/build-template.sh
```

The image disables auto-update, session sharing, models.dev refreshes, external
plugins, and LSP downloads. This removes hidden runtime destinations and makes a
default-deny allowlist practical.

### 2. Register the image as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/opencode-cube:1.18.9 \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

Use `8G+` for repositories that install large compilers or dependency caches.
Record the `template_id` when the job reaches `READY`.

### 3. Configure OpenCode

The source file is named `opencode.v1.json` to bind it explicitly to OpenCode
1.x. The Dockerfile copies it to OpenCode's standard runtime path,
`/root/.config/opencode/opencode.json`:

```json
{
  "model": "tokenhub/hy3",
  "autoupdate": false,
  "share": "disabled",
  "provider": {
    "tokenhub": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "{env:HY3_BASE_URL}",
        "apiKey": "{env:HY3_API_KEY}"
      },
      "models": {
        "hy3": {
          "name": "Hy3"
        }
      }
    }
  }
}
```

The image stores variable references only. The host `.env` is ignored:

```dotenv
E2B_API_URL=http://<cube-host>:3000
E2B_API_KEY=e2b_000000
CUBE_TEMPLATE_ID=<template-id>
HY3_API_KEY=<your-tokenhub-key>
HY3_BASE_URL=https://tokenhub.tencentmaas.com/v1
HY3_MODEL=hy3
```

### 4. Run a headless repair task

```bash
cd examples/opencode-integration
python -m venv .venv
.venv/bin/pip install -r requirements.txt
.venv/bin/python run_opencode.py
```

The driver seeds a function with one known defect and a two-case unittest. Hy3
drives OpenCode to run the failing test, inspect files, edit only the
implementation, and rerun it. The host then independently asserts:

- `tests/test_stats.py` did not change;
- the target unittest passes;
- `git diff --check` passes;
- `result.md` exists and is non-empty.

OpenCode runs as:

```bash
opencode run \
  --pure \
  --auto \
  --format json \
  --model tokenhub/hy3 \
  "<task>"
```

`--pure` suppresses external plugins. `--auto` is suitable here because the
whole task is already inside a disposable MicroVM, but the shipped config still
denies common push, privilege, external-directory, and network tools. It also
denies direct `rm`, `/bin/rm`, `/usr/bin/rm`, and `command rm` invocations.
Wrappers and equivalent tools can still bypass command-pattern guardrails, so
they do not replace sandbox isolation.

The concise JSONL renderer shows model text, tools, and errors. Pass
`--verbose-events` to identify other event types that were omitted, or `--raw`
to preserve every event unchanged.

## Key Injection

### Direct mode

`run_opencode.py` sends the key only in the exec envelope:

```python
result = sandbox.commands.run(
    command,
    envs={
        "HY3_API_KEY": tokenhub_key,
        "HY3_BASE_URL": "https://tokenhub.tencentmaas.com/v1",
        "HY3_MODEL": "hy3",
    },
)
```

This avoids a credential file inside the image. However, OpenCode and any child
process can read that environment, and open egress permits exfiltration. Use it
for local evaluation, not multi-tenant production. The driver prints this
warning to stderr before it creates a sandbox.

### CubeEgress vault mode

`network_policy.py` keeps the real key on the host and attaches it on the wire:

```python
rules = [
    Rule(
        name="allow_tokenhub_hy3",
        match=Match(
            scheme="https",
            sni="tokenhub.tencentmaas.com",
            host="tokenhub.tencentmaas.com",
        ),
        action=Action(
            allow=True,
            audit="metadata",
            inject=[
                Inject(
                    header="Authorization",
                    secret=tokenhub_key,
                    format="Bearer ${SECRET}",
                )
            ],
        ),
    )
]

sandbox = Sandbox.create(
    template=template_id,
    allow_internet_access=False,
    network={"rules": rules},
)
```

The script first verifies that the sandbox-wide environment has no
`HY3_API_KEY`, then gives only the OpenCode child process
`HY3_API_KEY=cube-egress-managed-placeholder`. OpenCode creates a placeholder
Authorization header, which CubeEgress replaces for the matched host. Other
destinations are denied and audited.

The standalone runtime must trust the CubeEgress interception CA. The example
sets both `SSL_CERT_FILE` and `NODE_EXTRA_CA_CERTS`; override
`OPENCODE_CA_BUNDLE` when the CA bundle has a different path.

## Session Persistence

```bash
.venv/bin/python resume_opencode.py
```

The script:

1. completes turn 1 and extracts the authoritative `sessionID` from JSONL;
2. calls `sandbox.pause()` after the agent process exits;
3. reconnects with `Sandbox.connect(sandbox_id=...)`;
4. verifies `/workspace/plan.md` and OpenCode's state directory;
5. calls `opencode run --session <id>` for turn 2;
6. destroys the sandbox in `finally`.

Do not wrap this flow in `with Sandbox.create(...)`: leaving the context kills
the sandbox and defeats pause/resume.

This driver uses direct per-process key injection and therefore prints the same
open-egress warning as the repair example. It demonstrates persistence; combine
the pattern with vault egress controls before production use.

## Use Cases and Best Practices

- **Isolated repository repair.** Clone or upload a repository into
  `/workspace`, run OpenCode, and export only reviewed patches and test logs.
- **Parallel candidates.** Snapshot a prepared workspace, then clone multiple
  sandboxes for alternative fixes or models.
- **Long refactors.** Pause between milestones and resume the exact agent
  session instead of reconstructing context from prose.
- **Untrusted generated code.** Keep execution inside the MicroVM, set CPU/time
  limits, and collect deterministic test artifacts.
- **Preinstall dependencies.** Default-deny sessions should not need package
  registries; bake common toolchains into a derived template.
- **Use acceptance checks.** Treat tests and output contracts as truth, not the
  agent's final statement.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `opencode: command not found` | Template predates the integration | Rebuild and re-register |
| `Model not found: tokenhub/hy3` | OpenCode V2 config used with V1 | Pin `1.18.9` and singular `provider` |
| `Unsupported TARGETARCH=...` | Non-amd64 build uses the x64 digest | Build the documented `linux/amd64` image |
| Provider returns `401` | Key absent or vault inject mismatch | Check host `.env` or Authorization rule |
| Provider returns `404` | Base URL lacks or duplicates `/v1` | Use the provider root ending once in `/v1` |
| `403 Forbidden - CubeEgress` | LLM hostname does not match | Derive SNI/Host from `HY3_BASE_URL` |
| TLS failure in vault mode | Runtime does not trust CubeEgress CA | Set `OPENCODE_CA_BUNDLE` |
| Unexpected external request | Updates/models/plugins still active | Keep image env flags and `--pure` |
| No `sessionID` after turn 1 | Non-JSON output or interrupted run | Use `--format json` and wait for completion |
| Resume fails | Platform lacks pause/resume | Upgrade CubeSandbox and SDK |
| Template stays `PULLING` | Node cannot pull the image | Use a reachable registry and credentials |
| Task times out | Model/tool loop exceeds limit | Increase exec timeout only after inspecting JSONL |

## Validation Status

On 2026-07-29, the pinned OpenCode binary and configuration completed a live Hy3
text request and a native `read` tool call. Offline tests cover URL validation,
the two-stage vault secret boundary, destructive-command guardrails, template
package/architecture/context invariants, the OpenCode V1 config binding,
runtime security warnings, command quoting, SDK compatibility fallback,
session-ID parsing, and verbose JSONL diagnostics. Full CubeEgress and
pause/resume validation requires an accessible CubeSandbox deployment and
should be recorded before merging.

## References

- Runnable example: [`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
- [Bring your own image](../tutorials/bring-your-own-image.md)
- [Template from image](../tutorials/template-from-image.md)
- [Snapshot / clone / rollback](../snapshot-rollback-clone.md)
- [Security proxy](../security-proxy.md)
- [OpenCode stable providers](https://opencode.ai/docs/providers/)
