# Resource metrics E2E

These tests validate CubeSandbox CPU and memory metrics on a deployed Linux node. Sandbox lifecycle operations must use CubeAPI and CubeProxy; host-side assertions may use `cubecli` or the Cubelet metrics endpoint.

## Guest workload transport

`guest_workload_task_stats.sh` validates the runtime transport introduced by the guest workload metrics change:

1. Create a sandbox through CubeAPI with the Python SDK before running the script.
2. Run a controlled CPU and memory workload through CubeProxy.
3. Run the script on the selected Cubelet node with the sandbox ID.
4. Repeat the query for a sandbox restored from a snapshot or template.

The test is explicitly node-local. Set both `CUBECLI` and `CUBELET_ADDRESS` to opt in when running on a Cubelet node; cluster E2E runners leave these node-local settings unset, report `SKIP`, and exit successfully. The SDK/API-initiated lifecycle case remains in the Cubelet resource-metrics change.

```bash
CUBECLI=/usr/local/services/cubetoolbox/Cubelet/bin/cubecli \
  CUBELET_ADDRESS=/data/cubelet/cubelet.sock \
  tests/e2e/resource_metrics/guest_workload_task_stats.sh <sandbox-id>
```

The script checks that containerd successfully decodes `Task.Stats` as the canonical cgroup v1 metrics shape used by the locked shim contract, that CPU counters are nanoseconds, and that memory usage and limit are present. The focused CubeShim test separately asserts the exact `io.containerd.cgroups.v1.Metrics` type URL.

## Lifecycle metrics

The lifecycle and Prometheus E2E is added with the Cubelet resource metrics collector. It requires the guest workload transport above and validates fresh collection, CPU and memory load, snapshot rollback, pause/resume, deletion, and series cleanup through `/v1/metrics/resource`.
