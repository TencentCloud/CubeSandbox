# Webhook tests and dedicated deployment

Start with the [feature guide](../../../CubeMaster/pkg/webhook/README.md) /
[中文使用说明](../../../CubeMaster/pkg/webhook/README_zh.md) for enabling webhooks,
event semantics and supported features. This runbook covers reproducible tests.
[Acceptance results](../../../CubeMaster/pkg/webhook/END-TO-END-VALIDATION.md)
describe a previous execution, not a promise that your deployment has passed.

## 1. Choose the test layer

| Layer | Needs | What it proves |
| --- | --- | --- |
| Receiver/resource self-tests | Python 3.10+, local TCP/Unix sockets | Fixture behavior and resource ownership checks |
| Payload tests | SDK test Python dependencies | Payload assertion behavior |
| Go components | Repository Go toolchain, isolated real Redis for store tests | Publisher, storage, retry and safety logic |
| SDK lifecycle | Dedicated running CubeSandbox, READY template, receiver | Real operations and notifications |
| Deployment faults | Linux, systemd, Docker, sudo, isolated services/data | Selected failure/recovery boundaries |
| Host reboot | Expendable host, explicit operator approval and recovery console | Optional normal reboot witness only |

No test runner provisions a complete platform automatically. Reuse
[one-click deployment](../../../deploy/one-click/README.md) or your existing
isolated deployment procedure. The fault-control suites specifically require
systemd/Docker control on the test runner host; they do not control arbitrary
remote Kubernetes deployments. A remote receiver is usable for basic lifecycle
tests if its evidence log is accessible to the runner.

## 2. Python and quick local checks

Run from the repository root; retain its absolute path if changing directories:

```bash
REPO=$(pwd)
python3 -m venv /tmp/webhook-test-venv
source /tmp/webhook-test-venv/bin/activate
python -m pip install -e "$REPO/sdk/python"
python -m pip install -r "$REPO/tests/e2e/sdk_compat/requirements.txt" PyYAML boto3
python -m unittest discover -s tests/e2e/webhook -p 'test_*.py' -v
(cd tests/e2e/sdk_compat && python -m pytest --confcutdir=. cases/framework/test_webhook.py -v)
```

These checks do not restart services or count as deployed-platform acceptance.
Use the Go version declared by the repository. Run component tests from
`CubeMaster`, following [STORE.md](../../../CubeMaster/pkg/webhook/STORE.md)
for the isolated Redis variables. For gomonkey suites, include
`-gcflags=github.com/tencentcloud/CubeSandbox/CubeMaster/...=-l` and `-count=1`;
repeat with `-race`. A skipped real-Redis test is not acceptance.

## 3. Inventory your dedicated deployment

Copy [env.example](env.example) outside Git, chmod it to 600, replace its values,
and source it in the runner shell. The example uses illustrative ports and
paths; **no `/data/cubelet`, home directory, date or historical binary name is
required**. Do not source an unreviewed environment file.

- `WEBHOOK_P6_RUN_ROOT`: existing absolute directory reserved for this run, on
  persistent storage for recovery tests. Use a fresh directory. Configs,
  binaries, receiver files, proxy sockets/modes and evidence must resolve inside
  it, including through symlinks. Do not point it at an existing installation,
  shared data root or another run. Spaces are best avoided in systemd setup.
- `WEBHOOK_P6_RESOURCE_LABEL`: unique `webhook-p6-...` Docker `purpose` label.
  Apply it at container creation, e.g. `--label purpose="$WEBHOOK_P6_RESOURCE_LABEL"`.
  Every controlled container must carry the exact label, not merely its prefix.
- Controlled containers and systemd units must have a `webhook-1903-` name prefix;
  units end in `.service`. These are reserved test names, not fixed names from
  the author's installation. Unit `ExecStart` binaries must be inside RUN_ROOT.
- Put dedicated Master/Cubelet binaries inside RUN_ROOT; set `MASTER_BINARY` and
  `NODE_BINARY` to their actual absolute paths. PID checks compare the actual
  executable, not a suffix such as `cubemaster-shutdown-fixed`.
- Keep business Redis and webhook Redis separate. Use Redis containers exposing
  internal port 6379 and publish chosen loopback ports. Inspect actual AOF,
  appendfsync, save, maxmemory/noeviction, volumes and free space. Platform
  evidence uses `WEBHOOK_P6_REDIS_DB` (default 6); OOM filler keys use DB 15 on
  the dedicated notification service. Never use shared Redis for fault tests.
