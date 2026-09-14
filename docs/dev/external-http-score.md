# External HTTP Score Plugin

`external_http_score` lets CubeMaster call an external HTTP service during the scheduler Score phase. The service returns a 0-100 score for every candidate node, and CubeMaster feeds those scores into the existing weighted score pipeline.

The plugin is disabled unless it is listed in `scheduler.score.enable_scorers`. Enabling `enable_scorers: external_http_score` **requires** a matching `score.plugin_conf.external_http_score` block; otherwise CubeMaster panics while constructing scorers at startup. It does not run as a Filter, does not remove candidates, and does not directly choose the final node.

Operator-facing configuration also lives in [CubeMaster scheduler config](../guide/cubemaster-scheduler-config.md#external-http-score-plugin) (English) / [中文](../zh/guide/cubemaster-scheduler-config.md#external-http-score-插件).

## Configuration

```yaml
scheduler:
  score:
    enable_scorers:
      - external_http_score
    resource_weights:
      mvm_num: 1
    plugin_conf:
      external_http_score:
        weight: 1.0
        endpoint: "http://127.0.0.1:18080/score"
        timeout: 200ms
        mode: ""
        disable: false
        failure_policy: fail_open   # default when omitted
        circuit_breaker:
          disable: false
          failure_threshold: 5
          open_duration: 5s
          half_open_max_probes: 1
```

Fields:

- `weight`: relative weight in `runScoreFilter`. Omitted defaults to `1.0`; explicit `0` is a staged inert no-op.
- `endpoint`: absolute `http://` / `https://` sidecar URL. Empty with a positive weight fail-opens observably.
- `timeout`: per-request timeout on the synchronous create path (default `200ms`, max `2s`).
- `mode`: opaque string forwarded to the sidecar.
- `disable`: when true, the scorer is a no-op.
- `failure_policy`: sidecar failure handling.
  - **`fail_open` (default**, including empty/unknown values): `Select` returns a plain error; `runScoreFilter` skips this scorer and continues scheduling (historical create-path behavior).
  - **`fail_closed`**: `Select` returns a typed `FailClosedError`; `runScoreFilter` aborts the Score phase so create fails closed.
- `circuit_breaker`: consecutive sidecar failures open the circuit so later Score calls fail immediately instead of waiting for the full HTTP timeout. After `open_duration`, half-open probes are allowed (`half_open_max_probes`, default 1); success closes the circuit, failure reopens it. Set `disable: true` to turn the breaker off.

Default circuit breaker values when the block is omitted or a field is zero (breaker still enabled unless `disable: true`):

- `failure_threshold`: 5
- `open_duration`: 5s
- `half_open_max_probes`: 1

## Production reliability

Sidecar outages should not pin every scheduling attempt to the HTTP timeout. After `failure_threshold` consecutive request or response failures, the circuit opens and `Select` returns immediately (`fail_closed` typed error, or plain fail-open error). Open-circuit rejects do not perform HTTP and map to outcome reason `circuit_open`.

Prometheus metrics (CubeMaster process, `#1700` naming):

| Metric | Type | Meaning |
|---|---|---|
| `cube_scheduler_external_http_score_outcomes_total{reason=...}` | counter | Outcomes by fixed reason (`success`, `timeout`, `connection`, `http_status`, `invalid_json`, `missing_candidate`, `circuit_open`, `other`) |
| `cube_scheduler_external_http_score_request_duration_seconds{reason=...}` | histogram | HTTP round-trip latency by outcome reason (open-circuit rejects are not sampled) |
| `cube_scheduler_external_http_score_circuit_state{target=...}` | gauge | `0=closed`, `1=half-open`, `2=open`; `target` is host:port only |

Implementation: `CubeMaster/pkg/selector/score/externalhttpscore.go`, `externalhttpscore_breaker.go`, `externalhttpscore_metrics.go`, `failclosed.go`.

## Request / response

See the wire contract in the scheduler config guide. Every requested candidate must receive a finite score in `[0, 100]`; extra keys are ignored; partial candidate sets fail the whole attempt.
