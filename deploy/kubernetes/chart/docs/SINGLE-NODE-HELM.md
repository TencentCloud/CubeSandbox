# 单节点 Helm 部署教程（试用）

本文档把一台机器既做 **control** 又做 **compute** 的试用路径写成可照做步骤。权威摘要见 [`QUICKSTART.md`](QUICKSTART.md) §6.1；调度与污点并集以仓库内 [`values-single-node.yaml`](../values-single-node.yaml) 为准。

> **仅测试 / 小规模试用。** 生产请按控制面 / 计算面分离部署（见 QUICKSTART §1）。

---

## 0. 当前实现要点（读完再动手）

| 项 | 现状 |
| --- | --- |
| `cube-node-pvm` | **原生** `apps/v1` DaemonSet；Pod 由 kube-controller-manager 创建，**不依赖** kruise-manager |
| Big Pod / bootstrap / installer | 仍为 OpenKruise Advanced DaemonSet（`apps.kruise.io`）；控制面为 CloneSet |
| OpenKruise | **硬依赖**（其它面 + preflight）；不可关闭 |
| Preflight | 查 Kruise CRD、**manager Ready**、**kruise-daemon Ready + 能容忍门闩**、逐节点 CNI/指纹 |
| cubevs CIDR | `cubeNode.network.cidr` 默认 `172.16.0.0/18`（避开常见 Service CIDR `192.168.0.0/16`）；`cubeEgress.network.tproxyOnIP` 默认同网关 `172.16.0.1` |
| CIDR preflight | `pre-install`/`pre-upgrade` Hook（weight `-110`）检测沙箱 CIDR 与集群 Service CIDR / ClusterIP 重叠；冲突则 helm 失败 |
| `kruise-controller-manager` tolerations | 安装命令**不**配置 Exists；preflight 只要求 Ready。Exists 为门闩下重建的可选项。门闩场景下 **CNI / kube-proxy / kruise-daemon** 须能容忍 `NoSchedule` |
| **安装顺序（硬约束）** | **先**装 OpenKruise 并确认 manager/daemon Ready + CRD，**再**打角色污点 `control`/`compute`。Label 可前后任意；污点不可提前 |

---

## 1. 前置条件

- Kubernetes v1.24+，`kubectl` / Helm v3.10+ 可用
- 节点具备 `/dev/kvm`（要跑沙箱时）；磁盘空间足够（hostPath 数据目录见下文）
- 能从集群拉取 Chart 所用镜像（公网 CCR 或你自建 registry）

确认节点名：

```bash
kubectl get nodes -o wide
export NODE=<你的节点名>
```

---

## 2. 节点 label

单节点必须同时具备控制面与计算面 **两枚独立 label**（不要用同一个 `role` key 覆盖写）：

```bash
kubectl label nodes "$NODE" \
  cube.tencent.com/cube-control=true \
  cube.tencent.com/cube-node=true \
  cube.tencent.com/allow-pvm-bootstrap=true
```

说明：

- Label **不挡调度**，可在装 OpenKruise **之前或之后**打，顺序无所谓。
- 需要 PVM 宿主机换核时保留 `allow-pvm-bootstrap=true`；纯 BM 试用可去掉该 label，并在 values 里设 `bootstrap.pvmHostKernel.enabled=false`（见 §8）。
- **本步不要**打角色污点 `control`/`compute`，也 **不要**打临时门闩 `pvm-not-ready`（见 §4、§5）。

---

## 3. 安装 OpenKruise（必须在角色污点之前）

> **硬约束：** 先完成本节并确认 Ready，再打 §4 的角色污点。若先打 `control`/`compute` 再装 Kruise，`kruise-controller-manager` 通常因 **untolerated taint** 一直 Pending。

```bash
helm repo add openkruise https://openkruise.github.io/charts/
helm repo update
helm install kruise openkruise/kruise --version 1.9.0 \
  --set featureGates='ImagePullJobGate=true\,InPlaceWorkloadVerticalScaling=true'
```

验收（全部通过后再进入 §4）：

```bash
kubectl get pods -n kruise-system
kubectl -n kruise-system get deploy kruise-controller-manager
kubectl -n kruise-system get ds kruise-daemon
kubectl api-resources | grep kruise.io
# 期望能看到 daemonsets ... apps.kruise.io 与 clonesets ... apps.kruise.io
```

期望：manager Ready ≥ 1、`kruise-daemon` 在该节点 Ready、能发现 Advanced DaemonSet API。

关于 toleration：

- 本教程的 OpenKruise 安装命令**不**配置 `manager.tolerations`。Chart preflight 要求 manager **Ready**，不检查 manager 是否 Exists。
- OpenKruise 1.9 的 **`kruise-daemon` 模板默认已有无条件 Exists**。门闩场景下须确认 daemon 仍能容忍 `NoSchedule`（preflight **会硬查**）。
- 门闩期间若删掉并重建 manager，且节点带有其不容忍的污点，manager 可能 Pending——此时可为 manager 配置 `NoSchedule` + `operator: Exists`（可选，重建韧性）。

