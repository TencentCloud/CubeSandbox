# Scheduler Strategy ↔ Workload ↔ Metrics Mapping

> This document is the module-2 (scenario strategy system) deliverable: it pins
> down how the three built-in scenario strategies map to their evaluation
> workloads, quality metrics, runtime overhead metrics, and trade-offs, serving
> as the Benchmark (module 3) evaluation contract. For the implementation see
> `CubeMaster/pkg/base/config/scheduler_factory.yaml` (go:embed factory config)
> and `CubeMaster/pkg/scheduler/profile/`; for the simulator metric definitions
> see `CubeMaster/cmd/schedsim/README.md`.

## Overview

| Strategy | Route | Workload | Primary quality metrics | Runtime / overhead metrics |
|---|---|---|---|---|
| BurstBalance | labels `{workload: burst_balance}` | burst (short-lived burst: 500 requests, Poisson 50/s, lifetime U(10s,120s), small spec) | herding, load balance (CV / Jain), success rate | create latency, scheduling throughput |
| TemplateReuse | labels `{workload: template_reuse}` | template_storm (same-template storm: 300 requests, one TemplateID, 30% of nodes preloaded with the template) | template hit rate, per-template load balance, success rate | create latency, concurrency-limit failure rate |
| MixedBinPack | default (fallback, no route) | mixed_spec (mixed sizes: 400 requests, 1C2G:2C4G:8C16G = 6:3:1, mixed lifetimes) | packing rate, resource fragmentation, success rate | scheduling latency, per-request compute overhead |

Route keys are controlled by `scheduler.profile_route_label_keys` (factory value
`["workload"]`); requests matching no route fall back to the default strategy
(MixedBinPack). All three strategies are carried by the scheduler core's generic
Mandatory Guards + parallel Filter + parallel Score + Top-N selection pipeline:
guards decide *whether a node can run the request at all*, strategies only order
the feasible candidates.

## Zero-config injection and legacy compatibility

The three strategies above are also the *zero-config default*: when a
deployment configures none of `scheduler.profiles`, `scheduler.filter` or
`scheduler.score`, `preHandleScheduler`
(`CubeMaster/pkg/base/config/config.go`) injects the embedded factory profiles
from `scheduler_factory.yaml`. Scheduling then follows the factory profiles —
mandatory guards, factory scorers and spread selection — which is **not**
identical to the pre-profile legacy static registration.

To keep the legacy behavior, configure at least one legacy key
(`scheduler.filter.enable_filters` and/or `scheduler.score.enable_scorers`):
injection is all-or-nothing and is then skipped entirely, and
`profile.Compile` rebuilds the pipeline from the legacy config via
`compileLegacy`, preserving the old tolerance semantics (unknown plugin names
and zero-weight scorers are skipped).

Compatibility is pinned by tests at two levels:

- compile layer: `TestLegacyCompileSkipsUnknownPluginsAndZeroWeights` and
  `TestProfileRoutingAndLegacyFallback` in
  `CubeMaster/pkg/scheduler/profile/profile_test.go`;
- same-input selection: `TestLegacyAndExplicitProfileSelectIdentically` in
  `CubeMaster/pkg/scheduler/profile_compat_test.go` runs the legacy-compiled
  pipeline and a semantically equivalent explicit profile over the same node
  state and the same request, and asserts identical per-node final scores,
  ordering and selected node.

## BurstBalance

**Profile composition** (`burst_balance`):

- Score: `real_time_weighted_average` (weight 1.0, quota watermark) +
  `create_concurrency_score` (weight 1.0, in-flight create pressure: the
  Cubelet-reported `realtime_create_num` plus this Master's local in-flight
  count scaled by the number of healthy Masters; the lower the share of the
  create-concurrency limit, the higher the score)
- Selection: `spread` with top_n=3, spreading among the top candidates by
  running sandbox count
- Failure policy: filter fail-closed / score default-score / no_candidate backoff

**Target workload**: burst. Under high concurrency, identically sized requests
see nearly identical quota watermarks at selection time, so quota scoring alone
cannot tell apart nodes already queuing creates — in-flight creation pressure is
a separate scoring dimension.

**Quality metrics**:

- Herding (`herding_top1_share`): share of placements won by the most-selected
  node; lower is better
- Load balance: coefficient of variation of per-node usage ratios
  (`load_cv_cpu`/`load_cv_mem`) and Jain's fairness index
  (`jain_cpu`/`jain_mem`)
- Success rate (`success_rate`)

