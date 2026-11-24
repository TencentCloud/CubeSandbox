---
title: Node Operations
lang: en-US
---

# Node Operations

For day-to-day node management, use the `cubeopscli` CLI on the control node, or the WebUI node detail page (port 12088).

This page covers the main compute-node operations in a CubeSandbox cluster: inspecting node compatibility, managing machine types and node labels, and adding, isolating, or deleting nodes.

> `cubeopscli` reaches the CubeOps service at `127.0.0.1:3010` by default. For split deployments such as the Terraform cluster edition, point it at the control-plane address with `--address` and `--port`.

Use the following commands to confirm node IDs and the current state:

```bash
cubeopscli node list

# Query one node
cubeopscli node list --hostid <node_id>
```

## Node identity and dynamic IP resolution

CubeSandbox separates a compute node's stable identity (`node_id` / `InstanceID`) from its dialable address (`HostIP` / endpoint IP) so that control-plane and data-plane traffic can follow a node even if its IP changes.

### How `node_id` and endpoint IP are selected

On startup, `cubelet` resolves its identity in the following precedence order (`Cubelet/pkg/utils/cloud.go`):

1. **`node_id` (stable node identity)**:
   1. `CUBE_SANDBOX_NODE_ID` — highest priority; can be any stable identifier. In Kubernetes Helm deployments, the chart injects `spec.nodeName` via the Downward API so `node_id` stays unchanged across `cube-node` Pod restarts or recreations on the same Kubernetes node.
   2. `CUBE_SANDBOX_NODE_IP` — used when `CUBE_SANDBOX_NODE_ID` is unset (the default in one-click / systemd / Terraform bare-metal deployments, where the host IP is static and doubles as `node_id`).
   3. Auto-detected primary non-loopback IPv4 address (default route interface → outbound IPv4 → first non-loopback IPv4) when neither environment variable is set.
2. **Endpoint IP (dialable `HostIP`)**:
   1. `CUBE_SANDBOX_ENDPOINT_IP` — in Kubernetes Helm deployments, the chart injects `status.podIP` via the Downward API.
   2. Otherwise, if `node_id` is a valid non-loopback IPv4 address, reuse `node_id`.
   3. Otherwise, fall back to the auto-detected primary non-loopback IPv4 address.

> **Requirement for dynamic IP tracking**: To keep control-plane and data-plane communication unaffected across IP changes, `node_id` must remain stable when the IP changes (i.e., set `CUBE_SANDBOX_NODE_ID` explicitly or use the Kubernetes Helm chart default `spec.nodeName`). If `CUBE_SANDBOX_NODE_ID` is unset and `node_id` falls back to the node's IP, an IP change is treated as a new node identity.

### How control plane and data plane track IP changes

When a compute node re-registers with the same `node_id` and a new `HostIP`:

- **CubeOps** updates the node's `HostIP` in storage and publishes `node_id -> host_ip` into the shared Redis Hash `cube:v1:shared:node:endpoint` on registration and every heartbeat (and deletes it when the node is deleted). Subsequent heartbeats preserve the latest registered `HostIP`.
- **CubeMaster** records `HostID` alongside `HostIP` across sandbox proxy routes, in-memory caches, pause snapshots, async tasks, and status reconcilers, resolving the live `HostIP` by `node_id` before dialing `cubelet`. When a node's IP changes, CubeMaster automatically closes stale gRPC connections to the previous IP.
- **CubeProxy** stores `HostID` in its local route cache and resolves the live upstream IP from `cube:v1:shared:node:endpoint` (cached locally with a 10s freshness TTL, single-flight refresh, and a longer last-known-IP fallback with 1s error backoff during Redis outages, without purging existing sandbox route entries).

**Backward compatibility & rolling upgrades**:

- Existing `sandbox:proxy:{sandboxID}` entries without `HostID` automatically fall back to their static `HostIP` in both CubeMaster and CubeProxy.
- Recommended rolling upgrade order: upgrade **CubeOps** first (heartbeats populate `cube:v1:shared:node:endpoint` within ~10s), then **CubeProxy** and **CubeMaster**. (Upgrading CubeMaster before CubeProxy is also safe because older CubeProxy ignores `HostID` and continues using static `HostIP`.)
- **Multi-replica CubeMaster rolling upgrade**: avoid cross-node Resume while old and new CubeMaster replicas coexist. An old replica does not know about `HostID` and would update `HostIP` without clearing a `HostID` written by a new replica, leaving `HostID` pointing at the previous node until all replicas finish upgrading.
- **Rollback**: rolling back **CubeMaster** from a new version to an old version leaves existing `HostID` fields in Redis intact; if the old CubeMaster later performs a cross-node Resume, it only rewrites `HostIP` while a new CubeProxy keeps resolving the stale `HostID` to the previous node. Roll back **CubeProxy** first (or remove the `HostID` field from affected `sandbox:proxy:{sandboxID}` entries).
- **CubeProxy reload behavior**: pre-upgrade cached routes in shared memory lack the `HostID` field and will trigger a one-time reload from Redis on first access after an in-place nginx reload.

