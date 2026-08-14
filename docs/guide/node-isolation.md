---
title: Node Isolation
lang: en-US
---

# Node Isolation

Node isolation (isolate) temporarily **stops CubeMaster from scheduling new sandboxes onto a compute node** during maintenance, upgrades, or troubleshooting. It behaves like Kubernetes `cordon`: the node can stay healthy and existing sandboxes keep running — it simply stops receiving new work.

:::: tip Current entry points
Use **`cubeopscli`** on the control node, or the **WebUI** node detail page, to isolate and unisolate nodes.
::::

## What you'll learn

- How isolation differs from taking a node offline or draining it
- How to find a `node_id` and isolate / unisolate it
- How to safely remove a retired node's control-plane record
- How to verify that isolation took effect
- Recommended order of operations for upgrades and maintenance

## Behavior

| Aspect | After isolation |
|---|---|
| **New sandbox scheduling** | The node is skipped; creates fail if no other schedulable node remains |
| **Existing sandboxes** | **Unaffected** — nothing is destroyed or migrated automatically |
| **Node health** | Orthogonal to isolation: an isolated node can still report `healthy=true` |
| **Cubelet heartbeat / register** | Continues normally; the node cannot override or clear the isolation mark itself |

Under the hood, CubeOps writes a reserved label on the node metadata:

```text
cube.cloud.tencentcloud.com/scheduling-disabled=true
```

That label **cannot** be forged or cleared via the generic labels API or Cubelet registration — only the isolate / unisolate APIs on this page can change it.

:::: warning Isolation is not drain
Isolation does **not** evict existing sandboxes. If your next step will interrupt sandbox networking or processes (for example, a Kubernetes compute-plane upgrade that recreates the Big Pod), **destroy sandboxes on that node yourself** after isolating, then proceed. See the [Kubernetes upgrade guide](./kubernetes/upgrade.md).
::::

## Prerequisites

- Reachable CubeOps on the control node (default HTTP port **3010**)
- The target node is already registered with CubeOps (`node_id` exists)
- For CLI use: `cubeopscli` is installed on the control node and can reach CubeOps

Examples below assume you run commands on the control node itself (`127.0.0.1:3010`). In multi-node deployments, replace the address with the control-plane IP.

## Find the node ID

List cluster nodes and check the current isolation state:

```bash
# CLI
cubeopscli --address 127.0.0.1 --port 3010 node list

# Or via the public API (JWT auth required)
TOKEN=$(curl -s http://127.0.0.1:3010/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"<username>","password":"<password>"}' | jq -r '.accessToken')
curl -s http://127.0.0.1:3010/api/v1/nodes \
  -H "Authorization: Bearer $TOKEN" | jq .
```

In the CLI output, watch the `SCHEDULING_DISABLED` column: `true` means the node is isolated.

You can also inspect a single node:

```bash
cubeopscli --address 127.0.0.1 --port 3010 node list --hostid <node_id>
```

## Isolate a node

### Option 1: cubeopscli

```bash
# Isolate one node
cubeopscli --address 127.0.0.1 --port 3010 node isolate <node_id>

# Isolate multiple nodes
cubeopscli --address 127.0.0.1 --port 3010 node isolate <node_id_1> <node_id_2>

# Raw JSON response
cubeopscli --address 127.0.0.1 --port 3010 node isolate --json <node_id>
```

On success the CLI prints something like:

```text
node node-1 isolated: scheduling_disabled=true
```

The call is **idempotent**: repeating `isolate` on an already-isolated node is safe.

### Option 2: HTTP API (for scripts / automation)

```bash
curl -X PUT "http://127.0.0.1:3010/api/v1/nodes/<node_id>/isolation" \
  -H "Authorization: Bearer $TOKEN"
```

A successful response looks like:

```json
{
  "node_id": "node-1",
  "host_ip": "10.0.0.1",
  "healthy": true,
  "scheduling_disabled": true,
  "labels": {
    "cube.cloud.tencentcloud.com/scheduling-disabled": "true"
  }
}
```

## Verify isolation

Query the node again and confirm `scheduling_disabled` is `true`:

```bash
cubeopscli --address 127.0.0.1 --port 3010 node list
# SCHEDULING_DISABLED should be true
```

:::: tip Wait window
After isolating, wait **≥ 60 seconds** so in-flight schedule / create windows can finish before you perform disruptive maintenance (reboot, upgrade, take-down, and so on).
::::

## Unisolate a node

When maintenance is done, remove the cordon so the node can receive new sandboxes again:

