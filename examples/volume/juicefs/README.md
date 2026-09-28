# JuiceFS Volume Plugin

**Plugin type:** `binary` · **Driver name:** `juicefs`

中文文档：[README.zh.md](README.zh.md) · Framework: [docs/guide/volume-plugin.md](../../../docs/guide/volume-plugin.md)

Mounts a directory of a [JuiceFS](https://github.com/juicedata/juicefs) file system into sandboxes.
JuiceFS keeps file data in object storage (S3, COS, OSS, GCS, …) and metadata in Redis / PostgreSQL /
MySQL / TiKV, so a volume behaves like a POSIX file system:

- directory listing, rename, append and file locks are fast — git and SQLite work on the volume;
- sandboxes on the same or different nodes see each other's writes immediately;
- data is in your object storage bucket, credentials never enter the sandbox.

Compared with the [S3 plugin](../s3/README.md) (s3fs, one object request per file operation), measured
inside a sandbox on AWS (m7i.xlarge, S3 and PostgreSQL metadata in the same region):

| Operation | s3 | juicefs | juicefs `--writeback` | sandbox local disk |
|---|---|---|---|---|
| 200 small files | 31.6 s | 6.6 s | 1.2 s | 37 ms |
| list a directory | 441 ms | 39 ms | 47 ms | 31 ms |
| git init + commit | 12.4 s | 1.4 s | 589 ms | 62 ms |
| SQLite write | fails | 301 ms | 118 ms | 61 ms |

Use the S3 plugin when a volume only holds files that are written once and read back; use this one
when programs work in the volume directly.

---

## How it works

```
JuiceFS file system (data: object storage, metadata: META_URL)
└── volumes/
    ├── vol-aaa/     ← volume A: juicefs mount --subdir volumes/vol-aaa
    └── vol-bbb/     ← volume B: another juicefs process
```

| Phase | Trigger | Where | Action |
|---|---|---|---|
| **create** | `Volume.create()` | CubeMaster | `mkdir volumes/<id>` through the control-plane mount |
| **attach** | sandbox create | Cubelet | first sandbox on the node: `juicefs mount --subdir volumes/<id>` at `<volume-base-dir>/juicefs-<id>`; later sandboxes reuse it |
| **detach** | sandbox destroy | Cubelet | last sandbox on the node: `juicefs umount --flush` |
| **destroy** | `Volume.destroy()` | CubeMaster | `juicefs rmr volumes/<id>` (goes to the JuiceFS trash if enabled) |

- **Isolation:** each volume is mounted with `--subdir`, so a sandbox never sees the file system root.
- **Control-plane mount:** create / destroy keep one mount of the root at `/run/cube-volume-juicefs/admin`
  on the CubeMaster node. It also runs JuiceFS background jobs (trash cleanup, compaction);
  per-volume mounts use `--no-bgjob`.
- **Credentials:** object storage credentials are stored in the metadata by `juicefs format`, not in this
  plugin. Format without `--access-key` / `--secret-key` to use the node's cloud identity (e.g. an EC2
  instance role). The plugin config holds only the metadata engine address and password.
- **Volume IDs** must match `^[A-Za-z0-9._-]{1,128}$`, and `.` / `..` are refused. That is what CubeAPI
  and CubeMaster accept, so a volume they created can always be attached here; the id is only ever one
  path component under `volumes/`, never an argument.
- **Locks are bounded** (`LOCK_WAIT_SECONDS`, 60 s by default): a lock left held by a killed invocation
  fails that attach with an error instead of blocking every later one forever.

---

## 1. Create the file system

Once, from any machine that can reach the metadata engine:

```bash
export META_PASSWORD='<metadata-engine-password>'
juicefs format 'postgres://juicefs@10.0.0.5:5432/juicefs?sslmode=disable' cube-volumes \
  --storage s3 --bucket https://<bucket>.s3.<region>.amazonaws.com
```

