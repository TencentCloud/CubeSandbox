# Helm Install

Follow a fixed order to complete a Helm install of CubeSandbox on an existing cluster.

::: warning Production note
- Before exposing services to untrusted networks, complete [Network Hardening](../network-hardening.md).
:::

## How you will install

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

Why must OpenKruise come first? CubeSandbox’s control plane (CloneSet) and the main compute path (Advanced DaemonSet) depend on it. If you apply role taints before installing OpenKruise, `kruise-controller-manager` can easily stay Pending forever.

---

## 1. Prerequisites

| Item | Requirement |
| --- | --- |
| Kubernetes | **v1.27+** (self-managed / k3s / TKE, etc.), with `InPlacePodVerticalScaling` enabled |
| Tools | `kubectl`, Helm v3.10+ |
| OpenKruise | **1.9+**, hard dependency (install in the next step; enable `InPlaceWorkloadVerticalScaling`) |
| Storage | A usable StorageClass in the cluster (or switch to hostPath; see below) |
| Image pull | Ability to pull Chart default images, or your own private registry |

::: tip Why 1.27+ with the feature gate?
Day-to-day in-place image upgrades (`InPlaceIfPossible`) can work on older versions. But if Big Pod slots later need **in-place resource (CPU/Memory) changes** to add components, that depends on Kubernetes `InPlacePodVerticalScaling` (Alpha in 1.27, must be enabled explicitly; on by default in 1.33+) and OpenKruise `InPlaceWorkloadVerticalScaling`. Therefore the install baseline is **1.27+ + FG**.
:::

### How node roles are split

CubeSandbox uses **labels** to distinguish two kinds of nodes (not the K8s master/worker concept):

| Role | Suggested size | Purpose | Labels to apply |
| --- | --- | --- | --- |
| **Control-plane node** | ≥ 1; production suggests 3+; ~4C8G + persistent disk | Runs CubeMaster / API / Proxy / WebUI / MySQL / Redis, etc. | `cube.tencent.com/cube-control=true` |
| **Compute node** | ≥ 1; suggest 16C32G+ | Runs sandboxes (cube-node / bootstrap / installer) | `cube.tencent.com/cube-node=true` |
| (Optional) PVM compute node | When host kernel swap is needed | Schedules `cube-node-pvm` and pulls PVM images | In addition to the compute label: `cube.tencent.com/allow-pvm-bootstrap=true` |

Compute nodes also need: `/dev/kvm` available, and an XFS data disk is recommended.

::: tip How to read the control-plane label?
The Chart **by default** sets control-plane Pods to `nodeSelector: cube.tencent.com/cube-control=true`:

- **Recommended**: label the nodes that should run the control plane; otherwise those Pods will be **Pending** (objects can be created, but the scheduler finds no matching node).
- It is **not** “install fails without the label”: Helm still creates objects successfully; the default selector simply does not match any node.
- If you use your own `placement.controlPlane.nodeSelector`, you do not have to use the `cube-control` key.
- The Chart **does not allow** clearing the controlPlane `nodeSelector` (`validate.yaml` fails the render).
- For Kubernetes’ own master nodes: if they already have `NoSchedule` and you do not plan to co-locate the Cube control plane, you do not need Cube labels.
:::

---

## 2. Confirm the cluster is ready

```bash
kubectl get nodes -o wide
kubectl get storageclass
helm version
```

Also confirm the cluster has **`InPlacePodVerticalScaling`** enabled (for self-managed clusters, check apiserver / kubelet `--feature-gates`; for managed clusters, check the console or distro docs. Kubernetes **1.33+** enables it by default).

Note the node names you will use later. Confirm at least one schedulable node can run the control plane, and one (or the same) node can run the compute plane.

---

## 3. Install OpenKruise

> **Hard constraint:** Finish this section and confirm Ready before applying the **role taints** in the next section.

```bash
helm repo add openkruise https://openkruise.github.io/charts/
helm repo update

helm install kruise openkruise/kruise --version 1.9.0 \
  --set featureGates='ImagePullJobGate=true\,InPlaceWorkloadVerticalScaling=true'
```

Acceptance (continue only after all pass):

