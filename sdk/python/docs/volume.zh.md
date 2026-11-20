# 持久化卷：同步管理与无沙箱直通访问

英文参考见 [volume.md](volume.md)。固定 E2B 版本（`e2b-dev/E2B@8cc9ed1e`，核对于
2026-11-02）、HTTP 协议和兼容边界以本文件「直通内容 API」章节为准。

Python SDK 的普通 Volume 文件操作通过管理 API 获取短时、单卷范围凭证，再使用 `boto3` 直连 S3-compatible endpoint；不会创建辅助沙箱，也不会让文件字节经过 CubeAPI/CubeMaster。安装 `.[s3]` extra 后可访问已实现凭证 Hook 的 S3-compatible 后端（AWS S3、COS、R2、MinIO 等）。凭证 Hook 尚未在仓库自带的 S3/COS 示例插件中实现，因此部署端需先配置能签发符合 TTL 和 prefix 限制的 Hook。显式传入内容 `token` 时仍保留旧 HTTP 内容网关路径。

## 配置与鉴权

| 配置 | 用途 |
|---|---|
| `CUBE_API_URL` / `E2B_API_URL` 或 `Config.api_url` | 管理 API：CRUD 与申请对象存储凭证 |
| `CUBE_API_KEY` / `E2B_API_KEY` 或 `Config.api_key` | 仅发给管理 API；不会发给 S3 endpoint |
| 安装 `.[s3]` extra | 提供通用 S3 客户端依赖 `boto3` |
| `CUBE_VOLUME_API_URL` / `E2B_VOLUME_API_URL` | 仅用于旧 HTTP 内容网关模式 |
| 显式内容 `token` | 选择旧 HTTP 网关模式；不是 Volume CRUD 返回的旧 token |
| `request_timeout`、`headers`、`proxy`、`http_version`、`logger` | 管理 API/旧网关逐次请求选项；S3 SDK 使用其自身传输配置 |

`Volume.create/connect(config=...)` 句柄首次执行文件操作时，会用管理 API key 请求
`POST /volumes/{id}/content-token`，缓存 S3 凭证并在剩余有效期不超过 2 分钟时刷新；对象请求直接发给凭证中的 endpoint，使用 `addressing_style`、region 和 session token。管理 key 和管理自定义 headers 不会发给对象存储。endpoint 必须是 HTTPS（仅 loopback 开发地址允许 HTTP）。直连不需要模板、`CUBE_PROXY_NODE_IP` 或沙箱 access token。

普通请求默认 60 秒，绑定 Cube Config 时保留其预算；缓冲文件操作默认一小时。
`request_timeout=0` 禁用超时。同步流默认 read 空闲限制为 60 秒；和所核对的 E2B
同步实现一样，`stream_idle_timeout` 参数不单独生效。HTTPX 超时是阶段/空闲限制，
尚不是与 E2B 原生传输完全相同的 wall-clock 总期限。

## 已有管理 API

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
| `Volume.create_with_private_data(name=None, *, private_data, driver=None, config=None)` | `POST /v2/volumes` | 不透明字符串输入；Cube/SDK ≥ 0.8.0 | `Volume` |
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

### 插件输入

通过 `Volume.create_with_private_data("my-data", driver="my-storage", private_data='{"cred_ref":"account-7"}')`
调用独立的新创建入口。要求 CubeAPI、所有处理卷操作的 CubeMaster 与 Cubelet 均为 0.8.0
或更高版本，所选插件支持该协议，且 Python SDK ≥ 0.8.0；原 `Volume.create` 的定义和路径保持不变。
`private_data` 必须是有效 UTF-8，最多 1024 字节，不含 NUL，且字符范围不超出 BMP。
只传凭据引用，不传明文密钥。空字符串会显式转发；CubeAPI 不支持 `/v2/volumes` 时直接报错，
不会回退旧路径并丢弃输入。校验只检查一次 JSON 解码后的字符串；ASCII 字面量 `\uD83D\uDE00`
仍按原样处理，不会再解析。返回的 `Volume` 继续使用已有查询、删除和挂载 API。

升级顺序：先检查或升级 Controller/Node 插件，确保 Create 接收输入 A、Attach/Destroy
使用返回状态 B；再升级**所有处理卷操作的 CubeMaster**；最后部署新 CubeAPI 并开始调用新方法。
新 API 连接旧 Master 时，可能返回 201 但 A 已被静默丢弃，旧 Master 删除时也不会传 B。
SDK 不会自动检测这种混合版本部署。回退版本时，依赖 A/B 的既有卷仍须由兼容的 Master 处理。
详情见[插件升级说明](../../../docs/zh/guide/volume-plugin.md)。

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

## 直连 S3 文件操作