`cube-volumes` is the object key prefix inside the bucket. Run the metadata engine as a managed or
replicated service with backups: if it is unavailable, every volume is. See the
[JuiceFS docs](https://juicefs.com/docs/community/databases_for_metadata) for engine choice.

## 2. Install dependencies and the plugin

On **every CubeMaster and Cubelet node**:

```bash
sudo ./install-deps.sh --all          # juicefs (pinned + checksum verified), fuse, jq
# JUICEFS_VERSION=1.4.2 sudo -E ./install-deps.sh --all   # to move off the pinned release

# This installs on the host, which is what the one-click / systemd deployment runs.
# In the containerized installs (docker, Helm) CubeMaster and Cubelet exec the plugin
# inside their containers, so `juicefs` and this script have to be in those images —
# and, unlike the COS and S3 plugins, the CubeMaster container also needs /dev/fuse and
# mount capability, because create/destroy go through a real mount rather than an
# object-storage API call.

DIR=/usr/local/lib/cube-volume-plugins/juicefs
sudo install -d -m 0755 "$DIR"
sudo install -m 0755 cube-volume-juicefs.sh "$DIR/cube-volume-juicefs"
sudo install -m 0600 volume-juicefs.conf.example "$DIR/volume-juicefs.conf"
sudo vi "$DIR/volume-juicefs.conf"    # META_URL, META_PASSWORD, cache
```

The directory above is outside `/usr/local/services/cubetoolbox` on purpose: an upgrade can replace that
tree and drop third-party plugins ([#1569](https://github.com/TencentCloud/CubeSandbox/issues/1569)). Any absolute path works.

| Field | Description | Required |
|---|---|---|
| `META_URL` | Metadata engine URL used with `juicefs format` | yes |
| `META_PASSWORD` | Metadata engine password, passed via env so it stays out of `ps` | no |
| `CACHE_DIR` | Local cache directory shared by all volumes; default `/var/jfsCache` | no |
| `CACHE_SIZE` | Cache size in MiB; default `102400` | no |
| `MOUNT_OPTS` | Extra `juicefs mount` flags for every mount | no |

## 3. Register the driver

**CubeMaster** — `CubeMaster/conf.yaml`:

```yaml
volume_plugins:
  - name: juicefs
    type: binary
    binary_path: /usr/local/lib/cube-volume-plugins/juicefs/cube-volume-juicefs
```

**Cubelet** — `Cubelet/config/config.toml`:

```toml
    [[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
      name        = "juicefs"
      type        = "binary"
      binary_path = "/usr/local/lib/cube-volume-plugins/juicefs/cube-volume-juicefs"
```

Restart CubeMaster and Cubelet.

## 4. Verify

```bash
P=/usr/local/lib/cube-volume-plugins/juicefs/cube-volume-juicefs
sudo $P --op create  --volume-id smoke --name smoke     # {"token":"","private_data":"volumes/smoke/","error":""}
sudo $P --op attach  --sandbox-id t --namespace default --volume-id smoke --ref-count 0 \
     --volume-base-dir /data/cube-shared/volume          # {"host_path":"/data/cube-shared/volume/juicefs-smoke",...}
echo ok | sudo tee /data/cube-shared/volume/juicefs-smoke/hello.txt
sudo $P --op detach  --sandbox-id t --namespace default --volume-id smoke --ref-count 0 \
     --metadata '{"mount_dir":"/data/cube-shared/volume/juicefs-smoke"}'
sudo $P --op destroy --volume-id smoke
```

With the SDK:

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-project", driver="juicefs")
with Sandbox.create(template="<template-id>", volume_mounts={"/project": vol}) as sb:
    sb.commands.run("cd /project && git init && echo hi > README && git add . && git commit -m init")
Volume.destroy(vol.volume_id)
```

---

## Performance notes

- **Small-file writes.** By default a file is uploaded to object storage when it is closed, about one
  request per small file. `MOUNT_OPTS='--writeback'` uploads in the background and was about 5× faster
  for 200 small files in the measurement above. Data waits in `CACHE_DIR` until uploaded: if the node
  restarts with the cache disk intact, upload resumes after remount; if the cache disk is lost, so is
  unuploaded data. Detach uses `--flush` to wait for uploads.
- **Many small files** (`pip install`, `node_modules`, large clones) are still far slower than local disk;
  keep them on the sandbox's own disk.
- **Memory.** Each mounted volume is one `juicefs` process on the node; budget it when sizing sandbox
  density, or lower `--buffer-size` via `MOUNT_OPTS`.
- **Deletes** go to the JuiceFS trash (`--trash-days` at format time, default 1 day) before objects are
  removed from the bucket.

## Troubleshooting

| Symptom | Check |
|---|---|
| `juicefs admin mount failed` on create | `META_URL` / `META_PASSWORD`; reach the metadata engine from CubeMaster; `/run/cube-volume-juicefs/admin` and `/var/log/cube-volume-juicefs/admin.log` |
| `juicefs mount failed for volume` on attach | `/dev/fuse`; `/var/log/cube-volume-juicefs/<volume-id>.log`; the node's access to the bucket (instance role or keys given to `juicefs format`) |
| `unknown driver: juicefs` | driver registered with the same name on both CubeMaster and Cubelet; plugin path still exists after an upgrade |
| Writes slow | see [Performance notes](#performance-notes) |
