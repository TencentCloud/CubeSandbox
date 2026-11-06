# 持久化卷：同步管理与无沙箱直通访问

英文参考见 [volume.md](volume.md)。固定 E2B 版本、HTTP 协议和兼容边界见
[E2B Volume 合同](../../../docs/e2b-volume-contract.md)，直通服务端范围见
[Volume 直通方案](../../../docs/volume-direct-access.md)。

本次只提供同步 Python，不加入异步或原生传输栈，也不为了读取卷而启动辅助沙箱。
现有卷管理、沙箱挂载与独立内容访问是三种不同能力。仓内有管理和插件挂载接口，
尚未找到完整的独立内容网关和卷级能力凭证签发链路。客户端可对接已部署的兼容网关，
不能据此宣称仅现有 CubeAPI 就已支持直接读取文件。

## 配置与鉴权

| 配置 | 用途 |
|---|---|
| `CUBE_API_URL` / `E2B_API_URL` 或管理 `api_url` | 管理 `/volumes` |
| `CUBE_API_KEY` / `E2B_API_KEY` 或管理 `api_key` | 管理 `X-API-Key` |
| `CUBE_VOLUME_API_URL` / `E2B_VOLUME_API_URL` 或内容 `api_url` | 内容网关地址 |
| 句柄 token 或内容 `token` | 内容 `Authorization: Bearer ...` |
| `domain`、`debug` | 无显式地址时按 E2B 域名/debug 规则解析 |
| `request_timeout`、`headers`、`proxy`、`http_version`、`logger` | 逐次请求选项 |

管理和内容地址分别解析。默认内容地址为 `https://api.<domain>`，domain 为
`E2B_DOMAIN` 或 `e2b.app`；debug 使用 `http://localhost:8080`。
Cube 额外保留绑定 `Config` 的兼容入口。非本机内容请求须 HTTPS，不能用管理 key
或空插件 token 冒充内容凭证。直通不需要模板、`CUBE_PROXY_NODE_IP` 或沙箱 access token。

普通请求默认 60 秒，绑定 Cube Config 时保留其预算；缓冲文件操作默认一小时。
`request_timeout=0` 禁用超时。同步流默认 read 空闲限制为 60 秒；和所核对的 E2B
同步实现一样，`stream_idle_timeout` 参数不单独生效。HTTPX 超时是阶段/空闲限制，
尚不是与 E2B 原生传输完全相同的 wall-clock 总期限。

## 已有管理 API

| Python 方法 | HTTP | 返回 |
|---|---|---|
| `Volume.create(name, **opts)` | `POST /volumes`，JSON `{name}` | `Volume` |
| `Volume.connect(volume_id, **opts)` | `GET /volumes/{id}` | `Volume`，不创建沙箱 |
| `Volume.list(**opts)` | `GET /volumes` | `list[VolumeInfo]` |
| `Volume.get_info(volume_id, **opts)` | `GET /volumes/{id}` | `VolumeAndToken` |
| `Volume.destroy(volume_id, **opts)` | `DELETE /volumes/{id}` | 成功 True，404 False |

`name` 是 Python 必填入参，非空值须满足 `^[a-zA-Z0-9_-]+$` 且最多 128 字符。
标准创建仅发送 name；Cube 可选 `driver` 扩展仅在显式非空时发送。
管理方法支持 E2B 风格直接 kwargs；`config` 作为 Cube 兼容扩展保留。

`VolumeInfo` 只有 `volume_id/name`，列表条目没有 token 属性。
`VolumeAndToken` 在基础信息上增加 `token/domain`。时间类型不涉及管理模型。
创建或连接句柄保留响应 domain；默认 S3 插件不会因此自动生成内容 token。

## 直通内容 API

以下是内容网关协议，不是把卷 ID 替换进现有沙箱文件接口。
所有路径使用 `/volumecontent/{volumeID}` 前缀，卷内 `path` 放在查询参数中。

