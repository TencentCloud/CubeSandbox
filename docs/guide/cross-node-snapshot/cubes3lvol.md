# CubeS3lvol service configuration

`backend=s3` uses CubeS3lvol to publish pause packages and snapshots to shared S3. This page retains the deployment, WAL planning, benchmark, and troubleshooting details from the original guide. See [Cross-node restore requirements](./requirements.md) for placement and CPU/kernel compatibility.

::: tip Deployment scope
CubeS3lvol is **off by default** in both one-click and Kubernetes deployments.
:::

## Configuring the S3 backend

Cube install ships MinIO as the default S3 service so you can try the feature out of the box.
To point at your own S3 store, follow the [CubeS3lvol README](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md).

### One-click

Enable the service in the bundle's `.env`. The bundled MinIO can reuse its existing credentials:

```bash
ONE_CLICK_ENABLE_S3LVOL=1
CUBE_S3LVOL_BUCKET=cube-s3lvol
```

For external object storage also set `CUBE_S3_ENDPOINT`, `CUBE_S3_ACCESS_KEY_ID`, `CUBE_S3_SECRET_ACCESS_KEY`, and `CUBE_S3_REGION`. Set `CUBE_S3LVOL_PATH_STYLE=true` for MinIO and other path-style services. The installer writes `/data/cubelet/s3.cfg`, creates the WAL, and enables `cube-sandbox-s3lvol.service`.

### Kubernetes (Helm)

Set `cubeS3lvol.enabled=true` — no other configuration is needed by default: the sidecar reuses the chart's built-in MinIO (or the configured `volumeS3`) endpoint and credentials, and writes to its own `cube-s3lvol` bucket, always separate from the S3 volume plugin's `cube-volumes` bucket.

::: warning Recreates the Big Pod
Enabling this switch recreates the Big Pod on the compute nodes. Do it in a maintenance window — ideally before any sandbox is created.
:::

Identity comes from the full Kubernetes node name, hashed to `rcow-<8hex>`. A Pod recreate keeps the same name. `cubeS3lvol.lvsName` pins the same name on every node — do not set it on a multi-node cluster.

Extra settings are only needed when:

- the S3 endpoint is external and path-style — set `cubeS3lvol.s3.pathStyle: true`;
- cores are isolated — set `cubeS3lvol.cpuMask` (default is the last two allowed CPUs).

```yaml
cubeS3lvol:
  enabled: true
  # Optional: override endpoint / keys. Bucket must stay separate from cube-volumes.
  # s3:
  #   endpoint: https://s3.example.com
  #   accessKeyId: ...
  #   secretAccessKey: ...
  #   bucket: cube-s3lvol
```

The first start creates the sparse WAL if it is missing; `journalMB` / `walMB` / `cacheMB` only apply before that file exists. Enabling the sidecar also raises the Big Pod's `terminationGracePeriodSeconds` to at least 180s so `rcow_stop` can disconnect and unload.

---

## Storage requirements on every node

Every node that runs the s3lvol target needs about **2 CPU + 19 GiB RAM**; x86_64 hosts need AVX2 (Haswell). This includes the default 1 GiB whole-object RAM cache (`RCOW_CACHE_HOT_BUFS=1024`); set it to `0` for disk-only caching.

Snapshot objects live in shared S3, but **every node that runs the s3lvol target also needs a local WAL image**. Cross-node restore depends on it: writes to the snapshot are staged on local disk first and flushed to S3 asynchronously, and a node without the image can neither take snapshots nor restore them.

### The WAL image

- Path: `/data/cubelet/rcow/wal_bdev.img`
- Logical size: **512 GiB** by default, created as a **sparse** file by one-click `install.sh` or the Helm sidecar entrypoint on first start
- Created once; the journal / WAL / cache split is fixed at creation and cannot be resized afterwards (only by re-creating the image)

The default 512 GiB is three regions:

| Region | Default size | Purpose |
|--------|-------------|---------|
| Journal | 1024 MiB | records of in-flight writes, replayed when the lvstore is attached |
| WAL | 32768 MiB (32 GiB) | locally staged writes before they are flushed to S3 |
| Chunk cache | 490496 MiB (≈479 GiB) | local cache of recently written chunks |

