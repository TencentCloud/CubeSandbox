# Compute-Node Autoscaling

CubeOps can size the compute-node pool from Cube's own scheduler quota and let
the Kubernetes Cluster Autoscaler add and remove the machines
([#2193](https://github.com/TencentCloud/CubeSandbox/issues/2193)). It is
disabled by default.

## Why a bridge is needed

Cube MicroVMs run inside Cubelet, not as Kubernetes Pods. The Cluster
Autoscaler therefore cannot see sandbox demand, and it considers every compute
node empty: lowering a node pool's size removes arbitrary nodes together with
the sandboxes running on them. Lookups of those sandboxes then fail until the
node metadata is removed from Cube.

## How it works

CubeOps keeps one small **placeholder Pod** on every compute node it wants to
keep. A node running a Pod without a controller is never removed by the Cluster
Autoscaler, so the placeholder pins the node.

- **Scale-out.** When scheduler-quota usage plus headroom exceeds the target
  utilization of the serving nodes, CubeOps creates another placeholder. It
  cannot fit on an existing node (one per node), stays Pending, and the Cluster
  Autoscaler adds a machine. A new node counts as capacity only after it has
  registered with Cube and reports healthy; no second request is made while one
  is still starting.
- **Scale-in.** When demand stays below the pool for `scaleInCooldown`, CubeOps
  isolates the node with the fewest MicroVMs, waits until it holds no running or
  paused sandbox, re-checks the isolation, and only then deletes that node's
  placeholder. The Cluster Autoscaler removes exactly that machine. After the
  machine is gone CubeOps deletes the Cube node metadata. Sandboxes are never
  stopped: a node that is not empty after `drainTimeout` is returned to service.
- One node is retired at a time. Nodes isolated by an operator are left alone
  and stay pinned.
- All state is derived from Cube node metadata, the node operation log, and the
  placeholder Pods, so a CubeOps restart continues where it stopped. Replicas
  coordinate through the database lock.

`GET /internal/v1/autoscale/status` on CubeOps returns the last decision:
demand, per-node state, planned actions and warnings.

## Enable

1. Make sure the compute-node pool has the Cluster Autoscaler enabled with
   limits that cover `minNodes..maxNodes`, but **do not lower its minimum yet**.
2. Enable the autoscaler and wait until every compute node has a placeholder
   (`kubectl get pods -l cube.cloud.tencentcloud.com/capacity-placeholder=true -o wide`):

   ```yaml
   cubeOps:
     autoscale:
       enabled: true
       nodeSelector:
         nodepool: cube-compute          # labels of the compute-node pool
       tolerations:
         - key: cube.tencent.com/compute
           operator: Equal
           value: "true"
           effect: NoSchedule
       minNodes: 1
       maxNodes: 10
       targetUtilization: 0.7
   ```

3. Lower the node pool minimum. From now on only drained nodes are removed.

Use `dryRun: true` first to see the decisions in the CubeOps log without any
change.

## Limits

- Sizing uses scheduler quota (`quota_cpu`, `quota_mem_mb`) and current usage;
  create requests rejected for lack of capacity are not yet an input.
- A node with long-lived paused sandboxes cannot be drained and blocks
  scale-in until `drainTimeout`, after which it is returned to service.
- Sandbox migration, consolidation and cloud interruption handling are out of
  scope for this version.
- The Kubernetes node name must equal the Cube node ID (the chart's default).
