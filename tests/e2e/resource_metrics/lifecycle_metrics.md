# Lifecycle resource metrics E2E

This case validates the Cubelet resource metrics collector on a deployed Linux node. It uses the repository Python SDK through CubeAPI and CubeProxy; it does not create sandboxes through Cubelet directly.

The case covers fresh collection, controlled CPU and memory load, snapshot rollback, pause/resume, deletion, and series cleanup through `/v1/metrics/resource`. It requires the guest `Task.Stats` transport from PR #1090.

Set exactly one template source. To use an existing compatible template:

```bash
CUBE_TEMPLATE_ID=tpl-example tests/e2e/resource_metrics/lifecycle_metrics.sh \\
  --output /tmp/resource-metrics-result.json
```

To build and clean up a temporary template from an image:

```bash
CUBE_E2E_IMAGE=localhost:5000/sandbox-python:latest \\
  tests/e2e/resource_metrics/lifecycle_metrics.sh \\
  --output /tmp/resource-metrics-result.json
```

The runner requires the Python SDK dependencies (`httpx` and `requests`) or `uv`. It deletes the sandbox, snapshot, and temporary template by default. Use `--keep-resources` only for debugging.
