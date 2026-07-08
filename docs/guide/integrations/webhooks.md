---
title: Webhook Event Notifications
author: CubeSandbox Contributors
date: 2026-07-08
tags:
  - integration
  - webhook
  - alerting
lang: en-US
---

# Webhook Event Notifications

[中文文档](../../zh/guide/integrations/webhooks.md)

CubeAPI can deliver structured events to user-owned HTTP endpoints. This is
useful for orchestration systems, alerting pipelines, audit collectors, and IM
robot integrations that should react without polling CubeAPI.

## Enable Webhooks

CubeAPI reads Webhook configuration once at startup. There is no REST management
API and no hot reload in this version.

Create `/usr/local/services/cubetoolbox/CubeAPI/webhooks.toml`:

```toml
[delivery]
queue_size = 10000
request_timeout_secs = 5
max_attempts = 3
initial_backoff_ms = 500
max_backoff_secs = 10
max_in_flight = 100

[[endpoints]]
name = "ops"
url = "http://127.0.0.1:8088/webhook"
events = [
  "sandbox.created",
  "sandbox.deleted",
  "sandbox.paused",
  "sandbox.resumed",
  "api.error",
]
secret_env = "CUBE_WEBHOOK_SECRET_0"
```

Add the config path and optional secret to
`/usr/local/services/cubetoolbox/.one-click.env`:

```bash
CUBE_API_WEBHOOK_CONFIG=/usr/local/services/cubetoolbox/CubeAPI/webhooks.toml
CUBE_WEBHOOK_SECRET_0=change-me
```

For multiple signed endpoints in one-click deployments, use the
`CUBE_WEBHOOK_SECRET_*` prefix, for example `CUBE_WEBHOOK_SECRET_0` and
`CUBE_WEBHOOK_SECRET_1`, then reference each variable from the endpoint's
`secret_env`.

Restart CubeAPI:

```bash
sudo systemctl restart cube-sandbox-cube-api.service
```

## Payload and Headers

CubeAPI sends one HTTP `POST` per matching event. The JSON body uses the same
flat shape as CubeAPI structured logs:

```json
{
  "timestamp": "2026-07-08T10:00:00Z",
  "level": "info",
  "event": "sandbox.created",
  "sandbox_id": "sbx-xxx",
  "template_id": "tpl-xxx"
}
```

Headers:

| Header | Description |
| --- | --- |
| `Content-Type` | `application/json` |
| `User-Agent` | `CubeSandbox-Webhook/1.0` |
| `X-Cube-Event` | Event name, for example `sandbox.created` |
| `X-Cube-Delivery` | Unique delivery id reused across retries |
| `X-Cube-Signature-256` | Present when `secret_env` is set. Format: `sha256=<hex>` |

Verify the signature against the raw request body:

```python
import hashlib
import hmac

def verify(secret: str, body: bytes, header: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(header, expected)
```

## Events

Recommended business events:

| Event | Fields |
| --- | --- |
| `sandbox.created` | `sandbox_id`, `template_id` |
| `sandbox.deleted` | `sandbox_id` |
| `sandbox.paused` | `sandbox_id` |
| `sandbox.resumed` | `sandbox_id`, `template_id` |
| `sandbox.timeout.updated` | `sandbox_id`, `timeout` |
| `sandbox.refreshed` | `sandbox_id`, `duration` |

Diagnostic events are also available when explicitly subscribed:

| Event | Notes |
| --- | --- |
| `api.request` | Debug-style request event. Webhook filtering is independent of FileLogger level filtering. |
| `api.response` | Successful API response event for selected handlers. |
| `api.error` | Structured API error event where CubeAPI emits one. |

Snapshot/template events and CubeProxy sidecar auto-pause/auto-resume are not
covered in this version.

## Retry and Non-Blocking Behavior

Webhook delivery is asynchronous. CubeAPI enqueues matching events and returns
from sandbox API calls without waiting for receivers.

Failures are retried for network errors, timeouts, HTTP `408`, `429`, and `5xx`
responses. Other `4xx` responses are treated as final failures. Retry delays use
exponential backoff capped by `max_backoff_secs`.

## IM Robot and Alerting Integration

Most IM robots, including WeCom robots, require their own message schema. Run a
small relay service as the Webhook endpoint: verify the Cube signature, map the
event payload to the robot's `msgtype` format, and send it to the robot URL.

For generic HTTP alerting systems that accept arbitrary JSON, subscribe the
receiver directly and filter by `event`.

The runnable receiver example lives in
[`examples/webhook-receiver`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver).
