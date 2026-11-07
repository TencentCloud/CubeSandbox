# Best-effort webhook lifecycle tests

These tests exercise bounded in-memory notification delivery. Process exit loses
pending work; there is no notification Redis, persisted terminal state, replay,
Sentinel or cross-Master takeover. Business Redis is still a platform dependency.
This runbook describes tests, not a completed platform acceptance report.

## Local checks

Use an existing Python 3.10+ environment satisfying
[SDK requirements](../sdk_compat/requirements.txt), including PyYAML. If preparing
a new environment, install those requirements and the repository SDK with its
existing versions; dependency/toolchain upgrades are not a test fix.

```bash
# repository root, with the chosen environment active
REPO=$(pwd)
python3 -m unittest discover -s tests/e2e/webhook -p test_receiver.py -v
python3 -m unittest discover -s tests/e2e/webhook -p test_resources.py -v
python3 -m unittest discover -s tests/e2e/webhook -p test_storage_proxy.py -v
cd "$REPO/tests/e2e/sdk_compat"
python3 -m pytest --confcutdir=. cases/framework/test_webhook.py -q
python3 -m pytest --confcutdir=. --collect-only cases/lifecycle/test_webhook*.py -q
```

Receiver tests cover bounded records/concurrency, raw body and headers, 503,
disconnect, retry identity and actual rejection policies. Resource tests cover
path/symlink escape, exact labels, units and executable ownership. The small Unix
RPC fixture tests native forwarding and local snapshot rejection. None of these
is a six-event platform run; collection is only discovery.

Use the repository's existing Go toolchain or builder, from `CubeMaster`:

```bash
go test -count=1 -gcflags=all=-l ./pkg/webhook ./pkg/base/config \
  ./pkg/service/httpservice/cube ./pkg/service/sandbox ./pkg/task \
  ./pkg/templatecenter ./pkg/lifecycle ./pkg/pausesnap ./pkg/sandboxspec \
  ./cmd/cubemaster/app
# Repeat with -race; gomonkey-based suites require disabled inlining.
go vet ./internal/webhooktest ./pkg/webhook ./pkg/base/config \
  ./pkg/service/httpservice/cube ./pkg/service/sandbox ./pkg/task \
  ./pkg/templatecenter ./pkg/lifecycle ./pkg/pausesnap ./pkg/sandboxspec \
  ./cmd/cubemaster/app
make test TEST_FLAGS=-count=1
make test-race
```

Core delivery tests need no Redis opt-in. The separate
`TestBusinessRedisTimeoutRuntimeHTTP` uses
`WEBHOOK_TEST_BUSINESS_REDIS_ADDR` / `WEBHOOK_TEST_BUSINESS_REDIS_PASSWORD` and DB 8
on an isolated disposable **business lifecycle** Redis. It verifies existing
Redis metadata → timeout response → memory Runtime → HTTP, and must never point
at a live business database. HSET/XADD order and best-effort failures remain
ordinary regressions. A missing fixture skip is not a pass. Existing create trace
and hotswap races must be reported with logs, not hidden by removing tests.

## Real six-event run

Reuse a healthy Master/Cubelet/API deployment and a real READY template. The
runner does not provision a platform, change mounts or repair storage. Copy and
review [env.example](env.example) outside Git, chmod 600, then source it. Basic
lifecycle tests need only the SDK API/template and accessible receiver log plus
subscription ID; they do not require systemd/Docker inventory or control opt-in.
Use synthetic metadata because evidence contains actual payloads.

Configure the existing dedicated Master with this notification block (normal
business Redis/MySQL configuration stays with the platform):

```yaml
webhooks:
  enabled: true
  allow_http: true            # private test receiver only
  allowed_cidrs: [127.0.0.0/8] # replace with its exact reachable test network
  max_attempts: 3
  store:
    max_records: 10000
  subscriptions:
    - id: review-receiver
      enabled: true
      url: http://127.0.0.1:18096/events
      events:
        - sandbox.lifecycle.created
        - sandbox.lifecycle.resumed
        - sandbox.lifecycle.paused
        - sandbox.lifecycle.checkpointed
        - sandbox.lifecycle.updated
        - sandbox.lifecycle.killed
```

