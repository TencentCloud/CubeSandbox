---
title: Scheduler Benchmark Report Schema
description: JSON report contract for CubeSandbox topic 1 scheduler benchmark and simulator.
status: draft
updated: 2026-09-03
---

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
`--format markdown`, or `--format both` to select output files.

Run `go run ./cmd/schedulerbench --verify --out ./schedulerbench-report` to
check the generated in-memory report against the default workload/profile
acceptance contract, including its metric, comparison, and simulator-only
latency requirements, before files are written. `--verify` is not a validator
for arbitrary runs: a valid run with a reduced `--profiles` or `--workloads`
selection fails verification because it does not contain the default acceptance
matrix. Such reduced runs can still generate reports when `--verify` is
omitted. Verification checks report structure and terminology only; it does not
validate a real multi-node run or measured CubeAPI/Cubelet create latency.

## Top-Level Shape

```json
{
  "run_id": "scheduler-sim-seed-20260903-nodes-4",
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
  "acceptance_path": {
    "acceptance_path": "Each workload runs once per profile and reports baseline-vs-profile placement and quality metrics.",
    "domain_lens": "Scheduler score semantics: filter infeasible nodes first, score remaining candidates, then bind the highest score.",
    "failure_path": "Requests that cannot fit any node are counted as rejected with explicit failure reasons; invalid profile/workload names fail the run.",
    "evidence_path": "The JSON and Markdown reports include seed, node count, workload definitions, profile names, placement counts, and metric schema.",
    "review_path": "Offline deterministic benchmark package plus thin CLI; no production scheduler default behavior changes.",
    "distinctive_angle": "Measurement-path integrity and claim-evidence mapping are built into the generated report instead of only producing headline numbers."
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
            "average_candidates_scored": 2.88,
            "score_evaluations": 230,
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
| `run_id` | string | yes | Deterministic run identifier derived from seed and node count. |
| `config.seed` | number | yes | Deterministic workload seed. |
| `config.node_count` | number | yes | Simulated node count (**1–4**). |
| `config.profiles` | string array | yes | Profiles included in the run. |
| `config.workloads` | string array | yes | Workloads included in the run. |
| `acceptance_path.*` | object | yes | Claim-evidence and review-scope context for the report. |
| `metric_schema[]` | array | yes | Metric contract entries with `name`, `description`, and `direction`. |
| `results[]` | array | yes | One entry per profile. |
| `results[].profile` | string | yes | Profile name. |
| `results[].workloads[]` | array | yes | Workload results for that profile. |
| `results[].workloads[].workload` | string | yes | Workload name. |
| `results[].workloads[].metrics` | object | yes | Metric values for this profile/workload pair. |
| `comparisons[]` | array | yes | Baseline-vs-candidate comparisons using `default` as baseline. |

The current report does not emit `generated_at`, `git_revision`, a captured
`command`, a top-level `simulator` object, top-level `baseline`/`candidate`
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
- `average_candidates_scored`
- `score_evaluations`
- `node_final_state`

When at least one request is rejected, the metrics object also includes:

- `failure_reasons`
- `warnings`

`failure_reasons` currently uses only `no_feasible_node`, which groups CPU,
memory, and sandbox-count capacity failures. Unknown workload/profile names fail
the command before a report is produced.

`average_candidates_scored` is `score_evaluations / scheduled_requests`.
`score_evaluations` counts the truncated ranked candidate set returned by
`scoreCandidates`, which is capped by `defaultPriorityCandidateNum` after
filtering and scoring. It is a decision-path cost proxy, not the full number of
feasible nodes examined internally.

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
- acceptance map;
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
