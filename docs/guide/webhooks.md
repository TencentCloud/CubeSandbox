# Sandbox lifecycle webhooks

[中文指南](../zh/guide/webhooks.md)

CubeMaster POSTs lifecycle notifications to operator-configured receivers. Static
subscriptions cover the whole cluster and are disabled by default; they are not
per-user SDK subscriptions. Webhooks observe existing business results; delivery
failure does not change them.

## Enable notifications

1. Prepare a JSON POST receiver that handles duplicates and returns 2xx promptly
   after accepting its downstream work.
2. Merge this block into each Master's configuration and replace the placeholder
   URL. Keep other platform settings, including business Redis.
3. Apply the same configuration and restart **every Master**. Running subscriptions
   do not hot-reload. Trigger a real sandbox operation and inspect receiver records
   and Master metrics.

```yaml
webhooks:
  enabled: true
  ingress_capacity: 1024
  workers: 4
  request_timeout: 5s
  max_attempts: 3
  store:
    max_records: 10000
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

Both global and subscription `enabled` must be true. Filters match full event
names exactly; wildcards and duplicate events are rejected. Subscription IDs must
be nonblank and unique; they identify subscriptions, not sandboxes.

| Setting under `webhooks` | Default | Accepted range / meaning |
| --- | --- | --- |
| `enabled` | `false` | Global switch |
| `ingress_capacity` | `1024` | 1–4096 captured events per Master |
| `workers` | `4` | 1–32 delivery workers per Master |
| `request_timeout` | `5s` | 1ms–1m for the whole HTTP operation |
| `max_attempts` | `3` | 1–1000 attempts, including the first |
| `store.max_records` | `10000` | 1–1000000 pending + inflight target tasks per Master |
| `allow_http` | `false` | Explicit private/local HTTP opt-in |
| `allowed_cidrs` | empty | At most 64 valid CIDRs for private/loopback destinations |
| `subscriptions` | empty | At most 64 subscriptions |
| `subscriptions[].enabled` | `false` | Subscription switch |
| `subscriptions[].id`, `.url`, `.events` | unset | Required; ID ≤1024 bytes, URL ≤4096 bytes, 1–6 distinct supported events |

These count limits do not bound process RSS. Global enable with no active
subscription does no capture or delivery work. Disabled operation skips
notification preparation; enabling still uses CPU and memory.

## Events and success boundaries

| Full event name | When CubeMaster observes success |
| --- | --- |
| `sandbox.lifecycle.created` | Public create receives the existing successful result and completes success-branch post-processing. Ordinary template fast-start remains `created`. |
| `sandbox.lifecycle.resumed` | Successful public create from a trusted user-snapshot template, or pause restoration after final proxy purge succeeds. Caller annotations/ID prefixes alone cannot classify a snapshot. |
| `sandbox.lifecycle.paused` | Cubelet local pause and Master `pausesnap.Complete` succeed. |
| `sandbox.lifecycle.checkpointed` | The executing snapshot job completes local work and persists READY, before its final query. A later query failure does not retract notification; querying READY does not publish again. |
| `sandbox.lifecycle.updated` | Public SetTimeout/Refresh returns a usable finite deadline, or the existing node network update succeeds. A best-effort spec write failure does not undo node network success. |
| `sandbox.lifecycle.killed` | The existing synchronous destruction call or asynchronous destroy handler succeeds, including existing tolerant/warning-only cases. Queue acceptance alone does not publish. |

Pause/checkpoint mean **local success** for both XFS and S3, not completed S3
upload, remote durability or cross-node recoverability. Later upload status does
not retract or repeat events. Failed create, pause, READY write or final resume
purge does not publish its corresponding success event. Internal create rollback,
pause teardown and resume timeout refresh add no user-facing killed/updated.
Mock bypasses are not real lifecycle notification entrypoints.

Create cancellation follows the existing business result. If cancellation wins
and the operation returns failure, later background success and internal rollback
emit no created/resumed/killed event. If business success returns first, subsequent
request cancellation before the notification hook does not suppress created or
snapshot resumed. Delivery remains best effort.

SetTimeout accepts positive seconds, `0` (immediate deadline) and `-1` (never
timeout); `-1` emits no timeout-only event. Refresh requires positive seconds,
rejecting `0` and negative values. A finite successful response with `EndAt > 0`
can publish even when the provider is absent or fails, because the existing
fallback can supply its deadline. **This does not confirm Redis HSET or XADD
success.** Resume's internal refresh does not publish updated. Repeated successful
operations may each produce a new event.

Killed follows original success rules for missing nodes, nil/not-found responses
and warning-only cleanup. It adds no independent proof of physical destruction
or completed binding removal. Business retries keep their original node calls;
notifications introduce no completion gate or producer deduplication.

## Receive and deduplicate

| Header | Value |
| --- | --- |
| `Content-Type` | `application/json` |
| `e2b-webhook-id` | Configured subscription ID |
| `e2b-delivery-id` | Fresh UUID per delivery attempt |

Payload `id` and exact body remain stable across retries and subscription fanout.
Deduplicate by payload `id`, with subscription scope if needed. Delivery ID
identifies an attempt, not an idempotency key. Save required downstream work
before acknowledging; tolerate concurrent, unordered and duplicate requests. A
receiver may accept a request whose response is lost, causing retry.

Any 2xx is success. Non-2xx (including 3xx/4xx/5xx), network errors and timeouts
consume the finite budget. Claim consumes an attempt, so cancellation, identity
creation or address checks can consume budget before HTTP headers are written.
Exponential backoff includes jitter and is capped at 30s. Exhausted tasks are
not automatically replayed when the receiver recovers.

## Payload compatibility

The feature's first release uses the existing **`v2` payload**, without claiming
complete E2B field/operation parity. Paths below are relative to the JSON root.

| Field | Source and empty/omission rule | Receiver impact |
| --- | --- | --- |
| `id` | UUID captured at success | Stable event deduplication key |
| `version` | Always `"v2"` | Not the feature release/signature version |
| `type` | One of the six full names | Exact matching |
| `timestamp` | Capture time, UTC RFC3339Nano | Not HTTP send time or execution start |
| `sandbox_id` | Actual response/request sandbox ID | Not an E2B execution ID |
| `sandbox_template_id` | Trusted create resolution or already-read spec; otherwise `""` | Timeout/network/killed do not query to fill it |
| `sandbox_execution_id` | `""`; no equivalent execution identity | Strict UUID/required-value decoders may reject |
| `sandbox_build_id` | `""`; no trusted build identity | No job/snapshot ID substitution |
| `sandbox_team_id` | `""`; no equivalent team | Strict UUID/team validation may reject |
| `event_category` | `""`, key retained | No invented legacy classification |
| `event_label` | `""`, key retained | No invented legacy label |
| `events_ttl_days` | Omitted | No event-history retention service |
| `event_data` | Omitted when empty | Do not assume it always exists |
| `event_data.sandbox_metadata` | Trusted current public create input before template merge; nil/unknown omitted, known empty `{}` | Snapshot create may carry current input; later operations do not inherit it |
| `event_data.execution` | Only paused/killed; at least `{"started_at":""}` | Requires tolerant partial-data decoding |
| `event_data.execution.started_at` | `""`; no reliable execution start | Empty string is not valid RFC3339 |
| `event_data.execution.execution_time` | Omitted; no reliable duration | No fabricated zero; unsuitable for billing |
| `event_data.execution.vcpu_count` | Available single-container `cubebox` CPU, exact positive integer; otherwise omitted | `1000m` → 1; `1500m` omitted |
| `event_data.execution.memory_mb` | Available single-container `cubebox` memory, ceil bytes then integer MiB; otherwise omitted | `1Gi` → 1024, `512M` → 488, `1.5Mi` → 1 |
| `event_data.set_timeout` | Captured finite response EndAt, UTC RFC3339 seconds precision | Can be fallback; no persistence confirmation or `-1` encoding |
| `event_data.network_egress.allowed_cidrs` | Requested IPv4 IP/CIDR; IP → `/32`, CIDR normalized; empty `null` | Not a complete policy diff; explicit DNS IPs remain |
| `event_data.network_egress.denied_cidrs` | Requested IPv4 denies; internet false overrides with `["0.0.0.0/0"]`; empty `null` | Internet true/nil uses this request's DenyOut |
| `event_data.network_egress.allowed_domains` | AllowOut domains/wildcards, lowercased and trailing dot removed; empty `null` | No L7 Rules/Inject/credentials or node-injected DNS |
| `event_data.pause_mode` | `"full"` on the current confirmed pause path | Not internal snapshot_type |
| `event_data.resume_mode` | `"restore"` on current pause/user-snapshot restore paths | No claim about future paths |
| `event_data.kill_reason` | Explicit reason preserved; known user request `"request"`, timeout `"timeout"`, absent source `"unknown"` | No internal error text or extra cleanup events |

Metadata excludes exact `host-mount`, `X-Caller` and the `cube.master.` namespace;
ordinary user keys containing token/secret/password remain user data. Treat
receiver logs accordingly. Create capture allows 64 user entries, 16 KiB total
key/value bytes, keys ≤256 bytes and values ≤4096 bytes. Unknown metadata is not
recovered from merged labels, caches or private stored fields. Oversize input
drops the whole notification without truncation or business failure.

Resource fields are independent of missing time. Other instance types, multiple
containers, invalid/fractional CPU and unavailable quantities are not guessed;
current killed producers have no resource source. Network projection does not
represent IPv6 policies (rejected by the current node), all L7-only changes or
internal DNS/private rules. No E2B default DNS is synthesized. Empty categories
are `null`, including protobuf round trips of empty lists; `[]`-requiring consumers
need adaptation.

There is no secret, configurable authentication header, signature,
`e2b-signature`, `e2b-signature-version` or other version header. Headers and IDs
do not authenticate the sender. Signature-required receivers are incompatible;
retain their security requirements when choosing an integration. Dynamic
subscription CRUD, per-user subscriptions and a public test-event API are absent.

### Encoding example

This is the existing `TestEncodeSixEvents` created fixture with `captureInput`
from `capture_test.go`, checked against the current encoder. IDs/time are test
data, not production observations. Unknown metadata omits `event_data`; a public
create with known empty metadata instead includes `sandbox_metadata: {}`.

```json
{
  "id": "d12727fc-ab97-43d8-80cf-556cc1215017",
  "version": "v2",
  "type": "sandbox.lifecycle.created",
  "timestamp": "2026-10-14T12:00:00Z",
  "event_category": "",
  "event_label": "",
  "sandbox_id": "sb-1",
  "sandbox_execution_id": "",
  "sandbox_template_id": "tpl-1",
  "sandbox_build_id": "",
  "sandbox_team_id": ""
}
```

## Capacity, loss and shutdown

Each Master owns two bounds, shared by all its subscriptions:

- Ingress counts captured events awaiting the writer. Full ingress drops the event.
- `store.max_records` counts target tasks, including pending retries and inflight
  HTTP. An event matching three subscriptions needs three records. Fanout can
  partially succeed; full task capacity drops that target without waiting. Retry
  reuses its record; success/exhaustion/cancellation releases it.

Each target task stores at most 256 KiB of encoded body. Target copies and
workers/transports also consume memory, so these bounds do not cap process RSS.

Notification state is process-local memory behind `DeliveryStore`. Restart, crash
or shutdown can lose remaining work. Masters neither share notification backlog
nor take over each other's tasks. There is no disk/Redis persistence, strict
ordering, exactly-once, zero-loss, history/query or replay API. Redis persistence/HA
is deferred work; business Redis remains a platform dependency. Some webhook
integration tests deliberately exercise business Redis → memory Runtime → HTTP;
this is not a notification Redis backend.

Webhook shutdown stops admission and tries to drain within **one two-second total
budget**, or the caller's earlier deadline. It cancels notification HTTP/waits and
discards remaining work. It neither guarantees all delivery nor limits the whole
App/systemd shutdown to two seconds. SIGKILL bypasses drain.

## HTTP and configuration safety

Public destinations require HTTPS with normal certificate/hostname checks.
Private/loopback HTTPS also requires a matching explicit `allowed_cidrs`. For
local HTTP testing, merge this exception into `webhooks` and change the
subscription URL to `http://127.0.0.1:18096/events`:

