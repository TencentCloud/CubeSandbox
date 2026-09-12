# Scheduler Evaluation: Real Control-Plane A/B Benchmark

> Branch: `feat/scheduler-eval-plugin` (scheduler plugin system, scenario profiles, schedsim, cube-bench rework)
> Date: 2026-09-07 | Environment: real control plane + simulated data plane on a single 96C/246G dev box

> **⚠ Timeliness notice (2026-09-09).** The A/B numbers below were measured
> **before the `selection.method: spread` fix** (commit `93f2f457`): at that
> time `spread` was a silent no-op identical to `random` (this report's own
> issue #1), so none of the three scenario profiles actually ran their stated
> selection semantics, and the legacy side compiled with zero scorers, i.e.
> near-first-fit stacking (issue #3). Read "legacy" below as *unscored
> first-fit* and every profile's placement as *score-sorted but near-random
> within top_n*. The post-fix picture from the simulator (same traces, same
> workload presets, 5 seeds) is in the section
> [Re-evaluation after the spread fix](#re-evaluation-after-the-spread-fix-2026-09-09)
> at the bottom, and in full detail in
> [Scheduler Simulator Evaluation Report](./scheduler-sim-report).

This report records a **real control-plane** A/B benchmark of the scheduler strategy profiles. Every component is a real process — real CubeMaster / CubeAPI (Rust) binaries built from the branch, MariaDB, Redis, and real cube-bench traffic — except the data plane (Kubelets), which is simulated because the box has no KVM. It serves as evidence for acceptance criterion 6 (quantified default-vs-new-strategy comparison).

## Topology and fidelity

```
cube-bench ──HTTP──▶ CubeAPI(:3000) ──HTTP──▶ CubeMaster(:8089) ──gRPC──▶ fakecubelet ×50
                     real Rust binary          real scheduler pipeline     (127.0.1.1–50:9999)
                     MariaDB 10.3 + Redis (node inventory in DB, node metrics via Redis
                     heartbeats — same paths as production)
```

Simulated data-plane behavior model: **50 ms on template-cache hit / 800 ms on miss with subsequent cache warming**, per-node quota accounting with periodic Redis metric heartbeats. 50 nodes × 64C/128G, 30% template preload (deterministic, seed 42).

Fidelity limits (important): the fake's load metrics (`cpu_load_usage`, etc.) do not grow with sandbox count, so `node_safety` never throttles concentration — legacy's "238 sandboxes on one node" outcome would be impossible on real Kubelets. Absolute latencies are not production predictions; **conclusions hold directionally for the locality-vs-spread tradeoff only**.

## Method

Three workloads (cube-bench presets), each run against legacy (no `profiles:` config) and its matching scenario profile — 6 runs total: burst ↔ burst_balance, template_storm ↔ template_reuse, mixed_spec ↔ mixed_binpack. Both variants of a workload replay **byte-identical** request traces (seed 42) over the same node fleet and preload draw — scheduler config is the only variable. Concurrency `-c` is sized above rate×lifetime (700–900); queue delay P50 ≈ 0.6 ms, so the client never self-throttles. Profiles verified active via the `profile="burst_balance"` etc. metric labels (legacy shows `profile="default"`).

## Results

Create latency (client-side; miss = latency >400 ms, matching the fake's own `cache_misses` counter exactly):

| workload / variant | n | P50 | P95 | avg | miss% | success |
|---|---|---|---|---|---|---|
| burst / legacy | 500 | 58.4ms | 810.0ms | **212.0ms** | **20.4%** | 100% |
| burst / burst_balance | 500 | 63.4ms | 814.2ms | **391.1ms (+84%)** | **44.2%** | 100% |
| template_storm / legacy | 300 | 59.7ms | 815.9ms | **265.6ms** | **27.3%** | 100% |
| template_storm / template_reuse | 300 | 60.5ms | 815.1ms | **357.1ms (+34%)** | **39.7%** | 100% |
| mixed_spec / legacy | 400 | 58.4ms | 812.1ms | **194.2ms** | **18.0%** | 100% |
| mixed_spec / mixed_binpack | 400 | 61.5ms | 817.2ms | **360.9ms (+86%)** | **40.0%** | 100% |

Placement spread (ground truth: per-node `mvm_num` probed from Redis every 5s mid-run):

| pair | peak distinct nodes | max sandboxes on one node |
|---|---|---|
| burst: legacy vs burst_balance | **3 vs 17** | 238 vs 58 |
| template_storm: legacy vs template_reuse | **4 vs 17** | 144 vs 35 |
| mixed_spec: legacy vs mixed_binpack | **9 vs 37** | 59 vs 23 |

Server-side cross-check: `sandbox_create_duration_seconds` means match the client (e.g. burst 209.8 vs 389.3 ms); the scheduling decision itself takes 0.2–0.3 ms with 1.0 attempt/create and zero reschedules — **all of the delta comes from placement**.

## Verdicts (counter-intuitive but directionally stable)

**All three new profiles are slower than legacy in this experiment**, by one shared mechanism: they spread placement wider, and every newly touched cold node costs an 800 ms cold start (with thundering-herd duplicates on concurrent creates to the same cold node). Legacy's naive concentration is accidentally optimal for template-cache warmth.

- **template_reuse**: master-side locality decisions did improve (`template_hit="true"` share 18.0%→28.7% storm / 20.3%→30.3% mixed), but because the seeded replica set spans 34/50 nodes, *preferring* replica nodes spreads placement over ~27–30 nodes per template; real cache misses rose 82→119 — **hit-rate decisions up, real latency 34% worse**.
- **mixed_binpack**: nominally a packer, actually the **most spread** variant (37/50 nodes). `resource_fit_score` does score in the packing direction (less remaining → higher score), but (1) `selection.method: spread` is a no-op (below) so the pick within top_n=2 is near-random; (2) under 3× overcommit the per-node quota deltas at these occupancies are tiny, so scores cluster; (3) `real_time_weighted_average` (a balancer, weight 0.3) pulls the other way. Worst case: mixed_spec tpl-c (8C16G) P50 = **808 ms** vs 60 ms legacy — 55% of big-spec creates went cold.
- **burst_balance**: placement is genuinely more balanced (peak 3→17 nodes), at +84% latency.

## Code issues uncovered by this experiment (filed in review)

1. **`selection.method: spread` is a no-op, identical to `random`**: `CubeMaster/pkg/scheduler/schedule.go:164-172` special-cases only `highest`; everything else goes to `LeastRandomSelect(TopN)`. All three shipped scenario profiles use `spread` — **none of them ever ran their stated semantics**. Much of the counter-intuitive outcome above traces to this.
2. **Profile compilation panics at boot when `scheduler.score.plugin_conf.<scorer>` is missing** (`imagescore.go:37-38`, `realtimescore.go:27-28`, `multifactorscore.go:23-24`) instead of returning a config validation error; the sim example yamls also fail to document that the `score:` block is required.
3. **The legacy default pipeline compiles with zero scorers** when the conf has no `score:` section; with `priority_select_num=1` it degenerates to near-deterministic first-fit, producing the extreme concentration (238/node).

## Re-evaluation after the spread fix (2026-09-09)

`spread` selection is real now (`schedule.go` `spreadSelect`), the sim baseline
gained a least-loaded scorer, and the matrix was re-run in schedsim (same
workload presets and trace parameters, 300 simulated nodes, 30% template
preload with remote-restore warming, seeds 42–46, quality + performance modes).
Full data: [Scheduler Simulator Evaluation Report](./scheduler-sim-report).
Headline corrections to the verdicts above:

| pair (300 nodes, mean of 5 seeds) | old conclusion (broken spread) | post-fix sim result |
| --- | --- | --- |
| burst: legacy vs burst_balance | profiles "spread wider, +84% latency" | Placement-neutral vs the *scored* legacy (Jain 0.567 both, hit 0.594 both, success 1.0). Vs the true unscored first-fit legacy, spread behavior is the big win: Jain 0.418→0.567, herding 1.3%→0.4%, CV −21%. The old legacy advantage came from zero-scorer stacking, not from a real policy. |
| template_storm: legacy vs template_reuse | "hit-rate decisions up, real latency 34% worse" | Decision quality is unambiguous post-fix: template_hit_rate 1.000 vs 0.324 (legacy) / 0.573 (first-fit), i.e. the locality scorer does exactly what it claims. Cost: balance concentrates on replica nodes (Jain 0.243, active nodes 78/300) — the intended locality-vs-balance trade. Whether the latency regression persists on a real data plane depends on restore cost and must be re-measured there. |
| mixed_spec: legacy vs mixed_binpack | "nominally a packer, actually the most spread variant (37/50 nodes)" | With real spread semantics the packer packs: 7.26/300 active nodes (legacy 165.5), template_hit 0.956, success 1.0, fragmentation 0 (fleet only ~2.3% allocated). Trade-offs: herding 16% and decision P99 +63% (1.56→2.54 ms). The old "binpack is insensitive" observation was an artifact of the no-op `spread` pick, not of `resource_fit_score`. |

Framework overhead measured directly (performance mode, per-stage wall-clock):
profiles add the mandatory-guards stage (~0.09 ms P50) but drop legacy's
optional-filter stage (~0.06 ms); net decision-cost delta is +3%…+13% P50,
dominated everywhere by prefilter candidate enumeration (~65% of total).
Scheduling-core throughput is ~1.0–1.4k decisions/s per master process on a
4 vCPU box, single-digit millisecond P99 throughout.

## Follow-up experiments

- ~~Implement real `spread` semantics (or switch to `method: highest`) and rerun this matrix~~ — **done** (commit `93f2f457`); sim re-run above. A real-cluster rerun of this exact matrix is still open and needed to re-validate the latency axis (the fake's 800 ms cold-start model interacts with placement width).
- Correlate fake load metrics with occupancy (so `node_safety` engages) and rerun, to see the comparison once legacy concentration is constrained.
- 3 repeats per cell to quantify herd variance (identical reruns here saw 52–102 misses; directions were stable).

## Reproducing

The harness (fakecubelet, orchestration scripts, configs) is one-off experiment code and is not merged into the repository. Key parameters: 50 nodes × 64C/128G, 30% template preload, fake latency model 50 ms hit / 800 ms miss (with warming), cube-bench `-c 700–900`, seed 42. Contact the report author for the harness under `/root/realtest/`.