## Node groups

Cross-node restore of a sandbox requires the source and the target node to have exactly the same CPU and kernel; we call such machines the same node type. A memory snapshot taken on one node type can only be restored onto the same node type.

List the node types in the cluster and their members:

```bash
cubeopscli node groups
```

Example output (compact by default — group, CPU identity, member count and nodes):

```text
GROUP    CPU_VENDOR     CPU_MODEL             COUNT  NODES
type-1   GenuineIntel   Intel(R) Xeon(R) ...  3      node-1,node-2,node-3
type-2   AMD            AMD EPYC 7B13 ...     2      node-4,node-5(cordoned)
```

- Nodes in the same group can restore each other's memory snapshots cross-node; nodes in different groups cannot.
- Node markers: `(down)` unhealthy, `(cordoned)` scheduling disabled, `(tainted)` the live KVM module carries a suspicious taint.
- `unknown` groups nodes that reported no `cpuid_hash`/`host_kernel_release` (for example, a cubelet old enough to never collect them). Their class cannot be verified, so cross-node restore fails closed for them.

Use this to see how many node types a cluster mixes before building templates on shared backends, scheduling cross-node restores, or planning rolling host-kernel upgrades.

## Machine types

A machine type declares a group of nodes that may restore memory snapshots onto each other.

The default cross-node restore rule requires the source and target nodes to share the same CPU **and** an identical host kernel. After a rolling host-kernel upgrade, machines that differ only by a kernel patch level get blocked, although their memory state is compatible. Binding such machines to one machine type waives the kernel requirement:

- Create a machine type and bind nodes to it in the WebUI (**Nodes → Machine types**) or with `cubeopscli machine-type`.
- A bound node carries the label `cube.cubesandbox.io/machine-type=<name>`; a node without the label is simply unbound — nothing to enable per node.
- A machine type waives only the kernel comparison; the CPU must still match.

Restore compatibility then works as follows:

| Source node | Target must match |
|---|---|
| No machine type | `cpuid_hash` and `host_kernel_release` (unchanged) |
| Machine type `<T>` | Machine type `<T>` and `cpuid_hash`; the host kernel is no longer compared |

After binding, a node restores only onto nodes with the same machine type and `cpuid_hash`; unbound nodes are excluded even when CPU and kernel match. When enabling a type for a group that could already restore between its members, bind the whole group.

A machine type that contains more than one CPU only produces a warning — those nodes still cannot restore onto each other. To see the CPU and kernel spread inside a type, use `cubeopscli machine-type get <name>`.

Deleting a type that still has nodes bound requires unbinding them at the same time (CLI: `--unbind`).

```bash
# Create a machine type and bind its first nodes.
cubeopscli machine-type create intel-oc9-k66 \
  --display-name "Intel OC9" \
  --description "same CPU, mixed host kernel" \
  --nodes node-1,node-2

# Inspect types: members, CPU and kernel spread, warnings.
cubeopscli machine-type list
cubeopscli machine-type get intel-oc9-k66

# Bind or unbind single nodes later; bind moves a node off its previous type.
cubeopscli machine-type bind intel-oc9-k66 node-3
cubeopscli machine-type unbind node-3

# Find nodes by type assignment.
cubeopscli node list --machine-type intel-oc9-k66
cubeopscli node list --unassigned-machine-type

# Deleting a type with bound nodes requires --unbind.
cubeopscli machine-type delete intel-oc9-k66 --unbind
```

The same commands are available as `cubeopscli node machine-type …`.

## Node labels

Nodes carry free-form labels for your own bookkeeping, for example `team=infra` or `zone=gz`:

```bash
cubeopscli node label list node-1
cubeopscli node label set node-1 team=infra
cubeopscli node label replace node-1 team=infra zone=gz
cubeopscli node label delete node-1 team
```

`set` adds or updates single labels, while `replace` installs the complete label set: any label missing from the command is deleted. System labels — the machine type, isolation state, and the data-plane labels reported by Cubelet (`kubernetes.io/*`, `beta.kubernetes.io/*`, `cube.cloud.tencentcloud.com/*`) — are never touched by label commands. The machine type is managed by the machine-type commands above; isolation is changed only with `node isolate` / `node unisolate`.

## Add a node

Adding a compute node means installing the compute-plane components and registering Cubelet with CubeOps. There is no separate `node add` CLI command; **registration completes automatically once the compute-node service starts**.

### Procedure

1. Copy and extract the same release bundle used by the control node.
2. Copy the environment template, then set the compute-node role, node IP, and control-plane address:

   ```bash
   cp env.example .env

   ONE_CLICK_DEPLOY_ROLE=compute
   CUBE_SANDBOX_NODE_IP=<current-node-ip>
   ONE_CLICK_CONTROL_PLANE_IP=<control-plane-ip>
   ```

