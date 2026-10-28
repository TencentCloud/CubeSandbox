# Delivery store (P2)

`NewRedisDeliveryStore` accepts a **value copy** of `config.RedisConf` and explicit
`RedisStoreOptions`. It creates a private bounded pool, performs no startup I/O,
and starts no Writer, Worker, maintenance loop, or HTTP request. Use a complete
connection configuration for a separate Redis endpoint; do not merge its address
with another deployment's credentials. Shared `RedisWrap` behavior is unchanged.

`DeliveryTask.Event` comes from P1 `Encode`. Put preserves the exact body, event
identity/time, subscription ID, URL, initial timestamps and attempt budget.
Identity is canonical UUID + length-prefixed subscription ID. Immutable input is
serialized once for equality checks, without a content hash. Storage uses URL-safe
base64 for body bytes to bound JSON escaping overhead; Claim restores the bytes.
Each target owns its own copy. There is no query API or full attempt history.

## State and resource contract

- Put is idempotent while the identity exists, including inflight and all terminal
  states. Changed immutable input conflicts. Capacity rejects only new identities.
  A successful Put can be a duplicate and must not always increment “new tasks”.
- Claim consumes a persisted attempt **before HTTP**, including a lost reply or
  crash before sending. At most 32 candidates are examined, so a short/empty result
  does not prove the queue is empty. Expired final attempts become failed without
  granting another send. Retry at the budget limit also terminates.
- Receipt ownership changes at a successful transition/takeover, not merely at
  wall-clock expiry. Generation plus random token fences old owners, including
  after cleanup/recreation. It cannot prevent overlapping external HTTP requests.
- Complete accepts succeeded, failed or cancelled. Retry preserves the last
  bounded failure. Summaries must be sanitized by the caller; size validation
  cannot identify sensitive text. Complete/Retry use the current process UTC
  clock for FinishedAt; Claim/Prune use their supplied scheduling clock.
- Prune examines at most 32 due terminal records and validates the entire batch
  before deleting. Pending/inflight have no TTL. Deduplication ends at pruning.
- Body <= 256 KiB, URL <= 4 KiB, subscription ID <= 1 KiB, summary message <= 1 KiB,
  code <= 64 bytes; max attempts is 1–1000. Immutable encoding <= 384 KiB, complete
  stored record <= 512 KiB, batch record payload <= 16 MiB. Summary/state expansion
  has reserved space. Record-count capacity includes retained terminal records.
  Redis allocation/index overhead is additional to these bounds.
- Millisecond scheduling timestamps are positive and <= 9,999,999,999,999, keeping
  integer precision through Redis Lua/cjson. TerminalRetention is whole
  milliseconds and must cover the explicitly supplied WriterRetryWindow.

All scripts use the fixed task hash / due sorted set / terminal sorted set keys
from `rediskey.WebhookDelivery`, with the same `{delivery}` slot tag. This does
**not** provide Cluster routing: the supported connections are standalone and
Sentinel. Deploy all Masters with consistent capacity/retention/budget policy.

Key types, bounded candidate records, indexes and encoded outputs are checked
before writes. Lua atomic execution is **not rollback**: OOM or unexpected runtime
failures may still leave inconsistent data. Corruption errors require diagnosis;
do not delete tasks or silently rebuild indexes to hide them.

## P3 handoff

1. Set a bounded Writer retry window including backoff, exit time and clock-error
   allowance; pass that window here and choose at least that much retention.
   Repeat uncertain Put with the same task snapshot, never a new event ID.
2. Every operation uses the earlier of the caller deadline and OperationTimeout.
   Pool wait, fresh Sentinel lookup, dial, AUTH/SELECT, EVALSHA and its NOSCRIPT
   fallback share it. Native redigo context cancellation closes the real socket;
   no wrapper goroutine runs uncancellable legacy `RedisWrap.Do`. Network errors
   never trigger an automatic script retry. READONLY sockets are discarded.
3. Choose a lease covering HTTP deadline plus state-submission budget and clock
   allowance. A Claim reply loss grants no usable Receipt: send nothing and let
   lease recovery apply. After uncertain Complete/Retry, stop sending this attempt.
   A repeated submission may return stale even when the first one succeeded.
4. Handle invalid input, capacity, conflict, stale receipt, unavailable,
   uncertain result and corrupt store separately. Corrupt-store errors can include
   non-rollback script failures; do not infer that no write happened.
5. Drive bounded Prune explicitly. When globally disabled, do not construct or
   consume this store. For shutdown, cancel/await bounded in-flight operations
   before idempotent Close. Close only releases this pool, with no flush or task
   state change. P3 owns subscriptions, URL-change cancellation, counters and HTTP.

