# Update and completed-destruction webhook validation — 2026-10-23

> Historical execution record: machine paths and resource names below identify
> that run, not required installation locations. For current portable setup and
> commands, use the [test runbook](../../../tests/e2e/webhook/README.md).


Date: 2026-10-22 (Asia/Shanghai). Workspace base: c771b279 on
feat/E2B-compatible-sandbox-lifecycle-webhooks. No commit, branch change, push,
PR or P6 implementation. Existing .agents/, .scratch/, spec/ and
"sue -1903 P1." were preserved. The spec index is intentionally edited within
the existing untracked spec tree.

## Implementation and success boundaries

- TimeoutProvider returns EndAt, confirmed HSET application, and read error
  separately. PublishUpdate still attempts XADD even after HSET failure.
  HSET success/XADD failure can notify; HSET failure, disabled store, missing
  meta/provider and response fallback cannot. SetTimeout and Refresh share one
  producer. Finite/zero values use captured EndAt, -1 skips normally.
- UpdateNetwork publishes after node success and the existing best-effort spec
  attempt. Only request AllowOut/DenyOut/internet false are captured. Writer
  maps the three E2B arrays; no L7 details, Inject or credentials enter capture.
- DestroySandbox captures reason/origin/P4 fields before defaulting. HTTP
  explicit delete supplies request; CLM already sends timeout; unknown internal
  empty reason becomes unknown. Sync and queued Task completion publish only
  after explicit destruction evidence and required Master cleanup.
- Task.Destroyed is accessed under the existing per-sandbox lifecycle lock and
  survives task retries. After confirmed normal node destruction a cleanup retry
  does not issue the destructive RPC again. This progress exists independently
  of notification enablement. Paused retries still execute package cleanup.
  The snapshot's atomic publication flag handles repeated completion calls.
- TryDeletePausedWithEvidence preserves the existing tolerant error result but
  distinguishes nil/absent Ret/not-found from explicit node success. Evidence
  survives same-task package cleanup failure. Warn-only pause metadata cleanup
  remains warn-only.
- PublishContext checks trusted origin; carrying context alone is not treated
  as suppression. App installs its one Publisher in task too. Direct destroy
  Task constructors were checked: only normal DestroySandbox and create
  failover exist; failover has no normal snapshot and retains internal origin.
- Existing spec reads are reused; test call counts assert one spec Get on normal
  network/delete paths. No notification-specific IO, JSON, DNS, waiting enqueue
  or per-event goroutine was added. Mapping/encoding stays in Writer.

## Environment and exact commands

Go 1.25.7 linux/amd64 at /tmp/webhook-p3-toolchain/go/bin/go; Linux x86_64.
Existing module/build caches used. Initial sandboxed listeners were denied;
tests were rerun with local-listener permission. No test result below is taken
from P3/P4 historical reports.

Disposable Redis 7.4.9 was started from the already cached image
cube-sandbox-image.tencentcloudcr.com/opensource/redis:7-alpine as
webhook-1903-p5-local-20261022, loopback 127.0.0.1:16393, AOF,
appendfsync=always, 128 MiB, noeviction. The empty port and container names were
checked first. Container ID db71665c860c7ba00dfeb39d8f3ec71a8aa2427b1db2becef99c523c00e5bcf4;
new anonymous data volume
a8187d62ce8ed624d24069663ac2e67dc8a62b729f4795ae94dc25b18bb28f82.
No shared Redis and no FLUSHDB/FLUSHALL. An initial Docker Hub redis:7.4 pull
stalled and was interrupted; the cached image provided the real test server.

P2/P3 use random namespaces in DB 9. Existing P4 components use DB 10/11/14,
fault tests DB 12. New P5 producer components use DB 13, authoritative lifecycle
HSET component DB 8, and App async-delete component DB 15.
Tests use local HTTP and Cubelet stubs, not a real sandbox platform.

From CubeMaster:

