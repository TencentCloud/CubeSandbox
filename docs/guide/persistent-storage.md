# Persistent Storage (Host Mount)

Cube Sandbox runs inside lightweight MicroVMs — by default, all data written inside a sandbox is **ephemeral** and disappears when the sandbox is killed. **Host mount** is a Cube-specific extension that bind-mounts directories from the sandbox host node into the sandbox, enabling persistent, shared storage without copying files.

## Concept

```
Sandbox host node
┌────────────────────────────────────────────────────────────────┐
│  /data/shared/models  ────────► /models   (read-only)          │
│  /data/shared/output  ────────► /output   (read-write)         │
│                                                                │
│              KVM MicroVM (sandbox)                              │
└────────────────────────────────────────────────────────────────┘
```

A host mount maps an **absolute path on the sandbox host node** to a **path inside the sandbox VM**. Changes to a read-write mount are visible on both sides immediately — no sync, no upload, no delay.

| Property | Description |
|----------|-------------|
| **Mechanism** | Linux bind-mount performed by Cubelet before the VM boots |
| **Scope** | Node-local — the `hostPath` must exist on the sandbox host node that runs the sandbox |
| **Multiplicity** | Multiple mounts can be specified in a single `Sandbox.create()` call |
| **Access mode** | Each mount can be independently `readOnly: true` or `readOnly: false` |
| **Path restriction** | `hostPath` must be under an allowed directory prefix (default `/data/shared/`) |
| **Compatibility** | Works with both E2B SDK (`e2b_code_interpreter`) and Cube SDK (`cubesandbox`) |

## Use Cases

- **Large datasets** — mount a multi-GB dataset directory into many sandboxes without copying
- **Model weights** — share a read-only model directory across concurrent inference sandboxes
- **Output persistence** — write sandbox results to a host path so they survive sandbox teardown
- **Source code workspace** — mount a code repository for on-demand execution or analysis

## Mount Descriptor

Host mounts are requested through the `metadata` field of `Sandbox.create()` using the key `host-mount`. The value is a **JSON-encoded array** of mount descriptors:

```json
[
  {
    "hostPath":  "/data/shared/mydir",
    "mountPath": "/mnt/data",
    "readOnly":  false
  }
]
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `hostPath` | string | Yes | Absolute path on the **sandbox host node**, must be under an allowed prefix |
| `mountPath` | string | Yes | Target path inside the sandbox VM |
| `readOnly` | bool | Yes | `true` = read-only; `false` = read-write |

::: warning
`hostPath` refers to the filesystem of the **sandbox host node** running the sandbox, not the machine where your SDK script executes. If you are calling the API from a remote machine, make sure the path exists on the sandbox host node, not on your laptop.
:::

## Quick Start

### Prerequisites

- A running Cube Sandbox deployment
- Python 3.8+
- The host directories must exist on the sandbox host node before creating the sandbox

Prepare the host directories on the sandbox host node:

```bash
sudo mkdir -p /data/shared/rw /data/shared/ro
echo "hello from host" | sudo tee /data/shared/ro/greeting.txt
sudo chown -R 1000:1000 /data/shared/rw
```

Install the SDK:

```bash
pip install e2b-code-interpreter
# or
pip install cubesandbox
```

### Create a Sandbox with Host Mounts

```python
import json
import os
from e2b_code_interpreter import Sandbox

template_id = os.environ["CUBE_TEMPLATE_ID"]

with Sandbox.create(
    template=template_id,
    metadata={
        "host-mount": json.dumps([
            {
                "hostPath":  "/data/shared/rw",
                "mountPath": "/mnt/rw",
                "readOnly":  False,
            },
            {
                "hostPath":  "/data/shared/ro",
                "mountPath": "/mnt/ro",
                "readOnly":  True,
            },
        ])
    },
) as sandbox:
    result = sandbox.commands.run("ls /mnt/rw /mnt/ro")
    print("mount contents:", result.stdout.strip())
```

Expected output:

```
mount contents: /mnt/ro:
greeting.txt
/mnt/rw:
```

### Write Data That Survives Sandbox Teardown

```python
import json
from cubesandbox import Sandbox

mounts = json.dumps([
    {"hostPath": "/data/shared/rw", "mountPath": "/mnt/rw", "readOnly": False},
])

with Sandbox.create(
    template=template_id,
    metadata={"host-mount": mounts},
) as sandbox:
    sandbox.commands.run("echo 'result data' > /mnt/rw/output.txt")