---

## 4. 角色污点（Kruise Ready 之后）

OpenKruise 已 Ready 后，再打两把长期角色污点：

```bash
kubectl taint nodes "$NODE" cube.tencent.com/control=true:NoSchedule --overwrite
kubectl taint nodes "$NODE" cube.tencent.com/compute=true:NoSchedule --overwrite
```

说明：

- `values-single-node.yaml` 给 control / compute / pvm 三类负载注入了 **control + compute 污点 toleration 并集**，否则混部会 Pending。
- **污点必须在 Kruise Ready 之后**；与 §2 的 label 不同，污点会挡调度。
- 临时门闩 `pvm-not-ready` 由 Helm preflight 在指纹未就绪时写入；安装顺序见 §3 → §5。

---

## 5. 确认 CNI / kube-proxy / kruise-daemon

在目标节点上确认这些组件对任意 `NoSchedule`（或显式 `cube.tencent.com/pvm-not-ready`）使用 `operator: Exists`（或等价显式 key），并在节点上仍为 Running：

```bash
# 按集群实际命名空间调整（常见 kube-system）
kubectl -n kube-system get pods -o wide | grep -E "$NODE|NAME"
kubectl -n kruise-system get pods -o wide | grep "$NODE"

# 抽查 tolerations（示例：kube-proxy DaemonSet、你的 CNI DS、kruise-daemon）
kubectl -n kube-system get ds -o yaml | grep -A2 -E 'tolerations:|operator:|effect:' | head -80
kubectl -n kruise-system get ds kruise-daemon \
  -o jsonpath='{range .spec.template.spec.tolerations[*]}{.key}{"|"}{.operator}{"|"}{.effect}{"\n"}{end}'
```

若 CNI 或 kube-proxy 在门闩下无法调度，PVM 无法访问 apiserver 清闩。

指纹未就绪时，Helm `pre-install` / `pre-upgrade` preflight Hook 会在 `placement.pvm` 节点写入 `cube.tencent.com/pvm-not-ready=true:NoSchedule`，再探针 CNI。指纹已匹配的日常升级不写该污点。主动改 kernel / boot args 使用 `value=maintenance`，见 [`UPGRADE.md`](UPGRADE.md)。

---

## 6. 准备本地 `runtime-values.yaml`

**不要**直接拿仓库里已有的 `deploy/kubernetes/chart/runtime-values.yaml` 当通用模板——那是本仓库 TCR / 环境专用覆盖，常含私有镜像、安全组、CBS SC 等，**不是**通用必选文件。

请从 **example** 复制到工作目录再改：

```bash
# 在仓库根目录执行
cp deploy/kubernetes/chart/runtime-values.example.yaml ./runtime-values.yaml
```

单节点试用建议用 hostPath，避免依赖 CSI / default StorageClass：

```yaml
# ./runtime-values.yaml（在 example 基础上修改）

cubeProxy:
  advertiseIP: "<本机可达 IP，通常为节点 HostIP>"
  domain: "cube.app"
  tls:
    mode: selfSigned

# 必填：不能保留 values.yaml 里的 CHANGE_ME_* 哨兵
mysql:
  host: ""
  password: "replace-me-mysql-password"
  rootPassword: "replace-me-mysql-root-password"
  persistence:
    hostPath: /data/mysql
redis:
  host: ""
  password: "replace-me-redis-password"
  persistence:
    hostPath: /data/redis

controlPlane:
  master:
    persistence:
      enabled: true
      hostPath: /data/CubeMaster/storage
```

按需改密码与 `advertiseIP`。完整注释见 [`runtime-values.example.yaml`](../runtime-values.example.yaml)。

---

## 7. Helm 安装

在仓库根目录：

```bash
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system \
  --create-namespace \
  -f ./runtime-values.yaml \
  -f ./deploy/kubernetes/chart/values-single-node.yaml \
  --wait \
  --timeout 90m
```

`-f` 顺序：先本地 runtime，再 `values-single-node.yaml`（后者覆盖 placement / gate 相关项）。

`values-single-node.yaml` 会：

- 让 control / compute / pvm 三类负载都带上 control+compute 污点 toleration
- 打开 PVM startup gate，并 `requirePreflight: true`

安装阶段大致顺序：

1. **秒级**：cubevs CIDR preflight（沙箱网段 vs Service CIDR / ClusterIP），再 PVM startup-gate preflight（Kruise Ready、daemon tolerate、CNI/指纹）
2. **首次约 5–15 分钟**：门闩节点上优先跑 `cube-node-pvm`（原生 DS）与系统组件；同节点控制面 / bootstrap / installer / Big Pod 常 Pending；可能 reboot，指纹就绪后清闩
3. **清闩后**：MySQL/Redis、CloneSet、其余三条 ADS 调度；bootstrap 写 `node-prep-ready`，Big Pod 注册 CubeMaster

