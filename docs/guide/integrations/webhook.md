---
title: Webhook Integration Guide
author: YYYSSSRRR
date: 2026-08-31
tags:
  - integration
  - webhook
  - event
  - callback
lang: en-US
---

# Webhook Integration Guide

[中文文档](../../zh/guide/integrations/webhook.md)

Cube Sandbox can push sandbox lifecycle events to any HTTP endpoint via
**webhooks**. This guide covers configuration, the payload format, signature
verification, delivery semantics, and how to hook events up to WeCom (企业微信)
bot or a generic HTTP alerting endpoint.

A runnable receiver is available at
[`examples/webhook-receiver/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver).

---

## Integration Target and Version

| Component | Version |
|---|---|
| cube-lifecycle-manager (webhook emitter) | `>= v0.6.0` |
| Receiver example | Python 3.10+ (stdlib only) |
| Webhook protocol | HTTP POST with HMAC-SHA256 signing |

The wire protocol is **identical to CubeAPI's webhook emitter**, so a receiver
built for one works for the other unchanged.

---

## How it works

The `cube-lifecycle-manager` (CLM) service already consumes the lifecycle Redis
stream (`cube:v1:shared:sandbox:lifecycle:events`) for auto-pause/auto-resume.
Webhook delivery is an **independent consumer of the same stream** using its own
consumer group (`cube-webhook-delivery`). Events are therefore:

- **Durable**: they live in Redis until delivered and acknowledged — a CLM crash
  cannot lose them.
- **Non-blocking**: webhook delivery never blocks sandbox create/destroy (the
  main lifecycle consumer group is separate).
- **Sourced from real transitions**: every pause/resume/delete emitted by
  CubeMaster, whether initiated by the platform (idle auto-pause, timeout kill)
  or by a user (SDK connect / API pause / delete), becomes a webhook event.

---

## Configuration

Webhooks are configured via environment variables on the **CLM process**.

### Basic setup

```bash
# Target URL(s) — comma-separated for multiple endpoints
export CUBE_LCM_WEBHOOK_URLS="http://your-receiver:8081/webhook"

# Event filter — "*" for all, or a comma-separated list
export CUBE_LCM_WEBHOOK_EVENTS="*"

# Optional: shared secret for payload signing
export CUBE_LCM_WEBHOOK_SECRET="my-shared-secret"
```

Webhook delivery is **disabled** when `CUBE_LCM_WEBHOOK_URLS` is empty.

### Tuning knobs

| Variable | Default | Meaning |
|---|---|---|
| `CUBE_LCM_WEBHOOK_TIMEOUT` | `10s` | Per-request HTTP timeout |
| `CUBE_LCM_WEBHOOK_MAX_RETRIES` | `2` | Retries after the first attempt (3 total) |

### Runtime endpoint management (optional REST API)

Endpoints can be added, updated, and removed at runtime via the CLM admin API.
All `/admin/webhooks*` routes require the `X-Cube-Admin-Token` header set to
`CUBE_LCM_ADMIN_TOKEN`:

```
GET    /admin/webhooks             # list endpoints
POST   /admin/webhooks             # add  (body: {"url","events","secret","enabled"})
PUT    /admin/webhooks/{id}        # update
DELETE /admin/webhooks/{id}        # delete
GET    /admin/webhooks/stats       # dropped / delivered / failed counters
```

```bash
curl -H "X-Cube-Admin-Token: $CUBE_LCM_ADMIN_TOKEN" \
     -d '{"url":"http://host-b:8080/hook","events":["sandbox.paused","sandbox.resumed"]}' \
     http://127.0.0.1:8083/admin/webhooks
```

---

## Event Types

| Event | Trigger | Extra fields |
|---|---|---|
| `sandbox.created` | Sandbox created successfully | — |
| `sandbox.deleted` | Sandbox deleted (user or timeout kill) | — |
| `sandbox.paused` | Pause completed (idle auto-pause or manual) | `state`, `actor`, `source` |
| `sandbox.resumed` | Resume completed (auto-resume or manual) | `state`, `actor`, `source` |
| `sandbox.timeout.updated` | Idle timeout refreshed (`set_timeout`) | `timeout` (seconds) |

---

## Payload

Every delivery is a JSON object with the following fields:

| Field | Type | Always present | Description |
|---|---|---|---|
| `event` | string | ✓ | Machine-readable event name |
| `event_id` | string | ✓ | Stable ID (= the Redis stream entry ID); **use it for deduplication** |
| `timestamp` | string | ✓ | When the event was generated (ISO 8601, UTC) |
| `sandbox_id` | string | ✓ | Sandbox identifier |
| `template_id` | string | — | Template the sandbox booted from (best-effort) |
| `host_id` / `host_ip` | string | — | Node the sandbox runs on |
| `instance_type` | string | — | Runtime instance type (e.g. `cubebox`) |
| `timeout_seconds` | int | — | Idle timeout in seconds |
| `auto_pause` / `auto_resume` | bool | — | Lifecycle flags |
| `created_at` / `end_at` | int | — | Unix ms timestamps |
| (state events) | — | — | `state`, `actor`, `source` flattened into the root |
| (timeout.updated) | — | — | `timeout` (seconds) flattened into the root |

Context fields are omitted (not `null`) when absent.

### Example — sandbox.created

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

### Example — sandbox.paused

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

## Signature Verification

When `CUBE_LCM_WEBHOOK_SECRET` is set, every request carries:

```
X-Cube-Signature-256: sha256=<hex>
```

where `<hex>` is the lowercase hex of **HMAC-SHA256 of the raw request body**
keyed by the secret's UTF-8 bytes. Verify against the exact body bytes you read
off the wire. Receivers without a secret configured skip verification.

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

## Delivery Semantics

- **Async + non-blocking**: delivery runs on a dedicated consumer group; it
  never blocks sandbox create/destroy or the CLM's main reconciliation loop.
- **Retry**: each delivery is attempted up to `CUBE_LCM_WEBHOOK_MAX_RETRIES + 1`
  times (default 3) with exponential backoff (200ms → 400ms → … capped at 1s).
  Any HTTP `2xx` stops the retry; anything else retries. After the budget is
  exhausted the event is acknowledged and dropped (with an error log).
- **At-least-once**: an event is acknowledged only after its delivery outcome is
  decided, so a CLM crash between read and ack redelivers it. **Receivers must
  deduplicate on `event_id`.**
- **Crash recovery**: on startup CLM drains the group's pending list; a periodic
  safety net additionally reclaims entries stuck in pending (e.g. a dead peer
  replica, a failed ack) and retries them. A permanently unreachable receiver
  will therefore see repeated (dedupe-able) attempts before events are dropped.

---

## Alerting: WeCom bot and generic HTTP

The CLM delivers the raw lifecycle JSON to whatever URL you configure. To turn
that into an alert you need a small relay that (a) verifies the signature and
(b) reformats the payload for the downstream system.

### Generic HTTP alerting (Slack, PagerDuty, your API)

Point `CUBE_LCM_WEBHOOK_URLS` at a relay that forwards to your alerting API.
The example receiver's signature check is reusable verbatim; the reformat step
is specific to each service.

### WeCom (企业微信) bot

The WeCom group-bot webhook expects a different body shape
(`{"msgtype":"markdown",...}`) and does not verify our signature, so the chain
is **CLM → relay → WeCom**. A minimal relay:

```python
#!/usr/bin/env python3
"""wecom-relay.py — verify CLM webhook, reformat, forward to a WeCom bot."""
import hashlib, hmac, json, urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

SECRET = "my-shared-secret"   # must match CUBE_LCM_WEBHOOK_SECRET
WECOM_URL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=YOUR_BOT_KEY"

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):  # silence
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

