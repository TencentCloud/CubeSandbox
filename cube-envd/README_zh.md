# cube-envd

> **English**: [README.md](./README.md)

`cube-envd` 是运行在每个 CubeSandbox 沙箱内部的 E2B 兼容数据面守护进程。它为 CubeSandbox SDK 和 E2B SDK 提供沙箱内运行时能力，包括执行命令、读写文件、操作文件系统、打开 PTY 终端以及初始化创建沙箱时的环境变量。

默认监听 `0.0.0.0:49983`。`GET /health` 在服务就绪后返回 `204 No Content`，因此也适合作为模板就绪探针：

```bash
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health
# => 204
```

## 在系统中的角色

```
用户 SDK / E2B SDK
        │  通过 CubeProxy / 沙箱数据面直连的 HTTPS
        ▼
   容器端口 49983
        │
        ▼
   cube-envd（本组件，运行在沙箱内）
        │  ┌───────────────────────┐
        ├──│ 进程执行              │  命令、PTY、信号、标准输入输出
        │  └───────────────────────┘
        │  ┌───────────────────────┐
        ├──│ 文件 / 文件系统 I/O    │  上传、下载、stat、watch、mkdir 等
        │  └───────────────────────┘
        │  ┌───────────────────────┐
        └──│ 环境变量快照          │  /init 创建时环境变量
           └───────────────────────┘
```

`cube-envd` 通常安装在 `cubesandbox-base` 镜像的 `/usr/bin/envd`，并由 [`docker/cube-entrypoint.sh`](../docker/cube-entrypoint.sh) 启动。也可以通过 `cubemastercli tpl create-from-image --enable-inject-envd` 注入到自定义模板中。

## API

