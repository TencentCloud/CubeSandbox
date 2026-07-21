# Helm 安装

按固定顺序，在已有集群上完成 CubeSandbox 的 Helm 安装。

::: warning 生产环境注意
- 将服务暴露到不可信网络前，请先完成[网络加固](../network-hardening.md)。
:::

## 你会怎么装

```text
① 集群就绪
    ↓
② 给节点打标签（以及角色污点）
    ↓
③ 准备 values 配置
    ↓
④ helm upgrade --install
    ↓
⑤ 验证
```

控制面为原生 Deployment，计算面（`cube-node` / bootstrap / installer / pvm）为原生 `apps/v1` DaemonSet；安装只需标准 Kubernetes / Helm。

---

## 1. 前置条件

| 项目 | 要求 |
| --- | --- |
| Kubernetes | **v1.24+**（自建 / k3s / TKE 等均可） |
| 工具 | `kubectl`、Helm v3.10+ |
| 存储 | 集群有可用 StorageClass（或改用 hostPath，见下文） |
| 镜像拉取 | 能拉取 Chart 默认镜像，或自备私有仓库 |

### 节点角色怎么分

CubeSandbox 用 **标签** 区分两类节点（不是 K8s 的 master/worker 概念）：

| 角色 | 建议规格 | 用途 | 需要打的标签 |
| --- | --- | --- | --- |
| **控制面节点** | ≥ 1 台；生产建议 3+；约 4C8G + 持久盘 | 跑 CubeMaster / API / Proxy / WebUI / MySQL / Redis 等 | `cube.tencent.com/cube-control=true` |
| **计算节点** | ≥ 1 台；建议 16C32G+ | 跑沙箱（cube-node / bootstrap / installer） | `cube.tencent.com/cube-node=true` |
| （可选）PVM 计算节点 | 需要换宿主机内核时 | 才会调度 `cube-node-pvm`、拉 PVM 镜像 | 在计算标签之外再加 `cube.tencent.com/allow-pvm-bootstrap=true` |

计算节点还需要：`/dev/kvm` 可用、数据盘建议 XFS。

::: tip 控制面标签怎么理解？
Chart **默认**给控制面 Pod 写了 `nodeSelector: cube.tencent.com/cube-control=true`：

- **推荐**：给准备跑控制面的节点打上该标签，否则这些 Pod 会 **Pending**（资源能创建，但调度器找不到匹配节点）。
- **不是**「不打就装不上」：Helm 仍会成功创建对象；只是默认 selector 对不上节点。
- 若你改用自己的 `placement.controlPlane.nodeSelector`，可以不使用 `cube-control` 这个 key。
- Chart **不允许**把 controlPlane 的 `nodeSelector` 清空（`validate.yaml` 会直接 fail）。
- Kubernetes 自己的 master 节点：若已有 `NoSchedule`、也不打算混部 Cube 控制面，不必打 Cube 标签。
:::

---

## 2. 确认集群就绪

```bash
kubectl get nodes -o wide
kubectl get storageclass
helm version
```

记下稍后要用的节点名。确认至少有一台可调度节点能跑控制面、一台（或同一台）能跑计算面。

---

## 3. 给节点打标签（及角色污点）

### 3.1 多节点（控制面 / 计算面分离，推荐）

```bash
# 控制面节点（跑 CubeMaster 等）
kubectl label nodes <control-node> cube.tencent.com/cube-control=true --overwrite

# 计算节点（跑沙箱）
kubectl label nodes <compute-node> cube.tencent.com/cube-node=true --overwrite

# 仅当该计算节点需要安装 PVM 宿主机内核时再加：
kubectl label nodes <pvm-compute-node> cube.tencent.com/allow-pvm-bootstrap=true --overwrite
```

角色污点（推荐，避免无关负载落到这些节点上）：

