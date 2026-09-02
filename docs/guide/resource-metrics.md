# Sandbox Resource Metrics

Cubelet exposes CPU and memory metrics for running Cube sandboxes. One-click installations enable this capability by default at:

```text
http://<cubelet-node>:9998/v1/metrics/resource
```

This endpoint is separate from Cubelet's generic `/v1/metrics` endpoint. The existing containerd cgroup monitor remains enabled and may continue to expose its generic `container_*` families under `/v1/metrics`; the Cube-native sandbox metrics below are collected and exported independently under `/v1/metrics/resource`.

Cubelet periodically collects resource data for the selected scopes in the background and caches the latest results in memory. A Prometheus scrape reads only this cache and does not synchronously contact every sandbox. The scrape therefore does not trigger runtime RPCs proportional to the number of sandboxes, although response size and serialization cost still grow with the exported series count.

::: warning Network access
Cubelet's HTTP service has no built-in authentication or TLS.

Expose port `9998` only on a trusted management network or restrict access with firewall or security-group rules. See [Network Hardening](./network-hardening.md).
:::

## Prerequisites

- Resource metrics cover running Cube sandboxes. A paused, stopped, or deleted sandbox does not export current series.
- The current release supports the single-container sandbox model, where the primary workload uses `container_id == sandbox_id`. Per-container breakdown for multi-container sandboxes is not yet available.
- `guest_workload` requires a `cube-agent` that supports resource metrics capability version `1` on a unified cgroup v2 hierarchy.
- `host_sandbox` depends only on the host sandbox cgroup, has broader compatibility, and is the default collected and exported scope.

## Accounting scopes

Resource metrics provide two independent accounting views:

| Scope | Accounting domain | Typical use |
| --- | --- | --- |
| `host_sandbox` | The sandbox cgroup accounted by the host kernel, including CubeShim, the VMM, and other charged host-side resources. | Host-side sandbox accounting and basic operational monitoring. |
| `guest_workload` | The workload container cgroup accounted by the guest kernel, excluding management processes such as `cube-agent`. | User-code CPU utilization, memory utilization, and memory-limit monitoring. |
| `all` | Exports both metric families. | Comparing workload usage with runtime overhead. |

::: tip Choosing a scope
- Use the default `host_sandbox` scope for basic node-side monitoring.
- Use `guest_workload` when workload-level utilization or memory pressure is required.
- Use `all` when both accounting domains need to be compared.
:::

The two scopes represent different accounting domains and must not be added into a generic total.

`host_sandbox` memory is the host cgroup's memory charge. Shared snapshot pages and copy-on-write behavior affect this value, so it is not the guest's logical working set or a proportional physical-memory estimate. VMM overhead, shared pages, and private COW pages can make `host_sandbox` and `guest_workload` memory differ substantially.

## How collection works

All collection is initiated by Cubelet on the host. A sandbox never pushes metrics to the host.

### `host_sandbox`

Cubelet reads the host sandbox cgroup directly. This view does not depend on the `cube-agent` version captured in a template and does not use the guest metrics epoch.

Host accounting follows the node's cgroup hierarchy and retains the project's existing cgroup v1 and cgroup v2 handling.

### `guest_workload`

Cubelet calls containerd `Task.Stats`, CubeShim proxies the request, and `cube-agent StatsContainer` reads the workload container cgroup inside the sandbox.

This view requires:

- a unified cgroup v2 hierarchy inside the sandbox;
- `cube-agent` resource metrics capability version `1`;
- a complete workload response through CubeShim and `Task.Stats`;
- Cubelet lifecycle state for metrics epochs and counter baselines.

CubeShim enables the unified cgroup v2 hierarchy for `cube-agent` with `agent.unified_cgroup_hierarchy=true` when starting a sandbox.

The `cube-agent` captured by an old template may not provide these capabilities. Such a sandbox does not export `guest_workload`, but it can still export `host_sandbox`.

## Cubelet configuration

One-click installations place the Cubelet configuration at:

```text
/usr/local/services/cubetoolbox/Cubelet/config/config.toml
```

The default resource metrics configuration is:

```toml
[plugins."io.cubelet.internal.v1.resource-metrics"]
  enabled = true
  collection_interval = "5s"
  request_timeout = "2s"
  max_concurrent_requests = 8
  stale_after = "15s"
  export_scopes = ["host_sandbox"]
```