```bash
kubectl get pods -n kruise-system
kubectl -n kruise-system get deploy kruise-controller-manager
kubectl -n kruise-system get ds kruise-daemon
kubectl api-resources | grep kruise.io
# Expect: daemonsets ... apps.kruise.io and clonesets ... apps.kruise.io
```

Expect: `kruise-controller-manager` Ready ≥ 1, `kruise-daemon` Ready on each node, and Advanced DaemonSet / CloneSet APIs discoverable.

---

## 4. Label nodes (and role taints)

After OpenKruise is Ready, mark the nodes.

### 4.1 Multi-node (control / compute split, recommended)

```bash
# Control-plane nodes (run CubeMaster, etc.)
kubectl label nodes <control-node> cube.tencent.com/cube-control=true --overwrite

# Compute nodes (run sandboxes)
kubectl label nodes <compute-node> cube.tencent.com/cube-node=true --overwrite

# Only when this compute node needs the PVM host kernel:
kubectl label nodes <pvm-compute-node> cube.tencent.com/allow-pvm-bootstrap=true --overwrite
```

Role taints (recommended, to keep unrelated workloads off these nodes):

```bash
kubectl taint nodes <control-node> cube.tencent.com/control=true:NoSchedule --overwrite
kubectl taint nodes <compute-node> cube.tencent.com/compute=true:NoSchedule --overwrite
```

### 4.2 Single-node trial (one machine is both control and compute)

```bash
export NODE=<your-node-name>

kubectl label nodes "$NODE" \
  cube.tencent.com/cube-control=true \
  cube.tencent.com/cube-node=true \
  cube.tencent.com/allow-pvm-bootstrap=true \
  --overwrite

# Taints must come after OpenKruise is Ready
kubectl taint nodes "$NODE" cube.tencent.com/control=true:NoSchedule --overwrite
kubectl taint nodes "$NODE" cube.tencent.com/compute=true:NoSchedule --overwrite
```

For single-node installs, also layer `values-single-node.yaml` (see next step). It injects the union of control/compute taint tolerations; otherwise co-location will Pending.

::: tip Pure BM, no PVM kernel swap
You can omit the `allow-pvm-bootstrap` label and set `bootstrap.pvmHostKernel.enabled: false` in values.
:::

### 4.3 System component checks before enabling PVM (when kernel swap is needed)

Confirm CNI, kube-proxy, and `kruise-daemon` tolerate `NoSchedule` (`operator: Exists`, or explicitly tolerate `cube.tencent.com/pvm-not-ready`), and remain Running on the target node. Otherwise the node may lose connectivity during the PVM gate and cannot clear the latch.

---

## 5. Prepare the values file

Copy the example from the repo into a local file (**do not** commit files that contain real passwords to Git):

```bash
cp deploy/kubernetes/chart/runtime-values.example.yaml runtime-values.yaml
```

Change at least these fields:

```yaml
cubeProxy:
  advertiseIP: "10.0.1.10"   # Docs/ops hint only; not used in Chart templates; external entry via Service/Ingress/DNS
  domain: "cube.app"
  tls:
    mode: selfSigned         # Trial; production: existingSecret / certManager

# Required: Chart rejects CHANGE_ME_* sentinels in values.yaml
mysql:
  host: ""                   # Empty = use built-in MySQL
  password: "replace-me-mysql-password"
  rootPassword: "replace-me-mysql-root-password"
redis:
  host: ""
  password: "replace-me-redis-password"
```

**Control-plane PVCs:**

- Default: CubeMaster / MySQL / Redis use the cluster **default StorageClass**
- To pick an SC: set `persistence.storageClassName: <name>` in `runtime-values.yaml`
- Single-node / no CSI: you can switch to hostPath (see comments in `runtime-values.example.yaml`)

**TKE:** Also layer `deploy/kubernetes/chart/values-tke.yaml` at install time (creates a CBS StorageClass and binds PVCs to it).

### Compute node data disk (`/data/cubelet`)

Sandbox rootfs / snapshots land on the host at `/data/cubelet`, which must be **XFS (reflink)**. By default the Chart prepares this disk with a **loopback image file**:

| values path | Default | Description |
| --- | --- | --- |
| `bootstrap.nodeInit.dataCubelet.loopback.enabled` | `true` | When `true`, bootstrap creates and mounts a loopback XFS on the node |
| `bootstrap.nodeInit.dataCubelet.loopback.imagePath` | `/data/cubelet-xfs.img` | Path to the image file |
| `bootstrap.nodeInit.dataCubelet.loopback.size` | `25G` | Size when the image is **first created** (`truncate -s`) |

