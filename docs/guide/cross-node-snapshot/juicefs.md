# JuiceFS service configuration

[JuiceFS](https://juicefs.com/docs/community/introduction/) is an open-source, POSIX-compatible distributed filesystem. It separates data from metadata, storing data in object storage and metadata in a metadata engine. Supported metadata engines include Redis, MySQL, and TiKV, while supported object stores include MinIO, S3, and COS.

CubeSandbox builds its cross-node storage on JuiceFS. See [JuiceFS cross-node storage architecture](/architecture/juicefs-storage) for the design and the [JuiceFS performance benchmark](/blog/posts/2026-11-04-cubesandbox-juicefs-perf-benchmark) for performance results.

::: warning Preview
The JuiceFS storage backend is currently in preview and intended for quick trials and early validation. Configuration options, deployment scripts, and runtime behavior may still change in later releases. The default control-node Redis/MinIO pairing is for trials only; validate in a non-production environment first, and switch to more reliable metadata and object stores as described in [Storage migration](#storage-migration) before production use.
:::

## Prerequisites

- Every node's kernel must support FUSE.
- Every node must be able to access the same JuiceFS metadata engine and object store.

## One-click installation

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

### Storage migration

#### Migrate from MinIO to another object store

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

Then follow [One-click installation](#one-click-installation) to initialize the JuiceFS filesystem on the control node and configure the compute nodes.

Cube does not currently support a seamless switch from object store A to object store B. Select the intended object store during one-click installation, or migrate data according to the JuiceFS official [data synchronization guide](https://juicefs.com/docs/community/guide/sync/).

#### Use a metadata engine other than Redis

`CUBE_JFS_META_URL` can point to MySQL, PostgreSQL, TiKV, or another metadata engine supported by JuiceFS. For example:

```bash
CUBE_JFS_META_URL="mysql://user:@(10.0.0.20:3306)/juicefs"
CUBE_JFS_META_PASSWORD="password"
```

For a new cluster, set this URL and initialize the control node. To migrate an existing volume, stop writes and unmount all clients, use `juicefs dump` to export metadata from the old engine, and use `juicefs load` to import it into an empty new engine. Then update `CUBE_JFS_META_URL` on every node and remount. Object-store credentials are excluded from the dump by default and must be configured again in the new metadata engine. See the JuiceFS official [metadata backup and migration](https://juicefs.com/docs/community/metadata_dump_load/) documentation.

> JuiceFS metadata storage is critical. For production deployments, a Redis cluster is recommended to reduce the risk of data loss.

## Kubernetes Helm deployment

First follow the [Kubernetes Helm install guide](/guide/kubernetes/install) to prepare Helm and the cluster.

```bash
cp deploy/kubernetes/chart/runtime-values.example.yaml runtime-values.yaml
```

JuiceFS is not enabled by default; edit the `runtime-values.yaml` file you pass to Helm:

```yaml
cubeJuiceFS:
  enabled: true
```

Enabling it adds a dedicated Redis for JuiceFS, a format Job that runs once, and a DaemonSet that mounts the filesystem on every compute node.

Then follow the [Helm install](/guide/kubernetes/install#_5-helm-install) section and run the install command for your topology (multi-node / Tencent Cloud TKE / single-node):

```bash
# Standard multi-node install
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system \
  --create-namespace \
  -f runtime-values.yaml \
  --wait \
  --timeout 90m
```

If Cube is already installed but JuiceFS was never enabled, the first upgrade that enables it recreates the Cube Node Pods. Stop or migrate every running sandbox on the compute nodes first, then temporarily set:

```yaml
cubeJuiceFS:
  enabled: true
  lifecycleChangeAck: true
```

After the first `helm upgrade` succeeds, set `lifecycleChangeAck` back to `false` and run the same `helm upgrade` once more.

> The second upgrade only clears the one-time acknowledgement from the Helm release; it does not restart Cube Node, JuiceFS, or the Redis. You can keep `false` afterwards or remove the line from `runtime-values.yaml` — `lifecycleChangeAck` is not a day-to-day JuiceFS setting.

For later changes to the JuiceFS image, storage endpoints, or mount options, see the [Kubernetes upgrade guide](/guide/kubernetes/upgrade#changing-juicefs-enablement-or-storage-configuration).

### Verify the Kubernetes deployment

```bash
kubectl get job,daemonset,pod -n cube-system

kubectl exec -n cube-system daemonset/cube-juicefs -c juicefs -- \
  findmnt -T /data/cubelet/jfs

kubectl exec -n cube-system daemonset/cube-node -c cubelet -- \
  findmnt -T /data/cubelet/jfs
```

Both mount checks must report `fuse.juicefs`. Then create a template with `--backend jfs`; existing XFS templates are not converted automatically.

### Storage migration

By default the chart creates a dedicated JuiceFS Redis and uses the `cube-jfs` bucket in the chart-managed MinIO. This section replaces those two defaults; the external Redis and the external object store can be replaced independently, and configuring both at once is not required.

#### Migrate from MinIO to another object store

Create the object-store Secret first so credentials are not stored in plain text in the Helm values:

```bash
kubectl -n cube-system create secret generic cube-jfs-object \
  --from-literal=access-key='<access-key>' \
  --from-literal=secret-key='<secret-key>'
```

Then add `storage` to `runtime-values.yaml`:

```yaml
cubeJuiceFS:
  enabled: true
  storage:
    type: s3
    bucket: https://<bucket>-<appid>.cos.<region>.myqcloud.com/cube-jfs
    existingSecret: cube-jfs-object
```

`storage.bucket` is the complete JuiceFS `--bucket` value, not just the bucket name. For S3-compatible path-style services, include the bucket in the URL, for example `http://minio.example.com:9000/cube-jfs`.

Cube does not currently support a seamless switch from object store A to object store B. Select the intended object store at installation time, or migrate data according to the JuiceFS official [data synchronization guide](https://juicefs.com/docs/community/guide/sync/).

#### Use a metadata engine other than Redis

> JuiceFS metadata storage is critical. For production deployments, a Redis cluster is recommended to reduce the risk of data loss.

To replace only Redis, first create the password Secret:

```bash
kubectl -n cube-system create secret generic cube-jfs-meta \
  --from-literal=meta-password='<redis-password>'
```

Then add `metadata` to `runtime-values.yaml`:

```yaml
cubeJuiceFS:
  enabled: true
  metadata:
    url: redis://redis.example.com:6379/0
    existingSecret: cube-jfs-meta
    secretRevision: initial
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

## JFS configuration notes

The `CUBE_JFS_*` variables can be adjusted in `.env` / `.one-click.env`. Most of them should be left unchanged; this section covers the ones worth attention.

| Variable | Default | Description |
|---|---|---|
| `CUBE_JFS_MOUNT_PATH` | `/data/cubelet/jfs` | Mount point |
| `CUBE_JFS_CACHE_DIR` | `/data/cubelet/jfs-cache` | Local cache directory holding both the read cache and write staging; put it on the node's NVMe/SSD data disk |
| `CUBE_JFS_CACHE_SIZE_MB` | `102400` (100 GiB) | Upper bound for the read cache |

### Cache directory and sizing

`CUBE_JFS_CACHE_DIR` holds two kinds of data. See [JuiceFS cross-node storage architecture](/architecture/juicefs-storage) for the directory layout:

- Read cache (`raw/`): blocks fetched back from object storage, evicted LRU-style at any time and bounded by `CUBE_JFS_CACHE_SIZE_MB`.
- Write staging (`rawstaging/`): with `--writeback`, dirty blocks not yet uploaded to object storage. They are deleted once uploaded and do not count against `CUBE_JFS_CACHE_SIZE_MB`.

The default `CUBE_JFS_CACHE_SIZE_MB` is 100 GiB, matching the setup used by the [performance benchmark](/blog/posts/2026-11-04-cubesandbox-juicefs-perf-benchmark). It is an **upper bound, not a reservation** — the cache grows on demand:

- If the disk has less than 100 GiB free, mounting still succeeds; the read cache simply grows to whatever is actually available.
- JuiceFS defaults to `--free-space-ratio 0.1`: once free space on the cache filesystem falls below 10%, the client proactively evicts read-cache blocks with an LRU-like 2-random algorithm until free space recovers above the threshold. Evicted blocks must be fetched from object storage again on their next read, so the cache hit rate drops.

  **Note that the 10% threshold is calculated against the total capacity of the data disk hosting the cache directory**: with `/data/cubelet/jfs-cache` on a 1000 GiB `/data` disk, even a cache at its full 100 GiB limit leaves far more than 10% free, so this fallback never fires — the effective bound on the read cache is `CUBE_JFS_CACHE_SIZE_MB`.
- If the disk keeps being filled by data outside the cache (Cubelet data, images, staging), eviction only frees space temporarily: the client evicts repeatedly to hold the threshold, the read cache shrinks, and more and more reads fall back to object storage — a noticeable performance drop.
- When free space drops below half the threshold (5% by default), staging writes are restricted too: JuiceFS uploads staged blocks early to release space, and new writes may fall back to synchronous writes to object storage. Tune `--free-space-ratio` (for example 0.2) via `CUBE_JFS_MOUNT_OPTS` if needed.
- Staging has no separate quota: if the data disk actually fills up, new `--writeback` writes fail outright and operations such as snapshot and pause may error. Always reserve space for staging and other data.

Place the cache directory on a dedicated fast data disk and keep `CUBE_JFS_CACHE_SIZE_MB` within 70%–80% of that disk's free space. On smaller data disks (for example 100–200 GiB), scale the value down proportionally. Read-cache eviction is normally triggered by `CUBE_JFS_CACHE_SIZE_MB` first; `--free-space-ratio` mainly acts as a fallback when the cache disk keeps being filled by other data — most notably a staging backlog. The roomier the cache disk, the later this fallback fires, so also monitor overall cache-disk usage.

## Image distribution

By default, each target node pulls `juicedata/mount:ce-v1.4.1` from the image registry.

For an offline environment, load the image in advance with `docker load`, then set `CUBE_JFS_IMAGE` in the configuration file to the local image name. Kubernetes deployments set `images.juicefs.repository` / `tag` in the values instead.

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

### Kubernetes: the format Job failed on the first install

Wrong metadata or object-storage credentials exhaust the format Job's retries; mount Pods then stay in `Init:CrashLoopBackOff` waiting for a volume that never becomes ready. Fix the credentials, delete the failed Job, and re-run the same `helm upgrade` — the Job is not retried on its own because its name only changes when its spec changes:

```bash
kubectl -n cube-system delete job -l app.kubernetes.io/component=cube-juicefs-format
```

### Kubernetes: the chart-created JuiceFS metadata Secret was deleted

The metadata Secret holds the password of the chart-managed JuiceFS Redis. Helm upgrades keep its current password via a lookup, so never delete it: the chart would generate a new random password while the running Redis still uses the old one, and every new JuiceFS Pod would fail to authenticate. If it happens anyway, set `cubeJuiceFS.metadata.password` to the password the Redis actually uses (read it from the Redis Pod's `/etc/redis/redis-auth.conf` before kubelet syncs the deletion), set `cubeJuiceFS.lifecycleChangeAck: true`, re-run the upgrade, and remove the acknowledgement afterwards.