**Runtime overhead metrics**: end-to-end create latency P50/P95
(`sandbox_create_duration_seconds`), scheduling throughput (derived from
`scheduler_duration_seconds`).

**Trade-offs**: spreading touches more cold nodes, and every newly touched node
pays one cold start (template miss); packing rate and template hit rate may drop
compared to concentrated placement. See the
[scheduler eval benchmark](/dev/scheduler-eval-benchmark) for measurements.

## TemplateReuse

**Profile composition** (`template_reuse`):

- Score: `image_score` (weight 0.7, template locality: full score when the node
  holds a local replica of the template) + `template_local_pressure`
  (weight 0.2, fewer in-flight creates of the *same* template on the node →
  higher score; fed by the localcache per-(node, template) counters registered
  on the create path) + `real_time_weighted_average` (weight 0.3, resource
  watermark backstop)
- Selection: `spread` with top_n=3
- Failure policy: filter fail-closed / score default-score / no_candidate backoff

**Target workload**: template_storm. With locality as the only dimension, a
same-template burst queues on a few replica nodes (intra-replica herding);
`template_local_pressure` spreads same-template create pressure across replica
nodes — this is the "spread by same-template create pressure" requirement.

**Quality metrics**:

- Template hit rate (`template_hit_rate`): share of successful placements where
  the selected node holds a local replica
- Per-template load balance: usage CV / Jain across the nodes holding the
  template's replicas
- Success rate

**Runtime overhead metrics**: create latency P50/P95 (the hit rate directly
drives the cold-start ratio), concurrency-limit failure rate
(`realtime_create_num` guard rejections / backoff retries, see
`scheduler_reschedules_total`).

**Trade-offs**: spreading across replica nodes means some requests land on
"freshly touched" replicas whose first request still pays cache warming; when
replica coverage is high (e.g. 30% preloaded), *preferring* replica nodes
already spreads placement over twenty-odd nodes, raising the hit rate while
potentially worsening real latency — the hit-rate vs create-latency balance is a
business-level choice.

## MixedBinPack

**Profile composition** (`mixed_binpack`, default):

- Score: `resource_fit_score` (weight 0.7, less remaining capacity after placing
  the request → higher score; `imbalance_penalty: 0.5` penalizes CPU/memory
  remaining-ratio skew) + `real_time_weighted_average` (weight 0.3, pulls back
  from extreme stacking)
- Selection: `spread` with top_n=2, Top-2 spread against herds
- Failure policy: filter fail-closed / score default-score / no_candidate backoff

**Target workload**: mixed_spec. With mixed sizes arriving together, prefer
filling already-used nodes and keep whole nodes empty, raising cluster packing
rate and reducing fragmentation.

**Quality metrics**:

- Packing rate: `cpu_alloc_rate` / `mem_alloc_rate` (Σ usage / Σ raw quota,
  time-weighted average)
- Resource fragmentation: `fragmentation_ratio` (free CPU on nodes that cannot
  fit the trace's largest shape, as a share of total free CPU)
- Active/empty nodes: `active_nodes_avg` / `empty_nodes_avg` (whole-empty nodes
  are reclaimable)
- Success rate

**Runtime overhead metrics**: scheduling latency P50/P95/P99
(`scheduler_duration_seconds`), per-request compute overhead (plugin time /
CubeMaster CPU).

**Trade-offs**: tight packing raises utilization but enlarges hotspots and
single-node blast radius; under 3× overcommit at moderate occupancy the per-node
quota deltas are tiny, so `resource_fit_score` values cluster and the effective
spread is governed by top_n and the `real_time_weighted_average` weight — tune
with single-variable A/B runs.

## Metric definitions and sources

The simulator (schedsim, `--workload burst|template_storm|mixed_spec`) and real
clusters (cube-bench + real Kubelets) share one set of metric definitions; only
the data source differs. The simulator aggregates them offline in
`pkg/scheduler/sim/metrics.go` (see the metrics table in
`cmd/schedsim/README.md`); production emits them via the Prometheus metrics in
`CubeMaster/pkg/scheduler/metrics.go`
(`scheduler_decisions_total{profile,template_hit}`,
`sandbox_create_duration_seconds{profile,result}`, etc.), labelled by profile
name, so benchmark conclusions extrapolate to production.

For the controlled-experiment method (same input, same environment, different
strategy) and the three modes (real-cluster primary, simulator secondary,
real-cluster A/B), see module 3 of the project plan; for quantified results see
the [scheduler eval benchmark](/dev/scheduler-eval-benchmark) and the
[factory profiles load test](/dev/scheduler-factory-loadtest).
