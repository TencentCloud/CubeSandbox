# Configurable persistent lifecycle delivery (P3–P5)

CubeMaster publishes created, resumed, paused, checkpointed, supported updated
and completed killed events through the same P3 persistent pipeline.
There is no public test-event API.

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

For P4, select `sandbox.lifecycle.created`, `sandbox.lifecycle.resumed`,
`sandbox.lifecycle.paused`, and/or `sandbox.lifecycle.checkpointed` in each
subscription's events list. The default switches remain off.

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

P4 captures retain seven common strings (≤4096 bytes each), up to 16 KiB of
metadata key/value bytes (64 entries; keys ≤256, values ≤4096), two resource
strings (≤128 each) and two mode strings (≤16 each). Content alone is bounded
by approximately 45 MiB at capacity 1024, or 180 MiB at 4096; structs, map
buckets/string headers and channel storage are additional. Preparation and
Capture temporarily hold separate bounded copies. Oversize input drops the
whole notification and increments capture-too-large; it never truncates values. One event's
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

## P4 lifecycle integration and compatibility

App installs the same P3 Publisher in sandbox and templatecenter through atomic
holders. No second Runtime, event goroutine, synchronous persistence, encoding,
resource parsing or remote lookup is added to business publication.

- `created`: final create proxy/spec post-processing succeeds and wins the
  existing response-once decision. Proxy errors or cancellation winning that
  decision suppress publication; spec/hook warn-only errors still permit it.
  MockCreateDirect remains a bypass with no event.
- `resumed`: trusted user-snapshot kind from existing HTTP template resolution,
  or successful pause restoration after the final proxy purge check. Both are
  memory restore paths (`resume_mode=restore`). Template fast-start alone is
  still created. Internal callers without a trusted resolution context default
  to created; no additional classification lookup is made.
- `paused`: Cubelet local completion plus `pausesnap.Complete`; `pause_mode=full`.
- `checkpointed`: the PENDING claim winner, after local commit, replica/snapshot
  state and final job READY persistence. Original template identity is copied
  before snapshot annotations are rewritten. READY reads, template commits and
  upload callbacks have no producer. A later final-read failure does not retract
  a successfully committed READY event.

XFS and S3 follow the same local success rules. Empty export UUIDs, uploads in
progress and later remote failures do not suppress or generate another event.
These events do not assert remote durability or cross-node recoverability.

Metadata is captured from incoming labels before internal injection/template
merge. Exact `host-mount`, `X-Caller`, and the reserved `cube.master.` namespace
are excluded; ordinary user keys containing token/secret/password are retained.
Annotations, env vars, network credentials and template labels are not exported.
The internal request field is excluded from API JSON. Only sandboxspec stores
its provenance extension inside existing request_json, with no new table or
extra query. Known nil is omitted; known empty is `{}`. Old specs, internal
requests without provenance and template fallback have unknown metadata, which
is omitted. CubeAPI has already moved host-mount out of labels; upstream
information that was collapsed cannot be recovered. Template request cloning
does not turn template-owned labels into known user metadata.

Paused execution resources are mapped only for a single-container `cubebox`
spec. Cubelet cgroup.GetResourceWithOverhead directly assigns its CPU/memory to
VM quantities in that branch; MatchVMSnapshotSpec passes CPU and getMbSize to
VmSnapshotSpec; genCgroupAnnotationOpt supplies it to the VM annotation.
Other instance types add overhead/select snapshots and are not inferred.
Multiple container quotas are not summed by the webhook mapper.
CPU must be an exact positive integer (1000m=1; 1500m omitted). Memory follows
Cubelet's ceil-to-byte then integer MiB conversion (1Gi=1024, 512M=488,
1.5Mi=1). Invalid, nonpositive, overflow/saturated quantities are omitted per
field and counted as mapping cpu_unavailable/memory_unavailable. No reliable
execution start exists in the current spec: started_at remains empty and
execution_time is absent. Missing time does not discard available resources;
no execution/build/team identity is invented. Strict receivers requiring those
identities, a valid started_at, or complete numeric fields require adaptation.
No new execution tracking model is introduced.

Create failover carries OriginCreateFailover in the actual queued Task context,
including retries and the handler's WithoutCancel. Resume timeout refresh alone
carries OriginResumeTimeoutRefresh; outer resumed retains lifecycle origin.
Pause Update and origin-package cleanup carry OriginPauseTeardown locally.
Cubelet keep_tombstone destruction stays inside its existing RPC, without a new
proto field or Master ordinary-delete call. P5 PublishContext checks the trusted
origin; deletion captures it before queueing and preserves it independently of
later logging/context wrappers.

See [P4-VALIDATION.md](P4-VALIDATION.md) for this run's commands, evidence,
measurements, skips and P5/P6 handoff. Signature, API, backend, retry, loss and
ordering limitations above remain unchanged.

