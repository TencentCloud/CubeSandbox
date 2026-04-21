# Code Sandbox Quickstart

[中文文档](README_zh.md)

The most basic Cube Sandbox usage: create a sandbox, run Python code inside it,
and execute shell commands — all from your local machine using the E2B Python SDK.

## 1. Background

**Cube Sandbox** is a lightweight MicroVM platform fully compatible with the
[E2B SDK](https://e2b.dev). Each `Sandbox.create()` call boots a new KVM
MicroVM from a template snapshot in under 50 ms. The sandbox is fully isolated —
dedicated kernel, filesystem, and network. When the `with` block exits, the
sandbox is automatically deleted.

```
Your script  (E2B SDK)
    │  REST API
    ▼
CubeAPI  (port 3000)
    │
    ▼
CubeMaster ──► Cubelet ──► KVM MicroVM
                               │
                           cube-agent (PID 1)
                               │
                           Python / shell process
```

## 2. Prerequisites

- A running Cube Sandbox deployment
- Python 3.8+

```bash
pip install -r requirements.txt
```

The example scripts use `python-dotenv` to best-effort load a `.env` file from
the current directory or the script directory. If no `.env` file exists, they
continue with the current process environment variables.

## 3. Quick Start

### Step 1 — Create the Code Template

```bash
cubemastercli tpl create-from-image \
  --image ccr.ccs.tencentyun.com/ags-image/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

Note the `template_id` printed on success.

### Step 2 — Configure Environment Variables

```bash
cp .env.example .env
# edit .env and fill in E2B_API_URL and CUBE_TEMPLATE_ID
```

After that, you can run any example script directly without manually exporting
the variables first.

Or export directly:

```bash
export E2B_API_KEY=dummy
export E2B_API_URL=http://<your-node-ip>:3000
export CUBE_TEMPLATE_ID=<template-id>

# Only needed when using Cube's built-in mkcert certificate:
# export SSL_CERT_FILE=$(mkcert -CAROOT)/rootCA.pem
```

### Step 3 — Run Python Code in a Sandbox

```bash
python exec_code.py
```

Expected output:

```
Python 3.x.x (...)
hello cube
sum(1..100) = 5050
```

### Step 4 — Execute Shell Commands

```bash
python cmd.py
```

Expected output:

```
hello cube
```

## 4. All Examples

| Script | What it shows |
|--------|---------------|
| `exec_code.py` | `sandbox.run_code()` — execute Python code inside a sandbox |
| `cmd.py` | `sandbox.commands.run()` — execute shell commands |
| `create.py` | `sandbox.get_info()` — retrieve sandbox metadata |
| `read.py` | `sandbox.files.read()` — read a file from the sandbox filesystem |
| `pause.py` | `sandbox.pause()` / `sandbox.connect()` — snapshot and restore |
| `network_no_internet.py` | `allow_internet_access=False` — fully air-gapped sandbox |
| `network_allowlist.py` | `allow_out` — whitelist specific CIDRs, block everything else |
| `network_denylist.py` | `deny_out` — block specific CIDRs, allow the rest |

### exec_code.py — Run Python Code

```python
with Sandbox.create(template=template_id) as sandbox:
    sandbox.run_code(python_code, on_stdout=lambda line: print(line))
```

### cmd.py — Shell Commands

```python
with Sandbox.create(template=template_id) as sandbox:
    result = sandbox.commands.run("echo hello cube")
    print(result.stdout)
```

### pause.py — Pause & Resume

Snapshot a running sandbox to free compute resources, then restore it later:

```python
with Sandbox.create(template=template_id) as sandbox:
    sandbox.pause()       # save memory snapshot, release VM
    time.sleep(3)
    sandbox.connect()     # restore snapshot, resume execution
    print(sandbox.get_info())
```

### Network Policies

```bash
# Fully air-gapped
python network_no_internet.py

# Whitelist: only allow specific CIDRs
python network_allowlist.py

# Denylist: block specific CIDRs, allow the rest
python network_denylist.py
```

## 5. Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS without CA cert | Set `SSL_CERT_FILE=$(mkcert -CAROOT)/rootCA.pem` |
| `Template not found` | Wrong template ID | Re-run `cubemastercli tpl list` |
| `Connection refused` | CubeAPI not reachable | Check `E2B_API_URL` and port 3000 |
| `Sandbox timeout` | Sandbox exceeded its TTL | Increase `timeout` in `Sandbox.create()` |

## 6. Directory Structure

```
code-sandbox-quickstart/
├── README.md                  # English documentation (this file)
├── README_zh.md               # Chinese documentation
├── exec_code.py               # Run Python code inside a sandbox
├── cmd.py                     # Execute shell commands
├── create.py                  # Create sandbox and inspect metadata
├── read.py                    # Read files from the sandbox filesystem
├── pause.py                   # Pause and resume a sandbox
├── network_no_internet.py     # Fully air-gapped sandbox
├── network_allowlist.py       # Outbound CIDR allowlist
├── network_denylist.py        # Outbound CIDR denylist
├── requirements.txt           # Python dependencies
└── .env.example               # Environment variable template
```