- Use a dedicated MySQL container with the run label and loopback publication.
  The cloned schema must match `whp6_[0-9_]+`, with user `p6_1903`; these narrow
  database ownership guards are intentional. Configure its actual address and
  credential in the dedicated Master's YAML. Database fault tests read that
  container's existing root credential for scoped trigger DDL only.

Create only your new run directories, for example after editing the environment:

```bash
source /absolute/path/to/your-reviewed-webhook.env
mkdir -p "$WEBHOOK_P6_RUN_ROOT"/{bin,master,evidence,receiver}
(cd "$REPO/CubeMaster" && go build -o "$WEBHOOK_P6_MASTER_BINARY" ./cmd/cubemaster)
```

Provision the remaining services from the deployment guide. The Master unit must
set `CUBE_MASTER_CONFIG_PATH` to `WEBHOOK_P6_MASTER_CONFIG`, and execute the
inventoried binary directly. Configure each service's real database/network and
mount dependencies; this test suite does not create units or modify the original
installation. Verify health before enabling fault control.

Merge the feature guide's webhook YAML into that Master config:

- Global/subscription enabled, all six exact event names.
- Subscription ID and URL match `SUBSCRIPTION_ID` and `RECEIVER_URL`.
- Complete `store.redis` override points to notification Redis and the selected DB.
- Allowlist the receiver CIDR, with `allow_http: true` for private HTTP tests.
- Restart all dedicated Masters. Use real XFS/S3 READY templates, not fabricated
  metadata or `mock_debug` / `mock_create_direct` paths.

## 4. Receiver and real lifecycle tests

In one shell with the reviewed environment loaded:

```bash
printf '204\n' > "$WEBHOOK_P6_RECEIVER_MODE"
python "$REPO/tests/e2e/webhook/receiver.py" --host 127.0.0.1 --port 18096 \
  --output "$WEBHOOK_P6_RECEIVER_LOG" --mode-file "$WEBHOOK_P6_RECEIVER_MODE" \
  --limit 2000 --fsync
```

Replace `18096` with the port in your `RECEIVER_URL`; for remote Masters use a
reachable restricted interface and the corresponding CIDR. Logs use exclusive
creation: every receiver restart needs a new log path. Update the runner's log
variable as well. Default concurrency is one; `--workers 1..16` is bounded.
The receiver limits request size (256 KiB), reads (5s) and record count. Evidence
contains actual payloads: use synthetic metadata and keep files private.

In another shell using the same environment:

```bash
cd "$REPO/tests/e2e/sdk_compat"
python -m pytest --run-e2e --confcutdir=. cases/lifecycle/test_webhook.py -vv
```

Use separate XFS and S3 templates and fresh evidence paths for each run.
`retry-once` receiver mode plus `WEBHOOK_P6_EXPECT_RETRY=1` requires actual retries.
Absent opt-ins skip; `--collect-only` verifies discovery, not execution.

## 5. Select fault experiments explicitly

Enable `WEBHOOK_P6_CONTROL=1` only after inventory and normal lifecycle success.
All names below have the `WEBHOOK_P6_` prefix. Set only applicable groups from
`env.example`. Run destructive scenarios serially, never with pytest-xdist.