## P5 updates and completed destruction

App installs its same Publisher into task as well as sandbox/templatecenter.
No second Runtime, new queue, notification query, per-event goroutine, proto
change or durable producer deduplication table is introduced.

### Timeout

The lifecycle provider now returns the actual written EndAt plus an independent
HSET application flag. Both HSET and XADD are still attempted under the existing
best-effort policy. XADD failure does not erase successful HSET evidence; failed
or uncertain HSET, disabled store, absent metadata/provider, and read failures
cannot turn a response fallback deadline into an event. Existing response
fallbacks remain intact. refreshTimeoutMeta is the single producer for
SetTimeout/Refresh; trusted resume refresh origin is suppressed.

Positive timeouts and SetTimeout(0) carry the actual Unix-millisecond EndAt.
Writer formats that captured value in UTC RFC3339 (existing seconds precision);
Worker never recomputes it. SetTimeout(-1) succeeds normally and emits no
timeout-only updated. Repeated successful independent requests may each notify.
Timeout has no existing spec lookup, so template and metadata are omitted/empty
rather than fetched solely for notification.

### Network

After the existing node update succeeds and the existing best-effort spec write
is attempted, the producer copies only AllowOut, DenyOut and the effective false
internet switch from this request. A spec write/read failure does not suppress
an already applied update. Metadata/resources reuse the spec already read.

Writer maps IPv4 literals to /32 and IPv4 CIDRs to network addresses, separates
domains/wildcards, and normalizes domain case/trailing dot to node semantics.
The current node rejects IPv6 policies; unsupported IP/CIDR values are never
promoted into domains. AllowInternetAccess=false projects denied_cidrs as
["0.0.0.0/0"], replacing explicit denies; true/nil project the request DenyOut.
User DNS addresses are ordinary allow entries. Node-injected DNS/private rules
are absent, and no unapplied E2B default DNS is synthesized.

All three network_egress keys are emitted. Empty categories are JSON null,
including explicit empty lists after protobuf round-trip (repeated fields lose
nil/empty distinction). Nonempty categories are arrays. Receivers requiring []
must adapt. Rules/Inject/credentials/ingress fields are never copied to the event.
L7-only requests passing this update path can emit three empty categories;
coverage of other L7-only paths is not promised. No old-policy comparison or
classification RPC is performed.

### Destruction

DestroySandbox snapshots original reason, trusted origin, and bounded P4
metadata/resources before reason defaulting and cleanup. HTTP delete supplies
request for an empty reason; lifecycle-manager supplies timeout; unknown internal
empty reasons encode as unknown. Explicit nonempty business reasons survive.
No error strings are used as kill reasons.

Sync and async completion require explicit successful node Ret evidence (or
confirmed paused deletion), followed by successful proxy/cache cleanup. Enqueue,
mock bypass, missing node with only Master cleanup, nil/absent Ret, and tolerated
paused not-found responses cannot independently produce killed. Hook failures
remain warnings. Pause package cleanup must succeed; pause metadata deletion
retains its existing warning-only policy, so killed does not promise all GC ended.

The Task retains confirmed node progress, independent of webhook enablement, so
a retry after proxy cleanup failure completes bookkeeping without repeating an
already confirmed normal node destruction. Paused tasks still retry package
cleanup. A task-local atomic publication flag prevents repeated completion
callbacks from generating new events. Event ID/time are created only at final
completion, not at request or queue time. This is neither a durable task nor
global exactly-once: a new request/process cannot borrow old in-memory evidence.
If all available responses are tolerant/not-found and no confirmed progress
survives, notification is conservatively omitted, even if cleanup returns nil.
An independent request returning an explicit success indistinguishable from
fresh node completion remains subject to the documented cross-request limitation.

Creation rollback has no normal destruction snapshot and carries its internal
origin. Pause teardown and resume cleanup remain excluded, without relying on
created delivery. Killed uses the same per-field resource mapper as paused;
missing execution identity/start/duration, fractional CPU, multi-container and
legacy metadata limitations remain unchanged. If a request already supplies its
storage backend, the existing stamp helper skips reading spec; notification does
not add that read and therefore has unknown template/metadata/resources.

Network capture adds at most 256 combined entries, 1024 bytes per entry and
16 KiB total; kill_reason is limited to 4096 bytes. These are bounded DTO budgets,
alongside existing identity/metadata budgets, not policy truncation. Any excess
drops only the notification and increments existing low-cardinality capture
observations. No partial network policy is sent.

See [P5-VALIDATION.md](P5-VALIDATION.md) for current commands, real Redis/HTTP
component evidence, failure isolation and measurement. P6 retains full deployed
Master/XFS/S3/multinode/restart/power-loss acceptance. Signature, dynamic API,
pre-persistence loss and possible post-persistence duplicates remain unchanged.
