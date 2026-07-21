# Kubernetes 部署

在已有 Kubernetes 集群上，用 Helm Chart 安装 CubeSandbox（控制面 + 计算面）。

::: tip 和「一键脚本」部署的区别
这是 **K8s 原生路径**：组件跑在集群里，由 Helm 管理。若只有单台物理机、不打算用 K8s，请改看[裸金属 / 物理机部署](../bare-metal-deploy.md)或[快速开始](../quickstart.md)。
:::

::: warning Preview 版本警告
当前版本的 K8s 部署是**预览版本**，已知问题：

1. 计算节点资源紧张时，Pod 可能被 K8s 控制面错误驱逐，导致沙箱中断。该问题正在解决中。
2. 计算节点的原地不停机升级流程仍在不断完善中，您应当谨慎评估、测试后操作，避免 `cube-node` Big Pod 重建导致存量沙箱丢失。建议在节点升级前，先调用 CubeMaster 的 isolate API，将节点隔离 60 秒以上，然后再执行升级。
3. 由于计算节点的 `cube-node` 配置在后续可能会进行调整，导致后续版本升级时出现 Pod 重建。因此，当在 K8s 上部署当前版本后，若您想升级到后续版本，应当仔细评估更改、做测试后再实施；最好先将待升级计算节点上的沙箱都销毁后再执行升级，以免因 `cube-node` 重建导致业务中断。

**上述问题将在后续版本得到解决。欢迎试用 K8s 部署方式，通过 Issue 反馈问题与建议。**
:::

## 文档导航

| 文档 | 内容 |
| --- | --- |
| [Helm 安装](./install.md) | 从集群就绪到验证的完整步骤（推荐主路径） |
| [架构说明](./architecture.md) | Chart 组件分层、四个 DaemonSet、启动顺序与数据流 |
| [升级](./upgrade.md) | 计算面镜像原地升级：升什么动哪条、红线与特殊场景 |
| [常见问题](./faq.md) | 安装、调度、PVM、Proxy、Egress、升级排障 |

## 安装顺序（必读）

```text
① 集群就绪
    ↓
② 安装 OpenKruise，并确认 Ready
    ↓
③ 给节点打标签（以及角色污点）
    ↓
④ 准备 values 配置
    ↓
⑤ helm upgrade --install
    ↓
⑥ 验证
```

必须先装 OpenKruise：控制面 CloneSet 与计算面 Advanced DaemonSet 依赖它。若先打角色污点再装 OpenKruise，`kruise-controller-manager` 很容易一直 Pending。

下一步 → [Helm 安装](./install.md)
