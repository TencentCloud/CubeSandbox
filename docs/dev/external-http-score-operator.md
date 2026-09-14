# External HTTP Score Operator Path

Canonical short path: repository file `CubeMaster/examples/external-http-score/VERIFY.md`.

This page is the docs/dev pointer for the productization operator journey. It does not change scheduler semantics. For YAML field details, see [CubeMaster Scheduler Configuration](../guide/cubemaster-scheduler-config#external-http-score).

## Boundaries

- Demo sidecar only; not production scoring.
- Live create latency must be measured with client timelines + sandbox ready, not simulator estimates.
- Formal binpack `VALID_NO_IMPROVEMENT` is unchanged by operator demos.

## Related

- Live supplement conclusion packs: [topic1-live-supplement-20260914](./topic1-live-supplement-20260914/)
- Scheduler profile YAML examples: [Scheduler Profile Configuration Example](./scheduler-profile-config-example)
