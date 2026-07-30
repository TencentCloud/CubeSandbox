---
title: Webhook 事件通知
author: CubeSandbox Contributors
date: 2026-07-30
tags:
  - integration
  - webhook
  - alerting
lang: zh-CN
---

# Webhook 事件通知

[English](../../../guide/integrations/webhooks.md)

CubeSandbox 可以把 CubeAPI 产生的结构化事件投递到用户自己的 HTTP endpoint。
CubeAPI 把所有事件类型转发给 CubeOps；CubeOps 负责订阅配置、endpoint 分批、
HMAC 签名、重试和外部 HTTP 投递。

## 启用 Webhook

CubeOps 只在启动时读取 webhook 配置，本版本不提供管理 API 或热更新。未配置文件时，
CubeOps 仍会接收内部事件，但不会向外部 endpoint 投递。

one-click 安装可把随包示例复制为
`/usr/local/services/cubetoolbox/CubeOps/webhooks.toml`：

```toml
[delivery]
event_queue_capacity = 10000
max_outstanding_deliveries = 1000
max_concurrent_requests = 100
default_batch_size = 1
flush_interval_secs = 5
request_timeout_secs = 5
max_attempts = 3
initial_backoff_ms = 500
max_backoff_secs = 10

[[endpoints]]
name = "ops-lifecycle"
url = "http://127.0.0.1:8088/webhook"
events = [
  "sandbox.created",
  "sandbox.deleted",
  "sandbox.paused",
  "sandbox.resumed",
  "api.error",
]
batch_size = 1
secret_env = "CUBE_WEBHOOK_SECRET_0"

[[endpoints]]
# 同一 URL 可以复用于互不重叠的高频事件集合。
name = "ops-api"
url = "http://127.0.0.1:8088/webhook"
events = ["api.request", "api.response"]
batch_size = 100
secret_env = "CUBE_WEBHOOK_SECRET_0"
```

在 `/usr/local/services/cubetoolbox/.one-click.env` 中设置路径和可选签名密钥：

```bash
CUBE_OPS_WEBHOOK_CONFIG=/usr/local/services/cubetoolbox/CubeOps/webhooks.toml
CUBE_WEBHOOK_SECRET_0=change-me
```

`secret_env` 必须以 `CUBE_WEBHOOK_SECRET_` 开头。修改 TOML 或密钥后重启
CubeOps：

```bash
sudo systemctl restart cube-sandbox-cubeops.service
```

Helm 部署可通过 `cubeOps.webhook.config` 提供内联 TOML，或通过
`cubeOps.webhook.existingConfigMap` 指定包含 `webhooks.toml` 的 ConfigMap。
`cubeOps.webhook.secretName` 指向的 Secret 应以 `CUBE_WEBHOOK_SECRET_*`
作为 key。

## Payload 与 Header

CubeOps 为每个 endpoint batch 发送一次 HTTP `POST`：

```json
{
  "batch_id": "8f6a3f7d-7d87-4ef5-a639-2f6c2b1976f8",
  "events": [
    {
      "timestamp": "2026-07-30T10:00:00Z",
      "level": "info",
      "event": "sandbox.created",
      "sandbox_id": "sbx-xxx",
      "template_id": "tpl-xxx"
    }
  ]
}
```

事件继续使用 CubeAPI 结构化日志的扁平 JSON 形式。CubeOps 为每个 endpoint
独立缓冲，达到 `batch_size` 或 `flush_interval_secs` 到期后发送。如果相同
规范化 URL 和事件被重复订阅，CubeOps 会拒绝启动。

| Header | 说明 |
| --- | --- |
| `Content-Type` | `application/json` |
| `User-Agent` | `CubeSandbox-Webhook/1.0` |
| `X-Cube-Signature-256` | 配置 `secret_env` 时出现，格式为 `sha256=<hex>` |

必须使用原始 request body 验签：

```python
import hashlib
import hmac

def verify(secret: str, body: bytes, header: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(header, expected)
```

## 投递语义

Webhook 是 best-effort：一个 batch 可能到达零次、一次或多次。外部 `batch_id`
标识一次 endpoint batch，CubeOps 重试该 batch 时会复用它。接收端应在产生该
batch 的外部副作用前原子记录已完成的 `batch_id`；如果逐条处理 batch，需要由
接收端自行实现故障恢复。

CubeAPI 对每个内部 batch 只尝试向 CubeOps 发送一次。CubeOps 会对外部网络错误、
超时、HTTP `408`、`429` 和 `5xx` 重试，最多执行 `max_attempts` 次；
其他 `4xx` 是最终失败。

不同 endpoint batch 可以并发投递，因此不保证跨 batch 到达顺序。接收端应依据事件
时间和业务状态，而不是到达顺序重建状态。CubeAPI 和 CubeOps 的 webhook 队列都只
存在于内存中；队列满、重试耗尽、进程崩溃、重启或关闭超时都可能丢失事件。
多个 CubeOps 副本之间不共享队列或去重状态。

沙箱 API 请求不会等待 CubeOps 或外部接收端。CubeAPI 使用有界内部队列，队列满时
直接丢弃；CubeOps 对内部 batch 要么整批接收，要么返回 `503`，不会部分入队。

## 事件

常见业务事件包括 `sandbox.created`、`sandbox.deleted`、`sandbox.paused`、
`sandbox.resumed`、`sandbox.timeout.updated` 和 `sandbox.refreshed`。
诊断事件包括 `api.request`、`api.response` 和 `api.error`。CubeAPI 不维护
订阅列表，因此后续新增的事件名也会被转发；CubeOps 根据各 endpoint 的 `events`
数组进行过滤。

## 容量

CubeOps 的容量参数限制不同阶段：

```text
event_queue_capacity -> max_outstanding_deliveries -> max_concurrent_requests
```

`event_queue_capacity` 统计已接收、等待分发的事件；
`max_outstanding_deliveries` 包括等待网络 permit 和处于重试退避的投递；
`max_concurrent_requests` 只统计正在执行的 HTTP attempt，因此退避不会占用网络
并发名额。

企业微信等 IM 机器人通常需要专用消息结构。建议使用 relay 验签、按 `batch_id`
去重、转换 payload，再调用机器人接口。可运行示例位于
[`examples/webhook-receiver`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)。
