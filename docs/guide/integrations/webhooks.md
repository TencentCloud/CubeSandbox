---
title: Webhook Event Notifications
author: CubeSandbox Contributors
date: 2026-07-30
tags:
  - integration
  - webhook
  - alerting
lang: en-US
---

# Webhook Event Notifications

[中文文档](../../zh/guide/integrations/webhooks.md)

CubeSandbox can deliver structured CubeAPI events to user-owned HTTP endpoints.
CubeAPI forwards all event types to CubeOps. CubeOps owns subscriptions,
endpoint batching, HMAC signing, retry, and external HTTP delivery.

## Enable Webhooks

CubeOps reads the webhook configuration once at startup. There is no management
API or hot reload in this version. Without a configured file, CubeOps accepts
internal events but does not send external webhooks.

For one-click installations, copy the bundled example to
`/usr/local/services/cubetoolbox/CubeOps/webhooks.toml`:

```toml
[delivery]
event_queue_capacity = 10000
max_outstanding_deliveries = 1000
max_concurrent_requests = 100
default_batch_size = 1
flush_interval_secs = 5
request_timeout_secs = 5
max_attempts = 3
initial_backoff_ms = 500
max_backoff_secs = 10

[[endpoints]]
name = "ops-lifecycle"
url = "http://127.0.0.1:8088/webhook"
events = [
  "sandbox.created",
  "sandbox.deleted",
  "sandbox.paused",
  "sandbox.resumed",
  "api.error",
]
batch_size = 1
secret_env = "CUBE_WEBHOOK_SECRET_0"

[[endpoints]]
# The same URL may be reused for a disjoint, higher-volume event set.
name = "ops-api"
url = "http://127.0.0.1:8088/webhook"
events = ["api.request", "api.response"]
batch_size = 100
secret_env = "CUBE_WEBHOOK_SECRET_0"
```

Set the path and optional signing secret in
`/usr/local/services/cubetoolbox/.one-click.env`:

```bash
CUBE_OPS_WEBHOOK_CONFIG=/usr/local/services/cubetoolbox/CubeOps/webhooks.toml
CUBE_WEBHOOK_SECRET_0=change-me
```

`secret_env` names must start with `CUBE_WEBHOOK_SECRET_`. Restart CubeOps
after changing the TOML or secret:

```bash
sudo systemctl restart cube-sandbox-cubeops.service
```

In Helm, set `cubeOps.webhook.config` to inline TOML, or set
`cubeOps.webhook.existingConfigMap` to a ConfigMap containing
`webhooks.toml`. Set `cubeOps.webhook.secretName` to a Secret whose keys are
`CUBE_WEBHOOK_SECRET_*` environment variable names.

## Payload and Headers

CubeOps sends one HTTP `POST` per endpoint batch:

```json
{
  "batch_id": "8f6a3f7d-7d87-4ef5-a639-2f6c2b1976f8",
  "events": [
    {
      "timestamp": "2026-07-30T10:00:00Z",
      "level": "info",
      "event": "sandbox.created",
      "sandbox_id": "sbx-xxx",
      "template_id": "tpl-xxx"
    }
  ]
}
```

Each event keeps CubeAPI's flat structured-log shape. CubeOps buffers events
separately per endpoint and sends when `batch_size` is reached or
`flush_interval_secs` elapses. A duplicate subscription for the same
normalized URL and event is rejected.

| Header | Description |
| --- | --- |
| `Content-Type` | `application/json` |
| `User-Agent` | `CubeSandbox-Webhook/1.0` |
| `X-Cube-Signature-256` | Present when `secret_env` is set; `sha256=<hex>` |

Verify the signature against the exact raw request body:

```python
import hashlib
import hmac

def verify(secret: str, body: bytes, header: str) -> bool:
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(header, expected)
```

## Delivery Semantics

Delivery is best effort. A batch may reach a receiver zero, one, or multiple
times. The external `batch_id` identifies one endpoint batch and remains
unchanged when CubeOps retries that batch. Receivers should atomically record a
completed `batch_id` before applying the batch's external side effects; partial
batch processing requires the receiver's own recovery logic.

CubeAPI makes one attempt to hand each internal batch to CubeOps. CubeOps retries
external network errors, timeouts, HTTP `408`, `429`, and `5xx` responses
up to `max_attempts`; other `4xx` responses are final.

Separate endpoint batches can be sent concurrently and may arrive out of order.
Use event timestamps and domain state rather than arrival order. All CubeAPI and
CubeOps webhook queues are memory-only. Queue pressure, exhausted retries,
process crashes, restarts, or shutdown deadlines can lose events. Multiple
CubeOps replicas do not share queues or deduplication state.

The sandbox API request path does not wait for either CubeOps or an external
receiver. CubeAPI uses bounded internal queues and drops when full; CubeOps
admits an internal batch atomically or returns `503`.

## Events

Common business events include `sandbox.created`, `sandbox.deleted`,
`sandbox.paused`, `sandbox.resumed`, `sandbox.timeout.updated`, and
`sandbox.refreshed`. Diagnostic events include `api.request`,
`api.response`, and `api.error`. CubeAPI forwards new event names without a
source-side subscription list; CubeOps filters them against the configured
`events` arrays.

## Capacity

The CubeOps settings bound different stages:

```text
event_queue_capacity -> max_outstanding_deliveries -> max_concurrent_requests
```

`event_queue_capacity` counts accepted events waiting for dispatch.
`max_outstanding_deliveries` includes requests waiting for a network permit
and deliveries sleeping in retry backoff. `max_concurrent_requests` counts
only active HTTP attempts, so retry sleeps do not consume network permits.

Most IM robots require their own message schema. Use a relay that verifies the
signature, deduplicates each completed batch by `batch_id`, maps the payload,
and sends the robot-specific request. A runnable receiver is available in
[`examples/webhook-receiver`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver).