```sh
export GO=/tmp/webhook-p3-toolchain/go/bin/go
export GOMODCACHE=/home/ubuntu/.cache/cube-sandbox-builder/go/pkg/mod
export GOCACHE=/home/ubuntu/.cache/go-build
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
export WEBHOOK_REDIS_ADDR=127.0.0.1:16393 WEBHOOK_REDIS_PASSWORD=p5-isolated-test
export WEBHOOK_P4_REDIS_ADDR=127.0.0.1:16393 WEBHOOK_P4_REDIS_PASSWORD=p5-isolated-test

"$GO" test -json -gcflags='github.com/tencentcloud/CubeSandbox/CubeMaster/...=-l'   ./pkg/webhook ./pkg/lifecycle ./pkg/service/sandbox ./pkg/task   ./pkg/pausesnap ./pkg/sandboxspec ./pkg/templatecenter   ./pkg/service/httpservice/cube ./cmd/cubemaster/app -count=1 -timeout 240s
"$GO" test -race -json -gcflags='github.com/tencentcloud/CubeSandbox/CubeMaster/...=-l'   ./pkg/webhook ./pkg/lifecycle ./pkg/service/sandbox ./pkg/task   ./pkg/pausesnap ./pkg/sandboxspec ./pkg/templatecenter   ./pkg/service/httpservice/cube ./cmd/cubemaster/app -count=1 -timeout 240s
"$GO" test ./pkg/webhook   -run 'TestP5CaptureDistribution|TestP4CaptureDistribution/sandbox.lifecycle.(updated|killed)'   -count=1 -v
```

The existing gomonkey fixtures require disabling inlining for CubeMaster;
race instrumentation remains enabled. Runner: /tmp/webhook-p5-run.sh.
Raw results: /tmp/p5-final-normal.jsonl, /tmp/p5-final-race.jsonl and
/tmp/p5-measure.log. An earlier complete ordinary run without Redis also passed;
its opt-in real-Redis components did not run and are not the evidence below.

## Final results

| Run | Result |
| --- | --- |
| normal | 9 packages passed; 1718 test/subtest pass records; 0 failures; 0 test skips |
| race | 9 packages passed; 1718 test/subtest pass records; 0 failures; 0 test skips |

Counts include parent tests and subtests, not independent acceptance cases.
The optional Redis service-restart substep is separately unexecuted below.

## Matrix and evidence

| Matrix row | Executed evidence |
| --- | --- |
| HSET/XADD four combinations | TestP5TimeoutAuthoritativeWrite asserts flag, both attempted commands and exact serialized EndAt |
| Missing store/meta, disabled, read error | Same test; existing lifecycle decode/load tests remain in full suite |
| Finite, zero, -1, fallback, provider absent, finite zero, invalid, repeated Refresh | TestP5TimeoutAuthority and existing sandbox_timeout/update_timeout suites; UTC seconds conversion asserted |
| Resume internal refresh | TestP5TimeoutAuthority/resume plus TestP4ResumeTimeoutOrigin; no updated |
| Network success/failure and best-effort spec | TestP5NetworkBoundary; TestP5ProducerFailureBoundaries checks lock and rejected Ret |
| Projection and input immutability | TestP5NetworkBoundary compares request JSON before/after; asserts CIDRs, wildcard, deny-all, explicit DNS; mixed and L7-only omit secrets/hosts |
| nil/empty/protobuf | TestP5NetworkProtoProjection marshals and unmarshals actual CubeNetworkConfig; all three empty keys are null |
| Sync/async completion | TestP5DestroyBoundary; enqueue has empty ingress; TestP5DestroyCompletionBarrier uses entered/release channels to prove no event before node completion |
| RPC, empty reply, missing node, cleanup failures | TestP5DestroyBoundary; failed operations have no event |
| Cleanup retry, duplicate callback, independent not-found | TestP5DestroyBoundary/cleanup-fail/async retains evidence with node now missing; TestP5TaskRetryAfterRPCFailure handles first RPC failure, explicit reason and fresh-request not-found |
| Paused READY/FAILED/CREATING/DELETE_FAILED | TestP5PausedDeleteEvidence covers success, nil, absent Ret, not-found and RPC error for each status |
| Paused package retry | TestP5PausedCleanupRetryEvidence retains evidence across cleanup failure then tolerated nil destroy and successful package cleanup; subsequent absent binding has no new evidence |
| Paused producer and hook result | TestP5ProducerFailureBoundaries covers unproven, error and confirmed paused result; warn-only hook still permits event |
| Internal origins | TestP5ProducerFailureBoundaries, TestP5CaptureBudgetAndSnapshot and existing TestP4OriginAcrossDestroyQueueRetry (actual queue, retry, WithoutCancel) |
| Reasons and fields | TestP5DestroyBoundary checks request/timeout/unknown, copied metadata after input mutation, CPU; explicit evicted reason in retry test; P4 resource mapper tests retained |
| Bounds and immutable DTO | TestP5CaptureBudgetAndSnapshot tests entry/count/byte limits and mutated network inputs; oversize network/reason producer tests |
| Disabled, full, stopped, disconnected/slow store, slow HTTP, concurrent shutdown | TestP5BusinessFailureIsolation reuses FaultPublishers with updated/killed subscriptions added; real production methods retain success |
| Runtime body and retry identity | TestP5BusinessRedisHTTP receives exactly two updated/two killed through real Redis; shared receiver returns 503 then 204 and checks identical body/event ID with new delivery ID |
| Authoritative timeout Runtime | TestP5AuthoritativeRedisHTTP uses actual Store/provider, real HSET/XADD, real SetTimeout and captured response/stored EndAt comparison at receiver |
| App task injection | TestP5AppPublisherRealAsyncDelete calls App.startWebhooks, real DestroySandbox and task handler; its HTTP receiver gets killed |
| P4 four event regressions | Entire existing webhook, sandbox, templatecenter and App suites rerun, including opt-in business Redis/HTTP components |

