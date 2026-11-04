# In-memory lifecycle delivery maintenance

Configuration, event boundaries and payload compatibility live in the
[English user guide](../../../docs/guide/webhooks.md) /
[中文指南](../../../docs/zh/guide/webhooks.md). Keep user-facing tables there.
Current commands are in the [test runbook](../../../tests/e2e/webhook/README.md).

## Ownership and flow

App constructs one Runtime from startup configuration and installs its Publisher
in sandbox, task and templatecenter. Disabled, invalid or no-active-subscription
configurations expose a true NopPublisher. Producers check Enabled before
preparation and capture values already available at existing success boundaries.
No notification-specific business query is added.

Capture validates and copies bounded data, fixes event ID/time, and TryPublish
attempts nonblocking ingress admission. Invalid/oversize data rejects the whole
notification. One writer filters subscriptions, maps resources/network, encodes
once, then independently puts target tasks. Partial fanout is allowed; full store
capacity drops that target immediately. There is no per-event application
goroutine, second pipeline or persistence retry window.

## Store and worker lifetime

DeliveryStore retains Put/Claim/Complete/Retry. MemoryDeliveryStore uses a mutex
and bounded map keyed by event ID plus subscription ID; pending and inflight
share capacity. Tasks own immutable body/target snapshots. Identical Put is
idempotent while the record exists; conflicting snapshots are rejected.

Claim scans at most configured capacity, selects the oldest due pending record,
increments attempts and assigns a process-local receipt. Receipt object identity
rejects stale completion/retry. Inflight records cannot be reclaimed. There is no
lease, cross-Master ownership or crash recovery. Change the scheduling structure
only if measured scan cost warrants it.

Fixed workers claim one task at a time, recheck ID/URL/type against the startup
subscription snapshot, generate a fresh delivery ID and invoke Sender once.
Every claim must resolve through Complete or Retry, including identity failure,
cancellation and address rejection. Retry retains the original slot/body/target/
budget and updates due time. Backoff is exponential plus up to 25% jitter, capped
at 30s. Completion/cancellation/exhaustion deletes the record and releases
capacity; there is no terminal history or Prune. Empty polling is cancellable.

Store body is limited to 256 KiB, URL to 4096 bytes and subscription ID to 1024
bytes. Counts do not bound RSS: targets own body copies and workers/transports
add memory. Masters have independent notification state. There are no Redis
connections, Lua, maintenance/sampling loop, persistence or takeover. Business
Redis and retained business Redis→Runtime→HTTP tests remain separate from
notification storage.

## Shutdown and security

Runtime work has its own cancellable context so parent cancellation can initiate
the same drain as explicit shutdown. Admission stops first; writer flush and
worker drain share one two-second total budget or an earlier caller deadline.
Expiry cancels HTTP/waits, joins owned workers, closes idle transport and discards
remaining memory. Repeated/concurrent shutdown is coordinated. Events() is never
closed; Shutdown does not become another channel consumer. App reaches its
original cancellation path; this is not a whole-process exit deadline.

Keep Sender's TLS/hostname verification, all-DNS-answer checks, checked-IP
dialing, private/loopback CIDR policy, special-use rejection, HTTP opt-in,
disabled proxies/redirects, GetBody=nil POST replay protection, bounded responses
and cancellation. Sender does no retry or body regeneration. Do not add business
state/evidence or weaken safety to compensate for notification loss.

Metrics are local outcomes, claim-age histogram, writer-observed ingress depth
and active workers, not persistent history or proof of receiver processing. See
runtime.go for labels and the user guide for operations. Redis STORE/validation
reports were removed from current source documentation; Git history preserves
their dated results. They do not establish acceptance of this implementation.