> The image is **sparse**: provisioning 512 GiB of logical space does not consume 512 GiB of disk up front. Plan physical disk usage around the write working set, not the logical size.

### Cluster planning

- **Every node that may restore cross-node needs its own WAL image on local disk** — compute nodes running sandboxes, and control nodes that run the s3lvol target, included.
- The region sizes are set at install time via `RCOW_JOURNAL_MB` / `RCOW_WAL_MB` / `RCOW_CACHE_MB` (one-click), chart `cubeS3lvol.journalMB` / `walMB` / `cacheMB` (Helm), or the equivalent runtime env. Tuning them only matters **before the first start** — the layout is frozen once the image exists.
- The image does not hold snapshot data permanently: it is a write buffer plus a cache. The durable copy is in S3.

---

## Benchmarks

Times are **milliseconds**. **avg** / **p95** are **per-sandbox** create latency (when that sandbox became `running`), not batch wall time divided by concurrency.

Figures below were measured on 2026-09-21. The S3 object store is the default in-cluster **MinIO** (path-style).
Numbers depend on hardware, image, and dirty-page load; treat them as a same-cluster xfs vs s3 comparison, not a SLA.

### Environment

Two identical Tencent Cloud CVM nodes (nested KVM), one control+compute and one compute-only.

| Item | Value |
|------|--------|
| OS | TencentOS Server 4.4 |
| Kernel | `6.6.69-opencloudos9.cubesandbox.pvm.host` |
| CPU | AMD EPYC 9K65, 16 vCPU, 1 thread/core |
| Memory | 30 GiB |
| Disks | System and data disks ~1 TB each, Tencent Cloud **SSD Cloud Disk** (`CLOUD_SSD`). `/` is XFS; `/data` is ext4. `/data/cubelet` is a bind-mount of the system-disk XFS (one-click cubelet data dir). The xfs/reflink volumes dir and the s3lvol WAL (`wal_bdev.img`; see “The WAL image” above) live there, not on the ext4 `/data` |
| S3 object store | In-cluster MinIO on the **control+compute** node, which is also the template origin. S3 local cells use that node-local endpoint. Cross-node cells isolate the origin so the restore lands on the compute-only node; dest→MinIO still reaches the origin host (isolate only unschedules it) |

### Template

Both backends use the **same** image and sandbox spec. Each template has a replica on **one** compute node (the origin). Local runs isolate the peer so jobs stay on the origin; cross-node FromSnap isolates the origin.

| Item | Value |
|------|--------|
| Image | `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` |
| vCPU / memory | 2000 millicores (2 vCPU) / 2048 MiB |
| Writable layer | 4Gi |
| Probe | port `49983`, path `/health` |
| Backends | `xfs` and `s3`, created with `tpl create-from-image --backend …`. `s3` uses MinIO |

### Method

Keep this method if you re-measure. Do not change table columns or round semantics.

1. **Round cleanup:** start `concurrency` sandboxes, **then kill all of them**, then start the next round. Do not pipeline the next round while the previous sandboxes are still up.
2. **Cold start and create-from-snapshot:** 50 sandbox starts per `(backend, concurrency)` cell. Concurrency 1 → 50 rounds of 1. Concurrency 5 → 10 rounds of 5. Discard one warmup round before measuring.
3. **Create snapshot:** 10 serial runs (create sandbox → `create_snapshot` → kill). S3 **must not** overlap two export requests.
4. **Share snapshot (S3 only):** after `create_snapshot` returns, poll until `remote_status=ready`. That wait is the share time; it is **not** included in “create snapshot”.
5. **Share pause (S3 only):** same poll-to-`ready` as item 4, applied to the pause package after `pause(wait=True)` returns. That wait is the “Share pause” row; it is **not** included in Pause. The published share-pause cells are the c=1 serial samples.
6. **Create from snapshot (S3 local):** isolate the peer; origin still has the replica, and MinIO is on that same node. **S3 cross-node:** wait until the snapshot is `ready`, isolate the origin, create on the compute-only node. Every timer in the tables below **stops at `running`**. Dest already had this template’s guest OS image and kernel sidecar, so the first ~1 Gi artifact fetch is excluded. Lazy / background fetch of the rest of the S3 package after `running` is also excluded. A dest that has never served the template still pays that one-time image fetch.
7. **Create from snapshot (cached):** one discarded FromSnap of the same snapshot on dest first (package already imported), then the same 50 starts as item 2. XFS has no row. The first 50-start cells already discarded a warmup FromSnap, so dest import is warm there too; these rows check that a second 50 does not change the result.
8. **Pause / Resume:** same 50 starts, c=1 / c=5, one warmup. Pause immediately after Create; no extra guest dirty-page load. Pause time is that sandbox’s `pause(wait=True)` from the call until it returns — **not** time spent waiting for another sandbox’s export. In an S3 c=5 round, Pause + share-pause run one sandbox at a time (next `pause()` starts after the previous package is `ready`). Resume time is `Sandbox.connect`. Cross-node Pause is `N/A`: Pause always runs on the origin and is the same as s3 local. Cross-node Resume: Pause on the origin, wait until `ready`, isolate the origin, connect on dest.
9. XFS has no share step and cannot restore cross-node.

