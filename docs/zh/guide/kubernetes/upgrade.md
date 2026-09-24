# 升级

目标只有一句话：**控制面可以有序滚动升级；计算面升级会 recreate `cube-node` Big Pod——默认的宿主机网络下这对沙箱网络是安全的，Pod 网络下则会中断沙箱。**

---

::: warning Preview 版本警告
计算面使用原生 `apps/v1` DaemonSet：镜像 / 资源 / template 变更会导致 Big Pod **删除重建**，cubelet 进程也会重启。这对存量沙箱的代价取决于 Pod 的网络模式：

- **宿主机网络（默认）**：tap 设备与 cubevs 钩子位于宿主机 netns，重建不会销毁，得以保留。在 cubelet 重启后沙箱存活的实机验证落地前，drain（isolate ≥ 60s、销毁沙箱）仍是支持路径。隔离操作详见[节点相关操作](../node-operations.md)。
- **Pod 网络**（`cubeNode.hostNetwork: false`）：重建销毁 netns，该节点上所有沙箱网络全部中断且不能自愈。**drain 必须做。**

若您暂时必须留在 Pod 网络，另一条路是用您熟悉的 K8s 插件实现“原地升级”——不重建 Pod，仅升级容器镜像。

部署当前版本后若计划升级，应当仔细评估更改、做测试后再实施。

**上述问题将在后续版本逐步得到解决。欢迎试用 K8s 部署方式，通过 Issue 反馈问题与建议。**
:::

## 为什么网络模式决定了重建的代价

CubeSandbox 的网络（cubevs）钩子挂在 Pod 的网卡上，沙箱的 tap 设备也与 Pod 处于同一 netns。Pod 网络下重建 Pod 会销毁该 netns，沙箱网络随之中断；宿主机网络下 netns 就是宿主机的，不随重建变化，设备因此保留。

