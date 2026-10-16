---
title: 分布式链路追踪
lang: zh-CN
---

# 分布式链路追踪

日志和指标能告诉你某次沙箱操作**慢或失败了**，却很少告诉你**卡在哪一步**。分布式链路追踪为每次操作记录一条 trace，经过的每个组件、每个已插桩的阶段各有一个 span。

创建沙箱、从镜像构建模板、暂停与恢复沙箱都要依次经过多个组件，追踪能展示时间花在这些阶段的哪些地方。

## 被追踪的工作流

沙箱生命周期操作已实现端到端追踪：

| 工作流 | 入口 | trace 展示的内容 |
|---|---|---|
| 创建沙箱（含从快照创建） | `POST /sandboxes` | 入队与调度、节点选择、创建容器，恢复场景下还有快照准备与 VM 恢复 |
| 镜像转模板 | `POST /templates` | 镜像构建、rootfs/ext4、产物发布、分发 |
| 沙箱转模板（commit） | `POST /cube/sandbox/commit` | 横跨 CubeMaster 与 Cubelet 的大致阶段：Master 上的提交、运行、快照与注册，Cubelet 上的抓取与发布 |
| 创建快照 | `POST /sandboxes/:sandboxID/snapshots` | 准备、抓取、注册 |
| 回滚 | `POST /sandboxes/:sandboxID/rollback` | 准备、恢复、注册 |
| 暂停 / 恢复 | `POST /sandboxes/:sandboxID/pause`、`.../resume` | 准备、抓取或运行时、收尾 |

Span 来自五个上报服务：

| 服务 | 在 trace 中的角色 |
|---|---|
| `cube-api` | 经 CubeAPI 发起的请求的 HTTP 入口 span |
| `cubemaster` | 编排：调度、派发、异步任务生命周期 |
| `cubetemplatecenter` | 模板镜像构建与产物处理 |
| `cubelet` | 单节点运行时工作流与生命周期锁 |
| `cube-shim` | 单沙箱 VMM 启动、引导或恢复、容器启动 |

containerd 插桩还会记录 Task、RPC 和传输等 span，并使用调用方服务的身份上报。

有几点边界值得了解：

- 绕过 CubeAPI 时，trace 可以从 CubeMaster 开始；此时入口 span 是 CubeMaster 自己的 HTTP `POST /cube/...` span，未必带有具名的 `cubemaster.*` 业务名称。
- 传播协议是 W3C Trace Context。未传播上下文的 SDK 发来的请求仍会在服务端被追踪——你拿到的是服务端 trace，只是缺少客户端自身的 span。
- 克隆等由 SDK 组合的多种操作不会合并成同一条 trace，客户机 agent 与 hypervisor 也没有被直接插桩；它们的工作体现为包裹它们的 `cube-shim` span 的耗时。

## 启用追踪

追踪默认关闭。把各组件各自的 `OTEL_EXPORTER_OTLP_ENDPOINT` 指向外部自备的 OTLP/**gRPC** 接收端——OpenTelemetry Collector 或 Tempo 的 OTLP/gRPC 接收端——取值为 `http://<receiver-host>:<OTLP-gRPC-port>`。

接收端和查询后端需要单独部署。各组件以各自的服务名上报（`cube-api`、`cubemaster`、`cubetemplatecenter`、`cubelet`、`cube-shim`），因此不要设置共享的 `OTEL_SERVICE_NAME`。

配置走环境变量，随安装方式而异：

- **裸金属 / one-click**：安装前设置该变量，参见[裸金属部署](./bare-metal-deploy.md)中的「OpenTelemetry 链路追踪（可选）」。
- **Kubernetes**：加到各组件的 `env` 数组中，参见 [Helm 安装 · 链路追踪配置](./kubernetes/install.md)中的「8.4 链路追踪配置」。

Cubelet 为每个沙箱启动一个全新的 CubeShim 进程；该进程在启动时继承 Cubelet 的环境变量，因此已在运行的 shim 会保留旧设置。

## 查询与解读耗时

可以按 trace ID 查询，也可以用接口返回的某个 ID。响应里的标识与 span 属性并不总是同一个字符串，而且它们都不是 trace ID：

| 你手上的标识 | span 属性 |
|---|---|
| 创建沙箱响应中的 `clientID`（该次创建的业务请求 ID） | `cube.request_id`（仅创建场景） |
| 沙箱 ID | `cube.sandbox_id` |
| 模板 ID | `cube.template_id` |
| 快照 ID | `cube.snapshot_id` |
| 异步任务 ID（`jobID` / `job_id`） | `cube.job_id` |

只有创建响应是这样映射的：详情或列表响应里的 `clientID` 可能是宿主机 ID，不要一概而论。HTTP 请求头 `x-request-id` 是另一回事——它对应 CubeAPI 入口 span 上的 `cube.http_request_id`。

属性只需存在于**部分** span 上就能找到整条 trace：只要任一个 span 匹配，整条 trace 就会被返回。下面示例使用 Tempo 的 TraceQL，其他后端有各自的查询语言。

```traceql
{ span.cube.request_id = "<creation-client-id>" }
```

```traceql
{ span.cube.sandbox_id = "<sandbox-id>" }
```

```traceql
{ span.cube.job_id = "<job-id>" }
```

同一个沙箱 ID 可能对应保留期内的多次操作，可按操作发生时间缩小查询范围。

查不到时，先确认查询时间范围覆盖了操作时间。失败请求可能尚未生成沙箱或快照 ID，这时可用已有的 `cube.request_id` 查询。

拿到 trace 后，按以下规则解读耗时：

- 父 span 的耗时一般**包含**其嵌套工作，因此不要在它之上再累加嵌套耗时。脱离请求生命周期的异步子 span 可能超出父 span；父子重叠属于正常的嵌套——只有时间上重叠的兄弟任务才提示存在并行。
- 子 span 的耗时之和不一定等于父 span 的耗时；阶段间的空隙可能包含等待或尚未单独埋点的工作。
- 异步模板任务的存活时间超过提交它的 API span。提交 span 只度量提交动作，构建与运行的 span 记录后台执行，但整个「提交到就绪」的任务未必是单个 span——是否成功要以任务状态为准，不能只看 span。
- 业务失败仍可能返回 HTTP 200。要一并检查 span 状态与业务属性（`cube.ret_code`）以及 HTTP 状态码，而不是只相信后者。

追踪回答的是「卡在哪一步」，不是「为什么」。当某个偏慢或失败的 span 指向某个组件的某个阶段后，原因通常就在该组件的日志里——参见 [沙箱日志](./sandbox-logs.md) 与 [服务管理与日志](./service-management.md)。
