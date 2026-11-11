# Volume 插件开发指南

CubeSandbox 正在逐步兼容 e2b Volume，为沙箱提供跨生命周期的持久化存储。本文档从**整体架构与核心原理**出发，逐步深入到**协议细节与插件开发**，帮助你将任意存储后端（对象存储、NFS、分布式文件系统等）接入 CubeSandbox。

> **版本要求**
>
> 基础 Volume 能力需要 **Cube 平台 ≥ 0.6.0**（CubeMaster、CubeAPI、Cubelet 均需升级），Python Volume API 还需 **Python SDK `cubesandbox` ≥ 0.6.0**。通过插件 `private_data` 创建卷需要 **CubeAPI、CubeMaster、Cubelet ≥ 0.8.0**、所选插件支持该协议，以及使用 SDK 方法时 **Python/Go/Node SDK ≥ 0.8.0**。该功能使用 `POST /v2/volumes`，不会回退到 `POST /volumes`。

> **当前进展**（API / SDK）
>
> | 能力 | 状态 |
> |------|------|
> | REST `GET /volumes` — 列出 Volume | ✅ 已支持（Cube ≥ 0.6.0） |
> | REST `POST /volumes` — 创建 Volume | ✅ 已支持 |
> | REST `POST /v2/volumes` — 携带可选 `private_data` 创建 | ✅ Cube ≥ 0.8.0；不回退 `/volumes` |
> | REST `GET /volumes/{volumeID}` — 查询 Volume + token | ✅ 已支持 |
> | REST `DELETE /volumes/{volumeID}` — 删除 Volume | ✅ 已支持（仍被挂载时返回 409） |
> | Python SDK `Volume.create` / `connect` / `list` / `get_info` / `destroy` | ✅ 已支持（SDK ≥ 0.6.0） |
> | Python SDK `Volume.create_with_private_data` | ✅ Cube + SDK ≥ 0.8.0；不回退 `/volumes` |
> | Python SDK `Sandbox.create(volume_mounts={path: volume})` | ✅ 已支持（e2b dict 映射） |
> | Go SDK `Client.CreateVolume / ListVolumes / GetVolume / DeleteVolume` | ✅ 已支持 |
> | Go SDK `Client.CreateVolumeWithPrivateData` / `CreateVolumeWithPrivateDataOptions` | ✅ Cube + SDK ≥ 0.8.0；不回退 `/volumes` |
> | Node SDK `Volume.createWithPrivateData` | ✅ Cube + SDK ≥ 0.8.0；不回退 `/volumes` |
> | Go SDK `CreateOptions.VolumeMounts`（含 `ReadOnly`） | ✅ 已支持 |
> | 同一 Volume 被多个沙箱同时挂载 | ✅ 已支持 |
> | 按沙箱只读挂载 | ✅ Cube SDK 扩展（Python `VolumeMount(..., read_only=True)`，Go `VolumeMount{ReadOnly: true}`）；官方 e2b SDK 自身没有只读挂载选项 |
> | 创建时省略 `driver`（e2b 默认行为） | ✅ 已支持 |

> **e2b API 与 SDK**
>
> CubeAPI 提供 **与 e2b 协议兼容** 的 `/volumes` REST 接口，可直接用 HTTP 客户端调用。
>
> **官方 e2b Python SDK 不能用于 CubeSandbox** — 其请求硬编码指向 e2b.cloud 后端。请使用 **`cubesandbox` Python SDK**（`Volume`、`Sandbox.create(volume_mounts={...})`），或对自有 CubeAPI 实例发起 REST 请求。

---

## 快速上手：用 `cubesandbox` 使用 Volume 插件

从插件到 SDK 跑通，只需四步。细节见下文各节链接。

### 按协议开发插件并部署到对应节点

