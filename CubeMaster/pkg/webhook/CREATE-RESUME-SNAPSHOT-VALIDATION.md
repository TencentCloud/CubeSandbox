# Create, resume and snapshot webhook validation — 2026-10-22

> Historical execution record: machine paths and resource names below identify
> that run, not required installation locations. For current portable setup and
> commands, use the [test runbook](../../../tests/e2e/webhook/README.md).


Starting branch: `feat/E2B-compatible-sandbox-lifecycle-webhooks`; HEAD:
`b5e9fe0e899b165bd5fa31bca5d2f67ece496179`. Tracked files were clean at entry;
P3 was already in the current commit. Existing `.agents/`, `.scratch/`, `spec/`
and `sue -1903 P1.` were preserved. No add/commit/amend/push/PR was performed.
Historical P1/P2 hashes and P3 validation were navigation aids, not this run's
results. This phase adds no updated/killed producers and does not start P5/P6.

## Supplemental acceptance run — 2026-10-22

The earlier completion statement overstated the coverage of the acceptance
matrix. This follow-up adds tests; it does not change lifecycle production code
or begin P5/P6. The working tree already contained the P4 implementation above.

- `FaultPublishers` now waits for a Redis protocol request or an HTTP handler
  entry before invoking the real business callbacks. Backend responses stay
  blocked until the callbacks return. HTTP shutdown tests first observe
  `PublishStopped`, assert Shutdown is still in flight, execute the business
  callbacks, and assert Shutdown has not completed. A 5-second deadlock guard
  is below the 10/30-second backend deadlines; it is not a latency benchmark.
  A synthetic primer establishes the blocked dependency only; all four event
  success paths still run through their real business functions.
- Checkpoint cancellation cases cover claim returning cancellation (zero
  events), cancellation immediately after successful claim or during Commit
  (one event after READY), and local Commit failure after cancellation (zero).
  The external claim/Commit/DB dependencies remain controlled test seams.
- `TestP4PauseDuplicateAndUploadResult` uses real Begin/Complete with a temporary
  SQLite database. The second pause is rejected by the existing binding, with
  no second event. This is rejection, not an idempotent success response.
- `TestP4PauseTimeoutHealingDoesNotPublish` exercises real Complete and the
  timeout-healing function against that temporary store: READY restoration
  does not emit a delayed paused event.
- Existing repeated-requestID checkpoint tests now assert zero publications;
  READY and lost-claim cases retain their no-repeat assertions.
- `TestP4CheckpointUploadResult` and the pause upload test execute the real
  `remotestatus.ReconcileOnce` twice for both ready and failed upload outcomes.
  Local READY and the already-published event are preserved; no second event
  is emitted. Checkpoint's controlled READY storage result is seeded into
  SQLite; pause uses its actual persisted record. Cubelet status is controlled.
  The current implementation polls upload status: there is no separate upload
  callback entry to test, and no new callback was introduced.

The initial supplemental run exposed a missing controlled Cubelet address in
the upload fixture; it was fixed before the passing runs. It was not a product
failure or a passing result. Full commands/results and refreshed measurements
for this follow-up are recorded below separately from the earlier run.

## Changes and boundaries

- Bounded immutable lifecycle fields, metadata provenance and background
  resource mapping in webhook; existing Writer/Store/Worker/Sender reused.
- Atomic Publisher injection from App into sandbox and templatecenter.
- Create notification in the winning response-once operation after final
  post-processing; cancellation cannot be overwritten by late success. Response
  field writes and cancellation snapshots are synchronized. The existing metrics
  goroutine waits for worker completion before reading worker timings.
- Resume notification after purgeErr, pause after Complete, checkpoint after
  final READY. Existing hooks and warn-only error policy remain in place.
- Trusted template classification reuses the HTTP resolution context. Original
  checkpoint template identity is captured before buildSnapshotRequests.
- Pre-merge user metadata is stored only as a private extension of the existing
  sandbox spec JSON. API JSON cannot supply that internal request field.
- Create rollback, pause teardown/cleanup and resume timeout contexts carry
  origins. Queue retry preserves them without a new task format or proto.
- Full regression exposed an existing FileOperator initialization race: data
  was set after starting the reload goroutine. Initialization now precedes
  listener startup. The existing P3 watcher test also waits past a possible
  truncate-before-write notification. No watcher policy or safety check removed.

