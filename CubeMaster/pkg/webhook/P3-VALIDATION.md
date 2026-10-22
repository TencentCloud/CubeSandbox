# P3 validation — 2026-10-21

This is a record of this run, not a frozen baseline or benchmark gate. P3 only:
no lifecycle producers, signatures, dynamic subscription API, proto, Cubelet,
L7 changes, internal Stream changes, commit, push or PR.

Starting and ending branch: `feat/E2B-compatible-sandbox-lifecycle-webhooks`.
HEAD remains `1ee94e85054f89fc90f8b518bc68c8f70f19a381`. Existing untracked
`.agents/`, `.scratch/`, `spec/` and `sue -1903 P1.` were preserved.

## Changes

- `pkg/base/config/webhook.go`: local strict YAML boundary, defaults/budget/URL
  validation, secret-safe formatting and JSON logging, original YAML preserved
  for watcher comparison. Invalid webhook fields do not reach global Fatal.
- `pkg/base/config/config.go`: startup/listener log redaction and restart notice.
  Other modules retain their validation/Fatal policy.
- `pkg/webhook/subscription.go`, `sender.go`, `runtime.go`: immutable Provider,
  safe exact-byte HTTP Sender, bounded single Writer and fixed Workers, finite
  shutdown, restoration/cancellation and low-cardinality metrics.
- `capture.go`: prebound atomic capture observations; existing publication,
  mapping and channel ownership remain intact.
- `store_redis.go` / scripts: bounded due-index sample and Claim-local recovery /
  exhaustion counts. No new Redis keys, history indexes or storage backend.
- `cmd/cubemaster/app/main.go`: owns Runtime and exposes the default-Nop
  Publisher injection point; waits for shutdown before returning.
- Default-off example in `configs/single-node/cubemaster.yaml`; operating details
  and P4/P5 handoff in [DELIVERY.md](DELIVERY.md).

## Environment and executed commands

Go 1.25.7 linux/amd64, AMD EPYC, 4 available CPUs. The shell initially had no Go.
The existing `cube-sandbox-cubemaster-builder:ubuntu2004` image provided Go.
Its glibc could not run the host Docker CLI for the Redis restart test; the same
Go distribution was extracted to `/tmp/webhook-p3-toolchain/go`, and the final
runs used the host Docker CLI. No system toolchain or project dependencies changed.

Before creating Redis, read-only inspection covered containers, images, listening
ports, volumes and existing mounts. A new dedicated container
`webhook-1903-p2-p3-20261021` used port `127.0.0.1:16390`, volume
`webhook-1903-p3-20261021-data` at `/data`, Redis 7.4.9 standalone, authentication,
DB 9, AOF, `appendfsync=always`, 128 MiB maxmemory and `noeviction`. The P2 prefix
in the container name satisfies the existing restart test's disposable-container
opt-in. Tests used random namespaces and removed only their own keys; no FLUSHDB
or FLUSHALL. Main-runtime construction probes made no matching task writes.

Final commands from `CubeMaster` (test password belongs only to this disposable
instance):

```sh
export GOMODCACHE=/home/ubuntu/.cache/cube-sandbox-builder/go/pkg/mod
export GOCACHE=/home/ubuntu/.cache/go-build
export WEBHOOK_REDIS_ADDR=127.0.0.1:16390
export WEBHOOK_REDIS_PASSWORD=p3-isolated-test
export WEBHOOK_REDIS_RESTART_CONTAINER=webhook-1903-p2-p3-20261021
GO=/tmp/webhook-p3-toolchain/go/bin/go

"$GO" test ./pkg/webhook ./pkg/base/config ./pkg/base/hotswap \
  ./cmd/cubemaster/app ./pkg/base/wrapredis ./pkg/base/rediskey \
  -count=1 -timeout 180s
"$GO" test -race ./pkg/webhook ./pkg/base/config ./pkg/base/hotswap \
  ./cmd/cubemaster/app ./pkg/base/wrapredis ./pkg/base/rediskey \
  -count=1 -timeout 180s
"$GO" test ./pkg/webhook -run TestPublishLatencyDistribution \
  -count=1 -v -timeout 120s
"$GO" test ./pkg/webhook -run '^$' -bench 'Publish|Capture' -benchmem
```

