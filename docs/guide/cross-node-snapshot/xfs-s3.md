# xfs-s3 backend

`backend=xfs-s3` runs sandboxes on the node-local XFS reflink store, exactly like `xfs`, and publishes every **pause package** to an S3-compatible object store as a self-contained checkpoint. When the origin cannot take a Resume, another node downloads the checkpoint and resumes the sandbox there.

Runtime I/O never leaves the local disk. Only pause packages move between nodes: templates and user snapshots of an xfs-s3 sandbox are plain `xfs` and restore only where a replica exists. See [Cross-node migration requirements](./requirements.md) for placement and CPU/kernel compatibility, and [xfs-s3 storage architecture](../../architecture/xfs-s3.md) for the design: checkpoint format, verification, Resume pins and the lineage sweep.

| | `xfs` | `xfs-s3` | `jfs` | `s3` |
|---|---|---|---|---|
| Runtime rootfs I/O | local XFS | local XFS | JuiceFS | CubeS3lvol block device |
| What can cross nodes | nothing | pause packages | pause packages, snapshots | pause packages, snapshots |
| Extra service per node | — | none (object store only) | JuiceFS mount | s3lvol target + WAL image |

## How a pause crosses nodes

1. **Pause (origin).** Before the guest is frozen, Cubelet checks that the node has a usable store and that the sandbox uses nothing a checkpoint cannot carry (see [Restrictions](#restrictions)). After the freeze it seals the package, records an export job and returns. Pause does not wait for the upload.
2. **Export (origin).** The upload runs in the background and survives a Cubelet restart. The checkpoint is committed by writing its fixed `candidate.json` key last, so a reader sees a complete checkpoint or none.
3. **Verify (another node).** Master asks a node other than the origin to vouch for the checkpoint. With `verify = full` it downloads every chunk, checks its SHA-256 and trial-restores the package in an isolated network namespace (`cube-runtime verify-restore`). With `verify = existence` it checks that every chunk is stored with its size. Only then does the pause row turn `remote_status = ready`, and Master pins the verified manifest digest.
4. **Resume (target).** When the origin is down or isolated, Master picks a compatible node and pins the attempt to it. The target downloads the pinned checkpoint, imports the disks into its reflink store and runs the ordinary xfs Resume. The checkpoint is then retired from the store.

A single-node cluster never reaches `ready`: there is no other node to verify the checkpoint. Resume on the origin works as with `xfs`.

With `incremental = on`, each node keeps a reflink **baseline** of the last checkpoint it exported or imported for a sandbox. The next export uploads only the chunks that changed since then, and an import onto a node that holds a baseline downloads only the chunks it lacks.

## Requirements

- Every node keeps `/data/cubelet` on XFS with reflink, as for `xfs`.
- One S3-compatible bucket reachable from every node. The store should honour `If-None-Match: *` on PUT; if it does not, set `conditional_writes = optional` (see below).
- At least two CPU- and kernel-compatible nodes, so a node other than the origin can verify.
- Every Cubelet and CubeMaster runs a release with xfs-s3 support, and the database migration `20261116120000_pause_resume_attempt` (MySQL and PostgreSQL) is applied; CubeMaster applies it with its other migrations at start.
- Every node reads the **same** store configuration: any node may be asked to verify or resume a checkpoint another node wrote.

## Configure the store

Cubelet reads the store from the first of:

1. the file named by `CUBE_XFSS3_CONFIG`;
2. the file named by `RCOW_S3_CFG`;
3. `/data/cubelet/s3.cfg`, the file one-click writes for CubeS3lvol.

The format is `key = value`, one per line. xfs-s3 reads the CubeS3lvol keys and adds its own; checkpoints live under their own prefix, so both backends can share a bucket.

```ini
# Store (shared with CubeS3lvol)
endpoint = s3.example.com
region = us-east-1
buckets = ["cube-s3lvol"]
access_key_id = <id>
secret_access_key = <secret>
path_style = true

# xfs-s3
checkpoint_prefix = cube-xfs-s3
conditional_writes = required
chunk_profile = remote
incremental = on
verify = full
import_verify = full
max_bandwidth_mbps = 0
```

| Key | Default | Meaning |
|---|---|---|
| `checkpoint_prefix` | `cube-xfs-s3` | Key prefix of all checkpoint objects in the bucket. |
| `conditional_writes` | `required` | `required` refuses a store that ignores `If-None-Match` (xfs-s3 pause is then refused before the freeze). `optional` commits on such a store with HEAD, PUT and a byte-for-byte readback. That is not mutual exclusion; it relies on only the origin exporting a pause package. |
| `chunk_profile` | `remote` | Default chunk sizes. `remote` (memory 4 MiB, rootfs 16 MiB) suits an object store with tens of milliseconds per request; `low_latency` (1 MiB / 4 MiB) suits a store on or next to the node, where smaller chunks keep incremental checkpoints small. |
| `memory_chunk_mib`, `rootfs_chunk_mib` | from the profile | 1–256. Override the profile. Chunks of different sizes are never shared, so changing a size makes the next checkpoint of every sandbox a full upload. |
| `export_workers`, `read_workers` | `32` | 1–64. Chunks an export, and a readback or import, move at once; each holds one chunk in memory. `CUBE_XFSS3_EXPORT_WORKERS` / `CUBE_XFSS3_READ_WORKERS` override them on one node. |
| `incremental` | `off` | Reuse unchanged chunks from the sandbox's previous checkpoint and its template's memory. |
| `verify` | `full` | `full`: read every chunk back and trial-restore. `existence`: check presence and size only; corruption in the store or an incompatible target then only shows at Resume, which still checks every chunk's digest. |
| `import_verify` | `full` | How an import checks chunks it takes from the local baseline. `full` reads and hashes them. `fiemap` trusts a chunk whose blocks the clone still shares with a baseline that has not moved since it was saved; faster, but it does not detect media corruption in place. |
| `max_bandwidth_mbps` | `0` (no cap) | Caps one node's checkpoint traffic, uploads and downloads together. Use it to keep every node below its share of a per-account quota. |
| `sweep_requests_per_second` | `30` | 1–1000. Request rate of the background deletion of chunks a newer checkpoint superseded (incremental only). |

Requests that the store throttles (429/503) are retried up to 10 times with backoff from 1 s to 30 s, honouring `Retry-After` up to a minute. Cubelet logs each throttled request at WARN.

## Create a template

```bash
cubemastercli tpl create-from-image \
  --image <img> \
  --writable-layer-size 4Gi \
  --backend xfs-s3 \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

Sandboxes created from the template inherit `xfs-s3`. Their user snapshots are committed as `xfs`.

## Restrictions

A checkpoint carries the sandbox's own disks only. CubeMaster refuses to create an xfs-s3 sandbox with host mounts, `host_dir` or Plugin Volumes. Cubelet refuses, before the guest is frozen, to pause a sandbox that has:

- host mounts or host paths, `host_dir`, or a directory `sandbox_path`;
- Plugin Volumes;
- more than one disk-backed `empty_dir`;
- a pmem without `discard_writes`.

The sandbox keeps running when the pause is refused.

## Operations

```bash
# Backend and remote state of pause packages
cubemastercli cubebox list --all
cubemastercli cubebox snapshot info --snapshot-id <snapshot-id>
```

- A checkpoint that cannot be verified yet stays pending. CubeMaster logs the verifier's reason as `remote_status xfs-s3 <snapshot-id>: ... retry(<stage>)`, for example `retry(compat)` on a node with another CPU model, and tries another node. A checkpoint fails only for corrupt data that a second readback 30 s later finds again, for a configuration no node can restore, or when two different nodes failed to restore it.
- **Resume pins.** While the outcome of a cross-node Resume is unknown, every Resume of that sandbox goes to the same node, so a lost reply can never start the sandbox twice. If that node is down for good, confirm it cannot run the sandbox any more (powered off, or its Cubelet and VMs stopped), then release the pin:

  ```bash
  cubemastercli cubebox release-resume-pin <sandbox-id>
  ```

  Master refuses while the node still answers. The same action is available as `POST /cube/sandbox/resume-pin/release`.
- **Cleanup.** A checkpoint is deleted from the store after a cross-node Resume and when its pause package is cleaned up; a failed delete is retried every five minutes. Every two minutes Master removes xfs-s3 pause packages that no sandbox references any more, after a ten-minute grace.

## Limitations

- Deleting a sandbox does not delete its incremental lineage in the store (`<checkpoint_prefix>/lineage/<sandbox-id>/`) or its baselines (`objects/checkpoint-base/<sandbox-id>/` in each node's XFS store), and deleting a template does not delete its memory base (`<checkpoint_prefix>/template/<template-id>/`). Remove them manually when space matters.
- A sandbox must not move between Cubelets with and without xfs-s3 support.
- `chunk_profile` must suit the store; measure before choosing `low_latency` for a remote store.
