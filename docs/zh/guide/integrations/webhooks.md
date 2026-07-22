---
title: Webhook 事件通知
author: io-wy
date: 2026-07-22
tags:
  - integration
  - webhook
  - cubeapi
lang: zh-CN
---

# Webhook 事件通知

CubeAPI 可以把沙箱生命周期事件投递到一个或多个 HTTP endpoint。投递路径是异步的：
沙箱 API 请求只负责把事件放入队列，不会等待远端 Webhook 接收端响应。

## 支持的事件

默认生命周期订阅包含：

- `sandbox.created`
- `sandbox.deleted`
- `sandbox.paused`
- `sandbox.resumed`

`sandbox.created` 包含 `sandbox_id` 和 `template_id`。其它生命周期事件包含
`sandbox_id`。

## 配置

启动 CubeAPI 前，把 `CUBE_API_WEBHOOK_ENDPOINTS` 设置为 JSON 数组：

```bash
export CUBE_API_WEBHOOK_ENDPOINTS='[
  {
    "url": "http://127.0.0.1:9000/webhook",
    "events": [
      "sandbox.created",
      "sandbox.deleted",
      "sandbox.paused",
      "sandbox.resumed"
    ],
    "secret": "dev-secret",
    "queue_capacity": 1024,
    "max_retries": 3,
    "retry_base_ms": 500,
    "retry_max_ms": 30000,
    "timeout_secs": 5
  }
]'
```

字段说明：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `url` | 是 | 接收 JSON `POST` 请求的 HTTP 或 HTTPS endpoint。 |
| `events` | 否 | 当前 endpoint 订阅的事件名。默认订阅四个沙箱生命周期事件。 |
| `secret` | 否 | HMAC-SHA256 签名密钥。省略或设为空字符串表示不启用签名。建议使用 16 字节或更长的强随机值。 |
| `queue_capacity` | 否 | 每个 endpoint 的内存队列上限。默认值：`1024`。 |
| `max_retries` | 否 | 初次投递失败后的重试次数。默认值：`3`。 |
| `retry_base_ms` | 否 | 指数退避的初始延迟。默认值：`500`。 |
| `retry_max_ms` | 否 | 最大重试延迟。默认值：`30000`。 |
| `timeout_secs` | 否 | 单次投递请求超时时间。默认值：`5`。 |

## Payload

CubeAPI 以 JSON 格式发送事件。示例：

```json
{
  "timestamp": "2026-07-22T10:00:00Z",
  "level": "info",
  "event": "sandbox.created",
  "sandbox_id": "sbx-123",
  "template_id": "tpl-456"
}
```

随着 CubeAPI 增加更多事件上下文，payload 可能包含其它结构化字段。

## 请求头

每次投递都会包含：

- `Content-Type: application/json`
- `X-Cube-Event`
- `X-Cube-Delivery`

配置 `secret` 后，CubeAPI 还会发送：

- `X-Cube-Timestamp`
- `X-Cube-Nonce`
- `X-Cube-Signature-256`

签名基于原始请求体计算：

```text
sha256=HMAC_SHA256(secret, timestamp + "." + nonce + "." + raw_body)
```

接收端应使用常量时间比较函数校验签名。
`X-Cube-Timestamp` 是每次投递时生成的 Unix 毫秒时间戳。
接收端也可以按照自己的时钟偏移策略拒绝过旧的 `X-Cube-Timestamp`，例如设置 5 分钟
容忍窗口。

## 失败处理

网络错误以及这些 HTTP 状态码会触发重试：`408`、`429`、`5xx`。其它 `4xx`
响应会被视为接收端侧的不可重试失败。

每个 Webhook endpoint 都有独立的有界队列和后台 worker。某个 endpoint 变慢或不可达，
不会阻塞沙箱生命周期 API 请求，也不会影响其它 Webhook endpoint。如果某个 endpoint
队列已满，CubeAPI 会丢弃该 endpoint 上的事件并写入 warning 日志。

## 本地接收端

可运行的接收端位于 `examples/webhook-receiver`：

```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=dev-secret python3 receiver.py
```

触发沙箱创建、暂停、恢复或删除操作后，接收端会打印 delivery ID、事件名、签名结果和
JSON payload。

## 通用告警对接

接收端可以把验签通过的事件转发到通用 HTTP 告警系统或企业 IM 机器人。建议尽量让
CubeAPI Webhook endpoint 处在内网，先完成 HMAC 验签再转发，并避免把密钥或大型
payload 发到第三方聊天系统。
