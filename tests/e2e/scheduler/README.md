# Scheduler profile routing e2e

Live checks that creating a sandbox through CubeAPI increments
`scheduler_attempts_total{profile,result="success"}` on CubeMaster's
`/metrics` endpoint.

The one-click package ships this scheduler behavior in
`configs/single-node/cubemaster.yaml`:

- Creates without `metadata.workload=cel_deny` use profile `default`. That
  profile does not run a CEL plugin. Filtering stays on the mandatory guards
  (`node_safety`, `cpu`, `mem`, `disk`, `template_locality`,
  `realtime_create_num`). `burst_balance` and `template_reuse` are not route
  labels in this package, so they also stay on `default`.
- `metadata.workload=cel_deny` uses profile `cel_deny`. Its CEL filter
  expression is `false`, so the create fails and the `deny_all` filter
  invocation counter increases.

One node cannot show whether `spread` and `mixed_binpack` place sandboxes
differently. Placement quality stays in `CubeMaster/tools/schedsim`. gRPC
plugins are not covered here.

## Run

From this directory, with the repo SDK on `PYTHONPATH` (the suite adds
`sdk/python` itself when that tree exists):

```bash
pip install pytest
CUBE_API_URL=http://127.0.0.1:3000 \
CUBE_TEMPLATE_ID=tpl-xxxxxxxxxxxxxxxxxxxxxxxx \
CUBEMASTER_METRICS_URL=http://127.0.0.1:8089/metrics \
pytest --run-e2e
```

`CUBEMASTER_METRICS_URL` is optional. When omitted, the host of
`CUBE_API_URL` is used with port `8089` and path `/metrics`.

`pytest` without `--run-e2e` collects the live cases and skips them. The
Prometheus parser tests are marked `framework` and run either way.

Do not point this suite at a cluster and then edit that cluster's
`conf.yaml` to force a mode. Run it against the configuration already in
effect.