### Cold start

Create from the **template** (`Sandbox.create(template=tpl-…)`).

| Concurrency | xfs avg | xfs p95 | s3 avg | s3 p95 |
|-------------|---------|---------|--------|--------|
| 1           | 56.1    | 64.2    | 242.9  | 270.9  |
| 5           | 68.0    | 79.6    | 663.8  | 839.6  |

### Snapshot / Pause / Resume

| Operation | xfs avg | xfs p95 | s3 local avg | s3 local p95 | s3 cross-node avg | s3 cross-node p95 |
|-----------|---------|---------|--------------|--------------|-------------------|-------------------|
| Create snapshot | 97.0 | 118.9 | 2242.2 | 2365.7 | N/A | N/A |
| Share snapshot (upload to shared S3; xfs has no step) | N/A | N/A | 4336.4 | 6267.5 | N/A | N/A |
| Pause (concurrency 1) | 106.2 | 122.7 | 2509.9 | 2709.3 | N/A | N/A |
| Pause (concurrency 5) | 152.7 | 170.4 | 3415.8 | 4154.5 | N/A | N/A |
| Share pause (upload to shared S3; xfs has no step) | N/A | N/A | 5318.8 | 5763.4 | N/A | N/A |
| Resume (concurrency 1) | 153.6 | 171.3 | 410.8 | 448.4 | 836.2 | 920.8 |
| Resume (concurrency 5) | 403.3 | 474.6 | 1755.4 | 2033.7 | 3191.3 | 3837.2 |
| Create from snapshot (concurrency 1) | 63.2 | 75.4 | 253.7 | 301.0 | 729.6 | 788.5 |
| Create from snapshot (concurrency 5) | 93.4 | 118.5 | 706.9 | 887.3 | 3194.4 | 3912.3 |
| Create from snapshot (concurrency 1, cached) | N/A | N/A | 250.2 | 272.8 | 722.5 | 767.6 |
| Create from snapshot (concurrency 5, cached) | N/A | N/A | 709.6 | 910.1 | 2829.3 | 3814.5 |

Cross-node **Resume** and **FromSnap** cells are **warm dest**: the target already had the template guest OS image (~1 Gi) and kernel sidecar (~50 MiB) before timing. Share rows are origin export to `remote=ready` (advertised package ~210 MiB); dest GET below is only bytes read from MinIO between start and `running`, not a download of that whole package.

Cross-node FromSnap / Resume payload (same spec; Snapshot or Pause immediately after Create; **no** extra guest dirty-page load):

| Item | Approx. size |
|------|----------------|
| Sandbox spec | 2 vCPU / 2048 MiB RAM, 4Gi writable layer |
| Origin export (reference advertised) | ~210 MiB / ~230 objects. Memory is ~205 MiB; rootfs and metadata are ~4 MiB each. This is what share waits for, not what dest pulls before `running` |
| Dest GET from MinIO | ~10–20 MiB **from FromSnap/Resume start until `running`**, for one restore (`dest_direct_get_bytes`). Import registers a lease and activates; it does **not** pull the full ~210 MiB. The guest can keep faulting chunks after `running` (not in the timer), so total transfer is larger |
| Guest OS image (not in the timer) | ~1 Gi `.ext4` plus a ~50 MiB kernel sidecar, already on dest. Not S3 package import |

