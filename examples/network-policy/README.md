# Network Policy

Control outbound network access for a Cube Sandbox at creation time.
Three modes are provided: fully air-gapped, CIDR allowlist, and CIDR denylist.

## 1. Background

**Cube Sandbox** is a lightweight MicroVM platform compatible with the
[E2B SDK](https://e2b.dev). Network policies are enforced at the **Cubelet tap
network layer** — before packets leave the VM — so they cannot be bypassed from
inside the sandbox.

```
Sandbox VM
    │  outbound packet
    ▼
Cubelet tap network layer  ◄── policy applied here
    │  allowed
    ▼
Public internet / internal network
```

## 2. Policy Modes

### Mode 1 — No Internet (`network_no_internet.py`)

All outbound traffic is blocked. Use this for code execution and data-processing
tasks that have no legitimate need for external network access.

```python
Sandbox.create(
    template=template_id,
    allow_internet_access=False,
)
```

### Mode 2 — Allowlist (`network_allowlist.py`)

Only traffic to the specified CIDR ranges is allowed; everything else is dropped.
Use this when the sandbox needs to reach specific internal services and must not
reach any other destination.

```python
Sandbox.create(
    template=template_id,
    allow_internet_access=False,
    network={
        "allow_out": ["10.0.0.53/32", "10.0.1.0/24"],
    },
)
```

### Mode 3 — Denylist (`network_denylist.py`)

Full internet access is allowed, but traffic to the specified CIDR ranges is
blocked. Use this to block cloud-provider metadata endpoints or sensitive
internal subnets while keeping general internet access intact.

```python
Sandbox.create(
    template=template_id,
    allow_internet_access=True,
    network={
        "deny_out": ["169.254.0.0/16", "10.0.0.0/8"],
    },
)
```

## 3. Policy Summary

| Mode | `allow_internet_access` | `network` key | Effect |
|------|------------------------|---------------|--------|
| No internet | `False` | _(none)_ | All outbound traffic blocked |
| Allowlist | `False` | `allow_out` | Only listed CIDRs reachable |
| Denylist | `True` | `deny_out` | Listed CIDRs blocked, rest allowed |

## 4. Prerequisites

- A running Cube Sandbox deployment
- Python 3.8+

```bash
pip install -r requirements.txt
```

## 5. Quick Start

### Step 1 — Create a Template

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-int.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

> **Image registry:** Use `cube-sandbox-int.tencentcloudcr.com` (international)
> or `cube-sandbox-cn.tencentcloudcr.com` (mainland China).

Note the `template_id` printed on success.

### Step 2 — Configure Environment Variables

```bash
cp .env.example .env
# edit .env and fill in E2B_API_URL and CUBE_TEMPLATE_ID
```

Or export directly:

```bash
export E2B_API_KEY=dummy
export E2B_API_URL=http://<your-node-ip>:3000
export CUBE_TEMPLATE_ID=<template-id>
```

### Step 3 — Run an Example

```bash
# Fully air-gapped sandbox
python network_no_internet.py

# Allowlist: only specific CIDRs are reachable
python network_allowlist.py

# Denylist: block specific CIDRs, allow everything else
python network_denylist.py
```

Expected output for `network_no_internet.py`:

```
internet access blocked: True
isolated execution ok
```

Expected output for `network_denylist.py`:

```
public internet: 200
metadata endpoint blocked: True
denylist network ok
```

## 6. How It Works

Cube Sandbox translates the `network` parameter into a `CubeVSContext` struct
that is passed to Cubelet when the VM is created:

| SDK parameter | Cubelet field | Enforcement |
|---------------|---------------|-------------|
| `allow_internet_access=False` | `AllowInternetAccess=false` | Drop all public-IP traffic |
| `network.allow_out` | `AllowOut` (CIDR list) | Forward only matching destinations |
| `network.deny_out` | `DenyOut` (CIDR list) | Drop matching destinations |

All enforcement happens in the tap network device of the KVM MicroVM, so
policies are applied at the kernel level and cannot be bypassed from inside
the sandbox.

## 7. Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| Sandbox cannot reach expected address | Wrong CIDR in allowlist | Verify the CIDR covers the target IP |
| Metadata endpoint still reachable | CIDR not in denylist | Add `169.254.0.0/16` to `deny_out` |
| `Template not found` | Wrong template ID | Run `cubemastercli tpl list` |
| `Connection refused` | CubeAPI not reachable | Check `E2B_API_URL` and port 3000 |

## 8. Directory Structure

```
network-policy/
├── README.md                  # This file
├── network_no_internet.py     # Mode 1: fully air-gapped sandbox
├── network_allowlist.py       # Mode 2: outbound CIDR allowlist
├── network_denylist.py        # Mode 3: outbound CIDR denylist
├── env_utils.py               # .env loader utility
├── requirements.txt           # Python dependencies
└── .env.example               # Environment variable template
```
