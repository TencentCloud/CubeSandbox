# Cross-Node Snapshots (Pause / Resume / Snapshot)

CubeSandbox persists a sandbox as a **package** of three objects (rootfs / memory / metadata) so you can **Pause**, **Resume**, and take a **Snapshot**:

- **Pause / Resume**: freeze a running sandbox (memory + filesystem) into a pause package, then restore it on the same node or another compatible node.
- **Snapshot**: persist that state as a reusable image. You can create a new sandbox from it (FromSnap) or roll the original sandbox back.

With the default `xfs` backend the package stays on **the node that created it**. Resume and FromSnap must return to that node. If the node is down, isolated, or out of capacity, a paused sandbox cannot be scheduled and a snapshot cannot be started elsewhere.

With the `s3` backend the package is uploaded to **cluster-shared S3** (managed by [CubeS3lvol](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md)). **Any compatible node can fetch it on demand**, which is what makes cross-node Pause / Resume and FromSnap possible. The XFS root disk itself never moves; what migrates is the snapshot / pause package, not the live VM disk.

For SDK snapshot / rollback / clone APIs see [Snapshot, Rollback & Clone](./snapshot-rollback-clone.md). This page covers the cross-node restore conditions, scheduler rules, and CLI fields.

---

## Conditions for cross-node restore

Resume / FromSnap landing on a node other than the origin is **not** the default. All of the following must hold.

### S3 backend, chosen when you build the template

The sandbox must already be running on the S3 backend. You **cannot switch backends later**. Declare it when you **create the template**; that choice is inherited and locked for every derived object (pause packages, snapshots, and sandboxes created from those snapshots).

- Only `backend=s3` templates / sandboxes upload the package to shared S3 and can restore cross-node. `xfs` templates cannot.
- Inheritance: `template(s3)` → `sandbox(s3)` → `pause package(s3)` / `snapshot(s3)` → `sandbox created from snapshot(s3)`. You cannot change the chain to `xfs` mid-way, or convert an `xfs` object to `s3`.

> An **xfs ↔ S3 conversion tool** is planned for existing templates and sandboxes. In this version, pick the backend at template-create time.

Create a template on S3:

```bash
# Omit --backend to keep the historical xfs path
cubemastercli tpl create-from-image \
  --image <img> \
  --writable-layer-size 4Gi \
  --backend s3 \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

Confirm `BACKEND` is `s3` with `cubemastercli cubebox template list`. Sandboxes and snapshots created from that template inherit `s3` (see [CLI fields](#cli-fields-for-cross-node-restore)).

### Origin first; cross-node only when the origin cannot schedule

The scheduler (`restoreplace`) **always prefers the origin node**. It leaves that node only when the origin cannot take the job **and** the snapshot is allowed to restore cross-node:

```
┌─────────────────┐   ┌──────────────────┐  yes ┌──────────────────┐
│Resume/FromSnap  │──▶│ origin schedulable?│────▶│ restore on origin │
└─────────────────┘   └────────┬─────────┘      └──────────────────┘
                               │ no
                               ▼
                      ┌──────────────────┐  yes ┌──────────────────┐
                      │ CanCrossNode?    │─────▶│ cross-node       │
                      │ backend=s3 ∧    │      │ (compatible peer)│
                      │ remote=ready ∧  │      └──────────────────┘
                      │ kernel/cpu match│  no
                      └────────┬─────────┘
                               ▼
                      ┌──────────────────┐
                      │ error: cannot    │
                      │ restore cross-   │
                      │ node             │
                      └──────────────────┘
