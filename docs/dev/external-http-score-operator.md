# External HTTP Score Operator Path

Canonical short path: `CubeMaster/examples/external-http-score/VERIFY.md`.

This page is the docs/dev pointer for the productization sprint. It does not change scheduler semantics.

## Boundaries

- Demo sidecar only; not production scoring.
- Live create latency must be measured with client timelines + sandbox ready, not simulator estimates.
- Formal binpack `VALID_NO_IMPROVEMENT` is unchanged by operator demos.
