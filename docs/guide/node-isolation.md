---
title: Node Isolation
lang: en-US
---

# Node Isolation

Node isolation (isolate) temporarily **stops CubeMaster from scheduling new sandboxes onto a compute node** during maintenance, upgrades, or troubleshooting. It behaves like Kubernetes `cordon`: the node can stay healthy and existing sandboxes keep running — it simply stops receiving new work.

::: tip Current entry points
WebUI / CubeOps / the public OpenAPI surface do **not** expose isolation yet. Use the **CubeMaster HTTP API**, or **`cubemastercli`** on the control node, to isolate and unisolate nodes.
:::

## What you'll learn

- How isolation differs from taking a node offline or draining it
- How to find a `node_id` and isolate / unisolate it
- How to verify that isolation took effect
- Recommended order of operations for upgrades and maintenance

## Behavior

| Aspect | After isolation |
|---|---|
| **New sandbox scheduling** | The node is skipped; creates fail if no other schedulable node remains |
| **Existing sandboxes** | **Unaffected** — nothing is destroyed or migrated automatically |
| **Node health** | Orthogonal to isolation: an isolated node can still report `healthy=true` |
| **Cubelet heartbeat / register** | Continues normally; the node cannot override or clear the isolation mark itself |

Under the hood, CubeMaster writes a reserved label on the node metadata:

```text
cube.cloud.tencentcloud.com/scheduling-disabled=true
```

That label **cannot** be forged or cleared via the generic labels API or Cubelet registration — only the isolate / unisolate APIs on this page can change it.

::: warning Isolation is not drain
Isolation does **not** evict existing sandboxes. If your next step will interrupt sandbox networking or processes (for example, a Kubernetes compute-plane upgrade that recreates the Big Pod), **destroy sandboxes on that node yourself** after isolating, then proceed. See the [Kubernetes upgrade guide](./kubernetes/upgrade.md).
:::

## Prerequisites

- Reachable CubeMaster on the control node (default HTTP port **8089**)
- The target node is already registered with CubeMaster (`node_id` exists)
- For CLI use: `cubemastercli` is installed on the control node and can reach CubeMaster

Examples below assume you run commands on the control node itself (`127.0.0.1:8089`). In multi-node deployments, replace the address with the control-plane IP.

## Find the node ID

List cluster nodes and check the current isolation state:

```bash
# CLI
cubemastercli --address 127.0.0.1 --port 8089 node list

# Or call the API directly
curl -s http://127.0.0.1:8089/internal/meta/nodes | jq .
```

In the CLI output, watch the `SCHEDULING_DISABLED` column: `true` means the node is isolated.

You can also inspect a single node:

```bash
curl -s http://127.0.0.1:8089/internal/meta/nodes/<node_id> | jq '{
  node_id,
  host_ip,
  healthy,
  scheduling_disabled,
  labels
}'
```

## Isolate a node

### Option 1: HTTP API (best for scripts / automation)

```bash
curl -X PUT "http://127.0.0.1:8089/internal/meta/nodes/<node_id>/isolation"
```

A successful response looks like:

```json
{
  "ret": {
    "RetCode": 200,
    "RetMsg": "Success"
  },
  "data": {
    "node_id": "node-1",
    "host_ip": "10.0.0.1",
    "healthy": true,
    "scheduling_disabled": true,
    "labels": {
      "cube.cloud.tencentcloud.com/scheduling-disabled": "true"
    }
  }
}
```

The call is **idempotent**: repeating `PUT` on an already-isolated node is safe. No request body is required.

### Option 2: cubemastercli

```bash
# Isolate one node
cubemastercli --address 127.0.0.1 --port 8089 node isolate <node_id>

# Isolate multiple nodes
cubemastercli --address 127.0.0.1 --port 8089 node isolate <node_id_1> <node_id_2>

# Raw JSON response
cubemastercli --address 127.0.0.1 --port 8089 node isolate --json <node_id>
```

On success the CLI prints something like:

```text
node node-1 isolated: scheduling_disabled=true
```

## Verify isolation

Query the node again and confirm `scheduling_disabled` is `true`:

```bash
curl -s http://127.0.0.1:8089/internal/meta/nodes/<node_id> | jq '.scheduling_disabled'
# Expected: true

cubemastercli --address 127.0.0.1 --port 8089 node list
# SCHEDULING_DISABLED should be true
```

::: tip Wait window
After isolating, wait **≥ 60 seconds** so in-flight schedule / create windows can finish before you perform disruptive maintenance (reboot, upgrade, take-down, and so on).
:::

## Unisolate a node

When maintenance is done, remove the cordon so the node can receive new sandboxes again:

```bash
# HTTP
curl -X DELETE "http://127.0.0.1:8089/internal/meta/nodes/<node_id>/isolation"

# CLI
cubemastercli --address 127.0.0.1 --port 8089 node unisolate <node_id>
```

Afterwards `scheduling_disabled` should be `false`, and the `scheduling-disabled` label should be gone.

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
- **Auth**: with CubeMaster HTTP auth disabled (the default), you can call the API directly. If you enable CubeMaster auth, add the required signature headers per your cluster config. Current `cubemastercli node isolate/unisolate` does **not** attach signatures automatically — prefer signed HTTP requests when auth is on.

## Related

- [Service Management & Logs](./service-management.md) — control / compute service lifecycle and logs
- [Kubernetes Upgrade](./kubernetes/upgrade.md) — isolate the node and clear sandboxes before upgrading
- [Multi-Node Deploy](./multi-node-deploy.md) — node registration and `/internal/meta/nodes` checks
