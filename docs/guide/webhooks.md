# Webhook Event Notifications

CubeAPI can send sandbox lifecycle events to one or more HTTP endpoints. Delivery
runs in the background and does not block sandbox create, delete, pause, or
resume requests.

## Supported events

| Event | Trigger | Fields |
|---|---|---|
| `sandbox.created` | A sandbox is created successfully | `sandbox_id`, `template_id` |
| `sandbox.deleted` | A sandbox is deleted successfully | `sandbox_id` |
| `sandbox.paused` | A sandbox is paused successfully | `sandbox_id` |
| `sandbox.resumed` | A sandbox is resumed successfully | `sandbox_id` |

## Quick verification

Start the example receiver:

```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=change-me python3 receiver.py --host 127.0.0.1 --port 9000
```

Configure CubeAPI with the simple environment variables:

```bash
export CUBE_API_WEBHOOK_URLS=http://127.0.0.1:9000/webhook
export CUBE_API_WEBHOOK_SECRET=change-me
export CUBE_API_WEBHOOK_EVENTS=sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed
sudo systemctl restart cube-sandbox-cube-api.service
```

Create, pause, resume, or delete a sandbox through the API. The receiver prints
one JSON line for each callback.

## Configuration

For simple deployments, configure one or more comma-separated URLs:

```bash
export CUBE_API_WEBHOOK_URLS=http://127.0.0.1:9000/webhook,http://127.0.0.1:9001/webhook
export CUBE_API_WEBHOOK_EVENTS=sandbox.created,sandbox.deleted
export CUBE_API_WEBHOOK_SECRET=change-me
export CUBE_API_WEBHOOK_MAX_RETRIES=3
export CUBE_API_WEBHOOK_TIMEOUT_SECS=5
export CUBE_API_WEBHOOK_RETRY_INITIAL_DELAY_MS=200
```

For per-endpoint subscriptions and secrets, use `CUBE_API_WEBHOOKS_JSON`:

```bash
export CUBE_API_WEBHOOKS_JSON='[
  {
    "url": "http://127.0.0.1:9000/webhook",
    "events": ["sandbox.created", "sandbox.deleted"],
    "secret": "ops-secret",
    "max_retries": 3,
    "timeout_secs": 5,
    "retry_initial_delay_ms": 200
  },
  {
    "url": "https://alert.example.com/cube",
    "events": ["sandbox.paused", "sandbox.resumed"]
  }
]'
```

`events` also accepts `"*"` to subscribe to every structured event that reaches
the HTTP backend.

## Payload and headers

CubeAPI sends a JSON POST with the structured event payload:

```json
{
  "timestamp": "2026-07-09T12:34:56.789Z",
  "level": "info",
  "event": "sandbox.created",
  "sandbox_id": "sb-123",
  "template_id": "tpl-abc"
}
```

The request includes:

| Header | Description |
|---|---|
| `Content-Type: application/json` | JSON body |
| `X-Cube-Event` | Event name |
| `X-Cube-Timestamp` | Event timestamp |
| `X-Cube-Signature-256` | Present when `secret` is configured |

## Signature verification

When a secret is configured, CubeAPI computes HMAC-SHA256 over the raw request
body and sends:

```text
X-Cube-Signature-256: sha256=<hex digest>
```

Python verification example:

```python
import hashlib
import hmac

def verify(secret: str, body: bytes, header: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, header)
```

Always verify the raw body bytes before parsing JSON.

## Delivery and retries

`log()` enqueues the event and returns immediately. A background task sends each
matching event to subscribed endpoints. If a receiver times out, is unreachable,
or returns a non-2xx status, CubeAPI retries with exponential backoff:

```text
retry_initial_delay_ms, retry_initial_delay_ms * 2, retry_initial_delay_ms * 4, ...
```

Failed attempts and exhausted deliveries are written to CubeAPI logs.

## WeCom and generic alerting

For systems that accept arbitrary JSON, point `url` directly at the alerting
endpoint and optionally enable HMAC verification.

WeCom group robot webhooks expect a message-shaped payload, not CubeAPI's raw
event payload. Use a tiny adapter service:

1. Receive CubeAPI webhook callbacks.
2. Verify `X-Cube-Signature-256`.
3. Convert the event into a WeCom `markdown` or `text` message.
4. POST the converted message to the WeCom robot URL.

This keeps the CubeAPI webhook contract stable while allowing each organization
to customize alert text, routing, and throttling.
