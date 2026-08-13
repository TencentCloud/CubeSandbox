# CubeSandbox Webhook 事件通知

CubeAPI 支持在沙箱生命周期事件发生时向用户配置的 HTTP 端点发送 Webhook 回调。可用于实时监控、自动化流程、审计追踪等场景。

## 开发验证（不需要 CubeMaster，约 5 分钟）

适用于贡献者或 reviewer 验证 Webhook 实现，无需运行 CubeSandbox 集群。

### 1. 核心逻辑 — 单元/集成测试

```bash
cd CubeAPI
cargo test -- logging::http
```

10 个测试覆盖：payload 构建、HMAC 签名、HTTP 投递、事件过滤、重试、shutdown。全部使用 mock HTTP server，无需外部依赖。

### 2. 接收端 — 独立验证

```bash
# 终端 1: 启动接收端
cd examples/webhook-receiver
WEBHOOK_SECRET=test-secret cargo run

# 终端 2: 用 curl 模拟 CubeAPI 发送 webhook
BODY='{"event":"sandbox.created","timestamp":"2026-07-20T12:00:00Z","sandbox_id":"sb-abc"}'
SIG="sha256=$(echo -n "$BODY" | openssl dgst -sha256 -hmac "test-secret" | cut -d' ' -f2)"
curl -X POST http://127.0.0.1:9090/webhook \
  -H "Content-Type: application/json" \
  -H "X-Cube-Event: sandbox.created" \
  -H "X-Cube-Signature: $SIG" \
  -d "$BODY"
# 终端 1 应打印接收到的 webhook 内容，签名验证通过
```

### 3. 代码规范检查

```bash
cd CubeAPI
cargo fmt --check
cargo clippy -- -D warnings 2>&1 | grep -E "http\.rs|main\.rs|handlers/sandboxes\.rs" || true
```

## 生产配置（需要 CubeMaster）

CubeAPI 连接 CubeMaster 后，通过环境变量或 CLI 参数配置 Webhook。

### 环境变量

```bash
export CUBE_WEBHOOK_URLS="https://your-server.com/webhook"
export CUBE_WEBHOOK_EVENTS="sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed"
export CUBE_WEBHOOK_SECRET="your-hmac-secret-key"
```

### CLI 参数

```bash
cube-api --webhook-urls "https://your-server.com/webhook" \
         --webhook-events "sandbox.created,sandbox.deleted" \
         --webhook-secret "your-hmac-secret-key"
```

CLI 参数优先级高于环境变量。Webhook URL 支持逗号分隔配置多个端点。

### 完整 E2E 测试

```bash
# 终端 1: 启动接收端
cd examples/webhook-receiver
WEBHOOK_SECRET=test-secret cargo run

# 终端 2: 启动 CubeAPI（配置 webhook）
cd CubeAPI
CUBE_WEBHOOK_URLS=http://127.0.0.1:9090/webhook \
CUBE_WEBHOOK_SECRET=test-secret \
cargo run

# 终端 3: 创建沙箱触发 sandbox.created 事件
curl -X POST http://localhost:3000/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"templateID": "your-template-id"}'
# 终端 1 应收到 webhook 回调
```

## 配置参考

| 环境变量              | CLI 参数           | 默认值           | 说明                        |
| --------------------- | ------------------ | ---------------- | --------------------------- |
| `CUBE_WEBHOOK_URLS`   | `--webhook-urls`   | `""` (禁用)      | 逗号分隔的 Webhook 端点 URL |
| `CUBE_WEBHOOK_EVENTS` | `--webhook-events` | 4 个生命周期事件 | 逗号分隔的订阅事件类型      |
| `CUBE_WEBHOOK_SECRET` | `--webhook-secret` | `""` (不签名)    | HMAC-SHA256 共享密钥        |

`CUBE_WEBHOOK_URLS` 为空时，Webhook 功能完全禁用，无任何性能开销。

## 支持的事件

| 事件名            | 触发时机     | 携带字段                    |
| ----------------- | ------------ | --------------------------- |
| `sandbox.created` | 沙箱创建成功 | `sandbox_id`, `template_id` |
| `sandbox.deleted` | 沙箱删除成功 | `sandbox_id`                |
| `sandbox.paused`  | 沙箱暂停成功 | `sandbox_id`                |
| `sandbox.resumed` | 沙箱恢复成功 | `sandbox_id`, `template_id` |

## Payload 格式

### HTTP 请求

```
POST <webhook-url>
Content-Type: application/json
X-Cube-Event: sandbox.created
X-Cube-Delivery: 550e8400-e29b-41d4-a716-446655440000
X-Cube-Signature: sha256=d5e9f... (仅当配置了 CUBE_WEBHOOK_SECRET)
User-Agent: CubeAPI-Webhook/1.0
```

