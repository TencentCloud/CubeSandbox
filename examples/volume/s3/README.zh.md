# S3 兼容 Volume 插件

把任意 S3 兼容对象存储（AWS S3、腾讯云 COS、Cloudflare R2、MinIO……）接入 CubeSandbox，作为沙箱的**跨生命周期持久卷**：创建 Volume → 挂到沙箱里读写 → 沙箱销毁后数据仍在 → 再挂回来继续用。

> **本页是"操作参考"**：面向需要**手动部署插件 / 接入外部 S3** 的运维人员。
>
> **如果你用的是默认安装（one-click 或 Helm 默认值）** —— 安装脚本已经自动起好 MinIO、装好插件、写好凭证，你**不需要看本页**，直接看用户教程：[S3 持久卷](../../../docs/zh/guide/s3-volume.md)。

English: [README.md](README.md)

---

## 这是什么

CubeSandbox 的沙箱默认是临时的，销毁即丢数据。Volume 插件给沙箱一个**用户级持久卷**：通过 e2b 兼容的 `/volumes` API 创建/挂载/卸载/删除，后端是对象存储。本插件是 S3 兼容后端的实现。

插件本体是一个内置 S3 客户端的静态 Go 二进制，控制面无需任何 S3 命令行工具；数据面挂载仍使用通用的 **s3fs**。不绑定任何厂商——后端只是配置文件里的一个 `ENDPOINT`。

**与 COS 插件的关系：** 本插件参照 COS 插件实现，区别是后端从腾讯云 COS 换成任意 S3 兼容 Endpoint，且挂载驱动 s3fs 同时支持 `amd64` 和 `arm64`（cosfs 仅 `amd64`）。两者可在同一集群并存（默认安装就同时注册了 `cos` 和 `s3`）。

> **版本要求：** Cube 平台 **≥ 0.6.0**，Python SDK **`cubesandbox` ≥ 0.6.0**。
> 协议与 Hook 细节：[Volume 插件开发框架](../../../docs/zh/guide/volume-plugin.md)。

---

## 我需要看本页吗？

