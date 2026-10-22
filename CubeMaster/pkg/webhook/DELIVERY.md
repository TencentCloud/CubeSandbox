# Configurable persistent delivery (P3)

This stage installs delivery infrastructure in CubeMaster. It does **not** yet
publish real sandbox lifecycle events. P4/P5 must wire actual successful business
operations and their field mappings. There is no public test-event API.

## Configuration

Both switches default to false. The minimal example is in
`configs/single-node/cubemaster.yaml`. Set global `webhooks.enabled` and each
subscription's `enabled` explicitly. IDs are stable cluster-wide strings; events
are a nonempty subset of the six full lifecycle names. No wildcard, secret,
signature, dynamic API or hot reload is supported. Unknown fields, wrong types,
invalid URLs and invalid budgets disable only this module. Invalid main YAML and
other modules keep their existing error policy. Configuration logs redact the
entire webhook block and Redis passwords; watcher comparison retains real values.

The runtime takes a deep startup snapshot. Restart **all** Masters to apply a
change. A file watcher update does not change a running runtime, even if the new
webhook block is invalid. Existing requests and old instances cannot be recalled.
For sensitive revocation, globally disable all instances and finish deployment.
Reusing an old ID and identical URL can resume unfinished old tasks; use a new ID
when a new identity is required. There is no subscription generation history.

| Option | Default | Limit / relation |
| --- | --- | --- |
| ingress_capacity | 1024 | 1–4096 |
| workers | 4 | 1–32, one claim at a time per worker |
| request_timeout | 5s | 1ms–1m, whole HTTP operation |
| max_attempts | 3 | 1–1000; existing tasks retain their stored budget |
| lease | 15s | greater than 2 × operation_timeout + request_timeout + 2s; at most 5m |
| store.backend | redis | only production backend |
| store.operation_timeout | 2s | 1ms–10s, including pool wait and connection setup |
| store.max_active / max_idle | 8 / 4 | active 1–64; idle 0–active |
| store.max_records | 10000 | 1–1000000, includes retained terminal tasks |
| store.terminal_retention | 24h | 32s–30d, whole milliseconds |
| allow_http | false | separate opt-in for explicitly allowed private/local addresses |
| allowed_cidrs | empty | at most 64; allows private/loopback addresses only |
| subscriptions | empty | at most 64, each ID ≤1024 bytes, URL ≤4096 bytes |

`store.redis`, if omitted, copies the main `redis` connection settings, including
password, DB and Sentinel settings. If supplied, it is a **complete replacement**;
no address/credential merging occurs. Use `nodes` for standalone or `master_name`
and `sentinel_nodes` for Sentinel. The independent pool uses the webhook pool
limits. No startup PING is required. A temporarily unavailable Redis is retried in
the background without making Master startup fail. See [STORE.md](STORE.md) for
persistence, authentication, same-slot keys and unsupported Cluster routing.

Private HTTPS requires an explicit CIDR. Plain HTTP requires both `allow_http:
true` and a matching private/loopback CIDR, for example `127.0.0.0/8` for a local
receiver. A CIDR never disables TLS certificate or hostname validation. Link-local,
metadata, unspecified, multicast, documentation and other listed special-use
ranges remain blocked even under a broad allowlist. IPv4-mapped IPv6 is checked
as IPv4. Public IPv6 is limited to global-unicast allocation 2000::/3 minus
excluded special-use ranges. Mixed permitted/forbidden DNS results are rejected;
fresh connections dial the checked IP directly. Existing checked connections may
be reused. HTTP(S)_PROXY and ALL_PROXY are not used. Redirects are never followed.

## Delivery and shutdown

A single Writer matches subscriptions, maps and encodes once, then persists each
target snapshot. It visits all unconfirmed targets before retrying. Each event
has a 30s hard writer window; uncertain Put retries keep the same identity, URL,
bytes, budget and initial timestamps. Partial fan-out success is not rolled back.
Prune runs at most every 30s and removes at most 32 expired terminal records.
Capacity exhaustion rejects new work; unfinished tasks are not deleted.

Each worker claims one task and immediately checks lease headroom and the startup
subscription snapshot. Deleted/disabled/changed URL/removed event targets are
cancelled, never redirected to a new URL. Global enable with zero active
subscriptions still cancels old targets. Global disable creates no store, sender,
DNS or delivery loop and does not consume or prune backlog.

