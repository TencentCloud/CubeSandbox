# Webhook Receiver Example

用于接收和验证 CubeAPI Webhook 事件的示例 HTTP 服务。

## 快速启动

```bash
cd examples/webhook-receiver

# 不带签名验证
cargo run

# 带 HMAC 签名验证
WEBHOOK_SECRET=test-secret cargo run
```

服务监听 `http://127.0.0.1:9090`。端口可通过 `PORT` 环境变量覆盖，监听地址可通过 `LISTEN` 覆盖。

## 独立验证（不需要 CubeMaster）

**终端 1** — 启动接收端：
```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=test-secret cargo run
```

**终端 2** — 模拟 CubeAPI 发送 Webhook：
```bash
# 计算签名
BODY='{"event":"sandbox.created","timestamp":"2026-07-20T12:00:00.123Z","sandbox_id":"sb-abc123","template_id":"tpl-xyz"}'
SIG="sha256=$(echo -n "$BODY" | openssl dgst -sha256 -hmac "test-secret" | cut -d' ' -f2)"

# 发送 webhook（签名正确 → 终端 1 打印 body）
curl -X POST http://127.0.0.1:9090/webhook \
  -H "Content-Type: application/json" \
  -H "X-Cube-Event: sandbox.created" \
  -H "X-Cube-Delivery: $(uuidgen 2>/dev/null || echo test-001)" \
  -H "X-Cube-Signature: $SIG" \
  -d "$BODY"

# 发送签名错误的请求（终端 1 应返回 401）
curl -X POST http://127.0.0.1:9090/webhook \
  -H "X-Cube-Signature: sha256=wrong" \
  -d "$BODY"
```

## 验证 CubeAPI Webhook 投递（需要 CubeAPI）

参考 `docs/guide/webhooks.md` 中的"本地验证"部分。

## 环境变量

| 变量 | 说明 | 默认 |
|------|------|------|
| `WEBHOOK_SECRET` | HMAC-SHA256 共享密钥 (空=不验证) | `""` |
| `WECOM_WEBHOOK_URL` | 企业微信群机器人 Webhook URL (空=不转发) | `""` |
| `PORT` | 监听端口 | `9090` |
| `LISTEN` | 监听地址 | `127.0.0.1` |

## 与 CubeAPI 集成

1. 确保 CubeMaster 可达
2. 启动接收端: `cd examples/webhook-receiver && WEBHOOK_SECRET=test-secret cargo run`
3. 启动 CubeAPI:
```bash
cd CubeAPI
CUBE_WEBHOOK_URLS=http://127.0.0.1:9090/webhook \
CUBE_WEBHOOK_SECRET=test-secret \
cargo run
```
4. 创建沙箱触发 `sandbox.created` 事件