3. (Optional but strongly recommended) Copy `CUBE_S3_*` from the control node to this node's `.env`:
   ```bash
   # Run on the control node; paste the output into the compute node's .env
   grep '^CUBE_S3_' /usr/local/services/cubetoolbox/.one-click.env
   ```
   Or point at another shared S3-compatible store. If missing, the installer warns and continues, but the S3 volume plugin stays disabled; you'll be reminded again when install finishes.
4. Install the compute node:

   ```bash
   sudo ./install-compute.sh
   ```

5. Verify the local services:

   ```bash
   sudo ./quickcheck.sh
   ```

6. From the control node, confirm that the node is registered and healthy:

   ```bash
   cubeopscli node list
   ```

7. Distribute the templates required on the new node. Depending on the template state and deployment workflow, use template redo with the target node or rebuild the template.

For complete environment variables, installation behavior, scheduler configuration, and troubleshooting, see [Multi-Node Cluster Deployment](./multi-node-deploy.md).

## Isolate a node

**Isolation temporarily prevents CubeMaster from scheduling new sandboxes onto a compute node.** It is similar to Kubernetes `cordon`: the node may remain healthy and existing sandboxes continue running.

For an isolated node, new sandbox scheduling skips it; creation fails if no other schedulable node is available. Existing sandboxes are unaffected and are not destroyed or migrated automatically.

::: warning Isolation is not drain
Isolation does not evict existing sandboxes. Before maintenance that interrupts processes or networking, isolate the node and then explicitly destroy its sandboxes.
:::

### Isolate

```bash
# One node
cubeopscli node isolate <node_id>

# Multiple nodes
cubeopscli node isolate <node_id_1> <node_id_2>
```

The operation is idempotent. After it succeeds, confirm that `SCHEDULING_DISABLED` is `true`:

```bash
cubeopscli node list
```

Wait at least 60 seconds for in-flight scheduling and creation windows to finish before disruptive maintenance.

For automation, use the CubeOps API with a JWT:

```bash
curl -X PUT "http://127.0.0.1:3010/api/v1/nodes/<node_id>/isolation" \
  -H "Authorization: Bearer $TOKEN"
```

### Unisolate

After maintenance, allow the node to receive new sandboxes again:

```bash
cubeopscli node unisolate <node_id>
```

Or use the API:

```bash
curl -X DELETE "http://127.0.0.1:3010/api/v1/nodes/<node_id>/isolation" \
  -H "Authorization: Bearer $TOKEN"
```

Confirm that `SCHEDULING_DISABLED` is `false` before returning the node to service.

For a Kubernetes compute-plane upgrade, follow [Kubernetes Upgrade](./kubernetes/upgrade.md) after isolating and emptying the node.

## Delete a node

Deleting a node removes its registration, status, component-version metadata, and cached metrics from CubeOps. It does not stop processes, destroy sandboxes, remove disks, or delete a Kubernetes Node object.

::: warning Isolate and empty the node first
CubeOps only deletes a node that is isolated and has no sandboxes. Deletion does not isolate the node or destroy its workloads.
:::

### Recommended sequence

1. Isolate the node.
2. Wait at least 60 seconds for in-flight scheduling to finish.
3. Destroy every sandbox on the node.
4. Stop or retire Cubelet so it cannot immediately register again.
5. Delete the node record.
6. Confirm that the node is absent from the list.

### Delete with cubeopscli

```bash
# Delete one node
cubeopscli node rm <node_id>

# `rm` is an alias for `delete`; multiple nodes are supported
cubeopscli node delete <node_id_1> <node_id_2>
```

Batch deletion attempts every node even if an earlier node fails, then returns an error listing the failures.

If the compute node is unreachable and CubeOps cannot verify its sandbox inventory, use forced deletion:

```bash
cubeopscli node rm --force <node_id>
```

`--force` bypasses only sandbox inventory verification. The node must still be isolated.

### Delete with the internal API

```bash
curl -X DELETE "http://127.0.0.1:3010/internal/v1/nodes/<node_id>"

# Bypass inventory verification when the node is unreachable
curl -X DELETE "http://127.0.0.1:3010/internal/v1/nodes/<node_id>?force=true"
```

Common failures are:

- `404 Not Found`: the node does not exist.
- `409 Conflict`: the node is not isolated or still has sandboxes.
- `502 Bad Gateway`: CubeMaster could not verify the sandbox inventory; restore connectivity or explicitly retry with force.

Deletion is not a permanent ban. If Cubelet is still running or starts again, it can register the same node ID as a new node record.

## Related documentation

- [Multi-Node Cluster Deployment](./multi-node-deploy.md)
- [CubeMaster Scheduler Configuration](./cubemaster-scheduler-config.md)
- [Service Management & Logs](./service-management.md)
- [Kubernetes Upgrade](./kubernetes/upgrade.md)