The async producer tests control queue acceptance and invoke the real handler;
the existing queue retry test separately exercises the real queue. These are
component seams, not a deployed Master task scheduler/platform test.

Initial failures were resolved: a P4 test still patched the old paused-delete
entrypoint after evidence API wiring, and initial new assertions assumed success
code 0 instead of the actual Master 200. Final runs include those corrections.
No failure is hidden by selecting only new tests.

## Measurements

These are observations, not performance gates. Each row uses 1000 samples;
allocations use AllocsPerRun(100). Small-field tests measure capture/publication,
not business RPCs. Full/stopped still pay bounded capture costs. Large-field
measurements also run existing PrepareLifecycle with 8209 bytes metadata even
when the holder is disabled, so their disabled result is not the production
early-skip path. The slow-store measurement injects a 200ms Put delay and the
slow HTTP peer delays 200ms; separate isolation tests use deterministic blocked
backend handshakes. Neither is a production Redis load benchmark.

| Probe | Observation |
| --- | --- |
| large/sandbox.lifecycle.updated/disabled | n=1000 metadata=8209B p50=1623ns p95=8717ns p99=41679ns |
| large/sandbox.lifecycle.updated/enabled | n=1000 metadata=8209B p50=9678ns p95=29326ns p99=142681ns |
| large/sandbox.lifecycle.updated/full | n=1000 metadata=8209B p50=7214ns p95=43803ns p99=190091ns |
| large/sandbox.lifecycle.updated/stopped | n=1000 metadata=8209B p50=6592ns p95=26962ns p99=118685ns |
| large/sandbox.lifecycle.updated/disconnected | n=1000 metadata=8209B p50=10610ns p95=25247ns p99=167509ns |
| large/sandbox.lifecycle.updated/slow-store | n=1000 metadata=8209B p50=9758ns p95=26550ns p99=111712ns |
| large/sandbox.lifecycle.updated/slow-http | n=1000 metadata=8209B p50=6292ns p95=18365ns p99=169191ns |
| large/sandbox.lifecycle.killed/disabled | n=1000 metadata=8209B p50=2735ns p95=39845ns p99=88548ns |
| large/sandbox.lifecycle.killed/enabled | n=1000 metadata=8209B p50=9418ns p95=28754ns p99=221060ns |
| large/sandbox.lifecycle.killed/full | n=1000 metadata=8209B p50=6833ns p95=36940ns p99=103867ns |
| large/sandbox.lifecycle.killed/stopped | n=1000 metadata=8209B p50=7044ns p95=27542ns p99=121270ns |
| large/sandbox.lifecycle.killed/disconnected | n=1000 metadata=8209B p50=8576ns p95=26230ns p99=108426ns |
| large/sandbox.lifecycle.killed/slow-store | n=1000 metadata=8209B p50=8305ns p95=29736ns p99=130979ns |
| large/sandbox.lifecycle.killed/slow-http | n=1000 metadata=8209B p50=5471ns p95=19567ns p99=51128ns |
| small/timeout/disabled | n=1000 p50=40ns p95=41ns p99=41ns allocs=0 |
| small/timeout/enabled | n=1000 p50=972ns p95=5230ns p99=6673ns allocs=5 |
| small/timeout/full | n=1000 p50=922ns p95=1382ns p99=1773ns allocs=5 |
| small/timeout/stopped | n=1000 p50=821ns p95=1262ns p99=1903ns allocs=5 |
| small/network/disabled | n=1000 p50=40ns p95=41ns p99=41ns allocs=0 |
| small/network/enabled | n=1000 p50=1112ns p95=1713ns p99=2264ns allocs=9 |
| small/network/full | n=1000 p50=1272ns p95=4319ns p99=8757ns allocs=9 |
| small/network/stopped | n=1000 p50=952ns p95=4248ns p99=6052ns allocs=9 |
| small/killed/disabled | n=1000 p50=40ns p95=41ns p99=41ns allocs=0 |
| small/killed/enabled | n=1000 p50=1252ns p95=1884ns p99=2304ns allocs=15 |
| small/killed/full | n=1000 p50=1483ns p95=2946ns p99=14247ns allocs=15 |
| small/killed/stopped | n=1000 p50=1132ns p95=3586ns p99=4358ns allocs=15 |

