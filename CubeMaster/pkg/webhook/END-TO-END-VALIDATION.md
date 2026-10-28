# Webhook end-to-end validation

This summary describes the 2026-10-28 acceptance of Issue #1903. It is an
execution report, not a performance gate or a guarantee for every deployment.
See the [runbook](../../../tests/e2e/webhook/README.md) for setup and opt-ins.
Raw receiver payloads, process/configuration snapshots, JUnit, JSONL and failed
attempts are retained separately by the operator and are not repository inputs.

## Coverage and results

The deployed Master was built from the tested working tree. CubeAPI, Cubelet,
CubeOps, lifecycle manager and S3 services used inventoried baseline binaries;
this does not claim that every service was rebuilt from current source.
Business Redis and notification Redis were separate services. XFS and S3 used
real VM operations and distinct templates. Faults targeted dedicated resources.

| Layer | Final result |
| --- | --- |
| Go ordinary / race | Each: 1,788 passing test/subtest identities, no failures, 3 skips |
| Python payload / HTTP receiver | 2 / 3 passed |
| XFS platform | 67 applicable parameterized identities passed |
| S3 platform | 41 applicable parameterized identities passed |
| SDK coverage | 78 distinct identities; 108 applicable backend combinations |
| Redis restart / process crash | Same-volume normal restart and SIGKILL/start passed |
| Sentinel | Actual election and full-Master delivery recovery passed |

Go counts include subtests, not just top-level functions. The default-suite skips
were the opt-in RESP proxy, opt-in Sentinel election and `config.TestInit`.
The first two ran successfully in separate dedicated experiments. `TestInit`
remained unrun because its external configuration fixture was unavailable.
Backend-independent delivery/control scenarios ran on XFS; their results are
not relabeled as S3 runs. XFS read-only directory faults are not applicable to
S3; native RPC injection supplies S3 local-failure coverage.

The suites exercise:

- All six lifecycle events through real SDK operations, payload projection,
  immutable metadata, retry body/event identity and per-attempt delivery IDs.
- Final create/resume/pause/checkpoint/update/destruction boundaries, failed
  database writes and cleanup, internal-operation suppression and no duplicate
  publication after remote status changes.
- Put/Claim/Complete/Retry faults both before execution and after execution with
  lost replies; missing Receipts never authorize HTTP delivery.
- Full-Master SIGTERM, inflight shutdown, SIGKILL, competing Masters, lease
  takeover, stale Receipt rejection and crash after Claim before HTTP.
- Subscription configuration/revocation, finite delivery budgets, queue capacity,
  notification Redis outage and OOM. OOM recovery audits records/indexes before
  restarting; it does not delete queues or replenish attempts.
- Trusted HTTPS, untrusted CA and hostname mismatch without disabling TLS checks.
- S3 export error/empty results for both pause and checkpoint, upload outage
  after local success, recovery and independent logical-node restore. Failed
  native manifest/registry PUTs remain storage diagnostics; valid local success
  notifications are neither retracted nor duplicated.

Initial failed attempts were retained separately. They included fixture runtime
permissions, proxy wiring, lease/slow-response timing, missing Python dependencies
and target-node storage readiness. Final passes followed corrections to dedicated
fixtures and test orchestration, without changing production storage behavior or
relaxing webhook security checks. This table reports final outcomes, not a claim
that every attempt passed.

## Business-path isolation measurements

`test_deployment_api_isolation_and_latency` calls the real SDK `set_timeout`
entrypoint with five warmups per phase, concurrency 1 and two delivery workers.
All 300 measured API calls succeeded. Notification Redis stayed paused for
40 seconds, exceeding the Writer retry window; requests completed while the
fault remained active. Resource snapshots and per-request measurements were
retained with the private run evidence.

| Webhook condition | Samples | p50 ms | p95 ms | p99 ms |
| --- | ---: | ---: | ---: | ---: |
| Disabled | 60 | 6.01 | 17.27 | 114.65 |
| Healthy | 60 | 6.78 | 18.76 | 99.45 |
| Slow HTTP | 60 | 6.20 | 8.64 | 62.33 |
| Notification Redis unresponsive | 40 | 7.54 | 23.38 | 81.71 |
| Capacity constrained | 80 | 5.63 | 19.22 | 245.83 |

Component tests independently block the Writer, fill ingress and verify prompt
`PublishFull` responses. Slow HTTP alone is not proof of saturated ingress.
These observations support business-path isolation under the tested faults;
they do not establish zero overhead, a throughput SLO or high-concurrency
performance across all lifecycle operations. Capacity pressure showed a larger
tail latency and must not be described as having no performance impact.

## Reproduction and limits

Use the runbook's dedicated deployment, explicit environment opt-ins and fresh
evidence directories. Run ordinary and race component tests, receiver/payload
checks, then real SDK suites separately for each backend. Run disruptive tests
serially. Preserve command, environment provenance, exit code, skip reason and
before/fault/after observations; do not count collection or missing-opt-in skips
as executed acceptance. Test files enforce resource ownership checks that must
remain enabled.

Cross-node and Sentinel results use logical resources on one host. They do not
prove physical HA, arbitrary source-path portability or off-host durability.
Host reboot, hard reset and physical power loss were not exercised in this
reacceptance; SIGKILL is not a power-loss experiment. Redis Cluster is outside
the supported routing scope. Pre-persistence loss, possible delivery duplicates,
finite retries and local snapshot versus remote durability semantics remain as
documented in [DELIVERY.md](DELIVERY.md). No signature protocol, dynamic
subscription API, replay API or exactly-once guarantee is introduced.

## Portable fixture configuration follow-up

The test fixtures now use an explicit operator-selected run root, exact Docker
ownership labels, inventoried unit/binary paths and configurable endpoints.
The runbook and environment example replace the original host-specific setup.
The results above describe the preceding full-platform execution; they are not
reclassified as a full-platform rerun of these fixture changes. Re-run selected
opt-in deployment scenarios on the configured target before claiming acceptance
there. Local fixture checks cover alternate paths and rejection of foreign paths,
labels and processes; the storage proxy check uses real Unix RPC forwarding.

Follow-up checks passed: 9 local receiver/resource/RPC fixture tests, 2 payload
assertion tests and collection of 78 platform cases. The changed Go fixtures
compile and skip without opt-ins. A fresh dedicated Redis container with a
random loopback publication verified real proxy forwarding and foreign-label
rejection; it was removed afterwards. No host reboot or full platform rerun was
performed for this portability follow-up.
