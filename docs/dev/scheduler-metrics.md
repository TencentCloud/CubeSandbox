# CubeSandbox Scheduler Evaluation Metrics

This document defines the topic 1 scheduling-quality metrics and how they map
to the current scheduler simulator report. Metrics are for relative comparison:
same node set, same workload, and same random seed, comparing `default` against
a candidate profile.

These numbers are offline scheduling-decision evidence, not production cluster
performance. The simulator does not start real MicroVMs and does not measure
CubeAPI create latency.

Implementation sources:

- Metric calculation: `Metrics` in `CubeMaster/pkg/scheduler/simulator/simulator.go`
- Report output: from the `CubeMaster` directory, run `go run ./cmd/schedulerbench`
- How to run: `docs/dev/scheduler-simulator-benchmark.md`

Current CLI flags (`CubeMaster/cmd/schedulerbench/main.go`):

| Flag | Default | Meaning |
|---|---|---|
| `--out` | `schedulerbench-report` | Directory for `report.json` and `report.md` |
| `--seed` | `20260903` (`DefaultConfig().Seed`) | Deterministic seed recorded in the report. Explicit CLI `--seed 0` is invalid (library zero-value `Config{}` still defaults to `20260903`). |
| `--nodes` | `4` (`DefaultConfig().NodeCount`) | Simulated node count; supported values are **1–4**. Explicit CLI `--nodes 0` is invalid (library zero-value `Config{}` still defaults to 4). |
| `--profiles` | `default,balanced_spread,template_locality_first,binpack_utilization` | Comma-separated profile list |
| `--workloads` | `burst_short_lived,same_template_repeated,mixed_size` | Comma-separated workload list |
| `--format` | `both` | Output format: `json`, `markdown`, or `both` |
| `--verify` | `false` | Before writing, check the default report's structure and internal consistency; this does not validate live performance |

Common commands:

```bash
go test ./pkg/scheduler/simulator ./cmd/schedulerbench
go run ./cmd/schedulerbench --verify --out ./schedulerbench-report
```

Unknown `--profiles` / `--workloads` names make `simulator.Run` return an error
and the CLI exits non-zero. `--verify` only validates the offline simulator
report contract. It does not run a real multi-node environment and does not
validate real CubeAPI/Cubelet create latency or production performance.

## Metric Overview

| Metric | JSON field | Unit | Direction | Topic acceptance |
|---|---|---|---|---|
| Schedule success rate | `schedule_success_rate` | ratio `[0,1]` | higher is better | schedule success rate |
| CPU quota utilization | `cpu_quota_utilization` | ratio `[0,1]` | higher is better | cluster packing |
| Memory quota utilization | `mem_quota_utilization` | ratio `[0,1]` | higher is better | cluster packing |
| Node load balance | `node_load_balance` | ratio `[0,1]` | higher is better | load balance |
| Template locality hit rate | `template_locality_hit_rate` | ratio `[0,1]` | higher is better | template locality |
| Create latency P50 | `create_latency_p50_ms` | ms | lower is better | create latency |
| Create latency P95 | `create_latency_p95_ms` | ms | lower is better | create latency |

The topic text requires at least packing rate, load balance, template hit rate,
schedule success rate, and create latency P50/P95. The current simulator report
emits all of the fields above, and every `metrics` object includes
`uses_estimated_latency` (always `true`). See the mapping tables below for the
full `Metrics` / `NodeLoad` fields.

## Measurement Semantics

All core metrics are written to `results[].workloads[].metrics` after one
workload finishes. Fix the following semantics before interpreting any single
metric.

1. Requests are processed in ascending `Arrival` order. Within the same tick,
   sandboxes with `EndsAt <= tick` are released first, then requests that arrive
   at that tick are scheduled.
2. Node infeasibility follows the same hard resource shape the simulator models
   (sandbox count at the limit, or CPU/memory quota cannot fit the request).
   This is a Filter→Score→bind *shape* with simulator-local constraints, not a
   claim of equivalence to CubeMaster's production Filter plugin set.
   Infeasible nodes never enter Score.
