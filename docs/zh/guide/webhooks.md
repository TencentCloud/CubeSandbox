# Webhook 事件通知

CubeAPI 支持把沙箱生命周期事件投递到一个或多个 HTTP 端点。投递在后台任务中执行，不会阻塞沙箱创建、删除、暂停或恢复请求。

## 支持的事件

| 事件 | 触发时机 | 字段 |
|---|---|---|
| `sandbox.created` | 沙箱创建成功 | `sandbox_id`, `template_id` |
| `sandbox.deleted` | 沙箱删除成功 | `sandbox_id` |
| `sandbox.paused` | 沙箱暂停成功 | `sandbox_id` |
| `sandbox.resumed` | 沙箱恢复成功 | `sandbox_id` |

## 快速验证

启动示例接收端：

```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=change-me python3 receiver.py --host 127.0.0.1 --port 9000
```

配置 CubeAPI：

```bash
export CUBE_API_WEBHOOK_URLS=http://127.0.0.1:9000/webhook
export CUBE_API_WEBHOOK_SECRET=change-me
export CUBE_API_WEBHOOK_EVENTS=sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed
sudo systemctl restart cube-sandbox-cube-api.service
```

随后通过 API 创建、暂停、恢复或删除沙箱。接收端会为每个回调打印一行 JSON。

## 配置方式

简单部署可以使用逗号分隔的 URL 列表：

```bash
export CUBE_API_WEBHOOK_URLS=http://127.0.0.1:9000/webhook,http://127.0.0.1:9001/webhook
export CUBE_API_WEBHOOK_EVENTS=sandbox.created,sandbox.deleted
export CUBE_API_WEBHOOK_SECRET=change-me
export CUBE_API_WEBHOOK_MAX_RETRIES=3
export CUBE_API_WEBHOOK_TIMEOUT_SECS=5
export CUBE_API_WEBHOOK_RETRY_INITIAL_DELAY_MS=200
```

如果不同端点需要不同订阅事件或密钥，可以使用 `CUBE_API_WEBHOOKS_JSON`：

```bash
export CUBE_API_WEBHOOKS_JSON='[
  {
    "url": "http://127.0.0.1:9000/webhook",
    "events": ["sandbox.created", "sandbox.deleted"],
    "secret": "ops-secret",
    "max_retries": 3,
    "timeout_secs": 5,
    "retry_initial_delay_ms": 200
  },
  {
    "url": "https://alert.example.com/cube",
    "events": ["sandbox.paused", "sandbox.resumed"]
  }
]'
```

`events` 也可以配置为 `"*"`，表示订阅 HTTP 后端收到的所有结构化事件。

## Payload 与请求头

CubeAPI 会发送 JSON POST：

```json
{
  "timestamp": "2026-07-09T12:34:56.789Z",
  "level": "info",
  "event": "sandbox.created",
  "sandbox_id": "sb-123",
  "template_id": "tpl-abc"
}
```

请求头如下：

| 请求头 | 说明 |
|---|---|
| `Content-Type: application/json` | JSON 请求体 |
| `X-Cube-Event` | 事件名 |
| `X-Cube-Timestamp` | 事件时间 |
| `X-Cube-Signature-256` | 配置 `secret` 后才会发送 |

## 签名验证

配置 `secret` 后，CubeAPI 会对原始请求体计算 HMAC-SHA256，并发送：

```text
X-Cube-Signature-256: sha256=<hex digest>
```

Python 验签示例：

```python
import hashlib
import hmac

def verify(secret: str, body: bytes, header: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, header)
```

接收端应先用原始 body 字节验签，再解析 JSON。

## 投递与重试

`log()` 只负责把事件放入队列并立即返回。后台任务会把事件投递给已订阅的端点。接收端超时、不可达或返回非 2xx 状态时，CubeAPI 使用指数退避重试：

```text
retry_initial_delay_ms, retry_initial_delay_ms * 2, retry_initial_delay_ms * 4, ...
```

失败尝试和最终投递失败会记录到 CubeAPI 日志。

## 企业微信与通用告警

如果告警系统能直接接收任意 JSON，可以把 `url` 配成告警端点，并按需启用 HMAC 验签。

企业微信群机器人要求消息体是固定的消息格式，不能直接消费 CubeAPI 原始事件。推荐加一个轻量适配服务：

1. 接收 CubeAPI Webhook。
2. 验证 `X-Cube-Signature-256`。
3. 把事件转换为企业微信 `markdown` 或 `text` 消息。
4. 把转换后的消息 POST 到企业微信群机器人 URL。

这样 CubeAPI 的 Webhook 载荷保持稳定，团队仍可按自己的规则定制告警文案、路由和限流。
