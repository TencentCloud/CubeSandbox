# Topic1 Live Supplement (2026-09-14)

Reviewer/maintainer-facing package. **No raw JSONL, task cards, or agent review drafts.**

## Provenance (read before STATUS PASS)

These arms were **not** re-run against this PR head. Numbers come from earlier lab binaries; treat `STATUS.json` PASS as mechanism evidence for those builds, **not** as validation of the scorer commit shipped here.

| Pack | Lab binary SHA (full in STATUS/ENVIRONMENT/RESTORE) | Metric family observed | Notes |
|---|---|---|---|
| `small-experiments/` | `edf40cb9…` (stock) | lab `cubemaster_scheduler_external_http_score_*` (not in-tree) | Fault arms **fail-closed** on that binary |
| `productization/` | `d5c6e2a8…` (staged candidate; see `PLAN_FROZEN.json`) restored to stock `edf40cb9…` | in-tree-style `cube_scheduler_external_http_score_*` | Timeout arm recorded fail-open creates |

In-tree this branch emits `cube_scheduler_external_http_score_outcomes_total` / `…_request_duration_seconds`.

## Product code (same branch)
- `CubeMaster/examples/external-http-score/` (+ `VERIFY.md`)
- `docs/dev/external-http-score-operator.md`

## Evidence indexes
| Package | Entry | Formal impact |
|---|---|---|
| Small experiments | [small-experiments/REVIEWER_SUPPLEMENT.md](./small-experiments/REVIEWER_SUPPLEMENT.md) | Does **not** override `VALID_NO_IMPROVEMENT` |
| Productization sprint | [productization/STATUS.json](./productization/STATUS.json) | Operator path + HTTP hot-path + signal/Filter; E5 probe `NO_IMPROVEMENT` |

## Claims you may rely on
- Operator enable/observe/fault/rollback path exists (example + VERIFY).
- HTTP scorer live cost is **measurable** on the productization lab binary (`d5c6e2a8…`), not a statistical ≤50 ms claim: control API P95 ≈590.8 ms vs healthy ≈449.4 ms (`healthy_api_p95_delta_ms: -141.45`, arm variance dominates); delayed/timeout arms vs healthy show injected cost (delayed P95 ≈623.4 ms, timeout ≈683.8 ms). See `productization/summaries/http_hot_path_latency.json`.
- Business signal can reorder Filter-legal candidates; Filter isolation observed (no Filter resurrection). Scorer-error degradation is binary-dependent (table above).
- Binpack can empty more nodes under mismatch fill; probe admission/frag not improved → still **NO_IMPROVEMENT**.

## Claims you must not infer
- That STATUS/summaries PASS validates **this** PR head's scorer binary (arms were not re-run here).
- Binpack/locality business quality SUPPORTED.
- Statistical significance, production SLO win, or a proven healthy-vs-control ≤50 ms scorer cost bound.
- Uniform fail-open across all lab binaries / packages.
- Override of formal CF01–CF06 / `TOPIC1_READY` adjudication.