All four final commands exited 0. Package durations:

| Package | Normal | Race |
| --- | ---: | ---: |
| webhook | 27.158s | 35.887s |
| config | 0.117s | 4.210s |
| app | 0.017s | 1.044s |
| wrapredis | 3.228s | 4.255s |
| rediskey | 0.001s | 1.008s |

`hotswap` has no standalone test files; the new webhook watcher integration test
exercises its real file notification path. The pre-existing `config.TestInit`
skips because `CubeMaster/test/conf.yaml` is absent. The new
`TestWebhookActualInitAndListener` uses real Init/listener subprocesses with its
own fixtures, including existing Fatal behavior, and passed; it does not borrow
that skip as evidence. No real-Redis tests skipped in the final runs.

Formatting and `git diff --check` passed. Earlier iterations caught an IPv6
loopback policy error, a Lua counter accidentally applied to Retry, test-server
body-draining behavior, a test-only TLS Transport mutation race, and YAML
round-trip handling of optional nil fields. These were fixed before the final
runs above. Earlier failed runs are not acceptance evidence.

## Requirement coverage and evidence boundaries

| P3 requirement | Current evidence |
| --- | --- |
| Defaults / invalid local types, unknown keys/backends, budgets, duplicate IDs, URL/duration | `TestWebhookConfigIsolation`; strict YAML rejects coercion and unsupported secret fields |
| Actual Init/listener isolation and redaction; unrelated errors still fail | `TestWebhookActualInitAndListener` child processes, injected URL/password tokens absent from output |
| Disabled zero resources / network | `TestDisabledDoesNotConstructResources` factory counts; `TestRuntimeNetworkFaultAndDisabledProbe` TCP probe; no store/sender/buffer on disabled Runtime |
| Snapshot / watcher / restart | `TestProviderSnapshot`, `TestWatcherDoesNotReplaceSubscriptionSnapshot`, child disabled/recover phases |
| YAML configuration through real Redis to multiple HTTP receivers | `TestRuntimeRealPipelineAndCompetition`: two Runtime consumers, type filtering, exact matching bodies, saved success, bounded terminal prune |
| Target removal, disabled subscription, path/query change, removed event | `TestRuntimeRevokedTargets`: cancelled old task, no request to either URL, immutable input retained |
| Exact bytes, three headers, no signature, status handling | `TestSenderExactHeadersAndStatuses`, `TestRuntimeSignatureRequiredFiniteFailure` (401 remains incompatible; two persisted attempts then failed) |
| TLS / SSRF / proxy / rebinding / redirects | `TestSenderAddressPolicy`, `TestSenderTLSProxyRedirectAndLimits`: trusted/untrusted/mismatched TLS, explicit private/local exceptions, mapped IPv4, mixed addresses rejected before dial, checked-IP dialing, rebinding simulation, zero proxy/redirect hits |
| Timeouts, cancellation, huge/slow headers/body, connection failure | Sender limit tests; reused-connection test asserts two explicit Send calls produce only two server requests |
| Put response loss / snapshot identity / partial fan-out | `TestWriterRealLostPutResponseSameSnapshot`: real Redis reply dropped by RESP proxy, identical initial timestamps/body/target reused, two records after three Put calls |
| Claim/Complete/Retry response loss | `TestWorkerRealUncertainTransitions`: separately committed and uncommitted cases, no guessed receipt or in-memory HTTP resend; real server state checked |
| Persisted budget, expired lease, last-claim crash | Runtime final-claim-crash test and P2 behavior/takeover/ABA regressions; never an extra HTTP after exhaustion |
| Slow/disconnected Redis, capacity, full ingress, slow HTTP | Runtime network-fault, bounded-shutdown and measurement scenarios; P2 pool/auth/command cancellation tests; caller never waits for I/O or queue room |
| Concurrent shutdown, finite flush and goroutine convergence | eight concurrent shutdown callers, 100ms caller deadline, full ingress, blocked context-aware calls, channel remains open, goroutines return near baseline |
| Corruption and maintenance observations | corruption stops admission; sample reads ZCARD/one due score only; Claim counter test separates normal Retry, lease recovery and exhausted termination; P2 Prune protects pending/inflight |
| Process recovery | `TestRuntimeRealSubprocessRecovery`: seed child exits without runtime shutdown, pending/future Retry/inflight persisted; disabled child leaves 3 due entries and sends nothing; recovery child sends original body and preserves attempts 2/2/1 |
| Redis service recovery | existing P2 process/service recovery rerun with explicit same-volume normal Docker restart, in both normal and race suites |
| App ownership | `TestAppWebhookOwnership` exercises production App setup, default Publisher and parent cancellation/Shutdown; full server/DB/platform boot was not performed |

