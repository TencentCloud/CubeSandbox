---
title: Webhook Event Notifications
lang: en-US
---

# Webhook Event Notifications

CubeSandbox can push **sandbox lifecycle events** to your own HTTP endpoints as
they happen, so upstream agent orchestrators, ops platforms, or IM tools no
longer need to poll the API for state changes.

A runnable receiver and a WeCom bridge live in
[`examples/webhook-receiver/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/webhook-receiver).

## Supported events

| Event | Fired when | Payload fields |
|---|---|---|
| `sandbox.created` | A sandbox is created | `sandbox_id`, `template_id` |
| `sandbox.paused` | A sandbox is paused | `sandbox_id` |
| `sandbox.resumed` | A sandbox is resumed | `sandbox_id` |
| `sandbox.deleted` | A sandbox is destroyed | `sandbox_id` |

## Configuration

Webhooks are declared in a config file (YAML / JSON / TOML) referenced by the
`--webhook-config` flag or the `CUBE_API_WEBHOOK_CONFIG` environment variable.
When neither is set, webhooks are disabled and there is zero overhead.

```yaml
# webhooks.yaml
webhooks:
  - url: "http://127.0.0.1:9100/webhook"
    events:
      - sandbox.created
      - sandbox.paused
      - sandbox.resumed
      - sandbox.deleted
    secret: "my-shared-secret"   # optional
    timeout_ms: 5000             # optional (default 5000)
    max_retries: 3               # optional (default 3)
```

```bash
cube-api --webhook-config /path/to/webhooks.yaml
# or
CUBE_API_WEBHOOK_CONFIG=/path/to/webhooks.yaml cube-api
```

You can register **multiple endpoints**, each subscribing to a different subset
of events. On startup CubeAPI logs `webhook event notifications enabled` with
the endpoint count.

| Field | Required | Description |
|---|---|---|
| `url` | yes | HTTP endpoint that receives the `POST`. |
| `events` | yes | Event types this endpoint subscribes to. |
| `secret` | no | Shared key for HMAC-SHA256 signing (see below). |
| `timeout_ms` | no | Per-request timeout in milliseconds. Default `5000`. |
| `max_retries` | no | Retries after the first attempt. Default `3`. |

## Management API

Endpoints can also be managed at runtime over HTTP — no restart required. This
is what the WebUI uses to let users add/remove webhooks from the dashboard.

| Method | Path | Description |
|---|---|---|
| `GET` | `/webhooks` | List registered endpoints (secrets masked). |
| `POST` | `/webhooks` | Register an endpoint; the server assigns its `id`. |
| `DELETE` | `/webhooks/{id}` | Remove an endpoint by id. |

```bash
# Register a webhook
curl -X POST http://localhost:3000/webhooks -H 'Content-Type: application/json' -d '{
  "url": "http://127.0.0.1:9100/webhook",
  "events": ["sandbox.created", "sandbox.deleted"],
  "secret": "my-shared-secret"
}'
# → 201 { "id": "8f3a…", "url": "…", "has_secret": true, ... }

