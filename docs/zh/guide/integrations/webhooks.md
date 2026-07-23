---
title: CubeSandbox Webhook 集成指南
author: dujunjin
date: 2026-07-23
tags:
  - integration
  - webhook
  - observability
lang: zh-CN
---

# CubeSandbox Webhook 集成指南

[English](../../../guide/integrations/webhooks.md)

CubeAPI 可以在沙箱生命周期操作成功后，异步通知 HTTP 接收端。这样上层 Agent
编排器、运维平台或企业微信机器人可以近实时响应状态变化，不必持续轮询 API。

## 配置

在启动 `cube-api` 前设置以下环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `WEBHOOK_URLS` | 未设置 | 逗号分隔的 HTTP(S) 接收端地址 |
| `WEBHOOK_EVENTS` | `sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed` | 事件白名单；`*` 表示所有结构化事件 |
| `WEBHOOK_SECRET` | 未设置 | 可选 HMAC-SHA256 secret |
| `WEBHOOK_QUEUE_SIZE` | `1024` | 有界队列容量 |
| `WEBHOOK_MAX_RETRIES` | `3` | 初次投递后的重试次数 |
| `WEBHOOK_RETRY_BASE_MS` | `250` | 指数退避初始延迟 |
| `WEBHOOK_REQUEST_TIMEOUT_SECS` | `5` | 每次 HTTP 请求超时 |

也支持 `CUBE_API_WEBHOOK_*` 别名。多个地址会收到相同的事件集合；不设置
`WEBHOOK_URLS` 即关闭 Webhook 投递。

本地配置示例：

```bash
export WEBHOOK_URLS='http://127.0.0.1:8099/webhook,https://ops.example.com/cube'
export WEBHOOK_EVENTS='sandbox.created,sandbox.paused,sandbox.resumed,sandbox.deleted'
export WEBHOOK_SECRET='replace-with-a-random-secret'
```

非本机地址应使用 HTTPS；HTTP 仅适合本地接收端示例。

## 事件与 Payload

以下四类事件会在对应 CubeMaster 操作成功后产生：

- `sandbox.created`
- `sandbox.deleted`
- `sandbox.paused`
- `sandbox.resumed`

每次 POST 的 `Content-Type` 为 `application/json`。结构化 CubeAPI 事件至少包含：

```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-23T12:00:00.000Z",
  "level": "info",
  "sandbox_id": "sbx-example",
  "template_id": "tpl-example"
}
```

当前 `template_id` 在创建事件中可用。未来可能增加字段，接收端应忽略未知字段，
不要依赖字段顺序。

## 投递语义

API handler 使用 `try_send` 将已订阅事件放入有界内存队列，不等待接收端响应；后台
worker 将同一份序列化 body 发往所有配置地址。队列满时会丢弃事件，并记录带事件
名称的 warning。这能保护沙箱创建、暂停、恢复、销毁主路径的延迟和内存使用。

HTTP 2xx 表示成功。网络错误、超时、HTTP 408、429 和 5xx 会按指数退避重试；其他
4xx 会记录日志但不重试。重试次数由 CubeAPI 限制，投递失败不会改变沙箱 API 请求的
结果。

## HMAC-SHA256 校验

设置 `WEBHOOK_SECRET` 后，CubeAPI 会添加：

```text
X-Cube-Signature-256: sha256=<小写十六进制摘要>
```

摘要针对 HTTP 请求的原始 body 计算。应在解析 JSON 前校验，并使用常量时间比较：

```python
import hashlib
import hmac

expected = "sha256=" + hmac.new(
    WEBHOOK_SECRET.encode(), raw_body, hashlib.sha256
).hexdigest()
if not hmac.compare_digest(
    request.headers.get("X-Cube-Signature-256", ""), expected
):
    return unauthorized()
```

可运行示例
[`examples/webhook-receiver`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)
包含完整校验逻辑，签名错误时返回 `401`。

## 本地验证

```bash
python3 examples/webhook-receiver/receiver.py \
  --port 8099 \
  --secret replace-with-a-random-secret

WEBHOOK_URLS=http://127.0.0.1:8099/webhook \
WEBHOOK_SECRET=replace-with-a-random-secret \
cargo run --manifest-path CubeAPI/Cargo.toml
```

使用正常的 SDK 或 REST API 创建、暂停、恢复、销毁沙箱，确认接收端打印四类事件。
接收端 README 还提供不依赖集群的 `curl` 签名验证命令。

## 对接企业微信和通用 HTTP 告警

建议把 CubeAPI Webhook 接收端作为内部适配器：先校验 CubeSandbox 签名，再把事件
转换为目标格式。企业微信机器人需要类似
`{"msgtype":"markdown","markdown":{"content":"..."}}` 的 envelope；通用告警
系统可以携带自己的 Authorization header 转发原始 JSON。第二跳应使用独立的超时和
重试策略，不要把 CubeSandbox 签名当作下游认证。

完整的标准库转发片段见双语接收端 README。
