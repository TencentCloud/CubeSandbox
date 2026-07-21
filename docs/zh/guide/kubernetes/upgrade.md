# 升级

目标只有一句话：**升级时，控制面节点能有序灰度滚动升级，同时计算节点的 `cube-node` Big Pod不重建**

---

::: warning Preview 版本警告
当前，计算节点的原地不停机升级流程仍在不断完善中，您应当谨慎评估、测试后操作，避免 `cube-node` Big Pod 重建导致存量沙箱丢失。
建议在节点升级前，先调用 CubeMaster 的 isolate API，将节点隔离 60 秒以上，然后再执行升级。
:::

## 为什么需要确保计算节点的 Big Pod 不重建？

由于CubeSandbox的网络（cubevs）的钩子挂在pod的网卡上，并且沙箱的tap设备也是跟pod的net namespace在同一netns内，而Pod重建会导致netns销毁，使得沙箱网络中断，因此为了确保升级不影响存量沙箱，我们需要确保升级操作不会导致cube-node 的pod重建。


## 升什么，动哪条工作负载？

计算面拆成四条线，**日常升级只改对应组件的镜像 tag**，不要顺手改 Big Pod 的 env / volumeMount / 容器列表。

| 你想升级的东西 | 动哪条工作负载 | values 里改谁 | 是否会 recreate Big Pod |
| --- | --- | --- | --- |
| cubelet / network-agent / wait-node-prep / 槽位镜像或 resources | **Big Pod**（`cube-node`） | `images.cubelet` 等 | **否**（InPlace） |
| shim / kernel / guest 产物 | **Installer** | `images.cubeShim` 等 | 否 |
| node-init / 节点预检逻辑 | **Bootstrap** | `images.nodeInit` | 否（Big Pod 应不变） |
| PVM 宿主机换核脚本 | **cube-node-pvm** | `images.pvmHostBootstrap` | 否（但节点可能 reboot） |

```text
升运行时组件  →  只改 Big Pod 相关 images.*.tag
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
当前，计算节点的原地不停机升级流程仍在不断完善中，您应当谨慎评估、测试后操作，避免 `cube-node` Big Pod 重建导致存量沙箱丢失。
建议在节点升级前，先调用 CubeMaster 的 isolate API，将节点隔离 60 秒以上，然后再执行升级。升级后再手动解除隔离。
:::

```bash
helm upgrade cube ./deploy/kubernetes/chart -n cube-system \
  -f runtime-values.yaml
# TKE / 单节点等场景继续叠加首次安装时用过的 values-tke.yaml / values-single-node.yaml
```

### 怎么确认升级是 InPlace 成功的？

```bash
# Big Pod 的 UID / PodIP 应与升级前一致
kubectl get pods -n cube-system -l app.kubernetes.io/component=cube-node -o wide

# 事件里应能看到原地更新成功（名称因版本略有差异）
kubectl get events -n cube-system --field-selector reason=SuccessfulUpdatePodInPlace
```

期望：

- Big Pod **没有被删掉重建**（UID / IP 不变）
- 存量沙箱仍可访问
- 对应组件版本已变成新 tag

---

## 红线：这些操作会毁掉「原地升级」

下面任一操作都可能让 Big Pod **recreate** → PodIP / netns 变 → 存量沙箱中断。只在明确安排的维护窗口做。

| 不要随便做 | 为什么 |
| --- | --- |
| 增删 Big Pod 容器（含改槽位数量） | 容器集合是冻结面 |
| 改 volumeMount / securityContext / 容器名 / 直接改 env | 同上，会 recreate |
| 改 `wait-node-prep` 的 env / mount（只 bump 镜像可以） | wait 的 env/mount 冻结 |
| 把 `rollingUpdateType` 改成 Standard，或手动删 Big Pod | 等于重建数据面 |
| 把产物安装塞进 Big Pod | 破坏分工；产物应走 Installer |

另外：`cubeNode.env`、`cubeNode.podAnnotations`、网络相关 env、`global.timezone`、`cubeEgress.enabled` 也会改冻结 template——**不是日常升级项**。

InPlace 白名单大致只有：

- 容器 **image**
- 槽位 **resources**（需集群 `InPlacePodVerticalScaling`，见[安装前置条件](./install.md#1-前置条件)）
- Chart 管理的 `cube.tencent.com/slot-*` **annotation**

具体可以参考: [OpenKruise的 “原地升级” 文档](https://openkruise.io/zh/docs/core-concepts/inplace-update)

---

## 特殊场景

### A. 改 PVM kernel pattern / boot args（会 reboot）

日常只换 `images.pvmHostBootstrap` 镜像、且指纹仍匹配时，一般**不会**再打 `pvm-not-ready` 门闩。

若你要**主动改** `bootArgs` / kernel pattern（期望指纹变化），建议在 `helm upgrade` **之前**打运维门闩（`value=maintenance`，与 Hook 自动打的 `true` 不同——旧 hold 默认不会清 maintenance）：

```bash
# 1. 确认 CNI、kube-proxy、kruise-daemon 能容忍该 NoSchedule 污点
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
| `images.waitNodePrep` | Big Pod / Bootstrap | `wait-node-prep`；Bootstrap 的 write-ready 也用它 |
| `images.cubeShim` | Installer | `cube-shim-install` |
| `images.cubeKernel` | Installer | `cube-kernel-install` |
| `images.cubeGuest` | Installer | `cube-guest-install` |
| `images.nodeInit` | Bootstrap | `wait-pvm-host` / `cube-node-init` |
| `images.pvmHostBootstrap` | cube-node-pvm | `pvm-host-bootstrap` / hold reconcile |

改完 `bootArgs` / `prepGeneration` 等策略后，若担心误伤 Big Pod template，可跑：

```bash
sh deploy/kubernetes/chart/scripts/test-big-pod-inplace-guard.sh
```

该守卫要求这些策略变化对 Big Pod Pod template **零 diff**。

---

## 下一步

- [架构说明](./architecture.md)
- [Helm 安装](./install.md)
- [常见问题](./faq.md)
