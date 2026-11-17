# 沙箱生命周期 Webhook

[English](README.md) · [用户指南](../../../docs/zh/guide/webhooks.md)

CubeMaster 观察六类生命周期成功点，向静态集群级订阅发送 JSON POST。通知默认关闭，
使用进程内有界内存和有限重试。重启/崩溃可丢失剩余任务；不提供持久化、接管、顺序或
恰好一次保证。平台业务 Redis 仍是依赖。

`App.startWebhooks` 验证原 HTTP YAML，再组装内存 store 与 HTTP `Sender`。
全局关闭、配置错误或没有启用订阅时使用 `NopPublisher`，不创建投递资源；没有新增
backend YAML 键。

Go 扩展使用 `NewRuntime(parent, RuntimeOptions, SubscriptionProvider,
DeliveryStore, DeliverySender)`。Runtime 深复制一次启动订阅，`Subscription.Target`
由 sender 解释，成功由 `SendResult.Succeeded` 决定，非 HTTP 成功允许
`HTTPStatus=0`。通用 send 诊断归 Runtime，HTTP attempt/status/success 只由 HTTP
adapter 计数；未知 adapter code 使用有限的 `other` 指标标签。

Backend 为每次 claim 生成非空 `Receipt.Token` 并原子校验 ownership。
`Event.Snapshot` / `RestoreEvent` 校验并双向复制 v2 正文，保留原 ID、type、时间和
精确字节，不重复 Capture。本轮没有实现持久 backend。
依赖必须是有效且非 typed-nil 的实现，并由一个 Runtime 独占；Claim 应返回有效任务、
Event 和 receipt，加载失败返回明确错误。
Runtime 运行期间，Claim 报错后记录诊断，经过可取消的 50ms 等待后继续轮询，
每次使用新的操作 context。Backend 仍负责失败或结果不明确的领取状态；继续轮询
不代表自动恢复或转移已有 claim。

构造失败时依赖仍归调用方；成功后由 Runtime 收尾：停止入口、flush writer、Drain、
取消并 join worker、关闭 sender、Close store。所有步骤共享两秒总预算及 caller
更早取消；存储操作使用有限 `StoreOperationTimeout`（App 为 1s），取消确认也受剩余
预算约束。Backend 必须遵守 context；sender.Close 只做快速本地释放。Complete 失败
不增加 terminal；Close 只返回实际丢弃数，持久保留不计丢弃。Memory Close 在预算
耗尽后仍释放本地记录和凭证索引。

外部包实际执行回归见 [Runtime](runtime_external_test.go)、
[Event 恢复](event_snapshot_external_test.go) 与 [Store](store_external_test.go)。

[用户指南](../../../docs/zh/guide/webhooks.md) 包含配置、事件时点、接收端 Header/去重、
字段兼容、HTTP 安全和旧 PR 试用配置迁移。Timeout 响应截止时间可来自 fallback，
killed 遵循原 handler 成功规则，metadata 仅来自可信当前创建输入。

当前 receiver/组件/SDK 命令和证据限制见
[测试运行说明](../../../tests/e2e/webhook/README.md)。旧 Redis 验证不构成内存版本验收。
