---
title: Webhook Event Notifications
lang: en-US
---

# Webhook Event Notifications

CubeSandbox can push **sandbox lifecycle events** to your own HTTP endpoints as
they happen, so upstream agent orchestrators, ops platforms, or IM tools no
longer need to poll the API for state changes.

A runnable receiver and a WeCom bridge live in
[`examples/webhook-receiver/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver).

## Supported events

| Event | Fired when | Payload fields |
|---|---|---|
| `sandbox.created` | A sandbox is created | `sandbox_id`, `template_id` |
| `sandbox.paused` | A sandbox is paused | `sandbox_id` |
| `sandbox.resumed` | A sandbox is resumed | `sandbox_id` |
| `sandbox.deleted` | A sandbox is destroyed | `sandbox_id` |

## Configuration

Webhooks are declared in a config file (YAML / JSON / TOML) referenced by the
`--webhook-config` flag or the `CUBE_API_WEBHOOK_CONFIG` environment variable.
When neither is set, webhooks are disabled and there is zero overhead.

```yaml
# webhooks.yaml
webhooks:
  - url: "http://127.0.0.1:9100/webhook"
    events:
      - sandbox.created
      - sandbox.paused
      - sandbox.resumed
      - sandbox.deleted
    secret: "my-shared-secret"   # optional
    timeout_ms: 5000             # optional (default 5000)
    max_retries: 3               # optional (default 3)
```

```bash
cube-api --webhook-config /path/to/webhooks.yaml
# or
CUBE_API_WEBHOOK_CONFIG=/path/to/webhooks.yaml cube-api
```

You can register **multiple endpoints**, each subscribing to a different subset
of events. On startup CubeAPI logs `webhook event notifications enabled` with
the endpoint count.

| Field | Required | Description |
|---|---|---|
| `url` | yes | HTTP endpoint that receives the `POST`. |
| `events` | yes | Event types this endpoint subscribes to. |
| `secret` | no | Shared key for HMAC-SHA256 signing (see below). |
| `timeout_ms` | no | Per-request timeout in milliseconds. Default `5000`. |
| `max_retries` | no | Retries after the first attempt. Default `3`. |

## Payload

Each event is delivered as an HTTP `POST` with `Content-Type: application/json`
and these headers:

| Header | Meaning |
|---|---|
| `X-Cube-Event` | The event type, e.g. `sandbox.created`. |
| `X-Cube-Delivery` | A unique id for this delivery. It stays the **same across retries**, so a receiver can deduplicate if a delivery is retried after it was already processed. |
| `X-Cube-Signature` | HMAC-SHA256 signature, present only when a `secret` is configured (see below). |

The body always contains `event`, `timestamp` (RFC 3339, UTC), and
`sandbox_id`, plus any additional context such as `template_id`.

```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-21T08:30:12.481Z",
  "sandbox_id": "sbx-abc123",
  "template_id": "base"
}
```

## Delivery semantics

- **Non-blocking.** Events are enqueued and delivered from background tasks.
  Sandbox API requests never wait on webhook delivery, so a slow or unreachable
  receiver can never delay or fail a create/pause/resume/delete call.
- **Isolated.** Each (event, endpoint) delivery runs independently — one slow
  endpoint does not hold up others.
- **Retried.** Failed deliveries (connection errors, timeouts, or non-2xx
  responses) are retried with exponential backoff (1s, 2s, 4s, …) up to
  `max_retries`. The final failure is logged at `error` level.
- **Best-effort.** Deliveries are not persisted across restarts; the durable
  record remains the [structured event log](./sandbox-logs.md).

Your receiver should respond `2xx` quickly and do any heavy work
asynchronously.

## Signature verification

When `secret` is set, CubeAPI signs the **raw request body** with HMAC-SHA256
and sends the result in the `X-Cube-Signature` header:

```
X-Cube-Signature: sha256=<hex-digest>
```

Verify it against the exact bytes you received (do not re-serialize the JSON
first — key ordering may differ):

```python
import hashlib, hmac

def verify(secret: str, body: bytes, header: str) -> bool:
    expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, header)
```

Use a constant-time comparison (`hmac.compare_digest`) to avoid timing attacks.

## Integrating with WeCom / generic alerting

A small bridge can turn events into WeCom (Enterprise WeChat) group-bot
messages. Create a group bot, copy its webhook URL, and run the example bridge:

```bash
WECOM_BOT_URL="https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxxxxxxx" \
CUBE_WEBHOOK_SECRET=my-shared-secret \
python3 examples/webhook-receiver/wecom_bridge.py
```

The same shape applies to Slack, Feishu, PagerDuty, or any HTTP alerting
system: receive the event, verify the signature, and forward a formatted
message to the target API.