See [DELIVERY.md](DELIVERY.md) for exact metadata, resource and compatibility
rules. No extra business database/node queries or notification I/O were added.

## Evidence

| Area | Actual evidence |
| --- | --- |
| Final create | TestP4CreateFinalBoundary: ordinary/snapshot/forged annotation, proxy failure, warn-only spec failure, repeated finalizer; deterministic cancellation during proxy write in TestP4CreateCancellationDuringPostProcessing |
| Real App injection | TestP4AppPublisherRealCreate calls App.startWebhooks and real sandbox.CreateSandbox; external Cubelet/proxy dependencies are controlled; body reaches isolated Redis/HTTP |
| Resume | TestP4ResumeFinalBoundary: node, identity, proxy and final purge errors; cleanup/binding warn-only failures still notify |
| Pause | TestP4PauseLocalBoundary: XFS/S3 local success including empty remote UUID, node failure, timeout, nil response/ret, final Complete failure |
| Detached operations / timeout | TestP4UpdateDetachedCancellationAndResumeTimeout invokes real Update with parent cancellation after lock acquisition and nil/0/positive/-1 timeout, then actual pause/resume functions; exactly one corresponding event and correct timeout provider origin |
| Checkpoint | TestP4CheckpointFinalBoundary runs executeSnapshotCreateJob and runSnapshotCreateJob; commit/replica/snapshot/READY failures, READY then read failure, lost claim, READY idempotent return, original template, S3 empty/in-progress UUID |
| Origins | TestP4FailoverEnqueuesOrigin observes real failover Task; TestP4OriginAcrossDestroyQueueRetry uses real AddAsyncTask, queue, handleTask and Destroy handler with one failing Cubelet attempt, then success; both receive create_failover through WithoutCancel |
| Field ownership | TestLifecycleMetadataAndCapture, TestLifecycleResources, TestWebhookProvenanceStoredOnly, TestP4TrustedResolveAndPreMergeMetadata: bounds, input mutation, user token keys, exact internal filtering, nil/empty/unknown, API spoofing, persisted JSON round trip, units and partial resources |
| Real business pipeline | TestP4BusinessRedisHTTP and TestP4CheckpointRedisHTTP use actual final business functions, Capture, P3 Writer, real Redis Store, Worker and HTTP; each receiver returns 503 once then 204, checks identical body/event ID and distinct delivery ID |
| Business isolation | TestP4BusinessFailureIsolation / TestP4CheckpointFailureIsolation: disabled, full, stopped, disconnected Redis, slow Redis-protocol peer, slow HTTP and shutdown; original success results preserved |
| Regression | Existing P1/P2/P3 tests and affected business package tests; normal and race commands below |

Tests replace external dependency boundaries with existing package seams or
repository-standard gomonkey patches. This is component evidence, not live VM,
XFS/S3 deployment, cross-node restore or power-loss evidence. The slow Redis peer
is a protocol blackhole; it is not a measured production Redis slowdown. The
latency slow-store case injects 200ms before real Store.Put.

## Environment and commands

Go 1.25.7 linux/amd64 from `/tmp/webhook-p3-toolchain/go`; existing module/build
caches reused. Before Redis creation, containers, volumes and listeners were
inspected read-only. New container `webhook-1903-p4-20261022`, new volume
`webhook-1903-p4-20261022-data`, loopback port 16391, Redis 7.4.9 standalone,
AOF, appendfsync=always, 128 MiB, noeviction. No shared service changes and no
FLUSHDB/FLUSHALL. P2/P3 tests use random key prefixes in DB 9; P4 business tests
use the production webhook namespace on isolated DBs 10/11, fault probes DB 12,
App test DB 14. Run normal and race suites sequentially against this instance.

From CubeMaster:

