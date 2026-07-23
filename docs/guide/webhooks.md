# CubeSandbox Webhook Event Notifications

CubeAPI can send webhook callbacks to user-configured HTTP endpoints when
sandbox lifecycle events occur. Use cases include real-time monitoring,
automated workflows, and audit trails.

## Development Verification (no CubeMaster, ~5 minutes)

For contributors or reviewers validating the webhook implementation without
a running CubeSandbox cluster.

### 1. Core logic — unit & integration tests

```bash
cd CubeAPI
cargo test -- logging::http
```

10 tests covering: payload construction, HMAC signing, HTTP delivery, event
filtering, retry behavior, and shutdown coordination. All use mock HTTP
servers — no external dependencies.

### 2. Receiver — standalone verification

```bash
# Terminal 1: Start the receiver
cd examples/webhook-receiver
WEBHOOK_SECRET=test-secret cargo run

# Terminal 2: Simulate CubeAPI sending a webhook
BODY='{"event":"sandbox.created","timestamp":"2026-07-20T12:00:00Z","sandbox_id":"sb-abc"}'
SIG="sha256=$(echo -n "$BODY" | openssl dgst -sha256 -hmac "test-secret" | cut -d' ' -f2)"
curl -X POST http://127.0.0.1:9090/webhook \
  -H "Content-Type: application/json" \
  -H "X-Cube-Event: sandbox.created" \
  -H "X-Cube-Signature: $SIG" \
  -d "$BODY"
# Terminal 1 should print the received webhook with HMAC verification OK
```

### 3. Code style

```bash
cd CubeAPI
cargo fmt --check
cargo clippy -- -D warnings 2>&1 | grep -E "http\.rs|main\.rs|handlers/sandboxes\.rs" || true
```

## Production Configuration (requires CubeMaster)

Once CubeAPI has a working CubeMaster backend, configure webhooks via
environment variables or CLI flags.

### Environment variables

```bash
export CUBE_WEBHOOK_URLS="https://your-server.com/webhook"
export CUBE_WEBHOOK_EVENTS="sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed"
export CUBE_WEBHOOK_SECRET="your-hmac-secret-key"
```

### CLI flags

```bash
cube-api --webhook-urls "https://your-server.com/webhook" \
         --webhook-events "sandbox.created,sandbox.deleted" \
         --webhook-secret "your-hmac-secret-key"
```

CLI flags take priority over environment variables. Multiple endpoints can be
specified as a comma-separated list in `CUBE_WEBHOOK_URLS`.

### Full E2E test

```bash
# Terminal 1: start the receiver
cd examples/webhook-receiver
WEBHOOK_SECRET=test-secret cargo run

# Terminal 2: start CubeAPI with webhook config
cd CubeAPI
CUBE_WEBHOOK_URLS=http://127.0.0.1:9090/webhook \
CUBE_WEBHOOK_SECRET=test-secret \
cargo run

# Terminal 3: create a sandbox to trigger sandbox.created
curl -X POST http://localhost:3000/sandboxes \
  -H "Content-Type: application/json" \
  -d '{"templateID": "your-template-id"}'
# Terminal 1 should print the received webhook
```

## Configuration Reference

| Env Variable | CLI Flag | Default | Description |
|-------------|----------|---------|-------------|
| `CUBE_WEBHOOK_URLS` | `--webhook-urls` | `""` (disabled) | Comma-separated webhook endpoint URLs |
| `CUBE_WEBHOOK_EVENTS` | `--webhook-events` | 4 lifecycle events | Comma-separated event types to subscribe to |
| `CUBE_WEBHOOK_SECRET` | `--webhook-secret` | `""` (no signing) | Shared HMAC-SHA256 secret key |

When `CUBE_WEBHOOK_URLS` is empty, webhook delivery is completely disabled
with zero performance overhead.

## Supported Events

| Event | Trigger | Fields |
|-------|---------|--------|
| `sandbox.created` | Sandbox created | `sandbox_id`, `template_id` |
| `sandbox.deleted` | Sandbox deleted | `sandbox_id` |
| `sandbox.paused` | Sandbox paused | `sandbox_id` |
| `sandbox.resumed` | Sandbox resumed | `sandbox_id`, `template_id` |

## Payload Format

### HTTP Request