```

In short: if the origin is up and schedulable, restore stays there. If it is gone or unschedulable **and** the snapshot meets the cross-node conditions, restore moves. Otherwise the API fails; it will not pick an incompatible node.

An [isolated](./node-isolation.md) origin is unschedulable, which is the usual way to force a cross-node Resume in tests. A sandbox with a **host-mount** is pinned to the origin (`PinToOrigin`) and will not cross even when `remote_status=ready`.

### The snapshot must be remotely `ready`

Master enforces this from DB state (not from client input):

> `CanCrossNode(backend, remote_status)` is true only when **`backend == s3` and `remote_status == ready`**.

- `backend` must be `s3` so other nodes can fetch the objects from shared S3.
- `remote_status` must be `ready`: Pause / Commit / AppSnapshot have finished exporting rootfs, memory, and metadata. The state machine is
  `pending → inprogress → ready / failed`. **Only `ready` unlocks cross-node restore.**

> **Not-ready snapshots are still usable on the origin.** If `remote_status` is not `ready` (`pending`, `inprogress`, `failed`, or empty on `xfs`), you can still resume or create from the snapshot, but `CanCrossNode` is false and the scheduler will only place the job on the **origin**. Cross-node restore unlocks after `remote_status` becomes `ready`.

`xfs` leaves `remote_status` empty, so `CanCrossNode` is always false — **xfs snapshots restore only on the origin**.

### Target kernel / CPU must match the origin

The target node's kernel and CPU identity must match the origin. Memory state (including CPU registers and feature bits) cannot restore correctly otherwise.

> **Current match policy:** cross-node compatibility currently requires **equality on `cpuid_hash` and `host_kernel_release` only**. Other fields (`cpu_vendor`, `host_kernel_fingerprint`, `kvm_api_version`) are collected and shown but **are not equality gates**. A target with a non-empty `kvm_module_taint` (forced / out-of-tree / unsigned `kvm.ko`) is rejected. Later releases may tighten this; follow the version you run.

Inspect `HostFacts` with `cubeopscli node list --json`:

| JSON field | Meaning | Used in matching today |
|------------|---------|------------------------|
| `cpuid_hash` | CPU feature hash | Yes (equality) |
| `host_kernel_release` | Host kernel release (`uname -r`) | Yes (equality) |
| `host_kernel_fingerprint` | Host kernel fingerprint (release + normalized cmdline) | Display only; may become a gate later |
| `cpu_vendor` | CPU vendor (Intel / AMD / Kunpeng, …) | Display only; may become a gate later |
| `kvm_api_version` | KVM API version | Display only; may become a gate later |
| `kvm_module_taint` | KVM module taint; empty means clean | Non-empty **target** is rejected |

> Because most display fields are not equality-checked automatically, still compare the full `HostFacts` objects of origin and target with `cubeopscli node list --json` before relying on cross-node restore.

#### How `cpuid_hash` is computed

Cubelet reads `/proc/cpuinfo` on the node and hashes CPU identity plus the feature set with a deterministic SHA-256 digest (prefix `sha256:`). Two hosts hash equal only when identity and features are identical. Inputs:

- **x86**: `vendor_id`, `cpu family`, `model`, `stepping`, `flags` (e.g. `vmx` / `avx2` / `smep` / `nx`)
- **ARM**: `CPU implementer`, `CPU architecture`, `CPU variant`, `CPU part`, `CPU revision`, `Features`

> Only the first logical CPU is hashed (the fleet is assumed homogeneous). `flags` / `Features` are sorted before hashing, so kernel export order does not matter. Heterogeneous hosts (big.LITTLE, Intel P+E) can hash equal even when secondary cores differ.

---

## Configuring the S3 backend

The S3 path in CubeSandbox is **on by default**. You need a ready **S3lvol** service; there is no extra feature flag.

Configure your S3 endpoint, bucket, and credentials per [CubeS3lvol README](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md). Each compute node reads `/data/cubelet/cos.cfg`. The WAL / journal image lives at `/data/cubelet/rcow/wal_bdev.img`. The one-click installer creates that image at the default size; do not copy it between nodes.

---

## CLI fields for cross-node restore

`cubemastercli` adds `backend` / `remote_status` / `origin_node` columns, and `--backend` on template create. Node list and isolate live on `cubeopscli` (CubeOps, default port `3010`); see [Node Isolation](./node-isolation.md) and [CLI Tools](./cli-tools.md).

### `cubebox list`

Two extra columns show whether a sandbox uses S3 and whether its pause package has synced:

| Column | Meaning |
|--------|---------|
| `backend` | CoW backend (`xfs` / `s3`); `xfs` prints `-` |
| `remote` | Pause-package `remote_status` (`pending` / `inprogress` / `ready` / `failed`); non-S3 prints `-` |

```bash
cubemastercli cubebox list --all
```

Non-paused rows sort by create time descending; paused rows come last and include `pause_snap`. After a successful Resume those columns return to `-`.

### `cubebox snapshot list` / `snapshot info`

| Field | Meaning |
|-------|---------|
| `backend` | CoW backend (`xfs` / `s3`); printed `backend` falls back to historical `storage_backend` |
| `remote_status` | S3 sync state; empty on `xfs` |
| `origin_node_id` / `origin_node_ip` | **Node that created the snapshot** (the “origin” for restore) |
| `replicas` table (`NODE_ID` / `NODE_IP` / `STATUS` / `PHASE` / `SPEC` / `ERROR`) | Per-node replica status |

```bash
cubemastercli cubebox snapshot list
cubemastercli cubebox snapshot info --snapshot-id <snapshot-id>
```

### `cubebox template list` / `template info`

The template list adds a `BACKEND` column; `template info` prints `backend: <xfs|s3>`. That value is the default CoW backend for sandboxes and snapshots created from the template.

```bash
cubemastercli cubebox template list
cubemastercli cubebox template info <template-id>
```

### `tpl create-from-image --backend xfs|s3`

```bash
# Declare the backend at template create; omit to keep historical xfs
cubemastercli tpl create-from-image \
  --image <img> \
  --writable-layer-size 4Gi \
  --backend s3
