# Webhook Receiver Example

用于接收和验证 CubeOps Webhook 事件的示例 HTTP 服务。实现
`proposal/webhook-delivery-spec.md` §6 的三步验证:

1. **HMAC 验签**:对请求体**原始字节**计算 `hex(HMAC-SHA256(secret, body))`,
   与 `X-Cube-Signature-256` 恒定时间比较(不匹配返回 401)。
2. **时间戳防重放**:`X-Cube-Timestamp`(unix 毫秒)与本地时间偏差在
   `TIMESTAMP_TOLERANCE_SECS`(默认 ±300s)内。
3. **幂等去重**:同一 `X-Cube-Delivery` 重复到达直接返回 200,不重复处理
   (进程内存去重集;重试用同一 delivery id,接收方应保持幂等)。

## 快速启动

```bash
cd examples/webhook-receiver

# 不带签名验证
cargo run

# 带 HMAC 签名验证
WEBHOOK_SECRET=test-secret cargo run
```

服务监听 `http://127.0.0.1:9090`。端口可通过 `PORT` 覆盖,监听地址可通过
`LISTEN` 覆盖。

## 单元测试

```bash
cargo test
```

覆盖签名正确/错误/非 hex、时间戳容差边界。

## 独立验证(不需要 CubeMaster/CubeOps)

**终端 1** — 启动接收端:

```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=test-secret cargo run
```

**终端 2** — 模拟 CubeOps 发送:

```bash
BODY='{"schema_version":"1","event":"sandbox.created","event_id":"test:1","timestamp":'"$(date +%s000)"',"occurred_at":"2026-08-15T00:00:00Z","sandbox_id":"sb-abc123","template_id":"tpl-xyz"}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "test-secret" | cut -d' ' -f2)

curl -X POST http://127.0.0.1:9090/webhook \
  -H "Content-Type: application/json" \
  -H "X-Cube-Event-ID: test:1" \
  -H "X-Cube-Delivery: test:1:42" \
  -H "X-Cube-Timestamp: $(date +%s000)" \
  -H "X-Cube-Signature-256: $SIG" \
  -d "$BODY"

# 重放同一 delivery(应返回 200 且不重复打印)
curl -X POST http://127.0.0.1:9090/webhook \
  -H "X-Cube-Delivery: test:1:42" \
  -H "X-Cube-Timestamp: $(date +%s000)" \
  -H "X-Cube-Signature-256: $SIG" \
  -d "$BODY"

# 签名错误(应返回 401)
curl -X POST http://127.0.0.1:9090/webhook \
  -H "X-Cube-Signature-256: deadbeef" \
  -d "$BODY"
```

## 环境变量

| 变量 | 说明 | 默认 |
|------|------|------|
| `WEBHOOK_SECRET` | HMAC-SHA256 共享密钥(空=不验证) | `""` |
| `TIMESTAMP_TOLERANCE_SECS` | 时间戳容差(秒) | `300` |
| `WECOM_WEBHOOK_URL` | 企业微信群机器人 Webhook URL(空=不转发) | `""` |
| `PORT` | 监听端口 | `9090` |
| `LISTEN` | 监听地址 | `127.0.0.1` |

## 与 CubeOps 集成

在 CubeOps 创建订阅时把 `url` 指向本服务的 `/webhook`,并配置相同的
`secret`。订阅 API 见 `docs/zh/guide/webhook.md`。