# The sandbox is gone, but the file persists on the host:
# cat /data/shared/rw/output.txt  →  result data
```

## How It Works

| Step | What happens |
|------|-------------|
| `Sandbox.create(metadata=...)` | CubeAPI passes the `host-mount` JSON to CubeMaster |
| CubeMaster validates paths | Checks `hostPath` is under allowed prefixes, resolves `..` to prevent traversal |
| Cubelet receives the request | Parses the mount list and performs bind-mounts before booting the VM |
| VM boots | The paths appear inside the sandbox at the specified `mountPath` locations |
| Read-only mount | The kernel enforces `MS_RDONLY`; writes are rejected with `EROFS` |

## Path Restriction

For security, `hostPath` is restricted to a set of **allowed directory prefixes**. By default, only paths under `/data/shared/` are permitted. Attempts to mount paths outside the allowed prefixes will be **rejected at sandbox creation time**.

### Default Behavior

Out of the box, only the following paths are valid:

```python
# ✅ Allowed
{"hostPath": "/data/shared/models", "mountPath": "/models", "readOnly": True}
{"hostPath": "/data/shared/team-a/output", "mountPath": "/output", "readOnly": False}

# ❌ Rejected
{"hostPath": "/etc/passwd", "mountPath": "/mnt/x", "readOnly": True}
{"hostPath": "/tmp/data", "mountPath": "/mnt/data", "readOnly": False}
{"hostPath": "/data/shared/../etc", "mountPath": "/mnt/x", "readOnly": True}  # traversal blocked
```

### Error Response

If a disallowed path is specified, the SDK raises an `ApiError`:

```python
from cubesandbox import ApiError

try:
    sandbox = Sandbox.create(
        template=template_id,
        metadata={"host-mount": json.dumps([
            {"hostPath": "/etc/passwd", "mountPath": "/mnt/x", "readOnly": True}
        ])}
    )
except ApiError as e:
    print(e.status_code)  # 500
    print(str(e))
    # "host-mount" entry[0]: hostPath "/etc/passwd" is not within an allowed mount prefix
```

### Custom Allowed Prefixes

Cluster administrators can configure additional allowed prefixes in CubeMaster's config file:

```yaml
extra_conf:
  allowed_host_mount_prefixes:
    - "/data/shared/"
    - "/data/team-assets/"
    - "/mnt/nfs/datasets/"
```

When the list is empty or omitted, the default `["/data/shared/"]` applies. The root path `/` is explicitly forbidden — CubeMaster will refuse to start if it appears in the list.

### Security Mechanisms

| Mechanism | Purpose |
|-----------|---------|
| `filepath.Clean` | Resolves `..` segments to prevent path-traversal bypass |
| Trailing `/` in prefix match | Prevents prefix spoofing (e.g. `/data/shared_evil` won't match `/data/shared/`) |
| Startup validation | Rejects `/` in config to prevent accidental full-host exposure |
| Dual enforcement | Both annotation path (CubeAPI → CubeMaster) and direct volume path are validated |

## Permissions

Host mounts preserve the original ownership and permission bits of the host directory. The `readOnly` flag only controls whether Cube mounts the path read-only or read-write — it does **not** override Linux file permissions.

If the sandbox user's UID does not match the host directory owner, writes will fail with `Permission denied`. Quick fix:

```bash
# Align directory owner to the sandbox user
sudo chown 1000:1000 /data/shared/rw
```

For other solutions (read-only mounts, POSIX ACLs, running as root, etc.), see [Host Mount Permissions Troubleshooting](./troubleshooting/host-mount-permissions.md).

## Multi-Node Cluster: Shared Storage

Host mounts are **node-local**. In a multi-node cluster, the `hostPath` must exist on whichever sandbox host node schedules the sandbox. The recommended approach is to use shared storage so the same filesystem is mounted at a uniform path on all nodes.

### NFS Example

Mount NFS on every sandbox host node:

```bash
# Add to /etc/fstab:
nfs-server:/export/shared  /data/shared  nfs  defaults,hard,intr  0 0
```

```bash
sudo mount -a
ls /data/shared   # All nodes see the same content
```

Sandboxes can then use any path under `/data/shared/` regardless of which node they are scheduled on.

### Object Storage (S3/COS) Example

Use a FUSE tool (e.g. `s3fs`, `cosfs`) to mount an object storage bucket as a local directory:

```bash
# Install cosfs (Tencent Cloud COS)
sudo apt-get install cosfs

# Configure credentials
echo "my-bucket:AKIDxxxx:xxxxxx" > /etc/passwd-cosfs
chmod 600 /etc/passwd-cosfs

# Mount to /data/shared
cosfs my-bucket /data/shared -ourl=https://cos.ap-guangzhou.myqcloud.com \
  -oallow_other -ouid=1000 -ogid=1000
```

```bash
# AWS S3 using s3fs
s3fs my-bucket /data/shared \
  -o iam_role=auto \
  -o url=https://s3.amazonaws.com \
  -o allow_other -o uid=1000 -o gid=1000
