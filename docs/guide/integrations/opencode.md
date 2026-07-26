---
title: OpenCode Integration Guide
author: Sam-Hui-dot
date: 2026-07-05
tags:
  - integration
  - opencode
  - coding-agent
  - agent
lang: en-US
---

# OpenCode Integration Guide

[中文文档](../../zh/guide/integrations/opencode.md)

Run [OpenCode](https://opencode.ai/), an open-source terminal coding agent,
inside CubeSandbox MicroVMs. This guide pairs with the runnable
[`examples/opencode-integration`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
project.

## Integration Target and Version

| Component | Version |
|---|---|
| OpenCode CLI | `opencode-ai` 1.17.13, pinned by `OPENCODE_VERSION` |
| Node.js | 22.23.1 official archive, verified with SHA-256 |
| CubeSandbox base image | `ghcr.io/tencentcloud/cubesandbox-base:2026.16`, pinned by digest |
| Host SDKs | `e2b==2.7.0`, `cubesandbox==0.3.0` |
| CubeSandbox platform | `>=0.3.0` for pause/resume; CubeEgress required for header injection |

OpenCode's documented non-interactive mode is `opencode run [message..]`. The
example uses:

```bash
opencode run --model <provider/model> --dir /workspace --auto --format json "<prompt>"
```

## Why Run OpenCode Inside CubeSandbox

OpenCode can edit files, execute commands, install packages, and call LLM APIs.
Running it in CubeSandbox gives each coding task a disposable MicroVM boundary:

| Concern | CubeSandbox pattern |
|---|---|
| Isolation | One MicroVM per task or session |
| Reproducibility | Boot from a pinned template image |
| Fast reuse | Pause/resume preserves `/workspace` and OpenCode state |
| Network control | Default-deny egress with explicit LLM host allow rules |
| Secret handling | CubeEgress can inject API keys on the wire |
| Reviewability | Host scripts verify generated files and test results |

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`.
- Docker plus a registry reachable from Cube nodes.
- Python 3.10+ for host-side driver scripts.
- An LLM provider API key supported by OpenCode.

## Integration Steps

### 1. Build the template image

```bash
cd examples/opencode-integration
docker build --platform linux/amd64 \
  -t <registry>/opencode-cube:latest .
docker push <registry>/opencode-cube:latest
```

The image installs Node.js, OpenCode, `git`, `ripgrep`, Python, and small
inspection tools on top of the CubeSandbox base image.

### 2. Register the Cube template

```bash
cubemastercli tpl create-from-image \
  --image <registry>/opencode-cube:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

The inherited CubeSandbox base entrypoint keeps envd on port `49983` and serves
the standard `/health` endpoint.

### 3. Configure the host driver

```bash
cp .env.example .env
pip install -r requirements.txt
```

Set:

```bash
E2B_API_URL=http://<node>:3000
E2B_API_KEY=e2b_000000
CUBE_TEMPLATE_ID=<template-id>
OPENCODE_MODEL=openai/gpt-4.1-mini
OPENAI_API_KEY=<provider-key>
```

For custom endpoints, set `OPENCODE_BASE_URL`. If it is not set, the scripts
also accept provider-specific variables such as `OPENAI_BASE_URL`. The scripts
write an `opencode.json` in the sandbox workspace with
`provider.<provider_prefix>.options.baseURL`.

When validating against the repository's `dev-env/` VM, set
`CUBE_DEV_SIDECAR=1` so the host-side SDK routes sandbox traffic through
`examples/e2b-dev-sidecar`.

`network_policy.py` and `checkpoint_fork_opencode.py` use the native
`cubesandbox` SDK. When running either script against `dev-env/`, also set:

```bash
CUBE_API_URL=http://127.0.0.1:13000
CUBE_PROXY_NODE_IP=127.0.0.1
CUBE_PROXY_PORT_HTTP=11080
```

### 4. Run the one-shot coding task

```bash
python3 run_opencode.py --dry-run
python3 run_opencode.py
```

The script seeds a small Python project, runs OpenCode, then verifies:

- `calculator.add` was implemented.
- `python3 -m unittest discover -v` passes.
- `result.md` contains the marker `OPENCODE_CUBE_OK`.

### 5. Demonstrate session persistence

```bash
python3 resume_opencode.py
```

The first turn writes `plan.md`. `sandbox.pause()` snapshots the VM and root
filesystem; the script reconnects to the same sandbox, verifies `/workspace`
and OpenCode's state directory survived, then continues the task and checks
`OPENCODE_RESUME_OK`.

### 6. Create a checkpoint, fork, and continue

```bash
python3 checkpoint_fork_opencode.py
```

This workflow runs OpenCode in a source sandbox, creates an explicit snapshot
after turn 1, starts a new sandbox with `template=snapshot_id`, and continues
the same OpenCode session in the fork. It proves the workspace and OpenCode
state were inherited while the source and fork remain independent, then deletes
the temporary snapshot. Both sandboxes use default-deny egress and the same
LLM-host allow rule. CubeEgress injects the real provider credential on the
wire, and the workflow verifies that both OpenCode commands receive only a
placeholder key while unrelated hosts remain blocked.

### 7. Restrict egress and inject credentials

```bash
python3 network_policy.py
```

The strict path uses the native `cubesandbox` SDK:

- `allow_internet_access=False` makes egress default-deny.
- A CubeEgress rule allows only the configured LLM host.
- `Inject` attaches the real provider credential as an HTTP header.
- The VM's global environment contains no provider key.
- The OpenCode command receives only a placeholder key.
- The script fails if either secret invariant is violated or an unrelated host
  is reachable.

Set `OPENCODE_LLM_HOST` when the LLM API host differs from the provider
default, especially when using `OPENCODE_BASE_URL` or a provider-specific
`<PROVIDER>_BASE_URL`:

```bash
OPENCODE_LLM_HOST=api.openai.com python3 network_policy.py
```

Use `--skip-agent` to verify policy behavior without spending LLM tokens:

```bash
python3 network_policy.py --skip-agent
```

## Key Code Snippets

Headless OpenCode command construction:

```python
opencode_command(
    prompt,
    workspace="/workspace",
    model="openai/gpt-4.1-mini",
    title="cube-opencode-demo",
)
```

Restricted egress rule:

```python
Rule(
    name="allow_openai_llm",
    match=Match(scheme="https", sni="api.openai.com", host="api.openai.com"),
    action=Action(
        allow=True,
        audit="metadata",
        inject=[Inject(
            header="Authorization",
            secret=secret,
            format="Bearer ${SECRET}",
        )],
    ),
)
```

## Caveats

- OpenCode still needs a real LLM provider key for live runs.
- `--auto` lets OpenCode perform edits and commands without interactive approval;
  use it only inside the isolated sandbox boundary.
- Direct env injection in `run_opencode.py` is convenient for local validation;
  only the active provider key is forwarded and known secret values are
  redacted from failure output. Shared clusters should prefer
  `network_policy.py` or `checkpoint_fork_opencode.py`.
- Custom provider support depends on OpenCode's provider configuration and the
  target endpoint's compatibility with the selected model.
- The example pins the Node.js archive and verifies its official SHA-256 before
  extraction. Update `NODE_VERSION` and `NODE_SHA256` together after checking
  Node.js `SHASUMS256.txt`.

## Validation

From `examples/opencode-integration`:

```bash
python3 -m py_compile env_utils.py _opencode_common.py egress_policy.py run_opencode.py resume_opencode.py checkpoint_fork_opencode.py network_policy.py
python3 -m unittest discover . -p 'test_*.py'
bash -n build-template.sh
docker build --platform linux/amd64 -t opencode-cube:verify .
docker run --rm opencode-cube:verify opencode --version
```

Live CubeSandbox validation:

```bash
python3 run_opencode.py
python3 resume_opencode.py
python3 checkpoint_fork_opencode.py
python3 network_policy.py --skip-agent
python3 network_policy.py
```

## References

- [OpenCode CLI documentation](https://opencode.ai/docs/cli/)
- [OpenCode provider documentation](https://opencode.ai/docs/providers/)
- [CubeSandbox OpenCode example](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/opencode-integration)
- [CubeSandbox Security Proxy](../security-proxy.md)