```sh
export GO=/tmp/webhook-p3-toolchain/go/bin/go
export GOMODCACHE=/home/ubuntu/.cache/cube-sandbox-builder/go/pkg/mod
export GOCACHE=/home/ubuntu/.cache/go-build
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
export WEBHOOK_REDIS_ADDR=127.0.0.1:16391 WEBHOOK_REDIS_PASSWORD=p4-isolated-test
export WEBHOOK_P4_REDIS_ADDR=127.0.0.1:16391 WEBHOOK_P4_REDIS_PASSWORD=p4-isolated-test

"$GO" test -gcflags='github.com/tencentcloud/CubeSandbox/CubeMaster/...=-l' \
  ./pkg/webhook ./pkg/service/sandbox ./pkg/service/httpservice/cube \
  ./pkg/templatecenter ./pkg/task ./pkg/sandboxspec ./cmd/cubemaster/app \
  ./pkg/base/hotswap -count=1 -timeout 180s
"$GO" test -race -gcflags='github.com/tencentcloud/CubeSandbox/CubeMaster/...=-l' \
  ./pkg/webhook ./pkg/service/sandbox ./pkg/service/httpservice/cube \
  ./pkg/templatecenter ./pkg/task ./pkg/sandboxspec ./cmd/cubemaster/app \
  ./pkg/base/hotswap -count=1 -timeout 180s
"$GO" test ./pkg/webhook -run '^TestP4CaptureDistribution$' -count=1 -v
```

Closing inlining is required for some existing gomonkey tests: the initial race
run reached a real uninitialized Redis through an inlined SandboxInfo call.
Initial failed runs also exposed the watcher race, saturated resource quantity
handling and test-fixture mistakes; they are not passing evidence.

Both full commands exited 0. Normal / race package durations (seconds):

| Package | Normal | Race |
| --- | ---: | ---: |
| webhook | 28.944 | 38.395 |
| sandbox | 7.994 | 12.874 |
| httpservice/cube | 0.080 | 1.359 |
| templatecenter | 54.247 | 57.033 |
| task | 0.022 | 1.053 |
| sandboxspec | 0.105 | 1.241 |
| app | 0.535 | 1.597 |

hotswap has no standalone test files; its real listener is exercised by the
webhook watcher test. A subsequent templatecenter `-json` run exited 0 with zero
skips (53.563s), including Docker MySQL/PostgreSQL artifact/alias/lock tests.
Additional focused race runs cover post-capture mutation, oversize business
isolation and the self-contained P4 sandbox matrix. The latter explicitly
provides ID lookup candidates rather than relying on prior tests' cache state.
Logs are `/tmp/webhook-p4-final-{normal,race,boundary2,measure}.log`,
`/tmp/webhook-p4-ownership.log` and `/tmp/webhook-p4-template-audit.jsonl`.
Formatting and `git diff --check` passed.

## Added capture-path measurements

1000 sequential samples per row, two metadata entries totaling 8209 bytes,
CPU `1`, memory `1Gi`. Timing includes PrepareLifecycle, UUID/time allocation,
Capture and TryPublish; created additionally includes pre-template UserMetadata
capture. Resume measures pause restoration's preparation (snapshot-start also
has the HTTP pre-template capture cost). Disabled still performs preparation,
then skips success Capture/UUID in the Nop holder. These measurements exclude
business RPC/database work and background mapping. They are not end-to-end
sandbox latency or a claim of zero overhead. No performance gate was added.

Enabled/slow-store/slow-http use real isolated Redis with random key namespaces.
Slow-store injects 200ms before Put; slow-http delays the receiver 200ms. Full
fills a one-slot buffer (the initial admission may succeed); stopped rejects all.
Disconnected uses a refused local port. Shutdown is bounded separately. Values
are nanoseconds, single-run observations with allocator/GC/scheduler noise; the
closed baseline still incurs the newly added provenance/preparation copying.


