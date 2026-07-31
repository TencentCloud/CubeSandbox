---
title: Webhook 事件通知
---

# Webhook 事件通知

CubeAPI 可在沙箱生命周期操作成功后，异步向 HTTP 端点发送事件通知。投递在 API
请求主路径之外执行，因此接收端缓慢或不可达不会导致沙箱生命周期请求失败。

## 配置端点

启动 CubeAPI 前设置 `CUBE_API_WEBHOOK_ENDPOINTS`，其值为 JSON 数组：

```bash
export CUBE_API_WEBHOOK_ENDPOINTS='[
  {
    "name": "automation",
    "url": "https://automation.example.com/cube-events",
    "events": ["sandbox.created", "sandbox.deleted"],
    "secret": "replace-with-a-random-secret",
    "timeout_secs": 5,
    "max_retries": 4
  },
  {
    "name": "monitoring",
    "url": "https://monitoring.example.com/events",
    "events": ["*"]
  }
]'
```

`events` 支持准确事件名，或使用 `*` 订阅全部事件。`secret` 可选；
`timeout_secs` 默认 5 秒；`max_retries` 默认在首次投递后再重试 4 次。端点名称必须唯一。

全局控制项：

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `CUBE_API_WEBHOOK_QUEUE_CAPACITY` | `1024` | 等待投递的最大任务数 |
| `CUBE_API_WEBHOOK_MAX_CONCURRENCY` | `16` | 同时进行的最大 HTTP 请求数 |

端点 JSON、URL 或事件名无效时，CubeAPI 会拒绝启动。

## 事件与 Payload

| 事件 | 触发时机 | 附加字段 |
| --- | --- | --- |
| `sandbox.created` | 沙箱创建成功 | `template_id` |
| `sandbox.deleted` | 沙箱删除成功 | 无 |
| `sandbox.paused` | 沙箱暂停成功 | 无 |
| `sandbox.resumed` | 沙箱恢复成功 | 无 |

每个 Payload 至少包含 `id`、`event`、`timestamp` 和 `sandbox_id`：

```json
{
  "id": "a79a9a61-7330-49cf-8108-c14347f4b94e",
  "event": "sandbox.created",
  "timestamp": "2026-07-31T10:30:00.123Z",
  "sandbox_id": "sandbox-123",
  "template_id": "template-456"
}
```

由于失败重试可能重复投递同一个事件，接收端应使用 `id` 作为幂等键。

## 验证签名

端点配置 `secret` 后，CubeAPI 会发送以下 Header：

```text
X-Cube-Webhook-Id: <事件 UUID>
X-Cube-Webhook-Timestamp: <Unix 时间戳>
X-Cube-Webhook-Signature: sha256=<十六进制摘要>
```

待签名内容为 UTF-8 时间戳、一个英文句点以及原始请求体。验签前不要解析并重新序列化请求体。

```python
import hashlib, hmac, time

timestamp = request.headers["X-Cube-Webhook-Timestamp"]
signature = request.headers["X-Cube-Webhook-Signature"]
if abs(time.time() - int(timestamp)) > 300:
    raise ValueError("stale webhook")
expected = "sha256=" + hmac.new(
    secret.encode(), timestamp.encode() + b"." + request.body, hashlib.sha256
).hexdigest()
if not hmac.compare_digest(expected, signature):
    raise ValueError("invalid signature")
```

## 投递与重试

HTTP 408、429、5xx、请求超时和连接错误会按指数退避和抖动策略重试；其他
4xx 响应被视为永久失败。CubeAPI 日志会记录每次失败和最终失败，但不会输出签名密钥。

队列保存在内存中。CubeAPI 重启会丢失尚未投递的事件；有界队列满时会丢弃新任务并记录告警。
当前仅覆盖由 CubeAPI Handler 成功处理的生命周期操作，绕过这些 Handler 的内部状态变化不会触发通知。
优雅关闭期间，CubeAPI 最多等待 10 秒完成待投递任务；超时后会记录告警并继续关闭。

## 本地示例与企业告警

[`examples/webhook-receiver`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver) 提供不依赖第三方库的
Python 接收端，能够打印事件并验证签名：

```bash
WEBHOOK_SECRET=development-secret python3 examples/webhook-receiver/receiver.py
```

企业微信机器人要求特定的消息体格式，不能直接接收 CubeSandbox Payload。可以让一个类似示例的
中转服务先验签，再将事件转换成企业微信所需的 `msgtype` Payload，并转发到机器人 URL。这样既能
保持 Webhook 协议通用，也能避免将第三方凭证存入 CubeAPI。