DNS answer changes and Sentinel protocol paths use injected/protocol test peers;
these are not real DNS infrastructure failover or real Sentinel election tests.
The network-fault peer intentionally delays Redis protocol replies. The overhead
`slow-store` case is an injected 200ms delay before the real store, explicitly not
a benchmark of a slow Redis deployment. All persisted recovery/state assertions
use the isolated real Redis rather than a second production backend.

No full Master/platform E2E, real XFS/S3 lifecycle, power-loss durability, real
Sentinel failover or Cluster routing was run. Real lifecycle mapping/success
boundaries are P4/P5, platform acceptance is P6; P3 component recovery was run now.
No claim of exactly-once, zero loss, zero overhead or lifecycle coverage is made.

## Caller latency and resource measurements

Each row uses 1000 samples, concurrency 1 or 4, created input with a 256-byte
sandbox ID, prepared UUIDs/timestamps/sample arrays outside the timed call.
The caller columns measure TryPublish or Capture+TryPublish only. Both Nop and
enabled Capture rows perform Capture, so the disabled baseline does not silently
omit it. Normal/fault runtimes use ingress 1024, two workers, real-store pool 4/2,
HTTP timeout 150ms and lease 3s; these are test settings, not default capacity
recommendations. Full-ingress uses a prefilled capacity-one publisher.

Throughput is caller attempts per second, **not delivered webhooks per second**.
Nop non-acceptance means disabled, not lost business events. All active scenarios
accept this burst into memory; that does not assert all 1000 tasks persisted or
were delivered. Full-ingress rejects the burst. Tests separately assert capacity
and persistence behavior.

Resource sampling continues 600ms after caller timing, then shutdown uses a 300ms
caller budget. Expected shutdown cancellation may log unconfirmed Put/Claim.
`allocs/op` below is the **whole-process** Mallocs delta divided by samples,
including background work during that interval, not isolated caller allocations.
Heap and goroutine deltas are observed peaks above the post-construction,
post-input-preparation baseline, sampled at 1ms; they exclude prepared inputs and
are observations, not absolute worst-case RSS/goroutine bounds. See DELIVERY.md
for structural bounds. Small single-run percentiles contain scheduler/GC noise
and do not establish performance guarantees.

