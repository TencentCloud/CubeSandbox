# RESULTS

## 真实多 VM：创建→Ready→首命令端到端延迟 (`vm_e2e_latency`) — PASS
Source: `summaries/vm_e2e_latency.json` (client timeline; not scheduler-internal delay).
{
  "experiment": "vm_e2e_latency",
  "status": "PASS",
  "arms": [
    {
      "arm": "default",
      "n": 10,
      "fail_n": 0,
      "api_p50_ms": 382.0,
      "api_p95_ms": 690.9499999999998,
      "ready_p50_ms": 813.0,
      "ready_p95_ms": 1123.35,
      "ready_max_ms": 1167.0,
      "usable_p50_ms": 904.0,
      "usable_p95_ms": 1217.5,
      "usable_max_ms": 1249.0,
      "burst_success": 5,
      "burst_n": 5,
      "metric_source": "client_timeline"
    },
    {
      "arm": "binpack_utilization",
      "n": 10,
      "fail_n": 0,
      "api_p50_ms": 372.0,
      "api_p95_ms": 878.0499999999995,
      "ready_p50_ms": 755.0,
      "ready_p95_ms": 1257.0499999999995,
      "ready_max_ms": 1505.0,
      "usable_p50_ms": 869.0,
      "usable_p95_ms": 1355.4499999999994,
      "usable_max_ms": 1607.0,
      "burst_success": 5,
      "burst_n": 5,
      "metric_source": "client_timeline"
    }
  ]
}

## 突发短创建：节点分散与启动延迟 (`burst_scenario`) — PASS
Business read: burst dispersion and ready latency.
{
  "experiment": "burst_scenario",
  "status": "PASS",
  "arms": [
    {
      "arm": "default",
      "success": 8,
      "n": 8,
      "node_distribution": {
        "node-a": 4,
        "node-d": 2,
        "node-b": 2
      },
      "hot_node_max": 4,
      "usable_p50_ms": 3044.0,
      "usable_p95_ms": 3070.65
    },
    {
      "arm": "binpack_utilization",
      "success": 8,
      "n": 8,
      "node_distribution": {
        "node-d": 2,
        "node-b": 2,
        "node-a": 2,
        "node-c": 2
      },
      "hot_node_max": 2,
      "usable_p50_ms": 2216.5,
      "usable_p95_ms": 2296.8
    }
  ],
  "business_read": "burst_dispersion_and_ready_latency",
  "default_hot_max": 4,
  "binpack_hot_max": 2
}

## 镜像局部性：有缓存/无缓存命中对照 (`locality_scenario`) — NO_IMPROVEMENT
Cache contrast: template missing on node-b, present on other three nodes.
{
  "experiment": "locality_scenario",
  "status": "NO_IMPROVEMENT",
  "cache_presence": {
    "node-c": true,
    "node-b": false,
    "node-d": true,
    "node-a": true
  },
  "arms": [
    {
      "arm": "default",
      "n": 6,
      "success": 6,
      "cache_hit_rate": 1.0,
      "node_distribution": {
        "node-c": 2,
        "node-a": 3,
        "node-d": 1
      },
      "usable_p50_ms": 2095.5,
      "usable_p95_ms": 4572.75
    },
    {
      "arm": "template_locality_first",
      "n": 6,
      "success": 6,
      "cache_hit_rate": 1.0,
      "node_distribution": {
        "node-c": 2,
        "node-a": 3,
        "node-d": 1
      },
      "usable_p50_ms": 1353.5,
      "usable_p95_ms": 1538.75
    }
  ]
}