3. Feasible nodes are scored with profile weights, then the highest-scored node
   is bound (deterministic argmax with stable node-ID tie-break). Production
   CubeMaster truncates to `scheduler.priority_select_num` (shipped config: `1`)
   and selects within that set using `scheduler.least_select_name` — uniform for
   the default `random`, score-weighted for `sw`/`rw`/`rrw`. With the shipped
   `priority_select_num: 1` the truncated set is a single node, so production's
   pick coincides with this simulator's argmax (modulo tie-break).
4. `average_score_margin` is a simulator-local decision-gap proxy under that
   argmax bind; it is not a production selection metric.
5. Quota utilization, peak utilization, and load balance use the **node
   snapshot after the last request arrival**, not a time average and not a
   historical peak.
6. Template hit rate and estimated latency count only successfully scheduled
   requests.
7. The initial per-node template cache is static. A successful placement does
   not add the template to that node's cache.

Therefore, a short-lived workload may leave only not-yet-expired requests on
nodes at the end. Packing and balance reflect that snapshot, not peak occupancy.

## Current Report Field Mapping

The current CLI writes two isomorphic reports:

- `schedulerbench-report/report.json`
- `schedulerbench-report/report.md`

JSON paths:

```text
report.json
  provenance                       git_revision / git_dirty
  metric_schema[]                  metric contract: name / description / direction
  results[]
    profile
    workloads[]
      workload
      metrics                      all fields in the tables below live here
```

Markdown result-table columns map to JSON fields (`Report.Markdown()`):

| Markdown column | JSON field |
|---|---|
| `profile` | `results[].profile` |
| `workload` | `results[].workloads[].workload` |
| `success` | `schedule_success_rate` |
| `rejected` | `rejected_requests` |
| `locality hit` | `template_locality_hit_rate` |
| `load balance` | `node_load_balance` |
| `cpu util` | `cpu_quota_utilization` |
| `mem util` | `mem_quota_utilization` |
| `latency p50 ms` | `create_latency_p50_ms` |
| `latency p95 ms` | `create_latency_p95_ms` |
| `avg feasible` | `average_feasible_candidates` |

`Metrics` JSON fields match `simulator.go` struct tags **one-to-one**; there are
no other metrics keys:

| Go field | JSON field | Purpose |
|---|---|---|
| `TotalRequests` | `total_requests` | Success-rate denominator |
| `ScheduledRequests` | `scheduled_requests` | Success-rate numerator; also hit-rate and latency sample denominator |
| `RejectedRequests` | `rejected_requests` | Requests with no feasible node |
| `SuccessRate` | `schedule_success_rate` | `scheduled_requests / total_requests` |
| `PlacementCounts` | `placement_counts` | Successful placements per node |
| `NodeLoadBalance` | `node_load_balance` | Final-snapshot node load balance |
| `TemplateLocalityHitRate` | `template_locality_hit_rate` | Share of successful requests landing on initially warm template nodes |
| `AverageCPUUtilization` | `cpu_quota_utilization` | Arithmetic mean of final-snapshot per-node CPU utilization |
| `PeakCPUUtilization` | `peak_cpu_utilization` | Max per-node CPU utilization in the final snapshot |
| `AverageMemUtilization` | `mem_quota_utilization` | Arithmetic mean of final-snapshot per-node memory utilization |
| `PeakMemUtilization` | `peak_mem_utilization` | Max per-node memory utilization in the final snapshot |
| `CreateLatencyP50MS` | `create_latency_p50_ms` | Estimated create latency P50 for successful requests |
| `CreateLatencyP95MS` | `create_latency_p95_ms` | Estimated create latency P95 for successful requests |
| `UsesEstimatedLatency` | `uses_estimated_latency` | Always `true` in the simulator |
| `AverageCPUHeadroom` | `average_cpu_headroom` | Mean CPU headroom after each successful placement |
| `AverageScoreMargin` | `average_score_margin` | Mean score gap between first and second place, averaged only over scheduled decisions that had at least two scored candidates; **0** when no such observations exist |
| `AverageFeasibleCandidates` | `average_feasible_candidates` | `feasible_candidate_evaluations / scheduled_requests` |
| `FeasibleEvaluations` | `feasible_candidate_evaluations` | Sum of feasible-node counts over scheduled requests |
| `FailureReasons` | `failure_reasons` | Present when there are rejections; currently only `no_feasible_node` (`omitempty`) |
| `Warnings` | `warnings` | Capacity hints when there are rejections (`omitempty`) |
| `NodeFinalState` | `node_final_state` | Final occupancy per node; values are `NodeLoad` |

