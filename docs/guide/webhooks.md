# Webhook Event Notifications

CubeAPI can asynchronously POST sandbox lifecycle events to one or more webhook
endpoints. The first supported events are:

| Event | Trigger |
|---|---|
| `sandbox.created` | `POST /sandboxes` succeeds |
| `sandbox.deleted` | `DELETE /sandboxes/{sandboxID}` succeeds |
| `sandbox.paused` | `POST /sandboxes/{sandboxID}/pause` succeeds |
| `sandbox.resumed` | `POST /sandboxes/{sandboxID}/resume` succeeds |

## Configure Endpoints

Set `CUBE_API_WEBHOOKS` to a JSON array and restart CubeAPI:

```bash
export CUBE_API_WEBHOOKS='[
  {
    "url": "http://127.0.0.1:9000/webhook",
    "events": ["sandbox.created", "sandbox.deleted", "sandbox.paused", "sandbox.resumed"],
    "secret": "change-me"
  }
]'
```

For simple deployments, a comma-separated URL list is also supported:

```bash
export CUBE_API_WEBHOOK_URLS='http://127.0.0.1:9000/webhook,http://127.0.0.1:9001/webhook'
export CUBE_API_WEBHOOK_EVENTS='sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed'
export CUBE_API_WEBHOOK_SECRET='change-me'
```

Optional delivery tuning:

| Variable | Default | Description |
|---|---:|---|
| `CUBE_API_WEBHOOK_QUEUE_CAPACITY` | `1024` | Buffered event queue size |
| `CUBE_API_WEBHOOK_REQUEST_TIMEOUT_SECS` | `5` | Per-request timeout |
| `CUBE_API_WEBHOOK_MAX_ATTEMPTS` | `3` | Attempts per endpoint |
| `CUBE_API_WEBHOOK_INITIAL_BACKOFF_MILLIS` | `200` | Initial exponential-backoff delay |

Webhook delivery is non-blocking for sandbox API calls. If the receiver is slow
or unreachable, CubeAPI retries in the background and logs delivery failures.

## Payload

CubeAPI sends a JSON object derived from the structured lifecycle event:

```json
{
  "timestamp": "2026-07-02T10:00:00Z",
  "level": "info",
  "event": "sandbox.created",
  "sandbox_id": "sb-123",
  "template_id": "tpl-123"
}
```

Every payload includes `event`, `timestamp`, and `sandbox_id`. Creation events
also include `template_id`; more contextual fields may be added over time.

CubeAPI also sets:

| Header | Description |
|---|---|
| `X-Cube-Event` | Event name |
| `X-Cube-Delivery` | Unique delivery ID |
| `X-Cube-Signature` | Present when `secret` is configured; format `sha256=<hex>` |

## Verify Signatures

Compute HMAC-SHA256 over the exact request body bytes:

```python
import hashlib
import hmac

def valid(body: bytes, header: str, secret: str) -> bool:
    expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, header)
```

## Local Receiver

A runnable receiver is available in `examples/webhook-receiver/`:

```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=change-me python3 receiver.py
```

Set the endpoint URL in `CUBE_API_WEBHOOKS`, restart CubeAPI, then create,
pause, resume, or delete a sandbox. The receiver prints each JSON payload.

## WeCom Or Generic Alerting

Use a small receiver service as the bridge between CubeAPI and downstream tools.
The example receiver forwards to a WeCom group robot when `WECOM_BOT_WEBHOOK` is
set. For generic HTTP alerting, keep the same verification step and transform
the Cube payload into the alert manager's expected schema before forwarding.
