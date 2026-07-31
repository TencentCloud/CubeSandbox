---
title: Webhook Events
---

# Webhook Events

CubeAPI can asynchronously notify HTTP endpoints after successful sandbox
lifecycle operations. Webhook delivery runs outside the API request path, so a
slow or unavailable receiver does not make lifecycle requests fail.

## Configure endpoints

Set `CUBE_API_WEBHOOK_ENDPOINTS` to a JSON array before starting CubeAPI:

```bash
export CUBE_API_WEBHOOK_ENDPOINTS='[
  {
    "name": "automation",
    "url": "https://automation.example.com/cube-events",
    "events": ["sandbox.created", "sandbox.deleted"],
    "secret": "replace-with-a-random-secret",
    "timeout_secs": 5,
    "max_retries": 4
  },
  {
    "name": "monitoring",
    "url": "https://monitoring.example.com/events",
    "events": ["*"]
  }
]'
```

`events` accepts exact event names or `*` for all supported events. `secret` is
optional. `timeout_secs` defaults to 5, and `max_retries` defaults to 4 retries
after the initial attempt. Endpoint names must be unique.

Global controls:

| Variable | Default | Description |
| --- | --- | --- |
| `CUBE_API_WEBHOOK_QUEUE_CAPACITY` | `1024` | Maximum number of pending deliveries |
| `CUBE_API_WEBHOOK_MAX_CONCURRENCY` | `16` | Maximum HTTP requests in flight |

Invalid endpoint JSON, URLs, or event names prevent CubeAPI from starting.

## Events and payload

| Event | Emitted after | Additional fields |
| --- | --- | --- |
| `sandbox.created` | A sandbox is created successfully | `template_id` |
| `sandbox.deleted` | A sandbox is deleted successfully | None |
| `sandbox.paused` | A sandbox is paused successfully | None |
| `sandbox.resumed` | A sandbox is resumed successfully | None |

Every payload contains `id`, `event`, `timestamp`, and `sandbox_id`:

```json
{
  "id": "a79a9a61-7330-49cf-8108-c14347f4b94e",
  "event": "sandbox.created",
  "timestamp": "2026-07-31T10:30:00.123Z",
  "sandbox_id": "sandbox-123",
  "template_id": "template-456"
}
```

Receivers should use `id` as an idempotency key because retries deliver the
same event more than once.

## Verify signatures

When an endpoint has a `secret`, CubeAPI sends:

```text
X-Cube-Webhook-Id: <event UUID>
X-Cube-Webhook-Timestamp: <Unix timestamp>
X-Cube-Webhook-Signature: sha256=<hex digest>
```

The signed input is the UTF-8 timestamp, a period, and the exact raw request
body. Do not parse and re-serialize the body before verification.

```python
import hashlib, hmac, time

timestamp = request.headers["X-Cube-Webhook-Timestamp"]
signature = request.headers["X-Cube-Webhook-Signature"]
if abs(time.time() - int(timestamp)) > 300:
    raise ValueError("stale webhook")
expected = "sha256=" + hmac.new(
    secret.encode(), timestamp.encode() + b"." + request.body, hashlib.sha256
).hexdigest()
if not hmac.compare_digest(expected, signature):
    raise ValueError("invalid signature")
```

## Delivery behavior

HTTP 408, 429, 5xx responses, timeouts, and connection errors are retried with
exponential backoff and jitter. Other 4xx responses are treated as permanent
failures. Each failed attempt and exhausted delivery is written to CubeAPI's
logs without exposing the signing secret.

The queue is held in memory. Pending events are lost when CubeAPI restarts, and
events are dropped with a warning if the bounded queue fills. Lifecycle events
currently cover successful operations handled by CubeAPI; internal state
changes that bypass these handlers do not emit webhooks.

During graceful shutdown CubeAPI waits up to 10 seconds for pending webhook
deliveries, then continues shutting down and logs a warning.

## Try the receiver and integrate alerting

The dependency-free receiver under
[`examples/webhook-receiver`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver) prints events
and verifies signatures. It can be started with:

```bash
WEBHOOK_SECRET=development-secret python3 examples/webhook-receiver/receiver.py
```

Enterprise WeChat robots require a provider-specific message body, not the
CubeSandbox event payload. Point CubeAPI at a small receiver such as the
example, verify the signature, transform the event into the required
`msgtype` payload, and POST it to the robot URL. This preserves the generic
Webhook protocol and keeps provider credentials outside CubeAPI.
