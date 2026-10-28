# Sandbox lifecycle webhooks

[中文](README_zh.md)

CubeMaster sends lifecycle notifications to statically configured HTTP endpoints.
Subscriptions are cluster-wide, configured by the operator and disabled by
default. They are not per-user subscriptions managed through the SDK.

## Enable delivery

1. Deploy a receiver that accepts JSON POST requests, handles duplicates and
   returns a 2xx status promptly after durably accepting the notification.
2. Configure `webhooks` in every CubeMaster configuration. Replace the example
   endpoint and Redis connection below with your actual services.
3. Restart **all Masters** to apply the same configuration. Hot reload is not
   supported. Trigger a real sandbox operation through the existing API/SDK
   and inspect the receiver and Master webhook metrics.

```yaml
webhooks:
  enabled: true
  workers: 4
  request_timeout: 5s
  max_attempts: 3
  store:
    backend: redis
    redis:
      nodes: "redis.internal:6379"
      password: "REPLACE_WITH_REDIS_PASSWORD"
      db_no: 7
  subscriptions:
    - id: lifecycle-receiver
      enabled: true
      url: https://receiver.example.com/cubesandbox/events
      events:
        - sandbox.lifecycle.created
        - sandbox.lifecycle.resumed
        - sandbox.lifecycle.paused
        - sandbox.lifecycle.checkpointed
        - sandbox.lifecycle.updated
        - sandbox.lifecycle.killed
```

Both global and subscription switches must be enabled. Event filters use exact
names; wildcards are unsupported. The URL above is a placeholder, not a live
receiver.

`store.redis` is a complete connection override. If omitted, the main Redis
settings are copied. Use a separate Redis service when webhook outages/OOM must
be isolated from business storage; a separate logical DB does not isolate server
failure or resource contention. Configure Redis persistence, storage capacity,
access control and `noeviction` yourself; webhook does not change server settings.

Public HTTPS uses normal certificate verification. Private/loopback destinations
require `allowed_cidrs`. Local HTTP testing additionally requires `allow_http`:

```yaml
# Merge into webhooks; also set the subscription URL to the local receiver.
allow_http: true
allowed_cidrs: ["127.0.0.0/8"]
```

CIDRs do not disable TLS verification or unblock prohibited special-use
addresses. Redirects and environment HTTP proxies are not used. See
[delivery configuration](DELIVERY.md#configuration) for limits and address rules,
and the [test runbook](../../../tests/e2e/webhook/README.md) for a local receiver.

## Events

| Event suffix (`sandbox.lifecycle.`) | Published after |
| --- | --- |
| `created` | Successful create completion and final post-processing |
| `resumed` | Successful pause restoration or a trusted user-snapshot restore; ordinary template fast-start remains `created` |
| `paused` | Local pause and Master pause completion |
| `checkpointed` | Local snapshot completion and final job READY persistence |
| `updated` | Confirmed finite timeout writes or supported successfully applied network updates |
| `killed` | Completed destruction and required cleanup, including supported synchronous/asynchronous paths |

Pause/checkpoint notifications describe **local success**, not completed S3
upload or remote durability. Later remote upload failure does not retract the
event. Internal refresh/rollback/pause teardown must not create user-visible
update/destruction notifications. Timeout `-1` alone does not emit `updated`;
L7-only network changes are not comprehensively represented. See
[DELIVERY.md](DELIVERY.md) for precise completion boundaries and field projection.

## Receiver contract

Requests use `Content-Type: application/json`, `e2b-webhook-id` (subscription ID)
and `e2b-delivery-id` (fresh UUID per actual attempt). The JSON envelope contains
`id`, `version`, `type`, `timestamp`, sandbox identity fields and event-specific
`event_data`; see [encode.go](encode.go) and [protocol.go](protocol.go).
Unavailable identity fields may be empty; receivers requiring complete E2B
execution/team/build identities are not fully compatible. No signature or
version HTTP header is sent.

- Deduplicate using payload `id`, with subscription scope if needed. Retries keep
  the same event ID and body; delivery IDs identify individual attempts.
- Do not depend on ordering, exactly-once delivery or zero loss. In-memory events
  can be lost before persistence; saved tasks can be delivered more than once.
- Return 2xx for success. Other statuses, network errors and timeouts consume the
  finite retry budget. Claims can consume attempts even before HTTP is sent.
- Persist downstream work before acknowledging when needed. After terminal
  budget exhaustion there is no automatic replay when the receiver recovers.

The business path captures bounded event data and performs a nonblocking enqueue.
Redis persistence, encoding and HTTP delivery run in the background. Full queues,
invalid/oversize captures or storage faults can lose notifications instead of
waiting in the business request. This does not promise zero CPU/memory overhead
or unaffected performance under every workload.

## Implemented and unsupported

| Capability | Status |
| --- | --- |
| Six lifecycle event types | Implemented, with the boundaries above |
| Static multiple subscribers and event filtering | Implemented; all Masters require restart on changes |
| Redis persistence, bounded retries, leases and competing workers | Implemented; standalone and Sentinel supported |
| TLS/address checks, bounded queues and shutdown, metrics | Implemented |
| Dynamic subscription CRUD / per-user subscription API | Not implemented |
| Signatures, configurable authentication headers, public test-event API | Not implemented |
| Delivery history/query/replay API | Not implemented |
| Redis Cluster routing | Unsupported |
| Total ordering, exactly-once, zero-loss or physical power-loss guarantee | Not provided |
| Complete E2B field/operation parity | Not provided; see compatibility details |

## Operations and validation

Monitor `cubemaster_webhook_operations_total` and the webhook queue/inflight
metrics. Counters are diagnostic categories, not a durable delivery history.
Check switches, event filters and final business outcome if an event is missing;
then inspect capture, persistence, claim and HTTP outcomes. Invalid webhook
configuration disables this module; check startup logs rather than assuming a
healthy API means webhook delivery is enabled.

For sensitive subscription revocation, stop all old delivery instances before
applying changes. Global disable leaves saved backlog untouched; changed or
removed targets are cancelled by enabled runtimes using the new startup snapshot.
An HTTP request already started cannot be recalled. Preserve queue state when
investigating OOM/corruption; restoring Redis capacity alone may not restart
scheduling. See [DELIVERY.md](DELIVERY.md) and [STORE.md](STORE.md).

[Acceptance summary](END-TO-END-VALIDATION.md) distinguishes component, real
XFS/S3, recovery and latency evidence from untested guarantees. The serial API
experiment demonstrates isolation under tested delivery faults, not a general
high-concurrency performance SLO. Reproduction and explicit disruptive-test
opt-ins are in the [test runbook](../../../tests/e2e/webhook/README.md).

For portable test setup, copy and edit the [test environment example](../../../tests/e2e/webhook/env.example).
Fault fixtures use your dedicated run root and inventoried resources; they do not
require the original developer's installation paths.