Restart the dedicated Master under your deployment procedure. In one shell:

```bash
printf '204\n' > "$WEBHOOK_TEST_RECEIVER_MODE"
python3 "$REPO/tests/e2e/webhook/receiver.py" --host 127.0.0.1 --port 18096 \
  --output "$WEBHOOK_TEST_RECEIVER_LOG" --mode-file "$WEBHOOK_TEST_RECEIVER_MODE" \
  --limit 2000 --workers 4 --fsync
```

`RECEIVER_MODE` is optional for an always-successful basic receiver: omit
`--mode-file` if not using faults. Set it explicitly for the command above.
For remote Masters choose a restricted reachable interface and matching CIDR.
Logs are exclusively created; restart with a new path and update the runner.
The receiver records bytes/headers before responding, enforces 256 KiB/read/time
limits and 1–16 workers. Use `retry-once` plus `WEBHOOK_TEST_EXPECT_RETRY=1` to
require 503→204 attempts for each operation. Slow mode defaults to 10 seconds;
retain that setting for the bounded HTTP timeout/shutdown scenarios.

In another shell:

```bash
cd "$REPO/tests/e2e/sdk_compat"
SDK_E2E_TRACE=true SDK_E2E_REPORT_DIR=/absolute/fresh/evidence/sdk \
  python3 -m pytest --run-e2e --confcutdir=. cases/lifecycle/test_webhook.py -vv \
  --junitxml=/absolute/fresh/evidence/junit.xml
```

This uses actual SDK operations, including checkpoint, pause/resume and kill.
Async acceptance is separate from handler success. Metadata is checked only on
trusted current create requests; subsequent events must omit it. Timeout/network/
killed template IDs are empty when unavailable; killed execution resources are
unknown. Finite/zero timeout uses the successful response deadline, including
fallback; -1 emits no timeout update. A successful response does not certify HSET.
No direct Capture or handcrafted HTTP event is platform evidence.

## Optional boundaries on an isolated deployment

Run serially, without xdist. Set `WEBHOOK_TEST_CONTROL=1` only for a deployment
inventoried for this run. `RUN_ROOT` must be an existing dedicated absolute
subdirectory; paths must resolve strictly inside it, including symlinks. Container
names and units retain `webhook-1903-`; units end in `.service`, with ExecStart
binaries inside RUN_ROOT. `MASTER_BINARY` / `NODE_BINARY` must match actual PID
executables. Every controlled container needs the exact unique
`purpose=webhook-test-...` label. Never relabel or clean an old experiment
implicitly. Control requires schema `wh_test_[0-9_]+`; trigger DDL additionally
requires user `webhook_test_1903` and the matching isolated MySQL container.

