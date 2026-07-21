# COS Volume Plugin (binary)

**Plugin type:** `binary` (each Hook forks a child process; one line of JSON on stdout)  
**Driver name:** `cos`

> Shared COS docs (layout, deps, credentials): [`../README.md`](../README.md)  
> rpc example: [`../rpc/`](../rpc/)  
> Framework: [docs/guide/volume-plugin.md](../../../../docs/guide/volume-plugin.md)

中文文档：[README.zh.md](README.zh.md)

---

## Before you start

| Step | Content |
|------|---------|
| 1 | [../README.md — Prerequisites](../README.md#prerequisites) |
| 2 | **Cubelet node:** cosfs — [§1](../README.md#1-install-dependencies) |
| 3 | **CubeMaster node:** coscmd + jq — [§1](../README.md#1-install-dependencies) |
| 4 | Deploy [cube-volume-cos.sh](cube-volume-cos.sh) + `volume-cos.conf` — [§2](../README.md#2-install-plugin-and-cos-credentials) |
| 5 | Configure CubeMaster + Cubelet — [§3–§5](../README.md#3-configure-cubemaster) |
| 6 | SDK verification — [§6–§7](../README.md#6-prepare-sdk-environment) |

**Dependencies:** [coscmd](https://cloud.tencent.com/document/product/436/6883) on CubeMaster; [cosfs](https://cloud.tencent.com/document/product/436/10976) on Cubelet. No COS Go SDK (see [rpc](../rpc/)).

---

## How it works

(COS bucket layout and Hook semantics: [../README.md](../README.md).)

The plugin uses **cosfs** (FUSE) to mount a COS bucket subpath as a local directory. Each volume is `volumes/<volume_id>/`, managed by its own cosfs process.

```
COS Bucket
└── volumes/
    ├── vol-aaa/          ← volume A (one cosfs process)
    │   ├── .keep
    │   └── data.bin
    └── vol-bbb/          ← volume B (another cosfs process)
        └── model.pt
```

**Isolation:** Each volume has its own cosfs process and mount point. Destroying sandbox A (unmount vol-aaa) does not affect vol-bbb used by sandbox B.

**Lifecycle:**

| Phase | Trigger | Action |
|-------|---------|--------|
| **create** | `Volume.create()` | coscmd creates `volumes/<id>/.keep` on COS |
| **attach** | sandbox create | cosfs mounts `BUCKET:/volumes/<id>` on host; returns `host_path` |
| **detach** | sandbox destroy | Last reference: `fusermount` unmounts |
| **destroy** | `Volume.destroy()` | coscmd deletes `volumes/<id>/` recursively |

---

## Deploy the plugin

### 1. Install the script

After one-click install, the binary plugin is at **`/usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos`** (Controller) and **`/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos`** (Node). Credentials: [../README.md §2](../README.md#2-install-plugin-and-cos-credentials).

**Manual install from source** (non one-click):

```bash
PREFIX=/usr/local/services/cubetoolbox
sudo install -m 0755 examples/volume/cos/binary/cube-volume-cos.sh \
  "$PREFIX/CubeMaster/plugin/cube-volume-cos"
sudo install -m 0755 examples/volume/cos/binary/cube-volume-cos.sh \
  "$PREFIX/Cubelet/plugin/cube-volume-cos"
```

Or download from GitHub:

```bash
PREFIX=/usr/local/services/cubetoolbox
curl -fsSL "https://raw.githubusercontent.com/TencentCloud/CubeSandbox/master/examples/volume/cos/binary/cube-volume-cos.sh" \
  -o /tmp/cube-volume-cos.sh
sudo install -m 0755 /tmp/cube-volume-cos.sh "$PREFIX/CubeMaster/plugin/cube-volume-cos"
sudo install -m 0755 /tmp/cube-volume-cos.sh "$PREFIX/Cubelet/plugin/cube-volume-cos"
```

### 2. Create config file

Same directory as the plugin binary (one file per node; edit on each host when roles are split):

```bash
PREFIX=/usr/local/services/cubetoolbox
sudo install -m 0600 examples/volume/cos/volume-cos.conf.example \
  "$PREFIX/CubeMaster/plugin/volume-cos.conf"
sudo install -m 0600 examples/volume/cos/volume-cos.conf.example \
  "$PREFIX/Cubelet/plugin/volume-cos.conf"
sudo ${EDITOR:-vi} "$PREFIX/CubeMaster/plugin/volume-cos.conf"
sudo ${EDITOR:-vi} "$PREFIX/Cubelet/plugin/volume-cos.conf"
```

Or write manually (example fields):

```bash
PREFIX=/usr/local/services/cubetoolbox
sudo tee "$PREFIX/CubeMaster/plugin/volume-cos.conf" > /dev/null << 'EOF'
SECRET_ID=AKIDxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
SECRET_KEY=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
BUCKET=mybucket-1250000000
REGION=ap-guangzhou
# Mount path comes from Cubelet --volume-base-dir (default /data/volume/cos-<id>).
EOF
sudo chmod 600 "$PREFIX/CubeMaster/plugin/volume-cos.conf"
# On Cubelet node, edit $PREFIX/Cubelet/plugin/volume-cos.conf similarly
```

| Field | Description | Example |
|-------|-------------|---------|
| `SECRET_ID` | Tencent Cloud API key ID | `AKIDxxx` |
| `SECRET_KEY` | API key secret | `xxxxx` |
| `BUCKET` | COS bucket `BucketName-APPID` | `mybucket-1250000000` |
| `REGION` | Region | `ap-guangzhou` |

Mount path is passed as `--volume-base-dir` (`volume_plugin_base_dir`, default `/data/volume`). `host_path` must be under it, e.g. `/data/volume/cos-<id>`.

> **Security:** Prefer a sub-account key with bucket read/write only.

CubeMaster / Cubelet config, restart, and verification: **[../README.md §3–§5](../README.md#3-configure-cubemaster)**. Copy below for offline reading.

### 3. Configure Cubelet

Edit `/usr/local/services/cubetoolbox/Cubelet/config/config.toml`:

```toml
[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
  name        = "cos"
  type        = "binary"
  binary_path = "/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos"
```

### 4. Configure CubeMaster

Edit `/usr/local/services/cubetoolbox/CubeMaster/conf.yaml`:

```yaml
volume_plugins:
  - name: cos
    type: binary
    binary_path: /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos
```

### 5. Restart services

```bash
systemctl restart cube-sandbox-cubemaster
systemctl restart cube-sandbox-cubelet
```

Verify plugins loaded:

```bash
grep -aF '[volume] registered' /data/log/CubeMaster/cubemaster-req.log | tail -5
grep -aF '[plugin_volume] initialized' /data/log/Cubelet/Cubelet-req.log | tail -5
```

---

## Usage

Examples use **Python SDK `cubesandbox` ≥ 0.6.0**. See [Framework §2.1](../../../docs/guide/volume-plugin.md).

### Create Volume

```python
from cubesandbox import Volume

vol = Volume.create("my-vol")
# Same when first volume_plugins entry is cos:
# vol = Volume.create("my-vol", driver="cos")
print(vol.volume_id, vol.name, vol.token)
```

Creates `volumes/my-vol/.keep` on COS. Verify with coscmd:

```bash
source /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf
coscmd -b $BUCKET -r $REGION list volumes/my-vol/
```

### Create sandbox with mount

```python
from cubesandbox import Sandbox, Volume

sb = Sandbox.create(
    volume_mounts={"/mnt/data": vol},
)
try:
    sb.files.write("/mnt/data/hello.txt", "persisted in COS")
    print(sb.files.read("/mnt/data/hello.txt"))
finally:
    sb.kill()
```

In-sandbox `/mnt/data` maps to COS `BUCKET:/volumes/my-vol/`.

**Verify mount** (Cubelet mntns):

```bash
CPID=$(pgrep -f "cubelet --config" | head -1)
nsenter -t $CPID -m -- cat /proc/mounts | grep cosfs
```

### Destroy sandbox (detach)

```python
sb.kill()
```

When the last reference is released, cosfs exits and the mount is removed. **COS data is not deleted.**

### Delete Volume

```python
Volume.destroy(vol.volume_id)
```

Runs `coscmd delete -r -f volumes/my-vol/` — **irreversible**.

---

## Code walkthrough

The plugin is one Shell script implementing CubeMaster (`create`/`destroy`) and Cubelet (`attach`/`detach`) hooks. Below is **what each step does** (comments match the source).

### Argument parsing

CubeMaster / Cubelet invoke the plugin with `--op` and flags; the script parses them and dispatches:

```bash
while [[ $# -gt 0 ]]; do
    case "$1" in
        --op)           OP="$2";           shift 2 ;;  # which hook: create|destroy|attach|detach
        --volume-id)    VOLUME_ID="$2";    shift 2 ;;  # volume identifier
        --sandbox-id)   SANDBOX_ID="$2";   shift 2 ;;  # sandbox using the volume (attach/detach)
        --ref-count)    REF_COUNT="$2";    shift 2 ;;  # sandboxes still using this volume on node
        --metadata)     METADATA="$2";     shift 2 ;;  # JSON saved at attach (used on detach)
        # ...
    esac
done
```

### create (control plane — user creates a volume)

Creates an empty folder for the volume in COS; no node mount yet.

```bash
do_create() {
    local volume_id="$1" name="$2"

    load_config                          # Step 1: read SECRET_ID/KEY, BUCKET, REGION
    cos_create_dir "$volume_id"          # Step 2: upload volumes/<id>/.keep via coscmd
    jq -cn '{ token: "", error: "" }'    # Step 3: return success (COS has no extra token)
}
```

### destroy (control plane — user deletes a volume)

Recursively deletes the volume folder from COS — **irreversible**.

```bash
do_destroy() {
    local volume_id="$1"

    load_config                          # Step 1: read COS credentials
    cos_remove_dir "$volume_id"          # Step 2: coscmd delete -r volumes/<id>/
    ok_json                              # Step 3: return success
}
```

### attach (data plane — sandbox mounts a volume)

Mounts the COS folder on the node with cosfs and returns `host_path` for Cubelet to bind-mount into the sandbox.

```bash
do_attach() {
    local sandbox_id="$1" volume_id="$2" ref_count="$3"

    load_config                          # Step 1: read COS credentials
    ensure_passwd_file                   # Step 2: write /etc/cube/.passwd-cosfs for cosfs

    volume_lock_acquire "$volume_id"     # Step 3: flock — one attach at a time per volume
    trap 'volume_lock_release' EXIT

    cosfs_mount_volume "$volume_id"      # Step 4: mount BUCKET:/volumes/<id> (skip if up)

    local mnt="$(volume_mountpoint "$volume_id")"

    # Step 5: return host_path; Cubelet bind-mounts into sandbox
    jq -cn --arg path "$mnt" --arg vid "$volume_id" \
        '{ host_path: $path,
           metadata: { mount_dir: $path, volume_id: $vid }, error: "" }'
}
```

**Idempotent attach:** `cosfs_mount_volume` uses `mountpoint -q`; skips if already mounted. When RefCount > 0, sandboxes share the same `host_path`.

### detach (data plane — sandbox unmounts a volume)

Unmounts cosfs only when **no sandbox on this node** still uses the volume (ref_count == 0). COS data remains until destroy.

```bash
do_detach() {
    local sandbox_id="$1" volume_id="$2" ref_count="$3"

    if [[ "$ref_count" -gt 0 ]]; then     # Step 1: others still mounted — do nothing
        ok_json; return
    fi

    volume_lock_acquire "$volume_id"      # Step 2: flock before unmount
    trap 'volume_lock_release' EXIT

    # Step 3: path from attach metadata, or recompute from volume_id
    local mnt="$(printf '%s' "$metadata_json" | jq -r '.mount_dir // empty')"
    [[ -n "$mnt" ]] || mnt="$(volume_mountpoint "$volume_id")"
    cosfs_unmount_volume "$mnt"           # Step 4: fusermount — last user gone
    ok_json
}
```

### cosfs passwd format

Format: `BucketName-APPID:SecretId:SecretKey`, mode `600`:

```bash
ensure_passwd_file() {
    local content="${BUCKET}:${SECRET_ID}:${SECRET_KEY}"   # cosfs credential line
    printf '%s\n' "$content" > /etc/cube/.passwd-cosfs
    chmod 600 /etc/cube/.passwd-cosfs                      # cosfs rejects world-readable files
}
```

---

## Verification and debugging

### Manual attach

```bash
/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos \
  --op attach \
  --sandbox-id test-sandbox-001 \
  --namespace default \
  --volume-id my-vol \
  --ref-count 0 \
  --volume-base-dir /data/volume

CPID=$(pgrep -f "cubelet --config" | head -1)
nsenter -t $CPID -m -- mountpoint /data/volume/cos-my-vol
```

### Manual detach

```bash
/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos \
  --op detach \
  --sandbox-id test-sandbox-001 \
  --namespace default \
  --volume-id my-vol \
  --ref-count 0 \
  --metadata '{"mount_dir":"/data/volume/cos-my-vol"}'
```

### Plugin logs

Logs go to stderr; Cubelet forwards to journald:

```bash
journalctl -u cube-sandbox-cubelet --since "5 min ago" | grep cube-volume-cos
```

---

## FAQ

**cosfs mount not visible in host `/proc/mounts`?**

Expected. cosfs runs in Cubelet mntns. Use `nsenter` as above.

**`libcrypto.so.1.1: cannot open shared object file`**

TencentOS 4.x uses OpenSSL 3; cosfs needs 1.1:

```bash
yum install -y compat-openssl11
```

**`coscmd: command not found`**

```bash
python3 -m venv /opt/coscmd-venv
/opt/coscmd-venv/bin/pip install coscmd
ln -sf /opt/coscmd-venv/bin/coscmd /usr/local/bin/coscmd
```

**`Duplicate entry 'vol-xxx' for key 'uniq_volume_id'`**

Volume still exists. `Volume.destroy("<id>")` first, then recreate.

---

## References

- Source: [cube-volume-cos.sh](cube-volume-cos.sh)
- [cosfs](https://cloud.tencent.com/document/product/436/10976) · [coscmd](https://cloud.tencent.com/document/product/436/6883)
- [COS Go SDK](https://cloud.tencent.com/document/product/436/31215) (rpc example)
- rpc example: [../rpc/](../rpc/)
- Binary driver: [Cubelet/plugins/volume/binary/driver.go](../../../../Cubelet/plugins/volume/binary/driver.go)
- Framework: [docs/guide/volume-plugin.md](../../../docs/guide/volume-plugin.md)