| Scenario / call / concurrency | p50 / p95 / p99 (ns) | Attempts/s | Accepted / nonaccepted | Process allocs/op | Heap peak delta (bytes) | Goroutine peak delta |
| --- | --- | ---: | --- | ---: | ---: | ---: |
| nop/publish/c1 | 30 / 40 / 41 | 8755954 | 0 / 1000 | 0.017 | 1904 | 0 |
| nop/publish/c4 | 30 / 50 / 71 | 11465655 | 0 / 1000 | 0.023 | 2712 | 0 |
| nop/capture-publish/c1 | 120 / 141 / 2975 | 4138353 | 0 / 1000 | 1.012 | 160720 | 0 |
| nop/capture-publish/c4 | 541 / 762 / 2435 | 4001809 | 0 / 1000 | 1.025 | 163256 | 0 |
| enabled/publish/c1 | 110 / 120 / 130 | 4870161 | 1000 / 0 | 47.616 | 665112 | 5 |
| enabled/publish/c4 | 231 / 1553 / 7304 | 5860085 | 1000 / 0 | 46.325 | 957128 | 7 |
| enabled/capture-publish/c1 | 201 / 220 / 802 | 3354253 | 1000 / 0 | 45.657 | 516696 | 10 |
| enabled/capture-publish/c4 | 321 / 1513 / 2795 | 3972494 | 1000 / 0 | 48.218 | 1014960 | 7 |
| slow-store/publish/c1 | 110 / 111 / 121 | 5533269 | 1000 / 0 | 0.310 | 52264 | 3 |
| slow-store/publish/c4 | 191 / 1092 / 4929 | 4655493 | 1000 / 0 | 0.282 | 44552 | 2 |
| slow-store/capture-publish/c1 | 200 / 221 / 832 | 3331412 | 1000 / 0 | 1.314 | 213328 | 2 |
| slow-store/capture-publish/c4 | 361 / 1423 / 3396 | 4610335 | 1000 / 0 | 1.279 | 202536 | 2 |
| disconnected-store/publish/c1 | 110 / 120 / 320 | 4506047 | 1000 / 0 | 0.095 | 10936 | 1 |
| disconnected-store/publish/c4 | 131 / 390 / 6913 | 4642051 | 1000 / 0 | 0.090 | 9896 | 1 |
| disconnected-store/capture-publish/c1 | 201 / 241 / 2976 | 2872210 | 1000 / 0 | 1.090 | 171032 | 1 |
| disconnected-store/capture-publish/c4 | 270 / 641 / 3487 | 4268634 | 1000 / 0 | 1.090 | 169896 | 1 |
| slow-http/publish/c1 | 110 / 151 / 180 | 5309857 | 1000 / 0 | 37.179 | 1287176 | 10 |
| slow-http/publish/c4 | 200 / 301 / 1944 | 5578738 | 1000 / 0 | 38.336 | 841264 | 10 |
| slow-http/capture-publish/c1 | 290 / 461 / 3987 | 2174301 | 1000 / 0 | 39.402 | 534512 | 10 |
| slow-http/capture-publish/c4 | 501 / 1523 / 3216 | 4499741 | 1000 / 0 | 37.063 | 1398944 | 10 |
| full-ingress/publish/c1 | 80 / 91 / 101 | 6553896 | 0 / 1000 | 0.012 | 720 | 0 |
| full-ingress/publish/c4 | 140 / 872 / 1382 | 6185899 | 0 / 1000 | 0.018 | 1176 | 0 |
| full-ingress/capture-publish/c1 | 170 / 261 / 972 | 3676079 | 0 / 1000 | 1.012 | 160720 | 0 |
| full-ingress/capture-publish/c4 | 622 / 862 / 2014 | 3736767 | 0 / 1000 | 1.018 | 161176 | 0 |
| capacity/publish/c1 | 111 / 121 / 130 | 5337091 | 1000 / 0 | 0.257 | 28464 | 0 |
| capacity/publish/c4 | 200 / 431 / 2775 | 3975511 | 1000 / 0 | 0.290 | 0 | 0 |
| capacity/capture-publish/c1 | 270 / 391 / 1032 | 2480472 | 1000 / 0 | 1.237 | 189304 | 0 |
| capacity/capture-publish/c4 | 441 / 711 / 2034 | 3628631 | 1000 / 0 | 1.254 | 189792 | 0 |

The isolated caller microbenchmarks (no asynchronous Runtime work; full queue
for the enabled publisher baseline) report:

```text
BenchmarkPublishP3/PublishNop-4         	553382164	         2.209 ns/op	       0 B/op	       0 allocs/op
BenchmarkPublishP3/PublishFull-4        	24874542	        47.88 ns/op	       0 B/op	       0 allocs/op
BenchmarkPublishP3/CapturePublishNop-4  	10307822	       118.3 ns/op	     160 B/op	       1 allocs/op
BenchmarkPublishP3/CapturePublishFull-4 	 7005448	       171.1 ns/op	     160 B/op	       1 allocs/op
```

## Handoff and cleanup

P4/P5 use the App Publisher injection point, bounded Capture and nonblocking
TryPublish, then extend the existing pure background mapper with real field
sources and success boundaries. Do not infer lifecycle correctness from the
synthetic created-event receiver test.

The dedicated Redis container/volume were re-inspected before cleanup, then
removed after final validation. Existing Docker assets and shared Redis settings
were untouched. Logs remain in `/tmp/webhook-p3-{tests,race,measure}.log` for this
workspace session; this document retains the command/results and measurement
values. No Git commit/push/PR was performed.
