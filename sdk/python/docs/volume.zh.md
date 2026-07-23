# 持久化卷（Persistent Volumes）

> 英文版见 [`volume.md`](./volume.md)。本文为中文版，内容对齐英文版。

`Volume` 是管理 CubeSandbox **持久化卷**的类级别工具类——卷是与 e2b 兼容的存储，
底层由卷插件（COS、NFS…）支撑。创建卷后通过
`Sandbox.create(volume_mounts={...})`（e2b 兼容的 mapping 形式）挂载进沙箱，其数据可跨沙箱重启保留，也可在多个
沙箱之间共享。

```python
from cubesandbox import Sandbox, Volume
```

`Volume` 的方法均为**类方法**——无需手动实例化即可调用。对齐 e2b：`create` 与
`connect` 返回一个 **`Volume` 实例**（携带 `volume_id` / `name` / `token`），而
`list` / `get_info` 返回纯数据 `VolumeInfo`，`destroy` 返回 `bool`。

---

## 配置

卷管理类调用只走管控面（`CUBE_API_URL`）。而对已挂载卷的**文件读写**走数据面，
因此在 CubeProxy 节点之外运行时还需要配置 `CUBE_PROXY_NODE_IP`。

| 环境变量 | 是否必填 | 默认值 | 使用方 |
|---|:---:|---|---|
| `CUBE_API_URL` | ✅ | `http://127.0.0.1:3000` | 所有 `Volume.*` 调用 |
| `CUBE_PROXY_NODE_IP` | 远程时必填 | — | 对已挂载卷的 `sb.files.*` 读写 |
| `CUBE_TEMPLATE_ID` | 挂载时必填 | — | `Sandbox.create(...)` |

也可以给每个方法显式传入 `config=` 参数指定 `Config`。

---

## API 参考

| 方法 | HTTP | 入参 | 返回 |
|---|---|---|---|
| `Volume.create(name=None, *, driver=None, config=None)` | `POST /volumes` | 见下方参数表 | `Volume` |
| `Volume.connect(volume_id, *, config=None)` | `GET /volumes/{id}` | `volume_id`：卷标识 | `Volume` |
| `Volume.list(*, config=None)` | `GET /volumes` | — | `list[VolumeInfo]`（**`token` 恒为空**） |
| `Volume.get_info(volume_id, *, config=None)` | `GET /volumes/{id}` | `volume_id`：卷标识 | `VolumeInfo`（**含 `token`**） |
| `Volume.destroy(volume_id, *, config=None)` | `DELETE /volumes/{id}` | `volume_id`：卷标识 | `bool`（成功 `True`，404 时 `False`） |

**`Volume.create` 参数**

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `name` | `str \| None` | 否 | 卷名称。须满足 `^[a-zA-Z0-9_-]+$`，≤128 字符。省略时服务端分配 UUID。 |
| `driver` | `str \| None` | 否 | 卷插件名称（如 `"cos"`）。传 `None` 或 `""` 时不发送该字段，后端使用默认插件。 |
| `config` | `Config \| None` | 否 | SDK 配置对象，覆盖环境变量。 |

### 返回值详解

`create` / `connect` 返回可用的 `Volume` 实例，`list` / `get_info` 返回 `VolumeInfo`
数据，`destroy` 返回 `bool`。各方法返回的字段填充情况如下：

| 方法 | 返回类型 | `.volume_id` | `.name` | `.token` |
|---|---|:---:|:---:|:---:|
| `create` | `Volume` | ✅ 有值 | ✅ 有值 | ✅ 插件签发时有值，否则为空串 |
| `connect` | `Volume` | ✅ 有值 | ✅ 有值 | ✅ 插件签发时有值，否则为空串 |
| `list` | `list[VolumeInfo]` | ✅ 每项有值 | ✅ 每项有值 | ⚠️ **恒为空串**（列表不返回 token） |
| `get_info` | `VolumeInfo` | ✅ 有值 | ✅ 有值 | ✅ 插件签发时有值，否则为空串 |
| `destroy` | `bool` | — | — | — |

### `create`

对齐 e2b —— `Volume.create(name)` 不带 driver：

- **`create(name)`** —— e2b 兼容。请求体仅为 `{"name": ...}`；后端使用
  **第一个已配置**的卷插件。
- **`create(name, driver="cos")`** —— CubeSandbox 扩展。传入非空 `driver` 即绑定指定插件。
  仅在需要在多个插件中显式选择时才用。

> 省略 `name` 时，服务端生成一个 UUID，并将其同时用作卷名与卷 ID。

