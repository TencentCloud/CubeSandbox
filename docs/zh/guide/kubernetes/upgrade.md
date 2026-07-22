# 升级

目标只有一句话：**控制面可以有序滚动升级；计算面升级会 recreate `cube-node` Big Pod，并中断该节点上的存量沙箱。**

---

::: warning Preview 版本警告
计算面使用原生 `apps/v1` DaemonSet：镜像 / 资源 / template 变更会导致 Big Pod **删除重建**（PodIP / netns 变化），存量沙箱网络会中断。计算面升级会 **recreate `cube-node` Big Pod**（原生 DaemonSet），**会中断该节点上的存量沙箱**。升级前请先调用 CubeMaster 的 isolate API，将节点隔离 60 秒以上；并且销毁节点上的沙箱。在销毁沙箱后，才能安全的升级节点。

**为解决平滑升级问题，您可采用您熟悉的k8s插件去实现“原地升级”  ** —— 即不重建pod，仅升级容器镜像。部署当前版本后若计划升级，应当仔细评估更改、做测试后再实施。

**上述问题将在后续版本逐步得到解决。欢迎试用 K8s 部署方式，通过 Issue 反馈问题与建议。**
:::

## 为什么 Big Pod recreate 会中断沙箱？

CubeSandbox 的网络（cubevs）钩子挂在 Pod 的网卡上，沙箱的 tap 设备也与 Pod 处于同一 netns。Pod 重建会销毁 netns，导致沙箱网络中断。因此计算面升级属于**有中断**操作，需要维护窗口与节点隔离、销毁存量沙箱。

## 升什么，动哪条工作负载？

计算面拆成四条线，**日常升级只改对应组件的镜像 tag**，避免顺手改 Big Pod 的 env / volumeMount / 容器列表（这些同样会 recreate）。

| 你想升级的东西 | 动哪条工作负载 | values 里改谁 | 是否会 recreate Big Pod |
| --- | --- | --- | --- |
| cubelet / network-agent / wait-node-prep / 槽位镜像或 resources | **Big Pod**（`cube-node`） | `images.cubelet` 等 | **是**（会中断沙箱） |
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
    tag: v0.5.2
  # 需要一起升再写上，例如：
  # networkAgent:
  #   tag: v0.5.2
  # cubeShim:
  #   tag: v0.5.2
```

只改你真正要升的键；其它镜像保持不动即可。完整键名见文末[附录](#附录镜像键速查)。

2. 用与安装时相同的 `-f` 组合执行升级：

**⚠️警告：** 在生产环境中执行升级时，请逐个节点、组件进行灰度升级。全量操作是非常危险的！

::: warning Preview 版本警告
升 Big Pod 运行时镜像前，先 isolate 节点并清空沙箱；升级会 recreate Big Pod。
:::

```bash
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f runtime-values.yaml
# TKE / 单节点等场景继续叠加首次安装时用过的 values-tke.yaml / values-single-node.yaml
```

### 怎么确认升级成功？

```bash
# Big Pod 会被重建：UID / PodIP 会变化；关注新 Pod Ready 与节点重新注册
kubectl get pods -n cube-system -l app.kubernetes.io/component=cube-node -o wide
kubectl get daemonset -n cube-system cube-node

# 控制面 Deployment 可按常规滚动验收
kubectl get deploy -n cube-system
kubectl rollout status deploy/cube-master -n cube-system
```

期望：

- 控制面 Pod 按 Deployment 策略完成（`cube-master` 为 Recreate；其它控制面 Deployment 为 RollingUpdate）
- 计算面：对应 DaemonSet 的 Pod 已换成新镜像并 Ready
- 若升了 Big Pod 运行时：节点上存量沙箱已中断；新沙箱可创建；节点已重新注册到 CubeMaster

---

## 红线：这些操作也会 recreate Big Pod

下面任一操作都会让 Big Pod **recreate** → PodIP / netns 变 → 存量沙箱中断。只在明确安排的维护窗口做。

| 不要随便做 | 为什么 |
| --- | --- |
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
| `images.cubelet` | Big Pod | `cubelet` |
| `images.networkAgent` | Big Pod | `network-agent` |
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
