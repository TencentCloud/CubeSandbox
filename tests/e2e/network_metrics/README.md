# Network metrics E2E

This test validates CubeVS Prometheus metrics on a deployed Linux Cubelet node. It creates a sandbox through CubeAPI, sends traffic through CubeProxy, and scrapes Cubelet's `/v1/metrics/network` endpoint.

The test verifies:

1. sandbox and SNAT metric sources are readable;
2. a new `sandbox_ip` series appears;
3. ingress and egress packet/byte counters increase after controlled traffic;
4. deleting the sandbox removes its metric series.

Run the test on the selected Cubelet node with an existing TAP-network template:

```bash
CUBE_TEMPLATE_ID=tpl-example \
  tests/e2e/network_metrics/network_metrics.sh \
  --output /tmp/network-metrics-result.json
```

Useful overrides:

```bash
CUBE_NETWORK_METRICS_URL=http://127.0.0.1:9998/v1/metrics/network \
CUBE_NETWORK_METRICS_TARGET=https://example.com/ \
CUBE_TEMPLATE_ID=tpl-example \
  tests/e2e/network_metrics/network_metrics.sh
```

The runner requires either `uv` or Python with the SDK dependencies (`httpx` and `requests`). It deletes the sandbox by default. Use `--keep-resources` only while debugging a failed run.

Run this case without concurrent sandbox creation on the same node. It identifies the tested sandbox by comparing the `sandbox_ip` label set before and after creation.