按 [核心原理](#核心原理) 中的 [插件类型](#插件类型) 与各 Hook 小节实现 Create / Destroy（Controller）与 Attach / Detach（Node）。Controller 部署到 **CubeMaster** 节点，Node 部署到 **Cubelet** 节点（同一二进制/进程可同时承担两侧）。

参考实现：[COS 插件](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.zh.md)（one-click 会将 binary 插件放到 `CubeMaster/plugin/` 与 `Cubelet/plugin/`）。

> **第三方插件请安装到 cubetoolbox 目录之外**，否则升级Cube的时候会被重置，详见 [注册与配置](#注册与配置) 与 [插件开发要点](#插件开发要点)。

### 配置 CubeMaster / Cubelet 并重启

两侧 `volume_plugins` 使用相同的 `driver` 名，`binary_path` / `socket_path` 指向已部署的插件；修改后重启 CubeMaster 与 Cubelet 使配置生效。见 [注册与配置](#注册与配置)。

### 安装 cubesandbox SDK ≥ 0.6.0

```bash
pip install 'cubesandbox>=0.6.0'
```

Go 侧使用 `sdk/go` 模块：

```bash
go get github.com/tencentcloud/CubeSandbox/sdk/go
```

须使用 **`cubesandbox`**，不可用官方 e2b Python SDK。配置 `CUBE_API_URL`、`CUBE_TEMPLATE_ID`，远程读写时还需 `CUBE_PROXY_NODE_IP`。见 [环境准备](#环境准备)。

### 运行 demo

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-data")  # 省略 driver → 取 volume_plugins 第一项

with Sandbox.create(volume_mounts={"/workspace": vol}) as sb:
    sb.files.write("/workspace/hello.txt", "from volume")
    print(sb.files.read("/workspace/hello.txt"))

Volume.destroy(vol.volume_id)
```

完整生命周期与多沙箱共用见 [SDK 用法](#sdk-用法)。COS 端到端（依赖与凭证）见 [`examples/volume/cos/README.zh.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.zh.md)。

---

## 核心原理

> **图示约定**：蓝色填充 = Cube 平台；橙色填充 = Volume Plugin（开发者实现）。

### 要解决什么问题

沙箱需要**跨重启、跨实例**保留数据（模型权重、工作区文件等）。Cube 平台负责 API、编排与透传；**Volume Plugin** 负责把真实存储后端（对象存储、NFS…）挂载成宿主机上的 `hostPath`，再由 Cubelet 经 virtiofs 暴露给 microVM。

### 双角色模型

借鉴 Kubernetes CSI，Hook 分为**管理面**与**数据面**，由不同进程触发，职责解耦：

| 角色 | 运行侧 | Hook | 职责 |
|------|--------|------|------|
| **Controller** | CubeMaster 调用 | Create / Destroy | 在后端创建或删除 Volume 资源 |
| **Node** | Cubelet 调用 | Attach / Detach | 在宿主机挂载 / 卸载，产出 `hostPath` |

同一套 Hook 协议可用 **binary / rpc** 两种插件类型实现；四个 Hook 可在一个插件里实现，也可拆分。

#### 管理面：Create / Destroy

```mermaid
flowchart LR
    subgraph Cube["Cube 平台"]
        SDK1["SDK"]
        API1["CubeAPI"]
        CM["CubeMaster"]
        SDK1 --> API1 --> CM
    end

    subgraph PluginC["Volume Plugin · Controller"]
        CH1["Create"]
        CH2["Destroy"]
    end

    BE["存储后端"]

    CM -->|"POST /volumes"| CH1
    CM -->|"DELETE /volumes/:id"| CH2
    CH1 --> BE
    CH2 --> BE

    style PluginC fill:#fff3e0,stroke:#e65100,stroke-width:2px
    style Cube fill:#e3f2fd,stroke:#1565c0,stroke-width:1px
```

#### 数据面：Attach / Detach

```mermaid
flowchart LR
    subgraph Cube2["Cube 平台"]
        CM2["CubeMaster"]
        CL["Cubelet"]
        VF["virtiofs\nbind-mount"]
        SB["microVM"]
        CM2 -->|"sandbox create"| CL
        CL --> VF --> SB
    end

    subgraph PluginN["Volume Plugin · Node"]
        NH1["Attach"]
        NH2["Detach"]
    end

    HP["hostPath"]

    CL -->|"沙箱创建"| NH1
    CL -->|"沙箱销毁"| NH2
    NH1 --> HP
    NH2 --> HP
    HP --> VF

    style PluginN fill:#fff3e0,stroke:#e65100,stroke-width:2px
    style Cube2 fill:#e3f2fd,stroke:#1565c0,stroke-width:1px
```

无论 **binary** 还是 **rpc**，插件都必须实现**同一套 Hook 字段**。**binary** 映射为 CLI 参数 / stdout JSON（`snake_case`，如 `volume_id` → `--volume-id`、JSON 里 `host_path`）；**rpc** 使用 `volumeplugin.proto` 中的同名字段。CubeMaster / Cubelet 在调用 Hook **之前**按配置的 `driver` 选定插件——**`driver` 不是 Hook 入参**。

> **错误返回：** **binary** 用非零 exit 与/或 stdout JSON 中非空 `"error"`；**rpc** 用 gRPC error status（响应消息不含 `error` 字段）。

### 插件类型

| 类型 | Controller | Node | 说明 |
|------|------------|------|------|
| **binary** | ✅ | ✅ | 外部可执行文件；每次 Hook fork 子进程，CLI + stdout JSON |
| **rpc** | ✅ | ✅ | 长驻 gRPC 插件；经 `socket_path` 连接（Unix socket 或 TCP） |

配置项 `type` 选择插件类型；**`name`（driver）须在 CubeMaster / Cubelet 全链路一致**。rpc proto 定义见 [rpc 插件 pb 定义说明](#rpc-插件-pb-定义说明)。

### Hook 定义

| Hook | 侧 | 触发 |
|------|----|------|
| **Create** | Controller | `POST /volumes` |
| **Destroy** | Controller | `DELETE /volumes/:id` |
| **Attach** | Node | 沙箱创建（`volumeMounts`） |
| **Detach** | Node | 沙箱销毁或创建失败回滚 |

#### Create

| 方向 | 字段 | 类型 | 说明 |
|------|------|------|------|
| 输入 | `volumeID` | string | 稳定 ID（UUID 或与 `name` 相同） |
| 输入 | `name` | string | 展示名 |
| 输入 | `private_data` | string | 可选的不透明调用方输入，由 CubeMaster 原样转发（最长 **1024** 个 UTF-8 字节）。binary：`--private-data`，为空时不传 |
| 输出 | `token` | string | 可选鉴权令牌，返回给 SDK |
| 输出 | `private_data` | string | 插件私有状态（最长 **1024** 字节）。写入 `t_cube_volume`，转发给 **Attach** 与 **Destroy**。成功的 Volume API/SDK 响应不含该字段。可为空。 |
| 输出 | `error` | string | 成功为 `""`（仅 binary stdout JSON） |

**binary 示例**

输入（CLI）：

```bash
/path/to/my-plugin --op create --volume-id my-vol --name my-vol
# 调用方输入非空时，以一个参数传入原始值：
#   --private-data '{"bucket":"bucket-a","prefix":"workspaces/a/","cred_ref":"user-a"}'
```

输出（stdout JSON，exit 0）：

```json
{"token":"","private_data":"","error":""}
```

#### Destroy

| 方向 | 字段 | 类型 | 说明 |
|------|------|------|------|
| 输入 | `volumeID` | string | 要在后端删除的 Volume |
| 输入 | `private_data` | string | 从 `t_cube_volume` 读取的插件返回状态；创建失败回滚时使用 Create 返回的状态。binary：`--private-data`，为空时不传 |
| 输出 | `error` | string | 成功为 `""`（仅 binary stdout JSON） |

插件使用 `volumeID` 和返回的 `private_data` 定位后端资源（例如绑定的桶与前缀）。Destroy 只能删除属于该卷的资源，**不会**自动 Detach 运行中沙箱。

**binary 示例**

输入（CLI）：

```bash
/path/to/my-plugin --op destroy --volume-id my-vol
# Create 返回的状态非空时，附加：
#   --private-data '{"bucket":"bucket-a","prefix":"workspaces/a/","cred_ref":"user-a"}'
```

输出（stdout JSON，exit 0）：

```json
{"error":""}
```

#### Attach

| 方向 | 字段 | 类型 | 说明 |
|------|------|------|------|
| 输入 | `sandboxID` | string | 正在创建的沙箱 |
| 输入 | `namespace` | string | containerd namespace |
| 输入 | `volumeID` | string | 与 `volumeMounts[].name` 相同 |
| 输入 | `refCount` | int64 | **本 node 挂载前**沙箱数；`0` = 本 node 首次 |
| 输入 | `volumeBaseDir` | string | 父目录；`hostPath` **必须**在其内 |
| 输入 | `private_data` | string | Create 返回并落库的同一私有状态；可为空。binary：可选 `--private-data`（为空时不传） |
| 输出 | `hostPath` | string | Cubelet mntns 内路径，供 virtiofs bind |
| 输出 | `metadata` | map[string]string | opaque 状态；Detach 原样回传 |
| 输出 | `error` | string | 成功为 `""`（仅 binary stdout JSON） |

- `refCount == 0`：**本 node 首次挂载** — 执行后端挂载。
- `refCount > 0`：**本 node 已有沙箱引用该 Volume** — 返回已有 `hostPath`（及 `metadata`），不要再次挂载。
- **`hostPath`：** 位于 `volumeBaseDir` 下的绝对路径（推荐 `<volumeBaseDir>/<插件名>-<volumeID>`）。否则 Cubelet 拒绝 attach、回滚并导致沙箱创建失败。默认 `volumeBaseDir`：`/data/cube-shared/volume`。

**binary 示例**

输入（CLI）：

```bash
/path/to/my-plugin --op attach \
  --sandbox-id sb-001 --namespace default \
  --volume-id my-vol --ref-count 0 \
  --volume-base-dir /data/cube-shared/volume
# Create 返回非空 private_data 时可选附加：
#   --private-data 'volumes/my-vol/'
```

输出（stdout JSON，exit 0）：

```json
{"host_path":"/data/cube-shared/volume/my-storage-my-vol","metadata":{"mount_dir":"/data/cube-shared/volume/my-storage-my-vol"},"error":""}
```

#### Detach

| 方向 | 字段 | 类型 | 说明 |
|------|------|------|------|
| 输入 | `sandboxID` | string | 同 Attach |
| 输入 | `namespace` | string | 同 Attach |
| 输入 | `volumeID` | string | 同 Attach |
| 输入 | `refCount` | int64 | **本 node 卸载后**沙箱数；`0` = 本 node 最后一个 |
| 输入 | `metadata` | map[string]string | Attach 返回的 map，原样回传 |
| 输出 | `error` | string | 成功为 `""`（仅 binary stdout JSON） |

- `refCount == 0`：**本 node 最后一个沙箱** — 拆除共享后端挂载（保留持久数据）。
- `refCount > 0`：本 node 仍有其他沙箱挂载 — no-op。

**binary 示例**

输入（CLI）：

```bash
/path/to/my-plugin --op detach \
  --sandbox-id sb-001 --namespace default \
  --volume-id my-vol --ref-count 0 \
  --metadata '{"mount_dir":"/data/cube-shared/volume/my-storage-my-vol"}'
```

输出（stdout JSON，exit 0）：

```json
{"error":""}
```

### `private_data` 契约

旧 CubeAPI `POST /volumes` 保持既有的 `name`/`driver` 请求定义。独立的 `POST /v2/volumes` 接受可选 `private_data`，并将其原样转发到 CubeMaster 内部 `POST /cube/volume`。超过 1024 个 UTF-8 字节、包含 NUL 或含有 BMP 以外字符时，新入口在调用 Master 前返回 HTTP 400。省略或 null 输入时，发往 Master 的请求省略该字段；显式空字符串按空值转发。两者返回相同的卷响应，共用既有查询、删除和沙箱挂载 API。

SDK 通过显式新方法访问新创建入口：Go `Client.CreateVolumeWithPrivateData`、Python `Volume.create_with_private_data`、Node `Volume.createWithPrivateData`。原创建方法继续调用 `/volumes`。新方法要求 CubeAPI、所有处理卷操作的 CubeMaster 和所选插件均支持该契约，不会自动回退旧入口。使用前须遵循下文的升级顺序：即使新 API 返回 201，旧 Master 仍可能静默忽略输入。

CubeMaster 将其视为不透明字符串，不解析 JSON、不 trim 空白、不规范化存储路径。调用方输入 A 与 Create 返回状态 B 必须是有效 UTF-8，最长 **1024 字节**（Go `len`，不是字符数），不得含 NUL，且字符范围不得超出 U+FFFF。该规则适用于所有 Controller 类型，因为同一状态后续可能交给 binary Node 插件。校验在外层 JSON 解码后进行；内容中由 ASCII 字符组成的字面量 `\uD83D\uDE00` 会原样透传，不会再解析一遍。

上述内容校验失败时，CubeMaster 返回 **HTTP 400**，响应体保留 `ret.ret_code = 130400`。非法 A 在预留数据库行和调用插件前被拒绝；非法 B 先走既有的 best-effort 回滚及预留行清理，再返回该响应。CubeAPI 将此错误映射为 HTTP 400，也兼容较早 Master 的 HTTP 200 + `130400` 响应。请求解码失败和其他内部错误分支保持原有响应约定。

- Create 接收输入 A，可以返回不同的状态 B。只持久化 B，并将 B 转发给 Attach 和正常 Destroy。
- 返回空值就保存空值，不回退到 A。
- Create 成功后，若 B 的长度校验或数据库更新失败，回滚 Destroy 直接接收 B；B 超过持久化上限时也传完整值。清理属于尽力尝试，binary 参数限制或插件错误可能导致清理失败。
- 回滚 Destroy 失败时，CubeMaster 记录卷 ID、driver、失败阶段和固定错误类别，不记录状态或插件原始错误；仍会尝试删除 DB 占位行，不提供自动清理恢复机制。数据库更新返回错误不能证明状态一定未提交。
- Binary Create/Destroy 与 Node Attach 在启动进程前拒绝 NUL，Node 校验也覆盖非法的存量状态。RPC 回滚仍可携带非法返回状态，以便插件尽力清理。

只放桶、前缀、endpoint/region 和 `cred_ref` 等凭据引用，**绝不能放明文访问密钥或其他秘密**。插件在沙箱外用引用换取凭据。该字段在数据库中明文存储，并经注解转发；binary 参数在宿主机上可见，且不能包含 NUL 字节。

插件不得记录该字段或把它写入错误信息。现有错误路径可能将 binary stderr/stdout、结构化插件错误或 gRPC status message 回显到 API 错误；启用 HTTP debug body dump 时也可能记录请求体。本次透传改动不提供完整的错误及日志脱敏。

### RefCount

同一 Volume 可被多个沙箱共享。Cubelet 维护 **node 本地**引用计数，在调用 Node Hook 时传入 `refCount`：

| 时机 | `refCount` | 插件行为 |
|------|------------|----------|
| Attach 前 | `0` | **本 node**首个沙箱；建立后端挂载 |
| Attach 前 | `> 0` | **本 node**已有其他沙箱挂载；返回已有 `hostPath` |
| Detach 后 | `0` | **本 node**最后一个沙箱；拆除共享后端挂载 |
| Detach 后 | `> 0` | 本 node 仍有其他沙箱挂载；no-op |

**当某 node 本地计数 0→1 或 1→0 时，Cubelet 通知 CubeMaster 更新 `t_cube_volume.refcount`；该计数非零时，管理面拒绝 `DELETE /volumes`。**

```mermaid
flowchart TD
    A["Attach(refCount)"] --> B{"refCount == 0 ?"}
    B -->|是| C["本 node 首次挂载"]
    B -->|否| D["复用本 node 已有 hostPath"]

    E["Detach(refCount)"] --> F{"refCount == 0 ?"}
    F -->|是| G["本 node 最后一次卸载"]
    F -->|否| H["no-op"]
```

### 端到端生命周期

```mermaid
sequenceDiagram
    box rgba(227,242,253,1) Cube 平台
        participant U as 用户 / SDK
        participant API as CubeAPI
        participant M as CubeMaster
        participant L as Cubelet
        participant VM as microVM
    end
    box rgba(255,243,224,1) Volume Plugin
        participant P as Controller Plugin
        participant N as Node Plugin
    end

    Note over U,VM: ① 创建 Volume（管理面）
    U->>API: POST /volumes {name, driver}
    API->>M: POST /cube/volume
    M->>P: Create(volumeID, name, private_data)
    P-->>M: stdout JSON {"token":"...","private_data":"...","error":""}
    M->>M: 写入 t_cube_volume
    M-->>U: {volumeID, name, token}

    Note over U,VM: ② 创建沙箱并挂载（数据面）
    U->>API: POST /sandboxes {volumeMounts: [{name, path}]}
    API->>M: sandbox create + annotations
    M->>L: plugin-volume-sources annotation
    L->>N: Attach(refCount=0)
    N-->>L: AttachResult {hostPath, metadata, ...}
    L->>L: bind-mount → virtiofs
    L-->>M: 响应 ext_info（本 node refcount 0→1）
    M->>M: t_cube_volume.refcount += 1
    L->>VM: /workspace 可见

    Note over U,VM: ③ 销毁沙箱（数据面）
    U->>API: DELETE /sandboxes/:id
    L->>N: Detach(refCount=0)
    N-->>L: ok
    L->>L: unmount bind
    L-->>M: 响应 ext_info（本 node refcount 1→0）
    M->>M: t_cube_volume.refcount -= 1

    Note over U,VM: ④ 删除 Volume（管理面）
    U->>API: DELETE /volumes/:id
    API->>M: DELETE /cube/volume/:id
    M->>M: refcount ≠ 0 → 拒绝删除
    M->>P: Destroy(volumeID, stored private_data)（refcount == 0 时）
    P-->>M: ok
    M->>M: 硬删除 DB 记录
```

---

## SDK 用法

以下示例基于 **Python SDK `cubesandbox` ≥ 0.6.0**，Go 示例见 [Go SDK 用法](#go-sdk-用法)。CubeAPI 提供 e2b 兼容的 `/volumes` REST 接口；应用侧推荐优先使用 SDK。

### e2b 兼容性说明

| 层级 | 是否 e2b 兼容 | 说明 |
|------|---------------|------|
| CubeAPI `/volumes` REST | ✅ 是 | `POST/GET/DELETE /volumes`、`GET /volumes/{volumeID}` |
| 官方 e2b Python SDK | ❌ 否 | 硬编码 e2b.cloud 后端；**勿**用于 CubeSandbox |
| `cubesandbox` Python SDK | ✅ 是 | `Volume`、`Sandbox.create(volume_mounts={path: volume})`（e2b dict） |
| `cubesandbox` Go SDK（`sdk/go`） | ✅ 是 | `Client.CreateVolume / ListVolumes / GetVolume / DeleteVolume`；挂载用显式结构体 `CreateOptions.VolumeMounts`（非 e2b dict） |
| 创建时省略 `driver` | ✅ 是 | CubeMaster 取 `volume_plugins` **列表第一项** |
| 按沙箱只读挂载 | ❌ 否 | 官方 e2b SDK 自身没有只读 Volume 挂载选项；Cube SDK 增加 `VolumeMount(volume, read_only=True)` 扩展 |
| 从快照创建时重新传入 `volumeMounts` | ✅ 是 | 与快照挂载一致时接受，省略也可以；挂载有变化时拒绝（见下文「快照、回滚、克隆与跨机恢复」） |

完整 COS 插件体验见 [`examples/volume/cos/README.zh.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.zh.md)。

### 环境准备

```bash
pip install 'cubesandbox>=0.6.0'

export CUBE_API_URL=http://<cubeapi-host>:3000
export CUBE_TEMPLATE_ID=<your-template-id>

# 远程访问时必填：数据面直连 CubeProxy，绕过 *.cube.app DNS
export CUBE_PROXY_NODE_IP=<cubeproxy-node-ip>

# 后端开启鉴权时可选
# export CUBE_API_KEY=<key>
```

| 环境变量 | 说明 |
|----------|------|
| `CUBE_API_URL` | CubeAPI 管控面地址 |
| `CUBE_TEMPLATE_ID` | 创建沙箱用的模板 ID |
| `CUBE_PROXY_NODE_IP` | CubeProxy 节点 IP；挂载后沙箱内读写走数据面 |
| `CUBE_API_KEY` | 可选；鉴权开启时对应 `X-API-Key` |

### 完整生命周期（创建 → 挂载 → 解绑 → 删除）

```python
from cubesandbox import Sandbox, Volume

# ① 创建 Volume（管理面）→ 返回 live Volume 实例（e2b 兼容）
# 省略 driver 与 Volume.create("my-data") 行为一致
vol = Volume.create("my-data")
# 或显式指定插件：
# vol = Volume.create("my-data", driver="my-storage")
print(vol.volume_id, vol.name, vol.token)  # token 由插件返回，可能为空

# 列出 / get_info 单查 → 返回 VolumeInfo（不含 live handle）
for v in Volume.list():
    print(v.volume_id, v.name)              # list 不含 token
info = Volume.get_info(vol.volume_id)       # get_info 含 token

# 重连已有卷（e2b Volume.connect）
# vol = Volume.connect(vol.volume_id)

# ② 创建沙箱并挂载 Volume（数据面：Attach）
with Sandbox.create(
    volume_mounts={"/workspace": vol},
) as sb:
    sb.files.write("/workspace/note.txt", "persisted!")
    print(sb.files.read("/workspace/note.txt"))

# ③ 退出 with / 调用 sb.kill() 销毁沙箱（数据面：Detach，解绑）

# ④ 删除 Volume（管理面：Destroy）
Volume.destroy(vol.volume_id)  # 返回 True；卷不存在时返回 False（幂等）
```

| SDK 参数 | 说明 |
|----------|------|
| `Volume.create(name, driver=...)` | 返回 **Volume 实例**；`name` 可选；省略则服务端生成 UUID 作为 `volume_id`；须匹配 `^[a-zA-Z0-9_-]+$`，最长 128 字符 |
| `Volume.connect(volume_id)` | 返回 **Volume 实例**（e2b 兼容；内部调用 `get_info`） |
| `Volume.list()` | 返回 `list[VolumeInfo]`（无 token） |
| `Volume.get_info(volume_id)` | 返回 **VolumeInfo**；含 `token`（插件未返回时为 `""`） |
| `Volume.destroy(volume_id)` | e2b 兼容删除；成功返回 `True`，404 返回 `False`（幂等） |
| `Volume.delete(...)` | `destroy` 的兼容别名（旧代码可用，推荐 `destroy`） |
| `driver` | 可选；插件名；**e2b 兼容用法下省略**——SDK 不发送该字段，CubeMaster 取 `volume_plugins` **列表第一项** |
| `volume_mounts` | e2b dict `{挂载路径: Volume \| volume_id \| name}` — key 为沙箱内路径，value 为 `Volume` 实例或 volume ID 字符串；Cube SDK 还可将 value 包装为 `VolumeMount(..., read_only=True)` 以启用只读挂载 |

`driver` 写入 `t_cube_volume`，沙箱创建时经 annotation 传给 Cubelet——CubeMaster 与 Cubelet 两侧 `volume_plugins[].name` 须与之一致。

### Go SDK 用法

Go SDK（`sdk/go`）提供与上文等价的完整生命周期，使用与 Python 相同的环境变量：

```go
import (
	"context"
	"errors"

	cubesandbox "github.com/tencentcloud/CubeSandbox/sdk/go"
)

client := cubesandbox.NewClient(cubesandbox.NewConfigFromEnv())
ctx := context.Background()

// ① 创建 Volume（管理面）——Driver 省略时取 volume_plugins 第一项
volume, err := client.CreateVolume(ctx, cubesandbox.CreateVolumeOptions{Name: "my-data"})

// 列出 / 单查
volumes, err := client.ListVolumes(ctx)              // 不含 token
volume, err = client.GetVolume(ctx, volume.VolumeID) // 含 token

// ② 创建沙箱并挂载（数据面：Attach）；ReadOnly 为 Cube 只读扩展
sb, err := client.Create(ctx, cubesandbox.CreateOptions{
	TemplateID: "base",
	VolumeMounts: []cubesandbox.VolumeMount{
		{Name: volume.VolumeID, Path: "/workspace"},
		// {Name: volume.VolumeID, Path: "/dataset", ReadOnly: true}
	},
})

// ③ 销毁沙箱（数据面：Detach，解绑）
err = sb.Kill(ctx)

// ④ 删除 Volume（管理面：Destroy）
if err := client.DeleteVolume(ctx, volume.VolumeID); err != nil {
	switch {
	case errors.Is(err, cubesandbox.ErrVolumeInUse):
		// 仍被挂载（HTTP 409）——先销毁使用该卷的沙箱
	case errors.Is(err, cubesandbox.ErrVolumeNotFound):
		// 已不存在——幂等清理可忽略
	}
}
```

与 Python SDK 的差异：挂载参数是显式的 `[]VolumeMount{Name, Path, ReadOnly}` 结构体（非 e2b dict 映射）；删除结果通过 `ErrVolumeInUse` / `ErrVolumeNotFound` 哨兵错误用 `errors.Is` 区分。name 规则、driver 省略行为、list 不返回 token 等语义与 Python 一致。更多示例见 [`sdk/go/README.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/sdk/go/README.md)。

### 按沙箱选择访问模式

e2b 兼容的 mapping 仍是默认写法，并创建读写挂载。CubeSandbox 在 mapping 的 value 上增加了 `VolumeMount`，因此同一个持久 Volume 可以由不同沙箱分别选择读写或只读：

```python
from cubesandbox import Sandbox, Volume, VolumeMount

dataset = Volume.create("shared-dataset")

# 该沙箱可以更新 Volume。
writer = Sandbox.create(
    volume_mounts={"/dataset": dataset},
)

# 同一个 Volume 在该沙箱中禁止修改。
reader = Sandbox.create(
    volume_mounts={"/dataset": VolumeMount(dataset, read_only=True)},
)
```

官方 e2b SDK 的 Volume 挂载接口自身没有只读选项，这不是 CubeSandbox 的兼容性限制。Cube SDK 和 REST 客户端可以显式使用 Cube 扩展 `volumeMounts[].readOnly: true`，原有 e2b 形式的请求不发送 `readOnly`，继续保持读写。

只读是**单次沙箱挂载属性**，不是 Volume 自身的全局属性。reader 不能通过该挂载创建、修改、重命名或删除文件，但仍可能看到其他读写挂载产生的变化，因此它不是不可变快照。当前一个沙箱内仍只能挂载同一个 Volume 一次；同一沙箱重复挂载同一个 Volume 会继续被拒绝。

### 多沙箱共用同一 Volume

```python
# e2b 兼容：省略 driver（默认走 CubeMaster 配置的第一项插件）
vol = Volume.create("shared-data")
# vol = Volume.create("shared-data", driver="my-storage")

sb_a = Sandbox.create(volume_mounts={"/workspace": vol})
sb_a.files.write("/workspace/shared.txt", "from A")

sb_b = Sandbox.create(volume_mounts={"/workspace": vol})
print(sb_b.files.read("/workspace/shared.txt"))  # from A

sb_a.kill()
sb_b.kill()
Volume.destroy(vol.volume_id)
```

同一 Volume 可被多个沙箱同时挂载；一个沙箱写入的数据对其他沙箱可见。调用 `Volume.destroy()` 前须销毁**所有**挂载该 Volume 的沙箱（平台如何跟踪共享引用见 [RefCount](#refcount)）。

### 快照、回滚、克隆与跨机恢复

Snapshot 会保存稳定的 Volume ID、容器挂载路径和只读属性，但不会复制 Volume 数据，也不会持久化运行时 `private_data`。FromSnap 由 Master 查询当前 Volume 记录，并把 driver 元数据发送给目标 Cubelet 执行 `Attach`；Pause/Resume 会校验已记录的 Volume ID，并根据 pause package 重新 Attach；原地 Rollback 则保留沙箱现有的外部挂载。

从快照创建沙箱时可以省略 `volumeMounts`，快照记录的挂载会自动重新挂载。官方 e2b 客户端会在每次创建时重新传入 `volumeMounts`，这同样可以接受，但必须与快照完全一致（相同的卷、路径和只读属性）。只要有任何不同，请求就会返回 HTTP 400（`volumeMounts must match the snapshot's volume mounts`），因为快照的 VM 内存中挂载已经固定，无法改挂到其他位置。

**fs-only 快照不记录挂载，创建时必须重新声明 `volumeMounts`（含容器路径）**。没声明的卷不会挂载，也不会要求与快照一致。Attach 或挂载失败（卷不存在、driver 报错）会让创建直接失败，不会留下空目录。

因此它采用 **external-reference（外部引用）**语义：

- FromSnap 和回滚会恢复 VM/rootfs 状态，但挂载后的 Volume 展示当前数据。
- 克隆继续共享同一个 Volume；读写挂载中的写入对源沙箱和其他克隆可见。
- Plugin Volume 不会把原本支持跨机的 VM Snapshot 固定到源节点。对于 `remote_status=ready` 的 S3 VM Snapshot，目标 Cubelet 会在启动 VM 前尝试 Attach Volume。
- 调度器当前只检查 VM 兼容性，不检查 Volume portability、topology、multi-attach 能力或目标节点 driver。Volume 不存在、目标节点未注册 driver 或 `Attach` 返回错误时，沙箱创建失败。运维方需要在每个候选节点配置相同 driver，并确保它们能够访问目标后端。

Volume backend 与 VM Snapshot backend 相互独立。VM Snapshot 包必须使用 `jfs` 或 `s3` backend 才能跨机；Plugin Volume 可以使用目标节点 driver 能够 Attach 的任意后端。raw host mount 始终固定在源节点。

### 常见异常（SDK）

| 场景 | SDK 异常 | 典型原因 |
|------|----------|----------|
| 卷不存在 | `VolumeNotFoundError`（404） | `Volume.get_info` / `Volume.connect` 的 ID 无效 |
| 未知 driver | `ApiError`（400，CubeMaster 130400） | 未配置对应 `volume_plugins` |
| 卷仍被引用 | `ApiError`（409，CubeMaster 130409） | Volume 仍被沙箱挂载时尝试删除 |
| 非法卷名 | `ValueError` | 客户端校验；名称不符合 `^[a-zA-Z0-9_-]+$` |
| 挂载不存在的卷 | `ApiError` | 沙箱 `volumeMounts[].name` 未创建 |
| 快照恢复时挂载有变化 | `ApiError`（400，CubeMaster 130400） | 从快照创建时传入的 `volumeMounts` 与快照记录的挂载不一致 |

> **注意**：Volume 仍被任意沙箱挂载时，`Volume.destroy()` 返回 **409**。须先销毁所有使用该 Volume 的沙箱，再删除。删除**不会**自动卸载正在使用的沙箱。

---

## 注册与配置

### CubeMaster（conf.yaml）

```yaml
volume_plugins:
  - name: <driver>
    type: binary
    binary_path: <binary_path>

  - name: <driver>
    type: rpc
    socket_path: /run/<driver>.sock   # Unix 裸路径；插件 SOCKET 须一致
```

### Cubelet（config.toml）

```toml
[plugins."io.cubelet.internal.v1.storage"]
  volume_plugin_base_dir = "<volume_plugin_base_dir>"

[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
  name        = "<driver>"
  type        = "binary"       # binary | rpc
  binary_path = "<binary_path>"

[[plugins."io.cubelet.internal.v1.storage".volume_plugins]]
  name        = "<driver>"
  type        = "rpc"
  socket_path = "/run/<driver>.sock"   # 与插件 SOCKET 相同
```

**`driver` 名须全链路一致：** `Volume.create(..., driver=...)`（省略则取列表第一项）→ 写入 DB → annotation → Cubelet 按同名路由。CubeMaster 与 Cubelet 的 **`volume_plugins[].name` 必须一致**。

**`volume_plugin_base_dir`：** 所有插件返回的 `host_path` **必须**落在该目录内（未配置时默认 `/data/cube-shared/volume`）。Cubelet 经 `volumeBaseDir`（rpc）/ `--volume-base-dir`（binary）传给插件；`host_path` 越界则 attach 被拒绝并回滚。

> **`binary_path` / `socket_path` 位置：** 第三方插件请安装到 cubetoolbox 目录之外，否则升级 Cube 时会被重置。

**`name` 必须唯一**：同一进程内不能有两条相同 `name` 的 `volume_plugins`。列表顺序决定省略 `driver` 时的默认插件。

---

## rpc 插件 pb 定义说明

rpc 插件实现 [`volumeplugin.proto`](https://github.com/TencentCloud/CubeSandbox/blob/master/pkgs/proto/services/volumeplugin/v1/volumeplugin.proto) 中的 gRPC 服务。消息字段与上文 [Hook 定义](#hook-定义) 一致（proto 使用 `snake_case`）。

| 文件 | 说明 |
|------|------|
| [`volumeplugin.proto`](https://github.com/TencentCloud/CubeSandbox/blob/master/pkgs/proto/services/volumeplugin/v1/volumeplugin.proto) | 协议源文件 |
| [`volumeplugin.pb.go`](https://github.com/TencentCloud/CubeSandbox/blob/master/pkgs/proto/services/volumeplugin/v1/volumeplugin.pb.go) | 已提交的 Go message |
| [`volumeplugin_grpc.pb.go`](https://github.com/TencentCloud/CubeSandbox/blob/master/pkgs/proto/services/volumeplugin/v1/volumeplugin_grpc.pb.go) | 已提交的 gRPC stub |

| Service | 调用方 | RPC |
|---------|--------|-----|
| `VolumeControllerService` | CubeMaster | `Create`、`Destroy` |
| `VolumePluginService` | Cubelet | `Attach`、`Detach` |

修改 proto 后重新生成：`cd pkgs/proto && make proto`。参考实现：[`examples/volume/cos/rpc/README.zh.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/rpc/README.zh.md)。

---

## 插件开发要点

实现自定义 Volume 插件时须遵守以下平台规则：

| # | 要点 | 说明 |
|---|------|------|
| 1 | mntns | Node Hook 须在 Cubelet mntns 内挂载；binary 由 Cubelet fork 自动继承 |
| 2 | 幂等 Attach | `refCount > 0` 表示本 node 已有沙箱挂载 — 直接返回已有 `hostPath` |
| 3 | hostPath | **必须**落在 `volumeBaseDir` 内（推荐 `<volumeBaseDir>/<driver>-<volumeID>`），框架再 bind 到 virtiofs |
| 4 | Detach 范围 | Node Detach 只拆除宿主机侧挂载（如 FUSE unmount）；不删除后端持久数据 |
| 5 | 凭证 | 访问密钥、桶名、地域等由**插件自行管理**（配置文件、环境变量等）；框架不规定布局 |
| 6 | CubeMaster / Cubelet 一致 | 两侧 `volume_plugins` 须注册**相同的 `driver` 名**；同一 Volume 的管理面 Hook（Create/Destroy）与数据面 Hook（Attach/Detach）须指向**同一套插件** |
| 7 | 升级安全路径 | 第三方插件请安装到 cubetoolbox 目录之外，否则升级 Cube 时会被重置 |

---

## 参考实现

仓库提供 **腾讯云 COS** 参考插件（binary Shell + rpc Go），含完整体验指南与依赖安装：

| 文档 | 内容 |
|------|------|
| [`examples/volume/cos/README.zh.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.zh.md) | COS 完整体验（依赖、配置、SDK 验证） |
| [`examples/volume/cos/binary/README.zh.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/binary/README.zh.md) | binary 插件脚本细节 |
| [`examples/volume/cos/rpc/README.zh.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/rpc/README.zh.md) | rpc 插件构建与部署 |
| [`examples/volume/cos/verify_volume.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/verify_volume.py) | Python SDK 验证脚本 |
| [`examples/volume/s3/README.zh.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/s3/README.zh.md) | 通用 S3 兼容后端完整体验（内置 S3 客户端的 Go 二进制 + s3fs；AWS S3、腾讯云 COS、MinIO、R2；支持 `arm64`） |
| [`examples/volume/juicefs/README.zh.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/juicefs/README.zh.md) | JuiceFS 完整体验（对象存储之上的 POSIX 文件系统；一个文件系统，每个卷 `--subdir`；多个沙箱可共享一个卷并立即看到彼此的写入） |

各后端专属的 Hook 行为、对象布局、实现取舍与排障说明均在上述 example 文档中，本文不再重复。

---

## 调试与排障

### Mount Namespace

Cubelet 通过 `unshare(CLONE_NEWNS)` 运行在**独立 mount namespace** 中。这是实现细节，不影响 Hook 协议本身，但开发 / 排障时需注意：

- Node Hook 产出的 `hostPath`（FUSE、bind 等）必须落在 **Cubelet mntns** 内；宿主机根 mntns 的 `/proc/mounts` 通常看不到这些挂载。
- **binary** 插件由 Cubelet fork，自动继承 mntns，一般无需 `nsenter`。
- 若在宿主机上手动 `mount` 做调试，沙箱内往往**看不到**——应通过 Cubelet 触发 Attach，或进入 Cubelet mntns 验证。

查看 Cubelet mntns 内的挂载：

```bash
CPID=$(pgrep -f "cubelet --config" | head -1)
nsenter -t "$CPID" -m -- mount | grep -E 'volume|fuse'
```

### 手动测试插件（binary 类型）

将 `/path/to/my-plugin` 与 `my-storage` 替换为你的插件二进制与配置的 `driver` 名：

```bash
# 模拟 Controller create
/path/to/my-plugin \
  --op create --volume-id test-vol --name test-vol

# 模拟 Node attach（第一次挂载）；host_path 必须落在 --volume-base-dir 内
/path/to/my-plugin \
  --op attach --sandbox-id sb-001 --namespace default \
  --volume-id test-vol --ref-count 0 \
  --volume-base-dir /data/cube-shared/volume
```

COS 插件的手动测试命令见 [`examples/volume/cos/README.zh.md`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/volume/cos/README.zh.md)。

### 常见问题

| 现象 | 可能原因 | 排查 |
|------|----------|------|
| `no plugin registered for driver` | Cubelet 未配置对应 `volume_plugins` | 检查 `config.toml`，重启 Cubelet |
| `unknown driver` | CubeMaster 未配置或 driver 名不一致 | 对比两侧配置的 `name` 字段 |
| `volume not found` | 沙箱 `volumeMounts[].name` 与已有 `volume_id` 不匹配 | 使用 `Volume.create` 返回的 `volume_id`，或在 `volume_mounts` 中直接传 `Volume` 实例 |
| FUSE 挂载成功但沙箱内看不到 | 挂载在宿主机 mntns 而非 Cubelet mntns | 确保插件由 Cubelet fork，不要手动在宿主机 mount |
| attach 成功但 detach 泄漏 | `ref_count > 0` 时误卸载了共享 FUSE | 检查 detach 逻辑是否遵守 RefCount 语义 |

## 兼容性说明

Volume 依赖 CubeMaster 与 Cubelet **双侧**均升级到支持 Volume 插件的版本。滚动升级期间：

| CubeMaster | Cubelet | Volume 创建 / 删除 | 沙箱 `volumeMounts` |
|---|---|---|---|
| 新 | 新 | 可用 | 可用 |
| 新 | 旧（如 v0.5.x） | 可用 | 不生效（创建不因此失败，但不会挂载） |
| 旧（如 v0.5.x） | 新 / 旧 | 不可用 | 不可用；勿传 `volumeMounts`，普通创建不受影响 |

混合版本时勿假设旧节点已挂载成功；需要挂载效果时，请确认调度到的 Cubelet 已升级。

### `private_data` 升级顺序

通过 `/v2/volumes` 或新 SDK 方法创建卷前：

1. 检查或升级 Controller 和 Node 插件，确保 Create 接收调用方输入 A，Attach 与 Destroy 使用返回状态 B，且过渡期间既有卷仍可使用。新旧 Master 并存时，须检查插件对两种调用方式的兼容性。
2. 升级**所有处理卷操作的 CubeMaster**，包括创建、删除和沙箱挂载元数据解析。在依赖每卷 A/B 之前完成 Master 滚动升级；需要新契约的卷不能再由旧 Master 处理。
3. 部署支持 `/v2/volumes` 的 CubeAPI，再开始调用新 API 或 SDK 方法。仅部署新 API 不能确认其下游 Master 已支持该能力。

新 CubeAPI 连接旧 Master 时，可能返回 **201**，但 A 已被旧 Master 忽略；旧 Master 调用 Destroy 时也不传 B，可能失败或按插件默认配置操作。目前没有自动探测下游能力的机制。回退版本时，须停止新的版本化创建请求，并让兼容的 Master 继续处理依赖 A/B 的卷，直到这些卷被删除或兼容迁移得到验证。

Controller 插件兼容性还需检查：

- Create 输入为空、Destroy 状态为空时，省略 `--private-data`，保留原有 binary 命令形式。
- 旧 binary 插件可能拒绝新 flag。即使调用方没有传该字段，只要旧插件 Create 返回非空状态，Destroy 仍会接收该状态，既有卷和创建回滚也如此。须检查两个操作；不能仅凭输入为空就保证整个生命周期兼容。
- 旧 RPC 服务可以忽略新增请求字段，但这不代表其使用了传入配置，也不代表返回空状态；仍以既有 Create 响应为准。
- CubeMaster 不会在 Destroy 失败后去掉 `private_data` 重试：第一次调用可能已有副作用，去掉状态也可能指向其他资源。

---

## 已知限制与路线图

以下为 **Cube 平台当前行为**（与具体插件实现无关）：

| 项目 | 说明 |
|------|------|
| 删除 Volume 检查引用计数 | `t_cube_volume.refcount ≠ 0`（仍被 node 挂载）时拒绝删除；不会自动 detach 运行中沙箱 |
| refcount 依赖响应可达 | 引用计数在沙箱创建/销毁**响应**中回传；若该响应丢失（网络异常等），计数可能与实际短暂不一致 |
| FUSE POSIX 语义 | 取决于后端与挂载实现（硬链接、原子 rename 等） |

---

## 源码索引

| 模块 | 路径 | 说明 |
|------|------|------|
| Node 接口 | `Cubelet/plugins/volume/interface.go` | `VolumePlugin` 抽象接口 |
| Node 请求类型 | `Cubelet/plugins/volume/context.go` | `AttachRequest` / `DetachRequest` |
| Controller 接口 | `CubeMaster/pkg/volume/plugin/plugin.go` | `ControllerPlugin` 抽象接口 |
| Node binary 类型 | `Cubelet/plugins/volume/binary/driver.go` | Hook → CLI 映射 |
| Node rpc 类型 | `Cubelet/plugins/volume/rpc/driver.go` | Hook → rpc 客户端 |
| Controller binary | `CubeMaster/pkg/volume/plugin/binary/driver.go` | Hook → CLI 映射 |
| Controller rpc | `CubeMaster/pkg/volume/plugin/rpc/driver.go` | Hook → rpc 映射 |
| 跨节点 refcount | `CubeMaster/pkg/volume/refcount/refcount.go` | 解析 ext_info 事件并累加 `t_cube_volume.refcount` |
| Volume DB 模型 | `CubeMaster/pkg/base/db/models/volume.go` | `VolumeRecord`（含 `refcount` 字段） |
| Node 挂载逻辑 | `Cubelet/storage/pluginvolume.go` | bind-mount + virtiofs；上报 node 级 refcount 转变 |
| 协议定义 | `pkgs/proto/services/volumeplugin/v1/volumeplugin.proto` | rpc 类型 proto |
| Go 生成代码 | `pkgs/proto/services/volumeplugin/v1/volumeplugin*.pb.go` | 已提交的 message / rpc stub |
| COS 参考（binary） | `examples/volume/cos/binary/cube-volume-cos.sh` | binary 类型示例 |
| COS 参考（rpc） | `examples/volume/cos/rpc/cmd/cube-volume-cos-rpc` | rpc 类型示例 |