The **cached** FromSnap rows are a second 50 starts after dest has already imported that snapshot. At this sample size they are indistinguishable from the first 50-start cells (c=1 differs by ~1%). They are kept as a null result: dest-import / dest-cache warmth does not move the wall time. Share-snapshot p95 with n=10 is one slow sample, not a wide distribution.

---

## Known limitations

1. **S3lvol deletes snapshot objects asynchronously, and referenced snapshots are refused.** After you delete an S3 snapshot, CubeS3lvol finishes removing the objects in the background. The delete RPC returning does not mean the objects are gone from S3 immediately.

   A snapshot that is still referenced **cannot be deleted immediately**: CubeS3lvol refuses with `EBUSY` while an export is publishing, has a live lease, has a lease miss younger than the source grace (~60 s from the *start* of that absence), is read by a local esnap clone, has more than one clone, or is being decoupled. CubeS3lvol then records a **pending-delete mark** keyed by **(lvstore uuid, lvol uuid)** — not by name, so a same-named snapshot cannot be deleted by mistake. Check `delete_pending` in `rcow_get_lvstores`; `deletable` shows whether it can be deleted right now (from the last lease poll, not a fresh GET). An idle lease-aware export, and a pre-lease export, do **not** pin: the snapshot delete releases the export internally. A new importer of an already-idle export is unprotected until the source's next 20 s poll, when the source first sees the PUT.

   A different refusal: when the volume is still an **active** NVMe-oF namespace, the delete is refused at the RPC layer (with a hint to run `rcow_deactive_bdev` first). That path does **not** record a mark — it is a precondition the caller can fix immediately, not a blocker to wait out.

   A ~60s poller then completes the delete once the blocker is gone for **leased** exports (the importer stops renewing and the miss is older than grace), extra clones, a finished decouple, an export that was still publishing, or a **pre-lease** export whose delete has already been recorded. An asynchronously failed destroy is left for an explicit retry.

   Marks are persisted to `<prefix>/meta/pending-deletes.json` and restored on attach. The delete RPC returns **before** that PUT; a crash in that window forgets the intent and the delete has to be asked for again. When the blocker is one the poller can finish on its own, the reply is still success, with **`deferred: true`**. A live or still-unknown lease still returns EBUSY; a stale or pre-lease export is released by the delete itself. Withdraw the intent with `rcow_cancel_pending_delete` (`lvol_name` required, `lvs_name` optional); it is idempotent. If the poller has already submitted destroy, cancel only drops the mark and the snapshot may still go away.

   A snapshot of a volume that is still decoupling from an import keeps that external parent; `rcow_create_snapshot` then returns **`decouple_cancelled: true`**.

   `--retry-pending` is still the operator tool for failed destroys and for not waiting for the poller:

   ```sh
   # from the CubeS3lvol directory
   test/tools/s3lvol_rpc.py --ls              # check the DEL / PEND columns
   test/tools/s3lvol_rpc.py --retry-pending   # retry every marked snapshot that is deletable now
   ```

   The cluster delete path (Cubelet `S3Cow.DeleteByKind`) currently treats a refused snapshot delete as success and does not run `--retry-pending`, so leftover objects on that path must be handled on the node as above. See [Retrying a refused snapshot delete](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md#retrying-a-refused-snapshot-delete---retry-pending) in `CubeS3lvol/README.md`.

2. **DB / filesystem layout changed vs pre-0.7.0; migration is tested from 0.6.0 only.** Table and on-disk layout differ from versions before 0.7.0. The new release adapts older data for cleanup, but that path is **tested against 0.6.0**. If adaptation fails, delete leftover snapshot files and the matching DB rows by hand.

3. **After a cross-node Resume or creating a sandbox from a snapshot, you cannot Pause or take a snapshot again for a short time.** How long depends on the snapshot object size. This will be fixed in the next release.

---

## See also

- [Snapshot, Rollback & Clone](../snapshot-rollback-clone.md)
- [Sandbox Lifecycle](../lifecycle.md)
- [Creating Templates from OCI Images](../tutorials/template-from-image.md)
- [Node Operations](../node-operations.md)
- [CubeS3lvol README](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md)
