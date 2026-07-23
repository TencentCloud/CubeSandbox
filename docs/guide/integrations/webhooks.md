---
title: CubeSandbox Webhook Integration Guide
author: dujunjin
date: 2026-07-23
tags:
  - integration
  - webhook
  - observability
lang: en-US
---

# CubeSandbox Webhook Integration Guide

[中文文档](../../zh/guide/integrations/webhooks.md)

CubeAPI can asynchronously notify HTTP endpoints when a sandbox lifecycle
operation succeeds. This avoids polling and lets an agent orchestrator, an
operations system, or a chat bot react to state changes in near real time.

## Configuration

Set these environment variables before starting `cube-api`:

| Variable | Default | Description |
| --- | --- | --- |
| `WEBHOOK_URLS` | unset | Comma-separated HTTP(S) endpoint URLs |
| `WEBHOOK_EVENTS` | `sandbox.created,sandbox.deleted,sandbox.paused,sandbox.resumed` | Event allowlist; `*` accepts every structured event |
| `WEBHOOK_SECRET` | unset | Optional HMAC-SHA256 secret |
| `WEBHOOK_QUEUE_SIZE` | `1024` | Bounded queue capacity |
| `WEBHOOK_MAX_RETRIES` | `3` | Retries after the initial request |
| `WEBHOOK_RETRY_BASE_MS` | `250` | Initial exponential backoff delay |
| `WEBHOOK_REQUEST_TIMEOUT_SECS` | `5` | Timeout for each request |

The `CUBE_API_WEBHOOK_*` aliases are also accepted. Multiple URLs receive the
same selected event set. Leave `WEBHOOK_URLS` unset to disable delivery.

Example local configuration:

```bash
export WEBHOOK_URLS='http://127.0.0.1:8099/webhook,https://ops.example.com/cube'
export WEBHOOK_EVENTS='sandbox.created,sandbox.paused,sandbox.resumed,sandbox.deleted'
export WEBHOOK_SECRET='replace-with-a-random-secret'
```

Use HTTPS for non-loopback endpoints. HTTP is useful for the local example only.

## Event and payload

The following lifecycle events are emitted after the corresponding CubeMaster
operation succeeds:

- `sandbox.created`
- `sandbox.deleted`
- `sandbox.paused`
- `sandbox.resumed`

Each POST has `Content-Type: application/json`. The JSON object is the structured
CubeAPI event and includes at least:

```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-23T12:00:00.000Z",
  "level": "info",
  "sandbox_id": "sbx-example",
  "template_id": "tpl-example"
}
```

`template_id` is currently available for creation events. Future event fields
may be added without changing the required fields; receivers should ignore
unknown fields.

## Delivery semantics

The API handler places a subscribed event into a bounded in-memory queue with
`try_send`; it does not wait for the receiver. A background worker sends the
same serialized body to all configured URLs. When the queue is full, the event
is dropped and CubeAPI writes a warning with the event name. This protects
sandbox create/pause/resume/delete latency and memory usage.

HTTP 2xx responses are successful. Network errors, timeouts, HTTP 408, 429, and
5xx responses are retried with exponential backoff. Other 4xx responses are
logged and not retried. The retry count is bounded by CubeAPI, and delivery
failure never changes the result of the sandbox API request.

## HMAC-SHA256 verification

When `WEBHOOK_SECRET` is set, CubeAPI adds:

```text
X-Cube-Signature-256: sha256=<lowercase-hex-digest>
```

The digest is HMAC-SHA256 over the exact raw request body. Verify before parsing
JSON and compare in constant time:

```python
import hashlib
import hmac

expected = "sha256=" + hmac.new(
    WEBHOOK_SECRET.encode(), raw_body, hashlib.sha256
).hexdigest()
if not hmac.compare_digest(
    request.headers.get("X-Cube-Signature-256", ""), expected
):
    return unauthorized()
```

The runnable receiver at
[`examples/webhook-receiver`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver)
implements the complete check and returns `401` for an invalid signature.

## Local verification

```bash
python3 examples/webhook-receiver/receiver.py \
  --port 8099 \
  --secret replace-with-a-random-secret

WEBHOOK_URLS=http://127.0.0.1:8099/webhook \
WEBHOOK_SECRET=replace-with-a-random-secret \
cargo run --manifest-path CubeAPI/Cargo.toml
```

Create, pause, resume, and delete a sandbox with the normal SDK or REST API and
confirm that the receiver prints the four event names. The receiver example
also includes a `curl` command for testing the signature without a cluster.

## WeCom and generic HTTP alerting

Keep CubeAPI's webhook endpoint as a small internal adapter. After verifying the
CubeSandbox signature, map the event into the target format. A WeCom bot expects
an envelope such as `{"msgtype":"markdown","markdown":{"content":"..."}}`;
generic alerting systems may instead accept the original JSON with their own
Authorization header. Apply independent timeouts and retries to this second
hop, and do not reuse the CubeSandbox signature as downstream authentication.

See the bilingual receiver README for a complete standard-library forwarding
snippet.
