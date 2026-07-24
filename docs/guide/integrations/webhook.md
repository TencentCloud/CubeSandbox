---
title: Webhook Integration Guide
author: YYYSSSRRR
date: 2026-07-24
tags:
  - integration
  - webhook
  - event
  - callback
lang: en-US
---

# Webhook Integration Guide

[中文文档](../../zh/guide/integrations/webhook.md)

Cube Sandbox can push sandbox lifecycle events to any HTTP endpoint via **webhooks**. This guide covers configuration, payload format, signature verification, and how to connect webhooks to notification services like WeCom (企业微信) bot, Slack, or PagerDuty.

A runnable receiver example is available at [`examples/webhook-receiver/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver).

---

## Integration Target and Version

| Component | Version |
|---|---|
| CubeAPI (webhook emitter) | `>= 0.5.0` |
| Receiver example | Python 3.10+ (stdlib only) |
| Webhook protocol | HTTP POST with HMAC-SHA256 signing |

---

## Configuration

Webhooks are configured via environment variables on the CubeAPI process.

### Basic Setup

```bash
# Target URL(s) — comma-separated for multiple endpoints
export CUBE_API_WEBHOOK_URLS="http://localhost:8080/webhook"

# Event filter — "*" for all, or a comma-separated list
export CUBE_API_WEBHOOK_EVENTS="*"

# Optional: shared secret for payload signing
export CUBE_API_WEBHOOK_SECRET="my-shared-secret"
```

### Event Filtering

To receive only sandbox lifecycle events:

```bash
export CUBE_API_WEBHOOK_EVENTS="sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed"
```

### Multiple Targets

Events fan out to all configured URLs in parallel:

```bash
export CUBE_API_WEBHOOK_URLS="http://host-a:8080/hook,http://host-b:8080/hook"
```

### Delivery Behavior

- **Non-blocking**: webhook delivery runs in a background Tokio task, never blocking the API response.
- **Retry**: exponential backoff at 200 ms → 500 ms → 1 s (3 attempts max).
- **Success**: HTTP 2xx is considered successful.
- **Failure**: non-2xx or transport errors trigger a retry; after 3 failures the event is dropped with an error log.

---

## Event Payload

### Schema

Each webhook POST body is a JSON object with the following fields:

| Field | Type | Always present | Description |
|---|---|---|---|
| `event` | string | ✓ | Machine-readable event name |
| `timestamp` | string (ISO 8601) | ✓ | When the event was generated |
| `sandbox_id` | string | — | Sandbox identifier (omit for API-level events) |
| `template_id` | string | — | Template identifier |
| (additional) | varies | — | Event-specific fields, flattened into the root object |

### Event Types

| Event | Trigger | Extra fields |
|---|---|---|
| `sandbox.created` | Sandbox created successfully | — |
| `sandbox.deleted` | Sandbox deleted | — |
| `sandbox.paused` | Sandbox paused | — |
| `sandbox.resumed` | Sandbox resumed | — |
| `sandbox.timeout.updated` | Sandbox timeout changed | `timeout` (seconds) |
| `sandbox.refreshed` | Sandbox TTL refreshed | `duration` (seconds) |
| `api.response` | API request completed | — |
| `api.error` | API handler error | `handler`, `error` |

### Example Payloads

**sandbox.created**:
```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-24T12:00:00Z",
  "sandbox_id": "sb-abc123",
  "template_id": "tpl-python-3.12"
}
```

**sandbox.timeout.updated** (with flattened extra field):
```json
{
  "event": "sandbox.timeout.updated",
  "timestamp": "2026-07-24T12:05:00Z",
  "sandbox_id": "sb-abc123",
  "timeout": 3600
}
```

**api.error**:
```json
{
  "event": "api.error",
  "timestamp": "2026-07-24T12:10:00Z",
  "handler": "sandboxes.create",
  "error": "template not found"
}
```

---

## Signature Verification

When `CUBE_API_WEBHOOK_SECRET` is set, every POST includes:

```
X-Cube-Signature-256: sha256=<hex-encoded-hmac>
```

The HMAC is computed as **HMAC-SHA256** of the raw JSON request body, keyed by the shared secret.

### Verification Examples

**Python** (fully compatible with the receiver example):
```python
import hmac

