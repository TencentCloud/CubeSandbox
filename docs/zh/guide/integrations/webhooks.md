---
title: Webhook 事件通知
author: CubeSandbox Contributors
date: 2026-07-09
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
# 等待 Webhook worker 处理的事件数。队列满时新事件会被丢弃并记录错误日志，
# 不阻塞 API 主路径。
event_queue_capacity = 10000
# 同时存在的 endpoint batch 投递数，包括等待 HTTP permit、发送请求和重试退避中的投递。
max_outstanding_deliveries = 1000
# 同时执行网络 I/O 的 HTTP 请求次数。
max_concurrent_requests = 100
# endpoint 未设置 batch_size 时使用的默认 batch 大小。设为 1 表示事件到达后尽快投递。
default_batch_size = 1
# 全局定时 flush 间隔，单位秒。未满 batch_size 的事件会在该间隔到期后投递。
flush_interval_secs = 5
# 单次 HTTP POST 请求超时时间，单位秒。
request_timeout_secs = 5
# 最大投递次数，包含首次请求和后续重试。
max_attempts = 3
# 首次重试前的等待时间，单位毫秒；后续重试按指数退避增长。
initial_backoff_ms = 500
# 指数退避的最大等待时间，单位秒。
max_backoff_secs = 10
[[endpoints]]
# endpoint 名称，用于日志定位。
name = "ops-lifecycle"
# Webhook 接收端 URL。CubeAPI 会向该地址发送 HTTP POST。
url = "http://127.0.0.1:8088/webhook"
# 该 endpoint 订阅的事件列表。
events = [
  "sandbox.created",
  "sandbox.deleted",
  "sandbox.paused",
  "sandbox.resumed",
  "api.error",
]
# 当前 endpoint 的 batch 大小；省略时使用 delivery.default_batch_size。
batch_size = 1
# 可选。HMAC-SHA256 签名密钥所在的环境变量名；密钥值不要直接写在本文件里。
secret_env = "CUBE_WEBHOOK_SECRET_0"

[[endpoints]]
# 同一个 URL 可以配置多次，但不同 endpoint 的事件集合不能与相同 URL 的其他配置重叠。
name = "ops-api"
url = "http://127.0.0.1:8088/webhook"
events = ["api.request", "api.response"]
batch_size = 100
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

CubeAPI 会为每个匹配 endpoint 的 batch 发送一次 HTTP `POST`。JSON body 是
batch envelope：

```json
{
  "batch_id": "8f6a3f7d-7d87-4ef5-a639-2f6c2b1976f8",
  "events": [
    {
      "timestamp": "2026-07-09T10:00:00Z",
      "level": "info",
      "event": "sandbox.created",
      "sandbox_id": "sbx-xxx",
      "template_id": "tpl-xxx"
    }
  ]
}
```

`events` 中的每个元素沿用 CubeAPI 结构化日志的扁平结构。endpoint 未设置
`batch_size` 时使用 `delivery.default_batch_size`。当 `batch_size = 1` 时，
每个 batch 只包含一个事件，worker 收到后会尽快投递。如果某个 endpoint
把 `batch_size` 设置为大于 1，CubeAPI 会为该 endpoint 单独缓冲事件，直到
达到 `batch_size` 或全局 `flush_interval_secs` 到期后再投递。

同一个 URL 可以出现在多条 endpoint 配置中，只要它们订阅的事件集合不重叠。
这种方式适合把生命周期事件实时投递、同时把高频诊断事件批量投递到同一个接收端。
如果多条 endpoint 配置同时订阅了相同 URL 和相同事件，CubeAPI 会拒绝启动。

Header：

| Header | 说明 |
| --- | --- |
| `Content-Type` | `application/json` |
| `User-Agent` | `CubeSandbox-Webhook/1.0` |
| `X-Cube-Signature-256` | 配置 `secret_env` 后出现，格式为 `sha256=<hex>` |

接收端可以使用 `batch_id` 做幂等处理。重试时会复用同一个 `batch_id`，并重新
发送相同的 batch body。

单个 batch 内的事件保留其产生顺序；但 CubeAPI 不保证不同 batch 的到达顺序，即使
它们发往同一个 endpoint。不同 batch 可能并发投递，较早 batch 的重试也可能晚于较新
batch 到达。接收端重建状态时应使用 `batch_id` 和事件时间戳，而不能依赖到达顺序。

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

### 投递容量与背压关系

三个容量参数限制不同阶段，单位也不同：

```text
事件 channel                   endpoint 投递任务                    HTTP 请求 attempt
event_queue_capacity      ->   max_outstanding_deliveries     ->   max_concurrent_requests
```

- `event_queue_capacity` 统计尚未被 Webhook worker 处理的事件。
- `max_outstanding_deliveries` 统计从 task 创建到成功或重试耗尽的 endpoint
  batch 投递；等待 HTTP permit 和重试退避期间都会占用这个名额。
- `max_concurrent_requests` 只统计正在执行网络 I/O 的 HTTP attempt；进入重试
  退避前会释放这个 permit。

三个值不要求严格的数值大小关系，因为一个事件可能 fan-out 到多个 endpoint，
一个 batch 投递也可能包含多个事件。通常让 `max_outstanding_deliveries` 大于
`max_concurrent_requests` 更有利，这样处于重试退避的投递不会阻止其他投递使用
空闲的请求并发。增大 `event_queue_capacity` 可以吸收短时突发流量，但也会占用
更多内存并允许形成更大的延迟积压。

达到 outstanding delivery 上限后，worker 会先等待一个投递完成，再继续读取事件。
此时事件 channel 会逐渐填满；达到 `event_queue_capacity` 后，新的匹配事件会被
丢弃并记录日志，沙箱 API 主路径仍会正常返回。

## 企业 IM 与通用告警集成

企业微信机器人等 IM 机器人通常要求自己的消息格式。建议部署一个小型 relay
服务作为 Webhook endpoint：先校验 Cube 签名，再把 batch payload 转成机器人的
`msgtype` 消息格式，最后转发到机器人 URL。

如果告警平台能直接接收任意 JSON，可以直接把平台的 HTTP endpoint 配到 Webhook，
并在平台侧按 `events` 中每个元素的 `event` 字段过滤。

可运行的接收端示例位于
[`examples/webhook-receiver`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)。
