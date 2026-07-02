# Webhook 事件通知

CubeAPI 可以把沙箱生命周期事件异步 POST 到一个或多个 Webhook 端点。首批支持事件：

| 事件 | 触发时机 |
|---|---|
| `sandbox.created` | `POST /sandboxes` 成功 |
| `sandbox.deleted` | `DELETE /sandboxes/{sandboxID}` 成功 |
| `sandbox.paused` | `POST /sandboxes/{sandboxID}/pause` 成功 |
| `sandbox.resumed` | `POST /sandboxes/{sandboxID}/resume` 成功 |

## 配置端点

设置 `CUBE_API_WEBHOOKS` 为 JSON 数组，然后重启 CubeAPI：

```bash
export CUBE_API_WEBHOOKS='[
  {
    "url": "http://127.0.0.1:9000/webhook",
    "events": ["sandbox.created", "sandbox.deleted", "sandbox.paused", "sandbox.resumed"],
    "secret": "change-me"
  }
]'
```

简单部署也可以使用逗号分隔的 URL 列表：

```bash
export CUBE_API_WEBHOOK_URLS='http://127.0.0.1:9000/webhook,http://127.0.0.1:9001/webhook'
export CUBE_API_WEBHOOK_EVENTS='sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed'
export CUBE_API_WEBHOOK_SECRET='change-me'
```

CubeAPI 只接受不包含内嵌用户名/密码的 `http` 和 `https` Webhook URL。使用 IP literal 且指向私网、链路本地、多播或未指定地址的 URL 会被拒绝；本地测试所需的 loopback 地址允许使用。配置签名密钥但仍使用 `http` 时会继续接受，但会记录告警日志。

可选投递参数：

| 变量 | 默认值 | 说明 |
|---|---:|---|
| `CUBE_API_WEBHOOK_QUEUE_CAPACITY` | `1024` | 事件队列容量 |
| `CUBE_API_WEBHOOK_REQUEST_TIMEOUT_SECS` | `5` | 单次 HTTP 请求超时 |
| `CUBE_API_WEBHOOK_MAX_ATTEMPTS` | `3` | 每个端点最大尝试次数；低于 `1` 时按 `1` 处理 |
| `CUBE_API_WEBHOOK_INITIAL_BACKOFF_MILLIS` | `200` | 指数退避的初始延迟 |

Webhook 投递不会阻塞沙箱 API 主路径。接收端慢或不可达时，CubeAPI 会在后台重试并记录投递失败日志。
投递语义是 best-effort、at-most-once：超过配置的重试次数后，CubeAPI 不会持久化事件，也不提供死信队列。

## Payload

CubeAPI 会发送结构化生命周期事件 JSON：

```json
{
  "timestamp": "2026-07-02T10:00:00Z",
  "level": "info",
  "event": "sandbox.created",
  "sandbox_id": "sb-123",
  "template_id": "tpl-123"
}
```

每个 payload 都包含 `event`、`timestamp`、`sandbox_id`。创建事件还会包含 `template_id`，后续可能增加更多上下文字段。

请求头：

| Header | 说明 |
|---|---|
| `X-Cube-Event` | 事件名 |
| `X-Cube-Delivery` | 唯一投递 ID |
| `X-Cube-Signature` | 配置 `secret` 时存在，格式为 `sha256=<hex>` |

## 验签示例

对原始请求体字节计算 HMAC-SHA256：

```python
import hashlib
import hmac

def valid(body: bytes, header: str, secret: str) -> bool:
    expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, header)
```

## 本地接收端

可运行示例位于 `examples/webhook-receiver/`：

```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=change-me python3 receiver.py
```

把接收端地址写入 `CUBE_API_WEBHOOKS`，重启 CubeAPI，然后创建、暂停、恢复或销毁沙箱，接收端会打印收到的 JSON。
示例默认监听 `127.0.0.1:9000`；只有在明确需要暴露到其他网卡时才设置 `WEBHOOK_HOST`。

## 企业微信或通用 HTTP 告警

建议使用一个轻量接收服务作为 CubeAPI 与下游工具之间的桥接层。示例接收端在设置 `WECOM_BOT_WEBHOOK` 后会转发到企业微信群机器人。对接其他 HTTP 告警系统时，保留验签步骤，然后把 Cube payload 转换成目标系统需要的格式再转发。