### Configuration settings

| Setting | Meaning |
| --- | --- |
| `enabled` | Enables resource collection. When `false`, the endpoint still returns HTTP 200 but exports no sandbox resource series. |
| `collection_interval` | Target interval for refreshing Cubelet's in-memory cache. It controls `Task.Stats` RPC and host cgroup read frequency independently of Prometheus scraping. |
| `request_timeout` | Timeout for one `Task.Stats` request or host cgroup read. |
| `max_concurrent_requests` | Maximum concurrent requests for one collector. `guest_workload` and `host_sandbox` each use this limit, and their concurrency limits are independent. |
| `stale_after` | Stops exporting a scope when its last successful sample becomes older than this duration. It must not be shorter than `collection_interval` and should allow for scheduling delay and transient collection failures. |
| `export_scopes` | Controls which scopes are collected and exposed. Supported values are `["host_sandbox"]`, `["guest_workload"]`, and `["all"]`; the default is `["host_sandbox"]`. An unselected collector is not started; `["all"]` starts both collectors. |

During a transient collection failure, Cubelet continues exporting the latest successful sample. The scope disappears only after the sample becomes older than `stale_after`.

Invalid configuration prevents Cubelet from starting. Examples include `stale_after` being shorter than `collection_interval` or `export_scopes` containing an unsupported value.

Restart Cubelet after editing the file:

```bash
sudo systemctl restart cube-sandbox-cubelet.service
```

## Prometheus scrape configuration

Use a separate scrape job for sandbox resource metrics:

```yaml
scrape_configs:
  - job_name: cubesandbox-resource
    scrape_interval: 30s
    scrape_timeout: 10s
    metrics_path: /v1/metrics/resource
    static_configs:
      - targets:
          - <compute-node-ip>:9998
```

Replace the target with each Cubelet node's address on the trusted management network.

A separate job allows scrape intervals, timeouts, and `metric_relabel_configs` to be tuned without changing Cubelet's generic metrics job.

The resource endpoint processes at most two scrapes concurrently. Requests above this limit receive HTTP 503 so overlapping large responses cannot consume unbounded Cubelet CPU and memory. A normal single Prometheus scrape job does not hit this limit. If 503 responses occur, check for duplicate Prometheus jobs or concurrent manual scrapes of the same node.

### Verify the endpoint

Confirm that Cubelet is running and that the node has at least one sandbox in the `Up` state:

```bash
sudo systemctl is-active cube-sandbox-cubelet.service
/usr/local/services/cubetoolbox/Cubelet/bin/cubecli cubebox ls -a --no-trunc
```

After at least one collection interval, run this command on the Cubelet node:

```bash
curl -fsS http://127.0.0.1:9998/v1/metrics/resource | \
  grep '^cubesandbox_'
```

The default configuration exports `cubesandbox_host_sandbox_*` metrics.

To export `guest_workload`:

1. Set `export_scopes` to `["guest_workload"]` or `["all"]`.
2. Restart Cubelet.
3. Confirm that the sandbox's `cube-agent` supports resource metrics capability version `1`.

HTTP 200 with an empty response does not necessarily indicate a failure. The endpoint is empty when the node has no running sandbox, the plugin is disabled, or Cubelet has not completed its first sample.

## Base metric families

Cubelet exports only counters and gauges. Prometheus queries derive CPU cores used and CPU or memory utilization percentages.

CPU and memory limit metrics are exported only when the cgroup has a finite limit. Cubelet does not substitute `0` or a large sentinel for an unlimited value. Current memory remains available whenever the corresponding scope is available.

### `host_sandbox` metrics

`host_sandbox` metrics have only the `sandbox_id` label.

Cumulative metrics contain usage from the current sandbox's assignment to the host cgroup. They exclude history left by a previous sandbox in the same reusable cgroup pool slot.