沙箱网段由 `cubeNode.network.cidr` 写入 Cubelet（默认 `172.16.0.0/18`）。若与集群 Service CIDR（例如 `192.168.0.0/16`，kube-dns ClusterIP 常落在该段）重叠，`cube-dev` 会黑洞 ClusterDNS，cubelet 无法解析 `cube-master`，节点注册失败。冲突时 Hook 直接失败；可改 `cubeNode.network.cidr`（并同步 `cubeEgress.network.tproxyOnIP` 为该网段网关），仅在明确接受风险时设 `cubeNode.network.cidrSkipConflictCheck=true`。

---

## 8. 纯 BM（不装 PVM host kernel）

若不需要宿主机换核：

```bash
kubectl label nodes "$NODE" cube.tencent.com/allow-pvm-bootstrap-   # 去掉授权
# 不要打 pvm-not-ready 门闩
```

在 `runtime-values.yaml`（或额外 `-f`）中：

```yaml
bootstrap:
  pvmHostKernel:
    enabled: false
```

仍须安装 OpenKruise（Big Pod / bootstrap / installer / CloneSet 依赖）。

---

## 9. 验收

```bash
# Pod 总览
kubectl get pods -n cube-system -o wide

# PVM：原生 DaemonSet（注意是 daemonsets，不是 daemonsets.apps.kruise.io）
kubectl -n cube-system get daemonset -l app.kubernetes.io/component=cube-node-pvm -o wide
kubectl -n cube-system get daemonset -l app.kubernetes.io/component=cube-node-pvm \
  -o jsonpath='{.items[0].apiVersion}{"\n"}'

# 其余计算面：Kruise ADS
kubectl -n cube-system get daemonsets.apps.kruise.io \
  -l 'app.kubernetes.io/component in (cube-node,cube-node-bootstrap,cube-node-installer)'

# 控制面 CloneSet
kubectl -n cube-system get cloneset

# 门闩应已清除（首次 PVM 成功后）
kubectl describe node "$NODE" | grep -E 'pvm-not-ready|Taints:' || true

# 节点已注册
kubectl exec -n cube-system cloneset/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'

# 可选：内置测试
helm test cube -n cube-system --timeout 20m --logs
```

期望：

- `cube-node-pvm` 的 apiVersion 为 `apps/v1`，且 Ready
- `cube-node` / bootstrap / installer ADS Ready
- master / ops / api / proxy / webui CloneSet Ready；内置 MySQL/Redis Ready
- WebUI：`http://<advertiseIP 或 HostIP>:12088`（`/opsapi` 与 SDK 上游为 CubeOps）
- CubeAPI：对外 E2B 兼容 HTTP API

---

## 10. 常见坑

| 现象 | 常见原因 | 处理 |
| --- | --- | --- |
| Helm 失败：`cubevs-cidr preflight` / Service CIDR overlap | `cubeNode.network.cidr` 与集群 Service CIDR 或已有 ClusterIP 重叠 | 改用非重叠私网段（默认 `172.16.0.0/18`），并同步 `cubeEgress.network.tproxyOnIP` |
| Helm preflight 失败：CNI / apiserver | 门闩下 CNI/kube-proxy 无 Exists | 先修系统组件 toleration（§5），再 helm install |
| preflight：`kruise-daemon does not tolerate...` | daemon 缺少 Exists | 给 `kruise-daemon` 补 Exists（或显式容忍门闩 key） |
| manager Pending | 先打了角色污点再装 Kruise | 临时去掉角色污点，或按 §2→§3→§4 重来；本教程不给 manager 配 Exists |
| 查错 GVK：ADS 找不到 pvm | PVM 是原生 DaemonSet | `kubectl get daemonset ... cube-node-pvm` |
| PVC Pending | 无 default SC 又未配 hostPath | 单节点用 §6 的 hostPath |
| 误用仓库内 TCR `runtime-values.yaml` | 私有镜像 / 安全组 / CBS | 从 `runtime-values.example.yaml` 复制本地文件 |
| 指纹已匹配仍写入 `pvm-not-ready` | 阻塞无关升级 | 日常升级不要打该污点；preflight 也不会写 |

排障更多条目见 [`FAQ.md`](FAQ.md)；架构与开关见 [`ARCHITECTURE.md`](ARCHITECTURE.md)。

---

## 11. 后续扩容提示

从单节点扩出**纯计算节点**时：只打 `cube-node=true`（可选 `allow-pvm-bootstrap=true`）和 `compute` 污点，**不要**打 `cube-control=true`。控制面被 `cube-control` nodeSelector 锁在原混部节点，不会漂到新计算节点。