```

> The backend is fixed at **template / sandbox create**. Snapshot create does **not** take a backend flag; it always uses the persisted backend.

### `cubeopscli node list`

The default table shows health and isolation. HostFacts are in JSON:

```bash
cubeopscli --address 127.0.0.1 --port 3010 node list
cubeopscli --address 127.0.0.1 --port 3010 node list --json
```

`HostFacts` keys are described in [Target kernel / CPU must match the origin](#target-kernel--cpu-must-match-the-origin). Before a cross-node restore, confirm `cpuid_hash` and `host_kernel_release` match, and review the rest of HostFacts.

---

## Benchmarks

> Times in ms. Values below are placeholders pending measured results.

### Cold start

**Method:** start 50 sandboxes in total, clean up after each round, report per-instance cold-start latency.

| Concurrency | xfs avg | xfs p95 | s3 avg | s3 p95 |
|-------------|---------|---------|--------|--------|
| 1           | —       | —       | —      | —      |
| 5           | —       | —       | —      | —      |

### Snapshot / Pause / Resume

- **Create snapshot / share snapshot:** 10 runs each; report avg and p95.
- **Create from snapshot:** concurrency 1 and 5; 50 starts per tier; clean up after each round.

| Operation | xfs avg | xfs p95 | s3 local avg | s3 local p95 | s3 cross-node avg | s3 cross-node p95 |
|-----------|---------|---------|--------------|--------------|-------------------|-------------------|
| Create snapshot | — | — | — | — | N/A | N/A |
| Share snapshot (upload to shared S3; xfs has no step) | N/A | N/A | — | — | N/A | N/A |
| Create from snapshot (concurrency 1) | — | — | — | — | — | — |
| Create from snapshot (concurrency 5) | — | — | — | — | — | — |

Existing XFS cold-start and snapshot numbers: [Performance Benchmark](./performance-benchmark.md).

---

## Known limitations

1. **Deleting an S3 snapshot may report success while objects are still busy.** Snapshot-object delete that the data plane rejects as `busy` is currently treated as success; S3lvol finishes cleanup asynchronously. A successful delete RPC does not mean the objects are gone immediately. Sandbox **volume** delete failures are still surfaced.

2. **DB / filesystem layout changed vs pre-0.7.0; migration is tested from 0.6.0 only.** Table and on-disk layout differ from versions before 0.7.0. The new release adapts older data for cleanup, but that path is **tested against 0.6.0**. If adaptation fails, delete leftover snapshot files and the matching DB rows by hand.

3. **Snapshots of sandboxes with a volume or host-mount are not supported.** Commit / snapshot of a sandbox that mounts a plugin volume or host-mount fails in this version. A host-mount sandbox that is paused still resumes only on the origin.

4. **A sandbox that just resumed cross-node, or was just created from a snapshot, cannot immediately Pause or Snapshot.** For a short window after cross-node Resume / FromSnap the S3 objects may still be decoupling; creating another snapshot fails until that settles. This is still being iterated in S3lvol.

---

## See also

- [Snapshot, Rollback & Clone](./snapshot-rollback-clone.md)
- [Sandbox Lifecycle](./lifecycle.md)
- [Creating Templates from OCI Images](./tutorials/template-from-image.md)
- [Node Isolation](./node-isolation.md)
- [CubeS3lvol README](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md)