`node_final_state.<id>` maps to `NodeLoad`:

| Go field | JSON field |
|---|---|
| `RunningSandboxCount` | `running_sandbox_count` |
| `UsedCPUMilli` | `used_cpu_milli` |
| `UsedMemMB` | `used_mem_mb` |
| `CPUUtilization` | `cpu_utilization` |
| `MemUtilization` | `mem_utilization` |

`scoreCandidates` scores and ranks every feasible node, then binds the top
scored node (deterministic argmax). Production CubeMaster truncates to
`scheduler.priority_select_num` (shipped config: `1`) and selects within that
set using `scheduler.least_select_name` — uniform for the default `random`,
score-weighted for `sw`/`rw`/`rrw`. With the shipped `priority_select_num: 1`
production coincides with this argmax (modulo tie-break). Under this
simulator's bind rule, truncating after a full sort would not change the
selected node, so the report exposes only `feasible_candidate_evaluations` /
`average_feasible_candidates` as the simulator-local breadth proxy (at most
node count).

`average_score_margin` is likewise a simulator-local decision-gap proxy under
argmax bind, not a production selection metric.

`docs/dev/scheduler-benchmark-report-schema.md` describes the profile × workload
matrix and `comparisons[]` shape emitted by the current CLI. This document is
authoritative for the current `Metrics` fields.

## Schedule Success Rate

Field: `schedule_success_rate`

### Definition

Share of workload requests that successfully selected a node and completed a
simulated bind.

### Calculation

```text
schedule_success_rate = scheduled_requests / total_requests
```

Code: `Metrics.SuccessRate = ratio(ScheduledRequests, TotalRequests)`.

A request counts as success if and only if at least one node satisfies:

```text
len(active) < MaxSandboxes
used_cpu + request.cpu <= node.cpu_quota
used_mem + request.mem <= node.mem_quota
```

Otherwise it increments `rejected_requests` and
`failure_reasons["no_feasible_node"]`. The current simulator does not distinguish
CPU shortage, memory shortage, or sandbox-count limits; all three share one
failure reason.

`scheduled_requests + rejected_requests` must equal `total_requests`.

### Workload reading guide

| Workload | How to read |
|---|---|
| `mixed_size` | Primary. Large shapes are more likely to have nowhere to land; success rate exposes packing and fragmentation differences. |
| `burst_short_lived` | Secondary. With the default 4 nodes and current seed it is often 1.0, so it alone does not prove a better strategy. |
| `same_template_repeated` | Secondary. Request shapes are uniform; success rate is usually near 1.0. Prefer hit rate and balance here. |

### What it can show

- Whether the strategy placed requests on feasible nodes under that node
  capacity and request sequence.
- Whether a candidate trades schedulability for packing, balance, or locality.
- Whether failures come from capacity pressure: inspect `rejected_requests` and
  `failure_reasons`.

### What it cannot show

- Real-cluster API success, Cubelet create success, or sandbox readiness.
- Full Filter-plugin semantics. The simulator keeps only resource/count hard
  constraints: no hard template-locality filter and no live create-count filter.
- Success rate 1.0 does not mean high scheduling quality; it only means this
  request set did not exceed simulated capacity.

## CPU Quota Utilization

Field: `cpu_quota_utilization`

### Definition

Arithmetic mean of per-node CPU quota utilization after the workload ends.
Occupancy is bound sandbox quota that has not yet expired, not host CPU busy.

### Calculation

For each node in the final snapshot:

```text
cpu_utilization_i = used_cpu_milli_i / node_cpu_milli_i
cpu_quota_utilization = mean(cpu_utilization_i)
```

Code: first return value of `cpuUtilization(nodes)`, stored as
`Metrics.AverageCPUUtilization`.

Notes:

