# Helm 安装

在现有 K8s 集群上完成 CubeSandbox 的 Helm 安装。


## 安装流程

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


## 1. 前置条件

| 项目 | 要求 |
| --- | --- |
| Kubernetes | **≥ v1.24+** |
| 工具 | `kubectl`、Helm v3.10+ |
| 存储 | 集群有可用 StorageClass |
| 镜像拉取 | 节点能联网拉取镜像 |

### 节点角色

CubeSandbox 用 **标签** 区分两类节点：

| 角色 | 建议规格 | 用途 |
| --- | --- | --- |
| **控制面节点** | ≥ 1 台；生产建议 3+；≥ 4C8G | 跑Cube的控制面组件 |
| **计算节点** | ≥ 1 台；建议 16C32G+ | 跑沙箱 |

::: tip 建议
使用独立的机器作为计算节点。
:::

::: details 什么是 PVM？（技术原理）
PVM（Pagetable-based Virtual Machine）是一种**基于页表的嵌套虚拟化框架**，构建于 KVM 之上。与传统嵌套虚拟化不同，PVM 不依赖宿主 hypervisor 向 guest 暴露 Intel VT-x / AMD-V 等硬件虚拟化扩展，而是在 guest 内核层通过共享内存区域和影子页表（shadow page table）来完成特权级切换与内存虚拟化，对宿主 hypervisor 完全透明。

