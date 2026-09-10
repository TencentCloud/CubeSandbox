# Scheduler Benchmark Report Schema

This document defines the JSON report emitted by the current topic 1 offline
scheduler simulator. The report is a deterministic profile-by-workload matrix:
each configured profile runs against each configured workload, and the
`comparisons` section computes candidate-minus-baseline deltas against the
`default` profile for the same workload.

The current generator is:

```bash
go run ./cmd/schedulerbench --out ./schedulerbench-report
```

It writes `report.json` and `report.md` by default. Use `--format json`,
`--format markdown`, or `--format both` to select output files. Unselected
`report.json` / `report.md` files already in `--out` are deleted.

Use these verification commands from `CubeMaster`:

```bash
go test ./pkg/scheduler/simulator ./cmd/schedulerbench
go run ./cmd/schedulerbench --verify --out ./schedulerbench-report
```

`--verify` checks the default report's structure and internal consistency
before files are written. It is not a validator for arbitrary reduced
`--profiles` or `--workloads` runs, and it does not validate live performance
or semantic equivalence to production scheduling.

## Top-Level Shape

```json
{
  "run_id": "scheduler-sim-seed-20260903-nodes-4-b2af389f1d2d",
  "config": {
    "seed": 20260903,
    "node_count": 4,
    "profiles": [
      "default",
      "balanced_spread",
      "template_locality_first",
      "binpack_utilization"
    ],
    "workloads": [
      "burst_short_lived",
      "same_template_repeated",
      "mixed_size"
    ]
  },
  "provenance": {
    "git_revision": "unknown",
    "git_dirty": null
  },
  "metric_schema": [
    {
      "name": "schedule_success_rate",
      "description": "Scheduled requests divided by total workload requests.",
      "direction": "higher is better"
    }
  ],
  "results": [
    {
      "profile": "default",
      "workloads": [
        {
          "workload": "burst_short_lived",
          "metrics": {
            "total_requests": 80,
            "scheduled_requests": 80,
            "rejected_requests": 0,
            "schedule_success_rate": 1.0,
            "placement_counts": {
              "node-a": 17
            },
            "node_load_balance": 0.92,
            "template_locality_hit_rate": 0.40,
            "cpu_quota_utilization": 0.70,
            "peak_cpu_utilization": 0.75,
            "mem_quota_utilization": 0.70,
            "peak_mem_utilization": 0.75,
            "create_latency_p50_ms": 226.4,
            "create_latency_p95_ms": 312.8,
            "uses_estimated_latency": true,
            "average_cpu_headroom": 0.58,
            "average_score_margin": 1.47,
            "average_feasible_candidates": 4.0,
            "feasible_candidate_evaluations": 320,
            "node_final_state": {
              "node-a": {
                "running_sandbox_count": 12,
                "used_cpu_milli": 3000,
                "used_mem_mb": 6144,
                "cpu_utilization": 0.75,
                "mem_utilization": 0.75
              }
            }
          }
        }
      ]
    }
  ],
  "comparisons": [
    {
      "workload": "burst_short_lived",
      "baseline_profile": "default",
      "candidate_profile": "template_locality_first",
      "result": "improved",
      "deltas": {
        "schedule_success_rate": 0.0,
        "cpu_quota_utilization": 0.0,
        "mem_quota_utilization": 0.0,
        "node_load_balance": 0.0,
        "template_locality_hit_rate": 0.15,
        "create_latency_p50_ms": -26.6,
        "create_latency_p95_ms": -6.7
      },
      "improved_metrics": [
        "template_locality_hit_rate",
        "create_latency_p50_ms",
        "create_latency_p95_ms"
      ],
      "regressed_metrics": [],
      "notes": [
        "Observed in this simulated workload \"burst_short_lived\": candidate profile \"template_locality_first\" versus baseline \"default\" is classified as improved.",
        "Latency metrics are estimated by the simulator, not measured from a live cluster.",
        "Deltas are candidate minus baseline; latency decreases are improvements. These numbers are offline estimates, not live cluster measurements."
      ]
    }
  ]
}
```

The example above is abbreviated. Generate a complete report locally when
needed; generated benchmark reports are not checked into the repository.

## Top-Level Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `run_id` | string | yes | Readable seed/node prefix plus a 12-hex SHA-256 prefix over effective seed, node count, ordered profiles/workloads, Git revision, and dirty state. |
| `config.seed` | number | yes | Deterministic workload seed. |
| `config.node_count` | number | yes | Simulated node count (**1–4**). |
| `config.profiles` | string array | yes | Profiles included in the run. |
| `config.workloads` | string array | yes | Workloads included in the run. |
| `provenance.git_revision` | string | yes | Source revision, or `unknown` when unavailable. Build-info VCS settings take priority; the CLI falls back to bounded, read-only Git commands. |
| `provenance.git_dirty` | boolean or null | yes | Whether the source tree was modified; `null` means the state is unknown. |
| `metric_schema[]` | array | yes | Metric contract entries with `name`, `description`, and `direction`. |
| `results[]` | array | yes | One entry per profile. |
| `results[].profile` | string | yes | Profile name. |
| `results[].workloads[]` | array | yes | Workload results for that profile. |
| `results[].workloads[].workload` | string | yes | Workload name. |
| `results[].workloads[].metrics` | object | yes | Metric values for this profile/workload pair. |
| `comparisons[]` | array | yes | Baseline-vs-candidate comparisons using `default` as baseline. |