The Sender POSTs exact stored bytes with `Content-Type: application/json`,
`e2b-webhook-id`, and a new UUID `e2b-delivery-id` per actual attempt. It does not
re-encode or retry. No signature/version headers are sent. A signature-required
receiver remains incompatible and eventually fails within its finite budget.
2xx status is success; other statuses, timeout and network/security errors fail.
Reading is limited to 8 KiB of response body and 16 KiB response headers; a 2xx
response does not require receiving its entire body. Request/response bodies,
URLs and underlying network/storage error strings are not logged.

Claims consume persisted attempts even without HTTP. Lost Claim replies do not
provide a usable receipt. Lost Complete/Retry replies end processing of that
in-memory task. Recovery uses stored state and expiry; restarting never replenishes
attempts. Backoff is exponential with bounded jitter, capped at 30s. Corruption
stops admission and further scheduling for diagnosis; it does not delete/rebuild
keys. An already running call may finish while corruption is detected elsewhere.

Shutdown stops admission, lets the sole Writer drain for at most 5s, then stops
new claims/maintenance and waits for in-flight work within a shared 10s total
budget (or the caller's earlier deadline). Remaining calls are cancelled before
closing the pool and idle HTTP connections. The publisher channel is never
closed and Shutdown never consumes it. The runtime work context is independent
of the App parent context, so signal cancellation does not invalidate flush.
Hard process termination can recover only tasks already saved in Redis.

Acceptance is not persistence; a Redis acknowledgement is not disk/power-loss
proof; HTTP success is not saved completion. Buffered events can be lost, saved
events can be duplicated, finite attempts can expire before any HTTP, and order
is not guaranteed. Receivers must deduplicate by payload event ID. No exactly-once
or zero-loss guarantee is made.

## Resource and observation bounds

One writer, N workers, one maintenance loop and one parent-cancellation watcher
are fixed; shutdown adds one temporary join waiter. There is no per-event
application goroutine. HTTP/Redis library goroutines and sockets are additionally
bounded by concurrency, pool limits and timeouts.

The current captured structure has seven bounded strings (≤4096 bytes each), so
1024 buffered captures retain at most a conservative 28 MiB of string contents
plus structs/channel overhead; the maximum 4096 setting is 112 MiB. One event's
≤64 target table shares its immutable Event. Bodies are ≤256 KiB and records
≤512 KiB; worker encoding/transport copies and Redis allocation/index overhead
are additional. 10000 records can represent **4.88 GiB of record content alone**;
this is a count limit, not a suitable memory configuration for every deployment.
Provision appropriate Redis capacity, persistence, noeviction and access control;
this module never changes server settings. Tasks contain sensitive body/URL data.

Prometheus `cubemaster_webhook_operations_total{operation,result}` distinguishes
capture/ingress, unmatched/encoding failures, Put confirmation and unconfirmed
abandonment, claims, lease recovery, budget exhaustion, actual HTTP header writes,
HTTP status, result submission, failure/cancellation and prune removals. Labels
are finite categories/status codes, never IDs, URLs or arbitrary errors. Local
counts can miss server actions when replies are lost and are not audit history.

`*_claim_age_seconds` measures task creation age. `*_inflight` is local active
work. `*_ingress_depth`, `*_backlog` and `*_due_delay_seconds` are sampled every
30s; the latter is oldest scheduling due delay, **not oldest event age**.
`*_sample_available=0` invalidates cached queue values after a sampling error.
Shared queue gauges must not be summed across Masters. Runtime repetition does
not register duplicate collectors. Only one production Runtime per App is
supported; process-wide local gauges aggregate/observe that ownership.

## P4/P5 integration

Use `App.WebhookPublisher()` after startup, defaulting to `NopPublisher` before
installation. At an actual successful business boundary, construct bounded
`CaptureInput` from already available data and call Capture then TryPublish.
Publication outcomes are observations, never business errors. Add the required
business fields and pure background MapToProtocol mapping with those real
callers; do not call Encode, query Redis/DNS or wait for queue room in business
code. P3 synthetic created events do not validate real lifecycle success points.
