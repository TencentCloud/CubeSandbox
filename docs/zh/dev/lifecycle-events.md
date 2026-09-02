# CubeAPI 生命周期事件规范

> 适用范围：CubeAPI Handler 产生的结构化 `LogEvent`、日志包装器及后端。本文描述当前仓库实现；HTTP Webhook 与 OTLP 后端尚未实现。
>
> 关联 issue：[#642 — CubeSandbox Webhook 事件通知](https://github.com/TencentCloud/CubeSandbox/issues/642)

## 1. 概述

CubeAPI 使用结构化 `LogEvent` 记录 API 遥测和资源生命周期结果。Handler 产生事件，`FilteredLogger` 过滤级别，`MultiLogger` 分发事件，已注册的后端负责消费。

生命周期事件是操作结果的观测记录，不是沙箱内部 hook，也不是 REST 响应的一部分。产生事件不会在沙箱中执行用户代码，也不得改变 API 操作结果。

**核心原则**：

- 所有后端复用同一套 `LogEvent` envelope，不为文件、Webhook 或 OTLP 分别定义事件结构。
- 成功事件只在业务操作成功后产生；快照操作还必须通过同步终态 `READY` 校验。
- Handler 只产生事件，不实现 HTTP 投递、签名或重试。
- 新增字段须保持向后兼容；消费者应忽略未知字段并容忍重复事件。
- 事件不得包含密钥、凭据、完整请求体等敏感信息。

## 2. `LogEvent` 数据格式

统一结构定义在 [`CubeAPI/src/logging/mod.rs`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeAPI/src/logging/mod.rs)：

| 字段 | JSON 类型 | 必填 | 含义 |
| --- | --- | --- | --- |
| `timestamp` | RFC 3339 string | 是 | `LogEvent::new` 构造事件时生成的 UTC 时间。 |
| `level` | string | 是 | `debug`、`info`、`warn` 或 `error`。 |
| `event` | string | 是 | 稳定的机器可读事件名；现有生命周期事件使用点号分隔的小写单词。 |
| 扩展字段 | 任意 JSON 值 | 否 | `fields` 中的上下文，通过 `#[serde(flatten)]` 平铺到顶层。 |

```json
{
  "timestamp": "2026-07-22T08:00:00Z",
  "level": "info",
  "event": "snapshot.created",
  "sandbox_id": "sb-123",
  "snapshot_id": "snap-456",
  "names": ["checkpoint"]
}
```

`field()` 写入字符串，`field_value()` 写入可序列化的 JSON 值。当前代码不会拒绝与 envelope 重名的扩展字段，因此产生端不得使用 `event`、`timestamp` 或 `level` 作为扩展字段名。

## 3. 强制规则

1. **成功后产生**：仅在底层 service 返回成功后产生生命周期成功事件。
2. **快照终态校验**：快照创建、回滚和删除仅在 `SnapshotService` 确认规范化状态为 `READY` 后产生；错误、缺失状态或其他状态均不得产生成功事件。
3. **每次一次**：每次成功的 Handler 调用只产生一个对应事件；未来 backend 重试仍可能造成客户端重复接收。
4. **响应不变**：事件产生不得修改既有 HTTP 状态码、响应体或响应头语义。
5. **使用事实来源**：优先使用成功响应中的规范化 ID，不得伪造无法取得的上下文。
6. **保持兼容**：增加可选字段是兼容扩展；删除字段、改变类型或重命名事件属于破坏性变更。
7. **传输解耦**：Handler 统一调用 `state.logger.log(event)`，禁止直接发送 Webhook 或实现重试。
8. **保护敏感信息**：不得记录 secret、API key、访问令牌、凭据、Webhook URL 或完整请求体。

`api.request`、`api.response` 和 `api.error` 是 Handler 遥测，不属于资源生命周期成功事件。它们只在部分 Handler 中产生，字段也随 Handler 变化。

## 4. 日志管线

```text
Handler
  -> AppState.logger.log(LogEvent)
  -> FilteredLogger（最低级别过滤）
  -> MultiLogger（并发分发）
  -> 已注册的后端
```

| 组件 | 当前职责 | 实现位置 |
| --- | --- | --- |
| `Logger` | 定义异步 `log`、`flush` 和后端名称接口。 | `CubeAPI/src/logging/mod.rs` |
| `FilteredLogger` | 丢弃低于阈值的事件，并转发 `flush`。 | `CubeAPI/src/logging/filtered.rs` |
| `MultiLogger` | 克隆事件，并发调用全部后端及其 `flush`。 | `CubeAPI/src/logging/multi.rs` |
| `AppState` | 保存供 Handler 使用的 `ArcLogger`。 | `CubeAPI/src/state.rs` |

结构化事件阈值当前只由 `--debug` 控制：默认是 `info`，使用 `--debug` 时为 `debug`。`--log-level`、`LOG_LEVEL` 和 `RUST_LOG` 控制 stdout 的 `tracing` 过滤，不会单独切换结构化事件阈值。

服务器 graceful shutdown 完成后会调用最外层 logger 的 `flush()`；包装器会将调用传给内部后端。

## 5. 后端清单

默认启动路径只注册 `FileLogger`。

| 后端 | 状态 | 默认注册 | 当前行为 |
| --- | --- | --- | --- |
| `FileLogger` | 已实现 | 是 | 通过无界 Tokio channel 入队，由后台任务写入按 UTC 日期滚动的 NDJSON 文件。 |
| `NoopLogger` | 已实现 | 否 | 丢弃事件，主要用于测试或禁用日志。 |
| `HttpLogger` | 空壳 | 否 | 配置结构存在，但 `log` 和 `flush` 不执行 HTTP 投递。 |
| `OtlpLogger` | 空壳 | 否 | 尚未把事件转换为 OTLP `LogRecord`。 |

`FilteredLogger` 和 `MultiLogger` 是包装器，不是传输后端。`main.rs` 中被注释的 HTTP/OTLP 代码只是扩展示意，不是可用配置。

### 5.1 文件后端

```text
{log_dir}/{log_prefix}-YYYY-MM-DD.log
```

| 配置 | 默认值 | 说明 |
| --- | --- | --- |
| `log_dir` / `LOG_DIR` / `--log-dir` | 可执行文件同级的 `log`；无法解析时为 `./log` | 输出目录。 |
| `log_prefix` / `LOG_PREFIX` / `--log-prefix` | `cube-api` | 文件名前缀。 |
| `--debug` | `false` | 是否保留 `debug` 级结构化事件。 |

每行是一个完整的 `LogEvent` JSON 对象。`log()` 只向 channel 发送消息，打开文件、序列化和写入均由后台任务完成。当前 channel 是无界队列，并非 Webhook 所需的有界投递队列。

## 6. 已登记事件清单

### 6.1 通用 API 遥测

| 事件 | 级别 | 当前字段 | 产生范围 |
| --- | --- | --- | --- |
| `api.request` | `debug` | `handler` 及请求上下文 | health 和部分 sandbox Handler 调用 service 前。 |
| `api.response` | `info` | `handler` 及结果上下文 | 部分 sandbox 查询 Handler 成功后。 |
| `api.error` | `error` | `handler`、`error` | 部分显式处理错误的 Handler。 |

这些事件不覆盖所有路由，消费者不得假定每次调用都有完整的 request/response/error 配对。

### 6.2 沙箱生命周期

| 事件 | 级别 | 事件字段 | API / 产生时机 |
| --- | --- | --- | --- |
| `sandbox.created` | `info` | `sandbox_id`、`template_id` | `POST /sandboxes` 成功后。 |
| `sandbox.deleted` | `info` | `sandbox_id` | `DELETE /sandboxes/{sandboxID}` 成功后。 |
| `sandbox.paused` | `info` | `sandbox_id` | `POST /sandboxes/{sandboxID}/pause` 成功后。 |
| `sandbox.resumed` | `info` | `sandbox_id` | `POST /sandboxes/{sandboxID}/resume` 成功后。 |
| `sandbox.timeout.updated` | `info` | `sandbox_id`、`timeout` | `POST /sandboxes/{sandboxID}/timeout` 成功后。 |
| `sandbox.refreshed` | `info` | `sandbox_id`、`duration` | `POST /sandboxes/{sandboxID}/refreshes` 成功后。 |

以上事件只描述由对应 CubeAPI Handler 完成的操作。绕过 CubeAPI 的自动状态转换不会自动产生这些事件。

### 6.3 快照生命周期

| 事件 | 级别 | 必填字段 | 可选字段 | API / 产生时机 |
| --- | --- | --- | --- | --- |
| `snapshot.created` | `info` | `sandbox_id`、`snapshot_id`、`names` | 无 | `POST /sandboxes/{sandboxID}/snapshots` 返回 `201` 前，且创建结果为 `READY`。 |
| `sandbox.rolled_back` | `info` | `sandbox_id`、`snapshot_id`、`operation_id`、`status` | 无 | `POST /sandboxes/{sandboxID}/rollback` 返回 `200` 前，且回滚结果为 `READY`。 |
| `snapshot.deleted` | `info` | `snapshot_id`、`operation_id`、`status` | `sandbox_id` | `DELETE /templates/{templateID}` 的快照分支返回 `204` 前，且删除结果为 `READY`。 |

删除前通过 `SnapshotService::get_snapshot_context` 判断 ID 是否属于快照。CubeMaster 返回非空 `origin_sandbox_id` 时，事件将其映射为 `sandbox_id`；旧记录缺少该字段时删除仍成功，事件省略 `sandbox_id`。普通模板删除不产生 `snapshot.deleted`。

快照事件名常量集中在 `CubeAPI/src/logging/mod.rs`。现有沙箱事件仍使用字符串字面量，当前代码尚无覆盖全部事件的集中注册表或订阅校验表。

## 7. Webhook 状态与边界

当前 CubeAPI **不会发送 Webhook 回调**。`HttpLogger` 是未注册的 no-op 空壳；产生 `LogEvent` 只会使 `FileLogger` 等已实现后端收到事件。

当前代码尚未提供：

- 一个或多个端点的配置和按事件订阅；
- JSON HTTP POST 投递；
- 有界异步队列和队列饱和策略；
- HMAC-SHA256 签名；
- 失败重试和指数退避；
- 可运行的接收端示例及端到端配置。

未来 Webhook backend 必须复用本文的 `LogEvent`。端点过滤、异步入队、签名、重试和关闭排空应由 backend 实现并独立测试；接收端必须按照至少一次投递语义容忍重复事件。

## 8. 各模块实现约定

### Handler

- service 成功且终态校验完成后调用 `state.logger.log()`。
- 使用响应中的规范化 ID，只添加必要上下文。
- 不直接操作文件、HTTP client、签名密钥或重试任务。

### Service

- 负责调用 CubeMaster、转换错误和验证业务响应。
- 快照 service 检查 `snapshot_id`、`operation_id` 和规范化 `READY` 状态，Handler 不重复校验。
- 旧数据缺少可选元数据时表达为字段缺失，不伪造 ID。

### Logger backend

- 实现 `Logger`，保证 `log()` 不在请求路径执行缓慢 I/O。
- 网络或磁盘 I/O 使用后台任务，并测试缓冲、丢弃和失败策略。
- 实现 `flush()`，让 graceful shutdown 尽可能排空已接受的事件。
- 诊断信息不得泄露 endpoint secret、签名密钥或敏感字段。

## 9. 新增事件流程

1. 定义稳定事件名和成功语义，确认它是生命周期事件而非普通 API 遥测。
2. 在成功终态构造 `LogEvent`，复用 `state.logger.log()`。
3. 检查扩展字段不与公共字段冲突，且不含敏感数据。
4. 增加成功路由测试，验证事件名、级别、字段和值。
5. 增加失败和非终态测试，验证不产生成功事件且 API 响应保持不变。
6. 对可能缺失的旧数据字段增加兼容测试，不伪造缺失值。
7. 同步更新本清单及英文版本。
8. 过滤、签名、重试和队列行为在 backend 测试中覆盖，不在 Handler 中重复传输协议。

---

## 附录：当前能力对照

| 能力 | 状态 | 说明 |
| --- | --- | --- |
| 结构化 `LogEvent` | 已实现 | 公共 envelope 加顶层扩展字段。 |
| 级别过滤、多后端分发 | 已实现 | `FilteredLogger` + `MultiLogger`。 |
| 异步 NDJSON 文件输出 | 已实现 | 默认注册 `FileLogger`。 |
| 沙箱生命周期事件 | 已实现 | 创建、删除、暂停、恢复、超时更新和刷新。 |
| 快照生命周期事件 | 已实现 | 创建、回滚和删除成功结果。 |
| HTTP Webhook 投递 | 未实现 | `HttpLogger` 当前为 no-op。 |
| HMAC、订阅、重试 | 未实现 | 应由未来 Webhook backend 提供。 |
| OTLP 导出 | 未实现 | `OtlpLogger` 当前为 no-op。 |
