# Host Mount

Mount host directories into a Cube Sandbox at creation time, giving the sandbox
read or read-write access to files that live on the Cubelet host node.

## 1. Background

**Cube Sandbox** is a lightweight MicroVM platform compatible with the
[E2B SDK](https://e2b.dev). Host mounts are a **Cube-specific extension** to
the standard E2B API: they are requested through the `metadata` field of
`Sandbox.create()` using the key `host-mount`.

```
Cubelet host node
┌──────────────────────────────────────────────────────┐
│  /tmp/rw  ───────────────────► /mnt/rw  (read-write) │
│  /tmp/ro  ───────────────────► /mnt/ro  (read-only)  │
│                                                       │
│              KVM MicroVM (sandbox)                    │
└──────────────────────────────────────────────────────┘
```

### Mount Descriptor Schema

```json
[
  {
    "hostPath":  "/absolute/path/on/host",
    "mountPath": "/path/inside/sandbox",
    "readOnly":  false
  }
]
```

| Field | Type | Description |
|-------|------|-------------|
| `hostPath` | string | Absolute path on the **Cubelet node** to bind-mount |
| `mountPath` | string | Target path inside the sandbox VM |
| `readOnly` | bool | `true` = read-only; `false` = read-write |

> **Note:** `hostPath` refers to the filesystem of the **Cubelet node** running
> the sandbox, not the machine where your script executes.

## 2. Use Cases

- Provide large datasets without copying them into the sandbox image
- Share a read-only model weights or config directory across many sandboxes
- Write sandbox outputs directly to a host path for easy retrieval
- Mount a source-code workspace for on-demand code execution

## 3. Prerequisites

- A running Cube Sandbox deployment
- Python 3.8+
- The host directories (`/tmp/rw` and `/tmp/ro` in the example) must exist on
  the Cubelet node before creating the sandbox

```bash
# On the Cubelet node (or wherever your sandbox VM runs):
mkdir -p /tmp/rw /tmp/ro
echo "hello from host" > /tmp/ro/greeting.txt
```

Install dependencies:

```bash
pip install -r requirements.txt
```

## 4. Quick Start

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

### Step 3 — Run the Example

```bash
python create_with_mount.py
```

Expected output:

```
sandbox info: SandboxInfo(sandbox_id='...', template_id='...', ...)
mount contents: /mnt/ro:
greeting.txt
/mnt/rw:
```

## 5. How It Works

```python
with Sandbox.create(
    template=template_id,
    metadata={
        "host-mount": json.dumps([
            {"hostPath": "/tmp/rw", "mountPath": "/mnt/rw", "readOnly": False},
            {"hostPath": "/tmp/ro", "mountPath": "/mnt/ro", "readOnly": True},
        ])
    },
) as sandbox:
    ...
```

| Step | What happens |
|------|-------------|
| `Sandbox.create(metadata=...)` | CubeAPI passes the `host-mount` JSON to Cubelet |
| Cubelet receives the request | Parses the mount list and performs bind-mounts before booting the VM |
| VM boots | The paths appear inside the sandbox at the specified `mountPath` locations |
| Read-only mount | The kernel enforces `MS_RDONLY`; writes are rejected with `EROFS` |

## 6. Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `No such file or directory` inside sandbox | `hostPath` does not exist on the Cubelet node | Create the directory on the node before running |
| `Read-only file system` on write | Mounted with `readOnly: true` | Use `readOnly: false` or switch to `/mnt/rw` |
| `Template not found` | Wrong template ID | Run `cubemastercli tpl list` |
| `Connection refused` | CubeAPI not reachable | Check `E2B_API_URL` and that port 3000 is open |

## 7. Directory Structure

```
host-mount/
├── README.md               # This file
├── create_with_mount.py    # Example script
├── env_utils.py            # .env loader utility
├── requirements.txt        # Python dependencies
└── .env.example            # Environment variable template
```
