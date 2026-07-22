# 快照生命周期事件契约

> 状态：已实现（事件产生端）
> 关联 issue：[#642 — CubeSandbox Webhook 事件通知](https://github.com/TencentCloud/CubeSandbox/issues/642)

## 目的

CubeAPI 已经为沙箱生命周期变更产生结构化 `LogEvent`，但快照操作成功后目前只写 tracing 日志。本更改为快照操作补充事件产生端的结构化事件，使任何已配置的日志后端（包括未来的 HTTP Webhook 后端）都能一致地投递这些事件。

本提案不实现 Webhook 传输、端点配置、签名或重试；这些能力仍由 logging backend 负责。

## 事件契约

所有事件沿用现有 `LogEvent` envelope，其中包含 `event`、`timestamp` 和 `level`；操作相关字段平铺在同一个 JSON 对象中。

| 操作 | 事件 | 必填字段 | 产生时机 |
| --- | --- | --- | --- |
| 创建快照 | `snapshot.created` | `sandbox_id`、`snapshot_id`、`names` | `SnapshotService::create` 成功返回后 |
| 回滚沙箱 | `sandbox.rolled_back` | `sandbox_id`、`snapshot_id`、`operation_id`、`status` | `SnapshotService::rollback` 确认终态成功后 |
| 删除快照 | `snapshot.deleted` | `snapshot_id`、`operation_id`、`status`，可获得时包含 `sandbox_id` | `DELETE /templates/{id}` 的快照分支确认终态成功后 |

`snapshot.deleted` 复用现有用于区分快照和普通模板的快照详情查询。CubeMaster 返回 `origin_sandbox_id` 时，事件会将其作为 `sandbox_id`；旧记录缺少该上下文时，删除仍然成功，事件会省略该字段。

## 产生规则

- 每次 Handler 调用成功时产生一个结构化事件。
- 底层 service 返回错误时不得产生成功事件。
- 只有同步 CubeMaster 操作到达成功终态后才能产生事件。
- 事件名称必须集中定义为常量，避免 Handler、订阅校验、测试和文档发生漂移。
- 事件字段不得包含请求体、密钥、端点 URL 或其他敏感值。
- 事件产生沿用现有 `Logger` 接口；网络投递必须保持异步，不得阻塞 API 请求主路径。
- Webhook backend 可能在投递结果不明确时重试，因此接收端必须容忍重复事件。

## 代码更改

实现预计只修改事件产生端及其测试：

- `CubeAPI/src/logging/`：定义三个事件名称常量。
- `CubeAPI/src/handlers/snapshots.rs`：产生 `snapshot.created` 和 `sandbox.rolled_back`。
- `CubeAPI/src/handlers/templates.rs`：仅在标识符被解析为快照的分支产生 `snapshot.deleted`。
- `CubeAPI/src/services/snapshots.rs`：保留现有快照查询上下文，使删除事件在 CubeMaster 提供来源信息时携带对应沙箱 ID。
- CubeAPI 测试：验证事件名称、Payload 字段、仅成功时产生事件，以及普通模板删除不会产生 `snapshot.deleted`。

## 非目标

- 实现或选择 HTTP Webhook backend。
- 修改 Webhook 配置、HMAC 签名、重试或队列行为。
- 在本次更改中增加失败事件。
- 产生模板构建完成事件。模板创建和重建 Handler 返回 `202 Accepted`，无法观察最终构建结果，因此不得产生 `template.created` 或 `template.build.succeeded`。
- 覆盖 CubeAPI 请求 Handler 之外的自动暂停/恢复转换。

## 验收标准

- 三个事件都只能在对应操作成功后产生。
- Payload 包含事件契约中列出的全部必填字段。
- 操作失败时不产生上述任何成功事件。
- 快照删除和普通模板删除仍能被准确区分。
- 现有 API 状态码和响应体保持不变。
- `cargo fmt --manifest-path CubeAPI/Cargo.toml -- --check` 通过。
- 相关 CubeAPI 单元测试或集成测试通过。