| Metric | Type | Unit and meaning |
| --- | --- | --- |
| `cubesandbox_host_sandbox_cpu_usage_seconds_total` | Counter | Total host CPU seconds used by the sandbox. |
| `cubesandbox_host_sandbox_cpu_user_seconds_total` | Counter | Total user-space CPU seconds. |
| `cubesandbox_host_sandbox_cpu_system_seconds_total` | Counter | Total system-space CPU seconds. |
| `cubesandbox_host_sandbox_cpu_throttled_seconds_total` | Counter | Total CPU throttling time in seconds. |
| `cubesandbox_host_sandbox_cpu_periods_total` | Counter | Total CPU scheduling periods. |
| `cubesandbox_host_sandbox_cpu_throttled_periods_total` | Counter | Total throttled CPU scheduling periods. |
| `cubesandbox_host_sandbox_cpu_limit_cores` | Gauge | Finite host sandbox cgroup CPU limit in cores. |
| `cubesandbox_host_sandbox_memory_current_bytes` | Gauge | Current bytes charged to the host sandbox cgroup. |
| `cubesandbox_host_sandbox_memory_limit_bytes` | Gauge | Finite host sandbox cgroup memory limit in bytes. |
| `cubesandbox_host_sandbox_memory_failures_total` | Counter | Memory-limit failures during the current sandbox's cgroup assignment. |

### `guest_workload` metrics

`guest_workload` metrics have these labels:

- `sandbox_id`
- `container_id`

Cumulative metrics are scoped to the current metrics epoch and exclude history inherited from a template, clone, or rollback. The lifecycle semantics are described below.

| Metric | Type | Unit and meaning |
| --- | --- | --- |
| `cubesandbox_guest_workload_cpu_usage_seconds_total` | Counter | Total CPU seconds. |
| `cubesandbox_guest_workload_cpu_user_seconds_total` | Counter | Total user-space CPU seconds. |
| `cubesandbox_guest_workload_cpu_system_seconds_total` | Counter | Total system-space CPU seconds. |
| `cubesandbox_guest_workload_cpu_throttled_seconds_total` | Counter | Total CPU throttling time in seconds. |
| `cubesandbox_guest_workload_cpu_periods_total` | Counter | Total CPU scheduling periods. |
| `cubesandbox_guest_workload_cpu_throttled_periods_total` | Counter | Total throttled CPU scheduling periods. |
| `cubesandbox_guest_workload_cpu_limit_cores` | Gauge | Finite workload CPU limit in cores. |
| `cubesandbox_guest_workload_memory_current_bytes` | Gauge | Current bytes charged to the workload cgroup. |
| `cubesandbox_guest_workload_memory_limit_bytes` | Gauge | Finite workload memory limit in bytes. |
| `cubesandbox_guest_workload_memory_failures_total` | Counter | Memory-limit failures during the current metrics epoch. |
| `cubesandbox_guest_workload_metrics_epoch` | Gauge | Current metrics epoch generation. |
| `cubesandbox_guest_workload_metrics_epoch_start_time_seconds` | Gauge | Metrics epoch start time as Unix seconds. |

## PromQL examples

### Host-side CPU cores used

This query returns the sandbox's average host CPU cores used, including VMM and CubeShim overhead:

```promql
rate(cubesandbox_host_sandbox_cpu_usage_seconds_total[5m])
```

### Host-side CPU utilization

This query calculates utilization relative to the finite CPU limit configured on the host sandbox cgroup:

```promql
100 *
rate(cubesandbox_host_sandbox_cpu_usage_seconds_total[5m])
/
cubesandbox_host_sandbox_cpu_limit_cores
```

Without a finite CPU limit, CPU cores used remain available but there is no well-defined denominator for a utilization percentage.

### Host-side memory charge

```promql
cubesandbox_host_sandbox_memory_current_bytes
```

This value shows the memory currently charged to the sandbox's host cgroup. It must not be interpreted as the guest's logical working set.

Shared snapshot pages can make it lower than `guest_workload` current memory, while VMM, CubeShim, and private COW pages can make it higher.

### Host-side memory utilization

```promql
100 *
cubesandbox_host_sandbox_memory_current_bytes
/
cubesandbox_host_sandbox_memory_limit_bytes
```

When the host sandbox cgroup has no finite memory limit, Cubelet omits `cubesandbox_host_sandbox_memory_limit_bytes` and a limit-relative percentage cannot be calculated.

### Workload CPU cores used

This query returns the workload's average CPU cores used during the selected window:

```promql
rate(cubesandbox_guest_workload_cpu_usage_seconds_total[5m])
```

A result of `0.5` means the workload used about half a CPU core on average during the query window.

### Workload CPU utilization

This query calculates utilization relative to the workload's finite CPU limit:

