# Topic1 Enhancement Ablation — Circuit Breaker (2026-09-14)

Reviewer-facing **conclusion pack** for live R011/R012 (E2 hot-path). Raw ledgers and orchestrator logs are **not** in this PR.

| Item | Value |
|---|---|
| Claim | Circuit breaker short-circuits repeated sidecar timeouts under `fail_open` |
| Live | R011 timeout arm + R012 healthy arm |
| Formal impact | Does **not** change `VALID_NO_IMPROVEMENT` / capacity-frontier adjudication |
| Code | Shipped on this PR (`external_http_score` circuit breaker + `failure_policy`) |

## Entry

- Results: [`R011_R012_RESULT.md`](./R011_R012_RESULT.md)
- Machine status: [`STATUS.json`](./STATUS.json)

## Boundaries

- No independent negative-cache module was under test; the short-circuit is **circuit open**.
- S2 (parallel/async score cache) and S3 (new Top-N / hysteresis) were **SKIP** this window (no code delta).
- Delayed arm is side evidence only; it does **not** overturn A1.
