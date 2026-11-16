# COS Volume 插件（rpc）

**插件类型**：`rpc`（长驻 gRPC 进程；传输层 `socket_path`：Unix / TCP）  
**driver 名**：`cos-rpc`

| 侧 | Hook | 本示例依赖 |
|----|------|------------|
| CubeMaster（Controller） | Create / Destroy | [COS Go SDK](https://cloud.tencent.com/document/product/436/31215) |
| Cubelet（Node） | Attach / Detach | [cosfs](https://cloud.tencent.com/document/product/436/6883) |
| CubeMaster（Content，可选） | IssueCredentials | [COS STS `GetFederationToken`](https://cloud.tencent.com/document/product/436/14048) |

与 [binary](../binary/) 对比：Controller 用 **Go SDK** 替代 **coscmd**；Node 仍用 **cosfs**。

> 共用说明（COS 布局、cosfs 安装、凭证）：[`../README.zh.md`](../README.zh.md)  
> 框架原理：[docs/zh/guide/volume-plugin.md](../../../../docs/zh/guide/volume-plugin.md)

> **说明**  
> 本示例**仅演示 rpc 插件用法**；Attach / Detach 须在 Cubelet 节点本地执行，生产环境 gRPC server 也应部署在 Node 上。  
> **下文以单机部署为例**（CubeMaster 与 Cubelet 同机）；多节点时请按实际角色分别部署。

---

## 体验前准备

| 步骤 | 内容 |
|------|------|
| 1 | 完成 [../README.zh.md — 前置条件](../README.zh.md#前置条件) |
| 2 | **Cubelet 节点**安装 cosfs：[§1](../README.zh.md#1-安装依赖)（脚本或[官方文档](https://cloud.tencent.com/document/product/436/6883)） |
| 3 | **Go 1.24+**（[go.mod](go.mod) 声明 `go 1.24.8`），编译本目录 `cube-volume-cos-rpc` |
| 4 | 配置 CubeMaster + Cubelet `socket_path` 与插件 `SOCKET`（`/run/cube-volume-cos-rpc.sock`） |
| 5 | SDK：`Volume.create(..., driver="cos-rpc")` 验证 |

**本示例不使用 coscmd**；Create/Destroy 由 COS Go SDK 完成（`go mod` 拉取，见 [31215](https://cloud.tencent.com/document/product/436/31215)）。

---

### 2.1 COS STS（可选：SDK 直连 COS）

`IssueCredentials`（`internal/cosapi/content.go`）调用 COS STS
[`GetFederationToken`](https://cloud.tencent.com/document/product/436/14048)
签发限定到单卷对象前缀（`volumes/<id>/*`）的临时凭证，TTL 封顶为 CubeMaster
的 15 分钟上限。需要：

- **APPID**：可在 `volume-cos.conf` 显式配置，也可从 `BUCKET` 的
  `-<数字>` 后缀自动推导（如 `mybucket-1250000000` 中的 `1250000000`）。
- **CAM 权限**：`SECRET_ID`/`SECRET_KEY` 需有 `sts:GetFederationToken`
  权限（主账号/管理员身份默认具备；生产环境建议使用限权子账号，参考
  [子账号密钥](https://cloud.tencent.com/document/product/598/37140)）。

CubeMaster 侧若未开启 `content_enabled: true`（见 §4），该 Hook 永远不会
被调用，Create/Destroy/Attach/Detach 不受影响。

---

## 1. cosfs（Cubelet 节点）

Attach/Detach 通过 cosfs 挂载。安装与校验见 [../README.zh.md §1](../README.zh.md#1-安装依赖)。

```bash
sudo /usr/local/services/cubetoolbox/Cubelet/plugin/install-deps.sh --cosfs
ls /dev/fuse && which cosfs && cosfs --version
```

官方文档：[cosfs 工具](https://cloud.tencent.com/document/product/436/6883)

---

## 2. COS Go SDK（Controller 侧）

- 依赖已在 [go.mod](go.mod)（`github.com/tencentyun/cos-go-sdk-v5`）
- 密钥、bucket、地域：`/usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf`（与 rpc 二进制同目录；Cubelet 节点 attach 用 cosfs 时也可在 `Cubelet/plugin/volume-cos.conf` 备一份）
- 国内可设 `GOPROXY=https://goproxy.cn,direct`

本地开发 `replace` Cubelet 模块（路径相对于本目录）：

```go
require github.com/tencentcloud/CubeSandbox/Cubelet v0.0.0

replace github.com/tencentcloud/CubeSandbox/Cubelet => ../../../../Cubelet
```

---

## 3. 构建与启动

需要 **Go 1.24+**（`go.mod` 声明 `go 1.24.8`）。

> Kubernetes / Terraform 部署时，`volume-cos.conf` 由用户自行按集群原生方式完成；下文以裸机路径为例。

```bash
PREFIX=/usr/local/services/cubetoolbox/CubeMaster/plugin
mkdir -p "$PREFIX"
cd examples/volume/cos/rpc
go build -o "$PREFIX/cube-volume-cos-rpc" ./cmd/cube-volume-cos-rpc

sudo cp volume-cos.conf.example "$PREFIX/volume-cos.conf"
# 编辑 SECRET_ID, SECRET_KEY, BUCKET, REGION；示例已含 SOCKET
sudo chmod 600 "$PREFIX/volume-cos.conf"

"$PREFIX/cube-volume-cos-rpc" serve
```

**systemd 示例**：

```ini
[Unit]
Description=COS Volume Plugin (rpc)
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/services/cubetoolbox/CubeMaster/plugin/cube-volume-cos-rpc serve
EnvironmentFile=-/usr/local/services/cubetoolbox/CubeMaster/plugin/volume-cos.conf
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

unit 文件须先安装再 `enable`（一行 `serve &` + `systemctl` 会报 *Unit … could not be found*）：

```bash
sudo cp cube-volume-cos-rpc.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now cube-volume-cos-rpc
```

`volume-cos.conf` 中的 `SOCKET` 须与 CubeMaster / Cubelet 的 `socket_path` **完全相同**（标准值：`/run/cube-volume-cos-rpc.sock`）。

实现：[internal/grpcsrv/server.go](internal/grpcsrv/server.go)

---

## 4. 配置 CubeMaster 与 Cubelet

**CubeMaster** `conf.yaml`：

```yaml
volume_plugins:
  - name: cos-rpc
    type: rpc
    socket_path: /run/cube-volume-cos-rpc.sock
    # 可选：开启 VolumeContentService（POST /volumes/{id}/content-token），
    # 让 Python SDK 无需沙箱直连 COS。需要 APPID（或可推导的数字 BUCKET
    # 后缀）及 SECRET_ID/SECRET_KEY 的 sts:GetFederationToken 权限，见 §2.1。
    content_enabled: true
```

**Cubelet** `config.toml`：

```toml
[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
  name        = "cos-rpc"
  type        = "rpc"
  socket_path = "/run/cube-volume-cos-rpc.sock"
```

可与 binary 插件并存（driver 分别为 `cos` / `cos-rpc`），见 [binary/README.zh.md](../binary/README.zh.md)。

修改配置后重启 CubeMaster 与 Cubelet，并确认插件已加载（[§5](#5-端到端验证)）。

---

## 5. 端到端验证

重启后查启动日志（大文件请用 `grep -a`）：

```bash
# CubeMaster — 每个插件一行
grep -aF '[volume] registered' /data/log/CubeMaster/cubemaster-req.log | tail -5

# Cubelet — Init 成功后每个插件一行
grep -aF '[plugin_volume] initialized' /data/log/Cubelet/Cubelet-req.log | tail -5
```

本示例期望：

```text
[volume] registered rpc plugin "cos-rpc" at /run/cube-volume-cos-rpc.sock
[plugin_volume] initialized rpc plugin "cos-rpc" at /run/cube-volume-cos-rpc.sock
```

另确认 rpc 守护进程：`systemctl is-active cube-volume-cos-rpc`。

### SDK 冒烟测试

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-data", driver="cos-rpc")

sb = Sandbox.create(
    volume_mounts={"/mnt/data": vol},
)
try:
    sb.files.write("/mnt/data/hello.txt", "via cos-rpc")
    print(sb.files.read("/mnt/data/hello.txt"))
finally:
    sb.kill()

Volume.destroy(vol.volume_id)
```

### 直连 S3 风格访问冒烟测试（无需沙箱）

需要开启 `content_enabled: true`（§4）并完成 STS 配置（§2.1）：

```python
from cubesandbox import Volume

vol = Volume.create("my-data", driver="cos-rpc")
try:
    vol.write_file("hello.txt", "via cos-rpc content-token")
    print(vol.read_file("hello.txt"))
    print([entry.name for entry in vol.list("/")])
finally:
    Volume.destroy(vol.volume_id)
```

需要 SDK 的可选依赖：`pip install "cubesandbox[s3]"`。

---

## 与 binary 对比

| | [binary](../binary/) | rpc（本示例） |
|---|---------------------|---------------|
| 类型 | binary | rpc |
| Controller | [coscmd](https://cloud.tencent.com/document/product/436/10976) | [COS Go SDK](https://cloud.tencent.com/document/product/436/31215) |
| Node | [cosfs](https://cloud.tencent.com/document/product/436/6883) | [cosfs](https://cloud.tencent.com/document/product/436/6883) |
| driver | `cos` | `cos-rpc` |

cosfs 排障、mntns 验证见 [binary/README.zh.md](../binary/README.zh.md#验证挂载在-cubelet-mntns-里查看)。

## 参考链接

- [cos 共用文档](../README.zh.md)
- [cosfs](https://cloud.tencent.com/document/product/436/6883) · [COS Go SDK](https://cloud.tencent.com/document/product/436/31215)
- 协议：`pkgs/proto/services/volumeplugin/v1/volumeplugin.proto`
- [Volume 插件框架](../../../../docs/zh/guide/volume-plugin.md)