| File | Coverage / additional settings (all `WEBHOOK_TEST_` prefixed) |
| --- | --- |
| `test_webhook_delivery.py` | Disabled/invalid config, static startup subscriptions, fanout/filter, SIGTERM/restart loss. `HTTP_FAILURES=1` covers finite 400/401/503/disconnect/timeout attempts. Fanout requires `CONCURRENT_RECEIVER_URL/LOG`. |
| `test_webhook_boundaries.py` | `NODE_FAULTS=1`, owned node unit/binary, XFS root, egress container and LCM unit as applicable. Preserve node failure negatives and sync/async/paused/timeout destroy behavior. `BUSINESS_PROXY_DIR` plus inventoried business Redis tests HGET fallback and HSET/XADD best-effort faults. |
| `test_webhook_completion_boundaries.py` | Existing business RESP proxy for create rollback and async cleanup; `NODE_CLI/SOCKET` for full-ID node observations. `DATABASE_FAULTS=1` plus owned MySQL for Complete/READY/spec failures and warn-only paused binding deletion. `PROXY_FAULTS=1` plus owned CubeProxy for resume purge. Receiver rejection and metadata limits need only Master/receiver control. |
| `test_webhook_storage.py` | `STORAGE_BACKEND=xfs` or `s3`, matching real template, receiver and `MASTER_URL`: local READY and actual same-node user snapshot restore. No control opt-in needed. |
| `test_webhook_s3_boundaries.py` | `STORAGE_BACKEND=s3`, `STORAGE_PROXY_MODE` with the narrow local snapshot fixture below; pause/checkpoint failure negatives. |
| `test_webhook_tls.py` | `TLS_CA` and trusted/untrusted/wrong-host HTTPS endpoints forwarding to the receiver log. Scope `SSL_CERT_FILE` to the dedicated Master. No global CA changes or disabled verification. |

Run the selected file with the same `--run-e2e --confcutdir=.` and fresh
JUnit/JSONL paths. Business proxy tests consume an **already running** dedicated
RESP fault fixture: owned directory containing `ready`, atomic `control.json`
(`id`, `op`, `when=before`, `error`, optional key/field/repeat), and
`fault-<id>*.json` match evidence. They do not start an old notification Redis
proxy test. If that external fixture is unavailable, record the missing
integration coverage; the Go business Redis regressions remain independently
runnable.

For S3 local rejection only, point the dedicated node's existing storage socket
configuration at a proxy under RUN_ROOT using your deployment procedure:

```bash
printf 'forward\n' > "$WEBHOOK_TEST_STORAGE_PROXY_MODE"
python3 "$REPO/tests/e2e/webhook/storage_proxy.py" \
  --upstream "$WEBHOOK_TEST_RUN_ROOT/rcow/native.sock" \
  --listen "$WEBHOOK_TEST_RUN_ROOT/proxy-local-snapshot/rpc.sock" \
  --control "$WEBHOOK_TEST_STORAGE_PROXY_MODE" \
  --evidence "$WEBHOOK_TEST_RUN_ROOT/proxy-local-snapshot/faults.jsonl"
```

Only `forward` and `snapshot-error` are supported; no fabricated success, export
reply manipulation, HTTP upload proxy, independent-node orchestration or host
reboot. Existing small Go upload-reconciliation tests retain local READY/no-repeat
checks. Run XFS and S3 separately and report their actual backend; one backend's
result cannot establish the other's behavior. Deterministic queue capacity,
DNS/SSRF/allowlist, TLS/hostname, proxy/redirect/POST-replay protection and webhook's
own two-second shutdown budget are exercised in Go components. The platform
SIGTERM observation includes existing App/systemd shutdown, not just that budget.

## Evidence and restoration

Save commands/cwd, UTC times, versions, source diff, redacted environment, exit
codes, stdout/stderr, JUnit/SDK JSONL and receiver raw bytes/headers. Keep initial
failures. Distinguish passed, failed, skipped, compile-only, collection-only and
not-run; do not claim full v1 acceptance from collection, normal restart or Redis
health. Scope an existing service problem separately; do not alter storage state
mapping, destroy retries or timeout/spec behavior to make webhook tests pass.

Control fixtures restore original YAML and receiver mode and record health/PID;
fault tests restore mounts, processes, containers, triggers and control modes in
finally blocks. After interruption, independently inspect those exact resources.
Preserve data/evidence; never FLUSHALL/FLUSHDB, global prune or delete another run.

All retained `WEBHOOK_P6_<suffix>` options were renamed to
`WEBHOOK_TEST_<suffix>` with no compatibility alias. Removed notification Redis,
Sentinel, second-Master, remote-upload and host inventory options have no v1
replacement. Database ownership names changed from `whp6_...` / `p6_1903` to the
explicit names above; existing resources are not migrated automatically.