def verify(body: bytes, header: str, secret: str) -> bool:
    """Verify X-Cube-Signature-256 header."""
    if not header.startswith("sha256="):
        return False
    expected = header[len("sha256="):]
    computed = hmac.new(secret.encode("utf-8"), body, "sha256").hexdigest()
    return hmac.compare_digest(computed, expected)
```

**Node.js / TypeScript**:
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

**Go**:
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

### Testing Signature with cURL

```bash
BODY='{"event":"sandbox.created","timestamp":"2026-07-24T12:00:00Z","sandbox_id":"sb-test"}'
SIG=$(echo -n "$BODY" | openssl dgst -sha256 -hmac "my-secret" | awk '{print $NF}')

curl -X POST http://localhost:8080/webhook \
  -H "Content-Type: application/json" \
  -H "X-Cube-Signature-256: sha256=$SIG" \
  -d "$BODY"
```

---

## Receiving Webhooks

A zero-dependency Python receiver is provided at [`examples/webhook-receiver/receiver.py`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver).

```bash
# Start the receiver on your CubeAPI server
python receiver.py --port 8080 --path /webhook

# With signature verification
WEBHOOK_SECRET=my-shared-secret python receiver.py
```

The receiver prints each event with color-coded output and supports optional HMAC verification. See the [example README](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver) for the full end-to-end walkthrough.

---

## Integrating with Notification Services

### WeCom (企业微信) Bot

WeCom group robots accept a JSON payload at a fixed webhook URL.

**Forwarding example (Python)**:
```python
import hmac
import json
from http.server import HTTPServer, BaseHTTPRequestHandler
import urllib.request

WECOM_WEBHOOK_URL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_KEY"

class WeComForwarder(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        payload = json.loads(body)

        event = payload.get("event", "unknown")
        sandbox_id = payload.get("sandbox_id", "N/A")

        # Build WeCom markdown message
        msg = {
            "msgtype": "markdown",
            "markdown": {
                "content": (
                    f"### Cube Sandbox Event: {event}\n"
                    f"> **Sandbox:** {sandbox_id}\n"
                    f"> **Time:** {payload.get('timestamp', 'N/A')}\n"
                )
            }
        }

        req = urllib.request.Request(
            WECOM_WEBHOOK_URL,
            data=json.dumps(msg).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        urllib.request.urlopen(req)

        self.send_response(200)
        self.end_headers()

HTTPServer(("0.0.0.0", 8080), WeComForwarder).serve_forever()
```

### Slack Webhook

Slack Incoming Webhooks accept a simple JSON body:

```python
import json
import urllib.request

SLACK_URL = "https://hooks.slack.com/services/T000000/B000000/xxxxxxxx"

def send_to_slack(payload: dict):
    msg = {
        "text": (
            f"*Cube Sandbox Event:* {payload['event']}\n"
            f"• Sandbox: `{payload.get('sandbox_id', 'N/A')}`\n"
            f"• Time: {payload.get('timestamp', 'N/A')}"
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

PD_ROUTING_KEY = "your-pagerduty-routing-key"

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

## Production Considerations

- **Idempotency**: webhook retries may deliver the same event more than once. Design your receiver to handle duplicates (e.g., by tracking `event` + `timestamp` pairs).
- **Rate limiting**: CubeAPI does not throttle outgoing webhooks. If your receiver calls an external API with rate limits, add a queue or rate limiter.
- **Monitoring**: CubeAPI logs delivery failures via `tracing` at `WARN` (retry) and `ERROR` (exhausted). Pipe these logs into your monitoring stack.
- **Scaling**: the receiver in this guide is single-threaded. For high-throughput production use, deploy a dedicated webhook framework (FastAPI, Flask, or a message queue consumer).
- **Authentication**: always enable `CUBE_API_WEBHOOK_SECRET` in production to prevent spoofed events.

---

## References

- [Source: CubeAPI webhook emitter](https://github.com/TencentCloud/CubeSandbox/tree/master/CubeAPI/src/logging/http.rs)
- [Example: Webhook receiver + quickstart](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)
- [Configuration reference](https://github.com/TencentCloud/CubeSandbox/tree/master/CubeAPI/src/config/mod.rs)
- [WeCom bot documentation](https://developer.work.weixin.qq.com/document/path/91770)
- [Slack Incoming Webhooks](https://api.slack.com/messaging/webhooks)
- [PagerDuty Events API v2](https://developer.pagerduty.com/docs/events-api-v2/overview)