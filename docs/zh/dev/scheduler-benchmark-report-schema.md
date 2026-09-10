---
title: 调度基准报告 Schema
description: CubeSandbox 课题一调度基准与 simulator 的 JSON 报告合同。
status: draft
updated: 2026-09-03
---

# 调度基准报告 Schema

本文定义当前课题一离线调度 simulator 输出的 JSON 报告。报告是确定性的
profile × workload 矩阵：每个配置的 profile 对每个配置的 workload 各跑一次，
`comparisons` 段按同一 workload 计算候选相对 `default` baseline 的
candidate-minus-baseline 差值。

当前生成命令：

```bash
go run ./cmd/schedulerbench --out ./schedulerbench-report
```

默认写出 `report.json` 与 `report.md`。可用 `--format json`、
`--format markdown` 或 `--format both` 选择输出文件。

运行 `go run ./cmd/schedulerbench --verify --out ./schedulerbench-report`
可在写文件前，对照默认 workload/profile 验收合同检查内存中的报告，包括指标、
对比与仅限 simulator 的延迟要求。`--verify` 不是任意运行的通用校验器：合法但
缩小了 `--profiles` 或 `--workloads` 选择的运行会因缺少默认验收矩阵而校验失败。
省略 `--verify` 时，这类缩减运行仍可生成报告。校验只检查报告结构与用语；
不验证真实多节点运行，也不验证实测的 CubeAPI/Cubelet 创建延迟。

## 顶层形状

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

以上示例为缩略版。需要时请本地生成完整报告；生成的基准报告不入库。

## 顶层字段

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `run_id` | string | 是 | 由 seed 与节点数派生的确定性运行标识。 |
| `config.seed` | number | 是 | 确定性 workload 种子。 |
| `config.node_count` | number | 是 | 模拟节点数（**1–4**）。 |
| `config.profiles` | string 数组 | 是 | 本次运行包含的 profile。 |
| `config.workloads` | string 数组 | 是 | 本次运行包含的 workload。 |
| `acceptance_path.*` | object | 是 | 报告的结论证据与审阅范围上下文。 |
| `metric_schema[]` | array | 是 | 指标合同项，含 `name`、`description`、`direction`。 |
| `results[]` | array | 是 | 每个 profile 一条。 |
| `results[].profile` | string | 是 | Profile 名称。 |
| `results[].workloads[]` | array | 是 | 该 profile 下的 workload 结果。 |
| `results[].workloads[].workload` | string | 是 | Workload 名称。 |
| `results[].workloads[].metrics` | object | 是 | 该 profile/workload 对的指标值。 |
| `comparisons[]` | array | 是 | 以 `default` 为 baseline 的 baseline-vs-candidate 对比。 |

当前报告不输出 `generated_at`、`git_revision`、捕获的 `command`、顶层
`simulator` 对象、顶层 `baseline`/`candidate` 对象，或顶层 `limitations`。
仅限 simulator 的限制写在指标描述、comparison notes 以及
`uses_estimated_latency` 中。

## Metrics 对象

每个 `results[].workloads[].metrics` 对象包含这些键：

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
- `average_score_margin`（仅对多候选决策取第一名与第二名分差均值；无观测时为 0）
- `average_candidates_scored`
- `score_evaluations`
- `node_final_state`

当至少有一个请求被拒绝时，metrics 对象还会包含：

- `failure_reasons`
- `warnings`

`failure_reasons` 当前只使用 `no_feasible_node`，把 CPU、内存与 sandbox 数容量失败归为一类。未知 workload/profile 名称会在生成报告前使命令失败。

`average_candidates_scored` 等于 `score_evaluations / scheduled_requests`。
`score_evaluations` 统计 `scoreCandidates` 返回的截断后有序候选集，过滤与打分后受 `defaultPriorityCandidateNum` 上限约束。它是决策路径成本的近似，不是内部检查过的全部可行节点数。

## 节点最终状态

每个 `node_final_state.<node_id>` 对象包含：

- `running_sandbox_count`
- `used_cpu_milli`
- `used_mem_mb`
- `cpu_utilization`
- `mem_utilization`

这些值来自最终仍 active 的请求快照，不是时间平均，也不是历史峰值。

## 对比字段

每个 `comparisons[]` 项在同一 workload 上把非 default profile 与 `default`
profile 对比。

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `workload` | string | 是 | 被对比的 workload。 |
| `baseline_profile` | string | 是 | 当前生成器中恒为 `default`。 |
| `candidate_profile` | string | 是 | 被对比的非 default profile。 |
| `result` | string | 是 | `improved`、`trade_off`、`neutral` 或 `regressed` 之一。 |
| `deltas` | object | 是 | 候选指标减去 baseline 指标。 |
| `improved_metrics` | string 数组 | 是 | 判定为改善的指标。 |
| `regressed_metrics` | string 数组 | 是 | 判定为回退的指标。 |
| `notes` | string 数组 | 是 | 限定在 simulator 范围内的解读说明。 |

参与对比的 delta 键：

- `schedule_success_rate`
- `cpu_quota_utilization`
- `mem_quota_utilization`
- `node_load_balance`
- `template_locality_hit_rate`
- `create_latency_p50_ms`
- `create_latency_p95_ms`

延迟指标越低越好，因此负的延迟差值表示改善。比率类指标至少需要 `0.005`
的绝对变化；估算延迟至少需要 `1.0` ms。`schedule_success_rate` 一旦下降，
就不能得到 `improved` 结果。

## Markdown 摘要

Markdown 报告以紧凑形式镜像同一数据：

- 运行元数据；
- 验收映射；
- profile/workload 结果表；
- 对比表；
- 指标合同。

Markdown 表不包含全部嵌套 JSON 字段。需要 `placement_counts`、
`failure_reasons`、`warnings` 与 `node_final_state` 时请看 `report.json`。

## 验收对应

本报告合同支撑：

- 验收 1：至少五项调度质量指标。
- 验收 5：至少三种 workload 的一键基准报告。
- 验收 6：量化对比与 trade-off 说明。

它仍只是 simulator 证据，本身不足以满足完整的插件/Profile 配置与自定义插件验收项。