未显式传入内容 `token` 时，实例级文件操作使用短时凭证直接访问 S3-compatible endpoint；文件 key 固定映射为服务端凭证 `prefix` + 去掉开头 `/` 的卷内路径。S3 目录是 prefix/marker 的近似表示，不等同于 POSIX 文件系统。

| Python 方法 | S3 操作 | 返回/边界 |
|---|---|---|
| `vol.read_file(path, format="text")` | `GetObject` | UTF-8 `str`；仅对象存在时可读 |
| `vol.read_file(path, format="bytes")` | `GetObject` | `bytes` |
| `vol.read_file(path, format="stream")` | 流式 `GetObject` | 惰性同步迭代器；用完后关闭迭代器 |
| `vol.write_file(path, data, force=None)` | `PutObject` | `VolumeEntryStat`；str/bytes/file-like 支持 |
| `vol.list(path)` | `ListObjectsV2` | 当前层对象和公共前缀 |
| `vol.get_info(path)` / `vol.exists(path)` | `HeadObject` / prefix 检查 | 对象存储元数据是近似值 |
| `vol.make_dir(path)` | 写入 0 字节目录 marker | 仅用于兼容目录 API |
| `vol.remove(path)` | `DeleteObject` | 删除对象；不支持递归删除目录 |
| `vol.update_metadata(...)` | — | 当前直连模式不支持，抛出 `VolumeContentUnavailableError` |

类级 `Volume.list/get_info` 仍是管理 API；实例级方法处理对象内容。path 必须是卷内绝对 POSIX 路径；NUL、反斜线和点穿越段被拒绝，不能通过传入路径离开凭证 prefix。`mode/uid/gid` 仅作为对象自定义元数据，不构成 POSIX 权限保证。

显式传入内容 `token` 会保留旧 HTTP 内容网关模式；此时需配置 `CUBE_VOLUME_API_URL`。

```python
import os
from contextlib import closing
from cubesandbox import Config, Volume

config = Config(api_url=os.environ["CUBE_API_URL"], api_key=os.environ["CUBE_API_KEY"])
vol = Volume.connect(os.environ["CUBE_VOLUME_ID"], config=config)
path = "/docs/example.txt"
vol.write_file(path, "hello S3", force=True)
assert vol.read_file(path) == "hello S3"
assert vol.read_file(path, format="bytes") == b"hello S3"
with closing(vol.read_file(path, format="stream")) as chunks:
    assert b"".join(chunks) == b"hello S3"
vol.remove(path)
```

## 无沙箱 E2E

在 `sdk/python` 执行；完整变量表和清理说明见
[中文 README](../README.zh.md#直通卷-e2e-测试)。

```sh
python3 -m pip install -e '.[dev,s3]'
python3 -m pytest -q tests/test_volume.py tests/test_volume_content.py tests/test_volume_s3.py
python3 -m pytest -q tests/test_volume_direct_harness.py
```

旧 HTTP 网关兼容 E2E 保留在 `test_volume_direct_e2e.py`。新的直连 S3 E2E 在仓库根目录执行：

```sh
SDK_E2E_DIRECT_S3_VOLUME_ID=<existing-volume> python3 -m pytest \
  tests/e2e/sdk_compat/cases/volume/test_s3_direct_python.py --run-e2e --sdk-e2e-backends=cubesandbox
```

需提供管理 API 的 `CUBE_API_URL`、`CUBE_API_KEY`，并指定一个已配置凭证 Hook 的现有卷 ID；测试只写唯一对象并删除它，不创建/连接沙箱，也不销毁卷。`e2b` backend 不支持 CubeSandbox Volume Plugin，该测试按 capability 自动跳过。

## 异常

| HTTP | Python 行为 |
|---|---|
| 内容 404 | `VolumePathNotFoundException`；exists 返回 False |
| 管理详情/连接 404 | `VolumeNotFoundException` |
| 管理 destroy 404 | False |
| 401 | `AuthenticationException`，兼容别名 `AuthenticationError` |
| 429 | `RateLimitException` |
| 503 | `ServiceBusyException` |
| 其他非 2xx（含 403） | `VolumeException`，同时兼容 Cube `ApiError` 捕获 |

鉴权和服务错误不变成空列表或 False。内容任何 404 都按 E2B 视为路径不存在，
因此不能用 exists 判断路由是否部署。所有认证请求禁止自动重定向。

## 沙箱挂载是另一条已有链路

原有 `Sandbox.create(volume_mounts={path: vol})` 保留。映射值可为 Volume、
VolumeInfo 或 ID 字符串；`VolumeMount(vol, read_only=True)` 仅控制本次沙箱挂载，
不生成不可变快照。这条链路不是直通读取前置条件，不会在上述 E2E 中调用。
删除卷应先释放所有真实挂载/读取引用，SDK 不自动 detach。
