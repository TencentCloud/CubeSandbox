# cube-envd API

[English](cube-envd-api.md) · [构建与使用](../README_zh.md)

本文说明 cube-envd 注册的 guest 接口，默认监听 **49983** 端口。沙箱与模板创建属于
CubeSandbox 平台 API，不由 daemon 提供；参见 [Python SDK](../../sdk/python)。

## 目录

- [连接与认证](#连接与认证)
- [HTTP 接口](#http-接口)
- [Process RPC](#process-rpc)
- [Filesystem RPC](#filesystem-rpc)
- [Connect 编码与流式传输](#connect-编码与流式传输)
- [错误与限制](#错误与限制)
- [协议源码与示例](#协议源码与示例)

## 连接与认证

将 `ENVD_URL` 设置为 sandbox 中 envd 的可达地址。经 CubeProxy 访问时，通常使用
`sandbox.get_host(49983)` 返回的主机名，协议和代理端口按部署配置选择。使用代理
节点 IP 时，必须在 `Host` 中保留该 sandbox 主机名。CubeAPI 地址不是 envd 地址。

下文示例使用 curl 和部署提供的令牌：

```bash
export ENVD_URL='https://envd.example.com'  # 替换为你的 sandbox 接口地址。
export ENVD_ACCESS_TOKEN='replace-with-the-sandbox-envd-token'
```

| 请求头或参数 | 含义 |
| --- | --- |
| `X-Access-Token` | 通过 `/init` 安装的 daemon 访问令牌；设置后访问受保护接口必须携带。 |
| `Authorization: Basic ...` | 为 Process/Filesystem RPC 选择 guest 用户。只使用用户名，密码不是登录凭据；例如 `curl --user root:`。 |
| 查询参数 `username` | 为 `GET /files` 和 `POST /files` 选择 guest 用户。 |
| JSON 字段 `username` | 为 `POST /files/compose` 选择 guest 用户。 |

未显式指定执行用户时，操作使用 daemon 当前默认用户，初始为 `root`。所选用户名
必须存在于 guest。文件 HTTP handler 根据查询参数或 JSON 字段选择执行用户，
不使用 Basic 用户名；但如果提供了 Basic 用户名，公共中间件仍会验证它。
平台 API key、代理流量令牌与 `X-Access-Token` 是不同的凭据。普通 sandbox 访问
应使用 SDK 已配置的路由与凭据。

`GET /health` 不要求 daemon 令牌；`/init` 自行校验令牌变更。
`GET /files` 和 `POST /files` 接受 daemon 令牌或签名 URL，`/files/compose` 不接受
签名 URL 认证。尚未安装 daemon 令牌时，其令牌检查不生效；这不代表外围平台
没有自己的认证要求。

### 文件 URL 签名

`/files` 支持查询参数 `path`、`username`、`signature`，以及可选的
`signature_expiration`（Unix 秒，带符号 64 位十进制整数）。

签名计算方式：

```text
input = path + ":" + operation + ":" + username + ":" + accessToken
input = input + ":" + decimalExpiration   # 仅在提供过期时间时追加。
signature = "v1_" + base64_without_padding(SHA256(UTF8(input)))
```

GET 的 `operation` 为 `read`，POST 为 `write`。Base64 使用标准字母表而非 URL-safe
字母表，查询参数需要 URL 编码。`path` 和 `username` 使用查询参数解码后、文件系统
路径展开前的字符串，省略时按空字符串参与计算。这是拼接输入的 SHA-256，不是
HMAC。过期时间早于当前 Unix 秒时拒绝访问。

非空 `X-Access-Token` 请求头优先于签名；即使 URL 签名正确，错误的头部令牌也会
被拒绝。`signature_expiration` 格式错误返回 HTTP 400，即使使用头部令牌认证也
如此。凭据缺失、错误或过期返回 HTTP 401。

## HTTP 接口

| 方法 | 路径 | 请求 | 成功响应 |
| --- | --- | --- | --- |
| GET | `/health` | 无请求体。 | 204，空响应体。 |
| POST | `/init` | 初始化 JSON。 | 204，空响应体。 |
| GET | `/envs` | 无请求体。 | 200，JSON 环境变量映射。 |
| GET | `/metrics` | 无请求体。 | 200，JSON 指标对象。 |
| GET | `/files` | 文件查询参数，可选 Range/条件请求头。 | 200 文件字节；满足范围请求时为 206；内容未修改时为 304。 |
| POST | `/files` | 原始字节或 multipart 文件分段。 | 200，写入文件摘要的 JSON 数组。 |
| POST | `/files/compose` | 目标路径和有序源路径。 | 200，目标文件摘要 JSON。 |

不支持的方法在适用的就绪、认证检查后返回 405。尤其不能用 HEAD 代替
`/health`、`/files`、`/envs` 或 `/metrics` 的 GET。CORS 预检使用带
`Access-Control-Request-Method` 的 OPTIONS，返回 204；预检成功不代表目标接口
实现了 CORS 宣告的所有方法。

### GET /health

daemon 就绪检查通过时返回 204，否则返回 503。成功响应体为空，并带有
`Cache-Control: no-store`。

```bash
curl -sS -o /dev/null -w '%{http_code}\n' "$ENVD_URL/health"
```

### POST /init

这是通常由运行时调用的平台初始化接口，会修改 guest 状态，不负责创建 sandbox。
JSON 字段如下：

| 字段 | 类型 | 作用 |
| --- | --- | --- |
| `envVars` | 字符串到字符串的对象映射 | 合并到后续操作使用的 daemon 环境。 |
| `accessToken` | 非空字符串 | 安装或验证 daemon 令牌。 |
| `defaultUser` | 字符串 | 非空时设置默认执行用户。 |
| `defaultWorkdir` | 字符串 | 非空时设置默认工作目录。 |
| `caBundle` | 字符串 | 提供 guest 证书配置所需的 CA 内容。 |
| `hyperloopIP` | 字符串 | 提供 guest 事件转发地址。 |
| `timestamp` | 带时区的时间戳字符串 | 控制初始化顺序，并尝试设置 guest 时钟。 |
| `volumeMounts` | 对象数组 | 每项包含字符串 `nfs_target` 和 `path`，用于 guest NFS 配置。 |

字段可省略，但省略字段不能绕过已安装的令牌。已有令牌时，请求体必须携带相同
令牌，或在修改/重置令牌时满足平台 MMDS 的令牌哈希验证。仅有头部令牌不授权
省略或变更请求体令牌，未授权变更返回 401。早于已有时间戳的初始化会被忽略；
相同时间可再次应用。后续操作失败前可能已经产生初始化副作用，因此返回错误
不保证事务式回滚。

平台管理隔离 guest 时的请求体示例：

```json
{
  "envVars": {"APP_MODE": "demo"},
  "accessToken": "example-guest-token",
  "defaultUser": "user",
  "defaultWorkdir": "/tmp"
}
```

请求体限制为 256 KiB，JSON 嵌套限制为 64 层；超出大小返回 413，嵌套过深、JSON 或
时间戳格式错误返回 400。时钟和挂载初始化应始终在 guest 隔离边界内执行。

### GET /envs 与 GET /metrics

两个接口均使用普通 daemon 令牌认证，并返回 `Cache-Control: no-store`。
`/envs` 返回当前 daemon 环境，不代表某个子进程叠加独立覆盖项后的完整环境。

```bash
curl -sS -H "X-Access-Token: $ENVD_ACCESS_TOKEN" "$ENVD_URL/envs"
curl -sS -H "X-Access-Token: $ENVD_ACCESS_TOKEN" "$ENVD_URL/metrics"
```

| 指标字段 | 类型/单位 | 含义 |
| --- | --- | --- |
| `ts` | 整数，Unix 秒 | 采样时间。 |
| `cpu_count` | 整数 | guest 可见的 CPU 数量。 |
| `cpu_used_pct` | 数值，百分比 | daemon 相邻采样间的 CPU 忙碌比例；首次采样使用自启动以来的计数器。 |
| `mem_total`、`mem_used`、`mem_cache` | 整数，字节 | guest 内存总量、已用量和缓存。 |
| `mem_total_mib`、`mem_used_mib` | 整数，MiB | 内存值除以 1024²。 |
| `disk_total`、`disk_used` | 整数，字节 | 根文件系统容量及基于可用块计算的使用量。 |

这些是 guest 资源指标，不是 daemon RSS。采样失败返回 500。

### GET /files

通过 `path` 指定 guest 文件，可选 `username` 指定执行用户。响应为文件内容，
没有 JSON 外层包装。下载支持 gzip 协商、包括 multipart 在内的字节范围，以及
HTTP 修改时间条件请求。无法满足的范围返回 416，前置条件失败可能返回 412。

```bash
curl -sS --fail --get "$ENVD_URL/files" \
  -H "X-Access-Token: $ENVD_ACCESS_TOKEN" \
  --data-urlencode 'path=/tmp/envd-api.txt' \
  --data-urlencode 'username=root' --output downloaded.txt
```

`/tmp/envd-api.txt` 是 sandbox 内的路径，`downloaded.txt` 写到调用者当前目录。
文件不存在返回 404。路径可以是绝对路径，或相对于所选 guest 用户 home 的路径；
`~` 和 `~/...` 指向该 home，不支持 `~otheruser` 展开。

### POST /files

| Content-Type | 请求体与目标路径 |
| --- | --- |
| `application/octet-stream` | 原始文件字节，必须提供查询参数 `path`。 |
| `multipart/form-data` 或 `multipart/mixed` | 文件分段使用 `Content-Disposition: form-data; name="file"; filename="..."`；默认按各分段 filename 写入，也可用 `path` 覆盖目标。 |

请求体支持 `Content-Encoding: gzip`。上传会截断已有目标文件，按需创建父目录。
上传不是事务：错误或断连可能留下被截断或部分写入的文件，也可能已有部分
multipart 文件写入成功。daemon 没有额外的上传总大小上限，但仍受磁盘、权限、
解析器及资源限制影响。

```bash
printf 'hello from the API\n' > upload.txt
curl -sS --fail -X POST "$ENVD_URL/files?path=%2Ftmp%2Fenvd-api.txt&username=root" \
  -H "X-Access-Token: $ENVD_ACCESS_TOKEN" \
  -H 'Content-Type: application/octet-stream' --data-binary @upload.txt
```

响应体示例：

```json
[{"name":"envd-api.txt","path":"/tmp/envd-api.txt","type":"file"}]
```

出于兼容性，成功响应包含 JSON，但 Content-Type 为 `text/plain; charset=utf-8`。
它是文件摘要数组，不是 protobuf `EntryInfo` 响应。

### POST /files/compose

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `destination` | 非空字符串 | guest 目标路径。 |
| `source_paths` | 非空字符串数组 | 按此顺序拼接的 guest 源文件。 |
| `username` | 可选字符串 | guest 执行用户。 |

```json
{
  "destination": "/tmp/combined.txt",
  "source_paths": ["/tmp/part-1.txt", "/tmp/part-2.txt"],
  "username": "root"
}
```

handler 写入临时文件后通过 rename 发布目标，再尝试删除已消费且未变化的源文件。
源文件清理尽力执行；已发现被替换的源文件会保留。已接受的 compose 可以在客户端
断连后继续完成。200 响应为包含 `name`、`path`、`type: "file"` 的单个文件摘要对象，
Content-Type 为 `application/json`。该接口使用 `X-Access-Token`，不使用 `/files`
的签名查询参数。

## Process RPC

全部方法使用 **POST**，路径前缀为 `/process.Process/`。下文类型与字段使用
ProtoJSON 写法；源协议见 [process.proto](../proto/process.proto)。

| 方法 | 请求字段 | 响应 | 调用形式 |
| --- | --- | --- | --- |
| `List` | `{}` | `processes: ProcessInfo[]` | Unary。 |
| `Start` | `process: ProcessConfig`，可选 `pty`、`tag`、`stdin` | `event: ProcessEvent` | 服务端流。 |
| `Connect` | `process: ProcessSelector` | `event: ProcessEvent` | 服务端流。 |
| `Update` | `process: ProcessSelector`，可选 `pty` | `{}` | Unary，更新 PTY 尺寸。 |
| `SendInput` | `process: ProcessSelector`、`input: ProcessInput` | `{}` | Unary。 |
| `StreamInput` | 由 `start`、`data` 或 `keepalive` 消息组成的序列 | `{}` | 客户端流。 |
| `SendSignal` | `process: ProcessSelector`、`signal` | `{}` | Unary。 |
| `CloseStdin` | `process: ProcessSelector` | `{}` | Unary。 |

### Process 类型

| 类型/字段 | 类型 | 含义 |
| --- | --- | --- |
| `ProcessConfig.cmd` | 字符串 | 可执行程序；shell 语法需显式调用 shell，例如 `/bin/sh` 配合 `args: ["-c", "..."]`。 |
| `ProcessConfig.args` | 字符串数组 | 程序参数。 |
| `ProcessConfig.envs` | 字符串映射 | 单个进程的环境覆盖项。 |
| `ProcessConfig.cwd` | 可选字符串 | 工作目录；未提供时使用初始化默认值，再回退到 guest 用户 home。相对路径相对于 home。 |
| `StartRequest.pty.size` | 含 `cols`、`rows` uint32 的对象 | 请求 PTY 及其尺寸。 |
| `StartRequest.tag` | 可选字符串 | 选择器标签；需要无歧义定位时使用返回的 PID。 |
| `StartRequest.stdin` | 可选布尔值 | 保持非 PTY stdin 管道打开，省略时默认 true；SDK 默认值可能不同。 |
| `ProcessSelector` | `pid: uint32` 或 `tag: string` 二选一 | 选择已有受管理进程。 |
| `ProcessInfo` | `config`、`pid`、可选 `tag` | List 返回的进程信息。 |
| `ProcessInput` | `stdin: bytes` 或 `pty: bytes` 二选一 | JSON 使用 Base64，例如 `"aGVsbG8K"` 代表带换行的 hello。 |
| `signal` | 枚举 | `SIGNAL_SIGTERM`（15）或 `SIGNAL_SIGKILL`（9）；未指定及其他不支持值会被拒绝。 |

Start 和 Connect 先发送带 PID 的 `event.start`，随后发送输出事件，最后发送进程
结束事件。进程非零退出本身不代表 RPC 传输失败；需要检查 `event.end.exitCode`
及其余结束字段。

| 进程事件 | 内容 |
| --- | --- |
| `start` | `pid: uint32`。 |
| `data` | `stdout`、`stderr`、`pty` 三者之一，内容为字节。 |
| `end` | `exitCode: sint32`、`exited: bool`、`status: string`，可选 `error: string`。 |
| `keepalive` | 空对象，不是命令输出。 |

Connect 订阅已有进程，不是持久化输出回放接口。输出订阅断开不会自行终止进程，
显式终止使用 SendSignal。CloseStdin 仅为非 PTY 进程发送 EOF；PTY 应通过 PTY
输入发送 Ctrl+D（`0x04`，Base64 为 `BA==`）。

StreamInput 先发送 `{"start":{"process":{"pid":123}}}`，再发送
`{"data":{"input":{"stdin":"aGVsbG8K"}}}` 等消息，序列保持输入顺序。
关闭 RPC 输入流不代替 CloseStdin。

Start 的 `Connect-Timeout-Ms` 在普通正数时同时控制进程存活期限和响应订阅期限；
Connect 的该请求头只限制本次订阅。SendInput、StreamInput 和 CloseStdin 可能等待
管道，不能把有效 Connect timeout 视为保证取消写入的机制。
Start/Connect 的 `Keepalive-Ping-Interval` 单位为秒，默认 90。

### Unary 示例

```bash
curl -sS --fail -X POST "$ENVD_URL/process.Process/List" \
  -H "X-Access-Token: $ENVD_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  --user root: --data '{}'
```

## Filesystem RPC

全部方法使用 **POST**，路径前缀为 `/filesystem.Filesystem/`。文件内容读写通过
HTTP `/files` 完成，本服务没有 `ReadFile` 或 `WriteFile` RPC。
源协议见 [filesystem.proto](../proto/filesystem.proto)。

| 方法 | 请求字段 | 响应 | 行为 |
| --- | --- | --- | --- |
| `Stat` | `path: string` | `entry: EntryInfo` | 获取元数据，包括末级符号链接信息。 |
| `MakeDir` | `path: string` | `entry: EntryInfo` | 创建目录及所需父目录；目标目录已存在时返回冲突。 |
| `Move` | `source: string`、`destination: string` | `entry: EntryInfo` | 移动或重命名条目。 |
| `ListDir` | `path: string`、`depth: uint32` | `entries: EntryInfo[]` | 列出后代，省略 depth 或设为 0 时按 1 处理。 |
| `Remove` | `path: string` | `{}` | 删除文件或递归删除目录；路径不存在也成功。 |
| `WatchDir` | `path: string`、`recursive: bool` | WatchDirResponse 流 | 持续监听，直到取消或终止错误。 |
| `CreateWatcher` | `path: string`、`recursive: bool` | `watcherId: string` | 创建轮询 watcher。 |
| `GetWatcherEvents` | `watcherId: string` | `events: FilesystemEvent[]` | 取出并清空已排队事件；空队列无事件。 |
| `RemoveWatcher` | `watcherId: string` | `{}` | 释放轮询 watcher。 |

路径位于 guest 文件系统，按所选用户权限操作。绝对路径保持绝对路径，相对路径和
`~/...` 相对于 home。空路径优先使用初始化的默认工作目录，否则使用用户 home。
破坏性操作保护文件系统根目录。

### EntryInfo

| 字段 | Proto 类型 / JSON 形式 | 含义 |
| --- | --- | --- |
| `name` | 字符串 | 条目名称。 |
| `type` | FileType 枚举 | `FILE_TYPE_FILE`、`FILE_TYPE_DIRECTORY` 或 `FILE_TYPE_SYMLINK`；零值为 `FILE_TYPE_UNSPECIFIED`。 |
| `path` | 字符串 | 解析后的条目路径。 |
| `size` | int64 / 十进制字符串 | 字节大小。 |
| `mode` | uint32 / 数值 | 数值形式的 mode 信息。 |
| `permissions` | 字符串 | 可读权限信息。 |
| `owner`、`group` | 字符串 | 所有者和组信息。 |
| `modifiedTime` | protobuf Timestamp / 字符串 | 时间戳形式的修改时间。 |
| `symlinkTarget` | 可选字符串 | 符号链接目标文本。 |

### 目录事件

`FilesystemEvent` 包含 `name: string` 和 `type: EventType`。
EventType 包括 `EVENT_TYPE_CREATE`、`EVENT_TYPE_WRITE`、`EVENT_TYPE_REMOVE`、
`EVENT_TYPE_RENAME`、`EVENT_TYPE_CHMOD`，零值为 `EVENT_TYPE_UNSPECIFIED`。
名称相对于监听目录。事件是文件系统通知，不保证一次用户操作恰好对应一个事件，
也不是持久化变更日志。

WatchDirResponse 为 `start: {}`、`filesystem: FilesystemEvent`、`keepalive: {}`
之一。轮询客户端先 CreateWatcher，再重复 GetWatcherEvents，最后 RemoveWatcher
释放资源；第二次轮询不会重放第一次已经取出的事件。流式客户端必须读取 Connect
结束记录，以区分正常结束与错误。

### Unary 示例

```bash
curl -sS --fail -X POST "$ENVD_URL/filesystem.Filesystem/Stat" \
  -H "X-Access-Token: $ENVD_ACCESS_TOKEN" \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  --user root: --data '{"path":"/tmp/envd-api.txt"}'
```

## Connect 编码与流式传输

| 调用形式 | Connect Content-Type | 请求体 |
| --- | --- | --- |
| Unary | `application/json` 或 `application/proto` | 单个 JSON 对象或 protobuf 消息。 |
| 服务端流/客户端流 | `application/connect+json` 或 `application/connect+proto` | 带长度前缀的消息帧。 |

使用 `Connect-Protocol-Version: 1`。JSON 使用 lowerCamelCase 字段名、枚举名、
Base64 字节、int64 十进制字符串和时间戳字符串。默认值字段可能省略。
protobuf `oneof` 的多个备选项只能选择一个，Proto 字段名见链接的 schema。

同一组方法也注册了 gRPC 和二进制 gRPC-Web 类型，支持 protobuf 或 JSON payload：
`application/grpc`、`application/grpc+proto`、`application/grpc+json` 及相应
`application/grpc-web` 变体。它们有独立的帧及终止状态约定，不能当作 Connect 帧
解码。本文 RPC 示例使用 Connect，与 SDK 的 RPC 调用一致；文件内容传输使用上文
的 HTTP 接口。

每个 Connect 流式帧结构如下：

```text
1 字节 flags | 4 字节无符号大端 payload 长度 | payload 字节
```

`0x00` 表示未压缩消息，`0x01` 位表示压缩。`0x02` 结束帧包含 JSON 元数据及可选
`error` 对象，即使 protobuf 流也使用这种结束格式。进程 `event.end` 是业务事件，
与传输结束帧不同；两者都要检查，不能只根据 HTTP 200 判断流式 RPC 成功。

Unary gzip 使用 `Content-Encoding` / `Accept-Encoding`，Connect 流式 gzip 使用
`Connect-Content-Encoding` / `Connect-Accept-Encoding`，压缩标记针对消息 payload。
不支持的编码和媒体类型会被拒绝，不会当作原始 JSON 解析。

以下示例在调用者当前目录生成未压缩的 Start 请求帧，并保存响应帧供 Connect
解码器读取：

```bash
python3 - <<'PY' > start-request.bin
import json
import struct
import sys

request = {"process": {"cmd": "/bin/sh", "args": ["-c", "printf hello"]}, "stdin": False}
payload = json.dumps(request).encode("utf-8")
sys.stdout.buffer.write(struct.pack(">BI", 0, len(payload)) + payload)
PY
curl -sS --fail -X POST "$ENVD_URL/process.Process/Start" \
  -H "X-Access-Token: $ENVD_ACCESS_TOKEN" \
  -H 'Content-Type: application/connect+json' -H 'Connect-Protocol-Version: 1' \
  --user root: --data-binary @start-request.bin --output start-response.bin
```

解码后的消息包括 `{"event":{"start":{"pid":123}}}` 和
`{"event":{"data":{"stdout":"aGVsbG8="}}}`。PID 和事件分段会变化，不应假定
只有一个输出事件，也不能把 `.bin` 响应当作普通 JSON。日常命令与文件调用优先
使用[模板与 SDK 使用指南](usage_zh.md)中的 SDK 示例。

## 错误与限制

Connect unary 错误使用字符串 code，例如：

```json
{"code":"invalid_argument","message":"cwd '/missing' does not exist"}
```

流式 RPC 错误放在结束帧的 `error` 字段中。常见 code 包括 `invalid_argument`、
`unauthenticated`、`permission_denied`、`not_found`、`already_exists`、
`failed_precondition`、`resource_exhausted`、`unimplemented`、`internal`、
`unknown`、`unavailable`、`canceled`、`deadline_exceeded`。部分进程 I/O 错误
为兼容性使用 `unknown`。message 用于诊断，不应当作稳定标识符解析。

HTTP 文件错误和公共令牌拒绝则使用数值 HTTP code：

```json
{"code":404,"message":"not found"}
```

这种响应的 Content-Type 为 `application/json; charset=utf-8`。令牌中间件可能在
请求到达 RPC handler 前返回该 HTTP 错误。`/init` 错误、查询参数绑定错误和未知
路由可能返回纯文本或空响应体；并非所有 HTTP 响应都使用同一种 JSON 错误结构。

daemon 不另设 RPC 消息或上传文件的总大小上限，但仍受解析器约束与操作系统资源
限制；`/init` 有前述 256 KiB、64 层限制。尚未就绪或发生故障时，普通请求可能返回
503。认证和就绪检查可能先于接口自身的参数验证。调用者还需处理磁盘满、权限
错误，以及响应头发送后的连接中断。

## 协议源码与示例

| 参考 | 用途 |
| --- | --- |
| [Process schema](../proto/process.proto) | 全部 Process 消息与枚举定义。 |
| [Filesystem schema](../proto/filesystem.proto) | 全部 Filesystem 消息与枚举定义。 |
| [HTTP 路由与 handler](../src/transport/rest.rs) | HTTP 响应与 Content-Type 行为。 |
| [认证实现](../src/transport/auth.rs) | 令牌与签名文件处理。 |
| [现有 SDK 验收](../../tests/e2e/sdk_compat/README_zh.md#选定-envd-验收) | 可运行的健康、命令、文件和模板场景。 |
| [文件传输测试](../tests/FILE_TRANSFERS.md) | Range、multipart、签名、compose 与错误覆盖。 |

本文仅覆盖已注册的生产路由和两个生产 protobuf service，测试专用 conformance
service 不属于普通 API。
