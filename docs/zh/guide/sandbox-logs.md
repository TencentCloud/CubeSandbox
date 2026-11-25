---
title: 沙箱日志
lang: zh-CN
---

# 沙箱日志

::: warning 功能迭代中
日志能力目前仍在持续迭代，本文介绍的 `cubecli logs` 命令是**临时使用版本**——后续版本可能对接口及底层存储结构进行调整。
:::

CubeSandbox 提供三个互补的日志层：

| 层级 | 记录内容 | 获取方式 |
|---|---|---|
| **沙箱日志** | 容器 init 进程（主入口）的 stdout/stderr | `cubecli logs`（本文） |
| **沙箱事件** | CubeShim 请求日志中属于该沙箱的行：生命周期与转发输出 | [事件 API](#事件-api)（本文） |
| **`envd` 任务日志** | 在沙箱内通过 `exec` 接口启动的子任务的 stdout/stderr | E2B SDK（`on_stdout` / `on_stderr` 回调） |

本文介绍**沙箱日志**和**沙箱事件**。`envd` 子任务的日志获取方式请参阅 [E2B SDK 文档](https://e2b.dev/docs)。

## 事件 API

沙箱事件来自 CubeShim 请求日志，而非 `cubecli logs` 读取的 stdout/stderr 文件。与 `cubecli` 不同，事件可在任意部署形态（单机、多机、K8s）下通过 API 远程获取。CubeAPI 经 CubeMaster 将请求转发到沙箱所在节点的 Cubelet：

```bash
# 最新事件（轮询 / tail 模式）
curl "/cubeapi/v1/v2/sandboxes/<sandbox-id>/logs?tail=true"

# 从头分页，每页 200 条
curl "/cubeapi/v1/v2/sandboxes/<sandbox-id>/logs?limit=200"

# 下一页：回传上一次响应的 nextCursor 和 nextCursorSeq
curl "/cubeapi/v1/v2/sandboxes/<sandbox-id>/logs?cursor=<nextCursor>&cursorSeq=<nextCursorSeq>&limit=200"
```

参数：

- `limit`（默认 1000，最大 2000）为每页条数的硬上限。
- `cursor` 与 `cursorSeq`（即上次的 `nextCursor`、`nextCursorSeq`）应成对传入，仅返回该位置之后的条目，不重不漏。`cursorSeq` 可为 `0`，请始终传入。只传 `cursorSeq` 不传 `cursor` 会返回 400。
- 仅传 `cursor` 仍被接受，用于兼容旧调用。毫秒时间戳无法区分同一毫秒内的事件，因此该形式会跳过与游标同毫秒的全部事件；需要完整性时请使用成对形式。
- `tail` 返回最新的 `limit` 条而非分页，与 `cursor`、`cursorSeq` 互斥。其 `nextCursor`/`nextCursorSeq` 可回传以读取之后新增的事件。
- `direction` 会被接受并忽略。

响应包含 `logs`、`nextCursor`、`nextCursorSeq` 和 `hasMore`。

::: warning 限制
- 游标仅在当前生效的 shim 日志文件内有效，不跨日志轮转（每小时一次）。
- `tail` 最多回看日志末尾 16 MiB。若最新事件所在毫秒恰好跨越该边界，`nextCursorSeq` 会偏小，后续读取可能重复返回少量事件，但不会漏。
- 位置由日志时间戳推算，时钟回拨期间写入的事件可能被跳过。
- 内部端点（`/cube/sandbox/logs`、`/api/v1/sdk/sandboxes/{id}/logs`）同样接受仅传 `cursor`，语义相同：跳过整个毫秒。`direction` 仅本 API 支持，内部端点不支持。
:::

::: warning 本功能的升级顺序
请先升级 Cubelet，再升级 CubeMaster，最后升级 CubeAPI 和 CubeOps（WebUI 后端）。

- 如果 CubeMaster 先于沙箱所在节点的 Cubelet 升级，事件请求会失败，提示 "upgrade cubelet on the owning node first"。
- 如果 CubeAPI 或 CubeOps 先于 CubeMaster 升级，新参数会被忽略：`tail=true` 返回最旧的一页而非最新；复合游标退化为仅 `cursor`，跳过与游标同毫秒的事件。在 Kubernetes 上，旧 CubeMaster 没有本地 shim 日志，请求会失败或返回空。
- 如果集群中 Cubelet 版本不一致，只有已升级节点上的沙箱能获取事件。
:::

## 前置条件

`cubecli` 随 Cubelet 一同构建，并在一键部署时自动安装。`logs` 子命令访问的日志文件位于 **Cubelet 挂载命名空间**内，因此必须**直接在计算节点上执行**——`cubecli logs` 无法通过 API 或非节点主机远程调用。如需远程获取沙箱事件，请使用[事件 API](#事件-api)。

## 读取沙箱日志

```bash
# 最后 100 行 stdout（默认）
cubecli logs <sandbox-id>

# 最后 100 行 stderr
cubecli logs --stderr <sandbox-id>

# 完整日志（全部行）
cubecli logs --all <sandbox-id>

# 最后 N 行
cubecli logs --tail 50 <sandbox-id>
# 简写
cubecli logs -t 50 <sandbox-id>

# 前 N 行
cubecli logs --head 20 <sandbox-id>
# 简写
cubecli logs -H 20 <sandbox-id>
```

### 参数说明

| 参数 | 简写 | 说明 |
|---|---|---|
| `--stderr` | `-e` | 读取 stderr，默认读取 stdout |
| `--all` | `-a` | 输出全部行；不可与 `--tail` 或 `--head` 同时使用 |
| `--tail N` | `-t N` | 输出最后 N 行（未指定其他标志时默认为 100） |
| `--head N` | `-H N` | 输出前 N 行 |

## 读取模板构建日志

模板构建过程中，容器的 stdout/stderr 会写入宿主机文件系统 `/data/log/template/<templateID>_0/`。这些文件无需进入 Cubelet 挂载命名空间，使用 `--tpl` 标志可跳过命名空间切换：

```bash
# 最后 100 行模板构建 stdout
cubecli logs --tpl <template-id>

# 完整模板构建 stderr
cubecli logs --tpl --all --stderr <template-id>
```

## 日志文件路径

| 场景 | 路径 |
|---|---|
| 沙箱 stdout | `/data/cubelet/state/io.containerd.runtime.v2.task/default/<sandbox-id>/stdout`（Cubelet 挂载命名空间内） |
| 沙箱 stderr | `/data/cubelet/state/io.containerd.runtime.v2.task/default/<sandbox-id>/stderr`（Cubelet 挂载命名空间内） |
| 模板 stdout | `/data/log/template/<template-id>_0/stdout`（宿主机文件系统） |
| 模板 stderr | `/data/log/template/<template-id>_0/stderr`（宿主机文件系统） |

::: tip 为什么需要挂载命名空间？
沙箱日志文件由 CubeShim 写入 bundle 目录，该目录仅在 Cubelet 的私有挂载命名空间内可见。`cubecli logs` 会在读取前自动重新进入该命名空间——你无需任何额外操作，只需在节点上直接运行该命令即可。
:::

## 范围与限制

- 这些日志仅记录 **init 进程**（容器内 PID 1）的输出。通过 `exec` 接口启动的进程输出需通过 E2B SDK 的 `on_stdout` / `on_stderr` 回调获取，详见 [E2B SDK 文档](https://e2b.dev/docs)。
- 日志转发需要 v0.4.0 及以上版本的 CubeShim。在更旧的部署上，日志文件将不存在。
- 日志目前不支持实时流式读取，暂无 `--follow` 标志。如需查看最新输出，请重新执行命令。
- 沙箱删除后，对应的日志文件会一并清除。

## 相关文档

- [服务管理与日志](./service-management.md) — 宿主机服务日志、journalctl 及诊断包
- [环境变量](./environment-variables.md)
