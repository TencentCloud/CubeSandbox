# Helm Install

Complete a Helm install of CubeSandbox on an existing Kubernetes cluster.


## Install process

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


## 1. Prerequisites

| Item | Requirement |
| --- | --- |
| Kubernetes | **≥ v1.24+** |
| Tools | `kubectl`, Helm v3.10+ |
| Storage | A usable StorageClass in the cluster |
| Image pull | Nodes can reach the Internet to pull images |

### Node roles

CubeSandbox uses **labels** to distinguish two kinds of nodes:

| Role | Suggested size | Purpose |
| --- | --- | --- |
| **Control-plane node** | ≥ 1; production suggests 3+; ≥ 4C8G | Runs Cube's control-plane components |
| **Compute node** | ≥ 1; suggest 16C32G+ | Runs sandboxes |

::: tip Recommendation
Use dedicated machines for compute nodes.
:::

::: details What is PVM? (Technical background)
PVM (Pagetable-based Virtual Machine) is a **pagetable-based nested virtualization framework** built on top of KVM. Unlike traditional nested virtualization, PVM does not rely on the host hypervisor exposing Intel VT-x / AMD-V hardware virtualization extensions to the guest. Instead, it performs privilege-level switching and memory virtualization inside the guest kernel layer through shared-memory regions and shadow page tables, and is fully transparent to the host hypervisor.