Then:

```bash
export CUBE_LCM_WEBHOOK_URLS="http://127.0.0.1:8082/"   # the relay, not WeCom
export CUBE_LCM_WEBHOOK_SECRET="my-shared-secret"
python wecom-relay.py
```

---

## Caveats

- **Deduplicate on `event_id`.** Delivery is at-least-once; you will see
  duplicates after a CLM crash or restart.
- **Events during CLM downtime are not backfilled.** Webhook delivery starts
  when the consumer group is created (new events only); it does not replay
  history or events that occurred while CLM was down.
- **`sandbox.deleted` carries no payload** on the stream; `template_id` is
  recovered from the CLM's in-memory meta cache and may be absent after a
  restart that skipped the sandbox's create event (best-effort).
- **Failed deliveries are dropped after the retry budget.** If a receiver is
  down for a long time, events eventually stop being retried.
- **Ordering is best-effort** (delivery is concurrent across endpoints).
- Always set `CUBE_LCM_WEBHOOK_SECRET` in production, and prefer HTTPS for the
  target URL — HMAC authenticates integrity, not transport.

---

## References

- Runnable receiver: [`examples/webhook-receiver/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)
- CLM configuration: `cube-lifecycle-manager/internal/config/config.go`
- [Sandbox Lifecycle Guide](../lifecycle.md)