- This is an equal-weight node mean, not cluster-weighted
  `sum(used_cpu) / sum(node_cpu)`. Default node CPU quotas differ
  (4000/4000/6000/8000), so the two formulas diverge.
- `used_cpu` only includes requests still active in the final snapshot.
  Expired short tasks are excluded.
- Per-node values are in `node_final_state.<id>.cpu_utilization`.
- `peak_cpu_utilization` is the max across that snapshot, not a historical peak
  during the run.

### Workload reading guide

| Workload | How to read |
|---|---|
| `mixed_size` | Primary. Shape diversity best exposes `binpack_utilization` packing goals. |
| `same_template_repeated` | Secondary. Longer lifetimes leave comparable occupancy in the final snapshot. |
| `burst_short_lived` | Not suitable alone as packing evidence. Short lifetimes release most requests by the end, so final utilization understates peak packing. |

### What it can show

- Whether a strategy tends to leave remaining requests on fuller or emptier
  nodes.
- Relative quota packing versus baseline on workloads that still hold occupancy
  at the end.
- With `peak_cpu_utilization` and `node_final_state`, whether occupancy is
  spread or concentrated.

### What it cannot show

- Real CPU busy, steal, cgroup usage, or post-overcommit runnability.
- Time-average or peak packing rate.
- Higher utilization is not automatically better; it may come with worse
  balance, higher latency, or lower success rate.

## Memory Quota Utilization

Field: `mem_quota_utilization`

### Definition

Arithmetic mean of per-node memory quota utilization after the workload ends.
Occupancy is not-yet-expired sandbox memory quota.

### Calculation

```text
mem_utilization_i = used_mem_mb_i / node_mem_mb_i
mem_quota_utilization = mean(mem_utilization_i)
```

Code: first return value of `memUtilization(nodes)`, stored as
`Metrics.AverageMemUtilization`. Memory units are MB from the simulated quota;
the report does not convert to bytes.

Semantics match CPU utilization: equal-weight nodes, final snapshot, excluding
released requests. Per-node detail is in
`node_final_state.<id>.mem_utilization`; snapshot max is `peak_mem_utilization`.

On default nodes, some workloads have similar CPU/memory request ratios, so the
two utilizations may move together. `mixed_size` includes larger memory shapes
and the two may diverge; compare packing with both fields.

### Workload reading guide

| Workload | How to read |
|---|---|
| `mixed_size` | Primary. Large memory requests fragment more easily; memory utilization is more sensitive than CPU. |
| `same_template_repeated` | Secondary. |
| `burst_short_lived` | Not suitable alone as packing evidence, for the same reason as CPU utilization. |

### What it can show

- Final packing differences on the memory-quota dimension.
- Whether one dimension looks full while the other is still empty (one-sided
  packing illusion).
- Whether memory utilization is already near node limits when large requests
  are rejected.

### What it cannot show

- Real RSS, cache, balloon, or host memory pressure.
- Time-average or peak memory packing.
- Less fragmentation by itself; also inspect success rate and
  `placement_counts`.

## Node Load Balance

Field: `node_load_balance`

### Definition

How evenly combined node load is distributed in the final snapshot. Values near
1 mean similar load across nodes; values near 0 mean skewed load.

### Calculation

For each node:

```text
load_i = 0.5 * cpu_utilization_i + 0.5 * mem_utilization_i
```

Then:

```text
mean = average(load_i)
if mean == 0:
    node_load_balance = 1
else:
    stddev = sqrt(sum((load_i - mean)^2) / n)   # population stddev; denominator is node count
    node_load_balance = clamp(1 - stddev / mean, 0, 1)
```

Code: `nodeLoadBalance(nodes)`. `stddev / mean` is the coefficient of variation
(CV). All-zero load is defined as 1 to avoid division by zero.

Because the metric uses the final snapshot, short-task release can erase peak
imbalance.

### Workload reading guide

| Workload | How to read |
|---|---|
| `burst_short_lived` | Primary for `balanced_spread`. If final occupancy has already drained, also read `placement_counts`; do not trust this number alone. |
| `mixed_size` | Primary to check whether binpack trades balance for packing. |
| `same_template_repeated` | Secondary. Concentrated template placement can lower balance by design. |

### What it can show