| Suite / selection | Additional configuration |
| --- | --- |
| `test_webhook_recovery.py` basic configuration cases | Master unit/config/URL/binary, notification Redis, receiver paths/ID, RUN_ROOT and label |
| `-k deployment_api` | `PERFORMANCE=1`, `MASTER_DEBUG_URL`, concurrent receiver URL/mode/log |
| `-k two_complete` / mixed-Master completion test | Second Master unit/config/URL; same queue and config; concurrent receiver |
| Redis restart / HTTP failures / OOM | Respectively `REDIS_RESTART=1`, `HTTP_FAILURES=1`, `OOM=1` |
| `test_webhook_boundaries.py` | `NODE_FAULTS=1`, node unit/binary; `NODE_XFS_ROOT` with real snapshots/pause-snapshots directories; egress container; LCM unit for manager timeout |
| `test_webhook_completion_boundaries.py` | `DATABASE_FAULTS=1` and dedicated MySQL for write failures; business RESP proxy for Redis cleanup faults; `NODE_CLI`/`NODE_SOCKET` for full-ID node observations; CubeProxy container for proxy faults |
| `test_webhook_faults.py` | `REPLY_FAULTS=1`, notification RESP proxy directory, Master configured to proxy `ready` address |
| Sentinel recovery | `SENTINEL_PLATFORM=1`, current primary/replica/Sentinel containers, published Sentinel endpoints, monitor name and both passwords |
| `test_webhook_storage.py` / `test_webhook_s3_boundaries.py` | `S3_STATUS=1`, MinIO container, storage evidence directory, RPC/HTTP proxy controls and S3 HTTP URL/bucket |
| Cross-node negative test | `CROSS_UNREADY=1`, independent node B container/target ID, Ops URL, node A unit/binary and reachable host/port |
| Cross-node positive restore | Saved `SNAPSHOT_MANIFEST`, `RESTORE_TARGET`, fresh `RESTORE_EVIDENCE`; preserve a real snapshot with `KEEP_SNAPSHOT=1` |
| `test_webhook_tls.py` | `TLS_CA`, trusted/untrusted/wrong-host HTTPS URLs; real CA/certificates, `SSL_CERT_FILE` scoped to dedicated Master |

Example selections, from `tests/e2e/sdk_compat`:

```bash
WEBHOOK_P6_CONTROL=1 python -m pytest --run-e2e --confcutdir=. \
  cases/lifecycle/test_webhook_recovery.py -k 'global_off or revoked_saved' -vv
WEBHOOK_P6_CONTROL=1 WEBHOOK_P6_PERFORMANCE=1 python -m pytest --run-e2e --confcutdir=. \
  cases/lifecycle/test_webhook_recovery.py -k deployment_api -vv
```

For concurrent delivery, start a second receiver with its own output/mode, the
configured port, `--workers 4 --slow-seconds 30`. The slow delay must exceed the
configured lease to exercise HTTP overlap. For multiple Masters, align every
subscription and its receiver; mixed-config behavior is tested separately.
Performance observations are serial SDK timeout calls with warmups, not a fixed
SLO or proof of zero overhead. Do not combine performance, OOM and Sentinel runs.

### Redis proxy and Sentinel

In a separate shell, from `CubeMaster`, set `PROXY_DIR` to a new directory,
`PROXY_UPSTREAM` to your Redis loopback publication, and `PROXY_REDIS_CONTAINER`
to that exact container. The proxy verifies the `purpose` label and that Docker's
published 6379 port matches the configured upstream; arbitrary addresses fail.

```bash
go test -gcflags=github.com/tencentcloud/CubeSandbox/CubeMaster/...=-l \
  ./pkg/webhook -run '^TestP6PlatformRedisProxy$' -count=1 -v
```

Point only the dedicated Master's notification Redis at the emitted `ready`
address. For business HSET/XADD faults start a separate proxy process with a
separate `PROXY_DIR`, business Redis upstream/container, and export that directory
as `BUSINESS_PROXY_DIR` in the SDK runner. Stop each fixture by creating its own
`stop` file before its 30-minute deadline. This serial proxy is for precise faults;
bypass it for performance measurements. It forwards authentication from clients.

For the component Sentinel election use `SENTINEL_PRIMARY_CONTAINER`,
`SENTINEL_MASTER_NAME`, `SENTINEL_NODES`, `SENTINEL_PASSWORD`, `RESOURCE_LABEL` and
`WEBHOOK_REDIS_PASSWORD`, then select `TestP6RealSentinelElection`. It uses DB 9
with unique test keys. The platform suite uses `SENTINEL_CURRENT_PRIMARY` and
`SENTINEL_CURRENT_REPLICA`; re-inventory roles after each election. Internal
Redis/Sentinel ports are 6379/26379; published ports are your choice.
Same-volume component restart uses `WEBHOOK_REDIS_RESTART_CONTAINER` with its
existing `webhook-1903-p2-` prefix and `WEBHOOK_REDIS_RESTART_MODE=normal|kill`.
These tests never discover/restart a business Redis automatically.

### S3 and HTTPS fixtures

Provision real S3 storage using existing CubeS3lvol build/export interfaces.
Use your own dedicated WALs, lvstores, endpoints and volumes. For RPC injection,
stop the dedicated node safely, point its RPC config to your proxy socket and
restart it. All four paths must resolve within RUN_ROOT:

