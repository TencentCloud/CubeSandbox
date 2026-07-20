# 计算面镜像升级 Runbook

在 **不销毁存量沙箱**、且 **Big Pod UID / PodIP / 网络命名空间不变** 的前提下升级计算面。架构说明见 [`ARCHITECTURE.md`](ARCHITECTURE.md)。

## 前置：OpenKruise

三条计算面工作负载使用 Advanced DaemonSet（Big Pod 为 `InPlaceIfPossible`，bootstrap/installer 为 `Standard`）；**`cube-node-pvm` 为原生 `apps/v1` DaemonSet**；无状态控制面使用 CloneSet（OpenKruise 硬依赖，用于 ADS + CloneSet）：

```bash
helm repo add openkruise https://openkruise.github.io/charts/
helm repo update

helm install kruise openkruise/kruise --version 1.9.0 \
  --set featureGates='ImagePullJobGate=true\,InPlaceWorkloadVerticalScaling=true'
```

（若已安装可跳过；与 [`QUICKSTART.md`](QUICKSTART.md) §1.4 / [`SINGLE-NODE-HELM.md`](SINGLE-NODE-HELM.md) §3 一致。安装命令不配置 `manager.tolerations`；**先**确认 manager/daemon Ready + CRD，**再**打角色污点 `control`/`compute`。Chart preflight 硬门禁：Kruise CRD、manager Ready、`kruise-daemon` Ready 且能容忍门闩、逐节点 CNI/指纹。门闩下若需删除并重建 manager，可为其配置 `NoSchedule` + `operator: Exists`（重建韧性；可选）。）

## 原理

1. toolbox 整树 hostPath 挂在 `/usr/local/services/cubetoolbox`（Big Pod volumeMount **冻结**）。
2. 计算面 DaemonSet：
   - **`*-node`（Big Pod）**：`wait-node-prep`（Kruise prio 10）+ `network-agent` / `cubelet`（self-stage）+ 可选 egress + **6 个冻结 `cube-slot-*` pause 占位**；**零 init**；日常只 **InPlace** bump **containers** 镜像 / slot annotation / slot resources。
   - **`*-node-installer`**：Standard ADS；shim / kernel / guest 安装；可增容器。
   - **`*-node-bootstrap`**：Standard ADS；`wait-pvm-host` / node-init / 写 `node-prep-ready`。
   - **`*-node-pvm`**（可选）：原生 `apps/v1` DaemonSet；PVM host kernel + startup gate；仅 `placement.pvm`。
3. Bootstrap 写「节点可启动」；有 PVM label 的节点写「宿主机 PVM 就绪」。Installer / Big Pod 把组件换到 toolbox：替换过程中版本矩阵可能短暂显示未完成（正常）；成功后组件就绪，矩阵恢复完整。
4. **NodeID** = `spec.nodeName`；**Endpoint** = Big Pod `status.podIP`。
5. `preStop` 只杀本容器 pidfile，禁止宽 `pkill -f`。
6. 日常分工：**升运行时 → 只 bump Big Pod 镜像**；**升产物 → 只动 Installer**；**升 node-init → 只动 Bootstrap**；**升 PVM → 只动 cube-node-pvm**。
7. 升 kernel **不会**因为 Chart 默认值把已在跑 PVM 的节点悄悄切成 bm；要关某节点 PVM，去掉其 `allow-pvm-bootstrap` label。
8. `startupGate` 开启时，pre-upgrade Hook 对指纹未就绪的 PVM 节点写入 `pvm-not-ready=true` 并探针 CNI。指纹已匹配的日常升级不写入该污点；主动换核见下方 `maintenance`；运行时换核路径会先 ensure taint、删除本节点依赖 Pod，再 invalidate/reboot。

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

验收：Big Pod **UID / PodIP 不变**；运行时镜像升级事件含 `SuccessfulUpdatePodInPlace`。

改 `bootArgs`、`prepGeneration` 或 gate 策略前可运行 `sh deploy/kubernetes/chart/scripts/test-big-pod-inplace-guard.sh`；该守卫要求这些策略变化对 Big Pod Pod template 零 diff。

### 改 kernel pattern / boot args（会 reboot）

pre-upgrade Hook 按**新 values**检查指纹。指纹已匹配的日常镜像升级不写入 `pvm-not-ready`。

**主动改变** kernel pattern / boot args（期望指纹会变）时，建议在 Helm 前打运维门闩 `value=maintenance`（与 Hook 自动打的 `true` 不同：旧 hold 默认不会清 maintenance）：

```bash
# 1. 确认 CNI、kube-proxy、kruise-daemon 能容忍该 NoSchedule 污点
# value=maintenance tells the currently running old reconciler not to clear it
kubectl taint node <pvm-node> cube.tencent.com/pvm-not-ready=maintenance:NoSchedule --overwrite

# 2. 再提交新的 kernel pattern / boot args
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f values-tke.yaml -f runtime-values.yaml \
  --set-string bootstrap.pvmHostKernel.bootArgs='nopti pti=off <new-arg>'
```

旧 PVM hold 会保留 `value=maintenance`；preflight 在探针结束后再次确认污点仍在。新 PVM init 随后执行 ensure → PDB-aware eviction/drain → invalidate → Lease → mutate/reboot；任一步失败都不会 reboot。节点恢复后仅新 init 在 live 指纹匹配时清除 maintenance 门闩。指纹仍匹配的纯镜像升级不写入临时污点。

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
| 把 `allow-pvm-bootstrap` 写进 `placement.compute` | validate 失败；且会让所有 compute 拉 PVM 镜像 |

`cubeNode.env`、`cubeNode.podAnnotations`、网络 env、`global.timezone` 和 `cubeEgress.enabled` 会改变冻结 Pod template，只能作为明确安排数据面重建的维护窗口操作。CI 守卫对这些值逐项验证为“可检测的 recreate 变化”，不会把它们误列为 InPlace allowlist；allowlist 仅含容器 image、resources 和 release 管理的 `cube.tencent.com/slot-*` annotation。

## 相关镜像键

| values 键 | 工作负载 | 容器 |
|-----------|----------|------|
| `images.cubelet` | Big Pod | `cubelet`（self-stage） |
| `images.networkAgent` | Big Pod | `network-agent`（self-stage） |
| `images.waitNodePrep` | Big Pod | `wait-node-prep`（Kruise prio 10 sidecar） |
| `images.cubeShim` | Installer | `cube-shim-install` |
| `images.cubeKernel` | Installer | `cube-kernel-install` |
| `images.cubeGuest` | Installer | `cube-guest-install` |
| `images.pvmHostBootstrap` | **cube-node-pvm** | `pvm-host-bootstrap` |
| `images.nodeInit` | Bootstrap | `wait-pvm-host` / `cube-node-init` |
| `images.waitNodePrep` | Bootstrap | `write-node-prep-ready` |
| `images.pvmHostBootstrap` | PVM | `pvm-host-bootstrap` / `hold-pvm-ready` reconcile |

## 卸载后全新安装

```bash
helm uninstall cube -n cube-system
sudo ./deploy/kubernetes/chart/scripts/cleanup-node-host.sh
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system -f values-tke.yaml -f runtime-values.yaml
```