- Whether final CPU/memory occupancy is more even across nodes.
- Whether spread-style strategies reduce skew relative to binpack / locality.
- With `placement_counts`, whether “spread placement” actually happened.

### What it cannot show

- That instantaneous create storms were spread. Balance uses only final-snapshot
  CPU/memory quota occupancy and does not track concurrent creates per node.
- Balance at every tick; only the end snapshot.
- Real node load average or symmetric CPU saturation.
- With unequal node capacity, equal-weight CV is not capacity-weighted cluster
  balance.

## Template Locality Hit Rate

Field: `template_locality_hit_rate`

### Definition

Share of successfully scheduled requests that landed on a node that already
cached that template at the start of the run.

### Calculation

After each successful bind, if `node.WarmTemplates[request.template] == true`,
increment the hit counter:

```text
template_locality_hit_rate = template_local_hits / scheduled_requests
```

Code: `Metrics.TemplateLocalityHitRate`. Remains 0 when
`scheduled_requests == 0`.

Default initial cache:

| Node | Initial templates |
|---|---|
| `node-a` | `python-agent`, `tiny-shell` |
| `node-b` | `python-agent` |
| `node-c` | `data-notebook` |
| `node-d` | `gpu-build`, `data-notebook` |

Hits use only the initial cache, not whether this run just created the same
template on that node.

### Workload reading guide

| Workload | How to read |
|---|---|
| `same_template_repeated` | Primary. All requests use `python-agent`, directly testing `template_locality_first`. |
| `mixed_size` | Secondary. Multiple templates show locality interacting with large-shape feasible sets. |
| `burst_short_lived` | Secondary. Templates alternate between `tiny-shell` and `python-agent`; hit-rate movement is usually smaller than the same-template case. |

### What it can show

- Whether a strategy more often chooses nodes that initially hold the template.
- Whether higher locality coincides with lower estimated latency or worse
  balance.
- Whether misses happen because no warm-template node was feasible.

### What it cannot show

- That a real template/image is local on Cubelet, or that a warm start actually
  happened.
- Cache warm-up during the run: the simulator never marks a template local after
  first placement.
- High hit rate does not automatically imply better P50/P95; resource pressure
  and node congestion still raise estimated latency.
- The denominator is successful schedules only. Failed requests that would have
  hit or missed warm nodes are invisible here.

## Create Latency P50 / P95

Fields:

- `create_latency_p50_ms`
- `create_latency_p95_ms`
- `uses_estimated_latency`

### Definition

**Estimated** create-latency distribution for successfully scheduled requests.
P50 is the median; P95 is the high tail. Neither is real create time.
`uses_estimated_latency` is always set to `true` in `runWorkload`, and the CLI
writes it into every `metrics` object.

### Calculation

One sample after each successful bind:

```text
estimated_latency_ms =
    80
  + (template_local ? 0 : 120)
  + (active_sandbox_count / max_sandboxes) * 80
  + ((cpu_pressure + mem_pressure) / 2) * 60
```

Where:

- `template_local` uses the initial `WarmTemplates`.
- Sampling happens after the request is written into `active`, so
  `active_sandbox_count` includes the current request.
- `cpu_pressure = used_cpu / node_cpu_quota` and
  `mem_pressure = used_mem / node_mem_quota`, using the post-bind node occupancy
  snapshot so the current request is counted once.
  `cpuHeadroomAfter` / `memHeadroomAfter` remain scoring helpers before bind;
  post-bind latency estimation does not call them, avoiding double-counting the
  current request in pressure.

Percentiles:

```text
sort(samples)
rank = (p / 100) * (n - 1)
value = linear_interpolate(sorted[floor(rank)], sorted[ceil(rank)])
```

Code: `estimatedCreateLatencyMS` and `percentile`. With no success samples,
P50/P95 are 0; `uses_estimated_latency` remains `true`.

Markdown columns `latency p50 ms` / `latency p95 ms` map to the first two
fields. `uses_estimated_latency` appears in the Markdown Metric Contract, not as
its own results-table column.

### Workload reading guide