```

::: tip
Object storage mounts work best for read-only scenarios (model weights, datasets). Write performance and POSIX compatibility are inferior to NFS; for frequent small-file writes, prefer NFS or block storage.
:::

## Multi-Tenant Isolation

In multi-tenant environments, each tenant's sandbox should only access its own data directory, with no visibility into other tenants' files. The recommended approach combines directory structure with application-layer enforcement.

### Directory Layout

Partition by tenant ID:

```
/data/shared/
├── tenant-a/
│   ├── datasets/
│   └── output/
├── tenant-b/
│   ├── datasets/
│   └── output/
└── tenant-c/
    └── ...
```

Each tenant's sandbox only mounts its own subdirectory:

```python
import json
from cubesandbox import Sandbox

tenant_id = "tenant-a"

mounts = json.dumps([
    {
        "hostPath": f"/data/shared/{tenant_id}/datasets",
        "mountPath": "/datasets",
        "readOnly": True,
    },
    {
        "hostPath": f"/data/shared/{tenant_id}/output",
        "mountPath": "/output",
        "readOnly": False,
    },
])

with Sandbox.create(
    template=template_id,
    metadata={"host-mount": mounts},
) as sandbox:
    sandbox.commands.run("ls /datasets /output")
```

Tenant A's sandbox can only see `/data/shared/tenant-a/` — it cannot access `tenant-b` or `tenant-c` directories.

### Application-Layer Enforcement

In the code that calls `Sandbox.create()`, construct `hostPath` from the authenticated user's tenant ID. **Never allow users to pass arbitrary paths:**

```python
def create_tenant_sandbox(tenant_id: str, template_id: str):
    """Platform controls mount paths; tenants cannot specify arbitrary hostPaths."""
    base = f"/data/shared/{tenant_id}"
    mounts = json.dumps([
        {"hostPath": f"{base}/input",  "mountPath": "/input",  "readOnly": True},
        {"hostPath": f"{base}/output", "mountPath": "/output", "readOnly": False},
    ])
    return Sandbox.create(
        template=template_id,
        metadata={"host-mount": mounts},
    )
```

### Filesystem Permission Hardening (Optional)

Combine with Linux permissions as a defense-in-depth layer — even if a path is somehow bypassed, the OS rejects cross-tenant access:

```bash
# Create isolated directories with distinct UIDs/GIDs per tenant
sudo mkdir -p /data/shared/tenant-a /data/shared/tenant-b
sudo chown 1001:1001 /data/shared/tenant-a
sudo chown 1002:1002 /data/shared/tenant-b
sudo chmod 0700 /data/shared/tenant-a
sudo chmod 0700 /data/shared/tenant-b
```

Even if a sandbox attempts to access another tenant's path (e.g. via traversal), it is blocked by both CubeMaster's path validation and OS-level permissions.

### Isolation Layers Summary

| Layer | Mechanism | Purpose |
|-------|-----------|---------|
| Directory layout | Isolated subdirectory per tenant; each sandbox only mounts its own path | Structural data isolation; tenants cannot see each other |
| Application | Platform code constructs paths; user input is not trusted | Tenant isolation under normal operation |
| OS | Directory owner/mode permissions | Last-resort protection against any bypass |

## Best Practices

- **Prefer read-only mounts** for input data (datasets, models, configs). This prevents accidental writes from the sandbox and is the safest default.
- **Use dedicated directories** for read-write mounts. Avoid mounting broad paths like `/` or `/home`. Create a purpose-specific directory (e.g. `/data/shared/output`) and scope the mount to it.
- **Check permissions early.** Run `id` and `stat` inside the sandbox to verify the effective UID matches the host directory owner before relying on writes.
- **Clean up output directories** periodically. Host-mounted output directories accumulate files across sandbox sessions — unlike the sandbox itself, they are not cleaned up on teardown.
- **Keep allowed prefixes narrow.** Only add specific trusted paths to `allowed_host_mount_prefixes`. Never add root-level paths like `/data/` — prefer deeper paths like `/data/shared/`.

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `hostPath "..." is not within an allowed mount prefix` | Path is outside allowed prefixes | Move data under `/data/shared/` or update `allowed_host_mount_prefixes` in CubeMaster config |
| `No such file or directory` inside sandbox | `hostPath` does not exist on the sandbox host node | Create the directory on the node before running |
| `Read-only file system` on write | Mounted with `readOnly: true` | Use `readOnly: false` |
| `Permission denied` on write | Host directory ownership mismatch | See [Permissions](#permissions) section above |
| `Template not found` | Wrong template ID | Run `cubemastercli tpl list` |
| `Connection refused` | CubeAPI not reachable | Check `E2B_API_URL` and that port 3000 is open |

## Reference

- Example code: [`examples/host-mount/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/host-mount)
- Related issue: [#239](https://github.com/TencentCloud/CubeSandbox/issues/239)
