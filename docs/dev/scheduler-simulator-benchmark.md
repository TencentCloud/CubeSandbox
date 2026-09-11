# Scheduler Simulator Benchmark

This document describes the minimal offline benchmark for evaluating CubeMaster
scheduler profiles without requiring a live multi-node cluster.

## Scope

The simulator is intentionally small. It follows a filter → score → bind
shape with simulator-local rules:

1. reject nodes that cannot fit the request;
2. score feasible candidates;
3. place the sandbox on the highest-scored node;
4. record placement and scheduling-quality metrics.

It does not replace live `cubebox multirun` benchmarking and does not claim
production throughput or latency.

## One-Command Run

From `CubeMaster`:

```bash
go run ./cmd/schedulerbench --out ./schedulerbench-report
```

The command writes:

- `schedulerbench-report/report.json`
- `schedulerbench-report/report.md`

Use `--format json`, `--format markdown`, or `--format both` to select the
output file set. The default is `both`. `--out` is treated as a directory the
CLI owns: a format that is not selected deletes any pre-existing
`report.json` or `report.md` in that directory (`--format json` therefore
removes a stale `report.md`). Use a fresh directory if you need to keep both.

The default run is deterministic and records the seed, node count, workloads,
profiles, source provenance, and metric schema in both reports. `--nodes`
accepts only **1–4**. An explicit CLI `--nodes 0` is rejected, and `--seed 0`
is invalid because zero is reserved as the library unset sentinel.

Source provenance uses `vcs.revision` when present and `vcs.modified` when it
is exactly `true`/`false`. The bounded git fallback runs only when no revision
stamp is usable. A known revision with unknown dirty state is reported as
`git_dirty: null` (and that null participates in `run_id`). If no revision can
be found, the report records `git_revision: "unknown"` and `git_dirty: null`.

Use `--verify` to check the default report's structure and internal consistency
before writing it:

```bash
go run ./cmd/schedulerbench --verify --out ./schedulerbench-report
```

Verification requires the complete default workload/profile matrix, required
metric schema keys, internally consistent request and candidate counters,
valid provenance/run identity, and one internally consistent
default-vs-candidate comparison for every expected pair. Human-readable metric
descriptions and comparison notes may be reworded.

This check is simulator-only. It does not run a real multi-node deployment,
create resources through CubeAPI/Cubelet, or validate production performance
or real create latency.

## Workloads

- `burst_short_lived`: 80 small requests arriving in a short burst with short
  lifetimes.
- `same_template_repeated`: 48 repeated requests using the same template to
  expose template-locality behavior.
- `mixed_size`: 60 requests mixing small, medium, and large resource
  shapes to expose capacity and binpacking trade-offs.

## Profiles

These names are the simulator's own public vocabulary for offline scoring
weight presets. They do not imply runtime `scheduler.profile` overlays or
equivalence to production scheduler plugins.

- `default`: balanced resource-headroom and spread scoring with light template
  locality.
- `balanced_spread`: favors even placement across nodes.
- `template_locality_first`: favors nodes that already have the requested
  template.
- `binpack_utilization`: favors tighter packing, with explicit balance and
  headroom trade-offs.

## Metrics

The report includes more than five scheduling-quality metrics:

- `schedule_success_rate`
- `rejected_requests`
- `node_load_balance`
- `template_locality_hit_rate`
- `cpu_quota_utilization`
- `mem_quota_utilization`
- `create_latency_p50_ms`: estimated create latency P50 in milliseconds. This is
  a simulator estimate, not CubeAPI/Cubelet create time. Empty success samples
  yield `0`.
- `create_latency_p95_ms`: estimated create latency P95 in milliseconds, with
  the same estimator and empty-sample rule as P50.
- `uses_estimated_latency`: always `true` in this simulator, so report readers
  do not treat P50/P95 as measured create latency.
- `peak_cpu_utilization`
- `average_cpu_headroom`
- `average_score_margin`: simulator-local decision-gap proxy under deterministic
  argmax bind: mean score gap between the selected node and the second-ranked
  candidate, averaged only over scheduled requests that had at least two scored
  candidates; `0` when no such observations exist. Not a production selection
  metric.
- `average_feasible_candidates`: feasible simulated nodes per scheduled request;
  at most the simulated node count. Simulator-local candidate breadth, not
  scheduler CPU cost or latency. This simulator binds the globally best scored
  feasible node. Production CubeMaster truncates to `scheduler.priority_select_num`
  (shipped config: `1`) and selects within that set using
  `scheduler.least_select_name` — uniform for the default `random`,
  score-weighted for `sw`/`rw`/`rrw`. With the shipped `priority_select_num: 1`
  production coincides with this argmax (modulo tie-break), so the report does
  not claim a retained-candidate metric for that production knob.

The Markdown results table includes `latency p50 ms` and `latency p95 ms`.
The estimator is deterministic:

```text
estimated_latency_ms =
    80
  + (template already on node ? 0 : 120)
  + (active_sandbox_count / max_sandboxes) * 80
  + ((cpu_pressure + mem_pressure) / 2) * 60
```

Only successfully scheduled requests contribute samples. P50/P95 are linear
interpolated percentiles over those samples.

## Comparisons

The JSON report includes a `comparisons` array. Each entry compares the
`default` profile against one non-default profile on the same workload.

Deltas are always `candidate - baseline`. Latency metrics are lower-is-better,
so a negative latency delta is an improvement.

Classification is conservative:

- Rate-like metrics need at least `0.005` absolute change; estimated latency
  needs at least `1.0` ms.
- Any decline in `schedule_success_rate` prevents an `improved` label.
- `regressed`: success rate declined with no other improvements, or only
  important metrics got worse.
- `trade_off`: success rate declined while some metrics improved, or some
  metrics improved while other important metrics got worse.
- `improved`: at least one key metric improved and none of the compared metrics
  clearly got worse.
- `neutral`: no compared metric moved past the small-change threshold.

The Markdown report has a `## Comparisons` table with workload, candidate,
result, and the main improved/regressed metrics. Notes are scoped to
"observed in this simulated workload" and are not production claims.

Compared delta keys:

- `schedule_success_rate`
- `cpu_quota_utilization`
- `mem_quota_utilization`
- `node_load_balance`
- `template_locality_hit_rate`
- `create_latency_p50_ms`
- `create_latency_p95_ms`

## Scope Notes

- Each default workload runs once per profile and reports baseline-vs-profile
  placement and quality metrics.
- The simulator uses a local filter → score → bind shape (not production
  Filter/Score plugin equivalence).
- Invalid profile/workload names fail the run; infeasible requests are counted
  as rejected with explicit reasons.
- JSON and Markdown reports include seed, node count, workload definitions,
  profile names, placement counts, and metric schema.
- The package lives under `pkg/scheduler/simulator` with a thin CLI under
  `cmd/schedulerbench`; production scheduler defaults are unchanged.
- Reports include measurement-path and claim-evidence context rather than only
  headline numbers.

## Verification

```bash
go test ./pkg/scheduler/simulator ./cmd/schedulerbench
go run ./cmd/schedulerbench --verify --out ./schedulerbench-report
```
