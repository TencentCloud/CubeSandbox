# External HTTP Score Demo

Local mock sidecar for CubeMaster `external_http_score`. It speaks the current Score-phase HTTP protocol and returns a `scores` map in `[0, 100]`.

This directory is a **demo**, not a production scorer. Behavior is tested by `main_test.go` (`go test ./examples/external-http-score` from `CubeMaster`).

## What this proves

This example can show a short, local chain:

```text
mock metrics change -> /score returns different node scores -> ExternalHTTPScore can consume the JSON
```

It does **not** prove a live scheduling decision by itself. CubeMaster consumption still requires enabling `external_http_score` in scheduler config and running the Score path.

## What this is not

- **Not** a real Prometheus integration. There is no Prometheus server, scrape config, or PromQL.
- **Not** proof of a real multi-node CubeMaster/Cubelet deployment.
- **Not** proof of real sandbox create latency. `estimated_create_latency_ms` is an in-process mock number.
- **Not** a production performance result.
- **Not** a change to the `external_http_score` request/response fields.

Mock metric names (`cpu_utilization`, `memory_utilization`, `sandbox_count`, `estimated_create_latency_ms`) live only inside this demo process. They are **not** added to the CubeMaster protocol.

## Modes

| Request `mode` | Scoring input | Behavior |
|---|---|---|
| omitted, `default`, or any value other than `mock_metrics` | Protocol node fields already sent by CubeMaster (`cpu_util`, `mem_usage`, `quota_mem`, `real_time_create_num`, `create_concurrent_num`) | Original demo scorer. Unchanged. |
| `mock_metrics` | In-process Prometheus-like snapshots keyed by `node_id` | Lower CPU, memory, sandbox count, and estimated create latency produce a higher score. |

CubeMaster still POSTs the current protocol body either way. `mode` is the existing opaque string from `plugin_conf.external_http_score.mode`.

## Mock metrics fields

Stored at `GET/POST /mock-metrics`, not in the `/score` protocol:

| Field | Meaning in this demo | Score direction |
|---|---|---|
| `cpu_utilization` | Mock CPU percent, treated as 0-100 | Lower -> higher score |
| `memory_utilization` | Mock memory percent, treated as 0-100 | Lower -> higher score |
| `sandbox_count` | Mock sandbox count on the node | Lower -> higher score |
| `estimated_create_latency_ms` | Mock estimated create latency | Lower -> higher score |

Default snapshots (deterministic demo data, not live cluster readings):

```json
{
  "node-a": {
    "cpu_utilization": 70,
    "memory_utilization": 60,
    "sandbox_count": 20,
    "estimated_create_latency_ms": 200
  },
  "node-b": {
    "cpu_utilization": 20,
    "memory_utilization": 30,
    "sandbox_count": 5,
    "estimated_create_latency_ms": 50
  }
}
```

If `/score` sees a `node_id` with no mock snapshot, the demo uses a documented fallback (`cpu=50`, `mem=50`, `sandboxes=10`, `latency_ms=100`) and logs `fallback=true`. That fallback is still mock data.

## Score formula (`mode=mock_metrics`)

Each component is clamped to `[0, 100]`, then averaged with equal weight `0.25`, then rounded to two decimal places:

```text
cpu_score      = 100 - cpu_utilization
mem_score      = 100 - memory_utilization
sandbox_score  = 100 - sandbox_count * 100 / 40
latency_score  = 100 - estimated_create_latency_ms * 100 / 400
score          = round((cpu_score + mem_score + sandbox_score + latency_score) / 4, 2)
```

`40` and `400` are demo normalization caps only. They are not production quotas.

Default snapshots therefore score as:

- `node-a` = `42.50`
- `node-b` = `81.25`

Changing the mock snapshots changes those numbers. That is the intended demo.

## Fault injection arms (explicit)

```bash
curl -s -X POST http://127.0.0.1:18080/fault \
  -H "Content-Type: application/json" \
  -d '{"delay_ms":0,"http_status":0,"bad_scores":false}'
```

| Field | Effect |
|---|---|
| `delay_ms` | Sleep before responding (for delayed/timeout arms) |
| `http_status` | If `>=400`, return that status with no JSON body |
| `bad_scores` | Return scores only for `no-such-node` (missing candidates) |

Operator short path: [`VERIFY.md`](./VERIFY.md).

### Default / original demo

```bash
curl -s http://127.0.0.1:18080/score \
  -H "Content-Type: application/json" \
  -d "{\"nodes\":[{\"node_id\":\"node-a\",\"cpu_util\":30,\"mem_usage\":32768,\"quota_mem\":131072,\"real_time_create_num\":1,\"create_concurrent_num\":30},{\"node_id\":\"node-b\",\"cpu_util\":15,\"mem_usage\":16384,\"quota_mem\":131072,\"real_time_create_num\":0,\"create_concurrent_num\":30}]}"
```

PowerShell:

```powershell
Invoke-RestMethod -Method POST -Uri http://127.0.0.1:18080/score -ContentType application/json -Body '{"nodes":[{"node_id":"node-a","cpu_util":30,"mem_usage":32768,"quota_mem":131072,"real_time_create_num":1,"create_concurrent_num":30},{"node_id":"node-b","cpu_util":15,"mem_usage":16384,"quota_mem":131072,"real_time_create_num":0,"create_concurrent_num":30}]}'
```

