# Network metrics E2E

This test validates CubeVS Prometheus metrics on a deployed Linux Cubelet node. It creates a sandbox through CubeAPI, sends traffic through CubeProxy, and scrapes Cubelet's `/v1/metrics/network` endpoint.

The test verifies:

1. sandbox, session, and SNAT metric sources are readable;
2. all sandbox counter families and SNAT gauge families are present;
3. SNAT capacity and unused-port gauges satisfy their basic invariants;
4. a new `sandbox_ip` series appears;
5. ingress and egress packet/byte counters increase after independently scheduled target traffic;
6. the tested sandbox's session count increases under its `sandbox_ip` and `packet_class`;
7. when traffic creates a `snat` or `hairpin` session, physical SNAT port occupancy increases;
8. deleting the target sandbox removes only its traffic metric series while a
   second sandbox on the same node remains exported with non-decreasing counters;
9. deleting the control sandbox independently removes its traffic metric series.

Run the test on the selected Cubelet node with an existing TAP-network template:

```bash
CUBE_TEMPLATE_ID=tpl-example \
CUBE_E2E_NODE=node-name-or-host-ip \
  tests/e2e/network_metrics/network_metrics.sh \
  --output /tmp/network-metrics-result.json
```

Useful overrides:

```bash
CUBE_NETWORK_METRICS_URL=http://127.0.0.1:9998/v1/metrics/network \
CUBE_NETWORK_METRICS_TARGET=https://example.com/ \
CUBE_TEMPLATE_ID=tpl-example \
CUBE_E2E_NODE=node-name-or-host-ip \
  tests/e2e/network_metrics/network_metrics.sh
```

`CUBE_E2E_NODE` pins both sandboxes to the Cubelet whose endpoint is scraped.
The runner requires either `uv` or Python with the SDK dependencies (`httpx`
and `requests`). It deletes both sandboxes by default. Use `--keep-resources`
only while debugging a failed run.

Run this case without concurrent sandbox creation on the same node. It identifies the tested sandbox by comparing the `sandbox_ip` label set before and after creation.