| Workload | How to read |
|---|---|
| `same_template_repeated` | Primary. Locality changes should show first in P50. |
| `burst_short_lived` | Primary. Congestion and spread should show first in P95. |
| `mixed_size` | Secondary. Large-shape resource pressure raises estimated latency; read with success rate. |

### What it can show

- How template locality, node congestion, and resource pressure change the
  latency distribution under this estimator.
- P50 drops usually move with higher hit rate; P95 drops more often come from
  avoiding overheated nodes.
- Whether a candidate trades higher latency for packing, or spread for a lower
  tail.

### What it cannot show

- Real create latency on CubeAPI, Cubelet, snapshot/clone, or network paths.
- P99, throughput, queue time, or end-to-end readiness.
- Absolute comparison across different seeds, node counts, or estimator
  formulas.
- Treating report P50/P95 as measured values. Simulator
  `uses_estimated_latency` is always `true`. If a future path uses measured
  create latency, keep the same field names and set the marker to `false`; that
  is not current CLI behavior.

## Workload Constants

`workloadRequests` accepts only the following constants; other names return
`unknown workload`. CLI `--workloads` must use the right-hand strings.

| Go constant | CLI / JSON value | Request count |
|---|---|---|
| `WorkloadBurstShortLived` | `burst_short_lived` | 80 |
| `WorkloadSameTemplateRepeat` | `same_template_repeated` | 48 |
| `WorkloadMixedSizeCreate` | `mixed_size` | 60 |

Profiles likewise have only four legal names: `default`, `balanced_spread`,
`template_locality_first`, `binpack_utilization` (matching `ProfileDefault` and
related constants). Other names return `unknown profile`.

## Workload Reading Guide

| Workload | Primary metrics | Secondary metrics | Common trade-off |
|---|---|---|---|
| `burst_short_lived` | `node_load_balance`, `schedule_success_rate`, `create_latency_p95_ms` | `template_locality_hit_rate`, `placement_counts` | Spread may lower template hit rate |
| `same_template_repeated` | `template_locality_hit_rate`, `create_latency_p50_ms`, `create_latency_p95_ms` | `node_load_balance` | Locality-first may concentrate on a few warm nodes |
| `mixed_size` | `cpu_quota_utilization`, `mem_quota_utilization`, `schedule_success_rate` | `node_load_balance`, `rejected_requests` | Packing may lower balance or push large shapes out of the feasible set |

Comparison rule: change only the profile; keep workload, seed, and node
capacity fixed. Do not compare absolute metric values across different
workloads.

## Improvement Judgment

Conclusions must stay on the same workload, seed, and node capacity. Suggested
labels:

- `improved`: target metrics improve and `schedule_success_rate` does not fall.
- `trade_off`: target metrics improve, but at least one important secondary
  metric worsens.
- `neutral`: differences are too small to support a clear improvement.
- `regressed`: target metrics worsen, or success rate falls materially.
- `invalid`: missing fields, zero requests, or two runs are not comparable.

Do not showcase only the best single run. The default CLI runs one seed per
profile/workload; phrase conclusions as “observed under this workload,” not
“this strategy is generally better.”

## Known Limitations

- The simulator uses its own filter → score → bind rules, not the full
  CubeMaster pipeline or production plugin semantics.
- Final-snapshot metrics are not peak occupancy; short-lived workloads make this
  especially clear.
- Packing rates are equal-weight node quota utilization, not capacity-weighted
  cluster packing and not real resource usage.
- Template hits use a static initial cache.
- Create latency is an estimator, including the current pressure calculation.
  Every `metrics` object carries `uses_estimated_latency: true`.
- Failure reasons currently only include `no_feasible_node`.
- `failure_reasons` and `warnings` use `omitempty` and are absent from JSON on
  all-success runs.
- A single-seed report cannot support cross-environment generalization.

## Related Coverage

This document covers:

- definitions for the scheduling-quality metrics emitted by the simulator report
  (at least five core metrics);
- how to run the three default workloads with one CLI command and produce a
  report;
- how baseline-vs-profile deltas under the same workload explain improvement or
  trade-off.

Related docs:

- Simulator usage: `docs/dev/scheduler-simulator-benchmark.md`
- Pairwise comparison report shape: `docs/dev/scheduler-benchmark-report-schema.md`
