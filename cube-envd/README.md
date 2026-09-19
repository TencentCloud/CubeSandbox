# cube-envd

`cube-envd` 是运行在 CubeSandbox 工作负载容器内、与 envd 协议兼容的 Rust 数据面实现。
默认监听 `0.0.0.0:49983`。

## 构建

在仓库根目录执行：

```bash
make cube-envd
```

或在本目录执行：

```bash
cargo build --release
cargo test --release
```

发布二进制位于 `target/release/cube-envd`。

## 命令行参数

```text
--port <PORT>       监听端口；默认：49983
--isnotfc           启用非 FC 容器模式
--version           打印所模拟的上游 envd 世代（`0.5.13`）
--commit            打印内嵌的 Git 短 commit
```

为兼容基础镜像的入口脚本，也接受旧式单横线写法
（`-port`、`-isnotfc`、`-version`、`-commit`）。

> `-version` 上报的是 cube-envd **所对标的上游 envd 世代**（`0.5.13`），而不是 crate 版本。
> E2B 客户端会按沙箱上报的版本启用特性（递归 watch、命令 stdin、默认用户、closeStdin、
> octet-stream 上传、文件元数据、watch `includeEntry`、网络挂载），Cubelet 也把
> `envd --version` 的输出记录为模板的 `envdVersion`。因此上报 crate 版本 `0.1.0` 会让这些
> 门槛一律判为“过旧”。真实实现版本（`CARGO_PKG_VERSION`）出现在启动日志里，构建短 sha 由 `-commit` 给出。

## 接口

如下标注的数据面接口使用 Connect 分帧：

| 方法 | 接口 | 用途 |
| --- | --- | --- |
| GET | `/health` | 就绪探针；返回 `204` |
| POST | `/process.Process/Start` | 执行非 PTY 命令；Connect 流 |
| POST | `/process.Process/Connect` | 重新连接 PTY；Connect 流 |
| POST | `/process.Process/SendSignal` | 向 PTY 进程发送信号 |
| POST | `/process.Process/SendInput` | 向 PTY/stdin 发送输入 |
| POST | `/process.Process/StreamInput` | 流式写入 stdin/PTY 输入 |
| POST | `/process.Process/CloseStdin` | 关闭 stdin 以投递 EOF |
| POST | `/process.Process/Update` | 调整 PTY 尺寸 |
| POST | `/filesystem.Filesystem/ListDir` | 列出目录项 |
| POST | `/filesystem.Filesystem/Stat` | 读取文件元数据 |
| POST | `/filesystem.Filesystem/Remove` | 删除文件或目录 |
| POST | `/filesystem.Filesystem/Move` | 重命名或移动条目 |
| POST | `/filesystem.Filesystem/MakeDir` | 创建目录 |
| POST | `/filesystem.Filesystem/WatchDir` | 流式推送文件系统事件 |
| GET | `/files?path=...` | 读取文件字节；支持 Range 与条件 GET |
| POST | `/files?path=...` | 写入原始或多段（multipart）文件字节 |
| POST | `/files/compose` | 返回 `501`（未实现，显式而非 404） |

`/files` 响应包含 CORS 头、`Accept-Ranges` 与 `Last-Modified`。
单段字节范围返回 `206`；非法或无法满足的范围返回 `416`；
`If-Modified-Since` 命中时返回 `304`。
仅提供 identity 编码：当 `Accept-Encoding` 明确不接受 identity 时返回 `406`。
Connect 二进制 protobuf 编解码统一返回 `501`（仅实现 JSON 编解码）。
`Start` 请求的 `stdin` 缺省为 `true`，可通过 `StreamInput`/`CloseStdin` 写入并关闭。

进程与 PTY 的结束事件保留上游的 `exitCode`、`status`、`error` 字段，
并在进程结束时额外携带 `termination` 对象：

```json
{
  "reason": "signal",
  "signal": 11,
  "signalName": "SIGSEGV",
  "coreDumped": true
}
```

`reason` 取值为 `exited`、`signal`、`timeout`、`oom` 或 `unknown`。
在 Linux cgroup v2 下，当进程 cgroup 的 `memory.events` 中 `oom_kill`
计数增加时判定为 `oom`；在 cgroup v1 下，envd 使用内存 cgroup 的
`memory.oom_control` 中 `oom_kill` 计数，并以 `memory.failcnt` 作为尽力而为的兜底。

## 本地冒烟

基础镜像冒烟会构建 Rust 实现、启动容器，并校验就绪与版本契约：

```bash
make smoke-cube-base-image CUBE_BASE_PLATFORM=linux/amd64
```

如需验证回滚到上游实现：

```bash
make smoke-cube-base-image CUBE_BASE_PLATFORM=linux/amd64 ENVD_IMPL=upstream-e2b
```

模板到沙箱的验证说明与可运行的 Go 验证器见
[`docs/TEMPLATE_VALIDATION.md`](docs/TEMPLATE_VALIDATION.md)。