## CPU/内存错配装箱：碎片·空节点·探针接纳·延迟 (`binpack_scenario`) — NO_IMPROVEMENT
Supplement only. Formal remains `VALID_NO_IMPROVEMENT`.
Same fill list (cpu_heavy/mem_heavy×6). Binpack packed all 12 fills onto `.55` and kept 3 empty nodes vs default 1; probe admission tied at 6/6; frag_loss tied.
{
  "experiment": "binpack_scenario",
  "status": "NO_IMPROVEMENT",
  "occupancy_matched": false,
  "arms": [
    {
      "arm": "default",
      "fill_success": 12,
      "fill_n": 12,
      "fill_nodes": {
        "node-c": 1,
        "node-a": 7,
        "node-b": 2,
        "node-d": 2
      },
      "post_fill_empty_nodes": 1,
      "post_fill_total_mvm": 12.0,
      "fragmentation_loss": 0.1578947368421053,
      "nodewise_slots": 16,
      "aggregate_upper": 19,
      "probe_success": 6,
      "probe_n": 6,
      "probe_admission_rate": 1.0,
      "latency_usable_p50_ms": 1053.5,
      "latency_usable_p95_ms": 1752.4999999999998,
      "latency_usable_max_ms": 1829,
      "note": "supplement_only; formal remains VALID_NO_IMPROVEMENT"
    },
    {
      "arm": "binpack_utilization",
      "fill_success": 12,
      "fill_n": 12,
      "fill_nodes": {
        "node-d": 12
      },
      "post_fill_empty_nodes": 3,
      "post_fill_total_mvm": 11.0,
      "fragmentation_loss": 0.1578947368421053,
      "nodewise_slots": 16,
      "aggregate_upper": 19,
      "probe_success": 6,
      "probe_n": 6,
      "probe_admission_rate": 1.0,
      "latency_usable_p50_ms": 1033.0,
      "latency_usable_p95_ms": 1280.85,
      "latency_usable_max_ms": 1331,
      "note": "supplement_only; formal remains VALID_NO_IMPROVEMENT"
    }
  ],
  "reasons": [
    "binpack_more_empty_nodes"
  ],
  "formal_override": false,
  "formal_remains": "VALID_NO_IMPROVEMENT"
}

## 外部 HTTP scorer：正常/超时/非2xx/非法分退化 (`external_scorer_degradation`) — PASS
ok=3/3 success; timeout/non2xx/bad_score fail-closed on this lab binary (0/3). No filter resurrection. In-tree path is fail-open. Prometheus `outcome_delta` tables omitted (lab names were not in-tree `cube_scheduler_external_http_score_*`).
{
  "experiment": "external_scorer_degradation",
  "status": "PASS",
  "modes": [
    {
      "mode": "ok",
      "n": 3,
      "success": 3,
      "fail_open_observed": false,
      "fail_closed_observed": false,
      "any_resurrected_filtered": false,
      "node_distribution": {
        "node-a": 3
      },
      "usable_latencies_ms": [
        863,
        869,
        823
      ]
    },
    {
      "mode": "timeout",
      "n": 3,
      "success": 0,
      "fail_open_observed": false,
      "fail_closed_observed": true,
      "any_resurrected_filtered": false,
      "node_distribution": {},
      "usable_latencies_ms": [
        null,
        null,
        null
      ]
    },
    {
      "mode": "non2xx",
      "n": 3,
      "success": 0,
      "fail_open_observed": false,
      "fail_closed_observed": true,
      "any_resurrected_filtered": false,
      "node_distribution": {},
      "usable_latencies_ms": [
        null,
        null,
        null
      ]
    },
    {
      "mode": "bad_score",
      "n": 3,
      "success": 0,
      "fail_open_observed": false,
      "fail_closed_observed": true,
      "any_resurrected_filtered": false,
      "node_distribution": {},
      "usable_latencies_ms": [
        null,
        null,
        null
      ]
    }
  ],
  "note": "Behavioral evidence only. Legacy cubemaster_scheduler_external_http_score_* counters are not the in-tree metric family."
}

## Limits
- Small samples; no significance claims.
- Degradation arms were fail-closed on the **lab binary** used for this run; the in-tree path is fail-open on scorer errors (see `docs/guide/cubemaster-scheduler-config.md`). No claim is made about lab binaries generally.
- Formal matrix not re-run.