```bash
# 可选
# kubectl taint nodes <control-node> cube.tencent.com/control=true:NoSchedule --overwrite
kubectl taint nodes <compute-node> cube.tencent.com/compute=true:NoSchedule --overwrite
```

### 3.2 单节点试用（一台机器既做控制面又做计算面）

```bash
export NODE=<你的节点名>

kubectl label nodes "$NODE" \
  cube.tencent.com/cube-control=true \
  cube.tencent.com/cube-node=true \
  cube.tencent.com/allow-pvm-bootstrap=true \
  --overwrite

kubectl taint nodes "$NODE" cube.tencent.com/control=true:NoSchedule --overwrite
kubectl taint nodes "$NODE" cube.tencent.com/compute=true:NoSchedule --overwrite
```

单节点安装时请叠加 `values-single-node.yaml`（见下一步），它会给控制面 / 计算面注入污点容忍并集，否则混部会 Pending。

::: tip 纯 BM、不需要 PVM 换核
可去掉 `allow-pvm-bootstrap` 标签，并在 values 里设置 `bootstrap.pvmHostKernel.enabled: false`。
:::

### 3.3 启用 PVM 前的系统组件检查（需要换核时）

确认 CNI、kube-proxy 能容忍 `NoSchedule`（`operator: Exists`，或显式容忍 `cube.tencent.com/pvm-not-ready`），并在目标节点上仍为 Running。否则 PVM 门闩期间节点可能失联，无法清闩。

---

## 4. 准备配置文件

从仓库复制示例并改成本地文件（**不要**把含真实密码的文件提交进 Git）：

```bash
cp deploy/kubernetes/chart/runtime-values.example.yaml runtime-values.yaml
```

至少改这几项：

```yaml
cubeProxy:
  advertiseIP: "10.0.1.10"   # 仅文档/运维提示，不进 Chart 模板；对外入口看 Service/Ingress/DNS
  domain: "cube.app"
  tls:
    mode: selfSigned         # 试用；生产建议 existingSecret / certManager

# 必填：Chart 会拒绝 values.yaml 里的 CHANGE_ME_* 哨兵
mysql:
  host: ""                   # 空 = 使用内置 MySQL
  password: "replace-me-mysql-password"
  rootPassword: "replace-me-mysql-root-password"
redis:
  host: ""
  password: "replace-me-redis-password"
```

**控制面 PVC：**

- 默认：CubeMaster / MySQL / Redis 走集群 **default StorageClass**
- 想指定 SC：在 `runtime-values.yaml` 写 `persistence.storageClassName: <name>`
- 单节点 / 无 CSI：可改用 hostPath（见 `runtime-values.example.yaml` 注释）

**TKE：** 安装时再叠加 `deploy/kubernetes/chart/values-tke.yaml`（会创建 CBS StorageClass，并把 PVC 绑上去）。

### 计算节点数据盘（`/data/cubelet`）

沙箱 rootfs / 快照等落在宿主机 `/data/cubelet`，且必须是 **XFS（reflink）**。Chart 默认用 **loopback 镜像文件** 自动准备这块盘：

| values 路径 | 默认 | 说明 |
| --- | --- | --- |
| `bootstrap.nodeInit.dataCubelet.loopback.enabled` | `true` | 为 `true` 时，bootstrap 会在节点上创建并挂载 loopback XFS |
| `bootstrap.nodeInit.dataCubelet.loopback.imagePath` | `/data/cubelet-xfs.img` | 镜像文件路径 |
| `bootstrap.nodeInit.dataCubelet.loopback.size` | `25G` | **首次创建**镜像时的大小（`truncate -s`） |

试用或节点上没有现成 XFS 数据盘时，在 `runtime-values.yaml` 里改大小即可，例如：

```yaml
bootstrap:
  nodeInit:
    dataCubelet:
      loopback:
        enabled: true
        size: 200G   # 按容量规划调整；需小于存放 image 的文件系统剩余空间
```

