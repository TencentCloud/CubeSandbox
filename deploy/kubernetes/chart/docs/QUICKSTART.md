# 一键安装指南 (Quick Start)

本文档描述如何在 30 分钟内,使用 Helm 把 CubeSandbox v0.5.1 完整部署到一个 Kubernetes/TKE 集群。适合首次接触 chart 的交付人员、SRE 或用户。

如果你需要理解组件关系、启动流程、DNS/Proxy/Egress 数据流,请阅读 [`ARCHITECTURE.md`](ARCHITECTURE.md);计算面镜像升级见 [`UPGRADE.md`](UPGRADE.md);运行中遇到问题请查阅 [`FAQ.md`](FAQ.md)。

---

## 1. 前置条件

### 1.1 集群要求

| 项目 | 最低要求 | 说明 |
| --- | --- | --- |
| Kubernetes / TKE | v1.24+ | 建议使用 TKE 托管集群或 TKE Serverless(节点池模式) |
| OpenKruise | 1.9+（**必需**） | Big Pod InPlace、bootstrap/installer ADS、控制面 CloneSet 硬依赖；需 CRD 与 kruise-daemon。**PVM 为原生 DaemonSet**，创建不依赖 kruise-manager。安装见下文 §1.4 |
| kubectl | 与集群同大版本 | 确保 `kubectl get nodes` 能正常返回 |
| Helm | v3.10+ | `helm version` 应可返回 |
| Docker(仅打包镜像时) | v20+ | 生产集群只需能 pull 已推送的镜像 |
| 集群 StorageClass | PVC 路径需要 | 默认**不**创建 SC；三个控制面 PVC 走集群 default SC。无 default SC 时显式设 `persistence.storageClassName`，或改用 hostPath（见 §6） |

### 1.2 节点要求

CubeSandbox 采用**控制面 / 计算面**分离部署,两组节点在 K8s 中通过 label 区分。

| 节点角色 | 数量 | 规格建议 | 需要具备 |
| --- | --- | --- | --- |
| Control 节点 | 至少 1 台(生产 3+) | 4C8G + 100G 持久盘 | 常规 K8s 节点 |
| Compute 节点 | 至少 1 台 | 16C32G+ | **KVM 支持**(`/dev/kvm` 存在)、XFS 数据盘、支持内核替换(可选 PVM host kernel) |

### 1.3 打节点 label

Chart **不会自动打 label**，请部署前手动执行：

```bash
# 控制面节点
kubectl label nodes <control-node-1> cube.tencent.com/cube-control=true

# 计算节点（运行 cube-node / bootstrap / installer）
kubectl label nodes <compute-node-1> cube.tencent.com/cube-node=true

# 需要 PVM 宿主机内核的节点额外授权（才会调度 cube-node-pvm、拉取 PVM 镜像）
kubectl label nodes <pvm-compute-node> cube.tencent.com/allow-pvm-bootstrap=true
```

- compute 节点：打 `cube-node=true` 即可跑 Big Pod / bootstrap / installer；控制面只认独立的 `cube-control=true`。
- **仅**需要替换 host kernel 的节点再打 `allow-pvm-bootstrap=true`；不打则不会拉 `cube-pvm-host-bootstrap`。
- Label **不挡调度**，可在装 OpenKruise **之前或之后**打。
- **角色污点** `control` / `compute` 须在 OpenKruise Ready **之后**再打（见 §1.5）。临时门闩 `pvm-not-ready` 由 Helm preflight 在需要时写入，不要手动预打。
- 混部：部分 compute 打 allow-pvm，其余不打即可。

### 1.4 安装 OpenKruise（计算面原地升级）

> **硬约束：** 先完成本节并确认 Ready，再打 §1.5 的角色污点。若先打 `control`/`compute` 再装 Kruise，`kruise-controller-manager` 通常因 untolerated taint 一直 Pending。

```bash
helm repo add openkruise https://openkruise.github.io/charts/
helm repo update
helm install kruise openkruise/kruise --version 1.9.0 \
  --set featureGates='ImagePullJobGate=true\,InPlaceWorkloadVerticalScaling=true'
kubectl get pods -n kruise-system
kubectl -n kruise-system get deploy kruise-controller-manager
kubectl -n kruise-system get ds kruise-daemon
kubectl api-resources | grep kruise.io
# 期望能看到 daemonsets ... apps.kruise.io 与 clonesets ... apps.kruise.io
```