| Event / scenario | p50 ns | p95 ns | p99 ns |
| --- | ---: | ---: | ---: |
| created/disabled | 2615 | 15109 | 92205 |
| created/enabled | 8376 | 24146 | 128885 |
| created/full | 7825 | 31039 | 119827 |
| created/stopped | 7524 | 21531 | 93918 |
| created/disconnected | 10780 | 23063 | 79601 |
| created/slow-store | 8226 | 26149 | 80182 |
| created/slow-http | 8586 | 24777 | 74842 |
| resumed/disabled | 2024 | 10310 | 57580 |
| resumed/enabled | 6092 | 17964 | 72057 |
| resumed/full | 5591 | 18435 | 66827 |
| resumed/stopped | 3667 | 10320 | 48181 |
| resumed/disconnected | 5571 | 13836 | 48642 |
| resumed/slow-store | 5029 | 18154 | 55115 |
| resumed/slow-http | 6012 | 14126 | 30929 |
| paused/disabled | 1993 | 9067 | 20069 |
| paused/enabled | 6662 | 17754 | 71646 |
| paused/full | 4959 | 18755 | 81384 |
| paused/stopped | 3346 | 9398 | 52741 |
| paused/disconnected | 6142 | 16391 | 62900 |
| paused/slow-store | 5059 | 19828 | 53422 |
| paused/slow-http | 5711 | 13085 | 27051 |
| checkpointed/disabled | 2365 | 7564 | 35617 |
| checkpointed/enabled | 6382 | 18465 | 78449 |
| checkpointed/full | 5490 | 19286 | 58401 |
| checkpointed/stopped | 2966 | 9488 | 46668 |
| checkpointed/disconnected | 6072 | 15619 | 52430 |
| checkpointed/slow-store | 4909 | 17723 | 59543 |
| checkpointed/slow-http | 5380 | 15138 | 33544 |

## Skips and deferred acceptance

- Redis service restart substep was not executed without WEBHOOK_REDIS_RESTART_CONTAINER;
  this phase did not reuse P3's historical restart result as current evidence.
  Existing Redis task recovery/failure tests run on the dedicated instance.
- No live full Master deployment, XFS/S3 VM lifecycle, remote upload durability,
  cross-node recovery, power loss, Sentinel election or Cluster routing test.
  These remain P6; controlled local dependency tests do not claim those results.
- No reliable execution start/identity exists in the persisted spec. There is no
  new time-source input to fabricate or validate; resources remain independent.
- P5 must add actual updated/killed publication, consume the origins tested here,
  and prove internal suppression together with normal-operation notification.
  Zero updated/killed events in P4 do not complete that acceptance.
- Exactly-once delivery, cross-request reconciliation, strict receiver migration
  and elimination of the success-to-persistence loss window are not promised.

## Test resource cleanup

After all tests finished, the dedicated container and its mount were re-inspected,
then `webhook-1903-p4-20261022` and `webhook-1903-p4-20261022-data` were removed.
The abandoned `/tmp/webhook-p4-gocache` from the initial restricted-cache attempt
was removed after confirming that attempt had exited; the existing shared Go
caches and all pre-existing Docker assets were preserved. Logs remain in /tmp.

## Supplemental run commands and results

New isolated Redis 7.4.9 container `webhook-1903-p4-supplement-20261022`,
volume `webhook-1903-p4-supplement-20261022-data`, loopback port 16392.
Container/volume/listener inventory preceded creation; its ID and mount were
verified again before cleanup. Same AOF/always/noeviction/128 MiB configuration
and isolated DB allocation as above. No shared service changes or FLUSH commands.

From `CubeMaster/`, using the same Go 1.25.7 and cache/proxy environment above:

```sh
export WEBHOOK_REDIS_ADDR=127.0.0.1:16392 WEBHOOK_REDIS_PASSWORD=p4-supplement-test
export WEBHOOK_P4_REDIS_ADDR=127.0.0.1:16392 WEBHOOK_P4_REDIS_PASSWORD=p4-supplement-test

"$GO" test -json -gcflags='github.com/tencentcloud/CubeSandbox/CubeMaster/...=-l' \
  ./pkg/webhook ./pkg/service/sandbox ./pkg/service/httpservice/cube \
  ./pkg/templatecenter ./pkg/task ./pkg/sandboxspec ./cmd/cubemaster/app \
  ./pkg/base/hotswap ./pkg/remotestatus ./pkg/pausesnap ./internal/webhooktest \
  -count=1 -timeout 180s
"$GO" test -race -json -gcflags='github.com/tencentcloud/CubeSandbox/CubeMaster/...=-l' \
  ./pkg/webhook ./pkg/service/sandbox ./pkg/service/httpservice/cube \
  ./pkg/templatecenter ./pkg/task ./pkg/sandboxspec ./cmd/cubemaster/app \
  ./pkg/base/hotswap ./pkg/remotestatus ./pkg/pausesnap ./internal/webhooktest \
  -count=1 -timeout 180s
"$GO" test -gcflags='github.com/tencentcloud/CubeSandbox/CubeMaster/...=-l' \
  ./pkg/templatecenter -run '^TestP4CheckpointFinalBoundary$' -count=1 -timeout 60s
"$GO" test ./pkg/webhook -run '^TestP4CaptureDistribution$' -count=1 -v
```