```promql
100 *
rate(cubesandbox_guest_workload_cpu_usage_seconds_total[5m])
/
cubesandbox_guest_workload_cpu_limit_cores
```

Without a finite workload CPU limit, CPU cores used remain available but a limit-relative percentage cannot be calculated.

### Workload memory utilization

```promql
100 *
cubesandbox_guest_workload_memory_current_bytes
/
cubesandbox_guest_workload_memory_limit_bytes
```

When the workload has no finite memory limit, Cubelet omits `cubesandbox_guest_workload_memory_limit_bytes` and a limit-relative percentage cannot be calculated.

### Workload memory-limit failures

This query returns memory-limit failures during the last five minutes:

```promql
increase(cubesandbox_guest_workload_memory_failures_total[5m])
```

### Detect metrics epoch changes

This query returns the number of epoch changes for existing series during the last five minutes and can mark rollback or other accounting-window changes:

```promql
changes(cubesandbox_guest_workload_metrics_epoch[5m])
```

## Lifecycle semantics

This section describes the `guest_workload` metrics epoch and how both scopes behave during pause, rollback, and deletion.

### Why `guest_workload` needs a metrics epoch

CPU time and memory-limit failures come from cumulative cgroup counters. Creating a template or snapshot preserves these counters with the sandbox state. A sandbox created from inherited state receives the existing values, and rollback can move raw counters back to their snapshot values.

Exporting raw counters directly under `sandbox_id` would include CPU and memory-failure history from template creation in every new sandbox. Rollback could also make a counter decrease without identifying why.

Cubelet therefore creates a metrics epoch for each new `guest_workload` state and uses the first successful sample as its baseline:

```text
exported cumulative value = current raw value - current epoch baseline
```

This removes inherited history and represents post-rollback data as a new accounting window. Current memory is a point-in-time value and is not baseline-subtracted.

### `guest_workload` lifecycle

| Lifecycle event | Metric behavior |
| --- | --- |
| Fresh creation, creation from a template or snapshot, clone, or workload recreation | Creates a new metrics epoch and uses the first successful sample to remove inherited cumulative history. |
| Snapshot creation or template commit | Keeps the current metrics epoch. |
| Rollback | Stops exporting during rollback, then creates a new metrics epoch whose cumulative metrics restart from `0`. If runtime restore fails after dispatch, the new epoch stays prepared and `guest_workload` remains unavailable until a later rollback succeeds or the sandbox is deleted and recreated. |
| Pause | Keeps the current metrics epoch but stops exporting instead of emitting `0`. |
| Resume | Continues the metrics epoch that existed before pause. |
| Delete | Removes cached samples and metric series. |
| Cubelet restart | Restores the persisted metrics epoch and baseline instead of recalculating the existing window. |

If a lifecycle metadata failure temporarily leaves a running sandbox without a persisted fresh metrics epoch, the `guest_workload` sampler recreates and persists the missing pending epoch before collecting it. A failed recovery remains unavailable and is retried on a later collection cycle.

Prometheus does not understand Cubelet's metrics epoch semantics. `rate()` and `increase()` treat an epoch transition as a counter reset only when the cumulative value actually decreases.

Use these metrics when an application must reliably identify rollback or another accounting-window change:

- `cubesandbox_guest_workload_metrics_epoch`
- `cubesandbox_guest_workload_metrics_epoch_start_time_seconds`

The current release does not export an exact memory peak scoped to a metrics epoch. Current memory remains the point value at collection time.

### `host_sandbox` lifecycle

`host_sandbox` does not use the guest metrics epoch. It follows the host cgroup assignment:

- When a new sandbox receives a reusable host cgroup pool slot, Cubelet reads and persists the assignment baseline before attaching the sandbox process.
- A sandbox that already existed when Cubelet was upgraded may not have this persisted field; Cubelet establishes its compatibility baseline from the first successful sample after upgrade.
- Cubelet retries transient assignment-counter read failures before the sandbox process is attached. If all attempts fail, sandbox creation continues but `host_sandbox` remains unavailable for that sandbox rather than exporting an incomplete cumulative window. Recreate the sandbox after correcting a persistent host cgroup read failure.
- A new sandbox does not inherit CPU or memory-failure history from the previous occupant of the same slot.
- Snapshot creation and rollback do not reset the baseline because they do not replace the host sandbox process or cgroup assignment.
- Pause stops exporting; resume continues the previous cumulative values.
- Deletion removes the corresponding series.