安装命令不配置 `manager.tolerations`。OpenKruise 1.9 的 `kruise-daemon` 模板默认已有无条件 `Exists` toleration。安装完成后确认 manager Ready、daemon 全部 Ready 和 ADS/CloneSet API 可发现。Chart preflight 硬门禁：Kruise CRD、manager Ready、daemon Ready 且能容忍门闩、逐节点 CNI/指纹。门闩期间若删掉并重建 manager，且节点带有其不容忍的污点，manager 可能 Pending——此时可为 manager 配置 `NoSchedule` + `operator: Exists`（可选，重建韧性）。OpenKruise 为硬依赖，不可关闭。

### 1.5 角色污点（Kruise Ready 之后）

```bash
# 控制面节点
kubectl taint nodes <control-node-1> cube.tencent.com/control=true:NoSchedule --overwrite

# 计算节点
kubectl taint nodes <compute-node-1> cube.tencent.com/compute=true:NoSchedule --overwrite
```

### 1.6 启用 PVM 前的系统组件检查

启用 PVM 前，确认 CNI、kube-proxy、**kruise-daemon** 对 `NoSchedule` 使用 `operator: Exists`（或显式容忍 `cube.tencent.com/pvm-not-ready`），并在目标节点上仍为 Running。

Helm preflight Hook 在指纹未就绪的 PVM 节点上写入 `cube.tencent.com/pvm-not-ready=true:NoSchedule`，再探针 CNI。指纹已匹配的日常升级不写入该污点。主动换核见 [`UPGRADE.md`](UPGRADE.md) 的 `value=maintenance`。

---

## 2. 准备镜像(可跳过)

若使用官方 v0.5.1 镜像,跳过本章,直接进入"安装"。若需自建镜像,一条命令构建 + 推送:

```bash
docker login ccr.ccs.tencentyun.com   # 或你的私有 registry

PUSH=1 \
REGISTRY=ccr.ccs.tencentyun.com/<your-namespace> \
IMAGE_TAG=v0.5.1 \
./deploy/kubernetes/images/build-cube-images.sh
```

脚本会自动:

1. 从 SourceForge 下载 `cube-sandbox-one-click-v0.5.1-amd64.tar.gz`(约 245MB)+ PVM host kernel RPM/DEB
2. 基于 `CubeMaster/CubeAPI/CubeProxy/CubeEgress` 源代码构建 10 个镜像
3. 推送到 `${REGISTRY}` 下,tag 为 `${IMAGE_TAG}`

**arm64 构建**:`ONE_CLICK_ARCH=arm64` 触发下载 arm64 一体化包。

**离线场景**:预先下载 tarball / kernel 包放到 `${BUILD_ROOT}/downloads/` 下,脚本会自动跳过下载步骤。详见 [`../images/README.md`](../images/README.md)。

---

## 3. 准备 values.yaml