PVM 最初由论文 [《PVM: Efficient Shadow Paging for Deploying Secure Containers in Cloud-native Environment》](https://dl.acm.org/doi/10.1145/3600006.3613158) 提出。腾讯云在此基础上进行了大量功能与性能改进、bugfix，并将相关工作开源至 [OpenCloudOS 内核](https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git)，供社区使用。数年来，我们已在腾讯云生产环境部署了大量 PVM 实例，其可靠性已经过生产验证。
:::


## 2. 确认集群就绪

```bash
kubectl get nodes -o wide
kubectl get storageclass # 非空
helm version --short # ≥ v3.10
```

记下稍后要用的节点名。确认至少有一台节点用于跑控制面、另一台跑计算面。


## 3. 给节点打标签、角色

### 3.1 多节点（推荐，控制面 / 计算面分离）

正常情况下，请为 K8s 添加至少 2 台机器，采用控制面、计算面分离的方式进行部署：

请执行命令为相关节点打标签：

```bash
# 控制面节点
kubectl label nodes <control-node> cube.tencent.com/cube-control=true --overwrite

# 计算节点（跑沙箱）
kubectl label nodes <compute-node> cube.tencent.com/cube-node=true --overwrite

# 若计算节点不是裸金属服务器，则还需要打这个 label ，即可自动安装 PVM
kubectl label nodes <pvm-compute-node> cube.tencent.com/allow-pvm-bootstrap=true --overwrite
```

打角色污点（避免无关负载落到这些节点上）：

```bash
# 可选
# kubectl taint nodes <control-node> cube.tencent.com/control=true:NoSchedule --overwrite

# 计算节点必打
kubectl taint nodes <compute-node> cube.tencent.com/compute=true:NoSchedule --overwrite
```

### 3.2 单节点（不推荐）

::: tip 提示

对于单节点的情况，我们更推荐采用 [快速开始](../quickstart.md) 的方式进行部署，而不是使用 K8s。
:::

::: details 单节点标签与污点
若您的 K8s 集群内仅有 1 台机器，可尝试通过以下方式打标签、污点（不推荐）:

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
:::


## 4. 准备配置文件

执行创建配置文件：

```bash
cp deploy/kubernetes/chart/runtime-values.example.yaml runtime-values.yaml
```

根据您的需求对其进行修改，至少填写以下内容：

```yaml
cubeProxy:
  advertiseIP: "10.0.1.10"
  domain: "cube.app"
  tls:
    mode: selfSigned         # 试用；生产建议 existingSecret / certManager

# 必填
mysql:
  host: ""                   # 空 = 使用内置 MySQL
  password: "replace-me-mysql-password"
  rootPassword: "replace-me-mysql-root-password"
redis:
  host: ""
  password: "replace-me-redis-password"
```



### 更多配置

**控制面 PVC 配置**

请转到 [8.1 控制面 PVC 配置](#_8-1-控制面-pvc-配置)

**计算节点数据盘**

Cube 会在宿主机 `/data/cubelet`下写入数据，且该路径必须是 **XFS**文件系统。 为便于体验，我们默认会帮您创建 一块 25GB 的**loopback 镜像文件** 并挂载。 

若您希望在生产环境部署，或调整相关配置，请转到 [8.2 计算节点数据盘配置](#_8-2-计算节点数据盘配置)。

**计算节点网络（cube-node 重建）**

::: warning 请在部署前决定
沙箱一旦开始运行，计算节点上的 `cube-node` Pod **不可重建**：重建会销毁沙箱网络设备所在的 network namespace，该节点上 **所有沙箱的网络都会中断（入站、出站均中断）**。生产环境建议在部署时就让 `cube-node` 启用 `hostNetwork`，使 Pod 重建不再引起 netns 变化。详细说明与注意事项（含 NetworkPolicy）：[8.3 cube-node 网络与 Pod 重建](#_8-3-cube-node-网络与-pod-重建)。
:::


## 5. Helm 安装

::: tip 中国大陆用户
在以下任一安装命令中额外加上 `-f deploy/kubernetes/chart/values-cn.yaml`，即可从国内镜像源拉取镜像。
:::


在仓库根目录执行以下命令即可完成安装：

由于该过程会下载较多资源、执行初始化动作，根据您的机器性能、网络状况，需要一段时间才能完成部署。

标准多节点部署：

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


## 6. 验证部署

```bash
# 1) Pod 是否 Ready
kubectl get pods -n cube-system -o wide

# 2) 计算节点是否已注册到 CubeOps
kubectl exec -n cube-system deploy/cube-cubemastercli -- \
  sh -lc 'cubeopscli --address "$CUBEOPSCLI_ADDRESS" --port "$CUBEOPSCLI_PORT" node list'

# 3) 内置端到端测试（约数分钟）
helm test cube -n cube-system --timeout 20m --logs
```

期望：

- `cube-node` Ready 数量 = 打了 `cube-node=true` 的节点数
- 命名空间 `cube-system` 下的各个服务均为 Ready
- `helm test` 通过

**访问Cube 的 WebUI 界面：**

```bash
kubectl -n cube-system port-forward svc/cube-webui 12088:12088
# 浏览器打开 http://127.0.0.1:12088
```



## 7. 卸载

```bash
helm uninstall cube -n cube-system
kubectl delete namespace cube-system
```

卸载**不会**自动清理：

- 节点上的 Cube 标签 / 角色污点
- 计算节点 hostPath 数据
- PVM 宿主机内核改动


## 8. 高级配置

### 8.1 控制面 PVC 配置

- 默认：CubeMaster / MySQL / Redis / MinIO 走集群 **default StorageClass**
- 指定 SC：在 `runtime-values.yaml` 修改 `persistence.storageClassName: <name>`
- 单节点 / 无 CSI：可改用 hostPath（见 `runtime-values.example.yaml` 注释）


### 8.2 计算节点数据盘配置

对于 **生产部署** ，我们建议您通过运维手段，为每台计算节点配置单独的数据盘，格式化为 XFS 文件系统，并挂载在 `/data/cubelet` 下，这样能获得最佳的性能、稳定性。
然后，在 `runtime-values.yaml` 内配置：

```yaml
bootstrap:
  nodeInit:
    dataCubelet:
      loopback:
        enabled: false
```

若您的节点上没有独立数据盘，我们默认会在初次初始化计算节点时，在`/data/cubelet`下为您挂载 1 块 25G 的loop 设备。

::: warning 只影响「第一次」创建
默认仅在 **`/data/cubelet-xfs.img` 不存在** 时自动创建磁盘镜像文件。 后续更改配置**不会**自动扩容。要换更大盘需在维护窗口内手工处理。
::: 

若您希望自定义数据盘配置，可按照以下方法在安装前调整配置：

| values 路径 | 默认 | 说明 |
| --- | --- | --- |
| `bootstrap.nodeInit.dataCubelet.loopback.enabled` | `true` | 为 `true` 时，bootstrap 会在节点上创建并挂载 loopback XFS |
| `bootstrap.nodeInit.dataCubelet.loopback.imagePath` | `/data/cubelet-xfs.img` | 镜像文件路径 |
| `bootstrap.nodeInit.dataCubelet.loopback.size` | `25G` | **首次创建**镜像时的大小（`truncate -s`） |

```yaml
bootstrap:
  nodeInit:
    dataCubelet:
      loopback:
        enabled: true
        size: 200G   # 按容量规划调整；需小于存放 image 的文件系统剩余空间
```


### 8.3 cube-node 网络与 Pod 重建

::: warning 一句话结论
沙箱运行期间，`cube-node` Pod **不可重建**——重建会导致该节点上所有沙箱的网络**全部中断（入站、出站均中断）且无法自愈**。生产环境建议部署时启用 `hostNetwork` 规避；代价是 NetworkPolicy 需另行处理（见下文）。
:::

#### 为什么 cube-node 不可重建

沙箱的网络设备（TAP 设备）与 cubevs 钩子位于 `cube-node` Pod 的 network namespace 中，Pod 重建即销毁该 netns：

| 项目 | 说明 |
| --- | --- |
| 触发条件 | 任何导致 Pod 重建的操作：DaemonSet template 变更、镜像升级、手工 `kubectl delete pod` 等 |
| 影响 | 该节点上**所有沙箱**的网络全部中断，**入站、出站均中断** |
| 能否自愈 | **不能**。只能销毁并重建受影响的沙箱 |

计算面升级同样受此影响，详见[升级](./upgrade.md)。

#### 建议：部署时启用 hostNetwork

::: tip 推荐做法
在**创建沙箱之前**让 `cube-node` 以 `hostNetwork: true` 运行：Pod 与宿主机共享 netns，netns 不随 Pod 重建而变化，沙箱网络设备因此得以保留。
:::

启用时的注意事项：

| 事项 | 说明 |
| --- | --- |
| 如何启用 | 当前 Chart 无 values 开关（`security.hostNetwork` 会被校验拒绝），需通过 Helm post-renderer / Kustomize / fork Chart 修改 DaemonSet |
| DNS | 需同时设置 `dnsPolicy: ClusterFirstWithHostNet`，保证集群内域名解析可用 |
| 端口冲突 | 确认 cubelet 端口（9998 / 9999 / 9966）不与宿主机其他服务冲突 |
| 监控 / 防火墙 | 基于 Pod IP / Pod CIDR 的监控、防火墙、策略需改为基于节点 IP |

#### 代价：NetworkPolicy 不再生效

启用 hostNetwork 后，`cube-node` 失去 CNI 分配的 Pod 网络身份，Kubernetes NetworkPolicy（例如「沙箱能否访问某个 Service」）**无法直接管控沙箱流量**。

::: info 参考实现：PR #1189（尚未合入，仅供参考）
若需要用 NetworkPolicy 管控沙箱访问集群内 Service / Pod 的流量，可参考 [PR #1189](https://github.com/TencentCloud/CubeSandbox/pull/1189) 的做法：

- 仅将发往集群 CIDR 的流量经节点本地 **EgressProxy Pod** 转发，并 SNAT 为 Proxy Pod IP，使其受您自定义的 NetworkPolicy 管控；其余流量仍走正常路由。
- 该 PR 同时将 hostNetwork 设为默认，并实现了完整的 `cube-node` 原地替换设计。
:::


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
- [鉴权](../authentication.md)
