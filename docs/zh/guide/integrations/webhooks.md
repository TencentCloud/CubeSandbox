---
title: Webhook 事件通知
author: CubeSandbox Contributors
date: 2026-07-08
tags:
  - integration
  - webhook
  - alerting
lang: zh-CN
---

# Webhook 事件通知

[English](../../../guide/integrations/webhooks.md)

CubeAPI 可以把结构化事件投递到用户自己的 HTTP endpoint。上层编排系统、
告警平台、审计采集器、企业 IM 机器人等系统可以通过 Webhook 实时联动，
无需轮询 CubeAPI。

## 启用 Webhook

CubeAPI 只在启动时读取 Webhook 配置。本版本不提供 REST 管理 API，也不支持热更新。

创建 `/usr/local/services/cubetoolbox/CubeAPI/webhooks.toml`：

```toml
[delivery]
queue_size = 10000
request_timeout_secs = 5
max_attempts = 3
initial_backoff_ms = 500
max_backoff_secs = 10
max_in_flight = 100

[[endpoints]]
name = "ops"
url = "http://127.0.0.1:8088/webhook"
events = [
  "sandbox.created",
  "sandbox.deleted",
  "sandbox.paused",
  "sandbox.resumed",
  "api.error",
]
secret_env = "CUBE_WEBHOOK_SECRET_0"
```

在 `/usr/local/services/cubetoolbox/.one-click.env` 中增加配置路径和可选密钥：

```bash
CUBE_API_WEBHOOK_CONFIG=/usr/local/services/cubetoolbox/CubeAPI/webhooks.toml
CUBE_WEBHOOK_SECRET_0=change-me
```

如果 one-click 部署中有多个带签名的 endpoint，建议统一使用
`CUBE_WEBHOOK_SECRET_*` 前缀，例如 `CUBE_WEBHOOK_SECRET_0` 和
`CUBE_WEBHOOK_SECRET_1`，再在各 endpoint 的 `secret_env` 中分别引用。

重启 CubeAPI：

```bash
sudo systemctl restart cube-sandbox-cube-api.service
```

## Payload 与 Header

CubeAPI 会为每个匹配事件发送一次 HTTP `POST`。JSON body 沿用 CubeAPI
结构化日志的扁平结构：

```json
{
  "timestamp": "2026-07-08T10:00:00Z",
  "level": "info",
  "event": "sandbox.created",
  "sandbox_id": "sbx-xxx",
  "template_id": "tpl-xxx"
}
```

Header：

| Header | 说明 |
| --- | --- |
| `Content-Type` | `application/json` |
| `User-Agent` | `CubeSandbox-Webhook/1.0` |
| `X-Cube-Event` | 事件名，例如 `sandbox.created` |
| `X-Cube-Delivery` | 投递 id，重试时保持不变 |
| `X-Cube-Signature-256` | 配置 `secret_env` 后出现，格式为 `sha256=<hex>` |

用原始 request body 验签：

```python
import hashlib
import hmac

def verify(secret: str, body: bytes, header: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(header, expected)
```

## 事件列表

推荐订阅的业务事件：

| 事件 | 字段 |
| --- | --- |
| `sandbox.created` | `sandbox_id`, `template_id` |
| `sandbox.deleted` | `sandbox_id` |
| `sandbox.paused` | `sandbox_id` |
| `sandbox.resumed` | `sandbox_id`, `template_id` |
| `sandbox.timeout.updated` | `sandbox_id`, `timeout` |
| `sandbox.refreshed` | `sandbox_id`, `duration` |

显式订阅时也可以使用诊断类事件：

| 事件 | 说明 |
| --- | --- |
| `api.request` | Debug 风格的请求事件。Webhook 的事件过滤独立于 FileLogger 的 level 过滤。 |
| `api.response` | 部分 handler 的成功响应事件。 |
| `api.error` | CubeAPI 已发出的结构化错误事件。 |

本版本暂不覆盖 snapshot/template 事件，也不覆盖 CubeProxy sidecar 自动触发的
auto-pause/auto-resume。

## 重试与非阻塞行为

Webhook 投递是异步的。CubeAPI 只把匹配事件放入队列，沙箱 API 调用不会等待接收端返回。

网络错误、超时、HTTP `408`、`429`、`5xx` 会触发重试。其他 `4xx`
视为最终失败。重试延迟使用指数退避，并受 `max_backoff_secs` 限制。

## 企业 IM 与通用告警集成

企业微信机器人等 IM 机器人通常要求自己的消息格式。建议部署一个小型 relay
服务作为 Webhook endpoint：先校验 Cube 签名，再把事件 payload 转成机器人的
`msgtype` 消息格式，最后转发到机器人 URL。

如果告警平台能直接接收任意 JSON，可以直接把平台的 HTTP endpoint 配到 Webhook，
并在平台侧按 `event` 字段过滤。

可运行的接收端示例位于
[`examples/webhook-receiver`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)。