`cube-envd` 在 `49983` 端口提供一个小型 HTTP API。大多数 RPC 方法使用 [Connect 协议](https://connectrpc.com/) 和 protobuf JSON 负载；协议定义位于 [`proto/`](./proto)，生成的接口参考文档位于 [`doc/cube-envd-api.md`](./doc/cube-envd-api.md)。

### 健康检查

| 方法 | 路径 | 说明 |
|--------|------|-------------|
| `GET` | `/health` | 当 `cube-envd` 可以接收 SDK/数据面请求时返回 `204`。 |

### 环境变量

| 方法 | 路径 | 说明 |
|--------|------|-------------|
| `POST` | `/init` | 原子替换默认环境变量快照。请求体：`{"envVars": {"KEY": "value"}}`。 |
| `GET` | `/envs` | 以 JSON 返回当前环境变量快照。 |

### 文件

| 方法 | 路径 | 说明 |
|--------|------|-------------|
| `GET` | `/files?path=...&username=...` | 流式读取磁盘上的普通文件。 |
| `POST` | `/files?path=...&username=...` | 使用 `application/octet-stream` 或 `multipart/form-data` 上传文件。写入采用原子替换（临时文件 + rename）。 |

### 进程 RPC

以下端点实现 `process.Process` 服务：

| 端点 | 类型 | 说明 |
|----------|------|-------------|
| `/process.Process/Start` | streaming | 启动命令或 PTY，并流式返回输出和退出事件。 |
| `/process.Process/List` | unary | 列出由 `cube-envd` 管理的存活进程。 |
| `/process.Process/Connect` | streaming | 订阅存活进程，或按 PID / 标签回放刚结束的进程。 |
| `/process.Process/Update` | unary | 调整 PTY 终端尺寸。 |
| `/process.Process/StreamInput` | streaming | 多帧客户端输入流，写入到已选择的进程。 |
| `/process.Process/SendInput` | unary | 写入一段 stdin 或 PTY 输入。 |
| `/process.Process/SendSignal` | unary | 向进程组发送 `SIGNAL_SIGTERM` 或 `SIGNAL_SIGKILL`。 |
| `/process.Process/CloseStdin` | unary | 关闭普通进程 stdin（EOF）；不适用于 PTY 进程。 |

### 文件系统 RPC

以下端点实现 `filesystem.Filesystem` 服务：

| 端点 | 类型 | 说明 |
|----------|------|-------------|
| `/filesystem.Filesystem/Stat` | unary | 返回文件/目录/符号链接的元数据。 |
| `/filesystem.Filesystem/MakeDir` | unary | 创建目录及其缺失的父目录。 |
| `/filesystem.Filesystem/Move` | unary | 重命名/移动文件或目录。 |
| `/filesystem.Filesystem/ListDir` | unary | 列出目录，可指定递归深度。 |
| `/filesystem.Filesystem/Remove` | unary | 删除文件，或递归删除目录。 |
| `/filesystem.Filesystem/WatchDir` | streaming | 监听目录，并流式返回 create/write/remove/rename/chmod 事件。 |
| `/filesystem.Filesystem/CreateWatcher` | unary | **未实现** — 返回 unimplemented RPC 错误。 |
| `/filesystem.Filesystem/GetWatcherEvents` | unary | **未实现** — 返回 unimplemented RPC 错误。 |
| `/filesystem.Filesystem/RemoveWatcher` | unary | **未实现** — 返回 unimplemented RPC 错误。 |

### 协议说明

- 一元 RPC 需要 `Content-Type: application/json` 和 `Connect-Protocol-Version: 1`；JSON 消息直接放在 HTTP body 中。
- 流式 RPC 需要 `Content-Type: application/connect+json` 和 `Connect-Protocol-Version: 1`。
- 流式 Connect 帧格式为：1 字节标志头 + 4 字节大端长度 + JSON 负载。结束流标志为 `0x02`。
- 流式单帧最大 16 MiB；一元 JSON body 最大 1 MiB。
- `Connect-Timeout-Ms` 可用于设置可选的进程超时。
- `Keepalive-Ping-Interval` 用于控制空闲流式 RPC 的服务端保活帧。

## 用户与路径解析

- 对于 RPC 端点，Basic `Authorization` 请求头中的用户名用于选择执行操作的本地 Unix 用户。如果请求头缺失，默认使用 `root`。Basic 头中的密码部分会被忽略。
- 对于 `/files`，可以通过 `username` 查询参数选择本地用户（默认：`root`）。
- 相对路径和 `~/...` 路径会基于所选用户的主目录解析。绝对路径直接使用。`~otheruser/...` 会被拒绝。

启动的进程会先清空环境变量，然后合并当前 `/init` 环境变量快照与请求中的 `envs`。当所选用户与运行 `cube-envd` 的用户不同时，会通过 `setpriv` 切换凭据。

## 仓库结构

```
cube-envd/
├── Cargo.toml              # Rust 包清单
├── Cargo.lock
├── Makefile                # build/install/fmt/lint/test/proto-doc 目标
├── build.rs                # 构建时生成 Rust protobuf 绑定
├── rust-toolchain.toml     # 固定 Rust 工具链（1.89）
├── proto/
│   ├── process/            # process.Process protobuf 定义
│   └── filesystem/         # filesystem.Filesystem protobuf 定义
├── src/
│   ├── main.rs             # CLI 入口和 HTTP 服务启动
│   ├── app.rs              # Axum 路由与共享应用状态
│   ├── auth.rs             # Basic 认证与本地用户解析
│   ├── paths.rs            # 安全路径解析
│   ├── connect.rs          # Connect 协议帧与错误处理
│   ├── wire.rs             # protobuf JSON / 领域模型转换
│   ├── logging.rs          # JSON 结构化日志初始化
│   ├── process/            # 进程生命周期、PTY、输入输出流
│   ├── filesystem/         # 文件系统 RPC 与文件传输
│   └── generated/          # 生成的 protobuf Rust 类型
├── tests/                  # CLI、HTTP、RPC、进程集成测试
└── doc/
    └── cube-envd-api.md    # 生成的协议参考
```

## 构建

`cube-envd` 是一个 Rust 二进制，编译为静态 musl release。

### 在本目录构建

```bash
# 构建静态 release
make build

# 运行测试
make test

# 格式检查 / lint
make fmt
make lint

# 安装到自定义目录
make install BINDIR=/path/to/bin

# 重新生成 doc/cube-envd-api.md（需要 protoc-gen-doc）
make proto-doc
```

### 在仓库根目录构建

```bash
make cube-envd
```

这会在 CubeSandbox builder 容器内构建静态 `cube-envd`，并安装到 `_output/bin/cube-envd`。

### Base 镜像

`cubesandbox-base` 镜像由 [`docker/Dockerfile.cube-base`](../docker/Dockerfile.cube-base) 构建；该 Dockerfile 会编译本 crate，并将生成的二进制安装为 `/usr/bin/envd`。

## CLI

```
envd [OPTIONS]
```

| 选项 | 默认值 | 说明 |
|--------|---------|-------------|
| `-port`, `--port` | `49983` | HTTP 服务监听端口。 |
| `-isnotfc`, `--isnotfc` | — | 保留用于 Firecracker 兼容。在 CubeSandbox 中用于跳过 Firecracker MMDS 查找（`169.254.169.254`）；手动启动时应使用。 |
| `-version`, `--version` | — | 输出版本并退出。 |
| `-commit`, `--commit` | — | 输出构建提交哈希并退出。 |

单横线旧式参数（`-port`、`-isnotfc`、`-version`、`-commit`）会被规范化为双横线形式以兼容调用。

手动启动示例：

```bash
/usr/bin/envd -port 49983 -isnotfc >/var/log/envd.log 2>&1 &
```

## 开发说明

### Rust 工具链

仓库在 `rust-toolchain.toml` 中固定使用 Rust `1.89`，并包含 `x86_64-unknown-linux-musl` 和 `aarch64-unknown-linux-musl` 目标。

### 日志

通过 `RUST_LOG` 控制日志过滤级别（默认：`info`）。日志以结构化 JSON 输出。

### 测试

```bash
make test
```

测试覆盖 CLI 兼容性、健康检查、Connect 帧、进程启动/PTY/输入/信号处理、文件系统 RPC、文件上传、目录监听、认证/路径解析以及优雅关闭。

## 相关文档

- [自定义模板镜像](../docs/zh/guide/tutorials/bring-your-own-image.md)
- [模板概览](../docs/zh/guide/templates.md)
- [协议文档](./doc/cube-envd-api.md)

## License

Apache-2.0 — 详见 [LICENSE](../LICENSE)。
