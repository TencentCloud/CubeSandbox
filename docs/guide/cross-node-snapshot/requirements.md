# Cross-node migration requirements

**Cross-node migration** here means restoring a Pause package or starting from a Snapshot on another node with Resume / FromSnap. It is not live migration of a running VM.

The default `xfs` backend keeps rootfs, memory, and metadata on the origin. Migration needs a shared backend:

- `jfs` stores files on one JuiceFS mounted by every eligible node.
- `s3` publishes pause packages and snapshots to shared S3 through CubeS3lvol.

See [JuiceFS service configuration](./juicefs.md), [CubeS3lvol service configuration](./cubes3lvol.md), and [Snapshot, Rollback & Clone](../snapshot-rollback-clone.md).

## Requirements

Cross-node Resume / FromSnap requires all conditions below.

### Use a shared backend

Select `--backend jfs|s3` when building the template. Templates, sandboxes, pause packages, and snapshots inherit that choice; a derived chain cannot change backend. Omitting the flag selects `xfs`.

```bash
cubemastercli tpl create-from-image \
  --image <img> \
  --writable-layer-size 4Gi \
  --backend jfs \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

Check `BACKEND` with `cubemastercli cubebox template list`. The current release cannot convert existing objects between `xfs`, `jfs`, and `s3`.

### The origin cannot schedule

The scheduler always prefers the origin:

1. Restore on the origin while it is healthy and schedulable.
2. When the origin is offline or [isolated](../node-operations.md), check shared-data readiness and host compatibility.
3. Select a compatible node only when all gates pass; otherwise return an error.

External storage adds placement constraints:

- A **host mount** has no cluster-wide identity and pins the sandbox to its origin.
- A **Plugin Volume** can move with the sandbox only when every eligible node has its driver and backend access. A missing Volume or driver, or a failed `Attach`, aborts restore.
- FromSnap may attach the same read-write Volume while the source still runs. Stop the source or use read-only access unless the backend safely supports that sharing pattern.

### Shared data is ready

Master evaluates persisted database state:

> `CanCrossNode(backend, remote_status)` is true only when `backend` is `jfs` or `s3` and `remote_status == ready`.

The state machine is `pending → inprogress → ready / failed`. `ready` means rootfs, memory, and metadata are complete on the shared backend. Data that is not ready remains usable on the origin but cannot migrate. `xfs` leaves the status empty and therefore restores only on the origin.

### CPU, kernel, and KVM are compatible

Restoring memory state requires a target compatible with the origin. Current automatic gates are:

- `cpuid_hash` equals the origin value;
- `host_kernel_release` equals the origin value;
- target `kvm_module_taint` is empty.

Inspect `HostFacts` with `cubeopscli node list --json`:

| Field | Meaning | Current gate |
|---|---|---|
| `cpuid_hash` | CPU identity and feature hash | Equality |
| `host_kernel_release` | `uname -r` | Equality |
| `kvm_module_taint` | Forced, out-of-tree, or unsigned KVM module marker | Reject non-empty target |
| `host_kernel_fingerprint` | Kernel release and normalized cmdline fingerprint | Display only |
| `cpu_vendor` | CPU vendor | Display only |
| `kvm_api_version` | KVM API version | Display only |

Cubelet computes `cpuid_hash` as SHA-256 over identity fields and sorted features from the first logical CPU in `/proc/cpuinfo`. x86 inputs include vendor, family, model, stepping, and flags; ARM inputs include implementer, architecture, variant, part, revision, and Features. Compare full `HostFacts` manually on heterogeneous-core machines.

## Inspection commands

```bash
# Backend and pause-package status
cubemastercli cubebox list --all

# Snapshot backend, remote_status, origin, and replicas
cubemastercli cubebox snapshot list
cubemastercli cubebox snapshot info --snapshot-id <snapshot-id>

# Template backend
cubemastercli cubebox template list
cubemastercli cubebox template info <template-id>

# Node health, isolation, and HostFacts
cubeopscli --address 127.0.0.1 --port 3010 node list
cubeopscli --address 127.0.0.1 --port 3010 node list --json
```

| Object | Field | Purpose |
|---|---|---|
| Sandbox | `backend`, `remote` | CoW backend and pause-package sync state |
| Snapshot | `backend`, `remote_status` | Whether cross-node placement is allowed |
| Snapshot | `origin_node_id`, `origin_node_ip` | Origin identity |
| Snapshot | `replicas` | Per-node status, phase, and error |
| Template | `BACKEND` | Backend inherited by derived resources |
| Node | `HostFacts` | CPU, kernel, and KVM comparison |

See [Node Operations](../node-operations.md) for isolation and [CLI Tools](../cli-tools.md) for full command documentation.
