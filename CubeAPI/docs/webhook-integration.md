# CubeSandbox Webhook 集成说明

## 概述

Webhook 是 CubeSandbox 的事件通知机制。当沙箱生命周期事件发生时（创建、删除、暂停、恢复），CubeAPI 会向用户配置的 HTTP 端点异步发送 JSON 格式的 POST 请求。

## 配置方法

Webhook 通过环境变量配置，在启动 `cube-api` 前设置即可：

| 环境变量 | 说明 | 默认值 |
|---|---|---|
| `WEBHOOK_ENDPOINTS` | 接收端点 URL，多个用逗号分隔 | 无（禁用） |
| `WEBHOOK_EVENTS` | 订阅的事件类型，逗号分隔 | 全部 4 种 |
| `WEBHOOK_SECRET` | HMAC-SHA256 签名密钥 | 无（不签名） |
| `WEBHOOK_RETRY_MAX` | 最大重试次数 | 3 |
| `WEBHOOK_RETRY_BASE_MS` | 重试基础间隔（毫秒，指数退避） | 1000 |

### 示例配置

```bash
# 基础配置
export WEBHOOK_ENDPOINTS="https://my-service.example.com/webhook"

# 多端点
export WEBHOOK_ENDPOINTS="https://hooks.example.com/primary,https://hooks.example.com/backup"

# 仅订阅创建和删除事件
export WEBHOOK_EVENTS="sandbox.created,sandbox.deleted"

# 启用签名验证
export WEBHOOK_SECRET="your-shared-secret"
```

配置完成后重启 `cube-api` 服务。

## 支持的事件类型

| 事件类型 | 触发时机 | 包含 template_id |
|---|---|---|
| `sandbox.created` | 沙箱创建成功 | 是 |
| `sandbox.deleted` | 沙箱销毁 | 否 |
| `sandbox.paused` | 沙箱暂停 | 否 |
| `sandbox.resumed` | 沙箱恢复 | 否 |

## Payload 格式

所有 Webhook 请求为 `POST`，`Content-Type: application/json`：

```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-27T08:30:00.123456Z",
  "sandbox_id": "sb-abc123def456",
  "template_id": "tpl-python-3.11"
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `event` | string | 事件类型名称 |
| `timestamp` | string | ISO-8601 UTC 时间戳 |
| `sandbox_id` | string | 触发事件的沙箱 ID |
| `template_id` | string? | 关联模板 ID（仅 sandbox.created 携带） |

## 签名验证

启用 `WEBHOOK_SECRET` 后，每个 Webhook 请求会携带 `X-Cube-Webhook-Signature` 头部，其值为请求体（raw bytes）的 HMAC-SHA256 十六进制编码结果。

### 验签示例

**Python:**
```python
import hmac, hashlib
def verify_signature(body, signature, secret):
    expected = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)
```

**Node.js:**
```javascript
const crypto = require("crypto");
function verifySignature(body, signature, secret) {
  const expected = crypto.createHmac("sha256", secret).update(body).digest("hex");
  return crypto.timingSafeEqual(
    Buffer.from(expected, "hex"), Buffer.from(signature, "hex")
  );
}
```

**Go:**
```go
import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
)
func verifySignature(body []byte, signature, secret string) bool {
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(body)
    expected := hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(expected), []byte(signature))
}
```

## 投递保证与重试

- 投递是**异步非阻塞**的，不会影响沙箱 API 的正常响应
- 投递失败时自动重试，使用**指数退避**策略：1s -> 2s -> 4s（可配置）
- 超过最大重试次数后放弃，错误详情记录在 cube-api 日志中
- 每个端点独立重试，一个端点失败不影响其他端点

## 对接企业微信机器人

参见 `examples/webhook-receiver/README.md` 中的 WeCom 桥接示例。

## 对接通用 HTTP 告警

任何能接收 HTTP POST 的系统都可以作为 Webhook 接收端，只需实现：
1. 一个 POST 端点接收 JSON
2. （可选）验证 X-Cube-Webhook-Signature 签名
3. 返回 2xx 状态码表示接收成功

## 验证清单

1. 配置 WEBHOOK_ENDPOINTS 并重启 cube-api
2. 创建沙箱 -> 接收端收到 sandbox.created（含 template_id）
3. 暂停沙箱 -> 接收端收到 sandbox.paused
4. 恢复沙箱 -> 接收端收到 sandbox.resumed
5. 删除沙箱 -> 接收端收到 sandbox.deleted
6. 接收端不可达时，沙箱 API 仍正常返回（非阻塞验证）
7. 配置 WEBHOOK_SECRET 后，接收端验签通过
8. 查看 cube-api 日志确认重试行为正常
