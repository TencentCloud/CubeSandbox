# 计算面镜像升级 Runbook

在 **不销毁存量沙箱**、且 **Big Pod UID / PodIP / 网络命名空间不变** 的前提下升级计算面。架构说明见 [`ARCHITECTURE.md`](ARCHITECTURE.md)。

## 前置：OpenKruise

计算面使用 Advanced DaemonSet + `InPlaceIfPossible`（OpenKruise 硬依赖）：

```bash
helm repo add openkruise https://openkruise.github.io/charts/
helm repo update

helm install kruise openkruise/kruise --version 1.9.0 \
  --set featureGates='ImagePullJobGate=true\,InPlaceWorkloadVerticalScaling=true'
```

（若已安装可跳过；与 [`QUICKSTART.md`](QUICKSTART.md) §1.4 一致。）

## 原理

1. toolbox 整树 hostPath 挂在 `/usr/local/services/cubetoolbox`（Big Pod volumeMount **冻结**）。
2. 同 `placement.compute`、selector 互斥的三个 DaemonSet：
   - **`*-node`（Big Pod）**：`wait-node-prep`（Kruise prio 10）+ `network-agent` / `cubelet`（self-stage）+ 可选 egress + **6 个冻结 `cube-slot-*` pause 占位**；**零 init**；日常只 **InPlace** bump **containers** 镜像 / slot annotation / slot resources。
   - **`*-node-installer`**：shim / kernel / guest 安装；可 RollingUpdate、可增容器。
   - **`*-node-bootstrap`**：pvm / node-init / 写 `node-prep-ready`；低频 RollingUpdate。
3. Bootstrap 写 `node-prep-ready`；Installer / self-stage 写组件 `.staged-*`；cubelet 等 artifact 与 network-agent sentinel。
4. **NodeID** = `spec.nodeName`；**Endpoint** = Big Pod `status.podIP`。
5. `preStop` 只杀本容器 pidfile，禁止宽 `pkill -f`。
6. 日常分工：**升控面 → 只 bump Big Pod 镜像**；**升产物 → 只动 Installer**；**升节点引导 → 只动 Bootstrap**。

## 按组件升级示例

```bash
# 只升 cubelet（Big Pod InPlace）
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f values-tke.yaml -f runtime-values.yaml \
  --set images.cubelet.tag=v0.5.1

# 只升 shim（仅 Installer）
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f values-tke.yaml -f runtime-values.yaml \
  --set images.cubeShim.tag=v0.5.1

# 只升 node-init（仅 Bootstrap；Big Pod UID/IP 应不变）
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f values-tke.yaml -f runtime-values.yaml \
  --set images.nodeInit.tag=v0.5.1
```

验收：Big Pod **UID / PodIP 不变**；控面升级事件含 `SuccessfulUpdatePodInPlace`。

## 控制面：`cube-proxy` 资源重命名

若从仍使用 `*-proxy-node` 的旧 Chart 升级：Deployment / Service 名变为 `*-proxy`，Helm 会删旧建新（Proxy 短暂中断一次）；Ingress 与集群 DNS rewrite 目标随之更新。Proxy Pod 无业务持久化数据。

## 不要当常规升级做的事

| 操作 | 后果 |
|------|------|
| 改 Big Pod `containers` 数量 / volumeMount / 直接改 env | recreate → IP/netns 变 |
| 改 `wait-node-prep` 的 env / volumeMount | Big Pod recreate |
| 仅 bump `images.waitNodePrep`（不改 env/mount） | **InPlace**（wait 为 sidecar） |
| Big Pod `rollingUpdateType: Standard` / 删 Big Pod | 数据面中断 |
| 把新产物 install 塞进 Big Pod | 破坏冻结契约（应加在 Installer） |
| 把 pvm / node-init 并进 Installer | 日常升 shim 可能 reboot / 扩大 hostPID 面 |

## 相关镜像键

| values 键 | 工作负载 | 容器 |
|-----------|----------|------|
| `images.cubelet` | Big Pod | `cubelet`（self-stage） |
| `images.networkAgent` | Big Pod | `network-agent`（self-stage） |
| `images.waitNodePrep` | Big Pod | `wait-node-prep`（Kruise prio 10 sidecar） |
| `images.cubeShim` | Installer | `cube-shim-install` |
| `images.cubeKernel` | Installer | `cube-kernel-install` |
| `images.cubeGuest` | Installer | `cube-guest-install` |
| `images.pvmHostBootstrap` | Bootstrap | `pvm-host-bootstrap` |
| `images.nodeInit` | Bootstrap | `cube-node-init` |
| `images.waitNodePrep` | Bootstrap | `write-node-prep-ready`（同镜像） |

## 卸载后全新安装

```bash
helm uninstall cube -n cube-system
sudo ./deploy/kubernetes/chart/scripts/cleanup-node-host.sh
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system -f values-tke.yaml -f runtime-values.yaml
```
