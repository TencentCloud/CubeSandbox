# 腾讯云 COS Volume 插件 — 体验指南

本文面向**第一次使用 Volume 插件**的用户：按顺序完成下面步骤，即可用 COS 做持久化存储（创建 Volume → 挂载到沙箱 → 读写 → 解绑 → 删除）。

> **版本要求**：Cube 平台 **≥ 0.6.0**、Python SDK **`cubesandbox` ≥ 0.6.0**。  
> 协议与 Hook 细节见 [Volume 插件框架](../../docs/zh/guide/volume-plugin.md)。

**默认走 binary 插件**（`driver=cos`，Shell + coscmd + cosfs，最容易跑通）。若要用 Go rpc 插件（`driver=cos-rpc`），见文末 [rpc 路径](#rpc-路径可选)。

---

## 你将完成什么

1. 在腾讯云 COS 上创建/删除 volume 目录（管理面）
2. 创建沙箱时把 volume 挂进 microVM，文件写入 COS（数据面）
3. 用 Python SDK 跑通完整生命周期

---

## 前置条件

| 项 | 说明 |
|----|------|
| 运行中的 Cube 集群 | 至少 **CubeMaster**、**Cubelet**、**CubeAPI**（端口通常 `3000`） |
| 可用沙箱模板 | 已有 `templateID`（见 [§7](#7-用-sdk-验证) 获取） |
| 腾讯云 COS | 已创建 [Bucket](https://cloud.tencent.com/document/product/436/13309)，子账号 [API 密钥](https://cloud.tencent.com/document/product/436/7751) 对该 bucket 有读写权限 |
| 本机权限 | 能 `sudo` 在 CubeMaster / Cubelet 所在机器上安装软件、改配置、重启服务 |

**单机开发**：CubeMaster 与 Cubelet 在同一台机器上时，依赖装在一台即可。  
**多机部署**：各工具装在哪台见 [§1 安装依赖](#1-安装依赖) 表格。

---

## 1. 安装依赖

### 装在哪台机器？

| 工具 | 安装节点 | 用途（Hook） | 插件类型 |
|------|----------|--------------|----------|
| **[cosfs](https://cloud.tencent.com/document/product/436/10976)** | **Cubelet** | attach / detach（FUSE 挂载 COS） | binary、rpc **都需要** |
| **[coscmd](https://cloud.tencent.com/document/product/436/6883)** | **CubeMaster** | create / destroy（在 COS 上建/删目录） | 仅 **binary** |
| **jq** | **CubeMaster**（与 coscmd 同机） | binary 插件构造 stdout JSON | 仅 **binary** |
| COS Go SDK | 编译 rpc 插件的机器 | create / destroy | 仅 **rpc**（`go build` 时拉取，见 [rpc 路径](#rpc-路径可选)） |

> **rpc 插件**：Cubelet 节点仍须 cosfs；**不需要** coscmd / jq。Controller 逻辑在 `cube-volume-cos-rpc` 进程内，用 Go SDK 访问 COS。

### 方式 A：一键脚本（推荐）

脚本路径：`examples/volume/cos/install-deps.sh`。支持 CentOS / TencentOS / Ubuntu 等；安装结束会自动做基础 check。

**Cubelet 节点**（跑 Cubelet、执行 attach 的机器）：

```bash
cd /path/to/CubeSandbox
sudo ./examples/volume/cos/install-deps.sh --cosfs
```

**CubeMaster 节点**（跑 CubeMaster、binary 插件 create/destroy 的机器）：

```bash
sudo ./examples/volume/cos/install-deps.sh --coscmd --jq
```

**单机 binary 全套**（CubeMaster + Cubelet 同机）：

```bash
sudo ./examples/volume/cos/install-deps.sh --all
```

仅验证、不安装：

```bash
./examples/volume/cos/install-deps.sh --cosfs --check-only    # 在 Cubelet 节点执行
./examples/volume/cos/install-deps.sh --coscmd --jq --check-only  # 在 CubeMaster 节点执行
```

### 方式 B：脚本失败时 — 按腾讯云官方文档手动安装

一键脚本无法覆盖所有发行版/架构（如 ARM、特殊镜像）。**请以腾讯云文档为准**自行安装对应包，再用下方命令确认成功。

| 工具 | 腾讯云官方安装文档 |
|------|-------------------|
| cosfs | [对象存储 cosfs 工具](https://cloud.tencent.com/document/product/436/10976) |
| coscmd | [COSCMD 工具](https://cloud.tencent.com/document/product/436/6883) |
| jq | 系统包管理器：`yum install jq` / `apt install jq`（无单独腾讯云文档） |

### 安装成功怎么验？

在**对应节点**上执行（与上表「安装节点」一致）：

**Cubelet 节点 — cosfs**

```bash
ls /dev/fuse && echo "FUSE ok"
which cosfs && cosfs --version
```

两条都成功即可；缺少 `/dev/fuse` 时 attach 会失败。

**CubeMaster 节点 — coscmd**（binary）

```bash
which coscmd && coscmd --version
```

可选：用 `volume-cos.conf` 里的 bucket 测读写权限（需已配置密钥）：

```bash
source /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf
coscmd -b "$BUCKET" -r "$REGION" list /
```

**CubeMaster 节点 — jq**（binary）

```bash
which jq && jq --version
printf '%s' '{"ok":true}' | jq -r '.ok'   # 应输出 true
```

脚本安装的 cosfs / coscmd / jq 会在 `--cosfs` / `--coscmd` / `--jq` 或 `--all` 结束时自动跑上述同类检查；手动安装后请自行执行一遍。

---

## 2. 安装插件与 COS 凭证

一键部署（one-click）会把 binary 插件分别放到 **`/usr/local/services/cubetoolbox/CubeMaster/plugin/`**（Controller）与 **`/usr/local/services/cubetoolbox/Cubelet/plugin/`**（Node），并在各目录从 `volume-cos.conf.example` 生成 `volume-cos.conf`。安装后只需在对应节点编辑凭证：

```bash
# CubeMaster 节点（create / destroy）
sudo chmod 600 /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf
sudo ${EDITOR:-vi} /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf

# Cubelet 节点（attach / detach、cosfs）
sudo chmod 600 /usr/local/services/cubetoolbox/Cubelet/plugin/volume-cos.conf
sudo ${EDITOR:-vi} /usr/local/services/cubetoolbox/Cubelet/plugin/volume-cos.conf
```

同一台机器跑 CubeMaster + Cubelet 时，两处路径都在本机；分节点部署时在各自节点编辑。

**从源码手动安装**（非 one-click）时，将插件复制到上述两个 `plugin/` 目录：

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
# 然后在对应节点编辑 volume-cos.conf
```

`volume-cos.conf` 必填字段：

| 字段 | 说明 | 示例 |
|------|------|------|
| `SECRET_ID` | API 密钥 ID | `AKIDxxx` |
| `SECRET_KEY` | API 密钥 Key | `xxxxx` |
| `BUCKET` | `BucketName-APPID` | `mybucket-1250000000` |
| `REGION` | 地域 | `ap-guangzhou` |

挂载基目录**不在此文件配置**——由 Cubelet 在 attach 时传入（默认 `/data/volume`，见 [§4](#4-配置-cubelet)）。

---

## 3. 配置 CubeMaster

编辑 CubeMaster 配置（常见路径：`/usr/local/services/cubetoolbox/CubeMaster/conf.yaml`），在顶层增加 **Controller** 侧插件（Create / Destroy 时 fork 插件）：

```yaml
volume_plugins:
  - name: cos
    type: binary
    binary_path: /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos
```

说明：

- `name: cos` 即 API/SDK 里的 **`driver`**；`Volume.create("x")` 省略 driver 时，使用列表**第一项**插件。
- 仅配置 binary 时，上面一段即可；不要重复添加同名 `cos`。

保存后**先不要重启**，与 Cubelet 一起重启（[§5](#5-重启服务并确认加载成功)）。

---

## 4. 配置 Cubelet

编辑 Cubelet 配置（常见路径：`/usr/local/services/cubetoolbox/Cubelet/config/config.toml`）。

在 `[plugins."io.cubelet.internal.v1.storage"]` 段中确认挂载父目录（可选，默认已是 `/data/volume`）：

```toml
[plugins."io.cubelet.internal.v1.storage"]
  volume_plugin_base_dir = "/data/volume"
```

在同一段下增加 **Node** 侧插件（Attach / Detach）：

```toml
[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
  name        = "cos"
  type        = "binary"
  binary_path = "/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos"
```

**两侧 `name` 必须与 CubeMaster 一致**（此处均为 `cos`）。  
插件返回的 `host_path` 必须是 `volume_plugin_base_dir` 下的子路径（示例脚本挂载为 `/data/volume/cos-<volumeID>`）。

---

## 5. 重启服务并确认加载成功

```bash
sudo systemctl restart cube-sandbox-cubemaster
sudo systemctl restart cube-sandbox-cubelet
# 若使用 CubeAPI：
sudo systemctl restart cube-sandbox-cube-api

sleep 5
systemctl is-active cube-sandbox-cubemaster cube-sandbox-cubelet cube-sandbox-cube-api
```

**确认插件已加载**（重启后）：

```bash
grep -aF '[volume] registered' /data/log/CubeMaster/cubemaster-req.log | tail -5
grep -aF '[plugin_volume] initialized' /data/log/Cubelet/Cubelet-req.log | tail -5
```

期望（binary 示例）：

```text
[volume] registered binary plugin "cos" at /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos
[plugin_volume] initialized binary plugin "cos" at /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos
```

**手动测插件 attach**（在 Cubelet 节点、Cubelet mntns 外执行即可，脚本会写 cosfs passwd）：

```bash
/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-cos \
  --op attach \
  --sandbox-id test-sandbox \
  --namespace default \
  --volume-id test-vol \
  --ref-count 0 \
  --volume-base-dir /data/volume
# 成功时 stdout 一行 JSON，含 "host_path":"/data/volume/cos-test-vol", "error":""
```

---

## 6. 准备 SDK 环境

在**你的开发机**（能访问 CubeAPI 的机器）：

```bash
pip install 'cubesandbox>=0.6.0'

export CUBE_API_URL=http://<cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# 远程访问沙箱、在 volume 里读写时必填（数据面走 CubeProxy）
export CUBE_PROXY_NODE_IP=<cubeproxy-或-cubelet-节点-ip>

# 集群开启鉴权时：
# export CUBE_API_KEY=<your-key>
```

| 变量 | 含义 |
|------|------|
| `CUBE_API_URL` | CubeAPI 地址 |
| `CUBE_TEMPLATE_ID` | 创建沙箱用的模板 |
| `CUBE_PROXY_NODE_IP` | 挂载后沙箱内文件读写走数据面；不填可能导致沙箱内写 volume 失败 |

---

## 7. 用 SDK 验证

完整生命周期（创建 Volume → 挂载 → 读写 → 销毁沙箱 → 删除 Volume）：

```python
from cubesandbox import Sandbox, Volume

# ① 创建 Volume（COS 上出现 volumes/<id>/.keep）
vol = Volume.create("my-data")          # driver 省略 → 使用 conf 里第一项 cos
print("volume_id:", vol.volume_id)

# ② 创建沙箱并挂载
with Sandbox.create(
    volume_mounts={"/workspace": vol},
) as sb:
    sb.files.write("/workspace/hello.txt", "from COS volume")
    print(sb.files.read("/workspace/hello.txt"))

# ③ 退出 with → 沙箱销毁，volume 从该沙箱解绑（COS 数据仍在）

# ④ 删除 Volume（COS 上 volumes/<id>/ 被删，不可逆）
Volume.destroy(vol.volume_id)
print("done")
```

**可选：在 Cubelet mntns 里确认 cosfs 已挂载**（attach 成功后、沙箱仍存在时）：

```bash
CPID=$(pgrep -f "cubelet --config" | head -1)
nsenter -t "$CPID" -m -- cat /proc/mounts | grep cosfs
```

**可选：在 COS 侧看对象**（binary 插件 create 之后）：

```bash
source /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf
coscmd -b "$BUCKET" -r "$REGION" list "volumes/my-data/"
```

### 自动化验证（Python）

HTTP 契约、逐 driver 生命周期、多沙箱共用与负向场景，见 [`verify_volume.py`](verify_volume.py)。

**安装依赖：**

```bash
pip install 'cubesandbox>=0.6.0' requests
```

**环境变量**（与 [§6](#6-准备-sdk-环境) 相同，另可选 driver 列表）：

| 变量 | 必填 | 说明 |
|------|------|------|
| `CUBE_API_URL` | 是 | CubeAPI 地址 |
| `CUBE_TEMPLATE_ID` | 是 | 沙箱模板 ID |
| `CUBE_PROXY_NODE_IP` | 建议 | 数据面访问；在 CubeProxy 机器上可用 `127.0.0.1` |
| `CUBE_VOLUME_DRIVERS` | 否 | 逗号分隔 driver（默认 `cos`；rpc 插件用 `cos-rpc`） |
| `CUBE_VOLUME_MOUNT_PATH` | 否 | 沙箱内挂载路径（默认 `/workspace`） |
| `CUBE_API_KEY` | 否 | 集群开启鉴权时设置 |

**执行：**

```bash
cd examples/volume/cos
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=tpl-xxxx
export CUBE_PROXY_NODE_IP=127.0.0.1
export CUBE_VOLUME_DRIVERS=cos   # 或 cos-rpc

python3 verify_volume.py
```

脚本会输出分组报告（通过 / 失败 / 跳过）；任一断言失败时退出码非 0。

---

## 8. 常见问题

| 现象 | 排查 |
|------|------|
| `unknown driver: cos` | CubeMaster `volume_plugins` 未配或未重启 |
| `no plugin registered for driver "cos"` | Cubelet 未配同名插件或未重启 |
| 沙箱创建失败 / attach 报错 | Cubelet 日志搜 `[plugin_volume]`、`cosfs`；确认 cosfs、FUSE、`volume-cos.conf` |
| SDK 写文件失败 | 是否设置 `CUBE_PROXY_NODE_IP`；CubeAPI / 模板是否 READY |
| `Volume.create` 无 driver 但不是 cos | `volume_plugins` **列表顺序**：第一项才是默认 driver |

更多见 [框架指南 §8 故障排查](../../docs/zh/guide/volume-plugin.md#八调试与排障)。

---

## COS 后端布局（了解即可）

```
<bucket>/volumes/<volumeID>/   ← 每个 Volume 一个目录
```

Attach 时 cosfs 挂到宿主机 `/data/volume/cos-<volumeID>/`，再经 virtiofs 进沙箱。

### Hook 行为（RefCount）

| Hook | 侧 | refCount | 行为 |
|------|----|----------|------|
| Create | Controller | — | `coscmd upload` 创建 `volumes/<id>/.keep` |
| Destroy | Controller | — | `coscmd delete -r` 删除 COS 目录 |
| Attach | Node | `0` | `cosfs` FUSE 挂载 → 返回 `hostPath` |
| Attach | Node | `> 0` | 返回已有 `hostPath` |
| Detach | Node | `> 0` | no-op |
| Detach | Node | `0` | `fusermount -u`；**保留** COS 数据 |

脚本级细节见 [binary/README.zh.md](binary/README.zh.md)。

### 实现取舍（非框架限制）

COS binary/rpc 示例在 `volume-cos.conf` 中**固定一个 `BUCKET`**，所有 Volume 落在 `volumes/<volumeID>/` 下。多 bucket 场景常见做法是部署多份插件、配置不同 `driver` 名——这只是**示例写法**，不是 Cube Volume 框架的能力边界。

自研插件可在 `Create`/Volume 元数据中指定 bucket、在单进程内维护多 bucket 路由表、或按 `driver` 区分后端类型。框架只要求 Hook 协议与 `driver` 路由一致。

---

## rpc 路径（可选）

适合需要 **Go gRPC 长驻进程**、Controller 用 **COS Go SDK**（不用 coscmd）的场景。

| 步骤 | 与 binary 的差异 |
|------|------------------|
| 依赖 | **Cubelet 节点**：仅 [cosfs](https://cloud.tencent.com/document/product/436/10976)（[§1 安装与校验](#1-安装依赖)）；**不需要** coscmd / jq |
| 插件 | `go build` 得到 `cube-volume-cos-rpc`，并配置 systemd 常驻 |
| CubeMaster / Cubelet | `type: rpc`，`name: cos-rpc`，`socket_path: /run/cube-volume-cos-rpc.sock`（与插件 `SOCKET` 相同） |
| SDK | `Volume.create("x", driver="cos-rpc")` |

逐步说明：[rpc/README.zh.md](rpc/README.zh.md)。

---

## 目录与延伸阅读

```
examples/volume/cos/
├── install-deps.sh          # 依赖一键安装 + check
├── verify_volume.py         # Python SDK 验证脚本
├── volume-cos.conf.example
├── binary/                  # Shell 插件源码与代码导读
└── rpc/                     # Go rpc 插件
```

| 文档 | 内容 |
|------|------|
| [binary/README.zh.md](binary/README.zh.md) | 插件脚本实现细节、手动 attach/detach |
| [rpc/README.zh.md](rpc/README.zh.md) | rpc 构建、systemd、双插件并存 |
| [verify_volume.py](verify_volume.py) | Python SDK 自动化验证 |
| [Volume 插件框架](../../docs/zh/guide/volume-plugin.md) | 协议、RefCount、Hook 语义 |
