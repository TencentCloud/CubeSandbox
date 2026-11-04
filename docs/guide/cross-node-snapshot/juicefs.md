# JuiceFS service configuration

[JuiceFS](https://juicefs.com/docs/community/introduction/) is an open-source, POSIX-compatible distributed filesystem. It separates data from metadata, storing data in object storage and metadata in a metadata engine. Supported metadata engines include Redis, MySQL, and TiKV, while supported object stores include MinIO, S3, and COS.

See [JuiceFS cross-node storage architecture](/architecture/juicefs-storage) for Cube's JuiceFS storage design and the [JuiceFS performance benchmark](/blog/posts/2026-10-28-cubesandbox-juicefs-perf-benchmark) for performance results.

> For more details about JuiceFS internals, see its official [technical architecture](https://juicefs.com/docs/community/architecture/).

## Prerequisites

- Every node's kernel must support FUSE.
- Every node must be able to access the same JuiceFS metadata engine and object store.

## Installation and deployment

The one-click installer does not enable JuiceFS storage by default. You must enable it manually.

You can configure it in the `.env` file during one-click installation, or edit Cube's global configuration file, `.one-click.env`, after installation.

```bash
# Create the .env file in the one-click package.
cp env.example .env

# Alternatively, edit /usr/local/services/cubetoolbox/.one-click.env after installation.
vim /usr/local/services/cubetoolbox/.one-click.env
```

### Initialize the control node

Configure and initialize the JuiceFS filesystem on the control node first, then configure each compute node.

By default, Cube uses Redis on the control node as the JuiceFS metadata engine and MinIO on the control node as the object store.

> **This setup is intended only for a quick trial. For production deployment, switch to another metadata engine or object store.** See [Storage migration](#storage-migration) below.

Set `ONE_CLICK_ENABLE_JFS=1`:

```bash
ONE_CLICK_ENABLE_JFS=1
```

Run the following command on the control node to initialize the JuiceFS filesystem:

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/init.sh
```

Then mount the JuiceFS filesystem on the control node:

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/mount.sh
```

### Initialize compute nodes

Edit each compute node's configuration file and set `ONE_CLICK_ENABLE_JFS=1`.

```bash
ONE_CLICK_ENABLE_JFS=1
```

Run the following command on each compute node to mount the JuiceFS filesystem:

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/mount.sh
```

> After you run `mount.sh`, JuiceFS will mount automatically at boot.

### Check from the control node

After all nodes are mounted, run the following script on the control node to check the JuiceFS filesystem mounts:

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/check.sh
```

The output should look similar to this:

```bash
JuiceFS volume
  Name:          cube
  Storage:       s3
  Bucket:        http://<control-node-ip>:9000/cube-jfs
  Active clients: 3
  Used space:    12288 bytes

Active client sessions
  SID  CLIENT HOST   REPORTED ADDRESSES  MOUNT PATH         MOUNT TIME
  ---  ------------  ------------------  -----------------  ------------------------------
  2    a26a3780ac6f  172.17.0.2          /data/cubelet/jfs  2026-10-28T08:46:15.482143729Z
  1    c9af1d5570f4  172.17.0.3          /data/cubelet/jfs  2026-10-28T08:43:29.693002719Z
  3    30e38b8072fe  172.17.0.2          /data/cubelet/jfs  2026-10-28T08:49:31.946778689Z
```

## Node changes

### Add a node

1. Install and register the new Cube compute node as usual.
2. Set `ONE_CLICK_ENABLE_JFS=1` in the new node's `.one-click.env`.
3. Run `sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/mount.sh` on the new node.
4. Run `sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/check.sh` on the control node and confirm that the new session appears.

The control node only needs to run `/usr/local/services/cubetoolbox/scripts/cube-jfs/init.sh` once during initialization. **Do not run it again when adding nodes.**

### Remove a node

First isolate the node and clear its sandboxes as described in [Node operations](/guide/node-operations). Then run the following command on the node to be removed:

```bash
sudo /usr/local/services/cubetoolbox/scripts/cube-jfs/mount.sh unmount
```

Then delete the node record on the control node:

```bash
cubeopscli node delete <node-id>
```

Unmounting a node does not delete data in the shared JuiceFS filesystem. Do not directly delete `CUBE_JFS_ROOT/nodes/<node-name>`; the cluster may still reference templates, snapshots, or paused sandbox data stored there.

## Storage migration

### Migrate from MinIO to another object store

Update the configuration file to use another object store:

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

Then follow [Installation and deployment](#installation-and-deployment) to initialize the JuiceFS filesystem on the control node and configure the compute nodes.

Cube does not currently support a seamless switch from object store A to object store B. Select the intended object store during one-click installation, or migrate data according to the JuiceFS official [data synchronization guide](https://juicefs.com/docs/community/guide/sync/).

### Use a metadata engine other than Redis

`CUBE_JFS_META_URL` can point to MySQL, PostgreSQL, TiKV, or another metadata engine supported by JuiceFS. For example:

```bash
CUBE_JFS_META_URL="mysql://user:@(10.0.0.20:3306)/juicefs"
CUBE_JFS_META_PASSWORD="password"
```

For a new cluster, set this URL and initialize the control node. To migrate an existing volume, stop writes and unmount all clients, use `juicefs dump` to export metadata from the old engine, and use `juicefs load` to import it into an empty new engine. Then update `CUBE_JFS_META_URL` on every node and remount. Object-store credentials are excluded from the dump by default and must be configured again in the new metadata engine. See the JuiceFS official [metadata backup and migration](https://juicefs.com/docs/community/metadata_dump_load/) documentation.

> JuiceFS metadata storage is critical. For production deployments, a Redis cluster is recommended to reduce the risk of data loss.

## Image distribution

By default, each target node pulls `juicedata/mount:ce-v1.4.1` from the image registry.

For an offline environment, load the image in advance with `docker load`, then set `CUBE_JFS_IMAGE` in the configuration file to the local image name.

Only upgrade the JuiceFS image when necessary, and use the same pinned version on every node.

## Troubleshooting

### The service does not start

Inspect systemd and container logs first:

```bash
journalctl -u cube-sandbox-jfs.service -n 200 --no-pager
docker logs cube-sandbox-jfs
docker logs cube-sandbox-jfs-redis
```

Common causes include:

- `CUBE_JFS_META_URL` or `CUBE_JFS_META_PASSWORD` is incorrectly configured in `.one-click.env`, or the values differ between nodes.
- `/dev/fuse` is unavailable on the node, or Docker is not running.
- The `CUBE_JFS_IMAGE` image cannot be pulled.
- The Redis or MinIO/S3 endpoint is unreachable, or the credentials used during formatting are no longer valid.

### The container runs but the host has no mount

```bash
findmnt -T /data/cubelet/jfs
docker inspect cube-sandbox-jfs --format '{{.State.Status}}'
```

The mount script creates a self bind for the mount point and sets `rshared` propagation. Do not bypass the systemd unit by directly running a container without `bind-propagation=rshared`; otherwise, the FUSE mount may only be visible inside the container's namespace.

### Cubelet reports `jfs storage is not configured`

Confirm `[cow.jfs] enable = true`, and check that `mount_path`, `root`, and `cache_dir` match `.one-click.env`. Restart Cubelet after editing its configuration:

```bash
sudo systemctl restart cube-sandbox-cubelet.service
```

### `remote_status` remains `inprogress`

With `--writeback`, closed files may still have blocks in the local staging area. Cubelet waits for those blocks to upload before recording `ready`. Check the JuiceFS container logs, object-store connectivity, cache-disk capacity, and upload bandwidth. Do not manually delete staging data in the cache directory.

### Restore remains pinned to the origin

Confirm that the resource has `backend=jfs` and `remote_status=ready`, every node uses the same metadata URL and `root`, and the target node's CPU and kernel satisfy the [cross-node restore requirements](./requirements.md). A host mount still pins the sandbox to its origin.