Redis must have appropriate persistence, disk capacity and an eviction policy
that preserves unfinished records (e.g. noeviction). Never change shared Redis
settings from this module. A successful command is not a zero-loss guarantee
across power failure, replication failover or storage failure.

## Validation

Ordinary unit and regression checks (from the repository root):

```sh
cd CubeMaster
go test -gcflags=github.com/tencentcloud/CubeSandbox/CubeMaster/...=-l \
  ./pkg/webhook ./pkg/base/rediskey -count=1 -timeout 300s
```

The port below is an example; choose an unused loopback publication. Do not run
existing `pkg/base/wrapredis` application tests against a shared default config;
they require their own isolated complete configuration. Deployment controls and
portable environment setup are in the [test runbook](../../../tests/e2e/webhook/README.md).

Real tests require an explicitly dedicated local Redis instance, authentication,
and DB 9. They use random key namespaces and delete only those keys. The tests
must not be pointed at a shared business Redis. Missing `WEBHOOK_REDIS_ADDR`
skips real tests and is **not** P2 acceptance. To include the service restart,
provide the name of a disposable container beginning with `webhook-1903-p2-`:

```sh
WEBHOOK_REDIS_ADDR=127.0.0.1:16390 \
WEBHOOK_REDIS_PASSWORD='<isolated-test-password>' \
WEBHOOK_REDIS_RESTART_CONTAINER='<this-test-container>' \
go test -race -gcflags=github.com/tencentcloud/CubeSandbox/CubeMaster/...=-l \
  ./pkg/webhook -run '^TestRedisProcessAndServiceRecovery$' -count=1 -timeout 120s
```

The behavior tests exercise real state transitions, immutable snapshots,
idempotency/conflicts, concurrent capacity/claims, takeover competition, old
Receipts, ABA, last-claim crash, last Retry, retention and pruning. Fault tests
consume the real Redis reply before dropping it for Put/Claim/Complete/Retry,
and test an uncommitted Complete separately. Connection tests cover pool
exhaustion, AUTH/command cancellation, slow peers, authentication, nonzero DB,
Sentinel independent password/address fallback/re-resolution, READONLY disposal,
and a shared budget through simulated NOSCRIPT fallback. Sentinel responses are
protocol simulations; they do not constitute real Sentinel failover acceptance.

Recovery starts separate test processes to seed and inspect pending, future retry
and inflight records, optionally normally restarts the specified Redis container
on its existing volume, then starts another process to check immutable data,
preserved schedules/budgets and expiry takeover. This is storage-component
recovery, not a complete Master/platform test.

### Executed P2 verification — 2026-10-20

Starting HEAD: `9e5a32453948ffc09a0c29251412ecceea07bc1b` on
`feat/E2B-compatible-sandbox-lifecycle-webhooks`. Go 1.25.7, linux/amd64.
No commit, push, PR, configuration loading or production delivery was performed.

After read-only Docker/container/image/mount/port inspection, testing used a new
`webhook-1903-p2-20261020` container, dedicated
`webhook-1903-p2-20261020-data` volume at `/data`, and localhost port 16390.
The existing Redis image ran Redis **7.4.9** standalone with authentication, DB 9,
AOF enabled, `appendfsync=always`, 128 MiB maxmemory and `noeviction`. RDB `save`
was `3600 1 300 100 60 10000`. Final persistence inspection reported
`aof_last_write_status=ok`, `aof_pending_bio_fsync=0`, `aof_delayed_fsync=0`.

Both commands below ran with all three real-test environment variables above
set, including the explicit disposable-container restart opt-in:

| Command | Result (exit 0) |
| --- | --- |
| `go test -gcflags=github.com/tencentcloud/CubeSandbox/CubeMaster/...=-l \
  ./pkg/webhook ./pkg/base/rediskey -count=1 -timeout 300s -count=1 -timeout 120s` | pass; 2.039s / 3.227s / 0.002s |
| `go test -race ./pkg/webhook ./pkg/base/wrapredis ./pkg/base/rediskey -count=1 -timeout 120s` | pass; 8.617s / 4.276s / 1.014s |

Formatting and `git diff --check` also passed. Earlier test iterations caught and
fixed Redis `ERR` reply classification, a test-only shared-channel race, and the
need to wait for AOF loading after Docker restart before starting the recovery
child. The final runs above include those fixes; failed iterations are not counted
as acceptance.

The dedicated container and volume were removed after verification; existing
Docker containers, business Redis/MySQL volumes and other assets were retained.
Existing uncommitted root files, `.agents`, `.scratch` and `spec` were not edited.

Not verified: real Sentinel election/replication failover, hard power-loss
persistence, Cluster routing, HTTP delivery or full Master/platform lifecycle
recovery. Normal same-volume Redis restart and separate client-process recovery
were executed; they do not establish those other properties.
