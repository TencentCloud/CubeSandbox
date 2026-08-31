---
title: Webhook 集成指南
author: YYYSSSRRR
date: 2026-08-31
tags:
  - integration
  - webhook
  - event
  - callback
lang: zh-CN
---

# Webhook 集成指南

[English](../../guide/integrations/webhook.md)

Cube Sandbox 可以通过 **webhook** 把沙箱生命周期事件推送到任意 HTTP 端点。本
指南涵盖配置方法、Payload 格式、签名验证、投递语义，以及如何对接企业微信机器
人或通用 HTTP 告警端点。

可运行的接收端示例见
[`examples/webhook-receiver/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)。

---

## 集成目标与版本

| 组件 | 版本 |
|---|---|
| cube-lifecycle-manager（webhook 发射端） | `>= v0.6.0` |
| 接收端示例 | Python 3.10+（仅标准库） |
| Webhook 协议 | HTTP POST + HMAC-SHA256 签名 |

线协议与 **CubeAPI 的 webhook 发射端完全一致**，为其中一方写的接收端可对另一
方原样使用。

---

## 工作原理

`cube-lifecycle-manager`（CLM）服务本身就在消费生命周期 Redis 流
（`cube:v1:shared:sandbox:lifecycle:events`）以执行自动暂停/恢复。Webhook 投递
是**同一事件流上的独立消费者**，使用自己的消费组（`cube-webhook-delivery`）。
因此事件具备：

- **持久化**：事件在投递并确认前一直存于 Redis，CLM 崩溃也不会丢失。
- **非阻塞**：webhook 投递绝不影响沙箱创建/销毁（主生命周期消费组是独立的）。
- **来自真实状态迁移**：CubeMaster 发布的每一次暂停/恢复/删除，无论是平台主动
  （空闲自动暂停、超时销毁）还是用户主动（SDK connect / API pause / delete），
  都会成为 webhook 事件。

---

## 配置方法

Webhook 通过 **CLM 进程**的环境变量配置。

### 基础配置

```bash
# 目标 URL —— 逗号分隔可配多个端点
export CUBE_LCM_WEBHOOK_URLS="http://your-receiver:8081/webhook"

# 事件过滤 —— "*" 表示全部，或用逗号分隔的事件列表
export CUBE_LCM_WEBHOOK_EVENTS="*"

# 可选：用于 payload 签名的共享密钥
export CUBE_LCM_WEBHOOK_SECRET="my-shared-secret"
```

当 `CUBE_LCM_WEBHOOK_URLS` 为空时，webhook 投递**关闭**。

### 调优参数

| 变量 | 默认 | 含义 |
|---|---|---|
| `CUBE_LCM_WEBHOOK_TIMEOUT` | `10s` | 每次请求的 HTTP 超时 |
| `CUBE_LCM_WEBHOOK_MAX_RETRIES` | `2` | 首次尝试之外的重试次数（共 3 次） |

### 运行时端点管理（可选 REST API）

端点可通过 CLM 管理 API 在运行时增删改。所有 `/admin/webhooks*` 路由要求
`X-Cube-Admin-Token` 请求头等于 `CUBE_LCM_ADMIN_TOKEN`：

```
GET    /admin/webhooks             # 列出端点
POST   /admin/webhooks             # 新增（body: {"url","events","secret","enabled"}）
PUT    /admin/webhooks/{id}        # 更新
DELETE /admin/webhooks/{id}        # 删除
GET    /admin/webhooks/stats       # dropped / delivered / failed 计数
```

```bash
curl -H "X-Cube-Admin-Token: $CUBE_LCM_ADMIN_TOKEN" \
     -d '{"url":"http://host-b:8080/hook","events":["sandbox.paused","sandbox.resumed"]}' \
     http://127.0.0.1:8083/admin/webhooks
```

---

## 事件类型

| 事件 | 触发时机 | 额外字段 |
|---|---|---|
| `sandbox.created` | 沙箱创建成功 | — |
| `sandbox.deleted` | 沙箱删除（用户或超时销毁） | — |
| `sandbox.paused` | 暂停完成（空闲自动暂停或手动） | `state`、`actor`、`source` |
| `sandbox.resumed` | 恢复完成（自动恢复或手动） | `state`、`actor`、`source` |
| `sandbox.timeout.updated` | 空闲超时被刷新（`set_timeout`） | `timeout`（秒） |

---

## Payload

每次投递是一个 JSON 对象，字段如下：

| 字段 | 类型 | 必有 | 说明 |
|---|---|---|---|
| `event` | string | ✓ | 机器可读的事件名 |
| `event_id` | string | ✓ | 稳定 ID（= Redis 流条目 ID）；**用于去重** |
| `timestamp` | string | ✓ | 事件生成时间（ISO 8601，UTC） |
| `sandbox_id` | string | ✓ | 沙箱标识 |
| `template_id` | string | — | 沙箱启动所用的模板（best-effort） |
| `host_id` / `host_ip` | string | — | 沙箱所在节点 |
| `instance_type` | string | — | 运行时实例类型（如 `cubebox`） |
| `timeout_seconds` | int | — | 空闲超时秒数 |
| `auto_pause` / `auto_resume` | bool | — | 生命周期开关 |
| `created_at` / `end_at` | int | — | Unix 毫秒时间戳 |
| （state 事件） | — | — | `state`、`actor`、`source` 扁平化在根对象 |
| （timeout.updated） | — | — | `timeout`（秒）扁平化在根对象 |

上下文字段缺失时**省略**（而非 `null`）。

### 示例 — sandbox.created

```json
{
  "event": "sandbox.created",
  "event_id": "1725030000000-0",
  "timestamp": "2026-07-24T12:00:00Z",
  "sandbox_id": "sb-abc123",
  "template_id": "tpl-python-3.12",
  "instance_type": "cubebox",
  "timeout_seconds": 300,
  "auto_pause": true,
  "auto_resume": true,
  "created_at": 1784966400000,
  "end_at": 1784966700000
}
```

### 示例 — sandbox.paused

```json
{
  "event": "sandbox.paused",
  "event_id": "1725030100000-0",
  "timestamp": "2026-07-24T12:01:40Z",
  "sandbox_id": "sb-abc123",
  "template_id": "tpl-python-3.12",
  "state": "paused",
  "actor": "cubemaster",
  "source": "api"
}
```

---

## 签名验证

设置 `CUBE_LCM_WEBHOOK_SECRET` 后，每个请求都会携带：

```
X-Cube-Signature-256: sha256=<hex>
```

其中 `<hex>` 是**对原始请求体做 HMAC-SHA256**（以 secret 的 UTF-8 字节为密钥）
得到的小写 hex。请对从网络上读到的**原始 body 字节**做校验。未配置 secret 的
接收端跳过验证。

### Python

```python
import hmac, hashlib

def verify(body: bytes, header: str, secret: str) -> bool:
    if not header.startswith("sha256="):
        return False
    expected = header[len("sha256="):]
    computed = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(computed, expected)
```

### Go

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
    expected := header[len("sha256="):]
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(body)
    computed := hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(computed), []byte(expected))
}
```

### curl / openssl

```bash
BODY='{"event":"sandbox.paused","event_id":"1725030100000-0","timestamp":"2026-07-24T12:01:40Z","sandbox_id":"sb-abc123"}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "my-shared-secret" | awk '{print $2}')
curl -X POST http://your-receiver/webhook \
     -H "Content-Type: application/json" \
     -H "X-Cube-Signature-256: sha256=$SIG" \
     -d "$BODY"
```

---

## 投递语义

- **异步且不阻塞**：投递运行在独立的消费组上，绝不影响沙箱创建/销毁或 CLM 的
  主对账循环。
- **重试**：每次投递最多尝试 `CUBE_LCM_WEBHOOK_MAX_RETRIES + 1` 次（默认 3 次），
  指数退避（200ms → 400ms → … 上限 1s）。任意 HTTP `2xx` 停止重试；其余都重试。
  预算耗尽后事件被 ack 并 drop（记录错误日志）。
- **At-least-once**：事件在投递结果确定后才 ack，因此 CLM 在"读取到 ack"之间
  崩溃会重投。**接收端必须按 `event_id` 去重。**
- **崩溃恢复**：启动时 CLM 会排空该消费组的 pending 列表；另有周期安全网捞回
  滞留在 pending 中的条目（如死副本、失败的 ack）并重试。因此接收端长期不可达
  时会看到重复的（可去重的）投递尝试，直到事件被丢弃。

---

## 对接告警：企业微信机器人与通用 HTTP

CLM 把原始生命周期 JSON 投递到你配置的 URL。要转成告警，需要一个小的 relay：
（a）校验签名，（b）按下游系统重排 payload。

### 通用 HTTP 告警（Slack、PagerDuty、自建 API）

把 `CUBE_LCM_WEBHOOK_URLS` 指向一个转发到你告警 API 的 relay。示例接收端的
签名校验可以直接复用；重排步骤因服务而异。

### 企业微信机器人

企业微信群机器人的 webhook 要求不同的请求体（`{"msgtype":"markdown",...}`），
且不校验我们的签名，因此链路是 **CLM → relay → 企业微信**。最小 relay：

```python
#!/usr/bin/env python3
"""wecom-relay.py — 校验 CLM webhook、重排格式、转发到企业微信机器人。"""
import hashlib, hmac, json, urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

SECRET = "my-shared-secret"   # 必须与 CUBE_LCM_WEBHOOK_SECRET 一致
WECOM_URL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_BOT_KEY"

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):  # 静默
        pass

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        sig = self.headers.get("X-Cube-Signature-256", "").removeprefix("sha256=")
        expect = hmac.new(SECRET.encode(), body, hashlib.sha256).hexdigest()
        if sig != expect:
            self.send_response(401); self.end_headers(); return
        ev = json.loads(body)
        content = (
            f"**{ev.get('event')}**\n"
            f"> sandbox: `{ev.get('sandbox_id', '-')}`\n"
            f"> template: `{ev.get('template_id', '-')}`\n"
            f"> time: `{ev.get('timestamp')}`"
        )
        data = json.dumps({"msgtype": "markdown", "markdown": {"content": content}}).encode()
        req = urllib.request.Request(WECOM_URL, data=data,
                                     headers={"Content-Type": "application/json"})
        urllib.request.urlopen(req)
        self.send_response(200); self.end_headers()

    do_GET = do_POST

HTTPServer(("0.0.0.0", 8082), Handler).serve_forever()
```

然后：

```bash
export CUBE_LCM_WEBHOOK_URLS="http://127.0.0.1:8082/"   # 指向 relay，不是企业微信
export CUBE_LCM_WEBHOOK_SECRET="my-shared-secret"
python wecom-relay.py
```

---

## 注意事项

- **务必按 `event_id` 去重。** 投递是 at-least-once，CLM 崩溃或重启后你会看到
  重复事件。
- **CLM 停机期间的事件不会补发。** webhook 投递在消费组创建时开始（只收新事件），
  不重放历史，也不补发停机期间发生的事件。
- **`sandbox.deleted` 在流上不带 payload**，`template_id` 从 CLM 内存 meta 缓存
  恢复，若重启后错过了该沙箱的 create 事件则可能缺失（best-effort）。
- **重试预算耗尽后事件被丢弃。** 接收端长时间不可达时，事件最终会停止重试。
- **顺序是 best-effort**（各端点并发投递）。
- 生产环境务必设置 `CUBE_LCM_WEBHOOK_SECRET`，并优先使用 HTTPS 目标 URL——
  HMAC 保证完整性，不保证传输安全。

---

## 参考资料

- 可运行接收端：[`examples/webhook-receiver/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)
- CLM 配置：`cube-lifecycle-manager/internal/config/config.go`
- [沙箱生命周期指南](../lifecycle.md)