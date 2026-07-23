---
title: Webhook Event Notifications
author: CubeSandbox Contributors
date: 2026-07-09
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
# Events waiting for the Webhook worker. A full queue drops new events without
# blocking the API request path.
event_queue_capacity = 10000
# Endpoint batch deliveries that may exist at once, including deliveries waiting
# for an HTTP permit, sending a request, or sleeping before a retry.
max_outstanding_deliveries = 1000
# HTTP request attempts that may perform network I/O concurrently.
max_concurrent_requests = 100
# Default batch size for endpoints that do not set batch_size. Use 1 to deliver
# each event as soon as possible.
default_batch_size = 1
# Global timed flush interval in seconds. Partial batches are delivered when this
# interval elapses.
flush_interval_secs = 5
# Timeout for one HTTP POST attempt, in seconds.
request_timeout_secs = 5
# Maximum delivery attempts, including the first attempt and retries.
max_attempts = 3
# Delay before the first retry, in milliseconds. Later retries use exponential
# backoff.
initial_backoff_ms = 500
# Maximum exponential-backoff delay, in seconds.
max_backoff_secs = 10
[[endpoints]]
# Endpoint name, used in delivery logs.
name = "ops-lifecycle"
# Webhook receiver URL. CubeAPI sends HTTP POST requests to this address.
url = "http://127.0.0.1:8088/webhook"
# Event types subscribed by this endpoint.
events = [
  "sandbox.created",
  "sandbox.deleted",
  "sandbox.paused",
  "sandbox.resumed",
  "api.error",
]
# Batch size for this endpoint. When omitted, delivery.default_batch_size is used.
batch_size = 1
# Optional. Environment variable that stores the HMAC-SHA256 signing secret. Do
# not put the secret value directly in this file.
secret_env = "CUBE_WEBHOOK_SECRET_0"

[[endpoints]]
# The same URL can be configured more than once, but event sets for the same URL
# must not overlap across endpoint entries.
name = "ops-api"
url = "http://127.0.0.1:8088/webhook"
events = ["api.request", "api.response"]
batch_size = 100
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

CubeAPI sends one HTTP `POST` per matching endpoint batch. The JSON body is a
batch envelope:

```json
{
  "batch_id": "8f6a3f7d-7d87-4ef5-a639-2f6c2b1976f8",
  "events": [
    {
      "timestamp": "2026-07-09T10:00:00Z",
      "level": "info",
      "event": "sandbox.created",
      "sandbox_id": "sbx-xxx",
      "template_id": "tpl-xxx"
    }
  ]
}
```

Each item in `events` uses the same flat shape as CubeAPI structured logs.
`delivery.default_batch_size` is used when an endpoint does not set
`batch_size`. With `batch_size = 1`, each batch contains one event and is
delivered as soon as the worker receives it. If an endpoint sets `batch_size`
greater than 1, CubeAPI buffers events separately for that endpoint and sends a
batch when either `batch_size` is reached or the global `flush_interval_secs`
elapses.

The same URL may appear in multiple endpoint entries when the event sets are
disjoint, which is useful for sending lifecycle events immediately and batching
high-volume diagnostic events to the same receiver. CubeAPI rejects
configuration where the same URL and event are subscribed by more than one
endpoint entry.

Headers:

| Header | Description |
| --- | --- |
| `Content-Type` | `application/json` |
| `User-Agent` | `CubeSandbox-Webhook/1.0` |
| `X-Cube-Signature-256` | Present when `secret_env` is set. Format: `sha256=<hex>` |

Receivers can use `batch_id` for idempotency. Retries reuse the same `batch_id`
and resend the same batch body.

Events keep their production order within one batch. CubeAPI does not guarantee
the delivery order of separate batches, even for the same endpoint: batches may
be delivered concurrently, and a retried older batch can arrive after a newer
batch. Receivers must use `batch_id` and event timestamps rather than arrival
order when reconstructing state.

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

### Delivery Capacity and Backpressure

The three capacity settings bound different stages and use different units:

```text
event channel                  endpoint delivery tasks             HTTP attempts
event_queue_capacity      ->   max_outstanding_deliveries     ->   max_concurrent_requests
```

- `event_queue_capacity` counts events not yet processed by the Webhook worker.
- `max_outstanding_deliveries` counts endpoint batch deliveries from task
  creation through success or retry exhaustion. Waiting for an HTTP permit and
  retry backoff both retain this slot.
- `max_concurrent_requests` counts only HTTP attempts currently performing
  network I/O. A delivery releases this permit before retry backoff.

These values do not require a strict numeric ordering because one event may fan
out to multiple endpoints and one batch delivery may contain multiple events.
It is normally useful for `max_outstanding_deliveries` to exceed
`max_concurrent_requests`, so deliveries in retry backoff do not prevent other
deliveries from using available request concurrency. A larger
`event_queue_capacity` can absorb short bursts but also consumes more memory and
allows a larger delayed backlog.

When the outstanding-delivery limit is reached, the worker waits for a delivery
to finish before reading more events. The event channel then fills up to
`event_queue_capacity`; additional matching events are dropped and logged while
the sandbox API path continues normally.

## IM Robot and Alerting Integration

Most IM robots, including WeCom robots, require their own message schema. Run a
small relay service as the Webhook endpoint: verify the Cube signature, map the
batch payload to the robot's `msgtype` format, and send it to the robot URL.

For generic HTTP alerting systems that accept arbitrary JSON, subscribe the
receiver directly and filter by each item in `events`.

The runnable receiver example lives in
[`examples/webhook-receiver`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver).
