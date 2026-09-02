# Cross-Node Snapshots (Pause / Resume / Snapshot)

::: tip Deployment scope (v0.7.0)
In this release, CubeS3lvol can only be enabled on compute nodes installed via **one-click (bare-metal) deployment**; Kubernetes (Helm) deployments do not include the s3lvol target, so the cross-node S3 snapshot path is unavailable there.
:::

CubeSandbox persists a sandbox as a **package** of three objects (rootfs / memory / metadata) so you can **Pause**, **Resume**, and take a **Snapshot**:

- **Pause / Resume**: freeze a running sandbox (memory + filesystem) into a pause package, then restore it on the same node or another compatible node.
- **Snapshot**: persist that state as a reusable image. You can create a new sandbox from it (FromSnap) or roll the original sandbox back.

With the default `xfs` backend the package stays on **the node that created it**. Resume and FromSnap must return to that node. If the node is down, isolated, or out of capacity, a paused sandbox cannot be scheduled and a snapshot cannot be started elsewhere.

With the `s3` backend the package is uploaded to **cluster-shared S3** (managed by [CubeS3lvol](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md)). **Any compatible node can fetch it on demand**, which is what makes cross-node Pause / Resume and FromSnap possible. The XFS root disk itself never moves; what migrates is the snapshot / pause package, not the live VM disk.

For SDK snapshot / rollback / clone APIs see [Snapshot, Rollback & Clone](./snapshot-rollback-clone.md). This page covers the cross-node restore conditions, scheduler rules, and CLI fields.

---

## 1. Conditions for cross-node restore

Resume / FromSnap landing on a node other than the origin is **not** the default. All of the following must hold.

### 1.1 S3 backend, chosen when you build the template

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

