---
title: Webhook 集成指南
author: YYYSSSRRR
date: 2026-07-24
tags:
  - integration
  - webhook
  - event
  - callback
lang: zh-CN
---

# Webhook 集成指南

[English](../../guide/integrations/webhook.md)

Cube Sandbox 支持通过 **Webhook** 将沙箱生命周期事件实时推送到任意 HTTP 端点。本文档涵盖配置方法、事件载荷格式、签名验证，以及如何对接企业微信机器人、Slack、PagerDuty 等通知服务。

可运行的接收端示例位于 [`examples/webhook-receiver/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)。

---

## 集成目标与版本

| 组件 | 版本 |
|---|---|
| CubeAPI（Webhook 发送端） | `>= 0.5.0` |
| 接收端示例 | Python 3.10+（仅标准库） |
| Webhook 协议 | HTTP POST，支持 HMAC-SHA256 签名 |

---

## 配置方法

通过 CubeAPI 进程的环境变量配置 Webhook。

### 基本设置

```bash
# 目标 URL（逗号分隔可配置多个端点）
export CUBE_API_WEBHOOK_URLS="http://localhost:8080/webhook"

# 事件过滤器："*" 表示全部，或逗号分隔的事件名列表
export CUBE_API_WEBHOOK_EVENTS="*"

# 可选：共享密钥，用于载荷签名
export CUBE_API_WEBHOOK_SECRET="my-shared-secret"
```

### 事件过滤

只接收沙箱生命周期事件：

```bash
export CUBE_API_WEBHOOK_EVENTS="sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed"
```

### 多目标投递

事件会并行推送至所有配置的 URL：

```bash
export CUBE_API_WEBHOOK_URLS="http://host-a:8080/hook,http://host-b:8080/hook"
```

### 投递行为

- **非阻塞**：Webhook 在后台 Tokio 任务中发送，不会阻塞 API 响应。
- **重试**：指数退避 200 毫秒 → 500 毫秒 → 1 秒（最多 3 次）。
- **成功条件**：HTTP 2xx 视为成功。
- **失败处理**：非 2xx 状态码或网络错误会触发重试；3 次均失败后丢弃事件并记录错误日志。

---

## 事件载荷

### 格式

每个 Webhook POST 请求体是一个 JSON 对象，包含以下字段：

| 字段 | 类型 | 始终存在 | 说明 |
|---|---|---|---|
| `event` | string | ✓ | 机器可读的事件名称 |
| `timestamp` | string (ISO 8601) | ✓ | 事件发生时间 |
| `sandbox_id` | string | — | 沙箱 ID（API 级别事件不含此字段） |
| `template_id` | string | — | 模板 ID |
| (其他) | varies | — | 事件特定字段，平铺在根对象中 |

### 事件类型

| 事件 | 触发时机 | 额外字段 |
|---|---|---|
| `sandbox.created` | 沙箱创建成功 | — |
| `sandbox.deleted` | 沙箱已删除 | — |
| `sandbox.paused` | 沙箱已暂停 | — |
| `sandbox.resumed` | 沙箱已恢复 | — |
| `sandbox.timeout.updated` | 沙箱超时时间已修改 | `timeout`（秒） |
| `sandbox.refreshed` | 沙箱 TTL 已刷新 | `duration`（秒） |
| `api.response` | API 请求完成 | — |
| `api.error` | API 处理错误 | `handler`, `error` |

### 示例

**sandbox.created**：
```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-24T12:00:00Z",
  "sandbox_id": "sb-abc123",
  "template_id": "tpl-python-3.12"
}
```

**sandbox.timeout.updated**（含平铺的额外字段）：
```json
{
  "event": "sandbox.timeout.updated",
  "timestamp": "2026-07-24T12:05:00Z",
  "sandbox_id": "sb-abc123",
  "timeout": 3600
}
```

**api.error**：
```json
{
  "event": "api.error",
  "timestamp": "2026-07-24T12:10:00Z",
  "handler": "sandboxes.create",
  "error": "template not found"
}
```

---

## 签名验证

当配置了 `CUBE_API_WEBHOOK_SECRET` 后，每个 POST 请求包含：

```
X-Cube-Signature-256: sha256=<十六进制 HMAC>
```

HMAC 使用 **HMAC-SHA256** 对原始 JSON 请求体计算，密钥为共享密钥。

### 验证示例

**Python**（与接收端示例完全兼容）：
```python
import hmac

def verify(body: bytes, header: str, secret: str) -> bool:
    if not header.startswith("sha256="):
        return False
    expected = header[len("sha256="):]
    computed = hmac.new(secret.encode("utf-8"), body, "sha256").hexdigest()
    return hmac.compare_digest(computed, expected)
```

**Node.js / TypeScript**：
```typescript
import { createHmac, timingSafeEqual } from "crypto";

function verify(body: Buffer, header: string, secret: string): boolean {
  if (!header.startsWith("sha256=")) return false;
  const expected = header.slice(7);
  const computed = createHmac("sha256", secret).update(body).digest("hex");
  if (computed.length !== expected.length) return false;
  return timingSafeEqual(Buffer.from(computed), Buffer.from(expected));
}
```

**Go**：
```go
import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "strings"
)

