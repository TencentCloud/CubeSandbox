# S3-Compatible Volume Plugin

Bring any S3-compatible object store (AWS S3, Tencent Cloud COS, Cloudflare R2, MinIO, …) into CubeSandbox as a **lifecycle-persistent volume** for sandboxes: create a Volume → mount it in a sandbox → read/write → destroy the sandbox, the data stays → remount it next time.

> **This page is an operator reference** for those who need to **deploy the plugin manually / connect external S3**.
>
> **If you're on the default install (one-click or Helm defaults)** — the installer already started MinIO, installed the plugin, and wrote the credentials. **You don't need this page** — see the user tutorial: [S3 Volumes](../../../docs/guide/s3-volume.md).

中文文档：[README.zh.md](README.zh.md)

---

## What this is

CubeSandbox sandboxes are ephemeral by default — data is lost when they're killed. The Volume plugin gives a sandbox a **user-scoped persistent volume**: created/mounted/unmounted/deleted via the e2b-compatible `/volumes` API, backed by object storage. This plugin is the S3-compatible backend implementation.

The plugin is a single static Go binary with a built-in S3 client, so the control plane needs no S3 command line tool; the data plane uses standard **s3fs** for the mount. Nothing is vendor-specific: the backend is just an `ENDPOINT` in the config file.

**Relationship to the COS plugin:** modelled on the COS plugin, but the backend is any S3-compatible endpoint instead of Tencent Cloud COS, and the mount driver s3fs supports both `amd64` and `arm64` (cosfs is `amd64`-only). The two can coexist in one cluster (the default install registers both `cos` and `s3`).

> **Version requirement:** Cube platform **≥ 0.6.0**, Python SDK **`cubesandbox` ≥ 0.6.0**.
> Protocol and Hook details: [Volume Plugin framework](../../../docs/guide/volume-plugin.md).

---

## Do you need this page?

