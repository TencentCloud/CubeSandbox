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

> **版本要求：** 基础 Volume 能力需要 Cube 平台 **≥ 0.6.0** 和 Python SDK **`cubesandbox` ≥ 0.6.0**。使用 `private_data` 的动态 S3 卷需要 **CubeAPI、CubeMaster、Cubelet ≥ 0.8.0**、支持该协议的插件，以及使用新 SDK 方法时 **Python/Go/Node SDK ≥ 0.8.0**：Python `Volume.create_with_private_data`、Go `Client.CreateVolumeWithPrivateData`、Node `Volume.createWithPrivateData`。请调用 `POST /v2/volumes`；这些方法不会回退到 `/volumes`。
> 协议与 Hook 细节：[Volume 插件开发框架](../../../docs/zh/guide/volume-plugin.md)。

---

## 我需要看本页吗？

| 你的情况 | 去哪 |
|----------|------|
| 默认安装（one-click / Helm 默认），想用 S3 Volume | [用户教程](../../../docs/zh/guide/s3-volume.md) —— 直接用 SDK，无需配置 |
| 想把后端从捆绑 MinIO 换成外部 S3 | 本页 [§2](#2-安装插件与凭证) 起，改 `volume-s3.conf` 即可 |
| 每个用户自带桶和凭证，不希望重新部署 | [动态桶与凭证引用](#动态桶与凭证引用) |
| 从零手动部署 S3 Volume 插件 | 本页全文 |

---

## 前置条件

| 项目 | 说明 |
|------|------|
| 运行中的 Cube 集群 | 至少包含 **CubeMaster**、**Cubelet**、**CubeAPI**（端口通常为 `3000`） |
| 沙箱模板 | 一个 `templateID`（见 [§7](#7-使用-sdk-验证)） |
| S3 兼容存储 | 一个存储桶，以及对该桶具备读写权限的访问密钥对（旧版 Volume 在桶不存在时可自动创建；动态 Volume 的桶必须已存在——见[动态桶与凭证引用](#动态桶与凭证引用)） |
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

> **第三方插件请安装到 cubetoolbox 目录之外**，否则升级 Cube 时会被重置。

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
| `ACCESS_KEY_ID` | 访问密钥 ID | 是¹˒² |
| `SECRET_ACCESS_KEY` | 密钥 | 是¹˒² |
| `CREDENTIALS` | `static`（默认）或 `instance_role`¹ | 否 |
| `BUCKET` | 默认与旧版 Volume 的存储桶；描述里省略 `bucket` 时的回退值 | 是² |
| `ENDPOINT` | S3 兼容 Endpoint 地址（见下表） | 是 |
| `REGION` | SigV4 签名地域，默认 `us-east-1` | 否 |
| `S3FS_EXTRA_OPTS` | 额外的 s3fs 挂载选项，空格分隔（如 MinIO 需要的 `-ouse_path_request_style`）。多选项值可以加引号以便该文件仍能被 `source`，插件会自行剥离引号。设置了 `-ouse_path_request_style` 时，插件自己的 S3 客户端也会切换为 path-style 寻址。 | 否 |
| `ADDRESSING_STYLE` | `auto`（默认）、`path` 或 `virtual`。控制面客户端与 s3fs 使用同一种桶寻址方式。未显式设置时，`S3FS_EXTRA_OPTS` 中的 path-style 选项会推导为 `path`；显式设置（包括 `auto`）与同一文件中的 path-style 选项冲突时会报错。 | 否 |
| `CREDENTIALS_DIR` | `<cred_ref>.conf` 所在目录；默认 `/etc/cube/s3-credentials`。见[动态桶与凭证引用](#动态桶与凭证引用)。 | 否 |
| `PASSWD_DIR` | 动态 Volume 的 s3fs 凭证文件目录；默认 `/etc/cube`。 | 否 |

¹设置 `CREDENTIALS=instance_role` 并删掉两个密钥，即可用主机的 EC2 实例角色代替静态密钥：控制面客户端通过实例元数据服务（IMDS）取凭证，s3fs 以 `-oiam_role=auto` 挂载，不写 passwd 文件，之前静态密钥留下的 passwd 文件也会被删除。设置了 instance_role 却又填了密钥会报错；`ENDPOINT` 必须是 AWS S3 地址（`*.amazonaws.com` 或 `*.amazonaws.com.cn`）。create、destroy、attach 会先取一次凭证，主机没有角色或访问不到 IMDS 时直接报 IMDS 错误。

²如果每个 Volume 都在 `private_data` 里自带 `bucket` 和 `cred_ref`，可以同时省略两个密钥和 `BUCKET`；此时缺少其中任一项的 Volume 会得到明确的报错。只填一个密钥始终是配置错误。`CREDENTIALS_DIR` 与 `PASSWD_DIR` 也可以用环境变量 `CUBE_S3_CREDENTIALS_DIR` / `CUBE_S3_PASSWD_DIR` 设置，环境变量优先。

> **哪些部署方式能用。** 只有本页的手动安装。`deploy/one-click/install.sh` 在设置了 `CUBE_S3_ENDPOINT`
> 而密钥为空时会直接退出，并且每次安装和升级都会重新生成 `volume-s3.conf`，手动加的 `CREDENTIALS` 行不会保留；Helm chart 同样要求
> `volumeS3.accessKeyId` / `secretAccessKey`，除非用 `volumeS3.existingSecret` 提供整个文件。
>
> **这是哪一种身份。** EC2 实例角色（经 IMDS），不是 AWS 的完整凭证链：`AWS_ACCESS_KEY_ID`、
> `~/.aws/credentials` 不会被使用。（如果插件进程环境里设置了 `AWS_WEB_IDENTITY_TOKEN_FILE` 或
> `AWS_CONTAINER_CREDENTIALS_*`，控制面客户端会使用它们；普通主机上不会有这些变量，s3fs 也只走 IMDS。）
> 所以上面表里的其他后端（COS、R2、MinIO）仍然需要静态密钥对。
>
> **角色要配在哪些机器上。** 每一台 CubeMaster 和 Cubelet 上都要有：create/destroy 用 CubeMaster 的身份，
> attach 用节点的身份。权限范围应当限定到 `BUCKET`——实例角色通常比前置条件要求的那把按桶授权的密钥宽得多。

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
| 动态创建报 `bucket "..." does not exist` | 动态桶从不自动创建：先建桶，或修正 `private_data` 中的 `bucket` |
| 报 `credential reference "x" is unreadable` | `<x>.conf` 必须同时存在于 CubeMaster **和** Cubelet 主机的 `CREDENTIALS_DIR` 中，由 root 所有、权限 `0600` |
| 报 `dynamic private_data requires cred_ref` | 任何非空动态描述都必须带合法 `cred_ref`；全局凭证只用于旧版 Volume |
| 报 `prefix "..." already contains objects` | 动态卷要求空前缀，否则创建回滚会删掉已有数据。换一个空前缀，或在创建完成后再导入数据 |
| 前缀重叠或删除期间复用前缀 | 插件不协调不同卷的前缀。平台必须分配互不重叠、互不嵌套的前缀，并等 Destroy 完成后再复用 |
| 动态 Volume 挂载到了错误的桶/前缀 | Cubelet 版本较旧，没有把 `private_data` 转发给 Attach；先升级 Cubelet |
| 报 `private_data must be a JSON object` | 插件无法将 `private_data` 解码为预期的 JSON 对象。检查 JSON 语法、是否重复编码、是否传入字符串或数组等非对象值，以及字段类型是否为字符串。 |
| 存储桶名包含点号 | s3fs 默认使用 virtual-hosted 风格寻址，带点号的桶名会导致 TLS 校验失败。请改用不含点号的桶名，或在 `volume-s3.conf` 中设置 `S3FS_EXTRA_OPTS=-ouse_path_request_style`（MinIO 通常也需要该选项）。凭证引用文件也可以单独设置 `ADDRESSING_STYLE=path` |
| 报 `ADDRESSING_STYLE=... conflicts with ... use_path_request_style` | 移除冲突的全局 path-style 选项；引用文件使用 `ADDRESSING_STYLE`，不能单独设置挂载参数 |
| SDK 写入失败 | 未设置 `CUBE_PROXY_NODE_IP`；CubeAPI 或模板未就绪 |
| `Volume.create` 未指定 driver 时没有走 s3 | 默认安装下 `s3` 是 `volume_plugins` 第一项（即默认 driver），省略 driver 即走 s3；若不走 s3，请检查 `volume_plugins` 顺序 |

更多内容：[框架 §8 调试与排障](../../../docs/zh/guide/volume-plugin.md)。

---

## 动态桶与凭证引用

同一个集群可以把每个 Volume 绑定到部署时未知的存储桶，并使用各自的凭证。创建 Volume 时把存储描述作为 `private_data` 传入：

```json
{"bucket":"customer-bucket","prefix":"workspaces/user-a/","cred_ref":"user-a"}
```

使用 `POST /v2/volumes`，或 SDK 的显式方法：Python `Volume.create_with_private_data`、Go `CreateVolumeWithPrivateData`、Node `Volume.createWithPrivateData`。旧接口 `POST /volumes` 行为不变。

动态卷要求 CubeAPI、CubeMaster、Cubelet ≥ 0.8.0；使用这些 SDK 方法时 Python/Go/Node SDK 也须 ≥ 0.8.0。新路由和 SDK 方法不会回退到 `POST /volumes`。

动态卷使用与旧版卷相同的基础 S3 操作：`HeadBucket`、`ListObjects`、`PutObject` 和 `RemoveObjects`，不依赖条件 PUT。已用 MinIO 与真实腾讯云 COS 桶（凭证仅限单一前缀）验证：创建、挂载、写入、Detach、重挂载、Destroy 与非空前缀拒绝均符合文档描述。

| 字段 | 必填 | 含义 |
|------|------|------|
| `bucket` | 否 | 调用方自有的、已存在的存储桶；省略时使用 `volume-s3.conf` 的 `BUCKET`。动态桶**从不自动创建**：桶不存在时创建失败。 |
| `prefix` | 否 | 桶内的 Volume 根前缀；省略时为 `volumes/<volume_id>/`，该状态能原样解析回来。会去掉开头的 `/`、补上结尾的 `/`，拒绝 `.`/`..`/空路径段。调用方不能自选 `volumes/` 下的其他前缀，也不能使用 `.cube-sandbox/`：这两个命名空间属于插件。**前缀必须是空的**：已存在对象时创建失败（见下文）。 |
| `cred_ref` | 动态描述必填 | 凭证引用文件名。动态卷不会回退到全局密钥；全局密钥只供旧版卷使用。 |

**落库的内容。** Create 会在 `private_data` 中返回解析后的状态，例如：

```json
{"v":1,"bucket":"customer-bucket","prefix":"workspaces/user-a/","cred_ref":"user-a"}
```

CubeMaster 只保存这个值；Attach 与 Destroy 接收的也是它。调用方最初的描述不会再被使用，且无论是描述还是状态都不会通过 Volume API 返回。

**凭证引用。** 在每台 CubeMaster 和 Cubelet 主机上，为每个用户放一个文件：`/etc/cube/s3-credentials/<cred_ref>.conf`（可用 `CREDENTIALS_DIR` 或 `CUBE_S3_CREDENTIALS_DIR` 修改目录）。文件由 root 所有、权限 `0600`：

```
ACCESS_KEY_ID=AKIA...
SECRET_ACCESS_KEY=...
# 可选的按引用覆盖项：
ENDPOINT=https://cos.ap-guangzhou.myqcloud.com
REGION=ap-guangzhou
ADDRESSING_STYLE=path
```

明文密钥不会进入 `private_data`、数据库、沙箱 annotation 或日志。插件只检查 `cred_ref` 格式并读取宿主机凭证文件，不会授权某租户能否使用该引用、桶或前缀；调用 API 前由平台负责授权。动态 Volume 的 s3fs 凭证文件写在 Cubelet 的 `PASSWD_DIR`（默认 `/etc/cube`）下的 `.passwd-s3fs-dynamic-<volume_id>`，位于挂载目录之外，权限 `0600`。最终 Detach 在成功卸载后删除该卷文件；文件已不存在视为清理完成，删除失败会记日志。Destroy 在 CubeMaster 上运行，不清理节点文件；该路径也绝不删除旧版按桶共享文件 `.passwd-s3fs-volume-<bucket>`。轮换引用文件后，下次 attach 生效；已经挂载的 Volume 需要重新挂载才能用上新凭证。

**腾讯云 COS 写大文件需要分片权限。** 文件达到 `multipart_size`（默认 10 MB）后，s3fs 会从单次 PUT 切换为分片上传。只授予 `GetObject`/`HeadObject`/`PutObject`/`DeleteObject` 的租户策略因此会在较大写入时失败：实测 20 MiB 写入返回 `EPERM`，s3fs 日志显示 `POST ?uploads` 被 403 `AccessDenied` 拒绝。请在同一前缀上补授 `cos:InitiateMultipartUpload`、`cos:UploadPart`、`cos:CompleteMultipartUpload` 和 `cos:AbortMultipartUpload`。补齐这四项后，同样的 20 MiB 写入可经单分片分片上传完成，桶内对象与本地内容逐字节一致。若策略无法扩展，可用 `S3FS_EXTRA_OPTS=-omultipart_size=512` 让低于该值的文件继续走单次 PUT（已用 20 MiB 写入验证），代价是超大文件的重试粒度变粗。

Attach 会在节点本地 metadata 中返回动态 passwd 路径。Detach 会将该提示与配置推导的卷路径核对，并且只删除推导出的固定路径。挂载失败后，只有确认没有活动挂载时才清理凭证文件；若挂载仍活动或状态未知，则保留该文件。

`ADDRESSING_STYLE` 取 `auto`、`path` 或 `virtual`，引用文件显式设置时覆盖全局模式。引用文件只接受密钥、endpoint、region 和寻址模式；`S3FS_EXTRA_OPTS` 统一在全局配置，引用文件中的非空值会被拒绝。引用选择 virtual 或 auto 时，会在挂载前移除继承的 path-style 选项。

**前缀归属由平台负责。** 插件不检查其他卷是否使用相同、父级或子级前缀。平台必须分配唯一、互不重叠的前缀，且直到前一次 Destroy 完成后才能复用。插件不提供分布式删除栅栏或生命周期协调。

**动态创建只接受空前缀。** 仅该前缀自己的 0 字节 `prefix/` 目录 marker 除外。不支持接管已有数据：CubeMaster 创建失败时可能调用 Destroy(B) 回滚，插件无法区分回滚清理和用户明确删除。插件会先编码并检查状态 B 大小，再访问 S3；随后检查桶已存在、前缀没有对象，最后写 marker。写 marker 失败只返回错误，不会递归删除数据。空前缀检查是预先检查，不是原子占用；平台必须串行化冲突创建并确保各卷前缀不重叠。

**Destroy 会递归删除所选前缀。** 桶本身会保留，前缀已不存在时按成功处理。由于前缀归属与删除/重建协调由平台负责，若删除的前缀与其他卷重叠，也会删除其他卷对象。动态创建不支持已有数据集；当前没有 adopt 或 mount-only 模式。

**Unicode 状态编码。** 插件保留将状态 B 中补充字符写成 JSON 代理对转义的逻辑；Attach 和 Destroy 会在插件内部解码回原前缀。这使返回状态文本保持在 BMP 内，兼容现有 utf8mb3 数据库连接。CubeAPI、CubeMaster 和 SDK 创建方法会拒绝外层 JSON 解码后的 `private_data` 字符串中含有 U+FFFF 以上字符，因此调用方不能在 A 中直接传这些字符。由 ASCII 字符组成的字面量 `\uD83D\uDE00` 仍按原样传递，框架不会再解析该字符串。最终编码后的 B 仍须不超过 1024 字节；一个补充字符代理对转义占 12 个 ASCII 字节。

**兼容性与升级顺序。** 基础 Volume 能力要求 Cube ≥ 0.6.0。动态 `private_data` 要求 CubeAPI、所有服务卷操作的 CubeMaster 与 Cubelet ≥ 0.8.0，且所选插件支持该协议；使用新 SDK 方法还要求 SDK ≥ 0.8.0。升级顺序：插件（CubeMaster **和** Cubelet）→ 所有 CubeMaster → CubeAPI → SDK。新 API 和 SDK 方法不会回退到 `/volumes`。旧版 Volume 仍使用 `volumes/<id>/` 状态和全局凭证。

---

## 后端存储布局

```
<bucket>/volumes/<volumeID>/       ← 旧版 Volume（创建时未传 private_data）
<bucket>/workspaces/user-a/...     ← 动态 Volume（前缀来自 private_data）
```

attach 时用 s3fs 把 `BUCKET:/<前缀>` 挂载到宿主机的 `/data/cube-shared/volume/s3-<volumeID>/`，Cubelet 再通过 virtiofs 暴露给 microVM。旧版 Volume 的前缀是 `volumes/<volumeID>`；动态 Volume 使用解析后状态中的前缀。

### Hook 行为（RefCount）

| Hook | 角色 | refCount | 行为 |
|------|------|----------|------|
| Create | Controller | — | 旧版：若桶不存在则创建，然后 PUT 0 字节的 `volumes/<id>/` 对象。动态：要求桶已存在且前缀为空，然后 PUT 前缀目录对象（s3fs 目录对象） |
| Destroy | Controller | — | 列举并递归删除该前缀下的所有对象；保留桶本身 |
| Attach | Node | `0` | 执行 `s3fs` 挂载并返回 `host_path` |
| Attach | Node | `> 0` | 返回已有 `host_path`，不重复挂载 |
| Detach | Node | `> 0` | 空操作 |
| Detach | Node | `0` | 执行 `fusermount -u`；**保留**存储桶中的数据 |

### 设计说明

- **默认单桶，一个集群可多桶。** 不传 `private_data` 时，所有 Volume 都位于配置的 `BUCKET` 下、使用 `volumes/<id>/` 前缀。传 `private_data` 时，每个 Volume 指定桶、前缀和必需的凭证引用，无需重新部署或重启插件。不传 `private_data` 的多桶场景仍可通过多个插件实例、不同 `driver` 名实现。
- **不依赖任何 S3 命令行工具。** 控制面在插件二进制内使用 [minio-go](https://github.com/minio/minio-go)：每个控制节点只需一个约 8MB 的静态二进制，而不是约 100MB 的 AWS CLI 安装。
- **仅旧版 Volume 自动建桶。** Create 会先检查桶是否存在。桶已存在时**不需要** `s3:CreateBucket`；只有桶还不存在时插件才会创建（内置 MinIO 的典型情况）。动态 Volume 的桶由用户所有，必须已存在。
- **Destroy 只容忍 not-found。** 桶或 key 不存在说明前缀已经没了；其他任何错误都会向上传播，避免 CubeMaster 删掉 Volume 记录而桶里对象仍然残留。
- **凭证不会进入沙箱。** 凭证保存在 CubeMaster/Cubelet 上 root 所有、权限 `600` 的配置或凭证引用文件中，microVM 只看到一个文件系统；`private_data` 只携带引用，不携带密钥。
- **`private_data` 携带绑定关系**（Create → Attach / Destroy），须为有效 UTF-8、不超过 1024 字节、不含 NUL 且不超出 U+FFFF，不会返回给 SDK 客户端。插件把调用方输入当作请求，把插件返回的状态当作唯一事实来源。
- **平台负责授权与协调。** 插件校验 `cred_ref` 格式并读取其凭证文件，但不授权租户是否能使用该引用、桶或前缀。平台必须分配唯一、互不重叠的前缀，串行化冲突创建，并在 Destroy 完成后才复用前缀。
- **前缀生命周期。** 动态 Create 要求桶事先存在且前缀为空，不会接管已有数据。Destroy 递归删除选定前缀但不删桶。已有数据集需等独立的 adopt/mount-only 设计。
- **节点凭证清理。** 最终 Detach 成功卸载后删除该卷的动态 passwd 文件；文件不存在视为已清理，删除失败记日志。旧版按桶共享的 passwd 文件不会被删除。
- **并发控制。** 按 Volume 粒度的 `flock` 串行化同一节点上同一 Volume 的 attach/detach，避免两个沙箱同时启动导致重复挂载。
- **Destroy 不可恢复**，会删除整个 Volume 前缀。API 的引用计数保护（有沙箱持有 Volume 时 `DELETE /volumes` 返回 409）是防止删除已挂载 Volume 的机制 —— 真正的风险点是节点崩溃后引用计数失真：集群级计数可能归零而某节点仍持有挂载。

---

## 目录结构

```
examples/volume/s3/
├── Makefile                       # build / fmt / lint / test
├── install-deps.sh                # 宿主依赖安装与检查（s3fs / jq）
├── volume-s3.conf.example
├── cmd/cube-volume-s3/main.go     # flag 解析、Hook 分发、stdout JSON
└── internal/
    ├── binding/                   # private_data 解析/校验与前缀规则
    ├── config/                    # volume-s3.conf 与凭证引用解析
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