复制示例并改成本地文件(完整注释见 [`runtime-values.example.yaml`](../runtime-values.example.yaml);生产请按 [`ARCHITECTURE.md#7`](ARCHITECTURE.md#7-关键-values-开关) 调整):

```bash
cp deploy/kubernetes/chart/runtime-values.example.yaml runtime-values.yaml
```

```yaml
# 镜像默认指向公网 CCR;私有镜像可用 global.imageRegistry 一次改写。

# StorageClass —— 默认不创建; 3 个 PVC 走集群 default SC。
# 显式指定(一处即可):
# persistence:
#   storageClassName: local-path   # 或 gp3 / premium-rwo / managed-csi
#
# TKE: 叠加 values-tke.yaml(创建 cube-cbs-wffc + CLB),见 §6.4。

cubeProxy:
  advertiseIP: "10.0.1.10"   # control 节点 HostIP;无 Ingress 时也可关 ingress.enabled
  domain: "cube.app"
  tls:
    mode: selfSigned

# 必填: validate.yaml 拒绝 values.yaml 里的 CHANGE_ME_* 哨兵
mysql:
  host: ""
  password: "replace-me-mysql-password"
  rootPassword: "replace-me-mysql-root-password"
redis:
  host: ""
  password: "replace-me-redis-password"
```

---

## 4. 一键安装

```bash
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system \
  --create-namespace \
  -f runtime-values.yaml \
  --wait \
  --timeout 90m
```

**关键参数说明**:

- `--wait` 会等到 CloneSet / Advanced DaemonSet / 原生 DaemonSet / StatefulSet ready 才返回
- `--timeout 90m` 给足 host kernel 安装 + 节点重启 + microVM 首次冷启动的时间
- 若 host kernel 需要重启节点,DaemonSet 会自动重试,不需要人工介入

安装过程中的关键节点:

1. **秒级**：preflight 验证 Kruise、CNI/apiserver 路径，以及每个 PVM 节点“已有门闩或 live 指纹已就绪”。
2. **首次 5-15 分钟**：门闩节点只运行 `cube-node-pvm`（原生 DS）与系统组件；同节点控制面、bootstrap、installer、Big Pod 保持 Pending。PVM 可能 reboot，live 指纹验证后清闩。
3. **清闩后**：MySQL/Redis、控制面 CloneSet 与其余三条 ADS 调度；bootstrap 写 `node-prep-ready`，Big Pod 注册到 CubeMaster。多节点分离部署中，未被门闩影响的控制面可与 PVM 并行启动。
4. **之后**：不改变 kernel/boot args 的常规镜像升级不需要重新打门闩。

---

## 5. 验证部署

```bash
# 1. 所有 Pod 都 Ready
kubectl get pods -n cube-system -o wide

# 2. compute 节点已注册到 CubeMaster
kubectl exec -n cube-system cloneset/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'

# 3. 运行 Helm 内置端到端测试(约 5 分钟)
helm test cube -n cube-system --timeout 20m --logs
```

**期望结果**:

- `cube-node` DaemonSet Ready 数量 = 打了 compute label 的节点数量
- `cube-master` / `cube-ops` / `cube-api` / `cube-proxy` / `cube-webui` CloneSet Ready
- `cube-mysql` / `cube-redis` StatefulSet Ready(若使用内置)
- `helm test` 全绿

登录 WebUI（默认 `http://<control-node-hostip>:12088`）开始使用。WebUI 将 `/opsapi` 与 SDK 路径反代到 CubeOps；外部 E2B 客户端使用 CubeAPI。

---

## 6. 常见部署模式

### 6.1 单节点试用(仅测试)

完整逐步教程与**正确安装顺序**见 [`SINGLE-NODE-HELM.md`](SINGLE-NODE-HELM.md)（§2 label → §3 OpenKruise Ready → §4 角色污点 → §5 CNI 检查）。

一台机器既做 control 又做 compute，使用独立双 label、角色污点 toleration 并集和单节点 values。**硬约束：先装 OpenKruise 并确认 manager/daemon Ready + CRD，再打角色污点 `control`/`compute`。** Label 可在装 Kruise 前或后打；先打角色污点再装 Kruise 会导致 manager Pending。OpenKruise 安装命令不配置 `manager.tolerations`（与 §1.4 / SINGLE-NODE-HELM §3 一致）。

```bash
# 1) label（可在 Kruise 前/后）
kubectl label nodes <node> \
  cube.tencent.com/cube-control=true \
  cube.tencent.com/cube-node=true \
  cube.tencent.com/allow-pvm-bootstrap=true

# 2) 安装 OpenKruise 并确认 Ready（见 §1.4 / SINGLE-NODE-HELM §3）——必须先于角色污点

# 3) 角色污点（仅在 Kruise Ready 之后；见 §1.5）
kubectl taint nodes <node> cube.tencent.com/control=true:NoSchedule --overwrite
kubectl taint nodes <node> cube.tencent.com/compute=true:NoSchedule --overwrite

# 4) 确认 CNI/kube-proxy/kruise-daemon 能容忍 NoSchedule（见 §1.6 / SINGLE-NODE-HELM §5）
#    pvm-not-ready 由 helm install 的 preflight Hook 在需要时写入
# 单节点试用若需要 PVM，保留 allow-pvm；纯 BM 可去掉 allow-pvm 并设 bootstrap.pvmHostKernel.enabled=false
```

安装时追加 `-f deploy/kubernetes/chart/values-single-node.yaml`。门闩存在时，CNI、kube-proxy 和 **kruise-daemon** 必须以 `operator: Exists`（或显式 key）容忍 `NoSchedule`，否则 PVM 无法访问 apiserver 清闩。Chart preflight 要求 manager Ready；安装命令不配置 manager Exists。

后续扩容纯计算节点时只打 `cube-node=true`（可选 `allow-pvm-bootstrap=true`）和 `compute` 污点，**不要**打 `cube-control=true`；控制面因此不会漂到新计算节点。

`runtime-values.yaml` 中把持久化改成 hostPath 以避免依赖任何 CSI:

```yaml
controlPlane:
  master:
    persistence:
      enabled: true
      hostPath: /data/CubeMaster/storage
mysql:
  persistence:
    hostPath: /data/mysql
redis:
  persistence:
    hostPath: /data/redis
```

### 6.2 Compute-only 模式(共用外部控制面)

多个集群共用同一套外部 CubeMaster:

```yaml
controlPlane:
  enabled: false
externalControlPlane:
  enabled: true
  masterEndpoint: <external-master-host>:8089
  apiEndpoint: http://<external-api-host>:3000   # 可选
cubeProxy:
  enabled: false   # 通常由外部集群提供
webui:
  enabled: false
mysql:
  enabled: false
redis:
  enabled: false
```

### 6.3 Self-hosted 集群 / EKS / GKE / AKS

Chart 的默认值就是 self-hosted 友好:
- **不创建 StorageClass** — 3 个 PVC(CubeMaster / MySQL / Redis)使用集群 default SC
- **不硬编码 provisioner** — 兼容任何 CSI 后端

若集群没有 default SC 或想显式指定,在 `runtime-values.yaml` 里配一处即可:

```yaml
persistence:
  storageClassName: <your-sc-name>   # 例如 local-path / gp3 / premium-rwo / managed-csi
```

需要三个 PVC 用不同 SC 时,再分别写 `controlPlane.master.persistence.storageClassName` /
`mysql.persistence.storageClassName` / `redis.persistence.storageClassName`(组件级优先于顶层)。

若集群完全没有可用 SC(纯本地实验环境),把 3 个 PVC 换成 hostPath:

```yaml
controlPlane:
  master:
    persistence:
      hostPath: /data/CubeMaster/storage
mysql:
  persistence:
    hostPath: /data/mysql
redis:
  persistence:
    hostPath: /data/redis
```

镜像 pull 若需要走内部 mirror,一个开关搞定 Cube-owned 10 个镜像:

```yaml
global:
  imageRegistry: my-mirror.example.com/cubesandbox
```

### 6.4 腾讯云 TKE

用 chart 内置的 preset,一行叠加:

```bash
helm upgrade --install cube ./deploy/kubernetes/chart \
  -f deploy/kubernetes/chart/values-tke.yaml \
  -f runtime-values.yaml \
  -n cube-system --create-namespace \
  --timeout 90m
```

`values-tke.yaml` 会:
- 让 chart 创建 `cube-cbs-wffc` StorageClass(provisioner=`com.tencent.cloud.csi.cbs`)
- 把 3 个 PVC 绑定到该 SC
- 使用 `WaitForFirstConsumer` 避免多可用区 CBS 盘错 zone

---

## 7. 卸载

```bash
helm uninstall cube -n cube-system
kubectl delete namespace cube-system
```

**卸载不会自动清理**以下内容(需要手动处理):

- 节点 label / taint(chart 不管理)
- 外部 MySQL / Redis 数据
- compute 节点上的 hostPath 数据:`/data/CubeMaster/storage`, `/data/cubelet`, `/data/cube-shim`, `/data/snapshot_pack`, `/usr/local/services/cubetoolbox`, `/data/log`
- PVM host kernel 修改(GRUB、`/boot`、initramfs)—— 需要按平台 runbook 回滚
- 外部 DNS / LB 记录

---

## 8. 下一步

- 阅读 [`ARCHITECTURE.md`](ARCHITECTURE.md) 深入理解组件关系和数据流
- 阅读 [`UPGRADE.md`](UPGRADE.md) 了解计算面镜像升级（不杀存量沙箱）
- 阅读 [`FAQ.md`](FAQ.md) 应对常见部署 / 运行问题
- 生产环境 TLS、DNS、监控、备份策略请参考主 README 相应章节