```
POST <webhook-url>
Content-Type: application/json
X-Cube-Event: sandbox.created
X-Cube-Delivery: 550e8400-e29b-41d4-a716-446655440000
X-Cube-Signature: sha256=d5e9f... (only when CUBE_WEBHOOK_SECRET is set)
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

Fields:
- `event`: Event name
- `timestamp`: Event time (RFC 3339)
- `sandbox_id`: Unique sandbox identifier
- `template_id`: Template identifier (present in `created` and `resumed` events)

## Security

### HMAC-SHA256 Signature Verification

CubeAPI signs the raw HTTP body bytes using HMAC-SHA256 and passes the
signature via the `X-Cube-Signature` header.

Signature format: `sha256=<lowercase hex>`

**Verification steps:**

1. Obtain the raw HTTP body bytes (do NOT deserialize and re-serialize JSON —
   JSON serialization is not deterministic)
2. Compute `HMAC-SHA256(secret, raw_body_bytes)`
3. Compare with the `X-Cube-Signature` header value (constant-time comparison
   recommended for production)

**Rust verification example:**

```rust
use hmac::{Hmac, Mac};
use sha2::Sha256;

type HmacSha256 = Hmac<Sha256>;

fn verify_signature(secret: &str, body: &[u8], signature: &str) -> bool {
    let mut mac = HmacSha256::new_from_slice(secret.as_bytes())
        .expect("HMAC can take key of any size");
    mac.update(body);
    let expected = format!("sha256={}", hex::encode(mac.finalize().into_bytes()));
    // Use constant-time comparison in production
    expected == signature
}
```

**Python verification example:**

```python
import hmac
import hashlib

def verify_signature(secret: str, body: bytes, signature: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode(), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature)
```

### Idempotency

Each delivery generates a unique `X-Cube-Delivery` UUID. Receivers can use
this ID for deduplication to prevent processing the same event more than once.

## Retry Strategy

| Condition | Behavior |
|-----------|----------|
| HTTP 2xx | Success |
| HTTP 4xx | No retry (client error is not recoverable) |
| HTTP 5xx / connection error | Exponential backoff retry, up to 3 times |

Backoff: 1s → 2s → 4s. Maximum 4 HTTP attempts (1 initial + 3 retries).

## WeCom Bot Integration

The CubeAPI Webhook payload format differs from the WeCom bot message format
(`msgtype`). Use the example receiver as an adapter:

```
CubeAPI → examples/webhook-receiver → WeCom bot
```

**Steps:**

1. Add a bot in your WeCom group and get the webhook URL
   (format: `https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx`)

2. Start the receiver with WeCom forwarding:
```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=test-secret \
WECOM_WEBHOOK_URL=https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx \
cargo run
```

3. Configure CubeAPI to point to the receiver:
```bash
cd CubeAPI
CUBE_WEBHOOK_URLS=http://127.0.0.1:9090/webhook \
CUBE_WEBHOOK_SECRET=test-secret \
cargo run
```

Sandbox events will be delivered as text messages to the WeCom group.

### Generic HTTP Alerts

Any alerting platform that accepts HTTP POST can receive CubeAPI Webhook
events — the payload is standard JSON with no special SDK required.

A minimal shell-based receiver that logs events and sends alerts:

```bash
#!/bin/bash
# minimal-webhook-receiver.sh — listen on :8080, print events, forward to
# your alerting platform via curl.
#
# Usage: ./minimal-webhook-receiver.sh

while true; do
  echo -e "HTTP/1.1 200 OK\r\n" | nc -l -p 8080 -q 1 | while read -r line; do
    echo "$line"
  done
done
```

For production use, any HTTP server framework (axum, Express, Flask, nginx)
can receive the POST, parse the JSON body, and route by `event` type to the
appropriate alert channel.

## Troubleshooting

| Symptom | Possible Cause | Check |
|---------|---------------|-------|
| No webhook received | Empty URL config | Verify `CUBE_WEBHOOK_URLS` or `--webhook-urls` |
| Signature verification fails | Key mismatch | Ensure sender and receiver use the same `CUBE_WEBHOOK_SECRET` |
| Signature verification fails | Body modified | Verify against raw HTTP body bytes, not re-serialized JSON |
| Duplicate events received | Normal retry | Deduplicate using `X-Cube-Delivery` UUID |
| Event not triggered | Event name typo | Check `CUBE_WEBHOOK_EVENTS` for correct event names |
| HTTPS certificate error | Self-signed cert | Use HTTP for internal testing, or configure CA certificates |
