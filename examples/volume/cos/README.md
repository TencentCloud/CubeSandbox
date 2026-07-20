# Tencent Cloud COS Volume Plugin — Walkthrough

This guide is for **first-time Volume Plugin users**: follow the steps in order to use COS as persistent storage (create Volume → mount in sandbox → read/write → unmount → delete).

> **Version requirement:** Cube platform **≥ 0.6.0**, Python SDK **`cubesandbox` ≥ 0.6.0**.  
> Protocol and Hook details: [Volume Plugin framework](../../docs/guide/volume-plugin.md).

**Default path: binary plugin** (`driver=cos`, Shell + coscmd + cosfs — easiest to run). For the Go rpc plugin (`driver=cos-rpc`), see [rpc path](#rpc-path-optional) at the end.

中文文档：[README.zh.md](README.zh.md)

---

## What you will accomplish

1. Create/delete volume directories on Tencent Cloud COS (control plane)
2. Mount volumes into microVMs on sandbox create; writes go to COS (data plane)
3. Run the full lifecycle with the Python SDK

---

## Prerequisites

| Item | Description |
|------|-------------|
| Running Cube cluster | At least **CubeMaster**, **Cubelet**, **CubeAPI** (port usually `3000`) |
| Sandbox template | A `templateID` (see [§7](#7-verify-with-the-sdk)) |
| Tencent Cloud COS | A [Bucket](https://cloud.tencent.com/document/product/436/13309) and sub-account [API keys](https://cloud.tencent.com/document/product/436/7751) with read/write on that bucket |
| Local access | `sudo` on CubeMaster / Cubelet hosts to install software, edit config, restart services |

**Single-machine dev:** CubeMaster and Cubelet on one host — install deps once.  
**Multi-node:** See the table in [§1 Install dependencies](#1-install-dependencies).

---

## 1. Install dependencies

### Which machine?

| Tool | Install on | Purpose (Hook) | Plugin type |
|------|------------|----------------|-------------|
| **[cosfs](https://cloud.tencent.com/document/product/436/10976)** | **Cubelet** | attach / detach (FUSE mount to COS) | binary and rpc |
| **[coscmd](https://cloud.tencent.com/document/product/436/6883)** | **CubeMaster** | create / destroy (COS directory) | **binary** only |
| **jq** | **CubeMaster** (with coscmd) | binary plugin stdout JSON | **binary** only |
| COS Go SDK | Machine that builds rpc plugin | create / destroy | **rpc** only (via `go build`; see [rpc path](#rpc-path-optional)) |

> **rpc plugin:** Cubelet still needs cosfs; **no** coscmd / jq. Controller logic lives in the `cube-volume-cos-rpc` process using the Go SDK.

### Option A: install script (recommended)

Script: `examples/volume/cos/install-deps.sh`. Supports CentOS / TencentOS / Ubuntu, etc.; runs basic checks after install.

**Cubelet node** (runs Cubelet, attach):

```bash
cd /path/to/CubeSandbox
sudo ./examples/volume/cos/install-deps.sh --cosfs
```

**CubeMaster node** (binary create/destroy):

```bash
sudo ./examples/volume/cos/install-deps.sh --coscmd --jq
```

**Single machine, full binary stack:**

```bash
sudo ./examples/volume/cos/install-deps.sh --all
```

Check only (no install):

```bash
./examples/volume/cos/install-deps.sh --cosfs --check-only
./examples/volume/cos/install-deps.sh --coscmd --jq --check-only
```

### Option B: manual install — Tencent Cloud official docs

The script may not cover every distro/arch (ARM, custom images). **Follow Tencent docs** and verify with the commands below.

| Tool | Official doc |
|------|--------------|
| cosfs | [COS cosfs tool](https://cloud.tencent.com/document/product/436/10976) |
| coscmd | [COSCMD tool](https://cloud.tencent.com/document/product/436/6883) |
| jq | OS package manager: `yum install jq` / `apt install jq` |

### Verify install

Run on the **correct node**:

**Cubelet — cosfs**

```bash
ls /dev/fuse && echo "FUSE ok"
which cosfs && cosfs --version
```

Both must succeed; missing `/dev/fuse` breaks attach.

**CubeMaster — coscmd** (binary)

```bash
which coscmd && coscmd --version
```

Optional: test bucket access (after configuring keys in `volume-cos.conf`):

```bash
source /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf
coscmd -b "$BUCKET" -r "$REGION" list /
```

**CubeMaster — jq** (binary)

```bash
which jq && jq --version
printf '%s' '{"ok":true}' | jq -r '.ok'   # should print true
```

The install script runs similar checks when using `--cosfs` / `--coscmd` / `--jq` / `--all`; after manual install, run these yourself.

---

## 2. Install plugin and COS credentials

One-click install places the binary plugin under **`/usr/local/services/cubetoolbox/CubeMaster/plugin/`** (Controller) and **`/usr/local/services/cubetoolbox/Cubelet/plugin/`** (Node), and seeds `volume-cos.conf` from `volume-cos.conf.example` in each directory. After install, edit credentials on the matching node:

```bash
# CubeMaster node (create / destroy)
sudo chmod 600 /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf
sudo ${EDITOR:-vi} /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf

# Cubelet node (attach / detach, cosfs)
sudo chmod 600 /usr/local/services/cubetoolbox/Cubelet/plugin/volume-cos.conf
sudo ${EDITOR:-vi} /usr/local/services/cubetoolbox/Cubelet/plugin/volume-cos.conf
```

On a single host running both services, both paths are local; on split roles, edit each on its node.

**Manual install from source** (non one-click): copy the plugin into both `plugin/` directories:

```bash
PREFIX=/usr/local/services/cubetoolbox
sudo install -m 0755 examples/volume/cos/binary/cube-volume-cos.sh \
  "$PREFIX/CubeMaster/plugin/cube-volume-cos"
sudo install -m 0755 examples/volume/cos/binary/cube-volume-cos.sh \
  "$PREFIX/Cubelet/plugin/cube-volume-cos"
sudo install -m 0600 examples/volume/cos/volume-cos.conf.example \
  "$PREFIX/CubeMaster/plugin/volume-cos.conf"
sudo install -m 0600 examples/volume/cos/volume-cos.conf.example \
  "$PREFIX/Cubelet/plugin/volume-cos.conf"
# Then edit volume-cos.conf on each node
```

Required fields in `volume-cos.conf`:

| Field | Description | Example |
|-------|-------------|---------|
| `SECRET_ID` | API key ID | `AKIDxxx` |
| `SECRET_KEY` | API key secret | `xxxxx` |
| `BUCKET` | `BucketName-APPID` | `mybucket-1250000000` |
| `REGION` | Region | `ap-guangzhou` |

Mount base directory is **not** in this file — Cubelet passes it on attach (default `/data/volume`; see [§4](#4-configure-cubelet)).

---

## 3. Configure CubeMaster

Edit CubeMaster config (common path: `/usr/local/services/cubetoolbox/CubeMaster/conf.yaml`). Add the **Controller** plugin (Create / Destroy):

```yaml
volume_plugins:
  - name: cos
    type: binary
    binary_path: /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos
```

Notes:

- `name: cos` is the API/SDK **`driver`**; when `Volume.create("x")` omits driver, the **first** list entry is used.
- For binary-only setup, the snippet above is enough; do not duplicate `cos`.

Save and restart together with Cubelet ([§5](#5-restart-services-and-verify)).

---

## 4. Configure Cubelet

Edit Cubelet config (common path: `/usr/local/services/cubetoolbox/Cubelet/config/config.toml`).

Under `[plugins."io.cubelet.internal.v1.storage"]`, confirm mount parent (optional; default `/data/volume`):

```toml
[plugins."io.cubelet.internal.v1.storage"]
  volume_plugin_base_dir = "/data/volume"
```

Add the **Node** plugin (Attach / Detach):

```toml
[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
  name        = "cos"
  type        = "binary"
  binary_path = "/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos"
```

**`name` must match CubeMaster** (both `cos` here).  
Plugin `host_path` must be under `volume_plugin_base_dir` (example script uses `/data/volume/cos-<volumeID>`).

---

## 5. Restart services and verify

```bash
sudo systemctl restart cube-sandbox-cubemaster
sudo systemctl restart cube-sandbox-cubelet
sudo systemctl restart cube-sandbox-cube-api

sleep 5
systemctl is-active cube-sandbox-cubemaster cube-sandbox-cubelet cube-sandbox-cube-api
```

**Verify plugins loaded** (after restart):

```bash
grep -aF '[volume] registered' /data/log/CubeMaster/cubemaster-req.log | tail -5
grep -aF '[plugin_volume] initialized' /data/log/Cubelet/Cubelet-req.log | tail -5
```

Expected (binary example):

```text
[volume] registered binary plugin "cos" at /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos
[plugin_volume] initialized binary plugin "cos" at /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos
```

**Manual attach test** (on Cubelet node; script writes cosfs passwd):

```bash
/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos \
  --op attach \
  --sandbox-id test-sandbox \
  --namespace default \
  --volume-id test-vol \
  --ref-count 0 \
  --volume-base-dir /data/volume
# Success: one JSON line on stdout with "host_path":"/data/volume/cos-test-vol", "error":""
```

---

## 6. Prepare SDK environment

On your **dev machine** (can reach CubeAPI):

```bash
pip install 'cubesandbox>=0.6.0'

export CUBE_API_URL=http://<cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# Required for remote sandbox I/O on mounted volumes (data plane via CubeProxy)
export CUBE_PROXY_NODE_IP=<cubeproxy-or-cubelet-node-ip>

# When cluster auth is enabled:
# export CUBE_API_KEY=<your-key>
```

| Variable | Meaning |
|----------|---------|
| `CUBE_API_URL` | CubeAPI address |
| `CUBE_TEMPLATE_ID` | Template for sandbox creation |
| `CUBE_PROXY_NODE_IP` | Data-plane access after mount; omitting may break in-sandbox writes |

---

## 7. Verify with the SDK

Full lifecycle (create Volume → mount → read/write → destroy sandbox → delete Volume):

```python
from cubesandbox import Sandbox, Volume

# ① Create Volume (COS gets volumes/<id>/.keep)
vol = Volume.create("my-data")          # omit driver → first cos in conf
print("volume_id:", vol.volume_id)

# ② Create sandbox with mount
with Sandbox.create(
    volume_mounts={"/workspace": vol},
) as sb:
    sb.files.write("/workspace/hello.txt", "from COS volume")
    print(sb.files.read("/workspace/hello.txt"))

# ③ Exit with → sandbox destroyed, volume detached (COS data remains)

# ④ Delete Volume (COS prefix removed — irreversible)
Volume.destroy(vol.volume_id)
print("done")
```

**Optional: confirm cosfs in Cubelet mntns** (after attach, sandbox still running):

```bash
CPID=$(pgrep -f "cubelet --config" | head -1)
nsenter -t "$CPID" -m -- cat /proc/mounts | grep cosfs
```

**Optional: list objects in COS** (after binary create):

```bash
source /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf
coscmd -b "$BUCKET" -r "$REGION" list "volumes/my-data/"
```

### Automated verification (Python)

For HTTP contract checks, per-driver lifecycle, multi-sandbox sharing, and negative tests, use [`verify_volume.py`](verify_volume.py).

**Install dependencies:**

```bash
pip install 'cubesandbox>=0.6.0' requests
```

**Environment** (same as [§6](#6-prepare-sdk-environment), plus optional driver list):

| Variable | Required | Description |
|----------|----------|-------------|
| `CUBE_API_URL` | yes | CubeAPI base URL |
| `CUBE_TEMPLATE_ID` | yes | Sandbox template ID |
| `CUBE_PROXY_NODE_IP` | recommended | Data-plane access; use `127.0.0.1` on the CubeProxy host |
| `CUBE_VOLUME_DRIVERS` | no | Comma-separated drivers (default `cos`; use `cos-rpc` for the rpc plugin) |
| `CUBE_VOLUME_MOUNT_PATH` | no | In-sandbox mount path (default `/workspace`) |
| `CUBE_API_KEY` | no | When cluster auth is enabled |

**Run:**

```bash
cd examples/volume/cos
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=tpl-xxxx
export CUBE_PROXY_NODE_IP=127.0.0.1
export CUBE_VOLUME_DRIVERS=cos   # or cos-rpc

python3 verify_volume.py
```

The script prints a grouped report (PASS / FAIL / SKIP). Exit code is non-zero if any assertion failed.

---

## 8. Troubleshooting

| Symptom | Check |
|---------|-------|
| `unknown driver: cos` | CubeMaster `volume_plugins` missing or not restarted |
| `no plugin registered for driver "cos"` | Cubelet missing same-name plugin or not restarted |
| Sandbox create / attach fails | Cubelet logs: `[plugin_volume]`, `cosfs`; cosfs, FUSE, `volume-cos.conf` |
| SDK write fails | `CUBE_PROXY_NODE_IP`; CubeAPI / template READY |
| `Volume.create` without driver not using cos | **First** entry in `volume_plugins` is the default driver |

More: [Framework §8 Troubleshooting](../../docs/guide/volume-plugin.md#8-debugging-and-troubleshooting).

---

## COS backend layout

```
<bucket>/volumes/<volumeID>/   ← one directory per Volume
```

Attach mounts cosfs to `/data/volume/cos-<volumeID>/` on the host, then virtiofs into the sandbox.

### Hook behavior (RefCount)

| Hook | Side | refCount | Behavior |
|------|------|----------|----------|
| Create | Controller | — | `coscmd upload` creates `volumes/<id>/.keep` |
| Destroy | Controller | — | `coscmd delete -r` removes COS prefix |
| Attach | Node | `0` | `cosfs` FUSE mount → return `hostPath` |
| Attach | Node | `> 0` | Return existing `hostPath` |
| Detach | Node | `> 0` | no-op |
| Detach | Node | `0` | `fusermount -u`; **retain** COS data |

See [binary/README.md](binary/README.md) for script-level details.

### Implementation trade-offs (not framework limits)

The COS binary/rpc demos use a **fixed `BUCKET`** in `volume-cos.conf`; all Volumes live under `volumes/<volumeID>/`. Multi-bucket setups often deploy multiple plugin processes with different `driver` names — **example pattern only**, not a platform limit.

Custom plugins may instead accept bucket/storage class in `Create` or Volume metadata, route multiple buckets inside one process, or use `driver` for backend type and pick resources per Volume. The framework only requires Hook protocol and `driver` routing consistency.

---

## rpc path (optional)

For a **long-running Go gRPC** plugin with **COS Go SDK** Controller (no coscmd).

| Step | Difference from binary |
|------|------------------------|
| Deps | **Cubelet:** cosfs only ([§1](#1-install-dependencies)); **no** coscmd / jq |
| Plugin | `go build` → `cube-volume-cos-rpc`, systemd service |
| CubeMaster / Cubelet | `type: rpc`, `name: cos-rpc`, `socket_path: /run/cube-volume-cos-rpc.sock` (same as plugin `SOCKET`) |
| SDK | `Volume.create("x", driver="cos-rpc")` |

Step-by-step: [rpc/README.md](rpc/README.md).

---

## Layout and further reading

```
examples/volume/cos/
├── install-deps.sh          # deps + checks
├── verify_volume.py         # Python SDK verification script
├── volume-cos.conf.example
├── binary/                  # Shell plugin source walkthrough
└── rpc/                     # Go rpc plugin
```

| Doc | Content |
|-----|---------|
| [binary/README.md](binary/README.md) | Script implementation, manual attach/detach |
| [rpc/README.md](rpc/README.md) | rpc build, systemd, running both plugins |
| [verify_volume.py](verify_volume.py) | Automated Python SDK verification |
| [Volume Plugin framework](../../docs/guide/volume-plugin.md) | Protocol, RefCount, Hook semantics |