### JSON Body

```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-20T12:00:00.123Z",
  "sandbox_id": "sb-abc123",
  "template_id": "tpl-xyz"
}
```

字段说明：
- `event`: 事件名称
- `timestamp`: 事件发生时间 (RFC 3339)
- `sandbox_id`: 沙箱唯一标识
- `template_id`: 模板标识 (`created` 和 `resumed` 事件中均携带)

## 安全

### HMAC-SHA256 签名验证

CubeAPI 对 HTTP Body 的原始字节计算 HMAC-SHA256 签名，通过 `X-Cube-Signature` Header 传递。

签名格式: `sha256=<lowercase hex>`

**验证步骤:**

1. 获取 HTTP Body 的原始字节 (注意: 不要先解析 JSON 再重新序列化 — JSON 序列化不是确定性的)
2. 使用共享密钥计算 `HMAC-SHA256(secret, raw_body_bytes)`
3. 与 `X-Cube-Signature` Header 的值比对 (安全比较，防时序攻击)

**Rust 验证示例:**

```rust
use hmac::{Hmac, Mac};
use sha2::Sha256;

type HmacSha256 = Hmac<Sha256>;

fn verify_signature(secret: &str, body: &[u8], signature: &str) -> bool {
    let mut mac = HmacSha256::new_from_slice(secret.as_bytes())
        .expect("HMAC can take key of any size");
    mac.update(body);
    let expected = format!("sha256={}", hex::encode(mac.finalize().into_bytes()));
    // 生产环境应使用 constant-time comparison
    expected == signature
}
```

**Python 验证示例:**

```python
import hmac
import hashlib

def verify_signature(secret: str, body: bytes, signature: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode(), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature)
```

### 幂等性

每次投递生成唯一的 `X-Cube-Delivery` UUID。接收端可以使用此 ID 进行去重，防止重复处理同一事件。

## 重试策略

| 条件 | 行为 |
|------|------|
| HTTP 2xx | 成功 |
| HTTP 4xx | 不重试 (客户端错误无法修复) |
| HTTP 5xx / 连接错误 | 指数退避重试，最多 3 次 |

退避间隔: 1s → 2s → 4s。总最多 4 次 HTTP 请求 (initial + 3 retries)。

## 对接企业微信机器人

CubeAPI Webhook 的 Payload 格式与企业微信机器人要求的 `msgtype` 格式不同，不能直接将企微 URL 配置为 CubeAPI 的 Webhook 端点。需要通过接收器示例做适配转发：

```
CubeAPI → examples/webhook-receiver → 企业微信群机器人
```

**步骤:**

1. 在企业微信群中添加机器人，获取 Webhook URL（格式: `https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx`）

2. 启动接收器并设置企微转发:
```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=test-secret \
WECOM_WEBHOOK_URL=https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx \
cargo run
```

3. 配置 CubeAPI 指向接收器:
```bash
cd CubeAPI
CUBE_WEBHOOK_URLS=http://127.0.0.1:9090/webhook \
CUBE_WEBHOOK_SECRET=test-secret \
cargo run
```

沙箱事件将自动以文本消息推送到企业微信群。

### 通用 HTTP 告警

任何支持 HTTP POST 的告警平台都可以直接接收 CubeAPI Webhook 事件 — payload
为标准 JSON，无需特殊 SDK。

最简单的 shell 接收器：

```bash
#!/bin/bash
# 监听 :8080，打印事件内容

while true; do
  echo -e "HTTP/1.1 200 OK\r\n" | nc -l -p 8080 -q 1 | while read -r line; do
    echo "$line"
  done
done
```

生产环境中可以使用任何 HTTP 框架（axum、Express、Flask、nginx）接收 POST，
解析 JSON body，按 `event` 字段路由到对应的告警通道。

## 故障排查

| 症状 | 可能原因 | 检查项 |
|------|---------|--------|
| Webhook 未收到事件 | URL 配置为空 | 检查 `CUBE_WEBHOOK_URLS` 或 `--webhook-urls` |
| 签名验证失败 | 密钥不匹配 | 确认发送端和接收端使用相同的 `CUBE_WEBHOOK_SECRET` |
| 签名验证失败 | Body 被修改 | 确保验证时使用的是原始 HTTP Body 字节 |
| 接收端收到重复事件 | 正常重试 | 使用 `X-Cube-Delivery` UUID 去重 |
| 事件未触发 | 事件名拼写错误 | 检查 `CUBE_WEBHOOK_EVENTS` 中的事件名是否正确 |
| HTTPS 证书错误 | 自签名证书 | 使用 HTTP + 内网环境测试，或配置 CA 证书 |
