---
title: Webhook Event Notifications
author: zhangzherui
date: 2026-07-12
tags: [integration, webhook, lifecycle, security]
---

# Webhook Event Notifications

CubeAPI can deliver sandbox lifecycle events to multiple HTTP endpoints without
waiting for receiver I/O. Each endpoint has an independent subscription and
HMAC secret.

## Configuration

Set `WEBHOOK_ENDPOINTS_JSON` to a JSON array:

```bash
export WEBHOOK_ENDPOINTS_JSON='[
  {"url":"http://receiver:8088/webhook","events":["sandbox.created","sandbox.deleted"],"secret":"created-endpoint-secret"},
  {"url":"https://audit.example.com/cube","events":["sandbox.paused","sandbox.resumed"],"secret":"audit-secret"}
]'
export WEBHOOK_QUEUE_CAPACITY=1024
export WEBHOOK_MAX_RETRIES=3
export WEBHOOK_RETRY_BASE_MS=250
export WEBHOOK_REQUEST_TIMEOUT_SECS=10
```

Invalid JSON, non-HTTP URLs, or empty subscriptions fail CubeAPI startup instead
of silently disabling notifications. Leave the variable unset to disable them.

## Payload and signature

Every POST is the existing structured `LogEvent`, flattened as JSON:

```json
{"timestamp":"2026-07-12T08:00:00Z","level":"info","event":"sandbox.created","sandbox_id":"sb-123","template_id":"tpl-456"}
```

Headers include `X-Cube-Event` and, when a secret is configured,
`X-Cube-Timestamp: <unix-seconds>`, `X-Cube-Nonce: <uuid>`, and
`X-Cube-Signature-256: sha256=<hex>`. Compute HMAC-SHA256 over
`<timestamp>.<nonce>.<exact raw request body>`, reject timestamps outside a
five-minute freshness window and nonces already accepted in that window, and
use constant-time comparison before parsing JSON. Each retry has a fresh nonce
and signature. See the
[receiver example](../../../examples/webhook-receiver/README.md).

## Delivery guarantees

The API path only performs a non-blocking bounded-queue enqueue. A full queue
drops the new event and logs a warning. Network errors, HTTP 408/429, and 5xx
responses retry with exponential backoff; other 4xx responses do not. Graceful
shutdown drains all events accepted before the flush barrier. Delivery is
at-least-once across retries but memory queues do not survive a process crash,
so receivers must be idempotent using `(event, sandbox_id, timestamp)`.
Events remain ordered for each endpoint; separate endpoints have independent
delivery workers.

For WeCom, set `WECOM_BOT_URL` on the example receiver. Keep the bot URL outside
CubeAPI configuration so the receiver remains the security and formatting
boundary.
