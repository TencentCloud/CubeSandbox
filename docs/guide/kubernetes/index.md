# Kubernetes Deployment

Install CubeSandbox (control plane + compute plane) with a Helm Chart on an existing Kubernetes cluster.

::: tip Difference from the “one-click script” deployment
This is the **native K8s path**: components run in the cluster and are managed by Helm. If you only have a single physical machine and do not plan to use K8s, see [Bare-Metal Deployment](../bare-metal-deploy.md) or [Quick Start](../quickstart.md) instead.
:::

::: warning Preview version warning
The current K8s deployment is a **preview** release. Known issues:

1. When compute nodes are under resource pressure, Pods may be incorrectly evicted by the K8s control plane, interrupting sandboxes. This is being fixed.
2. Compute-node upgrades currently have known issues: an upgrade recreates `cube-node` and interrupts existing sandbox networking on that node. Read the [Upgrade guide](./upgrade.md) **before you deploy**.

**These issues will be addressed in later versions. You are welcome to try the K8s deployment path and report issues and suggestions via Issues.**
:::

## Docs navigation

| Doc | Contents |
| --- | --- |
| [Helm Install](./install.md) | Full steps from cluster readiness to verification (recommended main path) |
| [Architecture](./architecture.md) | Chart component layers, four DaemonSets, startup order, and data flows |
| [Upgrade](./upgrade.md) | Control plane can roll; compute upgrades recreate the Big Pod and interrupt sandboxes |
| [FAQ](./faq.md) | Troubleshooting for install, scheduling, PVM, Proxy, Egress, and upgrades |

## Install order (required reading)

```text
① Cluster ready
    ↓
② Label nodes (and role taints)
    ↓
③ Prepare values
    ↓
④ helm upgrade --install
    ↓
⑤ Verify
```

The control plane uses native Deployments; the compute plane uses native `apps/v1` DaemonSets. Install needs only standard Kubernetes / Helm.


## Tested environments

CubeSandbox has been verified on Tencent Cloud TKE, Kubernetes, and K3s. Tencent Cloud TKE uses VPC-CNI as the CNI plugin; the other two environments use Flannel.

| Environment | CNI plugin |
| --- | --- |
| Tencent Cloud TKE | VPC-CNI |
| Kubernetes | Flannel |
| K3s | Flannel |


Next → [Helm Install](./install.md)