### `VolumeInfo`

| 属性 | 类型 | 说明 |
|---|---|---|
| `.volume_id` | `str` | 稳定标识（等于 `name` 或自动生成的 UUID）。由返回体的 `volumeID`/`volume_id` 映射而来。 |
| `.name` | `str` | 显示名称。 |
| `.token` | `str` | 插件签发的 token。`create` / `get_info` 时填充；**`list` 时恒为空串**。 |

### 挂载

传入一个 mount 路径到卷的映射（e2b 兼容）：

```python
Sandbox.create(volume_mounts={"/workspace": vol})
Sandbox.create(volume_mounts={"/workspace": "vol-xxx"})
```

每个值必须能解析为已存在的 `volume_id`。

---

## 示例

### 1. 创建

```python
from cubesandbox import Volume

vol = Volume.create("my-data")     # 指定名称
print(vol.volume_id, vol.name, vol.token)

vol = Volume.create()              # 省略 name，服务端生成 UUID
print(vol.volume_id)               # 自动生成的 UUID
```

### 2. 挂载进沙箱

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("my-data", driver="cos")

with Sandbox.create(
    volume_mounts={"/workspace": vol},
) as sb:
    sb.files.write("/workspace/note.txt", "已持久化！")
    print(sb.files.read("/workspace/note.txt"))   # "已持久化！"
```

### 3. 查询与销毁

```python
for v in Volume.list():            # 注意：此处 v.token 为 ""
    print(v.volume_id, v.name)

one = Volume.get_info(vol.volume_id)  # one.token 已填充
vol = Volume.connect(vol.volume_id)   # -> 返回可用的 Volume 实例
Volume.destroy(vol.volume_id)         # 删除前须先 kill 所有挂载它的沙箱（见注意事项）
```

### 4. 跨沙箱数据共享

多个沙箱可同时挂载同一个卷，数据对所有挂载者实时可见。

```python
from cubesandbox import Sandbox, Volume

vol = Volume.create("shared", driver="cos")
mount = {"/workspace": vol}

# 沙箱 A 写入数据。
a = Sandbox.create(volume_mounts=mount)
a.files.write("/workspace/probe.txt", "hello from A")

# 沙箱 B 挂载同一卷，立即可读。
with Sandbox.create(volume_mounts=mount) as b:
    print(b.files.read("/workspace/probe.txt"))   # "hello from A"

# ⚠️ 删除卷前，必须 kill 所有挂载它的沙箱。
a.kill()
b.kill()  # 上下文管理器退出时已自动 kill，此处仅为说明
Volume.destroy(vol.volume_id)
```

---

## 错误与状态码

`Volume` 的每个方法都会把非 2xx 响应经过同一套映射：

| HTTP 状态码 | 抛出的异常 | 含义 |
|---|---|---|
| 2xx | —（正常返回） | 成功 |
| 401 / 403 | `AuthenticationError` | 未认证 / 无权限 |
| 404 | `VolumeNotFoundError` | 卷不存在（`get_info` / `connect` / `destroy`） |
| 其余非 2xx（400 / 405 / 409 / 500 …） | `ApiError` | 参数非法、重名冲突、后端错误 |

客户端预校验会在**任何网络请求之前**就抛异常：

| 条件 | 异常 |
|---|---|
| `name` 不满足 `^[a-zA-Z0-9_-]+$` 或超过 128 字符 | `ValueError` |
| 挂载 dict 缺少 `name` / `path` | `ValueError` |

所有 API 异常都派生自 `CubeSandboxError`，并暴露 `.status_code`：

```python
from cubesandbox import Volume, VolumeNotFoundError, ApiError

try:
    Volume.get_info("does-not-exist")
except VolumeNotFoundError:
    ...                       # 404
except ApiError as e:
    print(e.status_code)      # 500
```

---

## 注意事项

- **删除卷前必须 kill 所有挂载方。** 一个卷可以被多个沙箱同时挂载。`Volume.destroy()` 不会
  自动 detach——若仍有运行中的沙箱持有该卷，删除操作可能失败或导致后端挂载泄漏。务必先对所有
  挂载它的沙箱执行 `sb.kill()`，再调用 `Volume.destroy(volume_id)`。
- **`list` 不返回 token。** token 仅在 `create` 和 `get_info` 时暴露；需要 token 时请调用
  `Volume.get_info(volume_id)`。
- **`name` 可选。** 省略时服务端分配一个 UUID，该 UUID 同时用作 `volume_id` 和 `name`。
