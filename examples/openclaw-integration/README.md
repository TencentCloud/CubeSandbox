# Integrating Cube Sandbox with OpenClaw

[中文文档](README_zh.md)

Deploy Cube Sandbox and configure the OpenClaw skill so AI agents can execute
code in isolated VM environments — safely and without any setup overhead.

## 1. Background

**Cube Sandbox** is a lightweight MicroVM platform fully compatible with the
[E2B SDK](https://e2b.dev). The `cube-sandbox` OpenClaw skill wraps the E2B
SDK, letting any OpenClaw agent execute arbitrary code and shell commands inside
ephemeral KVM MicroVMs.

```
OpenClaw Agent
    │  cube-sandbox skill
    ▼
E2B SDK (Python)
    │  REST API
    ▼
CubeAPI (port 3000)
    │
    ▼
CubeMaster ──► Cubelet ──► KVM MicroVM
                               │
                           cube-agent (PID 1)
                               │
                           sandboxed code
```

## 2. Key Features

| Feature | Description |
|---------|-------------|
| **Isolated execution** | Every agent task runs in a dedicated MicroVM — separate kernel, filesystem, and network |
| **Fast boot** | Sandbox creation from template snapshot in < 50 ms |
| **E2B compatible** | Uses the standard E2B SDK — works with any E2B-compatible toolchain |
| **Network policy** | Support for allow/deny CIDR lists and full air-gap mode |
| **Pause & resume** | Snapshot a running sandbox and restore it later (stateful reuse) |
| **File I/O** | Read and write files inside the sandbox via `sandbox.files` |
| **Host mount** | Mount a host directory into the sandbox via metadata |

## 3. Prerequisites

- A running Cube Sandbox deployment
- OpenClaw Gateway installed and running
- Python 3.8+ with `e2b-code-interpreter`

```bash
pip install e2b-code-interpreter
```

## 4. Setup

### Step 1 — Deploy Cube Sandbox

Follow one of the deployment guides to get a running Cube Sandbox instance:

- **[Quick Start](https://cube-sandbox.pages.dev/guide/quickstart)** — Fastest path: build → deploy → first sandbox in minutes
- **[One-Click Deployment Guide](https://cube-sandbox.pages.dev/guide/one-click-deploy)** — Full single-node setup for evaluation and development
- **[Multi-Node Cluster Deployment](https://cube-sandbox.pages.dev/guide/multi-node-deploy)** — Expand to a multi-node production cluster

### Step 2 — Configure HTTPS for CubeProxy

CubeProxy exposes **both HTTPS (port 443) and HTTP (port 80)** simultaneously:

- **E2B SDK (default):** Uses HTTPS. Cube ships with a built-in DNS service and
  a pre-installed `cube.app` certificate — no manual certificate setup needed
  for quick start.
- **Direct HTTP access:** Set the `Host` header to
  `<port>-<sandboxId>.<domain>` (e.g. `Host: 49999-abc123-cube.app`).

> `E2B_API_URL` always points to the **Cube API Server** (port 3000),
> not CubeProxy.

### Step 3 — Create a Code Template

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/ags-image/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

Note the `template_id` printed on success.

### Step 4 — Install the OpenClaw Skill

The `cube-sandbox` skill is included in this repository under
`examples/openclaw-integration/skills/cube-sandbox/`.

```bash
cp -r examples/openclaw-integration/skills/cube-sandbox/ ~/.openclaw/workspace/skills/
# then restart OpenClaw Gateway
openclaw gateway restart
```

### Step 5 — Configure Environment Variables

Set the following in your shell or OpenClaw environment:

```bash
export CUBE_TEMPLATE_ID=<template-id>       # from Step 3
export E2B_API_URL=http://<node-ip>:3000    # Cube API Server address
export E2B_API_KEY=dummy                    # any non-empty string

# Only needed when using Cube's built-in mkcert certificate:
# export SSL_CERT_FILE=$(mkcert -CAROOT)/rootCA.pem
```

## 5. Usage

Once installed, the skill is triggered automatically by phrases such as:

- "run this code in a sandbox"
- "safely execute Python"
- "run in isolated environment"
- "cube sandbox"

### Examples

**Run Python code:**
> "在沙箱里跑一段 Python，计算 1 到 100 的和"

**Execute shell commands:**
> "用沙箱执行 `uname -a` 并返回结果"

**Network isolation:**
> "在完全断网的沙箱中运行这段代码"

**File operations:**
> "读取沙箱中 /etc/hosts 的内容"

## 6. Skill Reference

The skill file is at `examples/openclaw-integration/skills/cube-sandbox/SKILL.md`. Key capabilities:

| Capability | E2B API |
|-----------|---------|
| Execute Python | `sandbox.run_code(code)` |
| Shell command | `sandbox.commands.run(cmd)` |
| Read file | `sandbox.files.read(path)` |
| Write file | `sandbox.files.write(path, content)` |
| Pause | `sandbox.pause()` |
| Resume | `sandbox.connect()` |
| No internet | `Sandbox.create(allow_internet_access=False)` |
| CIDR allowlist | `Sandbox.create(network={"allow_out": [...]})` |
| CIDR denylist | `Sandbox.create(network={"deny_out": [...]})` |
| Host mount | `Sandbox.create(metadata={"host-mount": ...})` |

## 7. Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| Skill not triggered | Skill not installed | Verify `~/.openclaw/workspace/skills/cube-sandbox/` exists |
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS without CA cert | Set `SSL_CERT_FILE=$(mkcert -CAROOT)/rootCA.pem` |
| `Template not found` | Wrong `CUBE_TEMPLATE_ID` | Re-run `cubemastercli tpl list` |
| DNS resolution fails | DNS not configured | See skill FAQ for `/etc/hosts` workaround |

## 8. Directory Structure

```
openclaw-integration/
├── README.md                        # English documentation (this file)
├── README_zh.md                     # Chinese documentation
└── skills/
    └── cube-sandbox/
        ├── SKILL.md                 # Skill definition and usage guide
        └── references/
            ├── api.md               # API reference
            └── examples.md          # Additional examples
```