All four commands exited 0. The focused normal command reruns the final
claim-before/after cancellation distinction added after the full normal run;
the full race run includes that final version. All four business Redis/HTTP
paths ran again. JSON logs contain zero skipped test cases. Go reports package
skip for `hotswap` and `internal/webhooktest` because neither has standalone test
files; both are exercised through their callers.

| Package | Normal seconds | Race seconds |
| --- | ---: | ---: |
| pkg/service/httpservice/cube | 0.104 | 1.414 |
| pkg/task | 0.020 | 1.050 |
| pkg/sandboxspec | 0.079 | 1.242 |
| cmd/cubemaster/app | 0.526 | 1.612 |
| pkg/remotestatus | 0.076 | 1.225 |
| pkg/pausesnap | 0.102 | 1.337 |
| pkg/service/sandbox | 13.864 | 10.944 |
| pkg/webhook | 29.817 | 38.095 |
| pkg/templatecenter | 51.061 | 55.355 |

Logs: `/tmp/webhook-p4-supplement-{normal,race}.jsonl`,
`/tmp/webhook-p4-supplement-final-cancel.log`,
`/tmp/webhook-p4-supplement-focused-race.log`, and
`/tmp/webhook-p4-supplement-measure.log`. The earlier failing supplemental fixture
log is `/tmp/webhook-p4-supplement-focused.log`.

The Redis service restart substep remains unrequested/unexecuted; this is not
silently counted as a passing restart test. Live platform/cross-node/power-loss
acceptance remains P6. P5's actual updated/killed suppression tests remain P5.

### Refreshed capture measurements

Same 1000 samples, 8209-byte metadata and capture-only scope as above, without
race instrumentation. These are new observations, not reused P3/P4 numbers.
The deterministic backend overlap checks are in the business fault tests;
this sequential distribution benchmark retains its original workload and is
not a measurement of sustained backend saturation.

| Event / scenario | p50 ns | p95 ns | p99 ns |
| --- | ---: | ---: | ---: |
| created/disabled | 2144 | 10390 | 61858 |
| created/enabled | 5901 | 26400 | 86754 |
| created/full | 6242 | 21290 | 86404 |
| created/stopped | 4008 | 11883 | 80874 |
| created/disconnected | 5350 | 20469 | 79611 |
| created/slow-store | 6482 | 27322 | 65705 |
| created/slow-http | 8366 | 31410 | 185092 |
| resumed/disabled | 3697 | 12203 | 57329 |
| resumed/enabled | 8566 | 20779 | 214247 |
| resumed/full | 4519 | 18996 | 68089 |
| resumed/stopped | 3005 | 12284 | 42721 |
| resumed/disconnected | 6071 | 18886 | 51338 |
| resumed/slow-store | 6994 | 19487 | 90131 |
| resumed/slow-http | 7203 | 25178 | 111291 |
| paused/disabled | 2986 | 10510 | 27061 |
| paused/enabled | 7834 | 21020 | 93948 |
| paused/full | 5450 | 17102 | 50997 |
| paused/stopped | 2916 | 10029 | 54143 |
| paused/disconnected | 5741 | 13676 | 50706 |
| paused/slow-store | 5641 | 17202 | 28584 |
| paused/slow-http | 6352 | 20619 | 69312 |
| checkpointed/disabled | 2044 | 30819 | 42711 |
| checkpointed/enabled | 7644 | 17433 | 93207 |
| checkpointed/full | 8416 | 41739 | 125207 |
| checkpointed/stopped | 6553 | 28254 | 95160 |
| checkpointed/disconnected | 9037 | 22102 | 98227 |
| checkpointed/slow-store | 6282 | 16301 | 27181 |
| checkpointed/slow-http | 7805 | 26710 | 111301 |

After all supplemental commands exited, the verified supplemental Redis
container and its dedicated volume were removed successfully. Logs remain in
`/tmp`. `git diff --check` passed. No staging, commits, pushes or PRs were made.