No fixed threshold or baseline was added. Timing was collected on this shared
development host while validation was active; it is not a latency SLO.

## Compatibility and unexecuted platform work

- killed is later than E2B accepted/stopping; timeout -1 emits no timeout-only
  event. Worker formats the captured millisecond deadline to RFC3339 seconds.
- Empty network categories are null, not []; explicit DNS is retained while
  node-injected DNS/private rules and unapplied E2B defaults are absent.
  L7-only updates on this path notify but Rules/Inject are not represented;
  other L7 paths have no complete coverage guarantee.
- Timeout does not fetch spec for template/metadata. Delete with pre-stamped
  backend retains the existing spec-read bypass and therefore unknown fields.
  Legacy spec metadata remains unknown. P4 resource mapping is reused:
  fractional CPU, ambiguous multi-container resources, missing execution
  identities/start/time and empty team remain strict-receiver limitations.
- A new request/process with only nil/not-found responses cannot reuse a lost
  prior task's evidence; it conservatively emits nothing. Task-local duplicate
  prevention is not cross-process or global exactly-once. Node idempotent
  success indistinguishable from fresh success cannot establish global
  producer deduplication. Pause metadata GC errors remain warning-only.
- No Redis service restart was requested via WEBHOOK_REDIS_RESTART_CONTAINER;
  that optional substep is not claimed. Existing Redis/task recovery and HTTP
  security tests did run. No Sentinel/Cluster failover, real VM destruction,
  XFS/S3 runtime, full Master deployment, cross-node or power-loss validation
  was run. Those platform/operational acceptance items remain P6.
- No signatures, dynamic subscription API, complete delivery-history API or
  new storage backend. Pre-persistence loss and post-persistence duplicate
  delivery windows remain. No notification fault changes business responses.

## Final workspace and fixture cleanup

After both final runs completed, container identity, mounts and volume references
were inspected again. Only the dedicated test container referenced its new
anonymous volume. The container and that volume were removed with
docker rm -fv webhook-1903-p5-local-20261022; no existing container/image/volume
or repository file was cleaned. Test logs remain under /tmp.

git diff --check passed. Current branch and HEAD remain unchanged. Changes are
unstaged; no commit, amend, push, PR, branch switch or reset was performed.
