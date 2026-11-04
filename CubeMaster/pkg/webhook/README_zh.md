# 沙箱生命周期 Webhook

[English](README.md) · [用户指南](../../../docs/zh/guide/webhooks.md)

CubeMaster 观察六类生命周期成功点，向静态集群级订阅发送 JSON POST。通知默认关闭，
使用进程内有界内存和有限重试。重启/崩溃可丢失剩余任务；不提供持久化、接管、顺序或
恰好一次保证。平台业务 Redis 仍是依赖。

[用户指南](../../../docs/zh/guide/webhooks.md) 包含配置、事件时点、接收端 Header/去重、
字段兼容、HTTP 安全和旧 PR 试用配置迁移。Timeout 响应截止时间可来自 fallback，
killed 遵循原 handler 成功规则，metadata 仅来自可信当前创建输入。

实现维护见 [DELIVERY.md](DELIVERY.md)。当前 receiver/组件/SDK 命令和证据限制见
[测试运行说明](../../../tests/e2e/webhook/README.md)。旧 Redis 验证不构成内存版本验收。
