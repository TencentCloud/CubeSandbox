# S3-Compatible Volume Plugin

Bring any S3-compatible object store (AWS S3, Tigris, Cloudflare R2, MinIO, …) into CubeSandbox as a **lifecycle-persistent volume** for sandboxes: create a Volume → mount it in a sandbox → read/write → destroy the sandbox, the data stays → remount it next time.

> **This page is an operator reference** for those who need to **deploy the plugin manually / connect external S3**.
>
> **If you're on the default install (one-click or Helm defaults)** — the installer already started MinIO, installed the plugin, and wrote the credentials. **You don't need this page** — see the user tutorial: [S3 Volumes](../../../docs/guide/s3-volume.md).

中文文档：[README.zh.md](README.zh.md)

---

## What this is

CubeSandbox sandboxes are ephemeral by default — data is lost when they're killed. The Volume plugin gives a sandbox a **user-scoped persistent volume**: created/mounted/unmounted/deleted via the e2b-compatible `/volumes` API, backed by object storage. This plugin is the S3-compatible backend implementation.

The plugin is built entirely on standard tooling — **s3fs** for the mount and the **AWS CLI** for the control plane. Nothing is vendor-specific: the backend is just an `ENDPOINT` in the config file.

**Relationship to the COS plugin:** modelled on the COS plugin, but the backend is any S3-compatible endpoint instead of Tencent Cloud COS, and the mount driver s3fs supports both `amd64` and `arm64` (cosfs is `amd64`-only). The two can coexist in one cluster (the default install registers both `cos` and `s3`).

> **Version requirement:** Cube platform **≥ 0.6.0**, Python SDK **`cubesandbox` ≥ 0.6.0**.
> Protocol and Hook details: [Volume Plugin framework](../../../docs/guide/volume-plugin.md).

---

## Do you need this page?