# List, then delete by id
curl http://localhost:3000/webhooks
curl -X DELETE http://localhost:3000/webhooks/8f3a…   # → 204
```

The response never contains the `secret` value — only `has_secret: true|false`.

::: warning Runtime changes are in-memory and per-instance
Endpoints added/removed via this API are **not persisted**: they are lost on
restart. The config file provides the durable startup set. (Persisting runtime
changes would require a datastore and is out of scope here.)

In an **HA deployment (multiple CubeAPI replicas behind a load balancer)** this
API mutates only the replica that served the request, so replicas diverge — an
endpoint registered on one replica will not receive events handled by another.
For HA, treat the **config file as the declarative source of truth** (identical
on every replica) and consider the runtime CRUD API per-instance and
non-authoritative. Fleet-consistent runtime management is part of the durable
control-plane consumer follow-up.
:::

## Payload

Each event is delivered as an HTTP `POST` with `Content-Type: application/json`
and these headers:

| Header | Meaning |
|---|---|
| `X-Cube-Event` | The event type, e.g. `sandbox.created`. |
| `X-Cube-Delivery` | A unique id for this delivery. It stays the **same across retries**, so a receiver can deduplicate if a delivery is retried after it was already processed. |
| `X-Cube-Signature` | HMAC-SHA256 signature, present only when a `secret` is configured (see below). |

The body always contains `event`, `timestamp` (RFC 3339, UTC), and
`sandbox_id`, plus any additional context such as `template_id`.

```json
{
  "event": "sandbox.created",
  "timestamp": "2026-07-21T08:30:12.481Z",
  "sandbox_id": "sbx-abc123",
  "template_id": "base"
}
```

## Delivery semantics

- **Non-blocking.** Events are enqueued and delivered from background tasks.
  Sandbox API requests never wait on webhook delivery, so a slow or unreachable
  receiver can never delay or fail a create/pause/resume/delete call.
- **Bounded queue.** The in-memory queue is capped. Under sustained overload
  (receivers not keeping up) the newest events are dropped rather than growing
  memory without limit — a dropped event is logged at `warn` (throttled)
  instead of risking an OOM that loses everything.
- **Isolated.** Each (event, endpoint) delivery runs independently — one slow
  endpoint does not hold up others. Concurrent in-flight deliveries are capped
  so a burst cannot open an unbounded number of connections at once.
- **Retried.** Failed deliveries (connection errors, timeouts, or non-2xx
  responses) are retried with exponential backoff (1s, 2s, 4s, …) up to
  `max_retries`. The final give-up is logged at `error` level.
- **Drained on shutdown.** On a graceful stop (SIGTERM, e.g. a rolling
  restart), queued and in-flight deliveries are drained — retry backoff is cut
  short and deliveries are given a bounded grace period to finish — so a normal
  deploy does not drop in-flight webhooks.
- **At-most-once across a hard crash.** Deliveries are held in memory, so a
  hard kill (SIGKILL / OOM / node failure) can still lose queued and in-flight
  events. The durable record remains the
  [structured event log](./sandbox-logs.md); durable at-least-once delivery
  across a crash is planned as a follow-up (a control-plane event-stream
  consumer, aligned with the control/data-plane separation on the roadmap).

Your receiver should respond `2xx` quickly and do any heavy work
asynchronously, and **deduplicate on `X-Cube-Delivery`** (stable per event) —
at-least-once retries mean an event may be delivered more than once.

### Ordering

Deliveries are **not** globally ordered: each (event, endpoint) is delivered by
an independent task, so a retried `sandbox.created` can arrive *after* the
`sandbox.deleted` for the same sandbox. Every payload carries a monotonic
`timestamp`, so a receiver should **reconcile by `timestamp`** — track the
latest state per `sandbox_id` and ignore an event whose `timestamp` is older
than one it has already applied (e.g. drop a `created` that arrives after a
newer `deleted`). Strict in-order delivery is planned as part of the durable
control-plane consumer (see below).

## Tuning

The delivery subsystem's defaults are production-safe; override per deployment
via environment variables (no rebuild needed):

| Env var | Default | Meaning |
|---|---|---|
| `CUBE_API_WEBHOOK_QUEUE_CAPACITY` | `10000` | In-memory event queue size before overload drops the newest events. |
| `CUBE_API_WEBHOOK_DRAIN_GRACE_SECS` | `25` | Max time shutdown waits for in-flight deliveries to drain. Keep below your orchestrator's termination grace period. |
| `CUBE_API_WEBHOOK_MAX_CONCURRENCY` | `256` | Ceiling on concurrent in-flight delivery requests. |

Event loss is surfaced in logs: a full queue logs a throttled `warn`
(`webhook: queue full, dropping event`) and a delivery that exhausts its
retries logs an `error` (`webhook delivery giving up after exhausting retries`).

## Signature verification

When `secret` is set, CubeAPI signs the **raw request body** with HMAC-SHA256
and sends the result in the `X-Cube-Signature` header:

```
X-Cube-Signature: sha256=<hex-digest>
```

Verify it against the exact bytes you received (do not re-serialize the JSON
first — key ordering may differ):

```python
import hashlib, hmac

def verify(secret: str, body: bytes, header: str) -> bool:
    expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, header)
```

Use a constant-time comparison (`hmac.compare_digest`) to avoid timing attacks.

## Integrating with WeCom / generic alerting

A small bridge can turn events into WeCom (Enterprise WeChat) group-bot
messages. Create a group bot, copy its webhook URL, and run the example bridge:

```bash
WECOM_BOT_URL="https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxxxxxxx" \
CUBE_WEBHOOK_SECRET=my-shared-secret \
python3 examples/webhook-receiver/wecom_bridge.py
```

The same shape applies to Slack, Feishu, PagerDuty, or any HTTP alerting
system: receive the event, verify the signature, and forward a formatted
message to the target API.