The original demo still prefers lower request-field CPU, memory, and create pressure. It does not read `/mock-metrics`.

### Mock metrics mode

Inspect the in-process snapshots:

```bash
curl -s http://127.0.0.1:18080/mock-metrics
```

Score with the current protocol, selecting mock metrics via `mode`:

```bash
curl -s http://127.0.0.1:18080/score \
  -H "Content-Type: application/json" \
  -d "{\"mode\":\"mock_metrics\",\"instance_type\":\"cubebox\",\"template_id\":\"tpl-example\",\"nodes\":[{\"node_id\":\"node-a\",\"node_ip\":\"10.0.0.1\"},{\"node_id\":\"node-b\",\"node_ip\":\"10.0.0.2\"}]}"
```

Example response with the default snapshots:

```json
{
  "scores": {
    "node-a": 42.5,
    "node-b": 81.25
  }
}
```

Change the mock metrics, then score again:

```bash
curl -s http://127.0.0.1:18080/mock-metrics \
  -H "Content-Type: application/json" \
  -d "{\"nodes\":{\"node-a\":{\"cpu_utilization\":10,\"memory_utilization\":15,\"sandbox_count\":2,\"estimated_create_latency_ms\":40},\"node-b\":{\"cpu_utilization\":80,\"memory_utilization\":75,\"sandbox_count\":24,\"estimated_create_latency_ms\":240}}}"

curl -s http://127.0.0.1:18080/score \
  -H "Content-Type: application/json" \
  -d "{\"mode\":\"mock_metrics\",\"nodes\":[{\"node_id\":\"node-a\"},{\"node_id\":\"node-b\"}]}"
```

After that patch, scores flip toward `node-a` (idle mock snapshot) and away from `node-b` (loaded mock snapshot). Example:

```json
{
  "scores": {
    "node-a": 90,
    "node-b": 31.25
  }
}
```

Reset the default snapshots:

```bash
curl -s -X POST http://127.0.0.1:18080/mock-metrics/reset
```

PowerShell equivalents:

```powershell
Invoke-RestMethod -Uri http://127.0.0.1:18080/mock-metrics
Invoke-RestMethod -Method POST -Uri http://127.0.0.1:18080/score -ContentType application/json -Body '{"mode":"mock_metrics","nodes":[{"node_id":"node-a"},{"node_id":"node-b"}]}'
Invoke-RestMethod -Method POST -Uri http://127.0.0.1:18080/mock-metrics -ContentType application/json -Body '{"nodes":{"node-a":{"cpu_utilization":10,"memory_utilization":15,"sandbox_count":2,"estimated_create_latency_ms":40},"node-b":{"cpu_utilization":80,"memory_utilization":75,"sandbox_count":24,"estimated_create_latency_ms":240}}}'
Invoke-RestMethod -Method POST -Uri http://127.0.0.1:18080/score -ContentType application/json -Body '{"mode":"mock_metrics","nodes":[{"node_id":"node-a"},{"node_id":"node-b"}]}'
Invoke-RestMethod -Method POST -Uri http://127.0.0.1:18080/mock-metrics/reset
```

## Use with `external_http_score`

Point CubeMaster at this process with the existing plugin config. Keep `plugin_conf.external_http_score` on `scheduler.score.plugin_conf`. Do not put `plugin_conf` under `scheduler.profiles.<name>.score`.

Default/demo mode (original behavior):

```yaml
scheduler:
  score:
    enable_scorers:
      - external_http_score
    resource_weights:
      external_http_score: 1
    plugin_conf:
      external_http_score:
        weight: 1
        endpoint: "http://127.0.0.1:18080/score"
        timeout: 200ms
        mode: default
        disable: false
```

Mock metrics mode:

```yaml
scheduler:
  score:
    enable_scorers:
      - external_http_score
    resource_weights:
      external_http_score: 1
    plugin_conf:
      external_http_score:
        weight: 1
        endpoint: "http://127.0.0.1:18080/score"
        timeout: 200ms
        mode: mock_metrics
        disable: false
```

A user-defined runtime profile may list `external_http_score` in `enable_scorers` / `resource_weights`, but the endpoint and `mode` still stay on `scheduler.score.plugin_conf.external_http_score`. Protocol details: `docs/dev/external-http-score.md`. Runtime profile YAML: `docs/dev/scheduler-profile-config-example.md`.

This config wiring is how CubeMaster would consume the demo. Running `go run ./examples/external-http-score` plus `curl` only proves the sidecar. It does not start CubeMaster, CubeAPI, or Cubelet.

## Evidence gap

Still not shown by this demo:

- Real Prometheus scrape or PromQL.
- Real multi-node / multi-VM CubeMaster deployment.
- CubeAPI / Cubelet create path.
- Real sandbox create latency or MicroVM ready latency.
- Production performance improvement.
- Live placement distribution after a real Schedule() call.

In-process scheduler tests already show that an external `scores` map can change candidate order. This example only adds a local, mutable mock-metrics input in front of that same protocol.
