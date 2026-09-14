# cube-envd

> **English**: [README.md](./README.md)
>
> 本文件与英文版逐节对应；唯一的例外是[声明差异](#声明差异)表 —— 它是生成物，只渲染进
> 英文版，本文件给出来源与指针。

`cube-envd` 是运行在每个 CubeSandbox 沙箱内部的 E2B 兼容数据面守护进程。它为 CubeSandbox SDK 和 E2B SDK 提供沙箱内运行时能力，包括执行命令、读写文件、操作文件系统、打开 PTY 终端以及初始化创建沙箱时的环境变量。

默认监听 `0.0.0.0:49983`。`GET /health` 在服务就绪后返回 `204 No Content`，因此也适合作为模板就绪探针：

```bash
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health
# => 204
```

## 为什么自研

`envd` 运行在每个沙箱内部，是 E2B SDK 与 CubeSandbox 运行时之间的兼容边界。从
`e2b-dev/infra` 消费它意味着 roadmap、修复节奏与发布计划都归另一个项目所有，而且那个
二进制里带着 CubeSandbox 从不使用的集成路径（Firecracker MMDS、Hyperloop、NFS volume
init）。

`cube-envd` 用一个 CubeSandbox 自有的 Rust 实现替换它，并保持线协议不变：[`proto/`](./proto)
里的协议定义保持 SDK 兼容，而每一处与 Go 基线的有意行为差异都连同理由列在
[声明差异](#声明差异) 一节 —— 那张表由
[`tests/e2e/cube_envd/conformance/declared_differences.toml`](../tests/e2e/cube_envd/conformance/declared_differences.toml)
生成并在 CI 校验，因此"声明"无法与"实测"脱节。上游 Go envd 仍以 `envd-go` 留在基础
镜像里，并可通过 `ENVD_BIN` 在运行期回滚（见 [集成说明](#集成说明)）。

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

#### 进程结束事件

`Start` 与 `Connect` 流都以 `EndEvent` 收尾。其字段形状属于 SDK 契约，管道进程与
PTY 进程完全一致：

| 场景 | `exitCode` | `exited` | `status` | `error` |
|----------|-----------|----------|----------|---------|
| 正常退出，退出码 `N` | `N`（为 `0` 时省略） | `true` | `exit status N` | 省略 |
| 被信号 `N` 终止 | `128 + N` | `false`（省略） | `terminated by signal N` | `terminated by signal N` |
| 回收失败 | `-1` | `false`（省略） | `failed to reap process` | 错误文本 |

说明：

- 遵循 shell 约定：被信号 `N` 终止的进程上报 `128 + N`，因此 `SIGKILL` 为 `137`、
  `SIGTERM` 为 `143`。参考实现 envd 对信号终止上报 `-1`；本实现把负值保留给回收失败。
- proto3 JSON 会省略零值字段，因此正常退出时没有 `exitCode`、被信号终止时没有
  `exited`。客户端需要依赖 `status`（与 `exited`）区分"正常退出"与"被信号终止"。
- 本仓库三套 SDK 在 `exitCode` 缺省时会从 `status` 解析退出码，因此 `status` 的
  文案属于契约的一部分。

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

启动的进程会先清空环境变量，然后按参考实现 envd 的语义构建基础环境：`PATH` 取自 `cube-envd` 自身，`HOME`、`USER`、`LOGNAME` 取自所选用户的 passwd 条目。随后依次叠加当前 `/init` 环境变量快照与请求中的 `envs`，请求可以覆盖上述任一变量。当所选用户与运行 `cube-envd` 的用户不同时，会通过 `setpriv` 切换凭据。

未指定 `cwd` 时，进程在所选用户的主目录下启动；`cwd` 为相对路径或 `~/...` 时同样基于该主目录解析。该目录必须存在。

## 资源边界与 cgroup 策略

MVP **不做**哪些事，这里显式写出来，免得有人只能从"代码里没有"去反推：

- **不做按命令的 cgroup v2 放置。** 命令不被限制在 leaf cgroup 内，因此 `cube-envd`
  自己不施加任何按命令的内存或 CPU 预算。沙箱自身的上限来自 CubeHypervisor（微虚拟机），
  而不是这里。
- **不做 OOM 归因。** 绝不产生 `EndEvent.oomKilled` / `killedBy: "oom"`；被内核 OOM
  killer 杀掉的进程按普通信号死亡上报（`exitCode: 128 + 9`、
  `status: "terminated by signal 9"`）。
- **超出进程组的逃逸不做额外保证。** 超时（`Connect-Timeout-Ms`）与 `SendSignal` 作用于
  命令的**进程组**，能覆盖命令自己 fork 出的子进程 —— 这一点比 Go 基线更严，基线只对直接
  子进程发信号（见进程结束事件一节）。

它保证什么、以及为什么对基础镜像已经够用：参考实现 envd 只有在 guest 暴露可写 cgroup v2
树时才会施加按命令的 cgroup，而在实际发布的基础镜像里并非如此，基线自己会打印
`falling back to no-op cgroup manager`（已用 `cubesandbox-base` 里的 `/usr/bin/envd-go`
实测），因此两者在该环境跑的是同一套 `noop` 策略。该差异只在 cgroup v2 可写的宿主上才
可观察。

若将来实现按命令的资源隔离，契约沿用基线：分配失败即拒绝 `Start`（`resource_exhausted`），
绝不静默降级为无隔离执行；并且要"上报"当前模式，而不是让调用方去推断。

## 安全模型

`cube-envd` 以**自身凭据**执行请求（在 `cubesandbox-base` 镜像中即 root），所选用户
**不是**授权边界。各机制的准确含义如下：

- **进程执行**以所选用户身份运行：当该用户与运行 `cube-envd` 的用户不同时，子进程经
  `setpriv --reuid --regid --init-groups` 启动，命令自身可访问的范围由内核约束。
  `setpriv` 来自 util-linux，会依次在 `/usr/bin`、`/bin`、`/sbin`、`/usr/sbin` 中查找；
  Alpine 与 busybox 需要额外 `apk add util-linux`，因为它们自带的同名 applet 不接受
  `--reuid`。请求选中的就是守护进程自身用户时完全不使用它。
- **文件系统 RPC 与 `/files` 以 `cube-envd` 自身凭据执行**（标准镜像中即 root）。所选
  用户决定路径基准（相对路径落在其主目录）以及新建文件/目录的属主，但**不限制**可读写
  删除的路径范围。`Stat`、`ListDir`、`Move`、`Remove` 均不限于用户主目录，`GET /files`
  可以流式读取 `cube-envd` 能打开的任何文件。
- **没有按请求的令牌。** `Authorization: Basic` 只用于指定以哪个用户身份执行，不认证
  调用方；沙箱内任意进程都可以声称自己是任意账户。
- **访问控制依赖网络边界。** `cube-envd` 监听 `0.0.0.0:49983`，必须不可被不可信客户端
  直达：沙箱 IP 位于私有网段，`CubeProxy` 是唯一公网入口并在那里校验按沙箱下发的
  traffic token。任何能直连 `49983` 的实体（包括沙箱内的任意进程）实际上拥有
  `cube-envd` 自身的权限。

请求受资源边界保护，单个客户端无法耗尽沙箱：一元 JSON 请求体上限 1 MiB、流式帧上限
16 MiB、`/files` 全流式、并发连接上限 1024 且带请求头读取超时、每进程订阅者队列有界并
淘汰慢订阅者。

## 设计

目录树同时是契约索引与依赖图：一条路径声明"承诺了什么"，一条边声明"谁可以调谁"。
依赖只允许单向流动：

```text
app ──▶ {filesystem, process} ──▶ {connect, wire, rest, cors, compress} ──▶ {auth, paths, init, logging, version, compat}
                   └──────────────▶ generated（谁都可以用，它自己不引用任何东西）
```

```
cube-envd/
├── Cargo.toml              # Rust 包清单
├── Cargo.lock
├── Makefile                # build/install/fmt/lint/test/proto-gen/proto-doc 目标
├── build.rs                # 构建时生成 Rust protobuf 绑定
├── rust-toolchain.toml     # 固定 Rust 工具链（1.89）
├── proto/                  # 线类型所镜像的协议定义
│   ├── process/            # process.Process
│   └── filesystem/         # filesystem.Filesystem
├── src/
│   ├── main.rs             # CLI 入口、HTTP 服务启动、accept 循环
│   ├── lib.rs              # 模块声明与 #![forbid(unsafe_code)]
│   ├── app.rs              # Axum 路由与共享应用状态
│   ├── auth.rs             # Basic 认证与本地用户解析
│   ├── paths.rs            # 以用户主目录为锚点的安全路径解析
│   ├── connect.rs          # Connect 帧、错误模型、请求上限
│   ├── wire.rs             # protobuf JSON <-> 领域模型转换
│   ├── rest.rs             # REST 面的错误体（与 Connect 面区分）
│   ├── cors.rs             # 与基线一致的 CORS 响应头
│   ├── compress.rs         # 缓冲式 JSON 面的 gzip 协商
│   ├── compat.rs           # Go 兼容的错误词表与退出语义
│   ├── init.rs             # /init 环境变量快照状态
│   ├── logging.rs          # JSON 结构化日志初始化
│   ├── version.rs          # 版本常量（唯一事实源）
│   ├── process/            # 进程生命周期、PTY、输入输出流
│   ├── filesystem/         # 文件系统 RPC、文件传输、watcher
│   └── generated/          # 生成的 protobuf Rust 类型（入库）
├── tests/                  # CLI、HTTP、RPC、进程与层规则测试
│   └── layer_rule.rs       # 断言上述依赖方向
└── doc/
    └── cube-envd-api.md    # 生成的协议参考
```

依赖方向由 [`tests/layer_rule.rs`](./tests/layer_rule.rs) 断言，而不是由类型系统保证：
`pub(crate)` 对同级模块一律可见，一个指错方向的 `use` 照样编译通过，`clippy` 与
`rustfmt` 也全绿。门禁是该文件里的一张 `(说明, 归属模块, 禁止引用的模块)` 表 ——
**新增一层时若不在表里加一行，它就不受任何约束**。

> **必须带 `tests` 目标运行。** `cargo test --lib` / `--bins` 会跳过
> `tests/layer_rule.rs`，规则静默失效。请用 `make cube-envd-test`（或裸 `cargo test`），
> 它们包含该文件。仓库里已有先例：`make hypervisor-test` 传 `--lib --bins`，因此那个
> 组件自己的 `tests/integration.rs` 从未被执行。

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

## 集成说明

- **entrypoint 契约。** [`docker/cube-entrypoint.sh`](../docker/cube-entrypoint.sh) 在后台
  启动 `${ENVD_BIN:-/usr/bin/envd} -port ${ENVD_PORT:-49983} ${ENVD_EXTRA_ARGS}`，随后要么
  `exec` 用户 `CMD`，要么等待 envd。它会为兼容 E2B 命令行习惯追加 `-isnotfc`，该参数在这里
  是 no-op（见 [CLI](#cli) 表）。
- **安装路径。** 镜像同时提供两个实现：`/usr/bin/envd`（本 crate，默认）与
  `/usr/bin/envd-go`（固定的上游版本），因此 `ENVD_BIN=/usr/bin/envd-go` 是无需重建镜像的
  运行期回滚。这个路径必须是字面的 `/usr/bin/envd`：Cubelet 靠 exec `envd --version` 收集
  模板的 envd 版本，只改 `ENVD_BIN` 会让模板注解留着**另一个实现**的版本号。按模板选择的
  具体做法见 [`docker/README.md`](../docker/README.md)。
- **keepalive 节奏。** 长时间静默的流式 RPC 会持续发送 `keepalive` 事件，避免代理与负载
  均衡在命令静默运行期间因空闲而断开连接。默认 **90 s**，与 Go 基线一致；`Keepalive-Ping-Interval`
  （请求头，单位秒）可按调用调小。**若你的 LB 空闲超时短于默认值（60 s 是常见取值），请发送
  该请求头**，不要依赖默认节奏。保持与基线一致是刻意的取舍：见 [声明差异](#声明差异) 里的
  对齐项。

## CLI

```
envd [OPTIONS]
```

| 选项 | 默认值 | 说明 |
|--------|---------|-------------|
| `-port`, `--port` | `49983` | HTTP 服务监听端口。 |
| `-isnotfc`, `--isnotfc` | — | 仅为兼容 E2B 命令行习惯而保留。cube-envd 不含 Firecracker MMDS 逻辑（CubeSandbox 使用 Cloud Hypervisor，`169.254.169.254` 不存在），因此它是 no-op：带不带行为完全一致。 |
| `-version`, `--version` | — | 输出版本并退出。 |
| `-commit`, `--commit` | — | 输出构建提交哈希并退出。 |

单横线旧式参数（`-port`、`-isnotfc`、`-version`、`-commit`）会被规范化为双横线形式以兼容调用。

### CLI 兼容矩阵

`cube-envd` **刻意比 Go 基线更严格**：Go 的 `flag` 包遇到第一个非 flag 参数就停止解析并
静默忽略它，接受越界的 `-port`（只到 bind 时才失败），也接受 `-isnotfc=false`。于是
`ENVD_EXTRA_ARGS` 里一个笔误就可能让守护进程带着默认值启动。基线**接受**的输入这里一律
接受；基线**静默忽略**的输入在这里是用法错误（退出码 2，stderr 输出错误信息与用法）。

| 输入 | Go envd 0.5.13 | cube-envd | 说明 |
|---|---|---|---|
| `-version` / `--version` | 输出版本，退出 0 | 相同 | 取值按设计不同（见版本号） |
| `-commit` / `--commit` | 输出提交哈希，退出 0 | 相同 | |
| `-port N` / `-port=N` / `--port N` | 接受（`int64`；越界只在 bind 时失败） | 接受并按 `u16` 校验；`--port=99999` → 退出 2 | 有意为之 |
| `-isnotfc` | 接受（关闭 Firecracker 分支） | 接受，no-op | cube-envd 没有 FC 代码 |
| `-isnotfc=false` | 接受 | **拒绝**（退出 2） | 有意为之 |
| 位置参数（含 `--` 之后） | 忽略，守护进程照常启动 | **拒绝**（退出 2） | 有意为之 |
| 未定义 flag | `flag provided but not defined` + 用法，退出 2 | `unexpected argument` + 用法，退出 2 | 文案不同，状态一致 |
| `-h` / `--help` | 用法，退出 0 | 用法，退出 0 | |

该表由 [`tests/cli.rs`](./tests/cli.rs) 验证（含 `rejects_the_arguments_the_baseline_silently_ignores`），
而不是由线协议对照套件验证：对照套件走 HTTP，而 CLI 只存在于 socket 绑定之前。

### 版本号

`-version` 输出 [`src/version.rs`](./src/version.rs) 中的 semver 常量，它是本组件版本的
**唯一事实源**，形态与参考实现 envd 的 `packages/envd/pkg/version.go` 一致。该值**有意**
不从 git tag 或 CI 运行派生，因为下游会解析它：

- Cubelet 与 CubeMaster 按 `\d+\.\d+\.\d+` 提取该值，写入
  `cube.master.components.envd.version` 注解，并作为沙箱信息上的公开字段 `envdVersion` 暴露；
- 参考实现 envd 还会用该值做最低版本门禁比较，且把非法格式判为 error 而非"更旧"，因此
  `sha-1a2b3c4` 这类构建标识在这里不是"没用"而是**有害**。

构建标识走单独的 `CUBE_ENVD_COMMIT`，由 `-commit` 输出。`CUBE_ENVD_VERSION` 仍作为发布
工具链的显式覆盖保留，但默认没有任何渠道注入它，空值会回落到常量。发版只需 bump 该常量；
`make version-check` 会断言它是 semver 且二进制自报版本与之一致。

手动启动示例：

```bash
/usr/bin/envd -port 49983 -isnotfc >/var/log/envd.log 2>&1 &
```

## 开发说明

### Rust 工具链

仓库在 `rust-toolchain.toml` 中固定使用 Rust `1.89`，并包含 `x86_64-unknown-linux-musl` 和 `aarch64-unknown-linux-musl` 目标。

### 日志

通过 `RUST_LOG` 控制日志过滤级别（默认：`info`）。日志以结构化 JSON 输出。

### 阻塞策略

守护进程运行单个 Tokio 多线程 runtime，配 **2 个 worker 线程**与 **64 线程的 blocking pool**
（`src/main.rs::build_runtime`）。这两个数字是刻意的 —— 本进程是"每沙箱一个"而不是"每主机
一个"：

- 文件系统 RPC 走 `tokio::fs`，即**每次系统调用**一次 blocking-pool 穿越；少数必须持有裸 fd
  或调用 libc 的路径（属主变更、用户查询、PTY 建立/回收）直接用 `spawn_blocking`；
- 默认的 512 线程 blocking pool 在满载时意味着数 MB 的线程栈，对一个 guest 守护进程来说
  完全不成比例。

实测（`tests/blocking_strategy.rs`，手工运行、`--ignored`、本机）：
`tokio::fs::metadata` 中位 **34.3 µs/次**，而 `spawn_blocking` + 同步 `std::fs` 中位
**24.2 µs/次**（每次调用便宜约 30%，因为它按请求付一次池穿越，而不是按系统调用付）。

**重评条件。** 若文件系统面显著变重，或 RSS 实测表明这个池确实成了瓶颈，预期的替代方案是
基线的形态：每请求一次 blocking 穿越、内部用同步 `std::fs`、外加一个在途计数器。改动前后都
要重跑上面的测量 —— 决定权在数字，不在这一段文字。无论选哪条，池都保持有界：无界 blocking
被否决，因为它会按沙箱数量成倍放大。

### 测试

```bash
make test
```

测试覆盖 CLI 兼容性、健康检查、Connect 帧、进程启动/PTY/输入/信号处理、文件系统 RPC、文件上传、目录监听、认证/路径解析以及优雅关闭。

## 相关文档

- [自定义模板镜像](../docs/zh/guide/tutorials/bring-your-own-image.md)
- [模板概览](../docs/zh/guide/templates.md)
- [协议文档](./doc/cube-envd-api.md)
- [一致性对照套件](../tests/e2e/cube_envd/conformance/README.md) —— Go 基线对比如何采集、
  归一化并设门禁；也是[声明差异](#声明差异)表的数据来源
- [docker/README.md](../docker/README.md) —— 基础镜像、两个 envd 实现与 `ENVD_BIN` 回滚

## License

Apache-2.0 — 详见 [LICENSE](../LICENSE)。

## 声明差异

完整的差异清单（含理由与实测结果）由
[`tests/e2e/cube_envd/conformance/conformance.py`](../tests/e2e/cube_envd/conformance/conformance.py)
渲染进**英文版** [`README.md`](./README.md#declared-differences) 的 `## Declared differences`
一节，并由同一脚本的 `check-docs` 在 CI 中校验其与
[`declared_differences.toml`](../tests/e2e/cube_envd/conformance/declared_differences.toml) 一致。

本文不再复制那张表：它是生成物（文件内标注了"请勿手改"），手工副本会静默过期。需要逐条
依据时请读英文版那一节，或直接读 `declared_differences.toml`。
