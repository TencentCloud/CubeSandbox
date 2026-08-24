# S3 Volumes

Give a sandbox storage that **survives its lifecycle**: create a Volume → mount it in a sandbox → read/write → destroy the sandbox, the data stays → remount it next time. The backend is any S3-compatible object store (AWS S3, Tencent Cloud COS, Cloudflare R2, MinIO, …), and the default CubeSandbox install ships a bundled MinIO as that backend.

> **The default install (one-click / Helm defaults) is already set up.** The installer starts a MinIO and wires the S3 Volume plugin, credentials, and CubeMaster/Cubelet config for you. **You don't need to edit `volume-s3.conf` or install the plugin** — jump straight to [Usage](#usage).
>
> Only if you want to point the backend at **external S3** (AWS S3, Tencent Cloud COS, R2, a self-hosted MinIO, …) do you need [Connecting external S3](#connecting-external-s3).

---

## What it gives you

| Capability | Notes |
|------------|-------|
| Survives sandbox teardown | Volume data stays after the sandbox is killed; remount it next time |
| Shared across sandboxes | One Volume can be mounted by several sandboxes at once |
| Read-only mounts | Each mount can be independently read-only — ideal for sharing models/datasets |
| Any S3-compatible backend | Bundled MinIO by default; swap to AWS S3, Tencent Cloud COS, Cloudflare R2, … |
| Credentials stay on the host | Secrets live only on CubeMaster/Cubelet; the sandbox sees just a filesystem |

---

## Usage

### 1. Install the SDK

```bash
pip install 'cubesandbox>=0.6.0'
```

::: warning Use cubesandbox, not the official e2b SDK
The official e2b Python SDK hardcodes requests to the e2b.cloud backend and **cannot target CubeSandbox**. Use `cubesandbox`.
:::

### 2. Set environment variables

On a dev machine that can reach CubeAPI:

```bash
export CUBE_API_URL=http://<cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# Required for remote sandbox I/O on mounted volumes (data plane via CubeProxy)
export CUBE_PROXY_NODE_IP=<cubeproxy-or-cubelet-node-ip>

# When cluster auth is enabled:
# export CUBE_API_KEY=<your-key>
```

### 3. Create a Volume and mount it

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-data", driver="s3")   # in the default install, omitting driver also uses S3
print("volume_id:", vol.volume_id)

with Sandbox.create(volume_mounts={"/workspace": vol}) as sb:
    sb.files.write("/workspace/hello.txt", "from S3 volume")
    print(sb.files.read("/workspace/hello.txt"))   # → from S3 volume

# The sandbox is gone but the data stays; remount it next time, the file is still there
with Sandbox.create(volume_mounts={"/workspace": vol}) as sb:
    print(sb.files.read("/workspace/hello.txt"))

# Actually delete the Volume (backend data wiped — irreversible)
Volume.destroy(vol.volume_id)
```

- `volume_mounts` is a `{in-sandbox path: Volume}` mapping; mount several at once.
- Destroying a sandbox does **not** delete Volume data; only `Volume.destroy()` wipes it.

### Read-only mount / sharing across sandboxes

```python
from cubesandbox import VolumeMount

vol = Volume.create("shared-dataset", driver="s3")

with Sandbox.create(volume_mounts={"/data": VolumeMount(vol, read_only=True)}) as sb:
    sb.files.read("/data/model.bin")
```

---

## Connecting external S3

The default install uses bundled MinIO. To switch to external S3 (AWS S3, Tencent Cloud COS, R2, a self-hosted MinIO, …), change the "source" for your deploy method — the installer generates `volume-s3.conf` for you, so **do not edit that file by hand** (it gets rewritten).

### one-click deploy

In `.one-click.env` (or `env.example`), turn off the bundled MinIO and fill in `CUBE_S3_*`, then re-run `install.sh` — it writes `volume-s3.conf`:

```bash
CUBE_SANDBOX_MINIO_ENABLED=0
CUBE_S3_ENDPOINT=https://s3.example.com
CUBE_S3_ACCESS_KEY_ID=...
CUBE_S3_SECRET_ACCESS_KEY=...
CUBE_S3_BUCKET=cube-volumes
# CUBE_S3_REGION=us-east-1
# CUBE_S3_S3FS_EXTRA_OPTS=-ouse_path_request_style
```

Compute nodes don't run MinIO; just copy the `CUBE_S3_*` from the control node's `.one-click.env`. See [one-click README · Bundled MinIO vs the S3 volume plugin](https://github.com/TencentCloud/CubeSandbox/blob/master/deploy/one-click/README.md#bundled-minio-vs-the-s3-volume-plugin).

### Helm deploy

In values, turn off chart MinIO and fill in `volumeS3.*` (or point `volumeS3.existingSecret` at an existing Secret); after `helm upgrade` the chart renders the `volume-s3.conf` Secret:

```yaml
minio:
  enabled: false
volumeS3:
  endpoint: https://s3.example.com
  accessKeyId: ...
  secretAccessKey: ...
  bucket: cube-volumes
  # region: us-east-1
  # extraOpts: -ouse_path_request_style
```

See the `volumeS3` section of the chart `values.yaml`.

### Manual deploy from scratch (not one-click / not Helm)

For manually deploying the plugin and hand-writing `volume-s3.conf`, see [`examples/volume/s3/README.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/s3/README.md); for a Tencent Cloud COS walkthrough (bucket, access keys) — using the dedicated `cos` driver plugin, not this one — see [`examples/volume/cos/README.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.md).

### Common backends

| Provider | `ENDPOINT` | `REGION` |
|----------|-----------|----------|
| AWS S3 | `https://s3.<region>.amazonaws.com` | the bucket's region |
| Tencent Cloud COS | `https://cos.<region>.myqcloud.com` | the bucket's region (e.g. `ap-guangzhou`) |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` | `auto` |
| MinIO | `http://<minio-host>:9000` | any value |

---

## FAQ

### How is S3 Volume different from Host Mount?

[Host Mount](./persistent-storage.md) bind-mounts a **pre-existing directory on the node** into a sandbox — node-local, great for sharing data that already lives there. S3 Volume is a **user-scoped persistent volume** (e2b Volume API): created/destroyed on demand by the plugin, managed across sandbox lifecycles, backed by object storage. They complement each other: Host Mount for "a directory already on the node", S3 Volume for "one independent persistent volume per user/task".

### What happens if I omit `driver`? Why does it default to S3?

When `Volume.create(...)` omits `driver`, CubeMaster picks the **first** entry in the `volume_plugins` list. The default install now lists `s3` first, so omitting `driver` routes to S3. Writing `driver="s3"` explicitly is still recommended — it's clearer in multi-backend clusters and avoids behavior silently changing if someone reorders `cos` ahead of `s3`.

### Does data survive sandbox teardown? What's the Volume lifecycle?

Yes. Mount/unmount is driven by a **reference count** — you don't manage mount points yourself:

| Operation | Behavior |
|-----------|----------|
| `Volume.create` | Creates a `volumes/<id>/` prefix in the backend |
| Sandbox mount (refCount 0→1) | Cubelet mounts the prefix with s3fs on the host, exposes it to the microVM |
| Sandbox mount (refCount >0) | Reuses the existing mount, no second mount |
| Sandbox teardown (refCount >1) | Decrements only, no unmount |
| Sandbox teardown (refCount →0) | `fusermount -u` unmounts; **backend data is retained** |
| `Volume.destroy` | Deletes the whole `volumes/<id>/` prefix — irreversible |

### What happens when I delete a Volume? Why do I sometimes get a 409?

`Volume.destroy` wipes the whole `volumes/<id>/` prefix in the backend — **irreversible**. While any sandbox still holds the Volume, the API rejects the call with 409 — destroy those sandboxes first. Note that a stale refcount after a node crash can let the cluster-wide count reach 0 while a node still holds the mount; confirm before deleting.

### Does the bucket have to exist already?

No. Create runs `head-bucket` first; it auto-creates the bucket if missing and reuses an existing one — no `s3:CreateBucket` permission needed when the bucket already exists.

---

## Troubleshooting

| Symptom | Check |
|---------|-------|
| `unknown driver: s3` | CubeMaster `volume_plugins` has no `s3` entry, or not restarted |
| `no plugin registered for driver "s3"` | Cubelet has no same-name plugin, or not restarted |
| Attach fails `s3fs mount failed` | `ls /dev/fuse` on the host; credentials/`ENDPOINT` correct |
| `InvalidAccessKeyId` / `SignatureDoesNotMatch` | Wrong key, missing bucket permission, or `REGION` doesn't match the endpoint's SigV4 region |
| Bucket name has dots | s3fs uses virtual-hosted-style addressing by default, which breaks TLS for dotted names. Use a dot-free name, or set `S3FS_EXTRA_OPTS=-ouse_path_request_style` in the conf (MinIO usually needs it too) |
| SDK write fails | `CUBE_PROXY_NODE_IP` unset; or CubeAPI/template not ready |
| `Volume.create` not using s3 | Omitting `driver` picks the **first** `volume_plugins` entry — in the default install that's `s3`. If your cluster reordered `cos` ahead of `s3`, write `driver="s3"` explicitly |

More: [Volume Plugin Development](./volume-plugin.md), [S3 plugin full walkthrough](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/s3/README.md).
