# Webhook 事件通知

CubeAPI 可以把沙箱生命周期事件投递到一个或多个 HTTP 端点。上层编排系统、
告警系统或审计流水线可以通过 Webhook 实时感知沙箱状态变化，避免轮询
CubeAPI。

Webhook 投递是异步的。沙箱 API Handler 只把事件写入队列，然后继续原本的
控制流；接收端慢、超时或不可达，不会阻塞沙箱创建、暂停、恢复或删除。

## 支持的事件

当前 CubeAPI Webhook 集成会发送以下沙箱生命周期事件：

| 事件 | 触发条件 | 额外字段 |
| --- | --- | --- |
| `sandbox.created` | `POST /sandboxes` 成功 | `template_id` |
| `sandbox.paused` | `POST /sandboxes/{sandboxID}/pause` 成功 | |
| `sandbox.resumed` | `POST /sandboxes/{sandboxID}/resume` 成功 | |
| `sandbox.deleted` | `DELETE /sandboxes/{sandboxID}` 成功 | |

## 配置

启动 `cube-api` 时设置 `CUBE_API_WEBHOOKS`，值为 JSON 数组。数组中的每一项
定义一个接收端。

```bash
export CUBE_API_WEBHOOKS='[
  {
    "url": "http://127.0.0.1:9000/webhook",
    "events": [
      "sandbox.created",
      "sandbox.deleted",
      "sandbox.paused",
      "sandbox.resumed"
    ],
    "secret": "dev-secret",
    "timeout_secs": 3,
    "max_retries": 3
  }
]'
```

字段说明：

| 字段 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `url` | string | 是 | 完整的 HTTP 或 HTTPS 回调地址。 |
| `events` | string array | 否 | 订阅的事件名。为空或不填表示订阅全部事件。 |
| `secret` | string | 否 | HMAC-SHA256 签名密钥。不填则不签名。 |
| `timeout_secs` | integer | 否 | 单次 HTTP 投递超时时间。默认 `3`，最小有效值 `1`。 |
| `max_retries` | integer | 否 | 首次投递失败后的重试次数。默认 `3`。 |

如果 JSON 无法解析，CubeAPI 会禁用 Webhook 投递并记录 warning。`url` 为空
的端点会被忽略。

## Payload

CubeAPI 使用 HTTP `POST` 发送事件，请求头包含 `Content-Type:
application/json`。

`sandbox.created` 示例：

```json
{
  "event": "sandbox.created",
  "timestamp": "2026-06-30T14:49:20.879472593+00:00",
  "sandbox_id": "c07539a1d61a4544b0c075a9b236871a",
  "template_id": "tpl-7c41d76bd0ea4457a2ccb470"
}
```

`sandbox.deleted` 示例：

```json
{
  "event": "sandbox.deleted",
  "timestamp": "2026-06-30T14:49:48.368169185+00:00",
  "sandbox_id": "c07539a1d61a4544b0c075a9b236871a"
}
```

每个 Payload 都包含：

| 字段 | 说明 |
| --- | --- |
| `event` | 事件名。 |
| `timestamp` | CubeAPI 生成的 RFC 3339 事件时间。 |
| `sandbox_id` | 沙箱 ID。 |

当 CubeAPI 有更多上下文时，会携带额外字段，例如 `sandbox.created` 的
`template_id`。

## 签名验证

配置 `secret` 后，CubeAPI 会对原始请求体签名，并发送以下 Header：

| Header | 说明 |
| --- | --- |
| `X-Cube-Event` | 事件名。 |
| `X-Cube-Timestamp` | Unix 秒级时间戳。 |
| `X-Cube-Signature` | `sha256=<hex hmac>`。 |

签名字符串为：

```text
<X-Cube-Timestamp>.<raw request body>
```

Python 验签示例：

```python
import hashlib
import hmac


def verify(secret: str, timestamp: str, body: bytes, signature: str) -> bool:
    signed = timestamp.encode("utf-8") + b"." + body
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), signed, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(signature, expected)
```

接收端应在验签失败时返回 `401 Unauthorized`。

## 重试行为

CubeAPI 会把非 2xx HTTP 响应和网络错误视为投递失败。失败后从 200 ms 开始
指数退避重试，直到 `max_retries` 用尽。失败会记录在 CubeAPI 日志中，但不会
回滚原始沙箱 API 请求。

## 可运行接收端

仓库内置了一个最小接收端：

```bash
cd examples/webhook-receiver
WEBHOOK_HOST=0.0.0.0 WEBHOOK_PORT=9000 WEBHOOK_SECRET=dev-secret \
  python3 receiver.py
```

接收端会把每个通过验签的回调打印成 JSON；设置 `WEBHOOK_SECRET` 后会校验
`X-Cube-Signature`。

## 本地 dev-env 验证

在 `dev-env` VM 流程中，CubeAPI 运行在 VM 内，receiver 运行在宿主机上。
VM 内访问宿主机 receiver 时使用 QEMU 网关地址 `10.0.2.2`：

```bash
export CUBE_API_WEBHOOKS='[{"url":"http://10.0.2.2:9000/webhook","events":["sandbox.created","sandbox.deleted","sandbox.paused","sandbox.resumed"],"secret":"dev-secret","timeout_secs":2,"max_retries":1}]'
```

重启 `cube-api` 后，创建沙箱并触发生命周期操作：

```bash
curl -X POST http://127.0.0.1:13000/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"templateID":"<template-id>","timeout":300}'

curl -X POST http://127.0.0.1:13000/sandboxes/<sandbox-id>/pause
curl -X POST http://127.0.0.1:13000/sandboxes/<sandbox-id>/resume \
  -H 'Content-Type: application/json' \
  -d '{"timeout":300}'
curl -X DELETE http://127.0.0.1:13000/sandboxes/<sandbox-id>
```

receiver 应能打印 `sandbox.created`、`sandbox.paused`、`sandbox.resumed`、
`sandbox.deleted` 四类事件。

## 告警集成参考

### 通用 HTTP 告警

大多数告警系统都支持 JSON `POST`。可以把 `url` 配置为告警系统的 ingest
地址，并只订阅需要告警的事件。如果告警系统要求特定 JSON 格式，建议运行
一个轻量 adapter：先接收 CubeAPI Webhook、完成验签，再把 Payload 转换后
转发给告警系统。

### 企业微信机器人

企业微信群机器人要求的 JSON 格式与 CubeAPI 事件 Payload 不同。建议使用
adapter：

1. 接收 CubeAPI Webhook 回调。
2. 使用配置的 `secret` 校验 `X-Cube-Signature`。
3. 生成企业微信消息，例如 `Sandbox c075... was deleted`。
4. POST 到企业微信群机器人地址。

转发给企业微信的 body 示例：

```json
{
  "msgtype": "text",
  "text": {
    "content": "CubeSandbox event sandbox.deleted for c07539a1d61a4544b0c075a9b236871a"
  }
}
```

除非机器人能直接接受 CubeAPI 的 Payload 和签名 Header，否则不要把企业微信
机器人地址直接配置成 `CUBE_API_WEBHOOKS.url`。
