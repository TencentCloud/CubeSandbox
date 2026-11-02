# Factory Scheduler Profiles: 4-Node Real-Cluster Load Test

> Branch: `feat/scheduler-eval-plugin` (go:embed factory scheduler profiles)
> Date: 2026-09-08 | Environment: 4 × 8C/15G x86_64 bare nodes, real KVM data plane, all components real

This report records a real-cluster load test of the **factory scheduler profile embedding**: CubeMaster's `conf.yaml` contains **no** `profiles` / `filter` / `score` scheduler configuration at all, and the three built-in profiles (`burst_balance` / `template_reuse` / `mixed_binpack`) are injected from the `go:embed`-ed factory YAML (`CubeMaster/pkg/base/config/scheduler_factory.yaml`). Unlike the earlier [control-plane A/B benchmark](/dev/scheduler-eval-benchmark) (real control plane + simulated data plane), this run is an end-to-end test on a **real 4-node cluster booting real KVM microVMs**.

## Topology and subject under test

```
agent_load.py ──HTTP──▶ CubeMaster(:8089, any15, built from this branch, v0.7.0-factoryprofiles)
(100 concurrent          │  conf.yaml has no filter/score/profiles → factory injection active
 agent sessions)         ▼ gRPC
                kubelet × 4 (any11/13/15/16, 8C/15G each, real KVM microVMs)
```

- Template `tpl-71ba92400dda437e9ea8ba19`: 500m CPU / 512Mi RAM / 512M writable disk, pre-distributed to 4/4 nodes via `tpl redo` (the `template_locality` guard is a hard filter requiring a local replica).
- Sandboxes are created via `POST /cube/sandbox` with a `workload` label for routing: `burst_balance` → spread, `template_reuse` → template locality, `default` and everything else → `mixed_binpack`.

## Workload model (simulating a real agent environment)

| Phase | Load | Purpose |
|---|---|---|
| 1. churn | 100 concurrent agent sessions × 360s; each session = create sandbox → work 20-60s → destroy → think 0.5-3s; label mix 50% default / 30% burst / 20% reuse | steady-state mixed tenancy |
| 2. burst wave | 80 simultaneous `burst_balance` creates | sudden work rush |
| 3. saturation probe | continuous default-label creates until 10 consecutive failures (cap 160) | find the capacity ceiling |
| 4. drain | destroy all live sandboxes | verify reclamation |
| 5. recovery | 20 mixed-label creates after the drain | verify the scheduler recovers |

## Results overview

**1179 scheduling operations, 0 failures**; peak concurrency **240 sandboxes** (counted at drain) without hitting the ceiling — the saturation probe exhausted its 160-create cap with the consecutive-failure counter still at 0.

Client-side latency (`POST /cube/sandbox` response time, i.e. scheduling decision + admission; microVMs then boot asynchronously):

| Phase | n | p50 | p95 | p99 | max |
|---|---|---|---|---|---|
| churn | 919 | 49ms | 55ms | 61ms | 133ms |
| burst wave (80 concurrent) | 80 | 168ms | 419ms | 840ms | 840ms |
| saturation probe (240 live) | 160 | 168ms | 230ms | 243ms | 694ms |
| recovery | 20 | 68ms | 84ms | 86ms | 86ms |

## Profile routing and placement distribution

The scheduler log shows 1211 `profile=` lines: `mixed_binpack` 645 / `burst_balance` 368 / `template_reuse` 198 (including internal retries; the ≈53%/30%/16% ratio matches the injected 50/30/20 mix). All three factory profiles genuinely participated in scheduling.

Successful ops by label × node (creates and destroys counted in pairs, reflecting placement shape; IP suffixes: any11=.6, any13=.28, any15=.228, any16=.31):

| Profile | any11 | any13 | any15 | any16 | Verdict |
|---|---|---|---|---|---|
| burst_balance | 105 | 60 | 28 | 73 | spread across all 4 nodes; per-node mvm counts drifted within 17–30 during churn |
| mixed_binpack | 93 | 156 | 186 | 189 | clear concentration; of the 160 saturation creates, 100 landed on any16 and 0 on any15 — a textbook binpack fill signature |
| template_reuse | 65 | 39 | 26 | 59 | all 4 nodes hold a local replica; the idlest among them wins |

Two design behaviors worth recording:

- **The binpack skew has a cause**: any11's disk usage was 21% (vs 8–9% on the others), so `resource_fit_score` scored it lower and default traffic avoided it — the scorer working as designed, not a routing bug.
- **`template_locality` is a hard guard**: a node only participates in `template_reuse` scheduling after the template is distributed to it via `tpl redo`; nodes without a local replica are filtered out entirely (4/4 distributed in this run, hence the even spread).

## Notes and known limitations

- The load driver mistakenly recorded the sandbox id into the host column during the burst-wave and recovery phases (a driver-side bug), so per-node distribution for those two phases was not directly tabulated; the distribution table above draws from the churn and saturation phases. Success-rate and latency conclusions are unaffected.
- 240 concurrent sandboxes did not reach the ceiling: at the 500m/512Mi spec a single node actually hosted 60+ sandboxes (~4× CPU overcommit), so the real cluster limit is higher than this probe's 160-create cap.
- The cluster was **left running** (dedicated to this project); the driver and raw data remain on any15 at `/root/agent_load.py` and `/root/load/{ops.csv,nodes.csv,driver.log}`. The load driver is one-off experiment code and is not merged into the repository.