```yaml
allow_http: true
allowed_cidrs: ["127.0.0.1/32"]
```

HTTP requires both opt-in and a matching private/loopback CIDR; public HTTP remains
blocked. Use the narrow reachable receiver subnet for remote Masters. CIDRs never
disable TLS or unblock prohibited special-use addresses such as metadata,
link-local, unspecified, multicast or documentation ranges. All DNS answers must
pass policy; mixed allowed/forbidden answers are rejected. New connections dial
checked IPs directly; checked connections may be reused. Environment proxies,
redirects and automatic POST replay are disabled. URL user credentials/fragments,
unknown keys and wrong types are rejected. Responses are bounded to 8 KiB body
reads and 16 KiB headers.

Invalid webhook configuration disables only notifications, with webhook values
redacted in diagnostics. Invalid main YAML and other modules keep their existing
error policy; this does not promise a separate generic configuration logging fix.
API health alone does not prove webhooks are enabled.

For sensitive revocation, stop every old delivery instance before applying the
new startup configuration; mixed configurations can still send to old targets.
An already-started request cannot be recalled. Restart discards old memory
backlog; new subscriptions do not receive historical events.

## Diagnose and verify

| Metric | Meaning |
| --- | --- |
| `cubemaster_webhook_operations_total{operation,result}` | Local capture/ingress, mapping/encoding, put/claim/complete/retry, send/HTTP/status and terminal outcomes |
| `cubemaster_webhook_claim_age_seconds` | Histogram of age since target task creation at claim, including retry; not execution duration |
| `cubemaster_webhook_ingress_depth` | Writer-observed local buffer depth; can lag admission, not target task backlog |
| `cubemaster_webhook_inflight` | Local active worker tasks, including pre-send checks |

