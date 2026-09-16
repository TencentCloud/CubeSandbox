# External HTTP Score Operator Path

Canonical short path: repository file `CubeMaster/examples/external-http-score/VERIFY.md`.

This page is the docs/dev entry that points at that VERIFY path. It does not change scheduler semantics. For YAML field details, see [CubeMaster Scheduler Configuration — External HTTP score plugin](../guide/cubemaster-scheduler-config#external-http-score-plugin).

## Boundaries

- Demo sidecar only; not production scoring.
- Live create latency must be measured with client timelines + sandbox ready, not simulator estimates.
- Formal binpack `VALID_NO_IMPROVEMENT` is unchanged by operator demos.
- **Candidate fan-out:** `external_http_score` POSTs the full post-filter candidate set from `selCtx.Nodes()`, whose size is gated by `scheduler.pre_select_num` (default `-1` = unlimited). Payload, sidecar CPU, and the "every candidate must return a finite score" contract all scale with that set. Keep `pre_select_num` bounded on large clusters before enabling this plugin. There is no negative cache; a circuit breaker is **enabled by default** (trips after `failure_threshold` consecutive failures, default 5) and can be disabled with `circuit_breaker.disable: true`.

## Related

- Live supplement conclusion packs (internal research): [topic1-live-supplement-20260914](./topic1-live-supplement-20260914/)
- Scheduler profile YAML examples: [Scheduler Profile Configuration Example](./scheduler-profile-config-example)
