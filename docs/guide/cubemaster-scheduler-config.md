# CubeMaster Scheduler Configuration

This page explains where CubeMaster scheduler configuration lives, how CubeMaster selects compute nodes, how Cubelet-reported metadata affects scheduling, and how one-click / Terraform deployment variables map to scheduling behavior.

If you only need basic multi-node scoring, start with [Multi-Node Cluster Deployment](./multi-node-deploy.md#configure-cubemaster-scheduler-scoring). Use this page as the complete reference when debugging scheduling failures, resource exhaustion, node-label mismatches, template locality, or template synchronization after adding compute nodes.

## Where configuration lives

CubeMaster scheduler configuration is stored in CubeMaster's `conf.yaml`:

| Deployment | Config location | How to apply |
|------------|-----------------|--------------|
| one-click / systemd | `/usr/local/services/cubetoolbox/CubeMaster/conf.yaml` | Restart `cube-sandbox-cubemaster.service` |
| source config template | `configs/single-node/cubemaster.yaml` | Rebuild the bundle or copy it into the runtime environment |
| Tencent Cloud Terraform / TKE | `deploy/one-click/terraform/tencentcloud/tke-addons.tf` (`kubernetes_secret.cubemaster_conf`) | Update the Terraform-generated `yamlencode` configuration, re-apply Terraform, and restart or roll cube-master Pods |
| Kubernetes / Helm chart | `deploy/kubernetes/chart/files/cube-master/conf.yaml`, rendered by `deploy/kubernetes/chart/templates/master-config-secret.yaml` | Update the chart file or rendered Secret and restart or roll cube-master Pods |

Cubelet node metadata and quota are not configured in CubeMaster. Each Cubelet reports them to CubeOps (port 3010). In one-click deployments, the main inputs are:

| Config | Location | Notes |
|--------|----------|-------|
| Cubelet static config | `/usr/local/services/cubetoolbox/Cubelet/config/config.toml` | Contains `node_status_update_frequency`; restart Cubelet after edits |
| Cubelet dynamic config | `/usr/local/services/cubetoolbox/Cubelet/dynamicconf/conf.yaml` | Contains `host.scheduler_label` and `host.quota`; restart Cubelet after edits |

Common Cubelet dynamic config:

```yaml
host:
  scheduler_label: "default-cluster"
  quota:
    mcpu_limit: 0
    mem_limit: ""
    mvm_limit: 0
    creation_concurrent_num: 0
```

`0` or an empty value usually means Cubelet derives a default from host resources. It does not mean unlimited capacity. For load tests or larger clusters, explicitly review CPU, memory, MVM count, and create-concurrency limits.

## How CubeMaster selects a compute node

For each sandbox create request, scheduling roughly follows four steps:

1. **Resolve request constraints**: read `instance_type`, template ID, resource requirements, explicit host IPs, node affinity / annotations, and similar constraints.
2. **Filter nodes**: remove unhealthy nodes, stale metric nodes, nodes over MVM limits, nodes without local template replicas, nodes with too many real-time or locally observed creates, nodes that do not satisfy affinity, and, when the disk filter or backoff path is active, nodes with high disk usage.
3. **Score nodes**: score remaining candidates, for example using weighted `mvm_num`, `local_create_num`, `quota_cpu_usage`, and `quota_mem_usage`.
4. **Pick the final node**: choose from the highest-scored candidate set. `priority_select_num` controls how many top nodes are eligible for final random selection, and `least_select_name` defaults to `random`.

Without scoring, CubeMaster still filters nodes but may choose from the filtered order, which can concentrate new sandboxes on the first eligible node until resource filters push traffic elsewhere.

## Key scheduler fields

Merge the following scoring fields into the existing `scheduler` section of `cubemaster.yaml`. Keep your existing `filter`, timeout, and instance-type-specific settings unless you intentionally want to replace them.

```yaml
scheduler:
  # Keep your existing filter, timeout, and other scheduler settings.
  priority_select_num: 3
  score:
    enable_scorers:
      - real_time_weighted_average
    resource_weights:
      mvm_num: 2
      local_create_num: 3
      quota_cpu_usage: 1
      quota_mem_usage: 1
    plugin_conf:
      real_time_weighted_average:
        weight: 1.0
        enable_weight_factors:
          - mvm_num
          - local_create_num
          - quota_cpu_usage
          - quota_mem_usage
```

| Field | Purpose |
|-------|---------|
| `priority_select_num` | Final selection is made from the top N scored nodes. Use a value greater than `1` for multi-node clusters; `3` is a good starting point for small clusters. |
| `metric_update_timeout` | Treat resource metrics as stale after this duration. It should be much larger than the Cubelet report interval. |
| `local_metric_update_timeout` | Reserved local-metric timeout field. Current prefilter logic gates both global and local metric freshness with `metric_update_timeout`. |
| `filter.enable_filters` | Enables scheduling filters. Common filters include CPU, memory, template locality, and real-time create concurrency. |
| `score.enable_scorers` | Enables scoring plugins. Multi-node deployments usually enable `real_time_weighted_average`; when it is enabled, the matching `score.plugin_conf.real_time_weighted_average` block is required or CubeMaster can panic during scheduler startup. |
| `score.resource_weights` | Controls the influence of MVM count, create concurrency, CPU quota usage, and memory quota usage. Higher weight means stronger influence; factors must also be listed under `score.plugin_conf.real_time_weighted_average.enable_weight_factors`. |
| `node_max_mvm_num` / `node_max_mvm_num_conf` | Global or per-instance-type single-node MVM limits. Cubelet-reported `max_mvm_num` also participates in the effective limit. |
| `disk_usage_max_percent` | Threshold used by the `disk` filter and backoff path to avoid placing more sandboxes on nearly full machines. |
| `affinityconf` / `node_affinity_selector_allowed_keys` | Controls affinity and constraints by cluster label, zone, CPU type, instance type, and other allowed selector keys. |

## How node metadata affects scheduling

Cubelet registers nodes and continuously reports status through CubeOps's `/internal/v1/node-agent` API. CubeOps persists this metadata to MySQL/Redis; CubeMaster syncs the node view from CubeOps every few seconds and keeps local cache snapshots for scheduling.

| Cubelet-reported field | Source | Scheduling effect |
|------------------------|--------|-------------------|
| `instance_type` | Cubelet node identity / instance type | Matches request `instance_type`, selects template replicas, and applies type-specific MVM settings. |
| `cluster_label` | `host.scheduler_label` | Used for cluster-label affinity and node-pool isolation. |
| `quota_cpu` | `host.quota.mcpu_limit` or derived from host resources | Schedulable CPU capacity used by CPU filtering and scoring. |
| `quota_mem_mb` | `host.quota.mem_limit` or derived from host resources | Schedulable memory capacity used by memory filtering and scoring. |
| `max_mvm_num` | `host.quota.mvm_limit` or memory-derived default | Single-node MVM limit. Nodes at the limit are filtered out. |
| `create_concurrent_num` | `host.quota.creation_concurrent_num` | Reported per-node create concurrency. `0` means Cubelet does not set an additional engine flow limit, but CubeMaster scheduling still falls back to `cubelet_conf.create_concurrent_limit`. |
| allocated / disk usage / cgroup metrics | Periodic Cubelet reports | Used for current resource usage, disk watermarks, and scoring factors. |

These values are configured and reported per compute node. Heterogeneous clusters can use different instance types, labels, quota, and create-concurrency limits on different nodes.

## Deployment variable mapping

### one-click multi-node

| Variable / config | Effect |
|-------------------|--------|
| `ONE_CLICK_DEPLOY_ROLE=compute` | Installs a compute node, runs Cubelet/runtime services, and registers to the control plane. |
| `CUBE_SANDBOX_NODE_IP` | Routable address registered for this node. Incorrect values can make the node unreachable or invisible. |
| `ONE_CLICK_CONTROL_PLANE_IP` / `ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR` | CubeOps endpoint used by Cubelet for registration and reports (port 3010). CubeMaster is reachable separately on 8089. |
| `node_status_update_frequency` in `Cubelet/config/config.toml` | Node status/resource report interval. Default is `1s`; do not put this in dynamic config. |
| `host.scheduler_label` in `Cubelet/dynamicconf/conf.yaml` | Node-pool label for affinity and isolation. |
| `host.quota.*` in `Cubelet/dynamicconf/conf.yaml` | CPU, memory, MVM count, and create-concurrency scheduling capacity. |

### Tencent Cloud Terraform / TKE

| Variable | Effect |
|----------|--------|
| `TENCENTCLOUD_COMPUTE_NODE_COUNT` | Number of PVM compute nodes, directly controlling the sandbox-hosting node-pool size. |
| `TENCENTCLOUD_COMPUTE_INSTANCE_TYPE` | Default compute-node instance type, affecting real CPU/memory and Cubelet-derived quota. |
| `TF_VAR_compute_instance_types` | Per-node instance types for heterogeneous compute pools. |
| `TENCENTCLOUD_COMPUTE_DATA_DISK_SIZE` | Data disk size for `/data/cubelet`, affecting template, snapshot, and runtime-data capacity. |
| `TENCENTCLOUD_CUBELET_NODE_STATUS_UPDATE_FREQUENCY` | Written into each compute node's Cubelet static config; controls node status/resource reporting cadence. |
| `TENCENTCLOUD_TKE_NODE_COUNT` / `TENCENTCLOUD_TKE_WORKER_INSTANCE_TYPE` | Control-plane Pod resources. They do not host sandboxes directly, but affect cube-master, cube-api, cube-proxy, and overall control-plane throughput. |

`TENCENTCLOUD_COMPUTE_NODE_COUNT` and `TENCENTCLOUD_TKE_NODE_COUNT` are separate resources: the former runs Cubelet and hosts sandboxes, while the latter runs control-plane Pods.

## Recommended configurations

### Small test cluster

Suitable for POC, functional validation, and low concurrency. This is a complete starting point for a new small test cluster and includes the shipped timeout/filter defaults; when updating an existing deployment, merge these values instead of replacing unrelated scheduler settings blindly.

```yaml
scheduler:
  priority_select_num: 3
  metric_update_timeout: 300s
  local_metric_update_timeout: 300s
  filter:
    enable_filters:
      - cpu
      - mem
      - template_locality
      - realtime_create_num
  score:
    enable_scorers:
      - real_time_weighted_average
    resource_weights:
      mvm_num: 2
      local_create_num: 3
      quota_cpu_usage: 1
      quota_mem_usage: 1
    plugin_conf:
      real_time_weighted_average:
        weight: 1.0
        enable_weight_factors:
          - mvm_num
          - local_create_num
          - quota_cpu_usage
          - quota_mem_usage
```

Recommendations:

- Use at least 2 compute nodes so node selection and failure isolation can be validated.
- Start with `priority_select_num: 3`; if the cluster has fewer than 3 compute nodes, set it to the node count.
- Keep Cubelet reporting at the default `1s` and CubeMaster metric timeouts at `300s`.
- Default-derived `host.quota` is acceptable for basic validation, but inspect it before load testing.

### Larger production-like cluster

For higher concurrency or long-running environments:

- Set clear `host.scheduler_label` values per node pool, such as general, memory-heavy, or load-test pools.
- Configure `node_max_mvm_num_conf` per `instance_type`; avoid using one limit for both large and small nodes.
- Increase `priority_select_num` to a small fraction of healthy compute nodes, but avoid making final selection fully random.
- Keep the `template_locality` filter enabled so creation only lands on nodes with usable template replicas.
- Set explicit `host.quota.creation_concurrent_num` values to avoid a burst of image, disk, or VMM work overloading a single node.
- Scale the control plane as well: increase `TENCENTCLOUD_TKE_NODE_COUNT` and relevant control-plane replicas so CubeMaster or cube-api does not become the bottleneck.

## Template redo after adding compute nodes

After a new compute node is added, successful node registration does not mean every template is available on that node. The `template_locality` filter requires a usable local template replica on the target node, so creates may fail or continue to land only on older nodes until templates are synchronized.

For image-built templates, run template redo after adding a node:

```bash
cubemastercli tpl redo \
  --template-id <tpl-id> \
  --node <node-ip>
```

Notes:

- `--node` accepts a node ID or host IP; repeat it for multiple nodes.
- `redo` waits for completion by default; use `--detach` to submit and exit.
- Use `--failed-only` to redo only failed nodes.
- Wait for the redo job to complete before creating sandboxes from that template on the new node.

Recommended scale-out flow:

1. Install the compute node and confirm it appears in the CubeMaster node list.
2. Confirm the node is healthy and Cubelet resource reports are fresh.
3. Run `cubemastercli tpl redo --template-id <tpl-id> --node <node-ip>` for templates that should run on the new node.
4. Wait for redo jobs to succeed before opening traffic or running E2E tests.

## Troubleshooting

### Scheduling fails or returns no more resource

Check:

- Whether `quota_cpu` / `quota_mem_mb` are lower than the requested resources.
- Whether `host.quota.mcpu_limit`, `host.quota.mem_limit`, and `host.quota.mvm_limit` still use low derived defaults.
- Whether `mvm_num` reached `max_mvm_num` or `node_max_mvm_num_conf`.
- Whether the effective create concurrency limit is blocking the node. Remember that `create_concurrent_num: 0` still falls back to CubeMaster's `cubelet_conf.create_concurrent_limit` at the scheduler layer.

Useful entry points:

```bash
curl http://127.0.0.1:3010/internal/v1/nodes
sudo tail -F /data/log/CubeMaster/cubemaster-req.log
sudo tail -F /data/log/Cubelet/Cubelet-req.log
```

### Node status or resource reports are stale

If CubeMaster logs mention metric update timeouts:

- Confirm `cube-sandbox-cubelet.service` is running on the compute node.
- Confirm `ONE_CLICK_CONTROL_PLANE_CUBEOPS_ADDR` or the Terraform-generated CubeOps endpoint is reachable from the compute node.
- Check whether `node_status_update_frequency` was accidentally set too high.
- Ensure `metric_update_timeout` is much larger than the report interval.

### Template unavailable

Typical symptoms: new sandboxes still land only on old nodes after scale-out, or logs mention unavailable local template replicas.

Fix:

- Confirm the request's `template_id`.
- Run `cubemastercli tpl redo --template-id <tpl-id> --node <node-ip>` for the new node.
- Check the template job status and confirm redo succeeds.
- Keep the `template_locality` filter enabled; do not disable it as a workaround.

### Node label mismatch

If a request uses node affinity, cluster labels, or a specific instance type but no candidate node remains:

- Check that Cubelet `host.scheduler_label` matches the request or `affinityconf`.
- Check that `instance_type` matches the template and request.
- Check that `node_affinity_selector_allowed_keys` allows the selector key used by the request.
- For heterogeneous node pools, confirm template replicas have been redone onto the target label / instance-type pool.

### Creates concentrate on only a few nodes

If new sandboxes still concentrate on one machine in a multi-node cluster:

- Confirm `score.enable_scorers` is enabled.
- Confirm `score.plugin_conf.real_time_weighted_average.enable_weight_factors` includes the expected factors when `real_time_weighted_average` is enabled.
- Set `priority_select_num` to a value greater than `1`.
- Check that weights for `local_create_num`, `mvm_num`, `quota_cpu_usage`, and `quota_mem_usage` are configured.
- Confirm templates are available on all intended nodes; otherwise `template_locality` shrinks the candidate set.

## See also

- [Multi-Node Cluster Deployment](./multi-node-deploy.md)
- [Tencent Cloud Cluster Deployment (Terraform)](./tencentcloud-terraform-deploy.md)
- [Service Management & Logs](./service-management.md)
- [Templates Troubleshooting](./troubleshooting/templates.md)