Useful counter pairs: `capture/oversized`, `ingress/full`, `event/unmatched`,
`put/capacity`, `http/attempt`, `http/success`, `terminal/failed`,
`terminal/cancelled`, `task/shutdown_discarded`, `ingress/unprocessed_shutdown`.
HTTP attempts count header writes, not claims or confirmed receiver processing.
Finite diagnostic labels are not persistent delivery history. There are no
shared Redis backlog gauges.

For missing events, check both startup switches, exact filters, actual business
success, capture/ingress drops, mapping/put outcomes, then receiver HTTP status,
timeout and address/TLS policy. Check process restarts and exhausted budgets.
Do not weaken TLS or change business behavior to obtain notifications. Trigger
real actions using [lifecycle operations](./lifecycle.md).

The repository [webhook test runbook](https://github.com/TencentCloud/CubeSandbox/blob/master/tests/e2e/webhook/README.md)
provides the bounded local receiver, environment example, component checks and
real SDK six-event command. The [SDK test README](https://github.com/TencentCloud/CubeSandbox/blob/master/tests/e2e/sdk_compat/README.md)
describes existing requirements and platform preflight. Use an existing matching
toolchain. Receiver/framework checks and collection do not certify deployed
Master/Cubelet, XFS/S3, TLS or faults. Run prepared applicable scenarios, preserve
first failures and distinguish skipped, collection-only and unrun items.
Documentation checks are not lifecycle acceptance.

## Migrate an earlier PR trial configuration

Remove `store.backend`, `store.redis`, `lease`, old store pool keys,
`operation_timeout` and `terminal_retention`; keep only `store.max_records` inside
the notification store block. Removed keys disable notifications through strict
validation; they do not silently select memory or retain durability. No automatic
Redis-to-memory backlog migration or replay exists. Review both switches,
subscriptions and capacity, then restart every Master. Leave platform business
Redis configuration unchanged.