```bash
# CLI
cubeopscli --address 127.0.0.1 --port 3010 node unisolate <node_id>

# HTTP
curl -X DELETE "http://127.0.0.1:3010/api/v1/nodes/<node_id>/isolation" \
  -H "Authorization: Bearer $TOKEN"
```

Afterwards `scheduling_disabled` should be `false`, and the `scheduling-disabled` label should be gone.

## Delete a node

Node deletion removes a retired node's control-plane record from CubeOps. It deletes the node registration, status, and component-version metadata and clears the per-node Redis metric cache. It **does not touch processes, sandboxes, disks, or the Kubernetes Node on the compute host**.

::: warning Isolate and empty the node first
CubeOps only deletes a node that is **isolated and has no sandboxes**. The delete operation does not isolate the node and does not destroy or migrate sandboxes. Isolate it, allow in-flight creates to finish, and explicitly destroy every sandbox on the node first.

If the compute node is currently unreachable but must be deleted, use forced deletion. Forced deletion bypasses sandbox inventory verification, but still requires prior isolation.
:::

### Recommended sequence

1. Isolate the target node.
2. Wait ≥ 60 seconds for in-flight scheduling / create windows to finish.
3. Destroy all sandboxes on the target node.
4. Delete the node.
5. Stop or retire Cubelet.
6. Confirm that the record is absent from the node list.

If the compute node is unreachable, normal node deletion fails because CubeOps cannot verify the sandbox inventory through CubeMaster. In that case, use forced deletion.

### Option 1 (recommended): cubeopscli

```bash
# Delete one node
cubeopscli --address 127.0.0.1 --port 3010 node rm <node_id>

# rm is an alias for delete; multiple nodes are also supported
cubeopscli --address 127.0.0.1 --port 3010 node delete <node_id_1> <node_id_2>

# Force deletion without sandbox inventory verification
cubeopscli --address 127.0.0.1 --port 3010 node rm --force <node_id>
```

Batch deletion processes every node independently: a failure does not prevent later nodes from being attempted, but the command ultimately returns an error listing the failed nodes. If inventory verification fails, the error tells the operator to restore connectivity or explicitly retry with `--force`. On success, the CLI prints:

```text
node node-1 deleted
```

### Option 2: HTTP API (internal)

```bash
curl -X DELETE "http://127.0.0.1:3010/internal/v1/nodes/<node_id>"

# Force deletion when live inventory cannot be verified
curl -X DELETE "http://127.0.0.1:3010/internal/v1/nodes/<node_id>?force=true"
```

The response is the JSON snapshot of the node that was just removed. Common failure status codes:

- `404 Not Found` — the node does not exist.
- `409 Conflict` — the node is not isolated, or still has sandboxes.
- `502 Bad Gateway` — CubeMaster could not be reached to verify the sandbox inventory; restore connectivity and retry, or use `force=true`.

`force=true` bypasses only the sandbox inventory check; it does not bypass isolation.

### After deletion

- The node disappears immediately from the current CubeOps instance; other CubeOps replicas remove it on their next metadata reload.
- Deletion is not a permanent deregistration or ban. A running or restarted Cubelet can register the same `node_id` again.
- To return the node to service, wait for it to register again and verify its health. The new registration does not retain the previous isolation mark.

## Typical workflows

### Before node maintenance / reboot

1. Isolate the target node
2. Wait ≥ 60 seconds
3. (If needed) destroy existing sandboxes on that node
4. Perform maintenance or reboot
5. After the node is back and re-registered, unisolate it

### Kubernetes compute-plane upgrade (recreates the Big Pod)

Compute-plane upgrades interrupt existing sandbox networking on that node. Recommended order:

1. Call the isolate API
2. Wait ≥ 60 seconds
3. **Destroy** sandboxes on that node
4. Proceed with the upgrade

Full steps: [Kubernetes upgrade guide](./kubernetes/upgrade.md).

## Scope and limitations

- **Not a drain**: existing sandboxes are not migrated or destroyed automatically.
- **Single-node / all-isolated clusters**: if no other schedulable node remains, new sandbox creates fail (no host selected).
- **Orthogonal to health checks**: an isolated node can stay Healthy and may still appear in healthy-node listings; it is only excluded from the schedulable set.
- **Independent of Kubernetes `kubectl cordon`**: this only affects CubeMaster scheduling; it does not cordon the Kubernetes Node.

## Related

- [Service Management & Logs](./service-management.md) — control / compute service lifecycle and logs
- [Kubernetes Upgrade](./kubernetes/upgrade.md) — isolate the node and clear sandboxes before upgrading
- [Multi-Node Deploy](./multi-node-deploy.md) — node registration and `/api/v1/nodes` checks
