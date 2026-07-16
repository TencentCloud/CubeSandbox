# 一键安装指南 (Quick Start)

本文档描述如何在 30 分钟内,使用 Helm 把 CubeSandbox v0.5.1 完整部署到一个 Kubernetes/TKE 集群。适合首次接触 chart 的交付人员、SRE 或用户。

如果你需要理解组件关系、启动流程、DNS/Proxy/Egress 数据流,请阅读 [`ARCHITECTURE.md`](ARCHITECTURE.md);计算面镜像升级见 [`UPGRADE.md`](UPGRADE.md);运行中遇到问题请查阅 [`FAQ.md`](FAQ.md)。

---

## 1. 前置条件

### 1.1 集群要求

| 项目 | 最低要求 | 说明 |
| --- | --- | --- |
| Kubernetes / TKE | v1.24+ | 建议使用 TKE 托管集群或 TKE Serverless(节点池模式) |
| OpenKruise | 1.9+（**必需**） | 计算面强依赖 Advanced DaemonSet + InPlaceIfPossible；需 CRD 与 kruise-daemon。安装见下文 §1.4 |
| kubectl | 与集群同大版本 | 确保 `kubectl get nodes` 能正常返回 |
| Helm | v3.10+ | `helm version` 应可返回 |
| Docker(仅打包镜像时) | v20+ | 生产集群只需能 pull 已推送的镜像 |
| 集群 StorageClass | 至少 1 个 | 默认使用 `cube-cbs-wffc`(chart 自动创建),可自定义 |

### 1.2 节点要求

CubeSandbox 采用**控制面 / 计算面**分离部署,两组节点在 K8s 中通过 label 区分。

| 节点角色 | 数量 | 规格建议 | 需要具备 |
| --- | --- | --- | --- |
| Control 节点 | 至少 1 台(生产 3+) | 4C8G + 100G CBS | 常规 K8s 节点 |
| Compute 节点 | 至少 1 台 | 16C32G+ | **KVM 支持**(`/dev/kvm` 存在)、XFS 数据盘、支持内核替换(可选 PVM host kernel) |

### 1.3 打节点 label / taint

Chart **不会自动打 label**,请部署前手动执行:

```bash
# 控制面节点
kubectl label nodes <control-node-1> cube.tencent.com/role=control cube.tencent.com/cube-control=true
kubectl taint nodes <control-node-1> cube.tencent.com/control=true:NoSchedule --overwrite

# 计算节点
kubectl label nodes <compute-node-1> \
  cube.tencent.com/role=compute \
  cube.tencent.com/cube-node=true \
  cube.tencent.com/allow-pvm-bootstrap=true
kubectl taint nodes <compute-node-1> cube.tencent.com/compute=true:NoSchedule --overwrite
```

`allow-pvm-bootstrap=true` 显式授权该节点可以被 chart 替换 host kernel;不打这个 label 就无法作为 compute 节点部署。

### 1.4 安装 OpenKruise（计算面原地升级）

```bash
helm repo add openkruise https://openkruise.github.io/charts/
helm repo update
helm install kruise openkruise/kruise --version 1.9.0 \
  --set featureGates='ImagePullJobGate=true\,InPlaceWorkloadVerticalScaling=true'
kubectl get pods -n kruise-system
kubectl api-resources | grep 'apps.kruise.io.*daemonsets'
```

安装完成后可用 `kubectl get pods -n kruise-system` 与 `kubectl api-resources | grep 'apps.kruise.io.*daemonsets'` 确认。OpenKruise 为计算面硬依赖，不可关闭。

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

