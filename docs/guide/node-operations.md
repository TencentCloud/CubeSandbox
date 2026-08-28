---
title: Node Operations
lang: en-US
---

# Node Operations

This page covers the three main compute-node operations in a CubeSandbox cluster: adding a node, isolating a node for maintenance, and deleting a retired node.

Use `cubeopscli` on the control node or the WebUI node detail page for day-to-day node management. The examples below assume CubeOps is available at `127.0.0.1:3010`; use the control-plane IP in a multi-node deployment.

List nodes before and after an operation to confirm the node ID and current state:

```bash
cubeopscli node list

# Query one node
cubeopscli node list --hostid <node_id>
```

## Add a node

Adding a compute node means installing the compute-plane components and registering Cubelet with CubeOps. There is no separate `node add` CLI command: registration happens automatically when the compute-node service starts.

### Prerequisites

- A working control node installed with the same CubeSandbox release bundle.
- A compute host that meets the hardware and software requirements.
- Network access from the compute node to CubeOps on port `3010`.
- Access to the cluster's S3-compatible storage endpoint. When using the bundled MinIO, the compute node must reach the control node on port `9000`.

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

Isolation temporarily prevents CubeMaster from scheduling new sandboxes onto a compute node. It is similar to Kubernetes `cordon`: the node may remain healthy and existing sandboxes continue running.

| Aspect | Behavior after isolation |
| --- | --- |
| New sandbox scheduling | The node is skipped. Creation fails if no other schedulable node is available. |
| Existing sandboxes | Unaffected; they are not destroyed or migrated automatically. |
| Node health | Independent of isolation; an isolated node can remain `healthy=true`. |
| Cubelet heartbeat | Continues normally and cannot clear the isolation state. |

CubeOps records isolation with the reserved label:

```text
cube.cloud.tencentcloud.com/scheduling-disabled=true
```

The generic labels API and Cubelet registration cannot set or clear this label.

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
