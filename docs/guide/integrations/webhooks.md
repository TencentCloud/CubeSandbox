---
title: Webhook Event Notifications
author: initiallyqq
date: 2026-07-10
tags:
  - integration
  - webhook
  - observability
lang: en-US
---

# Webhook Event Notifications

CubeAPI can asynchronously send sandbox lifecycle events to one or more HTTP
endpoints. Webhook delivery runs in a background task, so a slow or unavailable
receiver never blocks create, delete, pause, or resume requests.

## Configuration

Set `WEBHOOK_URLS` to a comma-separated list of endpoint URLs. Leaving it unset
keeps Webhooks disabled.

```bash
export WEBHOOK_URLS=https://ops.example.com/cube-events,https://audit.example.com/events
export WEBHOOK_EVENTS=sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed
export WEBHOOK_SECRET=replace-with-a-shared-secret
export WEBHOOK_QUEUE_CAPACITY=1000
export WEBHOOK_MAX_RETRIES=3
export WEBHOOK_RETRY_BASE_MS=200
export WEBHOOK_REQUEST_TIMEOUT_SECS=10
```

`WEBHOOK_EVENTS` is optional. Its default is all four sandbox lifecycle events.
Every configured endpoint receives the same selected event types.

## Payload and Signature

CubeAPI sends one JSON object per event. It includes `event`, `timestamp`,
`level`, and event-specific fields such as `sandbox_id` and `template_id`.

When `WEBHOOK_SECRET` is set, each request includes:

```text
X-Cube-Signature-256: sha256=<hex HMAC-SHA256 of the raw request body>
```

Verify the signature against the raw body before parsing it. The bundled
[receiver example](../../../examples/webhook-receiver/README.md) shows a
standard-library Python implementation.

## Delivery Behavior

Each endpoint has a bounded in-memory queue. If it fills, new events are
dropped with a warning instead of delaying the sandbox API. Network errors,
HTTP `408`, `429`, and `5xx` responses are retried with exponential backoff.
Other `4xx` responses are not retried because they normally indicate a
configuration or receiver error.