创建 `runtime-values.yaml`(下面是**最小可用**示例,生产环境请按 [`ARCHITECTURE.md#7`](ARCHITECTURE.md#7-关键-values-开关) 逐项调整):

```yaml
# 镜像 registry
# - Cube-owned 10 个镜像的默认 repository 已经指向公网可达的
#   ccr.ccs.tencentyun.com/cubesandbox-chart/*, 无需修改即可开箱使用。
# - 若把 chart 镜像镜像到私有 registry, 用一个开关一次改所有 Cube 镜像:
#     global:
#       imageRegistry: my-registry.example.com/mirror
#   会渲染成 my-registry.example.com/mirror/cubesandbox-chart/cube-master:v0.5.1
#   等等, 保留 sub-path (cubesandbox-chart/); 第三方镜像 (mysql/redis/
#   coredns/busybox/curl) 不受影响。
# - 也可以直接改单个镜像的 repository:
#     images:
#       master:
#         repository: my-registry.example.com/mirror/cubesandbox-chart/cube-master
#         tag: v0.5.1

# StorageClass —— 默认不创建, 3 个 PVC (CubeMaster / MySQL / Redis)
# 走集群 default StorageClass。self-hosted / EKS / GKE / AKS 用户
# 不需要额外配置。
#
# 若要显式指定 PVC 使用的 SC:
# controlPlane:
#   master:
#     persistence:
#       storageClassName: premium-rwo   # GKE 示例
# mysql:
#   persistence:
#     storageClassName: gp3             # EKS 示例
# redis:
#   persistence:
#     storageClassName: default
#
# TKE 用户直接叠加 chart 内置的 `values-tke.yaml` 预设:
#   helm upgrade --install cube ./deploy/kubernetes/chart \
#     -f deploy/kubernetes/chart/values-tke.yaml \
#     -f runtime-values.yaml
#
# CubeProxy 对外访问 IP(如果不用 LB,填 control 节点的 HostIP)
cubeProxy:
  advertiseIP: "10.0.1.10"
  domain: "cube.app"
  tls:
    mode: selfSigned   # 试用;生产建议 existingSecret 或 certManager

# 若使用外部 MySQL / Redis,填写这两项;留空则 chart 自动部署内置的 StatefulSet
mysql:
  host: ""
  # host: "mysql.example.com"
  # port: 3306
  # user: cube_user
  # database: cube
redis:
  host: ""
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

- `--wait` 会等到所有 Deployment / DaemonSet / StatefulSet ready 才返回
- `--timeout 90m` 给足 host kernel 安装 + 节点重启 + microVM 首次冷启动的时间
- 若 host kernel 需要重启节点,DaemonSet 会自动重试,不需要人工介入

安装过程中的关键节点:

1. **秒级**:控制面 Secret / ConfigMap / RBAC 创建
2. **1-3 分钟**:MySQL / Redis StatefulSet ready,CubeMaster 完成 schema migration
3. **1-2 分钟**:CubeAPI / CubeProxy / WebUI ready
4. **首次 5-15 分钟**:每 compute 节点上 `cube-node-bootstrap` 执行 `pvm-host-bootstrap`(可能触发重启)→ `cube-node-init` → 写 `node-prep-ready`；Big Pod `wait-node-prep` sidecar（Kruise prio 10）Ready 后主容器启动 → 节点向 CubeMaster 注册
5. **之后**:后续升级/滚动更新只需秒级

---

## 5. 验证部署

```bash
# 1. 所有 Pod 都 Ready
kubectl get pods -n cube-system -o wide

# 2. compute 节点已注册到 CubeMaster
kubectl exec -n cube-system deploy/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'

# 3. 运行 Helm 内置端到端测试(约 5 分钟)
helm test cube -n cube-system --timeout 20m --logs
```

**期望结果**:

- `cube-node` DaemonSet Ready 数量 = 打了 compute label 的节点数量
- `cube-master` / `cube-api` / `cube-proxy` / `cube-webui` Deployment Ready
- `cube-mysql` / `cube-redis` StatefulSet Ready(若使用内置)
- `helm test` 全绿

登录 WebUI(默认在 `http://<control-node-hostip>:12088`)开始使用。

---

## 6. 常见部署模式

### 6.1 单节点试用(仅测试)

一台机器既做 control 又做 compute,给这台节点打上两套 label,并去掉 taint:

```bash
kubectl label nodes <node> \
  cube.tencent.com/role=control \
  cube.tencent.com/cube-control=true \
  cube.tencent.com/cube-node=true \
  cube.tencent.com/allow-pvm-bootstrap=true
# 不要打 taint
```

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

若集群没有 default SC 或想显式指定,在 `runtime-values.yaml` 里配:

```yaml
controlPlane:
  master:
    persistence:
      storageClassName: <your-sc-name>   # 例如 EKS gp3 / GKE premium-rwo / AKS managed-csi
mysql:
  persistence:
    storageClassName: <your-sc-name>
redis:
  persistence:
    storageClassName: <your-sc-name>
```

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