::: warning 只影响「第一次」创建
`cube-node-init` 仅在 **`/data/cubelet-xfs.img` 尚不存在** 时按 `size` 建文件。镜像已存在后再改 values **不会**自动扩容。要换更大盘需维护窗口内手工处理（例如卸挂载、删旧 img 后重建——**会丢 `/data/cubelet` 数据**），或改用下方「预置 XFS 盘」。
:::

生产更推荐：在计算节点上提前挂好独立 XFS 盘到 `/data/cubelet`，并关掉 loopback：

```yaml
bootstrap:
  nodeInit:
    dataCubelet:
      loopback:
        enabled: false
```

---

## 5. Helm 安装

在仓库根目录执行：

```bash
# 标准多节点
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system \
  --create-namespace \
  -f runtime-values.yaml \
  --wait \
  --timeout 90m
```

腾讯云 TKE：

```bash
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system \
  --create-namespace \
  -f deploy/kubernetes/chart/values-tke.yaml \
  -f runtime-values.yaml \
  --wait \
  --timeout 90m
```


单节点试用：

```bash
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system \
  --create-namespace \
  -f deploy/kubernetes/chart/values-single-node.yaml \
  -f runtime-values.yaml \
  --wait \
  --timeout 90m
```



安装可能较久（首次 PVM 换核、节点重启、冷启动都算在 timeout 里）。`--wait` 会等到主要工作负载 Ready。

---

## 6. 验证部署

```bash
# 1) Pod 是否 Ready
kubectl get pods -n cube-system -o wide

# 2) 计算节点是否已注册到 CubeMaster
kubectl exec -n cube-system deploy/cube-cubemastercli -- \
  sh -lc 'cubemastercli --address "$CUBEMASTERCLI_ADDRESS" --port "$CUBEMASTERCLI_PORT" node list'

# 3) 内置端到端测试（约数分钟）
helm test cube -n cube-system --timeout 20m --logs
```

期望：

- `cube-node` Ready 数量 ≈ 打了 `cube-node=true` 的节点数
- `cube-master` / `cube-ops` / `cube-api` / `cube-proxy` / `cube-webui` Ready
- 内置 MySQL / Redis（若启用）Ready
- `helm test` 通过

打开 WebUI（默认 `http://<控制面节点 HostIP>:12088`）即可开始使用。

---

## 7. 卸载

```bash
helm uninstall cube -n cube-system
kubectl delete namespace cube-system
```

卸载**不会**自动清理：

- 节点上的 Cube 标签 / 角色污点
- 计算节点 hostPath 数据（如 `/data/cubelet`、`/data/cube-shared`、`/usr/local/services/cubetoolbox` 等）
- PVM 宿主机内核改动（需按平台 runbook 回滚）
- 外部 MySQL / Redis、DNS / LB 记录

---

## 常见问题速查

| 现象 | 怎么处理 |
| --- | --- |
| 控制面 Pod 一直 Pending | 默认需要节点有 `cube-control=true`（或你改过的 nodeSelector）；再查污点 / toleration |
| `cube-node` DESIRED=0 | 计算节点是否打了 `cube-node=true`？ |
| Helm 报 `CHANGE_ME_*` / 校验失败 | 检查 `runtime-values.yaml` 密码与 `placement` 是否完整 |
| 首次安装很久 / 节点重启 | 启用 PVM 时正常；等指纹就绪、门闩清除后再看 bootstrap / Big Pod |

更细的说明见本目录其它文档：

- [架构说明](./architecture.md)
- [升级](./upgrade.md)
- [常见问题](./faq.md)

---

## 下一步

- [Kubernetes 部署概览](./index.md)
- [架构说明](./architecture.md)
- [升级](./upgrade.md)
- [常见问题](./faq.md)
- [连接到已有 Cube 集群](../connect-existing-cluster.md)
- [WebUI 控制台](../webui.md)
- [网络加固](../network-hardening.md)
- [鉴权](../authentication.md)