取舍见[安装 · cube-node 网络与 Pod 重建](./install.md#_8-3-cube-node-网络与-pod-重建)；完整的原地替换设计可参考 [PR #1189](https://github.com/TencentCloud/CubeSandbox/pull/1189)（尚未合入）。

## 切换网络模式

`cubeNode.hostNetwork` 属于 Pod template 字段，改动会重建所有 Big Pod，且沙箱数据面会跨 netns 迁移。因此有一个 `pre-install` / `pre-upgrade` / `pre-rollback` Hook（`cube-node-hostnet-preflight`）拦截：当线上 DaemonSet 的模式与渲染出的模式不一致时，变更会被拒绝，失败信息给出两条出路：

1. **保持现状**——在 values 里把 `cubeNode.hostNetwork` 写成线上当前的值。
2. **采用新模式**——逐节点 isolate、等待 ≥ 60s、销毁沙箱（见[节点相关操作](../node-operations.md)），然后设置：

```yaml
cubeNode:
  hostNetworkChangeAck: true   # 升级完成后可移除
```

在宿主机网络成为默认值之前安装的 release，values 里通常没有 `cubeNode.hostNetwork`：日常只 bump 镜像 tag 的升级也会渲染出新默认值并被本闸门拦下。想在这类升级中保持旧行为，请在 values 里显式写 `cubeNode.hostNetwork: false`。

确认键只被 Hook 读取——它不验证 drain，这个开关是运维的自行确认；它也不属于 Pod template，设置或移除都不会重建 Pod。

关于切换还有两点：
- 切换后 node-init 会在已 ready 的节点上重跑（prepGeneration 已 bump），宿主机端口被占用时会卡在 node-init、cubelet 起不来。
- `helm rollback` / `--atomic` 仅在目标 revision 带本 Hook 时受闸门；更旧目标无闸门——请先 drain。回滚重放目标 revision 的 stored values，要把模式切回去请用带 `hostNetworkChangeAck: true` 的 `helm upgrade`，不要用 `helm rollback`。

## 升什么，动哪条工作负载？

计算面拆成四条线，**日常升级只改对应组件的镜像 tag**，避免顺手改 Big Pod 的 env / volumeMount / 容器列表（这些同样会 recreate）。

| 你想升级的东西 | 动哪条工作负载 | values 里改谁 | 是否会 recreate Big Pod |
| --- | --- | --- | --- |
| cubelet / wait-node-prep / 槽位镜像或 resources | **Big Pod**（`cube-node`） | `images.cubelet` 等 | **是**（见上方警告） |
| shim / kernel / guest 产物 | **Installer** | `images.cubeShim` 等 | 否（Big Pod template 不变） |
| node-init / 节点预检逻辑 | **Bootstrap** | `images.nodeInit` | 否（Big Pod template 不变） |
| PVM 宿主机换核脚本 | **cube-node-pvm** | `images.pvmHostBootstrap` | 否（但节点可能 reboot） |

```text
升运行时组件  →  只改 Big Pod 相关 images.*.tag（会 recreate Big Pod）
升 toolbox 产物 →  只改 Installer 相关 images.*.tag
升节点预检     →  只改 images.nodeInit.tag
升 PVM 换核     →  只改 images.pvmHostBootstrap.tag
```

---

## 日常升级（推荐路径）

1. 在本地 `runtime-values.yaml`（与首次安装相同的 values 文件）里更新要升的镜像 **tag**，例如：

```yaml
images:
  cubelet:
    tag: v0.7.2
  # 需要一起升再写上，例如：
  # cubeShim:
  #   tag: v0.7.2
```

只改你真正要升的键；其它镜像保持不动即可。完整键名见文末[附录](#附录-镜像键速查)。

2. 用与安装时相同的 `-f` 组合执行升级：

**⚠️警告：** 在生产环境中执行升级时，请逐个节点、组件进行灰度升级。全量操作是非常危险的！

::: warning Preview 版本警告
升 Big Pod 运行时镜像会 recreate Big Pod。请先 drain（见上）；Pod 网络下必须做。
:::

```bash
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f runtime-values.yaml
# TKE / 单节点等场景继续叠加首次安装时用过的 values-tke.yaml / values-single-node.yaml
```

### 怎么确认升级成功？

```bash
# Big Pod 会被重建：UID 会变（宿主机网络下 PodIP 即节点 IP，不会变）；
# 关注新 Pod Ready 与节点重新注册
kubectl get pods -n cube-system -l app.kubernetes.io/component=cube-node -o wide
kubectl get daemonset -n cube-system cube-node

# 控制面 Deployment 可按常规滚动验收
kubectl get deploy -n cube-system
kubectl rollout status deploy/cube-master -n cube-system
```

期望：

- 控制面 Pod 按 Deployment 策略完成（`cube-master` 为 Recreate；其它控制面 Deployment 为 RollingUpdate）
- 计算面：对应 DaemonSet 的 Pod 已换成新镜像并 Ready
- 若升的是**Pod 网络**节点上的 Big Pod 运行时：该节点存量沙箱已中断；新沙箱可创建；节点已重新注册到 CubeOps

---

## 升级顺序

在 cube-master → cube-ops 迁移过程中，各组件的目标端点不同
按以下顺序升级，避免切流期集群出现不一致：

1. **先起 CubeOps（保证可达）。** 新 cubelet 把 `meta_server_endpoint` 指向
   cube-ops，如果地址不可达会 fail-fast，所以必须先把 CubeOps 起好。
2. **CubeOps 起来后再起 CubeMaster。** CubeMaster 启动时如果配了
   `cube_ops_addr` 且 CubeOps 不通，会 fail-fast。
3. **切流期不要新老 cubelet 同集群混跑。** 旧 cubelet 向 cube-master `:8089`
   上报，新 cubelet 向 cube-ops `:3010` 上报；混跑会导致节点状态不一致。
   逐节点滚动升级，或先把整个计算面停了再升。
4. **以上就绪后**，再按需滚动 `cube-master`、`cube-api`、计算节点。


## 红线：这些操作也会 recreate Big Pod

下面任一操作都会让 Big Pod **recreate** → Pod UID 变化；Pod 网络下 netns 被销毁、存量沙箱中断。只在明确安排的维护窗口做。

| 不要随便做 | 为什么 |
| --- | --- |
| 改 `cubeNode.hostNetwork` | 会让沙箱数据面跨 netns 迁移；由 pre-install/pre-upgrade/pre-rollback Hook 拦截 |
| 增删 Big Pod 容器（含改槽位数量） | 改 Pod template，DaemonSet 会重建 Pod |
| 改 volumeMount / securityContext / 容器名 / 直接改 env | 同上 |
| 改 `wait-node-prep` 的 env / mount（只 bump 镜像也会 recreate） | wait 为 initContainer；template 变更即重建 |
| 手动删 Big Pod | 等于重建数据面 |
| 把产物安装塞进 Big Pod | 破坏分工；产物应走 Installer |

另外：`cubeNode.env`、`cubeNode.podAnnotations`、网络相关 env、`global.timezone`、`cubeEgress.enabled` 也会改 Pod template——**不是日常无感升级项**。

---

## 特殊场景

### A. 改 PVM kernel pattern / boot args（会 reboot）

日常只换 `images.pvmHostBootstrap` 镜像、且指纹仍匹配时，一般**不会**再打 `pvm-not-ready` 门闩。

若你要**主动改** `bootArgs` / kernel pattern（期望指纹变化），建议在 `helm upgrade` **之前**打运维门闩（`value=maintenance`，与 Hook 自动打的 `true` 不同——旧 hold 默认不会清 maintenance）：

```bash
# 1. 确认 CNI、kube-proxy 能容忍该 NoSchedule 污点
kubectl taint node <pvm-node> \
  cube.tencent.com/pvm-not-ready=maintenance:NoSchedule --overwrite

# 2. 在 runtime-values.yaml 里改好 bootArgs / kernel 相关配置后升级
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f runtime-values.yaml
```

例如 values 中：

```yaml
bootstrap:
  pvmHostKernel:
    bootArgs: "nopti pti=off <new-arg>"
```
节点恢复后，只有新的 PVM init 在 live 指纹匹配时才会清掉 maintenance。任一步失败都不应 reboot。细节见[架构说明 · PVM](./architecture.md#pvmcube-node-pvm)。

关某节点的 PVM：去掉该节点的 `allow-pvm-bootstrap` label即可。**不要**指望只改 `cubeNode.pvmGuestKernel.enabled=false` 把已在跑 PVM 的节点悄悄切回 bm。

### B. 卸干净重装（最后手段）

```bash
helm uninstall cube -n cube-system
sudo ./deploy/kubernetes/chart/scripts/cleanup-node-host.sh
helm upgrade --install cube ./deploy/kubernetes/chart \
  -n cube-system -f runtime-values.yaml
```

这会清掉 Chart 管理的对象；宿主机 hostPath / 内核改动需脚本与平台 runbook 另行处理。

---

## 附录：镜像键速查

需要查「这个 image 键对应哪个容器」时用：

| values 键 | 工作负载 | 容器 |
| --- | --- | --- |
| `images.cubelet` | Big Pod | `cubelet`（含内嵌网络运行时） |
| `images.waitNodePrep` | Big Pod / Bootstrap | Big Pod 的 `wait-node-prep` init；Bootstrap 的 write-ready 也用它 |
| `images.cubeShim` | Installer | `cube-shim-install` |
| `images.cubeKernel` | Installer | `cube-kernel-install` |
| `images.cubeGuest` | Installer | `cube-guest-install` |
| `images.nodeInit` | Bootstrap | `wait-pvm-host` / `cube-node-init` |
| `images.pvmHostBootstrap` | cube-node-pvm | `pvm-host-bootstrap` / hold reconcile |

改完 `bootArgs` / `prepGeneration` 等策略后，若担心误伤 Big Pod template，可跑：

```bash
sh deploy/kubernetes/chart/scripts/test-big-pod-inplace-guard.sh
```

该守卫要求这些策略变化对 Big Pod Pod template **零 diff**（避免无关配置变更触发 recreate）。

---

## 下一步

- [架构说明](./architecture.md)
- [Helm 安装](./install.md)
- [常见问题](./faq.md)