| Your situation | Go to |
|----------------|-------|
| Default install (one-click / Helm defaults), want to use S3 Volume | [User tutorial](../../../docs/guide/s3-volume.md) — just use the SDK, no config |
| Want to swap the bundled MinIO for external S3 | This page from [§2](#2-install-plugin-and-credentials) — just edit `volume-s3.conf` |
| Deploying the S3 Volume plugin from scratch | This whole page |

---

## Prerequisites

| Item | Description |
|------|-------------|
| Running Cube cluster | At least **CubeMaster**, **Cubelet**, **CubeAPI** (port usually `3000`) |
| Sandbox template | A `templateID` (see [§7](#7-verify-with-the-sdk)) |
| S3-compatible storage | A bucket, and an access key pair with read/write permission on it (auto-created if missing) |
| Local access | `sudo` on CubeMaster / Cubelet hosts to install software, edit config, restart services |

**Single-machine dev:** CubeMaster and Cubelet on one host — install deps once.
**Multi-node:** see the table in [§1](#1-install-dependencies).

---

## 1. Install dependencies

### Which machine?

| Tool | Install on | Purpose (Hook) |
|------|------------|----------------|
| **[s3fs](https://github.com/s3fs-fuse/s3fs-fuse)** | **Cubelet** | attach / detach (FUSE mount) |
| **AWS CLI v2** | **CubeMaster** | create / destroy (volume prefix) |
| **jq** | **CubeMaster** and **Cubelet** | binary plugin stdout JSON |

### Option A: install script

**Cubelet node:**

```bash
sudo ./install-deps.sh --s3fs --jq
```

**CubeMaster node:**

```bash
sudo ./install-deps.sh --aws --jq
```

**Single machine** (both roles on one host):

```bash
sudo ./install-deps.sh --all
```

Check without installing: add `--check-only`.

### Option B: manual install

```bash
# Cubelet — Debian/Ubuntu
sudo apt-get install -y s3fs jq
# Cubelet — RHEL/CentOS (needs EPEL)
sudo yum install -y epel-release && sudo yum install -y s3fs-fuse jq

# CubeMaster — AWS CLI v2 (use aarch64 on ARM64 hosts)
curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o awscliv2.zip
unzip -q awscliv2.zip && sudo ./aws/install
```

### Verify install

**Cubelet — s3fs**

```bash
ls /dev/fuse && echo "FUSE ok"
s3fs --version | head -1
```

Both must succeed; a missing `/dev/fuse` breaks attach.

**CubeMaster — AWS CLI, against your bucket**

```bash
AWS_ACCESS_KEY_ID=xxx AWS_SECRET_ACCESS_KEY=xxx AWS_REGION=<region> \
  aws s3 ls s3://my-cube-volumes/ --endpoint-url <your-endpoint>
```

An empty listing (exit 0) is success. `InvalidAccessKeyId` or `AccessDenied` means the key pair lacks read/write permission on the bucket.

---

## 2. Install plugin and credentials

Copy the plugin into both `plugin/` directories:

```bash
PREFIX=/usr/local/services/cubetoolbox
sudo install -m 0755 binary/cube-volume-s3.sh \
  "$PREFIX/CubeMaster/plugin/cube-volume-s3"
sudo install -m 0755 binary/cube-volume-s3.sh \
  "$PREFIX/Cubelet/plugin/cube-volume-s3"
sudo install -m 0600 volume-s3.conf.example \
  "$PREFIX/CubeMaster/plugin/volume-s3.conf"
sudo install -m 0600 volume-s3.conf.example \
  "$PREFIX/Cubelet/plugin/volume-s3.conf"
```

Then edit `volume-s3.conf` on each node:

| Field | Description | Required |
|-------|-------------|----------|
| `ACCESS_KEY_ID` | Access key ID | yes |
| `SECRET_ACCESS_KEY` | Secret access key | yes |
| `BUCKET` | Bucket holding all volumes | yes |
| `ENDPOINT` | S3-compatible endpoint URL (see table below) | yes |
| `REGION` | SigV4 signing region; default `us-east-1` | no |
| `S3FS_EXTRA_OPTS` | Extra s3fs mount options, whitespace-separated (e.g. `-ouse_path_request_style` for MinIO). Multi-option values are auto-quoted and safe to `source`. | no |

Common backends:

| Provider | `ENDPOINT` | `REGION` |
|----------|-----------|----------|
| AWS S3 | `https://s3.<region>.amazonaws.com` | the bucket's region |
| Tigris | `https://t3.storage.dev` | `auto` |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` | `auto` |
| MinIO | `http://<minio-host>:9000` | any value |

The config must be root-owned and mode `600` — it holds a secret in plaintext and is `source`d by the plugin:

```bash
sudo chown root:root "$PREFIX/CubeMaster/plugin/volume-s3.conf" "$PREFIX/Cubelet/plugin/volume-s3.conf"
sudo chmod 600 "$PREFIX/CubeMaster/plugin/volume-s3.conf" "$PREFIX/Cubelet/plugin/volume-s3.conf"
```

Mount base is **not** set here — Cubelet passes it on attach (default `/data/cube-shared/volume`; see [§4](#4-configure-cubelet)).

---

## 3. Configure CubeMaster

Edit CubeMaster config (common path: `/usr/local/services/cubetoolbox/CubeMaster/conf.yaml`). Add the **Controller** plugin (Create / Destroy):

```yaml
volume_plugins:
  - name: s3
    type: binary
    binary_path: /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-s3
```

`name: s3` is the API/SDK **`driver`**. When `Volume.create("x")` omits the driver, the **first** entry in the list is used — the default install now lists `s3` first, so omitting driver routes to S3.

---

## 4. Configure Cubelet

Edit Cubelet config (common path: `/usr/local/services/cubetoolbox/Cubelet/config/config.toml`).

Confirm the mount parent (optional; default shown):

```toml
[plugins."io.cubelet.internal.v1.storage"]
  volume_plugin_base_dir = "/data/cube-shared/volume"
```

Add the **Node** plugin (Attach / Detach):

```toml
[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
  name        = "s3"
  type        = "binary"
  binary_path = "/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-s3"
```

**`name` must match CubeMaster** (both `s3` here). The plugin returns `host_path` as `<volume_plugin_base_dir>/s3-<volumeID>`, which satisfies the framework's requirement that `host_path` live inside `volumeBaseDir`.

---

## 5. Restart services and verify

```bash
sudo systemctl restart cube-sandbox-cubemaster
sudo systemctl restart cube-sandbox-cubelet
sudo systemctl restart cube-sandbox-cube-api

sleep 5
systemctl is-active cube-sandbox-cubemaster cube-sandbox-cubelet cube-sandbox-cube-api
```

**Verify plugins loaded:**

```bash
grep -aF '[volume] registered' /data/log/CubeMaster/cubemaster-req.log | tail -5
grep -aF '[plugin_volume] initialized' /data/log/Cubelet/Cubelet-req.log | tail -5
```

Expected:

```text
[volume] registered binary plugin "s3" at /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-s3
[plugin_volume] initialized binary plugin "s3" at /usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-s3
```

**Manual attach test** (on the Cubelet node):

```bash
/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-s3 \
  --op attach \
  --sandbox-id test-sandbox \
  --namespace default \
  --volume-id test-vol \
  --ref-count 0 \
  --volume-base-dir /data/cube-shared/volume
```

Success: one JSON line on stdout with `"host_path":"/data/cube-shared/volume/s3-test-vol"` and `"error":""`.

Clean up after the manual test:

```bash
/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-s3 \
  --op detach --sandbox-id test-sandbox --namespace default \
  --volume-id test-vol --ref-count 0 \
  --metadata '{"mount_dir":"/data/cube-shared/volume/s3-test-vol"}'
```

---

## 6. Prepare SDK environment

On your **dev machine** (must reach CubeAPI):

```bash
pip install 'cubesandbox>=0.6.0'

export CUBE_API_URL=http://<cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# Required for remote sandbox I/O on mounted volumes (data plane via CubeProxy)
export CUBE_PROXY_NODE_IP=<cubeproxy-or-cubelet-node-ip>

# When cluster auth is enabled:
# export CUBE_API_KEY=<your-key>
```

---

## 7. Verify with the SDK

```python
from cubesandbox import Sandbox, Volume

# ① Create Volume (bucket gets the s3fs directory object volumes/<id>/)
vol = Volume.create("my-data", driver="s3")
print("volume_id:", vol.volume_id)

# ② Create sandbox with mount
with Sandbox.create(volume_mounts={"/workspace": vol}) as sb:
    sb.files.write("/workspace/hello.txt", "from S3 volume")
    print(sb.files.read("/workspace/hello.txt"))

# ③ Exit with → sandbox destroyed, volume detached (bucket data remains)

# ④ Delete Volume (bucket prefix removed — irreversible)
Volume.destroy(vol.volume_id)
print("done")
```

**Confirm the object landed in the bucket:**

```bash
source /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-s3.conf
AWS_ACCESS_KEY_ID="$ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$SECRET_ACCESS_KEY" AWS_REGION="${REGION:-us-east-1}" \
  aws s3 ls "s3://$BUCKET/volumes/" --recursive --endpoint-url "$ENDPOINT"
```

**Confirm the s3fs mount inside the Cubelet mount namespace** (while the sandbox runs):

```bash
CPID=$(pgrep -f "cubelet --config" | head -1)
nsenter -t "$CPID" -m -- cat /proc/mounts | grep s3fs
```

### Automated verification

The COS example's [`verify_volume.py`](../cos/verify_volume.py) is driver-agnostic — point it at this driver:

```bash
cd ../cos
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=tpl-xxxx
export CUBE_PROXY_NODE_IP=127.0.0.1
export CUBE_VOLUME_DRIVERS=s3
# The script skips driver names cfs/s3/nfs by default (undeployed in the COS
# demo environment); clear the skip list since s3 IS deployed here:
export CUBE_VOLUME_SKIP_DRIVERS=

python3 verify_volume.py
```

---

## 8. Troubleshooting

| Symptom | Check |
|---------|-------|
| `unknown driver: s3` | CubeMaster `volume_plugins` missing the entry, or not restarted |
| `no plugin registered for driver "s3"` | Cubelet missing the same-name plugin, or not restarted |
| Attach fails, `s3fs mount failed` | `ls /dev/fuse`; credentials and `ENDPOINT` in `volume-s3.conf`; run the manual attach in [§5](#5-restart-services-and-verify) to see the s3fs error |
| Attach fails, s3fs log `NoSuchKey` for `volumes/<id>/` | Create must PUT the trailing-slash directory object (s3fs mkdir). A `.keep` file under the prefix is a different key. Upgrade the plugin if an older copy still writes `.keep`. |
| `put-object` fails: `--body` is not a file | AWS CLI 2.x rejects `--body /dev/null` (character device). The plugin uploads a 0-byte temp file instead; upgrade the plugin if an older copy is still installed |
| `InvalidAccessKeyId` / `SignatureDoesNotMatch` | Key pair wrong, lacks bucket permission, or `REGION` doesn't match what the endpoint expects for SigV4 |
| Bucket name contains dots | s3fs uses virtual-hosted-style addressing by default, which breaks TLS for dotted names. Use a bucket without dots, or set `S3FS_EXTRA_OPTS=-ouse_path_request_style` in `volume-s3.conf` (MinIO usually needs it too) |
| SDK write fails | `CUBE_PROXY_NODE_IP` unset; CubeAPI or template not READY |
| `Volume.create` without driver not using s3 | In the default install `s3` is the first `volume_plugins` entry (the default driver), so omitting driver routes to s3; if it doesn't, check the `volume_plugins` order |

More: [Framework §8 Troubleshooting](../../../docs/guide/volume-plugin.md).

---

## Backend layout

```
<bucket>/volumes/<volumeID>/   ← one prefix per Volume
```

Attach mounts `BUCKET:/volumes/<volumeID>` with s3fs at `/data/cube-shared/volume/s3-<volumeID>/` on the host, which Cubelet then exposes to the microVM over virtiofs.

### Hook behavior (RefCount)

| Hook | Side | refCount | Behavior |
|------|------|----------|----------|
| Create | Controller | — | ensure bucket exists (create if missing), then `aws s3api put-object` writes `volumes/<id>/` (s3fs directory object) |
| Destroy | Controller | — | `aws s3 rm --recursive` removes the prefix |
| Attach | Node | `0` | `s3fs` mount → return `host_path` |
| Attach | Node | `> 0` | Return the existing `host_path`; no second mount |
| Detach | Node | `> 0` | no-op |
| Detach | Node | `0` | `fusermount -u`; **retain** the data in the bucket |

### Design notes

- **One bucket, one prefix per volume.** Matches the COS example. Multi-bucket setups typically run several plugin instances with different `driver` names, or extend `Create` to accept a bucket. The framework only requires Hook protocol and `driver` consistency.
- **Bucket auto-create.** Create runs `head-bucket` first. An existing bucket does **not** need `s3:CreateBucket`; the plugin only creates the bucket when it is missing (typical for bundled MinIO).
- **Credentials never enter the sandbox.** They live in the root-owned, mode-`600` config on CubeMaster/Cubelet; the microVM sees only a filesystem.
- **`private_data` carries the key prefix** from Create to Attach (max 1024 bytes, never returned to SDK clients).
- **Concurrency.** A per-volume `flock` serialises attach/detach for the same volume on a node, so two sandboxes starting at once cannot double-mount.
- **Destroy is irreversible** and removes the whole `volumes/<id>/` prefix. The API's refcount guard (409 on `DELETE /volumes` while any sandbox holds the volume) is what prevents deleting a mounted volume — the realistic hazard is a stale refcount after a node crash, where the cluster-wide count can reach 0 while a node still holds the mount.

---

## Layout

```
examples/volume/s3/
├── install-deps.sh          # deps + checks (s3fs / aws / jq)
├── volume-s3.conf.example
└── binary/
    └── cube-volume-s3.sh    # the plugin (all four hooks)
```

| Doc | Content |
|-----|---------|
| [S3 Volumes (user tutorial)](../../../docs/guide/s3-volume.md) | End-user quick start |
| [Volume Plugin framework](../../../docs/guide/volume-plugin.md) | Protocol, RefCount, Hook semantics |
| [Tigris integration guide](../../../docs/guide/integrations/tigris.md) | End-to-end vendor walkthrough using this plugin |
| [COS example](../cos/README.md) | The reference plugin this one is modelled on |
| [s3fs-fuse](https://github.com/s3fs-fuse/s3fs-fuse) | Mount driver options and behavior |
