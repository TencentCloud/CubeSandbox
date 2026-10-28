# JuiceFS service configuration

The `jfs` backend stores templates, sandbox disks, memory, and snapshots directly in a shared JuiceFS. Every node mounts the same filesystem, and Cubelet accesses it at `/data/cubelet/jfs`.

[JuiceFS](https://juicefs.com/docs/community/introduction/) is an open-source, POSIX-compatible distributed filesystem. Its FUSE client stores file data in S3, MinIO, or another object store, while directories, inodes, slices, and reference counts live in Redis, MySQL, TiKV, or another metadata engine. Multiple nodes can mount one volume and observe a strongly consistent file view. See the official [technical architecture](https://juicefs.com/docs/community/architecture/) for details.

See [Cube + JuiceFS cross-node storage architecture](/architecture/juicefs-storage) for CubeSandbox's JuiceFS storage design and the [JuiceFS performance benchmark](/blog/posts/2026-10-28-cubesandbox-juicefs-perf-benchmark) for performance results.

## Why CubeSandbox uses it

JuiceFS preserves Cube's file-and-CoW storage model while making it shared:

- **One template serves every node.** The cluster does not rebuild and retain one template copy per compute node.
- **Clones copy metadata only.** Cubelet uses `copy_file_range` for rootfs and memory files. JuiceFS copies slice references and uses Redirect-on-Write for modified regions. See [Clone Files or Directories](https://juicefs.com/docs/community/guide/clone/).
- **One file model covers local and cross-node restore.** Local restore uses the shared file plus local cache; cross-node restore imports through the same mount instead of a separate export format.
- **Hot/cold tiering.** Durable blocks live in object storage and hot blocks stay on node-local SSD/NVMe, so capacity is not limited to one node's XFS disk.
- **Standard storage services.** Metadata can use a dedicated Redis, MySQL, or TiKV, while data can reuse Cube's bundled MinIO with a separate bucket, or use COS and other compatible object stores.

The same-environment comparison in `jfs-data.html` shows advantages in template start, snapshot creation, local restore, local FromSnap, and concurrent cross-node restore. Representative results are shown below to explain the design; they are not an SLA.

| Operation | JuiceFS | CubeS3lvol | Observation |
|---|---:|---:|---|
| Template start, concurrency 1 / 5 | 152 / 231 ms | 341 / 775 ms | About 2–3× faster |
| Create snapshot | 652 ms | 1487 ms | About 2.3× faster |
| Local FromSnap, concurrency 1 / 5 | 137 / 227 ms | 482 / 874 ms | About 3–4× faster |
| Cross-node resume, concurrency 5 | 1796 ms | 3026 ms | Better under concurrency |
| Pause after dirtying 1 GiB | 0.77 s | 4.88 s | About 6× faster |

Single-concurrency cross-node resume is similar between the two backends, and cold caches or guest probes can still produce tails. See [Cube + JuiceFS cross-node storage architecture](/architecture/juicefs-storage) for the full topology, data paths, and trade-offs.

The one-click package installs separate initialization, local mount, and status-check scripts. The client runs in a container using `juicedata/mount:ce-v1.4.1` by default. The one-click build neither builds nor bundles this image.

The current Kubernetes chart does not format or mount JuiceFS. A Kubernetes deployment using `jfs` must provide the same host mount on every Cube Node and make it accessible to Cubelet.

## Prerequisites

Each node needs Docker, `/dev/fuse`, and network access to the same JuiceFS metadata engine and object store. Put the local cache on SSD/NVMe when possible.

`/data/cubelet/jfs` is a separate FUSE mount on each node. Every client uses the same `CUBE_JFS_META_URL` and `CUBE_JFS_META_PASSWORD`, so all nodes see one filesystem. The control node does not need SSH access to compute nodes.

## 1. Initialize on the control node

For a new installation with the bundled Redis and MinIO, copy `env.example` to `.env`, enable JFS, set the metadata password, and run `sudo ./install.sh`:

By default, CubeSandbox uses Redis as the JuiceFS metadata engine and MinIO as the object store. To use different storage services, see [Storage migration](#storage-migration).

```bash
ONE_CLICK_ENABLE_JFS=1
CUBE_JFS_META_PASSWORD="password"

CUBE_JFS_IMAGE="juicedata/mount:ce-v1.4.1"
CUBE_JFS_CONTAINER_NAME="cube-sandbox-jfs"
CUBE_JFS_MOUNT_PATH="/data/cubelet/jfs"
CUBE_JFS_ROOT="cube"
CUBE_JFS_CACHE_DIR="/data/cubelet/jfs-cache"
CUBE_JFS_CACHE_SIZE_MB=102400
CUBE_JFS_MOUNT_OPTS="--writeback"
```

On the control node, the installer derives the metadata URL from `CUBE_SANDBOX_NODE_IP` and the bucket URL from the bundled MinIO endpoint. It persists both effective values in `.one-click.env`. The selected node IP must be reachable from every compute node. Inspect the result with `grep -E '^(CUBE_JFS_META_URL|CUBE_JFS_BUCKET)=' /usr/local/services/cubetoolbox/.one-click.env`.

Set `CUBE_JFS_META_URL` and `CUBE_JFS_BUCKET` explicitly to override these defaults for an external metadata engine, external object store, custom port, or different address. Do not use `127.0.0.1` for a service that compute nodes must reach.

The current one-click topology supports one control node and multiple compute nodes; `ONE_CLICK_CONTROL_PLANE_IP` accepts one IPv4 address. A multi-control-node or VIP deployment must explicitly provide stable external metadata and object-store endpoints.

When the documented Redis URL points to an IPv4 address owned by the control node, `init.sh` automatically starts a dedicated `cube-sandbox-jfs-redis` container with persistent volume `cube-sandbox-jfs-redis-data`, using the URL port and `CUBE_JFS_META_PASSWORD`. Other metadata URLs are treated as externally managed. With bundled MinIO, `init.sh` reuses the generated `CUBE_S3_ACCESS_KEY_ID` and `CUBE_S3_SECRET_ACCESS_KEY`; set `CUBE_JFS_ACCESS_KEY` and `CUBE_JFS_SECRET_KEY` only to override them.

Keep the password out of `CUBE_JFS_META_URL`: the scripts pass `CUBE_JFS_META_PASSWORD` to JuiceFS as `META_PASSWORD`, which avoids exposing it in process arguments and routine service output. Legacy URLs such as `redis://:password@10.0.0.11:6380/0` remain supported when `CUBE_JFS_META_PASSWORD` is unset. Do not configure both forms at the same time.

Protect the credentials and initialize the filesystem:

```bash
sudo chmod 600 /usr/local/services/cubetoolbox/.one-click.env
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/init.sh
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/mount.sh
```

The init script only runs on a control node. It uses `juicefs format --no-update`, creating an unformatted volume while leaving an existing volume unchanged. The mount script enables the systemd service, updates Cubelet's `[cow.jfs]` section, verifies the mount, and restarts Cubelet.

## 2. Mount on each compute node

For a new compute node, set `ONE_CLICK_ENABLE_JFS=1` and copy `CUBE_JFS_META_PASSWORD` from the control node. The required `ONE_CLICK_CONTROL_PLANE_IP` already identifies the control node, so the installer derives `CUBE_JFS_META_URL=redis://<control-plane-ip>:6380/0`; do not repeat that IP in the normal configuration. Set `CUBE_JFS_META_URL` explicitly only for an external metadata service or custom endpoint. Keep the client settings the same, then run `sudo ./install-compute.sh`. Compute nodes do not need the bucket or object-store credentials because format stores them in JuiceFS metadata. Then run:

```bash
sudo chmod 600 /usr/local/services/cubetoolbox/.one-click.env
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/mount.sh
```

Repeat on every compute node. The mount always adds `--no-usage-report --prefetch 16 --max-fuse-io 1M --cache-large-write`. `--writeback` reduces foreground latency, but resources remain non-ready until staged data is uploaded.

The effective JFS switch and paths are persisted in `.one-click.env`. If a later upgrade replaces Cubelet's packaged `config.toml`, the Cubelet start script restores `[cow.jfs]` `enable`, `mount_path`, `root`, and `cache_dir` before starting the process; no manual config edit is required after an upgrade.

## 3. Check from the control node

After all nodes are mounted, run:

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/check.sh
```

The script checks the control node's service, mount, and read/write path, then prints active sessions from `juicefs status`. Confirm every expected node appears. Create a `--backend jfs` template, take a snapshot, wait for `remote_status=ready`, and isolate the origin to test cross-node start. See [Cross-node restore requirements](./requirements.md).

## Node changes

To add a node, install and register it normally, add the metadata password and client settings to its `.one-click.env`, run `/usr/local/services/cubetoolbox/scripts/cube-jfs/mount.sh`, then run `/usr/local/services/cubetoolbox/scripts/cube-jfs/check.sh` on the control node. The metadata URL is derived from `ONE_CLICK_CONTROL_PLANE_IP` unless explicitly overridden. Do not format again.

To remove a node, isolate it and clear its sandboxes as described in [Node operations](/guide/node-operations). Run `sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/mount.sh unmount` on that node, then run `cubeopscli node delete <node-id>` on the control node. Unmounting does not delete shared files; do not directly remove the node directory because snapshots or paused resources may still reference it.

## Storage migration

To use another object storage service, update the configuration file:

```bash
ONE_CLICK_ENABLE_JFS=1

# The existing Redis can be reused; a new volume requires a new, empty metadata address.
CUBE_JFS_META_URL="redis://<redis-host>:6379/0"
CUBE_JFS_META_PASSWORD="<redis-password>"

# Tencent Cloud COS example
CUBE_JFS_STORAGE="cos"
CUBE_JFS_BUCKET="https://<bucket>-<appid>.cos.<region>.myqcloud.com"
CUBE_JFS_ACCESS_KEY="<COS-SecretId>"
CUBE_JFS_SECRET_KEY="<COS-SecretKey>"
```

Then initialize the JuiceFS filesystem on the control and compute nodes as described in [Initialize on the control node](#1-initialize-on-the-control-node) and [Mount on each compute node](#2-mount-on-each-compute-node).

Cube does not currently support a seamless switch from object store A to object store B. Select the intended object store during one-click installation, or migrate data according to the JuiceFS official [data synchronization guide](https://juicefs.com/docs/community/guide/sync/).

For MySQL, PostgreSQL, TiKV, or another supported metadata engine, set its URL in `CUBE_JFS_META_URL`. New volumes can be initialized directly. To migrate an existing volume, stop writes and clients, use `juicefs dump` and `juicefs load` with an empty destination engine, restore object-store credentials, update every node's metadata URL, and remount. See [metadata backup and migration](https://juicefs.com/docs/community/metadata_dump_load/).

## Image distribution

By default, each target pulls `juicedata/mount:ce-v1.4.1` from its registry.

For an offline environment, load the image in advance with `docker load`, then set `CUBE_JFS_IMAGE` in the configuration file to the local image name.

When upgrading the image, use the same pinned version on every node.

## Troubleshooting

### The service does not start

Inspect systemd and container logs:

```bash
journalctl -u cube-sandbox-jfs.service -n 200 --no-pager
docker logs cube-sandbox-jfs
docker logs cube-sandbox-jfs-redis
```

Common causes are a missing or malformed `CUBE_JFS_META_URL`, a missing or incorrect `CUBE_JFS_META_PASSWORD`, unavailable `/dev/fuse`, a stopped Docker daemon, an image pull failure, or unreachable Redis/S3 credentials. Every node must use the same metadata URL and password.

### The container runs but the host has no mount

```bash
findmnt -T /data/cubelet/jfs
docker inspect cube-sandbox-jfs --format '{{.State.Status}}'
```

The mount script creates a self bind and uses `rshared` propagation. A manually started container without `bind-propagation=rshared` may leave the FUSE mount visible only inside its own namespace.

### Cubelet reports `jfs storage is not configured`

Confirm `[cow.jfs] enable = true` and make `mount_path`, `root`, and `cache_dir` agree with `.one-click.env`. Restart Cubelet after editing its configuration:

```bash
sudo systemctl restart cube-sandbox-cubelet.service
```

### `remote_status` remains `inprogress`

With `--writeback`, closed files may still have blocks in the local staging area. Cubelet waits for those blocks to upload before recording `ready`. Check the JuiceFS container logs, object-store connectivity, cache-disk capacity, and upload bandwidth. Do not delete staged cache data manually.

### Restore remains pinned to the origin

Confirm that the resource has `backend=jfs` and `remote_status=ready`, every node uses the same metadata URL and `root`, and the target satisfies the [cross-node restore requirements](./requirements.md). A host mount still pins the sandbox to its origin.