| 你的情况 | 去哪 |
|----------|------|
| 默认安装（one-click / Helm 默认），想用 S3 Volume | [用户教程](../../../docs/zh/guide/s3-volume.md) —— 直接用 SDK，无需配置 |
| 想把后端从捆绑 MinIO 换成外部 S3 | 本页 [§2](#2-安装插件与凭证) 起，改 `volume-s3.conf` 即可 |
| 从零手动部署 S3 Volume 插件 | 本页全文 |

---

## 前置条件

| 项目 | 说明 |
|------|------|
| 运行中的 Cube 集群 | 至少包含 **CubeMaster**、**Cubelet**、**CubeAPI**（端口通常为 `3000`） |
| 沙箱模板 | 一个 `templateID`（见 [§7](#7-使用-sdk-验证)） |
| S3 兼容存储 | 一个存储桶，以及对该桶具备读写权限的访问密钥对（桶不存在时可自动创建） |
| 本地权限 | 在 CubeMaster / Cubelet 主机上具备 `sudo`，用于安装软件、修改配置、重启服务 |

**单机开发：** CubeMaster 与 Cubelet 在同一台主机，只需安装一次依赖。
**多节点：** 见 [§1](#1-安装依赖) 中的表格。

---

## 1. 安装依赖

### 装在哪台机器

| 工具 | 安装节点 | 用途（Hook） |
|------|----------|--------------|
| **[s3fs](https://github.com/s3fs-fuse/s3fs-fuse)** | **Cubelet** | attach / detach（FUSE 挂载） |
| **jq** | 任意节点（可选） | 手工排查时阅读插件输出 |

**只跑 CubeMaster** 的节点无需本节任何工具：create / destroy 由插件二进制自己通过 HTTP 访问 Endpoint。

### 方式 A：安装脚本

**Cubelet 节点：**

```bash
sudo ./install-deps.sh --s3fs
```

**单机（两个角色同机），并额外装上便于排查的 jq：**

```bash
sudo ./install-deps.sh --all
```

只检查不安装：追加 `--check-only`。

### 方式 B：手动安装

```bash
# Cubelet —— Debian/Ubuntu
sudo apt-get install -y s3fs
# Cubelet —— RHEL/CentOS（需要 EPEL）
sudo yum install -y epel-release && sudo yum install -y s3fs-fuse
```

### 验证安装

**Cubelet —— s3fs**

```bash
ls /dev/fuse && echo "FUSE ok"
s3fs --version | head -1
```

两条都必须成功；缺少 `/dev/fuse` 会导致 attach 失败。

**CubeMaster —— 验证凭证能访问存储桶**

直接用插件的 `create` Hook 验证：凭证错误、Endpoint 写错或权限不足都会明确报错。

```bash
/usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-s3 \
  --op create --volume-id preflight-check --name preflight
/usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-s3 \
  --op destroy --volume-id preflight-check
```

两条都必须输出 `"error":""` 且退出码为 0。出现 `InvalidAccessKeyId` 或 `AccessDenied` 说明密钥对缺少该桶的读写权限。

---

## 2. 编译并安装插件与凭证

先编译。在仓库根目录执行（走项目 builder 镜像，本机无需 Go 工具链）：

```bash
make cube-volume-s3          # 产物：_output/bin/cube-volume-s3
```

或使用本机 Go 工具链（≥ 1.25）：

```bash
cd examples/volume/s3 && make    # 产物：bin/cube-volume-s3
```

one-click 发布包与容器镜像已内置编译好的二进制，位于
`<prefix>/{CubeMaster,Cubelet}/plugin/cube-volume-s3`。

把二进制安装到两个 `plugin/` 目录：

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

然后在每个节点上编辑 `volume-s3.conf`：

| 字段 | 说明 | 是否必填 |
|------|------|----------|
| `ACCESS_KEY_ID` | 访问密钥 ID | 是 |
| `SECRET_ACCESS_KEY` | 密钥 | 是 |
| `BUCKET` | 存放所有 Volume 的存储桶 | 是 |
| `ENDPOINT` | S3 兼容 Endpoint 地址（见下表） | 是 |
| `REGION` | SigV4 签名地域，默认 `us-east-1` | 否 |
| `S3FS_EXTRA_OPTS` | 额外的 s3fs 挂载选项，空格分隔（如 MinIO 需要的 `-ouse_path_request_style`）。多选项值可以加引号以便该文件仍能被 `source`，插件会自行剥离引号。设置了 `-ouse_path_request_style` 时，插件自己的 S3 客户端也会切换为 path-style 寻址。 | 否 |

常见后端：

| 存储提供方 | `ENDPOINT` | `REGION` |
|-----------|-----------|----------|
| AWS S3 | `https://s3.<region>.amazonaws.com` | 存储桶所在地域 |
| 腾讯云 COS | `https://cos.<region>.myqcloud.com` | 存储桶所在地域（如 `ap-guangzhou`） |
| Cloudflare R2 | `https://<account-id>.r2.cloudflarestorage.com` | `auto` |
| MinIO | `http://<minio-host>:9000` | 任意值 |

该配置文件必须为 root 所有、权限 `600` —— 其中以明文保存密钥。插件按 `KEY=VALUE` 解析该文件（不再 `source` 执行），默认在二进制同目录下查找 `volume-s3.conf`（可用 `CUBE_S3_CONFIG` 覆盖）：

```bash
sudo chown root:root "$PREFIX/CubeMaster/plugin/volume-s3.conf" "$PREFIX/Cubelet/plugin/volume-s3.conf"
sudo chmod 600 "$PREFIX/CubeMaster/plugin/volume-s3.conf" "$PREFIX/Cubelet/plugin/volume-s3.conf"
```

挂载根目录**不在**此文件中配置 —— Cubelet 在 attach 时传入（默认 `/data/cube-shared/volume`，见 [§4](#4-配置-cubelet)）。

---

## 3. 配置 CubeMaster

编辑 CubeMaster 配置（常见路径：`/usr/local/services/cubetoolbox/CubeMaster/conf.yaml`），添加 **Controller** 插件（Create / Destroy）：

```yaml
volume_plugins:
  - name: s3
    type: binary
    binary_path: /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-s3
```

`name: s3` 即 API/SDK 中的 **`driver`**。当 `Volume.create("x")` 未指定 driver 时，使用列表中的**第一项**——默认安装现已把 `s3` 排在第一，所以省略 driver 即走 S3。

---

## 4. 配置 Cubelet

编辑 Cubelet 配置（常见路径：`/usr/local/services/cubetoolbox/Cubelet/config/config.toml`）。

确认挂载父目录（可选，下方为默认值）：

```toml
[plugins."io.cubelet.internal.v1.storage"]
  volume_plugin_base_dir = "/data/cube-shared/volume"
```

添加 **Node** 插件（Attach / Detach）：

```toml
[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
  name        = "s3"
  type        = "binary"
  binary_path = "/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-s3"
```

**`name` 必须与 CubeMaster 一致**（此处均为 `s3`）。插件返回的 `host_path` 形如 `<volume_plugin_base_dir>/s3-<volumeID>`，满足框架对 `host_path` 必须位于 `volumeBaseDir` 之内的要求。

---

## 5. 重启服务并验证

```bash
sudo systemctl restart cube-sandbox-cubemaster
sudo systemctl restart cube-sandbox-cubelet
sudo systemctl restart cube-sandbox-cube-api

sleep 5
systemctl is-active cube-sandbox-cubemaster cube-sandbox-cubelet cube-sandbox-cube-api
```

**确认插件已加载：**

```bash
grep -aF '[volume] registered' /data/log/CubeMaster/cubemaster-req.log | tail -5
grep -aF '[plugin_volume] initialized' /data/log/Cubelet/Cubelet-req.log | tail -5
```

预期输出：

```text
[volume] registered binary plugin "s3" at /usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-s3
[plugin_volume] initialized binary plugin "s3" at /usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-s3
```

**手动 attach 测试**（在 Cubelet 节点执行）：

```bash
/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-s3 \
  --op attach \
  --sandbox-id test-sandbox \
  --namespace default \
  --volume-id test-vol \
  --ref-count 0 \
  --volume-base-dir /data/cube-shared/volume
```

成功时 stdout 输出一行 JSON，包含 `"host_path":"/data/cube-shared/volume/s3-test-vol"` 与 `"error":""`。

手动测试后清理：

```bash
/usr/local/services/cubetoolbox/Cubelet/plugin/cube-volume-s3 \
  --op detach --sandbox-id test-sandbox --namespace default \
  --volume-id test-vol --ref-count 0 \
  --metadata '{"mount_dir":"/data/cube-shared/volume/s3-test-vol"}'
```

---

## 6. 准备 SDK 环境

在能访问 CubeAPI 的**开发机**上：

```bash
pip install 'cubesandbox>=0.6.0'

export CUBE_API_URL=http://<cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# 远程访问已挂载 Volume 时必需（数据面经由 CubeProxy）
export CUBE_PROXY_NODE_IP=<cubeproxy-or-cubelet-node-ip>

# 集群开启鉴权时：
# export CUBE_API_KEY=<your-key>
```

---

## 7. 使用 SDK 验证

```python
from cubesandbox import Sandbox, Volume

# ① 创建 Volume（存储桶中生成 s3fs 目录对象 volumes/<id>/）
vol = Volume.create("my-data", driver="s3")
print("volume_id:", vol.volume_id)

# ② 创建带挂载的沙箱
with Sandbox.create(volume_mounts={"/workspace": vol}) as sb:
    sb.files.write("/workspace/hello.txt", "from S3 volume")
    print(sb.files.read("/workspace/hello.txt"))

# ③ 退出 with → 沙箱销毁，Volume 卸载（存储桶中数据保留）

# ④ 删除 Volume（存储桶前缀被移除，不可恢复）
Volume.destroy(vol.volume_id)
print("done")
```

**确认对象已写入存储桶。** 任何 S3 客户端都可以，[MinIO 的 `mc`](https://min.io/docs/minio/linux/reference/minio-mc.html) 是单文件二进制、无需 Python：

```bash
source /usr/local/services/cubetoolbox/CubeMaster/plugin/volume-s3.conf
mc alias set cube "$ENDPOINT" "$ACCESS_KEY_ID" "$SECRET_ACCESS_KEY"
mc ls --recursive "cube/$BUCKET/volumes/"
```

**在 Cubelet 挂载命名空间中确认 s3fs 挂载**（沙箱运行期间）：

```bash
CPID=$(pgrep -f "cubelet --config" | head -1)
nsenter -t "$CPID" -m -- cat /proc/mounts | grep s3fs
```

### 自动化验证

COS 示例中的 [`verify_volume.py`](../cos/verify_volume.py) 与 driver 无关，可直接指向本 driver：

```bash
cd ../cos
export CUBE_API_URL=http://127.0.0.1:3000
export CUBE_TEMPLATE_ID=tpl-xxxx
export CUBE_PROXY_NODE_IP=127.0.0.1
export CUBE_VOLUME_DRIVERS=s3
# 脚本默认跳过 cfs/s3/nfs 这几个 driver 名（COS 演示环境中未部署）；
# 本环境已部署 s3，需清空跳过列表：
export CUBE_VOLUME_SKIP_DRIVERS=

python3 verify_volume.py
```

---

## 8. 故障排查

| 现象 | 排查方向 |
|------|----------|
| `unknown driver: s3` | CubeMaster `volume_plugins` 未配置，或未重启 |
| `no plugin registered for driver "s3"` | Cubelet 缺少同名插件，或未重启 |
| attach 失败，提示 `s3fs mount failed` | 检查 `ls /dev/fuse`、`volume-s3.conf` 中的凭证与 `ENDPOINT`；执行 [§5](#5-重启服务并验证) 的手动 attach 查看 s3fs 报错 |
| attach 失败，s3fs 日志对 `volumes/<id>/` 报 `NoSuchKey` | Create 必须 PUT 带尾斜杠的目录对象（等同 s3fs mkdir）。前缀下的 `.keep` 是另一个 key。若机器上还是旧插件（只写 `.keep`），需要升级 |
| `open config ...: no such file or directory` | `volume-s3.conf` 必须与插件二进制同目录，或用 `CUBE_S3_CONFIG` 指向它 |
| `InvalidAccessKeyId` / `SignatureDoesNotMatch` | 密钥对错误、缺少桶权限，或 `REGION` 与 Endpoint 期望的 SigV4 地域不匹配 |
| 存储桶名包含点号 | s3fs 默认使用 virtual-hosted 风格寻址，带点号的桶名会导致 TLS 校验失败。请改用不含点号的桶名，或在 `volume-s3.conf` 中设置 `S3FS_EXTRA_OPTS=-ouse_path_request_style`（MinIO 通常也需要该选项） |
| SDK 写入失败 | 未设置 `CUBE_PROXY_NODE_IP`；CubeAPI 或模板未就绪 |
| `Volume.create` 未指定 driver 时没有走 s3 | 默认安装下 `s3` 是 `volume_plugins` 第一项（即默认 driver），省略 driver 即走 s3；若不走 s3，请检查 `volume_plugins` 顺序 |

更多内容：[框架 §8 调试与排障](../../../docs/zh/guide/volume-plugin.md)。

---

## 后端存储布局

```
<bucket>/volumes/<volumeID>/   ← 每个 Volume 一个前缀
```

attach 时用 s3fs 把 `BUCKET:/volumes/<volumeID>` 挂载到宿主机的 `/data/cube-shared/volume/s3-<volumeID>/`，Cubelet 再通过 virtiofs 暴露给 microVM。

### Hook 行为（RefCount）

| Hook | 角色 | refCount | 行为 |
|------|------|----------|------|
| Create | Controller | — | 若桶不存在则创建，然后 PUT 0 字节的 `volumes/<id>/` 对象（s3fs 目录对象） |
| Destroy | Controller | — | 列举并删除该前缀下的所有对象 |
| Attach | Node | `0` | 执行 `s3fs` 挂载并返回 `host_path` |
| Attach | Node | `> 0` | 返回已有 `host_path`，不重复挂载 |
| Detach | Node | `> 0` | 空操作 |
| Detach | Node | `0` | 执行 `fusermount -u`；**保留**存储桶中的数据 |

### 设计说明

- **单桶、每 Volume 一个前缀。** 与 COS 示例保持一致。多桶场景通常部署多个插件实例并使用不同 `driver` 名，或扩展 `Create` 使其接受桶名。框架只要求 Hook 协议与 `driver` 命名一致。
- **不依赖任何 S3 命令行工具。** 控制面在插件二进制内使用 [minio-go](https://github.com/minio/minio-go)：每个控制节点只需一个约 8MB 的静态二进制，而不是约 100MB 的 AWS CLI 安装。
- **自动建桶。** Create 会先检查桶是否存在。桶已存在时**不需要** `s3:CreateBucket`；只有桶还不存在时插件才会创建（内置 MinIO 的典型情况）。
- **Destroy 只容忍 not-found。** 桶或 key 不存在说明前缀已经没了；其他任何错误都会向上传播，避免 CubeMaster 删掉 Volume 记录而桶里对象仍然残留。
- **凭证不会进入沙箱。** 凭证保存在 CubeMaster/Cubelet 上 root 所有、权限 `600` 的配置文件中，microVM 只看到一个文件系统。
- **`private_data` 把对象前缀从 Create 传递到 Attach**（上限 1024 字节，不会返回给 SDK 客户端）。
- **并发控制。** 按 Volume 粒度的 `flock` 串行化同一节点上同一 Volume 的 attach/detach，避免两个沙箱同时启动导致重复挂载。
- **Destroy 不可恢复**，会删除整个 `volumes/<id>/` 前缀。API 的引用计数保护（有沙箱持有 Volume 时 `DELETE /volumes` 返回 409）是防止删除已挂载 Volume 的机制 —— 真正的风险点是节点崩溃后引用计数失真：集群级计数可能归零而某节点仍持有挂载。

---

## 目录结构

```
examples/volume/s3/
├── Makefile                       # build / fmt / lint / test
├── install-deps.sh                # 宿主依赖安装与检查（s3fs / jq）
├── volume-s3.conf.example
├── cmd/cube-volume-s3/main.go     # flag 解析、Hook 分发、stdout JSON
└── internal/
    ├── config/                    # volume-s3.conf 解析
    ├── s3api/                     # 基于 minio-go 的 create / destroy
    ├── s3fsmnt/                   # s3fs 挂载 / 卸载
    └── lockfile/                  # 跨进程的 per-Volume flock
```

运行单元测试（无需访问云端）：

```bash
cd examples/volume/s3 && make test
# 或在仓库根目录用项目 builder 镜像：
make cube-volume-s3-test
```

| 文档 | 内容 |
|------|------|
| [S3 持久卷（用户教程）](../../../docs/zh/guide/s3-volume.md) | 面向最终用户的快速上手 |
| [Volume 插件开发框架](../../../docs/zh/guide/volume-plugin.md) | 协议、RefCount、Hook 语义 |
| [COS 示例](../cos/README.zh.md) | 本插件所参照的参考实现 |
| [s3fs-fuse](https://github.com/s3fs-fuse/s3fs-fuse) | 挂载驱动选项与行为 |
