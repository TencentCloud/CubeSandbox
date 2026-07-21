# COS Volume 插件（binary）

**插件类型**：`binary`（每次 Hook fork 子进程，stdout 单行 JSON）  
**driver 名**：`cos`

> 共用说明（COS 布局、依赖安装、凭证）：[`../README.zh.md`](../README.zh.md)  
> rpc 类型示例：[`../rpc/`](../rpc/)  
> 框架原理：[docs/zh/guide/volume-plugin.md](../../../../docs/zh/guide/volume-plugin.md)

---

## 体验前准备

| 步骤 | 内容 |
|------|------|
| 1 | 完成 [../README.zh.md — 前置条件](../README.zh.md#前置条件) |
| 2 | **Cubelet 节点**：cosfs — [§1 安装与校验](../README.zh.md#1-安装依赖) |
| 3 | **CubeMaster 节点**：coscmd + jq — [§1](../README.zh.md#1-安装依赖) |
| 4 | 部署本目录 [cube-volume-cos.sh](cube-volume-cos.sh) 与 `volume-cos.conf` — [§2](../README.zh.md#2-安装插件与-cos-凭证) |
| 5 | 配置 CubeMaster + Cubelet — [§3–§5](../README.zh.md#3-配置-cubemaster) |
| 6 | SDK 验证 — [§6–§7](../README.zh.md#6-准备-sdk-环境) |

**本示例依赖**：CubeMaster 侧 [coscmd](https://cloud.tencent.com/document/product/436/6883)；Cubelet 侧 [cosfs](https://cloud.tencent.com/document/product/436/10976)。不使用 COS Go SDK（见 [rpc](../rpc/)）。

---

## 插件原理

（COS bucket 布局与 Hook 语义见 [../README.zh.md](../README.zh.md)。）

COS 插件用 **cosfs**（FUSE）将 COS bucket 的子路径挂载为本地目录，每个 volume 对应 bucket 内的一个独立子目录 `volumes/<volume_id>/`，由一个独立的 cosfs 进程管理。

```
COS Bucket
└── volumes/
    ├── vol-aaa/          ← volume A（一个 cosfs 进程）
    │   ├── .keep
    │   └── data.bin
    └── vol-bbb/          ← volume B（另一个 cosfs 进程）
        └── model.pt
```

**隔离性保证**：每个 volume 有独立的 cosfs 进程和挂载点，销毁沙箱 A（卸载 vol-aaa）不会影响沙箱 B 正在使用的 vol-bbb。

**两阶段生命周期**：

| 阶段 | 触发 | 操作 |
|------|------|------|
| **create** | `Volume.create()` | coscmd 在 COS 上创建 `volumes/<id>/.keep` 目录占位符 |
| **attach** | sandbox 创建 | cosfs 挂载 `BUCKET:/volumes/<id>` 到宿主机目录，返回 `host_path` |
| **detach** | sandbox 销毁 | 最后一个引用卸载时，fusermount 卸载该 volume |
| **destroy** | `Volume.destroy()` | coscmd 递归删除 `volumes/<id>/` 目录 |

---

## 部署插件

### 1. 安装插件脚本

一键部署后，binary 插件已在 **`/usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos`**（Controller）与 **`/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos`**（Node）。凭证配置见 [../README.zh.md §2](../README.zh.md#2-安装插件与-cos-凭证)。

**从源码手动安装**（非 one-click）：

```bash
PREFIX=/usr/local/services/cubetoolbox
sudo install -m 0755 examples/volume/cos/binary/cube-volume-cos.sh \
  "$PREFIX/CubeMaster/plugin/cube-volume-cos"
sudo install -m 0755 examples/volume/cos/binary/cube-volume-cos.sh \
  "$PREFIX/Cubelet/plugin/cube-volume-cos"
```

或从 GitHub 下载：

```bash
PREFIX=/usr/local/services/cubetoolbox
curl -fsSL "https://raw.githubusercontent.com/TencentCloud/CubeSandbox/master/examples/volume/cos/binary/cube-volume-cos.sh" \
  -o /tmp/cube-volume-cos.sh
sudo install -m 0755 /tmp/cube-volume-cos.sh "$PREFIX/CubeMaster/plugin/cube-volume-cos"
sudo install -m 0755 /tmp/cube-volume-cos.sh "$PREFIX/Cubelet/plugin/cube-volume-cos"
```

### 2. 创建配置文件

与插件脚本同目录（CubeMaster / Cubelet 各一份；分节点部署时在对应节点编辑）：

```bash
PREFIX=/usr/local/services/cubetoolbox
sudo install -m 0600 examples/volume/cos/volume-cos.conf.example \
  "$PREFIX/CubeMaster/plugin/volume-cos.conf"
sudo install -m 0600 examples/volume/cos/volume-cos.conf.example \
  "$PREFIX/Cubelet/plugin/volume-cos.conf"
sudo ${EDITOR:-vi} "$PREFIX/CubeMaster/plugin/volume-cos.conf"
sudo ${EDITOR:-vi} "$PREFIX/Cubelet/plugin/volume-cos.conf"
```

或手动写入（示例字段）：

```bash
PREFIX=/usr/local/services/cubetoolbox
sudo tee "$PREFIX/CubeMaster/plugin/volume-cos.conf" > /dev/null << 'EOF'
SECRET_ID=AKIDxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
SECRET_KEY=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
BUCKET=mybucket-1250000000
REGION=ap-guangzhou
# 挂载路径由 Cubelet 的 --volume-base-dir 决定（默认 /data/volume/cos-<id>），无需在此配置。
EOF
sudo chmod 600 "$PREFIX/CubeMaster/plugin/volume-cos.conf"
# Cubelet 节点同样编辑 $PREFIX/Cubelet/plugin/volume-cos.conf
```

**配置项说明**

| 字段 | 说明 | 示例 |
|------|------|------|
| `SECRET_ID` | 腾讯云 API 密钥 ID | `AKIDxxx` |
| `SECRET_KEY` | 腾讯云 API 密钥 Key | `xxxxx` |
| `BUCKET` | COS bucket，格式 `BucketName-APPID` | `mybucket-1250000000` |
| `REGION` | COS 地域 | `ap-guangzhou` |

挂载路径由 Cubelet 在 attach 时传入 `--volume-base-dir`（配置项 `volume_plugin_base_dir`，默认 `/data/volume`），插件返回的 `host_path` 须在其下，例如 `/data/volume/cos-<id>`。

> **安全建议**：建议使用仅拥有该 bucket 读写权限的子账号密钥，避免使用主账号密钥。

CubeMaster / Cubelet 配置、重启与加载验证见 **[../README.zh.md §3–§5](../README.zh.md#3-配置-cubemaster)**。下面保留一份副本便于离线阅读。

### 3. 配置 Cubelet

编辑 `/usr/local/services/cubetoolbox/Cubelet/config/config.toml`，在 `[plugins."io.cubelet.internal.v1.storage"]` 下添加：

```toml
[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
  name        = "cos"
  type        = "binary"
  binary_path = "/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos"
```

### 4. 配置 CubeMaster

编辑 `/usr/local/services/cubetoolbox/CubeMaster/conf.yaml`，添加：

```yaml
volume_plugins:
  - name: cos
    type: binary
    binary_path: /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos
```

### 5. 重启服务

```bash
systemctl restart cube-sandbox-cubemaster
systemctl restart cube-sandbox-cubelet
```

确认插件已加载：

```bash
grep -aF '[volume] registered' /data/log/CubeMaster/cubemaster-req.log | tail -5
grep -aF '[plugin_volume] initialized' /data/log/Cubelet/Cubelet-req.log | tail -5
```

---

## 使用

以下示例使用 **Python SDK `cubesandbox` ≥ 0.6.0**。请先安装 SDK 并配置环境变量（见 [框架指南 §2.1](../../../docs/zh/guide/volume-plugin.md)）。

### 创建 Volume

```python
from cubesandbox import Volume

# e2b 兼容：省略 driver
vol = Volume.create("my-vol")
# 等价于（CubeMaster volume_plugins 第一项为 cos 时）：
# vol = Volume.create("my-vol", driver="cos")
print(vol.volume_id, vol.name, vol.token)
# volume_id='my-vol' name='my-vol' token=''
```

指定 `name` 时 `volume_id` 通常与 `name` 相同；省略 `name` 则服务端生成 UUID。

这一步会在 COS 上创建 `volumes/my-vol/.keep` 占位符。可用 coscmd 在 COS 侧核对：

```bash
source /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf
coscmd -b $BUCKET -r $REGION list volumes/my-vol/
```

### 创建沙箱并挂载 Volume

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

沙箱内 `/mnt/data` 即对应 COS `BUCKET:/volumes/my-vol/`。

**验证挂载**（在 Cubelet mntns 里查看）：

```bash
# Binary 插件在 Cubelet 的 mount namespace 里执行挂载
# 宿主机 /proc/mounts 看不到，需要 nsenter
CPID=$(pgrep -f "cubelet --config" | head -1)
nsenter -t $CPID -m -- cat /proc/mounts | grep cosfs
```

### 销毁沙箱（解绑）

```python
sb.kill()  # 或 with Sandbox.create(...) as sb: 退出时自动销毁
```

沙箱销毁后，当该 volume 的最后一个引用被卸载时，cosfs 进程自动退出，挂载点自动卸载。**COS 上的数据不会被删除**（数据生命周期独立于沙箱）。

### 删除 Volume

```python
Volume.destroy(vol.volume_id)
```

这一步会调用 `coscmd delete -r -f volumes/my-vol/`，**递归删除 COS 上该 volume 的所有数据**，操作不可逆。

---

## 插件代码详解

插件本体是一个 Shell 脚本，同时实现了 CubeMaster 侧（`create`/`destroy`）和 Cubelet 侧（`attach`/`detach`）的操作。下面按 Hook 说明**每一步在做什么**（注释均为英文，与源码一致）。

### 参数解析

CubeMaster / Cubelet 每次调用插件时都会传入 `--op` 和若干参数；脚本解析后跳转到对应函数：

```bash
while [[ $# -gt 0 ]]; do
    case "$1" in
        --op)           OP="$2";           shift 2 ;;  # which hook: create|destroy|attach|detach
        --volume-id)    VOLUME_ID="$2";    shift 2 ;;  # volume identifier
        --sandbox-id)   SANDBOX_ID="$2";   shift 2 ;;  # sandbox using the volume (attach/detach)
        --ref-count)    REF_COUNT="$2";    shift 2 ;;  # how many sandboxes still use this volume
        --metadata)     METADATA="$2";     shift 2 ;;  # JSON saved at attach time (for detach)
        # ...
    esac
done
```

### create 核心逻辑（管控面：用户创建 Volume 时）

在 COS 桶里为该 volume 建一个空目录；不涉及节点挂载。

```bash
do_create() {
    local volume_id="$1" name="$2"

    load_config                          # Step 1: read SECRET_ID/KEY, BUCKET, REGION
    cos_create_dir "$volume_id"          # Step 2: upload volumes/<id>/.keep via coscmd
    jq -cn '{ token: "", error: "" }'    # Step 3: return success (no extra token for COS)
}
```

### destroy 核心逻辑（管控面：用户删除 Volume 时）

从 COS 桶里递归删除该 volume 的目录；**数据不可恢复**。

```bash
do_destroy() {
    local volume_id="$1"

    load_config                          # Step 1: read COS credentials
    cos_remove_dir "$volume_id"          # Step 2: coscmd delete -r volumes/<id>/
    ok_json                              # Step 3: return success
}
```

### attach 核心逻辑（数据面：沙箱挂载 Volume 时）

在节点上用 cosfs 把 COS 目录挂到本地路径，并把 `host_path` 还给 Cubelet，由 Cubelet bind-mount 进沙箱。

```bash
do_attach() {
    local sandbox_id="$1" volume_id="$2" ref_count="$3"
    # VOLUME_BASE_DIR comes from --volume-base-dir (default /data/volume)

    load_config                          # Step 1: read COS credentials
    ensure_passwd_file                   # Step 2: write /etc/cube/.passwd-cosfs for cosfs

    volume_lock_acquire "$volume_id"     # Step 3: flock — one attach at a time per volume
    trap 'volume_lock_release' EXIT

    cosfs_mount_volume "$volume_id"      # Step 4: mount BUCKET:/volumes/<id> (skip if already up)

    local mnt="$(volume_mountpoint "$volume_id")"   # e.g. /data/volume/cos-my-vol

    # Step 5: return host_path; Cubelet bind-mounts it into the sandbox
    jq -cn --arg path "$mnt" --arg vid "$volume_id" \
        '{ host_path: $path,
           metadata: { mount_dir: $path, volume_id: $vid }, error: "" }'
}
```

**attach 的幂等性**：`cosfs_mount_volume` 内部先 `mountpoint -q` 检查，已挂载则直接返回，不重复挂载。RefCount > 0 时同一个 `host_path` 被多个沙箱共享使用。

### detach 核心逻辑（数据面：沙箱卸载 Volume 时）

只有当本节点上**没有沙箱再使用该 volume**（ref_count 降为 0）时才真正卸载 cosfs；COS 上的数据仍保留，需 destroy 才删。

```bash
do_detach() {
    local sandbox_id="$1" volume_id="$2" ref_count="$3"

    if [[ "$ref_count" -gt 0 ]]; then     # Step 1: others still mounted — do nothing
        ok_json; return
    fi

    volume_lock_acquire "$volume_id"      # Step 2: flock before unmount
    trap 'volume_lock_release' EXIT

    # Step 3: mount path from attach metadata, or recompute from volume_id
    local mnt="$(printf '%s' "$metadata_json" | jq -r '.mount_dir // empty')"
    [[ -n "$mnt" ]] || mnt="$(volume_mountpoint "$volume_id")"

    cosfs_unmount_volume "$mnt"           # Step 4: fusermount — last user gone
    ok_json
}
```

### passwd 文件格式

cosfs 的 passwd 文件格式为 `BucketName-APPID:SecretId:SecretKey`（**必须包含 bucket 前缀**），权限必须是 600：

```bash
ensure_passwd_file() {
    local content="${BUCKET}:${SECRET_ID}:${SECRET_KEY}"   # cosfs credential line
    printf '%s\n' "$content" > /etc/cube/.passwd-cosfs
    chmod 600 /etc/cube/.passwd-cosfs                      # cosfs refuses world-readable files
}
```

---

## 验证与调试

### 手动测试 attach

```bash
/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos \
  --op attach \
  --sandbox-id test-sandbox-001 \
  --namespace default \
  --volume-id my-vol \
  --ref-count 0 \
  --volume-base-dir /data/volume
# → {"host_path":"/data/volume/cos-my-vol","metadata":{...},"error":""}

# 验证挂载（在 Cubelet mntns 里）
CPID=$(pgrep -f "cubelet --config" | head -1)
nsenter -t $CPID -m -- mountpoint /data/volume/cos-my-vol
# → /data/volume/cos-my-vol is a mountpoint
```

### 手动测试 detach

```bash
/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos \
  --op detach \
  --sandbox-id test-sandbox-001 \
  --namespace default \
  --volume-id my-vol \
  --ref-count 0 \
  --metadata '{"mount_dir":"/data/volume/cos-my-vol"}'
# → {"error":""}
```

### 查看插件日志

插件把所有日志写到 stderr，Cubelet 会将其转发到 journald：

```bash
journalctl -u cube-sandbox-cubelet --since "5 min ago" | grep cube-volume-cos
```

---

## 常见问题

**cosfs 挂载后宿主机 `/proc/mounts` 看不到？**

正常现象。cosfs 由 Cubelet fork，在 Cubelet 的 mount namespace 里挂载，宿主机看不到。需要 nsenter 进入 Cubelet 的 mntns 查看：

```bash
CPID=$(pgrep -f "cubelet --config" | head -1)
nsenter -t $CPID -m -- cat /proc/mounts | grep cosfs
```

**`libcrypto.so.1.1: cannot open shared object file`**

TencentOS 4.x 默认使用 OpenSSL 3，cosfs 需要 OpenSSL 1.1：

```bash
yum install -y compat-openssl11
```

**`coscmd: command not found`**

需要安装 coscmd 并创建 wrapper：

```bash
python3 -m venv /opt/coscmd-venv
/opt/coscmd-venv/bin/pip install coscmd
ln -sf /opt/coscmd-venv/bin/coscmd /usr/local/bin/coscmd
```

**`Duplicate entry 'vol-xxx' for key 'uniq_volume_id'`（创建同名 volume 报错）**

说明该 `volumeID` 对应的 Volume 仍存在（未软删除）。请先 `Volume.destroy("<id>")` 再重建。

---

## 参考链接

- 插件源码：[cube-volume-cos.sh](cube-volume-cos.sh)
- cosfs：[对象存储 cosfs 工具](https://cloud.tencent.com/document/product/436/10976)
- coscmd：[COSCMD 工具](https://cloud.tencent.com/document/product/436/6883)
- COS Go SDK（rpc 示例用）：[Go SDK 快速入门](https://cloud.tencent.com/document/product/436/31215)
- rpc 类型示例：[../rpc/](../rpc/)
- Binary plugin 驱动：[Cubelet/plugins/volume/binary/driver.go](../../../../Cubelet/plugins/volume/binary/driver.go)
- VolumePlugin 框架：[docs/zh/guide/volume-plugin.md](../../../docs/zh/guide/volume-plugin.md)