PVM was originally proposed in the paper [*PVM: Efficient Shadow Paging for Deploying Secure Containers in Cloud-native Environment*](https://dl.acm.org/doi/10.1145/3600006.3613158). Tencent Cloud has since done substantial feature and performance work on top of it, fixed many bugs, and open-sourced the upstreamed kernel changes in [OpenCloudOS Kernel](https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git) for the community. Over the years we have deployed a large fleet of PVM instances in Tencent Cloud production, and its reliability has been validated in production.
:::


---

## 2. Confirm the cluster is ready

```bash
kubectl get nodes -o wide
kubectl get storageclass # Must not be empty
helm version --short # ≥ v3.10
```

Note the node names you will use later. Confirm at least one node for the control plane and another for the compute plane.


## 3. Label nodes and roles

### 3.1 Multi-node (control / compute split, recommended)

Under normal circumstances, add at least 2 machines to Kubernetes and deploy with the control plane and compute plane separated:

Run the following commands to label the relevant nodes:

```bash
# Control-plane nodes
kubectl label nodes <control-node> cube.tencent.com/cube-control=true --overwrite

# Compute nodes (run sandboxes)
kubectl label nodes <compute-node> cube.tencent.com/cube-node=true --overwrite

# If the compute node is not a bare-metal server, you also need to apply this label, and PVM will be installed automatically
kubectl label nodes <pvm-compute-node> cube.tencent.com/allow-pvm-bootstrap=true --overwrite
```

Set role taints (to keep unrelated workloads off these nodes):

```bash
# Optional
# kubectl taint nodes <control-node> cube.tencent.com/control=true:NoSchedule --overwrite

# Required for compute nodes
kubectl taint nodes <compute-node> cube.tencent.com/compute=true:NoSchedule --overwrite
```

### 3.2 Single-node (not recommended)

::: tip Hint

For a single-node setup, we recommend deploying via [Quick Start](../quickstart.md) instead of Kubernetes.
:::

::: details Single-node labels and taints
If your Kubernetes cluster has only one machine, you can try the following labels and taints (not recommended):

```bash
export NODE=<your-node-name>

kubectl label nodes "$NODE" \
  cube.tencent.com/cube-control=true \
  cube.tencent.com/cube-node=true \
  cube.tencent.com/allow-pvm-bootstrap=true \
  --overwrite

kubectl taint nodes "$NODE" cube.tencent.com/control=true:NoSchedule --overwrite
kubectl taint nodes "$NODE" cube.tencent.com/compute=true:NoSchedule --overwrite
```
:::


## 4. Prepare the values file

Create the configuration file:

```bash
cp deploy/kubernetes/chart/runtime-values.example.yaml runtime-values.yaml
```

Edit it to fit your needs. At minimum, fill in the following:

```yaml
cubeProxy:
  advertiseIP: "10.0.1.10"
  domain: "cube.app"
  tls:
    mode: selfSigned         # Trial; production: existingSecret / certManager

# Required
mysql:
  host: ""                   # Empty = use built-in MySQL
  password: "replace-me-mysql-password"
  rootPassword: "replace-me-mysql-root-password"
redis:
  host: ""
  password: "replace-me-redis-password"
```



### More configuration

**Control-plane PVC configuration**

See [8.1 Control-plane PVC configuration](#_8-1-control-plane-pvc-configuration).

**Compute node data disk**

Cube writes data to the host at `/data/cubelet`, which must be an **XFS** filesystem. For a smoother first-time experience, we create a 25 GB **loopback image file** and mount it by default.

For production deployments, or to adjust related configuration, see [8.2 Compute node data disk configuration](#_8-2-compute-node-data-disk-configuration).


## 5. Helm install

::: tip Users in Mainland China
Add `-f deploy/kubernetes/chart/values-cn.yaml` to whichever install command you use below to pull images from the China registry.
:::


Run the following commands from the repository root to complete the install:

Because this process downloads many resources and runs initialization actions, it can take some time depending on your machine performance and network conditions.

Standard multi-node deployment:

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


## 6. Verify the deployment

```bash
# 1) Are Pods Ready?
kubectl get pods -n cube-system -o wide

# 2) Have compute nodes registered with CubeMaster?
kubectl exec -n cube-system deploy/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'

# 3) Built-in end-to-end tests (a few minutes)
helm test cube -n cube-system --timeout 20m --logs
```

Expect:

- `cube-node` Ready count = number of nodes labeled `cube-node=true`
- All services in the `cube-system` namespace are Ready
- `helm test` passes

**Access the Cube WebUI:**

```bash
kubectl -n cube-system port-forward svc/cube-webui 12088:12088
# Open http://127.0.0.1:12088 in your browser
```



## 7. Uninstall

```bash
helm uninstall cube -n cube-system
kubectl delete namespace cube-system
```

Uninstall **does not** automatically clean up:

- Cube labels / role taints on nodes
- Compute-node hostPath data
- PVM host kernel changes


## 8. Advanced configuration

### 8.1 Control-plane PVC configuration

- Default: CubeMaster / MySQL / Redis use the cluster **default StorageClass**
- To specify an SC: set `persistence.storageClassName: <name>` in `runtime-values.yaml`
- Single-node / no CSI: switch to hostPath (see comments in `runtime-values.example.yaml`)


### 8.2 Compute node data disk configuration

For **production deployments**, we recommend provisioning a dedicated data disk for each compute node, formatting it as XFS, and mounting it under `/data/cubelet`. This gives the best performance and stability. Then configure `runtime-values.yaml`:

```yaml
bootstrap:
  nodeInit:
    dataCubelet:
      loopback:
        enabled: false
```

If your nodes have no dedicated data disk, by default we mount a 25 GB loop device at `/data/cubelet` during compute-node initialization.

::: warning Only affects the "first" creation
By default, the disk image file is created automatically only when **`/data/cubelet-xfs.img` does not exist**. Subsequent config changes **do not** auto-expand. To resize, manual steps in a maintenance window are required.
:::

To customize the data disk configuration before install:

| values path | Default | Description |
| --- | --- | --- |
| `bootstrap.nodeInit.dataCubelet.loopback.enabled` | `true` | When `true`, bootstrap creates and mounts a loopback XFS on the node |
| `bootstrap.nodeInit.dataCubelet.loopback.imagePath` | `/data/cubelet-xfs.img` | Path to the image file |
| `bootstrap.nodeInit.dataCubelet.loopback.size` | `25G` | Size when the image is **first created** (`truncate -s`) |

```yaml
bootstrap:
  nodeInit:
    dataCubelet:
      loopback:
        enabled: true
        size: 200G   # Size to capacity plan; must be less than free space on the filesystem holding the image
```


---

## FAQ quick reference

| Symptom | What to do |
| --- | --- |
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
- [Authentication](../authentication.md)