| Your situation | Go to |
|----------------|-------|
| Default install (one-click / Helm defaults), want to use S3 Volume | [User tutorial](../../../docs/guide/s3-volume.md) — just use the SDK, no config |
| Want to swap the bundled MinIO for external S3 | This page from [§2](#2-install-plugin-and-credentials) — just edit `volume-s3.conf` |
| Each user brings their own bucket / credentials, no redeploy | [Dynamic buckets and credential references](#dynamic-buckets-and-credential-references) |
| Deploying the S3 Volume plugin from scratch | This whole page |

---

## Prerequisites

| Item | Description |
|------|-------------|
| Running Cube cluster | At least **CubeMaster**, **Cubelet**, **CubeAPI** (port usually `3000`) |
| Sandbox template | A `templateID` (see [§7](#7-verify-with-the-sdk)) |
| S3-compatible storage | A bucket, and an access key pair with read/write permission on it (legacy volumes auto-create the bucket if missing; dynamic buckets must already exist and their endpoint must support conditional PUT — see [Dynamic buckets](#dynamic-buckets-and-credential-references)) |
| Local access | `sudo` on CubeMaster / Cubelet hosts to install software, edit config, restart services |

**Single-machine dev:** CubeMaster and Cubelet on one host — install deps once.
**Multi-node:** see the table in [§1](#1-install-dependencies).

---

## 1. Install dependencies

### Which machine?

| Tool | Install on | Purpose (Hook) |
|------|------------|----------------|
| **[s3fs](https://github.com/s3fs-fuse/s3fs-fuse)** | **Cubelet** | attach / detach (FUSE mount) |
| **jq** | anywhere (optional) | reading plugin output by hand while debugging |

A **CubeMaster-only** node needs nothing from this section: create / destroy talk to the endpoint over HTTP from inside the plugin binary.

### Option A: install script

**Cubelet node:**

```bash
sudo ./install-deps.sh --s3fs
```

**Single machine** (both roles on one host), plus jq for debugging:

```bash
sudo ./install-deps.sh --all
```

Check without installing: add `--check-only`.

### Option B: manual install

```bash
# Cubelet — Debian/Ubuntu
sudo apt-get install -y s3fs
# Cubelet — RHEL/CentOS (needs EPEL)
sudo yum install -y epel-release && sudo yum install -y s3fs-fuse
```

### Verify install

**Cubelet — s3fs**

```bash
ls /dev/fuse && echo "FUSE ok"
s3fs --version | head -1
```

Both must succeed; a missing `/dev/fuse` breaks attach.

**CubeMaster — credentials, against your bucket**

The plugin's own `create` hook is the check: it fails loudly on bad credentials, a wrong endpoint or a missing permission.

```bash
/usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-s3 \
  --op create --volume-id preflight-check --name preflight
/usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-s3 \
  --op destroy --volume-id preflight-check
```

Both must print `"error":""` and exit 0. `InvalidAccessKeyId` or `AccessDenied` means the key pair lacks read/write permission on the bucket.

---

## 2. Build and install plugin and credentials

Build the binary. From the repository root (uses the project builder image, so no
local Go toolchain is needed):

```bash
make cube-volume-s3          # -> _output/bin/cube-volume-s3
```

Or with a local Go toolchain (≥ 1.25):

```bash
cd examples/volume/s3 && make    # -> bin/cube-volume-s3
```

One-click release bundles and the container images already ship the compiled
binary at `<prefix>/{CubeMaster,Cubelet}/plugin/cube-volume-s3`.

> **Third-party plugins:** install outside the cubetoolbox tree, otherwise they are reset on Cube upgrade.

Install it into both `plugin/` directories:

```bash
PREFIX=/usr/local/services/cubetoolbox
sudo install -m 0755 _output/bin/cube-volume-s3 \
  "$PREFIX/CubeMaster/plugin/cube-volume-s3"
sudo install -m 0755 _output/bin/cube-volume-s3 \
  "$PREFIX/Cubelet/plugin/cube-volume-s3"
sudo install -m 0600 volume-s3.conf.example \
  "$PREFIX/CubeMaster/plugin/volume-s3.conf"
sudo install -m 0600 volume-s3.conf.example \
  "$PREFIX/Cubelet/plugin/volume-s3.conf"
```

Then edit `volume-s3.conf` on each node:

| Field | Description | Required |
|-------|-------------|----------|
| `ACCESS_KEY_ID` | Access key ID | yes¹˒² |
| `SECRET_ACCESS_KEY` | Secret access key | yes¹˒² |
| `CREDENTIALS` | `static` (default) or `instance_role`¹ | no |
| `BUCKET` | Default and legacy bucket; also the fallback for a descriptor without `bucket` | yes² |
| `ENDPOINT` | S3-compatible endpoint URL (see table below) | yes |
| `REGION` | SigV4 signing region; default `us-east-1` | no |
| `S3FS_EXTRA_OPTS` | Extra s3fs mount options, whitespace-separated (e.g. `-ouse_path_request_style` for MinIO). Multi-option values may be quoted so the file stays `source`-compatible; the plugin strips the quotes. Setting `-ouse_path_request_style` also switches the plugin's own S3 client to path-style addressing. | no |
| `ADDRESSING_STYLE` | `auto` (default), `path`, or `virtual`. Selects the same bucket addressing for the control-plane client and s3fs. If unset, a path-style option in `S3FS_EXTRA_OPTS` implies `path`. An explicit style — including `auto` — together with such an option in the same file is rejected. | no |
| `CREDENTIALS_DIR` | Directory of `<cred_ref>.conf` files; default `/etc/cube/s3-credentials`. See [dynamic buckets](#dynamic-buckets-and-credential-references). | no |
| `PASSWD_DIR` | Directory for the per-volume s3fs passwd files; default `/etc/cube`. | no |

¹Set `CREDENTIALS=instance_role` and remove both keys to use the host's EC2 instance role instead of a static key. The control-plane client then gets credentials from the instance metadata service (IMDS) and s3fs mounts with `-oiam_role=auto`; no passwd file is written, and one left from earlier static keys is removed. Keys together with `instance_role` are an error, and `ENDPOINT` must be an AWS S3 host (`*.amazonaws.com` or `*.amazonaws.com.cn`). Create, destroy and attach fetch credentials once up front and fail with an IMDS error if the host has no role or cannot reach IMDS.

²Both keys and `BUCKET` may be omitted when every volume carries its own `bucket` and `cred_ref` in `private_data`; a volume that then lacks one fails with a clear error. Setting only one of the two keys is always an error. `CREDENTIALS_DIR` and `PASSWD_DIR` can also be set through `CUBE_S3_CREDENTIALS_DIR` / `CUBE_S3_PASSWD_DIR`, which win over the file.

> **Which deployments can use this.** Only the manual install on this page. `deploy/one-click/install.sh`
> refuses a config with `CUBE_S3_ENDPOINT` set and the keys empty, and re-renders `volume-s3.conf` on every
> install and upgrade, so a hand-added `CREDENTIALS` line would not survive; the Helm chart likewise requires
> `volumeS3.accessKeyId` / `secretAccessKey` unless you supply the whole file through `volumeS3.existingSecret`.
>
> **Which identity this is.** The EC2 instance role through IMDS — not the wider AWS credential chain:
> `AWS_ACCESS_KEY_ID` and `~/.aws/credentials` are ignored. (The control-plane client would honour
> `AWS_WEB_IDENTITY_TOKEN_FILE` or `AWS_CONTAINER_CREDENTIALS_*` if they were set in the plugin's environment;
> they are not on a normal host, and s3fs only uses IMDS.) The other backends in the table above (COS, R2,
> MinIO) therefore still need a static key pair.
>
> **Where the role has to exist.** On every CubeMaster *and* Cubelet host: create and destroy run with
> CubeMaster's identity, attach with the node's. Scope it to `BUCKET` — an instance role is usually much
> broader than the bucket-scoped key pair the prerequisites ask for.

Common backends:

| Provider | `ENDPOINT` | `REGION` |
|----------|-----------|----------|
| AWS S3 | `https://s3.<region>.amazonaws.com` | the bucket's region |
| Tencent Cloud COS | `https://cos.<region>.myqcloud.com` | the bucket's region (e.g. `ap-guangzhou`) |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` | `auto` |
| MinIO | `http://<minio-host>:9000` | any value |

The config must be root-owned and mode `600` — it holds a secret in plaintext. The plugin parses it as `KEY=VALUE` lines rather than executing it, and looks for `volume-s3.conf` next to the binary (override with `CUBE_S3_CONFIG`):

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

**Confirm the object landed in the bucket.** Any S3 browser works; [MinIO's `mc`](https://min.io/docs/minio/linux/reference/minio-mc.html) is a single binary and needs no Python:

```bash
source /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-s3.conf
mc alias set cube "$ENDPOINT" "$ACCESS_KEY_ID" "$SECRET_ACCESS_KEY"
mc ls --recursive "cube/$BUCKET/volumes/"
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
| `open config ...: no such file or directory` | `volume-s3.conf` must sit next to the plugin binary, or `CUBE_S3_CONFIG` must point at it |
| `InvalidAccessKeyId` / `SignatureDoesNotMatch` | Key pair wrong, lacks bucket permission, or `REGION` doesn't match what the endpoint expects for SigV4 |
| `bucket "..." does not exist` on a dynamic create | Dynamic buckets are never auto-created: create the bucket first, or fix `bucket` in `private_data` |
| `credential reference "x" is unreadable` | `<x>.conf` must exist in `CREDENTIALS_DIR` on **both** the CubeMaster and Cubelet hosts, root-owned and mode `0600` |
| `prefix "..." is already assigned to volume "..."` | Another volume owns that prefix or a parent/child of it; choose a different prefix or delete the old volume |
| `volume claims registry is not valid JSON` / `unsupported version` | `.cube-sandbox/volume-claims.json` in that bucket is damaged or was written by a newer plugin. Dynamic creates fail closed; repair or remove the object only after confirming no dynamic volume still uses that bucket |
| `does not support conditional PUT (If-Match / If-None-Match)` | The backend cannot do the atomic registry update dynamic volumes require. Use an endpoint that implements conditional PUT (AWS S3, MinIO, R2) or a gateway that does; legacy volumes still work on this endpoint |
| `prefix "..." already contains objects` | Dynamic volumes require an empty prefix, because a rolled-back create would otherwise delete the existing data. Choose an empty prefix, or import the data after the volume exists |
| `refusing to delete prefix "...": no ownership claim records it` | The registry no longer has a claim for this dynamic volume but objects exist under its prefix. Inspect `.cube-sandbox/volume-claims.json` instead of deleting the data by hand |
| `destroy of prefix "..." is already in progress` | Another destroy holds the 30-minute deletion lease. Retry after it finishes; the lease is only taken over once it expires |
| `ownership was contended N times` | Many dynamic creates raced on one bucket; retry the create |
| Dynamic volume attaches the wrong bucket/prefix | Cubelet is older than the plugin and does not forward `private_data` to Attach; upgrade Cubelet first |
| `private_data must be a JSON object` | The create call did not use `POST /v2/volumes` / the SDK's `WithPrivateData`/`create_with_private_data` method |
| Bucket name contains dots | s3fs uses virtual-hosted-style addressing by default, which breaks TLS for dotted names. Use a bucket without dots, or set `S3FS_EXTRA_OPTS=-ouse_path_request_style` in `volume-s3.conf` (MinIO usually needs it too). A credential reference file can set its own `ADDRESSING_STYLE=path` |
| `ADDRESSING_STYLE=... conflicts with ... use_path_request_style` | Remove the conflicting global path-style option. Reference files must use `ADDRESSING_STYLE` and cannot set mount options |
| SDK write fails | `CUBE_PROXY_NODE_IP` unset; CubeAPI or template not READY |
| `Volume.create` without driver not using s3 | In the default install `s3` is the first `volume_plugins` entry (the default driver), so omitting driver routes to s3; if it doesn't, check the `volume_plugins` order |

More: [Framework §8 Troubleshooting](../../../docs/guide/volume-plugin.md).

---

## Dynamic buckets and credential references

A single cluster can bind each Volume to a bucket that was not known at deploy time, with its own credentials. Pass a storage descriptor as `private_data` when creating the volume:

```json
{"bucket":"customer-bucket","prefix":"workspaces/user-a/","cred_ref":"user-a"}
```

Use `POST /v2/volumes`, or the SDK's explicit method: Python `Volume.create_with_private_data`, Go `CreateVolumeWithPrivateData`, Node `Volume.createWithPrivateData`. The legacy `POST /volumes` path keeps its old behavior.

| Field | Required | Meaning |
|-------|----------|---------|
| `bucket` | No | Existing bucket owned by the caller. Falls back to `BUCKET` in `volume-s3.conf`. Dynamic buckets are **never auto-created**: a missing bucket fails the create. |
| `prefix` | No | Volume root inside the bucket. Falls back to `volumes/<volume_id>/`, and that state parses back unchanged. A leading `/` is stripped, a trailing `/` is added, and `.`/`..`/empty segments are rejected. Callers cannot choose another prefix under `volumes/`, nor any under `.cube-sandbox/`: those namespaces belong to the plugin. |
| `cred_ref` | No | Name of a credential reference file. Falls back to the keys in `volume-s3.conf`. |

**What is stored.** Create returns a *resolved* state as `private_data`, for example:

```json
{"v":1,"bucket":"customer-bucket","prefix":"workspaces/user-a/","cred_ref":"user-a"}
```

CubeMaster stores that value; Attach and Destroy receive it. The caller's original descriptor is never used again, and neither the descriptor nor the state is returned by the Volume API.

**Credential references.** Put one file per user at `/etc/cube/s3-credentials/<cred_ref>.conf` on every CubeMaster and Cubelet node (override the directory with `CREDENTIALS_DIR` or `CUBE_S3_CREDENTIALS_DIR`). Keep it root-owned, mode `0600`:

```
ACCESS_KEY_ID=AKIA...
SECRET_ACCESS_KEY=...
# Optional per-reference overrides:
ENDPOINT=https://cos.ap-guangzhou.myqcloud.com
REGION=ap-guangzhou
ADDRESSING_STYLE=path
```

Plaintext keys never go into `private_data`, the database, sandbox annotations, or logs. For a dynamic volume the plugin writes the s3fs passwd file under `PASSWD_DIR` (default `/etc/cube`) as `.passwd-s3fs-dynamic-<volume_id>` — outside the mounted tree, mode `0600`. The name deliberately differs from the legacy per-bucket `.passwd-s3fs-volume-<bucket>` file, so a volume ID that happens to equal a bucket name cannot overwrite it. Rotating a reference file takes effect on the next attach; a volume that is already mounted keeps the old credentials until it is remounted.

`ADDRESSING_STYLE` accepts `auto`, `path`, or `virtual`; an explicit reference value overrides the global mode. Reference files accept credentials, endpoint, region and addressing only. Configure `S3FS_EXTRA_OPTS` globally; a non-empty value in a reference is rejected. A reference selecting virtual or auto removes inherited path-style flags before mounting.

**Overlapping prefixes.** The plugin keeps an ownership registry at `.cube-sandbox/volume-claims.json` and refuses a new prefix that equals, contains, or is contained by another volume's prefix. The conflict check and the registration are a single conditional PUT (`If-Match` / `If-None-Match`), so two CubeMaster instances cannot both claim overlapping prefixes: a lost race re-reads the registry and reports the conflict. This requires an endpoint that implements conditional PUT; AWS S3, MinIO and R2 do. If the endpoint answers 501, the dynamic create **fails** with `does not support conditional PUT`. There is deliberately no fallback to an unconditional write: replaying a stale registry snapshot would drop another master's claim. Destroy removes only the volume's own prefix, never the bucket, and refuses if another volume's prefix is nested below it or if no claim records the prefix.

**Dynamic create requires an empty prefix.** CubeMaster reserves the database row before Create and persists the returned binding after Create succeeds, and if that database write fails it calls Destroy with the binding it just received. That rollback delete cannot be told apart from an explicit user delete, so Create refuses a prefix that already contains objects; only the zero-byte `prefix/` directory object from an earlier failed attempt is ignored. Adopting pre-existing objects would let the rollback recursively delete data the volume never wrote. The resolved state is still encoded and size-checked before the plugin touches the bucket, and a plugin-side create failure never recursively deletes prefix data.

**Destroy takes a deletion lease.** Before removing objects, Destroy writes a lease on the claim and keeps the claim until the data is gone. Each attempt gets a random `lease_id`; `deleting_at` records when it was taken and is used only to decide when a crashed attempt's lease may be taken over after 30 minutes. Release and clear compare `lease_id` strictly, so a delayed retry of an earlier attempt cannot release a newer attempt's lease even when both ran within the same second. A duplicate destroy that finds a fresh lease is refused with `deletion in progress` rather than deleting concurrently, and a prefix with no claim is refused unless it is already empty. The lease does not identify volume generations: an old DELETE delayed before taking its lease can still affect a same-ID, same-prefix replacement. A delete that outlives its lease can also overlap with a later owner. These lifecycle limits require separate coordination; the lease is not complete distributed fencing.

**Create response recovery.** A new claim carries a random `create_id` for its creating invocation. If the registration PUT reports an error, including a final 412 after an SDK replay, Create re-reads and recognizes its own committed claim. A later directory-write failure releases that newly added claim, while ownership that existed before this invocation is preserved. If the registry stays unreachable or the request is canceled before recovery, automatic reconciliation is not guaranteed; inspect ownership before retrying with a different volume ID.

**Unicode state encoding.** The plugin writes supplementary characters in state B as standard JSON surrogate escapes. Attach and Destroy decode them back to the original prefix; existing unescaped JSON states remain readable. This lets the built-in S3 workflow use supplementary characters with the existing utf8mb3 database connection, without changing the opaque state contract or the shared driver. The final encoded B must still fit 1024 bytes: one 4-byte character uses 12 bytes as a surrogate escape, so some prefixes reach the state limit before the independent 512-byte prefix limit. Oversize state is rejected before accessing the bucket. Other plugins that return raw supplementary characters remain subject to their database connection limits.

**Compatibility and upgrade order.** Dynamic state requires a Cubelet that forwards `private_data` to Attach. Upgrade in this order: every volume plugin (CubeMaster **and** Cubelet) → CubeMaster → CubeAPI → SDKs. A legacy volume (created without `private_data`) keeps the plain `volumes/<id>/` state and mounts exactly as before; it never reads or writes the registry, so credentials scoped to `volumes/*` keep working.

---

## Backend layout

```
<bucket>/volumes/<volumeID>/       ← legacy Volume (created without private_data)
<bucket>/workspaces/user-a/...     ← dynamic Volume (prefix from private_data)
<bucket>/.cube-sandbox/volume-claims.json  ← ownership registry for dynamic volumes
```

Attach mounts `BUCKET:/<prefix>` with s3fs at `/data/cube-shared/volume/s3-<volumeID>/` on the host, which Cubelet then exposes to the microVM over virtiofs. For a legacy volume the prefix is `volumes/<volumeID>`; for a dynamic volume it is the prefix from the resolved state.

### Hook behavior (RefCount)

| Hook | Side | refCount | Behavior |
|------|------|----------|----------|
| Create | Controller | — | legacy: ensure bucket exists (create if missing), then PUT the 0-byte `volumes/<id>/` object. dynamic: require the user bucket to exist, require the prefix to hold no objects, claim the prefix, then PUT the prefix directory object (s3fs directory object) |
| Destroy | Controller | — | list and delete every object under the prefix (dynamic: after taking the registry deletion lease) |
| Attach | Node | `0` | `s3fs` mount → return `host_path` |
| Attach | Node | `> 0` | Return the existing `host_path`; no second mount |
| Detach | Node | `> 0` | no-op |
| Detach | Node | `0` | `fusermount -u`; **retain** the data in the bucket |

### Design notes

- **One bucket per volume by default; many buckets per cluster.** Without `private_data`, every Volume lives under the configured `BUCKET` at `volumes/<id>/`. With `private_data`, each Volume names its own bucket, prefix and credential reference; no redeploy or plugin restart is needed. Multi-bucket deployments that do not pass `private_data` can still run several plugin instances with different `driver` names.
- **No S3 command line tool.** The control plane uses [minio-go](https://github.com/minio/minio-go) inside the plugin binary — one ~8MB static binary instead of a ~100MB AWS CLI install on every control node.
- **Bucket auto-create only for legacy volumes.** Create checks for the bucket first. An existing bucket does **not** need `s3:CreateBucket`; the plugin only creates the bucket when it is missing (typical for bundled MinIO). Dynamic buckets are user-owned and must already exist.
- **Destroy only tolerates not-found.** A missing bucket or key means the prefix is already gone; every other error propagates so CubeMaster does not drop the volume record while objects remain.
- **Credentials never enter the sandbox.** They live in the root-owned, mode-`600` config or credential reference files on CubeMaster/Cubelet; the microVM sees only a filesystem. `private_data` carries references, never keys.
- **`private_data` carries the binding** from Create to Attach and Destroy (max 1024 bytes, never returned to SDK clients). The plugin treats the caller's input as a request and the plugin's returned state as the source of truth.
- **Atomic ownership registry, fail closed.** Dynamic prefixes are recorded in `.cube-sandbox/volume-claims.json` under a conditional PUT. The plugin refuses overlapping prefixes, will not delete a prefix that contains another volume, and requires an empty prefix at create. AWS S3, MinIO and R2 honour `If-Match` / `If-None-Match` on PUT; a backend that answers 501 fails the dynamic create instead of degrading to an unconditional write that could lose a concurrent claim. Legacy volumes never touch the registry, preserving bucket policies scoped to `volumes/*`.
- **Time-bounded delete fencing.** Dynamic destroy writes a lease with a random per-attempt `lease_id` and releases the claim only after the prefix is gone. Concurrent duplicate destroys are refused while the 30-minute lease is fresh; a crashed deleter's lease expires so a retry can take over. Release and clear match the lease ID, not the timestamp, so same-second retries cannot release each other. Long-running deletes and old requests spanning same-ID volume replacement remain lifecycle limits.
- **Concurrency.** A per-volume `flock` serialises attach/detach for the same volume on a node, so two sandboxes starting at once cannot double-mount.
- **Destroy is irreversible** and removes the whole volume prefix. The API's refcount guard (409 on `DELETE /volumes` while any sandbox holds the volume) is what prevents deleting a mounted volume — the realistic hazard is a stale refcount after a node crash, where the cluster-wide count can reach 0 while a node still holds the mount.

---

## Layout

```
examples/volume/s3/
├── Makefile                       # build / fmt / lint / test
├── install-deps.sh                # host deps + checks (s3fs / jq)
├── volume-s3.conf.example
├── cmd/cube-volume-s3/main.go     # flag parsing, hook dispatch, stdout JSON
└── internal/
    ├── binding/                   # private_data parse/validate, prefix rules, ownership registry
    ├── config/                    # volume-s3.conf and credential reference parsing
    ├── s3api/                     # create / destroy via minio-go
    ├── s3fsmnt/                   # s3fs mount / unmount
    └── lockfile/                  # cross-process per-volume flock
```

Run the unit tests (no cloud access needed):

```bash
cd examples/volume/s3 && make test
# or, in the project builder image, from the repository root:
make cube-volume-s3-test
```

| Doc | Content |
|-----|---------|
| [S3 Volumes (user tutorial)](../../../docs/guide/s3-volume.md) | End-user quick start |
| [Volume Plugin framework](../../../docs/guide/volume-plugin.md) | Protocol, RefCount, Hook semantics |
| [COS example](../cos/README.md) | The reference plugin this one is modelled on |
| [s3fs-fuse](https://github.com/s3fs-fuse/s3fs-fuse) | Mount driver options and behavior |
