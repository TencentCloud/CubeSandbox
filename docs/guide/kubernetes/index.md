# Kubernetes Deployment

Install CubeSandbox (control plane + compute plane) with a Helm Chart on an existing Kubernetes cluster.

::: tip Difference from the “one-click script” deployment
This is the **native K8s path**: components run in the cluster and are managed by Helm. If you only have a single physical machine and do not plan to use K8s, see [Bare-Metal Deployment](../bare-metal-deploy.md) or [Quick Start](../quickstart.md) instead.
:::

::: warning Preview version warning
The current K8s deployment is a **preview** release. Known issues:

1. When compute nodes are under resource pressure, Pods may be incorrectly evicted by the K8s control plane, interrupting sandboxes. This is being fixed.
2. The in-place, zero-downtime upgrade flow for compute nodes is still being refined. Evaluate and test carefully before operating, to avoid recreating the `cube-node` Big Pod and losing existing sandboxes. Before upgrading a node, call CubeMaster’s isolate API, isolate the node for at least 60 seconds, then proceed with the upgrade.
3. Because `cube-node` configuration on compute nodes may still change, later version upgrades may recreate Pods. After deploying the current version on K8s, if you plan to upgrade later, carefully evaluate the changes and test first; ideally destroy all sandboxes on the compute nodes to be upgraded before upgrading, so that `cube-node` recreation does not interrupt workloads.

**These issues will be addressed in later versions. You are welcome to try the K8s deployment path and report issues and suggestions via Issues.**
:::

## Docs navigation

| Doc | Contents |
| --- | --- |
| [Helm Install](./install.md) | Full steps from cluster readiness to verification (recommended main path) |
| [Architecture](./architecture.md) | Chart component layers, four DaemonSets, startup order, and data flows |
| [Upgrade](./upgrade.md) | In-place compute image upgrades: what to bump, which workload, red lines, and special cases |
| [FAQ](./faq.md) | Troubleshooting for install, scheduling, PVM, Proxy, Egress, and upgrades |

## Install order (required reading)

```text
① Cluster ready
    ↓
② Install OpenKruise and confirm Ready
    ↓
③ Label nodes (and role taints)
    ↓
④ Prepare values
    ↓
⑤ helm upgrade --install
    ↓
⑥ Verify
```

You must install OpenKruise first: the control-plane CloneSet and the compute-plane Advanced DaemonSet depend on it. If you apply role taints before installing OpenKruise, `kruise-controller-manager` can easily stay Pending forever.

Next → [Helm Install](./install.md)