func Verify(body []byte, header, secret string) bool {
    if !strings.HasPrefix(header, "sha256=") {
        return false
    }
    expected := header[7:]
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(body)
    computed := hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(computed), []byte(expected))
}
```

### 使用 cURL 测试签名

```bash
BODY='{"event":"sandbox.created","timestamp":"2026-07-24T12:00:00Z","sandbox_id":"sb-test"}'
SIG=$(echo -n "$BODY" | openssl dgst -sha256 -hmac "my-secret" | awk '{print $NF}')

curl -X POST http://localhost:8080/webhook \
  -H "Content-Type: application/json" \
  -H "X-Cube-Signature-256: sha256=$SIG" \
  -d "$BODY"
```

---

## 接收 Webhook

项目提供了零依赖的 Python 接收端，位于 [`examples/webhook-receiver/receiver.py`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)。

```bash
# 在 CubeAPI 服务器上启动接收端
python receiver.py --port 8080 --path /webhook

# 开启签名验证
WEBHOOK_SECRET=my-shared-secret python receiver.py
```

接收端支持彩色输出和可选的 HMAC 签名验证。完整的端到端流程参见[示例 README](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)。

---

## 对接通知服务

### 企业微信机器人

企业微信群机器人接收固定格式的 JSON 请求。

**转发示例（Python）**：
```python
import json
from http.server import HTTPServer, BaseHTTPRequestHandler
import urllib.request

WECOM_URL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=你的KEY"

class WeComForwarder(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        payload = json.loads(body)

        event = payload.get("event", "unknown")
        sandbox_id = payload.get("sandbox_id", "N/A")

        msg = {
            "msgtype": "markdown",
            "markdown": {
                "content": (
                    f"### Cube Sandbox 事件：{event}\n"
                    f"> **沙箱：** {sandbox_id}\n"
                    f"> **时间：** {payload.get('timestamp', 'N/A')}\n"
                )
            }
        }

        req = urllib.request.Request(
            WECOM_URL, data=json.dumps(msg).encode(),
            headers={"Content-Type": "application/json"}, method="POST",
        )
        urllib.request.urlopen(req)
        self.send_response(200)
        self.end_headers()

HTTPServer(("0.0.0.0", 8080), WeComForwarder).serve_forever()
```

### Slack Webhook

Slack Incoming Webhooks 接收简单的 JSON 消息：

```python
import json
import urllib.request

SLACK_URL = "https://hooks.slack.com/services/T000000/B000000/xxxxxxxx"

def send_to_slack(payload: dict):
    msg = {
        "text": (
            f"*Cube Sandbox 事件：* {payload['event']}\n"
            f"• 沙箱：`{payload.get('sandbox_id', 'N/A')}`\n"
            f"• 时间：{payload.get('timestamp', 'N/A')}"
        )
    }
    req = urllib.request.Request(
        SLACK_URL, data=json.dumps(msg).encode(),
        headers={"Content-Type": "application/json"}, method="POST",
    )
    urllib.request.urlopen(req)
```

### PagerDuty Events API v2

```python
import json
import urllib.request

PD_ROUTING_KEY = "你的 PagerDuty Routing Key"

def send_to_pagerduty(payload: dict):
    event_name = payload["event"]
    severity = "error" if event_name == "api.error" else "info"
    msg = {
        "routing_key": PD_ROUTING_KEY,
        "event_action": "trigger",
        "payload": {
            "summary": f"Cube Sandbox: {event_name}",
            "severity": severity,
            "source": "cube-api",
            "custom_details": payload,
        },
    }
    req = urllib.request.Request(
        "https://events.pagerduty.com/v2/enqueue",
        data=json.dumps(msg).encode(),
        headers={"Content-Type": "application/json"}, method="POST",
    )
    urllib.request.urlopen(req)
```

---

## 生产环境建议

- **幂等处理**：Webhook 重试可能导致同一条事件投递多次。接收端应处理好重复事件（例如按 `event` + `timestamp` 去重）。
- **速率限制**：CubeAPI 不会限制 Webhook 的发送速率。如果接收端调用外部限速 API，应添加队列或限速器。
- **监控**：CubeAPI 通过 `tracing` 记录投递失败日志（`WARN` 表示重试，`ERROR` 表示重试耗尽）。请将这类日志接入监控系统。
- **性能**：本文的接收端示例是单线程的。生产环境高吞吐场景建议使用专用 Webhook 框架（FastAPI、Flask 或消息队列消费者）。
- **安全**：生产环境中务必启用 `CUBE_API_WEBHOOK_SECRET`，防止伪造事件。

---

## 参考

- [CubeAPI Webhook 源码](https://github.com/TencentCloud/CubeSandbox/tree/master/CubeAPI/src/logging/http.rs)
- [Webhook 接收端示例](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)
- [配置参考](https://github.com/TencentCloud/CubeSandbox/tree/master/CubeAPI/src/config/mod.rs)
- [企业微信机器人文档](https://developer.work.weixin.qq.com/document/path/91770)
- [Slack Incoming Webhooks](https://api.slack.com/messaging/webhooks)
- [PagerDuty Events API v2](https://developer.pagerduty.com/docs/events-api-v2/overview)