Confirm `BACKEND` is `s3` with `cubemastercli cubebox template list`. Sandboxes and snapshots created from that template inherit `s3` (see [CLI fields](#4-cli-fields-for-cross-node-restore)).

### 1.2 Origin first; cross-node only when the origin cannot schedule

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

An [isolated](./node-operations.md) origin is unschedulable, which is the usual way to force a cross-node Resume in tests. A sandbox with a **host-mount** is pinned to the origin (`PinToOrigin`) and will not cross even when `remote_status=ready`.

### 1.3 The snapshot must be remotely `ready`

Master enforces this from DB state (not from client input):

> `CanCrossNode(backend, remote_status)` is true only when **`backend == s3` and `remote_status == ready`**.

- `backend` must be `s3` so other nodes can fetch the objects from shared S3.
- `remote_status` must be `ready`: Pause / Commit / AppSnapshot have finished exporting rootfs, memory, and metadata. The state machine is
  `pending → inprogress → ready / failed`. **Only `ready` unlocks cross-node restore.**

> **Not-ready snapshots are still usable on the origin.** If `remote_status` is not `ready` (`pending`, `inprogress`, `failed`, or empty on `xfs`), you can still resume or create from the snapshot, but `CanCrossNode` is false and the scheduler will only place the job on the **origin**. Cross-node restore unlocks after `remote_status` becomes `ready`.

`xfs` leaves `remote_status` empty, so `CanCrossNode` is always false — **xfs snapshots restore only on the origin**.

### 1.4 Target kernel / CPU must match the origin

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

#### 1.4.1 How `cpuid_hash` is computed

Cubelet reads `/proc/cpuinfo` on the node and hashes CPU identity plus the feature set with a deterministic SHA-256 digest (prefix `sha256:`). Two hosts hash equal only when identity and features are identical. Inputs:

- **x86**: `vendor_id`, `cpu family`, `model`, `stepping`, `flags` (e.g. `vmx` / `avx2` / `smep` / `nx`)
- **ARM**: `CPU implementer`, `CPU architecture`, `CPU variant`, `CPU part`, `CPU revision`, `Features`

> Only the first logical CPU is hashed (the fleet is assumed homogeneous). `flags` / `Features` are sorted before hashing, so kernel export order does not matter. Heterogeneous hosts (big.LITTLE, Intel P+E) can hash equal even when secondary cores differ.

---

## 2. Configuring the S3 backend

Cube install ships MinIO as the default S3 service so you can try the feature out of the box.
To point at your own S3 store, follow the [CubeS3lvol README](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md).

---

## 3. Storage requirements on every node

Snapshot objects live in shared S3, but **every node that runs the s3lvol target also needs a local WAL image**. Cross-node restore depends on it: writes to the snapshot are staged on local disk first and flushed to S3 asynchronously, and a node without the image can neither take snapshots nor restore them.

### 3.1 The WAL image

- Path: `/data/cubelet/rcow/wal_bdev.img`
- Logical size: **512 GiB** by default, created as a **sparse** file by `install.sh`
- Created once; the journal / WAL / cache split is fixed at creation and cannot be resized afterwards (only by re-creating the image)

The default 512 GiB is three regions:

| Region | Default size | Purpose |
|--------|-------------|---------|
| Journal | 1024 MiB | records of in-flight writes, replayed when the lvstore is attached |
| WAL | 32768 MiB (32 GiB) | locally staged writes before they are flushed to S3 |
| Chunk cache | 490496 MiB (≈479 GiB) | local cache of recently written chunks |

> The image is **sparse**: provisioning 512 GiB of logical space does not consume 512 GiB of disk up front. Plan physical disk usage around the write working set, not the logical size.

### 3.2 Cluster planning

- **Every node that may restore cross-node needs its own WAL image on local disk** — compute nodes running sandboxes, and control nodes that run the s3lvol target, included.
- The region sizes are set at install time via `RCOW_JOURNAL_MB` / `RCOW_WAL_MB` / `RCOW_CACHE_MB` (one-click install), or the equivalent runtime env in the CubeS3lvol config. Tuning them only matters **before the first start** — the layout is frozen once the image exists.
- The image does not hold snapshot data permanently: it is a write buffer plus a cache. The durable copy is in S3.

---

## 4. CLI fields for cross-node restore

`cubemastercli` adds `backend` / `remote_status` / `origin_node` columns, and `--backend` on template create. Node list and isolate live on `cubeopscli` (CubeOps, default port `3010`); see [Node Operations](./node-operations.md) and [CLI Tools](./cli-tools.md).

### 4.1 `cubebox list`

Two extra columns show whether a sandbox uses S3 and whether its pause package has synced:

| Column | Meaning |
|--------|---------|
| `backend` | CoW backend (`xfs` / `s3`); `xfs` prints `-` |
| `remote` | Pause-package `remote_status` (`pending` / `inprogress` / `ready` / `failed`); non-S3 prints `-` |

```bash
cubemastercli cubebox list --all
```

Non-paused rows sort by create time descending; paused rows come last and include `pause_snap`. After a successful Resume those columns return to `-`.

### 4.2 `cubebox snapshot list` / `snapshot info`

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

### 4.3 `cubebox template list` / `template info`

The template list adds a `BACKEND` column; `template info` prints `backend: <xfs|s3>`. That value is the default CoW backend for sandboxes and snapshots created from the template.

```bash
cubemastercli cubebox template list
cubemastercli cubebox template info <template-id>
```

### 4.4 `tpl create-from-image --backend xfs|s3`

```bash
# Declare the backend at template create; omit to keep historical xfs
cubemastercli tpl create-from-image \
  --image <img> \
  --writable-layer-size 4Gi \
  --backend s3
```

> The backend is fixed at **template / sandbox create**. Snapshot create does **not** take a backend flag; it always uses the persisted backend.

### 4.5 `cubeopscli node list`

The default table shows health and isolation. HostFacts are in JSON:

```bash
cubeopscli --address 127.0.0.1 --port 3010 node list
cubeopscli --address 127.0.0.1 --port 3010 node list --json
```

`HostFacts` keys are described in [1.4 Target kernel / CPU must match the origin](#14-target-kernel--cpu-must-match-the-origin). Before a cross-node restore, confirm `cpuid_hash` and `host_kernel_release` match, and review the rest of HostFacts.

---

## 5. Benchmarks

Times are **milliseconds**. **avg** / **p95** are **per-sandbox** create latency (when that sandbox became `running`), not batch wall time divided by concurrency.

Figures below were measured on 2026-08-25. Numbers depend on hardware, image, and dirty-page load; treat them as a same-cluster xfs vs s3 comparison, not a SLA.

### 5.1 Environment

Two identical Tencent Cloud CVM nodes (nested KVM), one control+compute and one compute-only.

| Item | Value |
|------|--------|
| OS | TencentOS Server 4.4 |
| Kernel | `6.6.69-opencloudos9.cubesandbox.pvm.host` |
| CPU | AMD EPYC 9K65, 16 vCPU, 1 thread/core |
| Memory | 30 GiB |
| Data disk | ~1 TB virtio, XFS on `/data` |

### 5.2 Template

Both backends use the **same** image and sandbox spec. Each template has a replica on **one** compute node (the origin). Local runs isolate the peer so jobs stay on the origin; cross-node FromSnap isolates the origin.

| Item | Value |
|------|--------|
| Image | `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` |
| vCPU / memory | 2000 millicores (2 vCPU) / 2048 MiB |
| Writable layer | 4Gi |
| Probe | port `49983`, path `/health` |
| Backends | `xfs` and `s3`, created with `tpl create-from-image --backend …` |

### 5.3 Method

Keep this method if you re-measure. Do not change table columns or round semantics.

1. **Round cleanup:** start `concurrency` sandboxes, **then kill all of them**, then start the next round. Do not pipeline the next round while the previous sandboxes are still up.
2. **Cold start and create-from-snapshot:** 50 sandbox starts per `(backend, concurrency)` cell. Concurrency 1 → 50 rounds of 1. Concurrency 5 → 10 rounds of 5. Discard one warmup round before measuring.
3. **Create snapshot:** 10 serial runs (create sandbox → `create_snapshot` → kill). S3 **must not** overlap two export requests.
4. **Share snapshot (S3 only):** after `create_snapshot` returns, poll until `remote_status=ready`. That wait is the share time; it is **not** included in “create snapshot”.
5. **Create from snapshot (S3 local):** isolate the peer; origin still has the replica. **S3 cross-node:** wait until the snapshot is `ready`, isolate the origin, create on the peer.
6. XFS has no share step and cannot restore cross-node.

### 5.4 Cold start

Create from the **template** (`Sandbox.create(template=tpl-…)`).

| Concurrency | xfs avg | xfs p95 | s3 avg | s3 p95 |
|-------------|---------|---------|--------|--------|
| 1           | 50.9    | 57.2    | 430.6  | 471.4  |
| 5           | 59.5    | 81.2    | 747.4  | 885.8  |

### 5.5 Snapshot / Pause / Resume

| Operation | xfs avg | xfs p95 | s3 local avg | s3 local p95 | s3 cross-node avg | s3 cross-node p95 |
|-----------|---------|---------|--------------|--------------|-------------------|-------------------|
| Create snapshot | 105.9 | 129.8 | 2314.9 | 2524.9 | N/A | N/A |
| Share snapshot (upload to shared S3; xfs has no step) | N/A | N/A | 5579.0 | 5740.4 | N/A | N/A |
| Create from snapshot (concurrency 1) | 64.5 | 74.3 | 439.0 | 473.3 | 6495.5 | 7322.8 |
| Create from snapshot (concurrency 5) | 80.3 | 94.7 | 732.7 | 902.8 | 12285.1 | 14703.1 |

---

## 6. Known limitations

1. **S3lvol deletes snapshot objects asynchronously, and referenced snapshots are refused.** After you delete an S3 snapshot, CubeS3lvol finishes removing the objects in the background. The delete RPC returning does not mean the objects are gone from S3 immediately.

   A snapshot that is still referenced **cannot be deleted**: CubeS3lvol refuses with `EBUSY` when it is being or has been exported (another node may be reading through it), has more than one clone, or is being decoupled. CubeS3lvol then records a **pending-delete mark** keyed by **(lvstore uuid, lvol uuid)** — not by name, so a same-named snapshot cannot be deleted by mistake. Check `delete_pending` in `rcow_get_lvstores`; `deletable` shows whether it can be deleted right now.

   A different refusal: when the volume is still an **active** NVMe-oF namespace, the delete is refused at the RPC layer (with a hint to run `rcow_deactive_bdev` first). That path does **not** record a mark — it is a precondition the caller can fix immediately, not a blocker to wait out.

   Once the blocker clears (the export is released or expires, the extra clone is deleted, decouple finishes, or the volume is deactivated), you must **retry by hand** on that node:

   ```sh
   # from the CubeS3lvol directory
   test/tools/s3lvol_rpc.py --ls              # check the DEL / PEND columns
   test/tools/s3lvol_rpc.py --retry-pending   # retry every marked snapshot that is deletable now
   ```

   Limits of this mechanism: marks live **only in the s3lvol_tgt process memory** and are lost on restart or lvstore unload; there is **no automatic retry** (no background poller) and **no way to cancel** a recorded mark. The cluster delete path (Cubelet `S3Cow.DeleteByKind`) currently treats a refused snapshot delete as success and does not run `--retry-pending`, so leftover objects must be handled on the node as above. See [Retrying a refused snapshot delete](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md#retrying-a-refused-snapshot-delete---retry-pending) in `CubeS3lvol/README.md`.

2. **DB / filesystem layout changed vs pre-0.7.0; migration is tested from 0.6.0 only.** Table and on-disk layout differ from versions before 0.7.0. The new release adapts older data for cleanup, but that path is **tested against 0.6.0**. If adaptation fails, delete leftover snapshot files and the matching DB rows by hand.

3. **After a cross-node Resume or creating a sandbox from a snapshot, you cannot Pause or take a snapshot again for a short time.** How long depends on the snapshot object size. This will be fixed in the next release.

---

## 7. See also

- [Snapshot, Rollback & Clone](./snapshot-rollback-clone.md)
- [Sandbox Lifecycle](./lifecycle.md)
- [Creating Templates from OCI Images](./tutorials/template-from-image.md)
- [Node Operations](./node-operations.md)
- [CubeS3lvol README](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md)
