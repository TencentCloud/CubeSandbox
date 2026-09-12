# Scheduler Simulator Evaluation Report (schedsim)

> Date: 2026-09-09 ｜ Code: `9441033c` plus uncommitted module fixes (schedsim
> `--mode=performance`, cube-bench `mixed_spec` built-in template pool,
> schedsim `--template-preload` default 1.0 → 0.3)
> Environment: schedsim (pure Go, in-process scheduler core) on a 4 vCPU
> AMD Ryzen 7 7840HS / ~4 GB free RAM dev box, go1.25.7 linux/amd64.

This report is the simulator leg of the benchmark plan (仿真器大规模模拟): it
replays fixed request traces through the real scheduling core
(`scheduler.Select`, or its stage-timed sim replica in performance mode) over a
simulated homogeneous fleet and compares the legacy scheduler config against
the three scenario profiles (`burst_balance` / `template_reuse` /
`mixed_binpack`). It complements the real control-plane A/B report in
[scheduler-eval-benchmark](./scheduler-eval-benchmark) — see that report's
timeliness notice for what changed since it was written.

## Run metadata (all experiments)

| field | value |
| --- | --- |
| traces | cube-bench `--dump-trace`, seed 42: `burst` (500 req, Poisson 50/s, life U(10s,120s), tpl-burst 1C2G), `template_storm` (300 req, 30/s, U(30s,90s), single tpl-storm 2C4G), `mixed_spec` (400 req, 10/s, U(30s,300s), built-in pool 1C2G:2C4G:8C16G = 6:3:1) |
| fleet | 300 homogeneous nodes × 64 C / 128 GiB (§"500-node scale" uses 500) |
| overcommit | cpu_ratio 3.0 / mem_ratio 2.0 (except the overcommit tuning arm) |
| template_preload | 0.3 (30% of nodes hold a local replica of each template) |
| allow_non_local_template | true (cold nodes restore remotely and warm their cache, modeling the fake-cubelet behavior of the real A/B) |
| seeds / rounds | base seed 42, 5 rounds (seeds 42–46) per variant; tooling 95% CI = mean ± 1.96·s/√n (aligned with cube-bench compare) — the tables below predate that change and use the t(0.975, n−1) multiplier, so they are ~1.4× wider than what current tooling emits |
| variants | `legacy` = `cmd/schedsim/example.sim.yaml` (least-loaded top-1 scoring); `legacy_firstfit` = same minus the `score:` section (degenerates to near-first-fit, the pre-fix behavior documented in the real A/B report); plus the scenario profile matching the workload |
| mode | `quality` (virtual clock, quality metrics) and `performance` (per-stage wall-clock timing) |
| report metadata | every run's JSON report records `config.version` (git revision of the binary, `-dirty` when uncommitted) and `config.metric_sync_interval` (effective `scheduler.metric_update_timeout` in seconds) |