```bash
python "$REPO/tests/e2e/webhook/storage_proxy.py" \
  --upstream "$WEBHOOK_P6_RUN_ROOT/rcow/s3lvol.sock" \
  --listen "$WEBHOOK_P6_RUN_ROOT/proxy-s3/rpc.sock" \
  --control "$WEBHOOK_P6_STORAGE_PROXY_MODE" \
  --evidence "$WEBHOOK_P6_RUN_ROOT/proxy-s3/faults.jsonl"
```

Create the parent directories and initialize the mode to `forward`. RPC modes
are `forward`, `snapshot-error`, `error` and `empty`; no mode fabricates success.
Copy [s3_http_proxy.conf](s3_http_proxy.conf) into your run directory and edit its
`listen` and `proxy_pass` to your chosen endpoints. The default port values in
that file are examples. `/evidence` is a **container mount point**: bind your own
HTTP proxy evidence directory there and initialize its `mode` to `forward`.
Use the existing OpenResty image, restrict access, and point only the test node's
S3 endpoint at it. Host/SigV4 is preserved. Modes are `deny-manifest`,
`deny-exports` and `hold-manifest`; the hold expires after 120s. Node B needs its
own data/mounts and reachable object storage; do not copy A's snapshots.

For TLS, provision three actual endpoints forwarding to the receiver: trusted CA
with matching SAN, untrusted CA, trusted CA with wrong SAN. Use your own ports
and temporary certificates, set the three `TLS_*_URL` values and `TLS_CA`, and
scope `SSL_CERT_FILE` to the dedicated Master. Never install the CA globally or
disable certificate checks. There is no dependency on an unpublished TLS script.

## 6. Optional normal reboot witness

This is separate from ordinary acceptance and needs explicit operator approval,
an expendable host and console recovery. Keep the reviewed environment file and
RUN_ROOT persistent; configure the boot unit's `EnvironmentFile` with the same
settings (systemd environment files omit shell `export` and do not expand `$VAR`:
write resolved absolute values). Boot control URLs and receiver must be loopback.

`prepare_reboot.py RUN_ROOT` uses real SDK create/kill operations and saved tasks,
then kills only the inventoried Master. It writes private `reboot/seed.json` and
does not reboot or arm anything. Set Master/Ops units, business/notification Redis
containers, receiver paths/URL, subscription and SDK API/template beforehand.
Ensure the dedicated Master supervisor will not auto-restart after this stop.

Inventory mounts and services, stop own test nodes/VMs, restore original service
state yourself, and verify it. Copy the reviewed seed to `reboot/armed.json` only
after setting `original_cubelet_restored=true` from actual checks. Configure the
operator-owned boot oneshot with `RequiresMountsFor=RUN_ROOT`, database/network
ordering, `ConditionPathExists=.../reboot/armed.json`, a bounded timeout and:

```bash
python /absolute/checkout/tests/e2e/webhook/host_recovery.py /your/run/root --verify-plan
# Boot ExecStart uses the same command without --verify-plan.
```

`--verify-plan` is read-only. The witness requires a new boot ID, validates the
inventoried resources, reapplies loopback firewall restrictions to the configured
Master/Ops ports and starts only dedicated Redis/Ops/Master services. It never
starts or changes original installation services. It uses the configured receiver
port, saves a fresh log, checks byte/identity/budget recovery and disarms itself.
Provision/recover other dependencies through your own units. A normal reboot is
not hard-reset, physical power-loss or physical HA evidence.

## 7. Results and cleanup

Retain argv, source revision/diff, binaries, exit codes, JUnit, receiver records,
actual storage settings and before/fault/after state outside Git. Preserve failed
attempts and skip reasons. Never label a skipped opt-in as a pass or replace
real business operations with synthetic event injection.

OOM can partially execute Lua and stop scheduling. Restore capacity, audit saved
records and due/terminal indexes read-only, and restart only after consistency
supports it; do not clear queues or reset budgets. Storage-status inconsistencies
are separate diagnostics, not permission to change business/storage behavior.

Most tests restore their fault modes in finally blocks. After a failed process
or host experiment, independently recheck service/container states, database
triggers, proxy modes, mounts and Sentinel replication. Stop/remove only exact
inventoried resources and firewall rules from this run. Preserve logs and data;
do not use FLUSHALL/FLUSHDB, global cleanup or an existing volume purge.