| Python 方法 | HTTP | 返回 |
|---|---|---|
| `vol.read_file(path, format="text", stream_idle_timeout=None, **opts)` | `GET /file?path=...` | str / bytes / Iterator[bytes] |
| `vol.write_file(path, data, uid=None, gid=None, mode=None, force=None, **opts)` | `PUT /file`，原始字节；元数据查询参数 | `VolumeEntryStat` |
| `vol.list(path, depth=None, **opts)` | `GET /dir`，path/depth 查询参数 | `list[VolumeEntryStat]` |
| `vol.make_dir(path, uid=None, gid=None, mode=None, force=None, **opts)` | `POST /dir`，查询参数 | `VolumeEntryStat` |
| `vol.get_info(path, **opts)` | `GET /path?path=...` | `VolumeEntryStat` |
| `vol.update_metadata(path, uid=None, gid=None, mode=None, **opts)` | `PATCH /path`，JSON 元数据 | `VolumeEntryStat` |
| `vol.exists(path, **opts)` | 复用查询元数据 | bool |
| `vol.remove(path, **opts)` | `DELETE /path?path=...` | None |

类级 list/get_info 走管理，实例级方法必须有内容 path，不再按相对路径猜测管理调用。
path 为卷内绝对 POSIX 路径，不是宿主机路径；NUL、反斜线和点穿越段被拒绝，
禁止删除卷根目录。客户端校验不能代替网关的符号链接和跨租户隔离。
未指定元数据省略；显式 uid/gid/mode/depth 的 0 和 force=False 都保留。

文件线上返回原始字节。Python 文本为 str，bytes 为 bytes，stream 为惰性同步迭代器。
元数据字段为 `name/type/path/size/mode/uid/gid/atime/mtime/ctime/target`；
type 为 `unknown/file/directory/symlink` 枚举，时间返回 datetime，target 可为 None。

```python
import os
from contextlib import closing
from cubesandbox import Volume

vol = Volume(os.environ["CUBE_VOLUME_ID"], "dataset", os.environ["CUBE_VOLUME_CONTENT_TOKEN"])
opts = {"api_url": os.environ["CUBE_VOLUME_API_URL"]}
path = os.environ["CUBE_VOLUME_TEST_PATH"]
data = vol.read_file(path, format="bytes", **opts)
info = vol.get_info(path, **opts)
entries = vol.list("/", depth=1, **opts)
with closing(vol.read_file(path, format="stream", **opts)) as chunks:
    for chunk in chunks:
        pass
```

## 无沙箱 E2E

在 `sdk/python` 执行；完整变量表和清理说明见
[中文 README](../README.zh.md#直通卷-e2e-测试)。

```sh
python3 -m pip install -e '.[dev]'
python3 -m pytest -q tests/test_volume.py tests/test_volume_content.py tests/test_volume_direct_harness.py
python3 -m pytest -q tests/test_volume_direct_e2e.py --run-e2e
```

真实测试必需 `CUBE_VOLUME_API_URL`（或 `E2B_VOLUME_API_URL`）、`CUBE_VOLUME_ID`、
`CUBE_VOLUME_CONTENT_TOKEN`、`CUBE_VOLUME_TEST_PATH`；最后一项必须是稳定已有文件。
凭证从环境注入，不提交到仓库。可选 SHA-256 校验预期数据。

16 个用例覆盖二进制、流、可选文本、元数据、目录深度、路径缺失、非法 token、
逐次地址/token/headers 和提前关闭流。默认只读；管理验证由
`CUBE_VOLUME_TEST_MANAGEMENT=1` 开启，写入由 `CUBE_VOLUME_E2E_WRITE=1` 单独开启。
写入只在唯一 `sdk-e2e-*` 目录中进行并清理自建路径，绝不销毁传入的卷。
未加 `--run-e2e` 时所有真实用例跳过；开启后缺配置会失败。

本地回环验证器用相同用例校验只读/写入模式和清理；不是生产服务验证。
用例禁止 SDK 沙箱创建/连接和非卷 HTTP 路由，但服务端是否启动 VM、挂载租约是否
正确仍需服务端观测。没有部署兼容网关时测试失败应修服务，不允许启动沙箱兜底。

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