For trials or when the node has no ready XFS data disk, adjust the size in `runtime-values.yaml`, for example:

```yaml
bootstrap:
  nodeInit:
    dataCubelet:
      loopback:
        enabled: true
        size: 200G   # Size to capacity plan; must be less than free space on the filesystem holding the image
```

::: warning Only affects the “first” creation
`cube-node-init` creates the file by `size` only when **`/data/cubelet-xfs.img` does not yet exist**. Changing values after the image exists **does not** auto-expand. To grow, you need a maintenance window and manual steps (e.g. unmount, delete the old img, recreate — **this loses `/data/cubelet` data**), or use a “pre-provisioned XFS disk” below.
:::

For production, prefer mounting a dedicated XFS disk to `/data/cubelet` on compute nodes and disabling loopback:

```yaml
bootstrap:
  nodeInit:
    dataCubelet:
      loopback:
        enabled: false
```

---

## 6. Helm install

From the repository root:

```bash
# Standard multi-node
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system \
  --create-namespace \
  -f runtime-values.yaml \
  --wait \
  --timeout 90m
```

Tencent Cloud TKE:

```bash
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system \
  --create-namespace \
  -f deploy/kubernetes/chart/values-tke.yaml \
  -f runtime-values.yaml \
  --wait \
  --timeout 90m
```


Single-node trial:

```bash
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system \
  --create-namespace \
  -f deploy/kubernetes/chart/values-single-node.yaml \
  -f runtime-values.yaml \
  --wait \
  --timeout 90m
```



Install may take a while (first-time PVM kernel swap, node reboots, and cold starts all count toward the timeout). `--wait` waits until the main workloads are Ready.

---

## 7. Verify the deployment

```bash
# 1) Are Pods Ready?
kubectl get pods -n cube-system -o wide

# 2) Have compute nodes registered with CubeMaster?
kubectl exec -n cube-system cloneset/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'

# 3) Built-in end-to-end tests (a few minutes)
helm test cube -n cube-system --timeout 20m --logs
```

Expect:

- `cube-node` Ready count ≈ number of nodes labeled `cube-node=true`
- `cube-master` / `cube-ops` / `cube-api` / `cube-proxy` / `cube-webui` Ready
- Built-in MySQL / Redis (if enabled) Ready
- `helm test` passes

Open the WebUI (default `http://<control-plane-node-HostIP>:12088`) and start using it.

---

## 8. Uninstall

```bash
helm uninstall cube -n cube-system
kubectl delete namespace cube-system
```

Uninstall **does not** automatically clean up:

- Cube labels / role taints on nodes
- Compute-node hostPath data (e.g. `/data/cubelet`, `/usr/local/services/cubetoolbox`, etc.)
- PVM host kernel changes (roll back per platform runbook)
- External MySQL / Redis, DNS / LB records

---

## FAQ quick reference

| Symptom | What to do |
| --- | --- |
| `kruise-controller-manager` Pending | Did you apply `control`/`compute` taints first? Remove the taints or add tolerations to the manager; install Kruise, then re-apply taints |
| Control-plane Pods stay Pending | By default nodes need `cube-control=true` (or your custom nodeSelector); also check taints / tolerations |
| `cube-node` DESIRED=0 | Are compute nodes labeled `cube-node=true`? |
| Helm reports `CHANGE_ME_*` / validation failure | Check that passwords and `placement` in `runtime-values.yaml` are complete |
| First install is slow / nodes reboot | Expected when PVM is enabled; wait for fingerprints to be ready and gates cleared, then check bootstrap / Big Pod |

More detail in the other docs in this directory:

- [Architecture](./architecture.md)
- [Upgrade](./upgrade.md)
- [FAQ](./faq.md)

---

## Next steps

- [Kubernetes Deployment Overview](./index.md)
- [Architecture](./architecture.md)
- [Upgrade](./upgrade.md)
- [FAQ](./faq.md)
- [Connect to an Existing Cube Cluster](../connect-existing-cluster.md)
- [WebUI Console](../webui.md)
- [Network Hardening](../network-hardening.md)
- [Authentication](../authentication.md)