The current report does not emit `generated_at`, a captured `command`, a
top-level `simulator` object, top-level `baseline`/`candidate`
objects, or top-level `limitations`. Simulator-only limitations are represented
in metric descriptions, comparison notes, and `uses_estimated_latency`.

## Metrics Object

Every `results[].workloads[].metrics` object contains these keys:

- `total_requests`
- `scheduled_requests`
- `rejected_requests`
- `schedule_success_rate`
- `placement_counts`
- `node_load_balance`
- `template_locality_hit_rate`
- `cpu_quota_utilization`
- `peak_cpu_utilization`
- `mem_quota_utilization`
- `peak_mem_utilization`
- `create_latency_p50_ms`
- `create_latency_p95_ms`
- `uses_estimated_latency`
- `average_cpu_headroom`
- `average_score_margin` (mean first–second score gap over multi-candidate decisions only; 0 when none)
- `average_feasible_candidates`
- `feasible_candidate_evaluations`
- `node_final_state`

When at least one request is rejected, the metrics object also includes:

- `failure_reasons`
- `warnings`

`failure_reasons` currently uses only `no_feasible_node`, which groups CPU,
memory, and sandbox-count capacity failures. Unknown workload/profile names fail
the command before a report is produced.

`average_feasible_candidates` is
`feasible_candidate_evaluations / scheduled_requests` and counts feasible
nodes for each scheduled request, so it is at most `node_count`. It is a
simulator-local candidate-breadth proxy, not scheduler CPU cost or latency.
This simulator binds the globally best scored feasible node (deterministic
argmax). Production CubeMaster truncates to `scheduler.priority_select_num`
(shipped config: `1`) and selects within that set using
`scheduler.least_select_name` — uniform for the default `random`,
score-weighted for `sw`/`rw`/`rrw`. With the shipped `priority_select_num: 1`
production coincides with this argmax (modulo tie-break), so the report does
not include a separate post-cap retained-candidate metric.
`average_score_margin` is a simulator-local decision-gap proxy under that
argmax bind, not a production selection metric.

Every JSON field on the emitted `Metrics` object has a matching
`metric_schema[]` entry (including accounting fields such as `total_requests`,
`placement_counts`, `peak_mem_utilization`, `feasible_candidate_evaluations`,
and `node_final_state`).

If both build information and the CLI Git fallback are unavailable,
`git_revision` is `unknown` and `git_dirty` is `null`. This state cannot
distinguish code versions. Reports contain no Git errors, paths, remotes, or
timestamps.

## Node Final State

Each `node_final_state.<node_id>` object contains:

- `running_sandbox_count`
- `used_cpu_milli`
- `used_mem_mb`
- `cpu_utilization`
- `mem_utilization`

These values are from the final active-request snapshot, not a time average and
not a historical peak.

## Comparison Fields

Each `comparisons[]` entry compares a non-default profile against the `default`
profile for the same workload.

| Field | Type | Required | Description |
|---|---|---|---|
| `workload` | string | yes | Workload being compared. |
| `baseline_profile` | string | yes | Always `default` in the current generator. |
| `candidate_profile` | string | yes | Non-default profile being compared. |
| `result` | string | yes | One of `improved`, `trade_off`, `neutral`, or `regressed`. |
| `deltas` | object | yes | Candidate metric minus baseline metric. |
| `improved_metrics` | string array | yes | Metrics classified as improved. |
| `regressed_metrics` | string array | yes | Metrics classified as regressed. |
| `notes` | string array | yes | Simulator-scoped interpretation notes. |

Compared delta keys:

- `schedule_success_rate`
- `cpu_quota_utilization`
- `mem_quota_utilization`
- `node_load_balance`
- `template_locality_hit_rate`
- `create_latency_p50_ms`
- `create_latency_p95_ms`

Latency metrics are lower-is-better, so negative latency deltas are
improvements. Rate-like metrics need at least `0.005` absolute change;
estimated latency needs at least `1.0` ms. Any decline in
`schedule_success_rate` prevents an `improved` result.

## Markdown Summary

The Markdown report mirrors the same data at a compact level:

- run metadata;
- profile/workload result table;
- comparison table;
- metric contract.

The Markdown table does not include every nested JSON field. Use `report.json`
for `placement_counts`, `failure_reasons`, `warnings`, and `node_final_state`.

## Acceptance Mapping

This report contract supports:

- Acceptance 1: at least five scheduling quality metrics.
- Acceptance 5: one-command benchmark report for at least three workloads.
- Acceptance 6: quantitative comparison and trade-off explanation.

It is still simulator-only evidence. It does not by itself satisfy the full
plugin/Profile configuration and custom-plugin acceptance items.