`legacy_firstfit` exists because the real A/B report's "legacy" ran with zero
scorers (its issue #3); the shipped sim example config already carries
least-loaded scoring, so without this variant the spread effect would be
invisible.

## Multi-seed A/B (quality mode, 300 nodes, 5 seeds)

Values are mean ± 95% CI across rounds. Δ is vs `legacy`.

### burst ↔ burst_balance

| metric | legacy | burst_balance | Δ | legacy_firstfit |
| --- | --- | --- | --- | --- |
| success_rate | 1.0000 ± 0 | 1.0000 ± 0 | +0.0% | 1.0000 ± 0 |
| sched_latency_p50_ms | 0.648 ± 0.135 | 0.704 ± 0.174 | +8.7% | 0.602 ± 0.122 |
| sched_latency_p99_ms | 2.512 ± 1.941 | 1.814 ± 0.633 | −27.8% | 2.975 ± 2.201 |
| load_cv_cpu | 1.286 ± 0.006 | 1.285 ± 0.023 | −0.0% | 1.561 ± 0.055 |
| jain_cpu | 0.5672 ± 0.0015 | 0.5659 ± 0.0096 | −0.2% | 0.4175 ± 0.0205 |
| herding_top1_share | 0.0040 ± 0 | 0.0040 ± 0 | +0.0% | 0.0132 ± 0.0033 |
| template_hit_rate | 0.594 ± 0.012 | 0.594 ± 0.012 | +0.0% | 0.668 ± 0.013 |
| active_nodes_avg | 187.2 ± 0.5 | 186.8 ± 2.9 | −0.2% | 156.3 ± 3.1 |

**Reading.** The `legacy` baseline already spreads via least-loaded top-1, so
`burst_balance` (spread top-3 over the same scorer family) is placement-neutral
against it — identical balance, hit rate and success. Against the true pre-fix
baseline (`legacy_firstfit`) the spread behavior is the large effect: Jain
0.418 → 0.567 (+36%), CV −21%, herding 1.3% → 0.4%, at zero success-rate cost.
Also note first-fit stacking's *higher* template hit rate (0.668 vs 0.594):
concentration is accidentally cache-friendly, the same mechanism that made
legacy look good in the real A/B.

### template_storm ↔ template_reuse

| metric | legacy | template_reuse | Δ | legacy_firstfit |
| --- | --- | --- | --- | --- |
| success_rate | 1.0000 ± 0 | 1.0000 ± 0 | +0.0% | 1.0000 ± 0 |
| sched_latency_p50_ms | 0.637 ± 0.132 | 0.743 ± 0.164 | +16.6% | 0.510 ± 0.140 |
| sched_latency_p99_ms | 1.889 ± 1.065 | 2.188 ± 0.939 | +15.8% | 1.483 ± 0.384 |
| load_cv_cpu | 1.431 ± 0 | 2.305 ± 0.034 | +61.1% | 1.997 ± 0.051 |
| jain_cpu | 0.609 ± 0 | 0.243 ± 0.009 | −60.1% | 0.342 ± 0.016 |
| herding_top1_share | 0.0033 ± 0 | 0.0127 ± 0.0019 | +280% | 0.0167 ± 0 |
| **template_hit_rate** | 0.324 ± 0.020 | **1.000 ± 0** | **+208.6%** | 0.573 ± 0.008 |
| active_nodes_avg | 182.8 ± 0 | 78.4 ± 2.7 | −57.1% | 126.7 ± 4.6 |

**Reading.** `template_reuse` achieves a perfect 1.000 template hit rate
(legacy scatters at the 30% preload ratio, 0.324; first-fit reaches only 0.573
because its concentration is template-blind). The trade-off is balance: load
concentrates on the ~90 replica-holding nodes (Jain 0.243, active nodes −57%),
which is the intended locality-vs-balance trade for a same-template storm.

### mixed_spec ↔ mixed_binpack

| metric | legacy | mixed_binpack | Δ | legacy_firstfit |
| --- | --- | --- | --- | --- |
| success_rate | 1.0000 ± 0 | 1.0000 ± 0 | +0.0% | 1.0000 ± 0 |
| sched_latency_p50_ms | 0.646 ± 0.146 | 0.740 ± 0.205 | +14.5% | 0.529 ± 0.135 |
| sched_latency_p99_ms | 1.558 ± 0.318 | 2.541 ± 0.971 | +63.0% | 2.107 ± 0.777 |
| load_cv_cpu | 2.468 ± 0.008 | 7.021 ± 0.008 | +184.4% | 2.719 ± 0.091 |
| jain_cpu | 0.311 ± 0.001 | 0.0225 ± 0.0001 | −92.8% | 0.230 ± 0.015 |
| herding_top1_share | 0.0055 ± 0.0014 | 0.160 ± 0 | +2809% | 0.014 ± 0.0017 |
| **template_hit_rate** | 0.382 ± 0.022 | **0.956 ± 0.012** | **+150.5%** | 0.468 ± 0.018 |
| **active_nodes_avg** | 165.5 ± 1.6 | **7.26 ± 0** | **−95.6%** | 131.9 ± 5.1 |

**Reading.** `mixed_binpack` consolidates the entire 400-request mixed fleet
onto ~7.3 of 300 nodes (peak demand ≈ 800 vCPU fits in 7 × 64C × 3.0
overcommit), keeps ~293 nodes completely empty for future large requests, and
reaches 0.956 template hit rate as a side effect of consolidation (warm nodes
stay warm). `fragmentation_ratio` stays 0 everywhere in this experiment because
the cluster is only ~2.3% allocated — there is nothing to strand; the metric
needs a near-saturated fleet to discriminate. Trade-offs: balance metrics are
deliberately sacrificed (that is what binpacking means), and herding rises to
16% because `selection: spread top_n=2` chooses among only 2 candidates whose
scores are usually dominated by the same packed nodes. P99 decision latency
rises +63% (1.56 → 2.54 ms), still single-digit milliseconds.

## Scheduling overhead (performance mode, 300 nodes, 5 seeds)

`--mode=performance` drives each decision through the sim-side staged replica
of `scheduler.Select` (`pkg/scheduler/sim/perf.go`, equivalence with the real
`Select` covered by `TestPerfModeMatchesQualityMode`) and times every stage on
the wall clock. Cross-round means below; `total` is the full decision.

The report also carries a process-overhead block (`perf.overhead` in the JSON,
extra rows in the compare markdown) covering the schedsim process's own cost
over the replay window (the same window `wall_seconds` measures):

- `cpu_seconds` — cumulative process CPU time (user + system, `/proc/self/stat`
  utime+stime) consumed during the replay;
- `avg_cpu_cores` — `cpu_seconds / wall_seconds`, the average number of CPU
  cores the process kept busy (×100 = percent of one core);
- `peak_rss_mib` — peak resident set size (`/proc/self/status` VmHWM) in MiB,
  a process-lifetime high-water mark;
- `heap_alloc_mib` / `heap_sys_mib` — Go heap in use / obtained from the OS
  (`runtime.ReadMemStats`) at the end of the replay, in MiB.

The procfs-derived fields are Linux-only; elsewhere they are omitted and the
Go heap gauges still report. Cross-round aggregation sums CPU seconds, takes
the max peak RSS, and averages the heap gauges.

| stage P50 (ms) | legacy | burst_balance | legacy | template_reuse | legacy | mixed_binpack |
| --- | --- | --- | --- | --- | --- | --- |
| workload | burst | burst | storm | storm | mixed | mixed |
| prefilter | 0.409 | 0.425 | 0.385 | 0.384 | 0.408 | 0.393 |
| guards | 0.0001 | 0.0932 | 0.0001 | 0.0823 | 0.0001 | 0.0836 |
| filter | 0.0612 | 0.0001 | 0.0601 | 0.0001 | 0.0610 | 0.0001 |
| score | 0.0828 | 0.0826 | 0.0820 | 0.1089 | 0.0893 | 0.0914 |
| pick | 0.0005 | 0.0001 | 0.0005 | 0.0001 | 0.0005 | 0.0001 |
| **total P50** | 0.597 | 0.666 | 0.575 | 0.650 | 0.608 | 0.624 |
| **total P95** | 1.515 | 1.675 | 1.079 | 1.215 | 1.111 | 1.262 |
| **total P99** | 3.192 | 3.459 | 1.955 | 1.695 | 1.621 | 1.923 |
| **throughput (decisions/s)** | 1113 | 1036 | 1181 | 1389 | 1175 | 1377 |

**Reading.**

- The plugin framework's own overhead is small and mostly a stage reshuffle:
  the profiled pipeline adds the mandatory-guards stage (~0.09 ms P50) but runs
  no optional filters (legacy spends ~0.06 ms there) — net Δtotal P50 is
  +0.02…0.07 ms (+3%…+13%).
- Prefilter (candidate enumeration over 300 nodes + snapshot freeze) dominates
  at ~65% of the decision cost; scoring is ~15%. Neither grows with the
  profiles.
- Throughput differences are second-order and partly *favor* the profiles on
  storm/mixed (+17%): locality/binpack filters shrink the candidate set before
  scoring, so there is less per-node scoring work.
- These are single-process, zero-queue numbers on a 4 vCPU box; they quantify
  framework overhead, not production latency budgets.

## 500-node scale check (quality mode, 3 rounds, seeds 42–44)

Same traces, fleet raised to 500 nodes. Direction and magnitude hold from the
300-node runs (CIs widen on latency at n=3; balance/allocation metrics are
near-deterministic):

| workload | metric | legacy | profile | reading |
| --- | --- | --- | --- | --- |
| burst | jain_cpu / active_nodes | 0.514 / 256.8 | 0.514 / 256.8 | burst_balance placement-neutral vs scored legacy at scale too |
| storm | template_hit_rate | 0.308 ± 0.104 | **1.000 ± 0** | perfect locality holds at 500 nodes |
| storm | active_nodes_avg | 182.8 | 112.4 ± 12.9 | load confined to replica nodes (~150 preloaded) |
| mixed | active_nodes_avg | 197.4 | **7.25 ± 0.05** | consolidation is scale-invariant (demand-bound, not fleet-bound) |
| mixed | template_hit_rate | 0.296 ± 0.031 | 0.961 ± 0.019 | hit-rate gain holds |
| all | sched_latency_p50_ms | 0.89–0.93 | 0.98–1.13 | decision cost grows sub-linearly (1.67× nodes → ~1.5× P50), no cliff at 500 nodes |
| all | success_rate | 1.0 | 1.0 | no failures at scale |

## Single-variable tuning (mixed_spec trace, 300 nodes, 5 seeds)

Both arms vary one knob of `mixed_binpack` against the shipped profile
(`resource_fit_score:real_time_weighted_average` = 0.7:0.3, cpu_ratio 3.0).

### Score weights (fit : realtime)

| weights | active_nodes_avg | jain_cpu | herding_top1 | template_hit | verdict |
| --- | --- | --- | --- | --- | --- |
| 0.5 : 0.5 | 166.0 ± 1.6 | 0.311 | 0.006 | 0.378 | collapses into a balancer — packing lost |
| **0.7 : 0.3 (shipped)** | 7.24 ± 0.02 | 0.0225 | 0.160 | 0.956 | packs; reference point |
| 0.9 : 0.1 | 7.25 ± 0.02 | 0.0225 | 0.160 | 0.954 | identical to shipped |

There is a **phase boundary between 0.5:0.5 and 0.7:0.3**: once the balancing
scorer reaches parity, the placement flips from packed to balanced (active
nodes 7 → 166). Above the boundary the result is insensitive to the exact
ratio. Recommendation: keep 0.7:0.3; do not lower the fit weight toward 0.5.

### Overcommit (cpu_ratio; mem_ratio fixed at 2.0)

| cpu_ratio | active_nodes_avg | jain_cpu | herding_top1 | template_hit | fragmentation | success |
| --- | --- | --- | --- | --- | --- | --- |
| **3.0 (shipped)** | 7.24 ± 0.02 | 0.0224 | 0.160 | 0.959 ± 0.008 | 0 | 1.0 |
| 1.5 | 9.01 ± 0.04 | 0.0271 | 0.128 | 0.956 ± 0.010 | 0.0001 | 1.0 |
| 1.0 | 13.18 ± 0 | 0.0394 | 0.090 | 0.919 ± 0.006 | 0.0003 | 1.0 |

Lower overcommit **reduces** consolidation (7.2 → 9.0 → 13.2 active nodes):
with less effective capacity per node, packed nodes fill up sooner and the
placement spills to fresh nodes. Herding eases accordingly and fragmentation
appears only marginally (≤0.03%) because utilization is low. No success-rate
cost at any setting in this workload. Recommendation: keep 3.0 for maximal
consolidation; if node-level burst risk matters more than empty-node count,
1.5 buys 2× herding relief for ~2 extra active nodes.

## template_reuse weight comparison (300 nodes, 5 seeds, seeds 42–46)

> Added 2026-09-10 on the merged baseline (`fix2-template-reuse` worktree).
> Arbitrates the shipped `template_reuse` weights (image_score /
> template_local_pressure / real_time_weighted_average = **0.7/0.2/0.3**)
> against the rejected-PR proposal **0.1/0/0.9** ("let realtime load dominate,
> avoid persistent hotspots on replica nodes"). Variants: `baseline` =
> 0.7/0.2/0.3, `rt-dominated` = 0.1/−/0.9 (pressure scorer removed), `mid` =
> 0.5/0.2/0.5, `rt-hot` = 0.7/0.2/0.9. Values are mean ± 95% CI across rounds.

### Standard template_storm trace (300 req, 30/s, U(30s,90s))

| metric | baseline 0.7/0.2/0.3 | rt-dominated 0.1/0/0.9 | rt-hot 0.7/0.2/0.9 |
| --- | --- | --- | --- |
| success_rate | 1.0000 ± 0 | 1.0000 ± 0 | 1.0000 ± 0 |
| template_hit_rate | 1.000 ± 0 | 1.000 ± 0 | 1.000 ± 0 |
| jain_cpu | 0.245 ± 0.015 | 0.243 ± 0.014 | 0.243 ± 0.015 |
| load_cv_cpu | 2.306 ± 0.066 | 2.303 ± 0.063 | 2.319 ± 0.085 |
| herding_top1_share | 0.0127 ± 0.0019 | 0.0127 ± 0.0019 | 0.0127 ± 0.0019 |
| active_nodes_avg | 79.0 ± 4.5 | 78.5 ± 3.9 | 78.5 ± 4.5 |
| sched_latency_p50_ms | 0.601 ± 0.179 | 0.581 ± 0.193 | 0.610 ± 0.197 |

At this load the variants are indistinguishable: locality is perfect for all
three, and balance/herding differ only within noise. The realtime score deltas
between nodes are too small to move placement at any weight, so the claimed
hotspot-relief benefit of 0.1/0/0.9 does not exist in the nominal regime.

### Hot template_storm trace (1500 req, ~62/s, lifetime U(60s,180s))

To make the weights discriminate, the storm was pushed until replica-holding
nodes ran hot (avg concurrency ≈ 7.5k sandboxes; the ~90 preload nodes carry
~86% of effective CPU while cluster-wide allocation stays ~9%):

| metric | baseline 0.7/0.2/0.3 | rt-dominated 0.1/0/0.9 | mid 0.5/0.2/0.5 | rt-hot 0.7/0.2/0.9 |
| --- | --- | --- | --- | --- |
| success_rate | 1.0000 ± 0 | 1.0000 ± 0 | 1.0000 ± 0 | 1.0000 ± 0 |
| **template_hit_rate** | **1.000 ± 0** | **0.953 ± 0.004** | 1.000 ± 0 | 1.000 ± 0 |
| jain_cpu | 0.286 ± 0.018 | 0.442 ± 0.002 | 0.283 ± 0.016 | 0.285 ± 0.017 |
| load_cv_cpu | 1.833 ± 0.065 | 1.471 ± 0.010 | 1.846 ± 0.057 | 1.831 ± 0.062 |
| herding_top1_share | 0.0107 ± 0.0006 | 0.0060 ± 0 | 0.0107 ± 0.0006 | 0.0107 ± 0.0006 |
| active_nodes_avg | 89.9 ± 5.3 | 142.0 ± 0.8 | 89.6 ± 5.2 | 89.7 ± 5.1 |
| sched_latency_p50_ms | 0.593 ± 0.166 | 0.572 ± 0.160 | 0.589 ± 0.171 | 0.597 ± 0.169 |

**Reading.** There is a phase boundary between image weight 0.1 and 0.5: while
the boolean image score (replica = 100, none = 0) dominates the realtime deltas,
placement is identical for every locality-preserving setting — 0.5/0.2/0.5 and
0.7/0.2/0.9 change *nothing* versus shipped. Below the boundary,
`rt-dominated`'s balance gains (Jain +55%, CV −20%, herding 1.07% → 0.60%) come
entirely from breaking locality: hit rate drops 1.000 → 0.953 (4.7% of creates
pay a remote template restore) and placement spills onto 58% more nodes — i.e.
the profile degenerates toward legacy least-loaded, which is what the default /
burst_balance profiles are already for.

**Decision: keep 0.7/0.2/0.3.** The strategy contract for TemplateReuse ("pick
among template-runnable nodes first, then spread by same-template create
pressure") makes locality the primary objective; the sim shows the 0.1/0/0.9
proposal only trades locality away, and shows no benefit for it in the nominal
regime. Spreading *within* replica nodes is the job of
`template_local_pressure` + top-3 spread. Caveat: the sim engine never drives
the in-flight create counters (`localcache.IncrNodeTemplateCreate`), so
`template_local_pressure` scores a constant 100 here and its anti-herding
benefit is asserted from code semantics, not from this experiment — the sim
arbitrates only the image-vs-realtime axis.

## Trade-offs and caveats

- **Locality vs balance is a real, quantified trade**: template_reuse buys a
  1.000 hit rate with Jain 0.243; mixed_binpack buys 96% fewer active nodes
  with Jain 0.0225 and 16% herding. Neither is "free"; pick per workload.
- **fragmentation_ratio is 0 in all runs**: at 2–4% cluster allocation nothing
  is stranded. A near-saturation trace would be needed to make this metric
  discriminate between policies.
- **Latency CIs are wide at n=5 on a shared 4 vCPU box** (P99 ±40–100%);
  quality-mode P50 differences under ±10% should not be over-read. Balance,
  hit-rate and consolidation metrics are near-deterministic (CI ≪ 1%) and are
  the reliable signals.
- **CI convention change**: schedsim compare used to render the sample
  standard deviation, and this report's tables were computed with the
  t(0.975, n−1) multiplier; the tooling now emits mean ± 1.96·s/√n (the
  cube-bench compare convention), so freshly generated reports show ~1.4×
  tighter intervals at n=5 than the tables here.
- Performance mode measures the sim-side staged replica of `Select`. Its
  per-request total matches the quality-mode `sched_latency` within ~10%
  (same pipeline, minus the metrics hook), and placement-quality summaries of
  the two modes agree within tie-break noise.
- The sim legacy baseline (`example.sim.yaml`) carries least-loaded scoring;
  the zero-scorer first-fit stacking from the original real A/B is reproduced
  here as `legacy_firstfit`.

## Reproducing

```bash
# traces (cube-bench writes the trace before the run starts; kill it once the file exists)
cd examples/cube-bench && go run . --workload mixed_spec --dry-run --no-tui --seed 42 --dump-trace /tmp/mixed.trace.json
# (burst: --templates "tpl-burst:1:1000:2048"; storm: --workload template_storm --templates "tpl-storm:1:2000:4096")

# A/B/C compare, 5 seeds
cd CubeMaster && go build -o /tmp/schedsim ./cmd/schedsim
/tmp/schedsim --compare legacy=cmd/schedsim/example.sim.yaml,\
mixed_binpack=cmd/schedsim/mixed_binpack.profiles.sim.yaml,\
legacy_firstfit=<example.sim.yaml without the score: section> \
  --trace /tmp/mixed.trace.json --nodes 300 --rounds 5 --seed 42 \
  --allow-non-local-template=true -o compare.md --out-dir ./sim-out

# performance mode (per-stage timing + throughput)
/tmp/schedsim --mode=performance --compare legacy=...,burst_balance=... \
  --trace /tmp/burst.trace.json --nodes 300 --rounds 5 --seed 42 \
  --allow-non-local-template=true -o perf.md

# template_reuse weight comparison: hot storm trace + 4 weight variants
cd examples/cube-bench && go run . --workload template_storm --total 1500 \
  --rate 60 --lifetime "60,180" --dry-run --no-tui --seed 42 \
  --templates "tpl-storm:1:2000:4096" --dump-trace /tmp/storm-hot.trace.json
cd ../../CubeMaster
/tmp/schedsim --compare baseline=cmd/schedsim/template_reuse.profiles.sim.yaml,\
rt-dominated=<same with image 0.1, no template_local_pressure, realtime 0.9>,\
mid=<same with 0.5/0.2/0.5>,rt-hot=<same with 0.7/0.2/0.9> \
  --trace /tmp/storm-hot.trace.json --nodes 300 --template-preload 0.3 \
  --rounds 5 --seed 42 --allow-non-local-template=true -o weights.md
```
