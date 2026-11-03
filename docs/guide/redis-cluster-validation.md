# Redis Cluster validation report

Related review: [PR #2122 validation checklist](https://github.com/TencentCloud/CubeSandbox/pull/2122#issuecomment-6594681723).

This report and its recorded evidence remain English-only by design to keep a single reference for commands and outcomes. The deployment and migration guide is maintained in both [English](./redis-cluster.md) and [Chinese](../zh/guide/redis-cluster.md).

The checks below ran against an authenticated, disposable Redis Cluster. The starting revision was `6df45c9bc84032f2f61815861711bbfe9ef8c2b1`. Validation found a primary-scan defect; the corrected Ops binary was deployed and the affected integration tests and complete sandbox lifecycle were repeated successfully.

## Fixture and deployment

- Redis 6.2.23; three hosts, three primaries and three cross-host replicas; password authentication enabled; DB 0.
- Separate Redis data directories and bounded instances: 1 GiB Redis memory limit, 1536 MiB process memory limit, and one CPU per instance.
- An isolated Kubernetes namespace and temporary MySQL database for Master, Ops, TemplateCenter, Lifecycle Manager and Proxy. Existing control-plane and database services were preserved.
- One idle worker and one sandbox at a time, using an S3-backed template with 2 CPUs and 2000 MiB of memory.
- Go 1.25.7, `CGO_ENABLED=0`, bounded build concurrency. Candidate binaries were mounted into pinned runtime images; Proxy used the candidate Lua sources. No new release images were published.
- Running Go executable hashes and Proxy source hashes were checked against the deployed artifacts. After the scan correction, the Ops executable SHA-256 was `f6c198b9aaab11d4eb93588e8c25a7f0cef3396a9b5ac731d760e5cac27539b8`.

Host labels below replace private addresses. Initial and final slot ownership matched:

| Primary | Slots | Replica |
| --- | --- | --- |
| redis-a:6380 | 0–5460 | redis-b:6381 |
| redis-b:6380 | 5461–10922 | redis-c:6381 |
| redis-c:6380 | 10923–16383 | redis-a:6381 |

All 16,384 slots were healthy. Six endpoint checks from each of the actual Master, Ops, Lifecycle Manager and Proxy Pod network namespaces passed: [24/24 connections](./redis-cluster-validation/endpoint-reachability.json). The existing Cubelet was not reconfigured; its actual registration/status snapshots were relayed to the isolated Ops instance for the selected worker during the sandbox checks.

## Results

| Requirement | Result |
| --- | --- |
| Real Go Cluster commands with fixture endpoints configured | PASS; selected real integration cases executed without skips |
| Primary-promotion case with `TEST_REDIS_CLUSTER_FAILOVER=1` | PASS; first seed was a primary with a replica |
| Master wrapper SET/NX and Lua operations | PASS |
| Ops snapshot scan across multiple primary owners | PASS after the primary-scan correction |
| Lifecycle cross-slot reads/writes, result ordering and missing keys | PASS |
| Cluster Helm render with DB 0 and no conflicting mode | PASS |
| Nonzero DB, mixed Cluster/standalone/Sentinel and malformed endpoint rejection | PASS at render time |
| Standalone/Sentinel render compatibility | PASS; Cluster endpoint variables remained empty |
| Sandbox create, route, pause, resume, route again, delete | PASS with the corrected Ops binary deployed |
| Primary promotion during 100 INCR operations | PASS; 100 successful calls, final counter 100, no observed errors |
| Response lost after Redis applied a write | PASS; EOF returned, exactly one applied write |
| Response withheld past the read deadline | PASS; timeout returned after approximately 5 seconds, exactly one applied write |
| Native READONLY, Go topology refresh and one successful retry | PASS using the non-mutating diagnostic described below |
| Native READONLY, Proxy cache invalidation and one successful retry | PASS in actual OpenResty with real Redis sockets |
| Repeated READONLY rejection and Proxy MOVED/ASK cases | Existing bounded-retry and Lua routing suites passed; these remain mock coverage |
| Sandbox cleanup | PASS; both sequential smoke sandboxes were deleted and final engine inventory was empty |

## Evidence index

| Review item | Commands and evidence |
| --- | --- |
| 1. Real Go integration and primary promotion | [Recorded commands, RUN/PASS lines and package results](./redis-cluster-validation/go-integration.txt); no selected real case was skipped |
| 2. Cluster Helm render and rejected conflict | [Values, exact render commands, exit codes, configuration excerpts and rejection messages](./redis-cluster-validation/helm-render.json) |
| 3. Sandbox lifecycle and cross-primary operations | [Six-step request and response record](./redis-cluster-validation/sandbox-lifecycle.json); Ops and Lifecycle observations in the Go transcript |
| 4. Traffic during promotion, READONLY and ambiguous transport | Separate live scenarios and full traces in the Go transcript, including the actual OpenResty result |
| 5. Invalid configuration and legacy modes | Four rejected configurations plus standalone/Sentinel environment values in the Helm record |
| Advertised endpoint reachability | [Checks from all four client Pod network namespaces](./redis-cluster-validation/endpoint-reachability.json) |

Go and sandbox records come from the completed live validation. The Master transcript keeps test-run and result lines and omits application startup logs. Sandbox request bodies were reconstructed from the executed drivers; responses were extracted from their retained records. Host addresses, identifiers, run labels, key suffixes, credentials and timestamps are redacted; outcomes and durations are preserved.

The original Helm guard retained only its final success message. A supplemental **offline** render with Helm v3.19.0 against revision `104654b79ad3bb35b30c82fbfdfe9eb95276c483` captured the detailed outputs below. It contacted neither Kubernetes nor Redis. This documentation update did not repeat the live tests.

## Primary-scan correction

In redisc v1.4.0, `EachNode(true, ...)` visits replicas. `ScanKeys` used that value despite its contract to scan primaries. The initial cross-slot test checked where keys belonged, but did not inspect the endpoints that received SCAN.

The new `TestLiveScanUsesPrimaries` observes actual connections and compares them with the current primary set. Before the fix it failed on all three replica addresses. Changing the call to `EachNode(false, ...)` made the same test pass on all three primaries:

```text
Before:
scan_address=redis-b:6381 current_primary=false
scan_address=redis-c:6381 current_primary=false
scan_address=redis-a:6381 current_primary=false
FAIL: TestLiveScanUsesPrimaries

After:
scan_address=redis-a:6380 current_primary=true
scan_address=redis-b:6380 current_primary=true
scan_address=redis-c:6380 current_primary=true
PASS: TestLiveScanUsesPrimaries
```

Ops found snapshots in slots 15495 and 3300, owned by redis-c and redis-a respectively. Lifecycle wrote `active` and `paused` values in those slots, requested them in reversed order with a missing key between them, and verified both order and nil preservation.

## Native READONLY recovery

Ordinary keyed writes to a demoted native Cluster primary returned MOVED. Redis performs key-based routing before its replica write-protection check. To reach the native READONLY guard without modifying data, the tests use:

```text
MIGRATE 127.0.0.1 1 "" 0 100 KEYS
```

This is a write-class command with an empty key list. A replica returns top-level READONLY; a primary returns NOKEY before opening a destination connection or changing data. The rejection comes from Redis itself. The connection observers preserve command arguments and server replies.

The Go test warms a route, promotes its replica, and records:

```text
redis-c:6380  MIGRATE        READONLY You can't write against a read only replica.
redis-b:6380  CLUSTER SLOTS  OK
redis-a:6381  MIGRATE        NOKEY
native_readonly=1 successful_retries=1 topology_refreshes=1 data_keys_touched=0
PASS: TestLiveReadOnlyRefreshAfterPromotion
```

The Proxy test first fills its slot cache through an actual MOVED reply. After promotion it receives READONLY from the cached former primary, deletes that cache entry, and retries once on the promoted node:

```text
native_readonly=1
successful_retries=1
stale_cache_invalidated=true
data_keys_touched=0
Native Redis Cluster READONLY routing test passed
```

Both tests restored the original primary/replica roles. This is a dedicated native rejection/recovery diagnostic, not a claim that an ordinary INCR returned READONLY. Actual writes were tested separately during promotion and response loss.

The diagnostic relies on Redis 6.2.23 behavior in [processCommand](https://github.com/redis/redis/blob/6.2.23/src/server.c#L3827) and [migrateCommand](https://github.com/redis/redis/blob/6.2.23/src/cluster.c#L4991).

## Writes during promotion and ambiguous responses

The live writer issued 100 INCR operations with a 10 ms interval and requested CLUSTER FAILOVER after the first ten. The final candidate produced:

```text
primary=redis-b:6381 promoted=redis-a:6380
slot=3585 direct_demoted_reply="MOVED 3585 redis-a:6380"
successful_writes=100 counter=100 errors=[]
```

The response-loss test forwards real Redis traffic through local TCP listeners. After Redis successfully executes INCR, the listener discards the reply and either closes the socket or holds it beyond the client's read deadline. Both cases returned an error to the caller and left counter 1:

```text
fault=disconnect error="EOF" applied_writes=1 counter=1
fault=timeout error="read timeout" elapsed=approximately_5_seconds applied_writes=1 counter=1
```

The timeout message above omits ephemeral local socket addresses. This tests lost responses after application, rather than connection failure before the write reaches Redis.

## Commands

Supply `TEST_REDIS_CLUSTER_NODES` and, when needed, `TEST_REDIS_CLUSTER_PASSWORD` from a private environment source. Use only a disposable fixture. Set the first seed to a current primary with a replica before running the promotion case.

```bash
export TEST_REDIS_CLUSTER_FAILOVER=1
export CGO_ENABLED=0

(cd pkgs/redisx && go test -p 4 -v -count=1 -timeout 90s \
  -run 'TestClusterCommands|TestLiveScanUsesPrimaries|TestServerErrorsAreReturnedAsErrors|TestClusterEndpoints|TestReadOnlyRefreshDoesNotReplayAmbiguousWrites|TestLiveAmbiguousTransportIsNotReplayed' .)
(cd CubeMaster && go test -p 4 -v -count=1 -timeout 90s \
  -run TestClusterWrapper ./pkg/base/wrapredis)
(cd CubeOps && go test -p 4 -v -count=1 -timeout 90s \
  -run TestClusterSnapshots ./internal/nodemanagement/nodemetric)
(cd cube-lifecycle-manager && go test -p 4 -v -count=1 -timeout 90s \
  -run TestClusterStateReads ./internal/redisstream)
bash deploy/kubernetes/chart/scripts/test-redis-cluster-guard.sh
```

Run each promotion scenario separately, checking roles and allowing election state to expire between runs. The primary-promotion case requires the first seed to be a current primary with a replica; it changes that shard's roles. The traffic exercise discovers the current primary and replica. The dedicated READONLY tests restore their starting roles.

```bash
(cd pkgs/redisx && go test -p 4 -v -count=1 -timeout 90s \
  -run '^TestClusterPrimaryPromotion$' .)
(cd pkgs/redisx && go test -p 4 -v -count=1 -timeout 90s \
  -run '^TestLiveWritesDuringPromotion$' .)
(cd pkgs/redisx && go test -p 4 -v -count=1 -timeout 90s \
  -run 'TestLiveReadOnlyRefreshAfterPromotion|TestLiveScanUsesPrimaries' .)
(cd CubeProxy && resty \
  --main-conf 'env TEST_REDIS_CLUSTER_PASSWORD; env TEST_REDIS_CLUSTER_NODES; env TEST_REDIS_CLUSTER_FAILOVER;' \
  --shdict 'redis_cluster_slots 1m' -I ./lua \
  tests/redis_cluster_live_test.lua)
```

The Go cases were executed in focused runs to capture regression and recovery evidence separately. Promotion changes the fixture topology: inspect current roles before repeating the fixed-seed test. Allow Redis's election state to expire between consecutive promotions; the native READONLY tests wait for that interval and role convergence before reversing their promotion.

The deployed sandbox check used POST `/cube/sandbox`, Proxy routing to `/sandbox/<id>/49983/process.Process/Start`, pause/resume via `/cube/sandbox/update`, and synchronous DELETE `/cube/sandbox`. Both routed commands returned their expected unique stdout markers and exit status 0. The final run passed without manual route-cache intervention.

## Recorded Go integration output

These excerpts are from separate completed invocations. Full test output, including the cross-primary addresses and transport-fault observations, is in the [integration transcript](./redis-cluster-validation/go-integration.txt).

```text
=== RUN   TestClusterCommands
--- PASS: TestClusterCommands (0.00s)

=== RUN   TestClusterPrimaryPromotion
--- PASS: TestClusterPrimaryPromotion (0.06s)
PASS
ok  github.com/tencentcloud/CubeSandbox/pkgs/redisx  0.062s

=== RUN   TestClusterWrapper
--- PASS: TestClusterWrapper (0.00s)
PASS
ok  github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/wrapredis  0.008s

=== RUN   TestClusterSnapshots
--- PASS: TestClusterSnapshots (0.01s)
PASS
ok  github.com/tencentcloud/CubeSandbox/CubeOps/internal/nodemanagement/nodemetric  0.009s

=== RUN   TestClusterStateReads
--- PASS: TestClusterStateReads (0.01s)
PASS
ok  github.com/tencentcloud/CubeSandbox/cube-lifecycle-manager/internal/redisstream  0.007s
```

## Helm render details

The supplemental render used the following values; `render-only` is an arbitrary rendering fixture value, not a deployed credential. The seed names are aliases and are not contacted by `helm template`.

```bash
chart=deploy/kubernetes/chart
values_file=$(mktemp)
trap 'rm -f "$values_file"' EXIT
cat > "$values_file" <<'YAML'
mysql:
  password: render-only
  rootPassword: render-only
redis:
  password: render-only
  db: 0
  clusterNodes: "redis-a:6380,redis-b:6380,redis-c:6380"
YAML

helm template cluster-validation "$chart" -f "$values_file"

# Each invocation below must exit nonzero.
for setting in redis.db=1 redis.host=redis.example \
  redis.masterName=mymaster redis.clusterNodes=bad-endpoint; do
  if helm template rejected-validation "$chart" -f "$values_file" \
    --set-string "$setting"; then
    echo "Unexpectedly accepted $setting" >&2
    exit 1
  fi
done

helm template standalone-validation "$chart" -f "$values_file" \
  --set-string redis.clusterNodes=
helm template sentinel-validation "$chart" -f "$values_file" \
  --set-string redis.clusterNodes= --set-string redis.masterName=mymaster \
  --set-string redis.sentinelNodes=redis.example:26379
```

All seven render cases had the expected exit code. The [full record](./redis-cluster-validation/helm-render.json) includes the actual per-case commands, values supplied on stdin, and stderr, rather than only a summary from the guard script.

| Case | Exit code | Observed result |
| --- | --- | --- |
| Cluster, DB 0, no host/Sentinel | 0 | All four Cluster environment entries and Master/TemplateCenter configuration contain the seed list; bundled Redis absent |
| Nonzero DB | 1 | `redis.clusterNodes requires redis.db=0` |
| Cluster plus standalone host | 1 | `redis.clusterNodes cannot be combined with redis.host or Sentinel` |
| Cluster plus Sentinel master name | 1 | `redis.clusterNodes cannot be combined with redis.host or Sentinel` |
| Malformed endpoint | 1 | `redis.clusterNodes must contain host:port endpoints` |
| Standalone | 0 | All Cluster entries empty; bundled Redis present |
| Sentinel | 0 | All Cluster entries empty; bundled Redis absent |

The rendered Deployment values were:

| Component / variable | Cluster | Standalone | Sentinel |
| --- | --- | --- | --- |
| Ops / `REDIS_CLUSTER_NODES` | Three seed endpoints | `""` | `""` |
| Lifecycle / `CUBE_LCM_REDIS_CLUSTER_NODES` | Three seed endpoints | `""` | `""` |
| Proxy / `REDIS_CLUSTER_NODES` | Three seed endpoints | `""` | `""` |
| Proxy / `CUBE_PROXY_REGISTRY_REDIS_CLUSTER_NODES` | Three seed endpoints | `""` | `""` |

Master and TemplateCenter receive `cluster_nodes` through configuration rather than these environment variables. Their rendered configuration lines were also checked in all three modes. The original live-validation guard output was `Redis Cluster chart guards passed`.

## Sandbox requests and responses

The [six-step record](./redis-cluster-validation/sandbox-lifecycle.json) contains each method, path, request body and response excerpt from the final run with the corrected Ops binary. Placeholders consistently replace the same sandbox, worker and template. The two route requests use Connect streaming framing; sending their JSON directly with an ordinary JSON content type would not reproduce the request.

| Step | Method and path | Recorded response |
| --- | --- | --- |
| Create | `POST /cube/sandbox` | `ret_code=200`; sandbox ID and worker returned |
| Route after create | `POST /sandbox/<sandbox-id>/49983/process.Process/Start` | HTTP 200; expected stdout marker; `exit status 0` |
| Pause | `POST /cube/sandbox/update`, `action=pause` | `{"ret":{"ret_code":200,"ret_msg":""}}` |
| Resume | `POST /cube/sandbox/update`, `action=resume` | `{"ret":{"ret_code":200,"ret_msg":""}}` |
| Route after resume | Same Proxy process endpoint | HTTP 200; second expected marker; `exit status 0` |
| Delete | `DELETE /cube/sandbox`, `sync=true` | `ret_code=200`, `ret_msg=Success`; matching sandbox ID |

The following reproduces the request sequence. Set `MASTER_URL`, `PROXY_URL`, `TEMPLATE_ID` and `TEMPLATE_VERSION` for the disposable deployment first. Use the same prepared worker/template prerequisites described above. The code saves the returned sandbox ID before issuing subsequent requests, prints the relevant response fields, and checks the process-end event and decoded stdout. It is a parameterized form of the recorded drivers, not a new live run.

```bash
python3 - <<'PY'
import base64
import json
import os
import struct
import urllib.request
import uuid

master = os.environ["MASTER_URL"].rstrip("/")
proxy = os.environ["PROXY_URL"].rstrip("/")

def call(method, path, body):
    request = urllib.request.Request(master + path, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"}, method=method)
    with urllib.request.urlopen(request, timeout=100) as response:
        result = json.load(response)
    assert result["ret"]["ret_code"] == 200, result["ret"]
    return result

created = call("POST", "/cube/sandbox", {
    "requestID": str(uuid.uuid4()), "instance_type": "cubebox", "timeout": 600,
    "auto_resume": True, "auto_pause": True, "backend": "s3",
    "annotations": {
        "cube.master.appsnapshot.template.id": os.environ["TEMPLATE_ID"],
        "cube.master.appsnapshot.template.version": os.environ["TEMPLATE_VERSION"],
    },
    "labels": {"validation": "redis-cluster-smoke"},
})
sid, worker = created["sandbox_id"], created["host_ip"]
print(json.dumps({"step": "create", "ret": created["ret"], "sandbox_id": sid}))

def route(label):
    marker = "redis-cluster-" + label + "-" + sid[:8]
    payload = json.dumps({"process": {"cmd": "/bin/sh",
        "args": ["-c", "printf " + marker], "envs": {}, "cwd": "/tmp"},
        "stdin": False}).encode()
    framed = b"\x00" + struct.pack(">I", len(payload)) + payload
    request = urllib.request.Request(
        proxy + "/sandbox/" + sid + "/49983/process.Process/Start", data=framed,
        headers={"Content-Type": "application/connect+json",
            "Connect-Protocol-Version": "1", "Connect-Timeout-Ms": "15000",
            "X-User": "root"}, method="POST")
    with urllib.request.urlopen(request, timeout=20) as response:
        raw, status = response.read(), response.status
    pos, stdout, end = 0, "", None
    while pos < len(raw):
        assert pos + 5 <= len(raw)
        size = struct.unpack(">I", raw[pos + 1:pos + 5])[0]
        assert pos + 5 + size <= len(raw)
        item = json.loads(raw[pos + 5:pos + 5 + size])
        pos += 5 + size
        assert "error" not in item, item
        event = item.get("event", {})
        stdout += base64.b64decode(event.get("data", {}).get("stdout", "")).decode()
        if "end" in event:
            end = event["end"]
    assert status == 200 and stdout == marker
    assert end and end.get("exited") and end.get("status") == "exit status 0", end
    print(json.dumps({"step": label, "http_status": status, "stdout": stdout,
        "process_end": end}))

route("route-after-create")
for action in ("pause", "resume"):
    response = call("POST", "/cube/sandbox/update", {
        "requestID": str(uuid.uuid4()), "sandbox_id": sid,
        "instance_type": "cubebox", "action": action,
    })
    print(json.dumps({"step": action, "response": response}))
route("route-after-resume")
deleted = call("DELETE", "/cube/sandbox", {
    "requestID": str(uuid.uuid4()), "sandbox_id": sid,
    "instance_type": "cubebox", "host_ip": worker, "sync": True,
})
print(json.dumps({"step": "delete", "response": deleted}))
PY
```

## Corrections and retained failures

- The original promotion fixture assumed PING had initialized a particular seed pool. PING may select another primary; the test now accesses a key owned by the seed before checking that pool.
- The real integration cases now accept a password through the environment so the fixture can remain authenticated.
- Three early native-Go runs passed their READONLY/refresh/retry assertions but failed immediate topology restoration. Logs showed incomplete role propagation and reuse of previous election state during rapid reversals. The fixture now waits for convergence and the election-state interval. Client retry counts and timeouts were not relaxed. The complete Go and OpenResty drills subsequently passed, including restoration.
- The first sandbox run exposed inconsistent admin tokens in the temporary deployment. The candidate configuration was aligned; the final lifecycle passed without manual cache clearing. Existing service authentication was not disabled.
- Private addresses, sandbox identifiers and credentials are omitted from this report. The excerpts retain command outcomes and node-role relationships; the original failed and successful runs remain in the local evidence bundle.

## Limits and final state

Cross-slot reads are not a globally atomic snapshot. Independent-key deletion may partially complete. Every advertised slot, MOVED and ASK target must be reachable from clients. The observed 100-write result does not establish an exactly-once guarantee or eliminate asynchronous-replication loss under network partitions.

TLS, ACL usernames, webhook Cluster support in #2080, performance benchmarking, and live ASK resharding were not part of this run. ASK routing has mock coverage. Standalone/Sentinel compatibility here was checked through chart rendering; no standalone/Sentinel runtime failover result is claimed.

Both smoke sandboxes were removed. The temporary worker relay was stopped, the five candidate Deployments were scaled to zero, and their Pods were removed. The temporary MySQL Pod and six Redis instances were retained for inspection. All slots were healthy and the original Redis topology was restored. Existing Redis/Sentinel services stayed active; the MySQL PID on each fixture host was unchanged.
