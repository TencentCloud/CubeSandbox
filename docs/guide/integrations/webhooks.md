---
title: Webhook Event Notifications
author: io-wy
date: 2026-07-22
tags:
  - integration
  - webhook
  - cubeapi
lang: en-US
---

# Webhook Event Notifications

CubeAPI can deliver sandbox lifecycle events to one or more HTTP endpoints. The
delivery path is asynchronous: sandbox API requests enqueue events and do not
wait for remote webhook receivers.

## Supported Events

The default lifecycle subscription contains:

- `sandbox.created`
- `sandbox.deleted`
- `sandbox.paused`
- `sandbox.resumed`

`sandbox.created` includes `sandbox_id` and `template_id`. The other lifecycle
events include `sandbox_id`.

## Configuration

Set `CUBE_API_WEBHOOK_ENDPOINTS` to a JSON array before starting CubeAPI:

```bash
export CUBE_API_WEBHOOK_ENDPOINTS='[
  {
    "url": "http://127.0.0.1:9000/webhook",
    "events": [
      "sandbox.created",
      "sandbox.deleted",
      "sandbox.paused",
      "sandbox.resumed"
    ],
    "secret": "dev-secret",
    "queue_capacity": 1024,
    "max_retries": 3,
    "retry_base_ms": 500,
    "retry_max_ms": 30000,
    "timeout_secs": 5
  }
]'
```

Fields:

| Field | Required | Description |
| --- | --- | --- |
| `url` | Yes | HTTP or HTTPS endpoint that receives JSON `POST` requests. |
| `events` | No | Event names subscribed by this endpoint. Defaults to the four sandbox lifecycle events. |
| `secret` | No | HMAC-SHA256 signing secret. Omit or set empty to disable signing. Use a strong random value; 16 bytes or longer is recommended. |
| `queue_capacity` | No | Bounded in-memory queue size per endpoint. Default: `1024`. |
| `max_retries` | No | Retry count after the initial attempt. Default: `3`. |
| `retry_base_ms` | No | Initial exponential backoff delay. Default: `500`. |
| `retry_max_ms` | No | Maximum retry delay. Default: `30000`. |
| `timeout_secs` | No | HTTP request timeout per delivery attempt. Default: `5`. |

## Payload

CubeAPI sends each event as JSON. Example:

```json
{
  "timestamp": "2026-07-22T10:00:00Z",
  "level": "info",
  "event": "sandbox.created",
  "sandbox_id": "sbx-123",
  "template_id": "tpl-456"
}
```

Additional structured fields may be included as CubeAPI adds more event context.

## Headers

Every delivery includes:

- `Content-Type: application/json`
- `X-Cube-Event`
- `X-Cube-Delivery`

When `secret` is configured, CubeAPI also sends:

- `X-Cube-Timestamp`
- `X-Cube-Nonce`
- `X-Cube-Signature-256`

The signature is calculated from the raw request body:

```text
sha256=HMAC_SHA256(secret, timestamp + "." + nonce + "." + raw_body)
```

Receivers should compare signatures with a constant-time comparison function.
`X-Cube-Timestamp` is a Unix millisecond timestamp generated for each delivery
attempt.
They may also reject stale `X-Cube-Timestamp` values according to their own
clock-skew policy, for example a five-minute tolerance window.

## Failure Handling

CubeAPI retries delivery failures for network errors and retryable HTTP status
codes: `408`, `429`, and `5xx`. Other `4xx` responses are treated as
non-retryable receiver-side failures.

Each webhook endpoint has an independent bounded queue and background worker. A
slow or unreachable endpoint does not block sandbox lifecycle API requests or
other webhook endpoints. If an endpoint queue is full, CubeAPI drops the event
for that endpoint and writes a warning log.

## Local Receiver

A runnable receiver is available under `examples/webhook-receiver`:

```bash
cd examples/webhook-receiver
WEBHOOK_SECRET=dev-secret python3 receiver.py
```

Trigger sandbox create, pause, resume, or delete operations. The receiver prints
the delivery ID, event name, signature result, and JSON payload.

## Generic Alerting

The receiver can forward accepted events to a generic HTTP alerting service or
an enterprise chat robot. Keep the CubeAPI webhook endpoint private when
possible, verify the HMAC signature before forwarding, and avoid sending secrets
or large payloads to third-party chat systems.