::: tip Collection and scrape tuning
`collection_interval` controls `Task.Stats` RPC and host cgroup read frequency, while Prometheus `scrape_interval` controls HTTP scrapes and sample ingestion. On nodes with many sandboxes, or when Prometheus scrapes only every few minutes, increase `collection_interval` and `stale_after` together.

The resource endpoint stores only the latest sample and no history. Short memory peaks between Prometheus scrapes are not retained. A CPU rate window should contain multiple samples successfully written to Prometheus.

Neither interval reduces the number of active series. With `export_scopes = ["all"]`, one fully populated single-container sandbox exports at most 22 series. To reduce series count, change `export_scopes` or use Prometheus `metric_relabel_configs`.
:::

## Upgrading old templates and snapshots

A snapshot template captures the guest processes and their memory state. Upgrading Cubelet, CubeShim, or guest image files on the node does not replace the `cube-agent` captured in an old template's memory snapshot.

An old `cube-agent` may lack the required cgroup v2 accounting semantics and resource metrics capability version `1`. Such a sandbox does not export `guest_workload`, but it can still export `host_sandbox`.

### Image-built templates

Run template `redo` for an image-built template.

`redo` uses the node's current guest image and `cube-agent` to rebuild a replica for the same template ID. After the task completes, newly created sandboxes can export `guest_workload` metrics.

After a node upgrade, templates marked **Needs rebuild** in the Dashboard must be rebuilt (click **Rebuild Template**) before you create sandboxes from them. Other templates can still be used directly — creation looks up the recorded versions on the node (or downloads them from the component warehouse). Click **Rebuild Template** on an already-usable template only when you want the node's current guest image / `cube-agent` so new sandboxes can export `guest_workload` metrics.

### User snapshots and existing sandboxes

- For a user snapshot created from a running sandbox, create a sandbox from a compatible new template and then create a new snapshot.
- A running or paused old sandbox retains its in-memory `cube-agent` and must be deleted and recreated to complete the upgrade.

The one-click bundle writes the reviewed `cube-agent` into the guest image. As long as the template is `READY` and is not marked **Needs rebuild**, you can still create sandboxes after an upgrade.

At runtime, CubeShim also validates the resource metrics capability version returned by `StatsContainer`, preventing all-zero or incomplete `guest_workload` data from being accepted as valid metrics.

## Troubleshooting

| Symptom | Common cause and action |
| --- | --- |
| Cannot connect to port `9998` | Confirm that `cube-sandbox-cubelet.service` is running, then check the listener address, firewall, and security-group rules. |
| HTTP 200 but no `cubesandbox_*` metrics | Confirm `enabled = true`, verify that the node has a sandbox in the `Up` state, and wait at least one `collection_interval`. Paused sandboxes do not export metrics. |
| `guest_workload` is configured but no guest metrics appear | Confirm that `export_scopes` is `["guest_workload"]` or `["all"]`. If it is, the sandbox's `cube-agent` usually has not declared resource metrics capability version `1`; run template `redo` for an image-built template. If the template is compatible, inspect Cubelet's `Task.Stats` errors. |
| Existing metrics suddenly disappear | The sandbox may have been paused or deleted, or repeated collection failures may have made the latest sample older than `stale_after`. |
| A new sandbox has no `host_sandbox` metrics | Cubelet may have exhausted the assignment-baseline read attempts during creation. Inspect the Cubelet log for `capture host metrics baseline`, correct the persistent host cgroup read failure, and recreate the sandbox. |
| A scrape returns HTTP 503 | Two resource-metrics scrapes are already being processed on the node. Check for duplicate scrape jobs or concurrent manual requests. |
| CPU or memory limit metrics are missing | The corresponding cgroup has no finite limit. This is expected; cumulative CPU and current memory metrics remain available. |
| Metrics update at an unexpected frequency | `collection_interval` controls Cubelet collection, while Prometheus `scrape_interval` controls sample persistence. Check both settings. |

Check systemd logs when Cubelet fails to start. When the service is running but collection fails, inspect the Cubelet application log:

```bash
sudo journalctl -u cube-sandbox-cubelet.service -n 200 --no-pager
sudo tail -200 /data/log/Cubelet/Cubelet-req.log
```
