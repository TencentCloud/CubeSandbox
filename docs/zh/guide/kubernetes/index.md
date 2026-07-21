# Kubernetes 部署

在已有 Kubernetes 集群上，用 Helm Chart 安装 CubeSandbox（控制面 + 计算面）。

::: tip 和「一键脚本」部署的区别
这是 **K8s 原生路径**：组件跑在集群里，由 Helm 管理。若只有单台物理机、不打算用 K8s，请改看[裸金属 / 物理机部署](../bare-metal-deploy.md)或[快速开始](../quickstart.md)。
:::

::: warning Preview 版本警告
当前版本的 K8s 部署是**预览版本**，已知问题：

1. 计算节点资源紧张时，Pod 可能被 K8s 控制面错误驱逐，导致沙箱中断。该问题正在解决中。
2. 计算面升级会 **recreate `cube-node` Big Pod**（原生 DaemonSet），**会中断该节点上的存量沙箱**。升级前请先调用 CubeMaster 的 isolate API，将节点隔离 60 秒以上；并且销毁节点上的沙箱。
3. 由于计算节点的沙箱网络绑定pod网卡，因此，一旦 `cube-node` 重建，存量沙箱网络将会中断。为解决这个问题，您可采用您熟悉的k8s插件去实现“原地升级” —— 即不重建pod，仅升级容器镜像。部署当前版本后若计划升级，应当仔细评估更改、做测试后再实施。

**上述问题将在后续版本逐步得到解决。欢迎试用 K8s 部署方式，通过 Issue 反馈问题与建议。**
:::

## 文档导航

| 文档 | 内容 |
| --- | --- |
| [Helm 安装](./install.md) | 从集群就绪到验证的完整步骤（推荐主路径） |
| [架构说明](./architecture.md) | Chart 组件分层、四个 DaemonSet、启动顺序与数据流 |
| [升级](./upgrade.md) | 控制面可滚动；计算面升级会 recreate Big Pod 并中断沙箱 |
| [常见问题](./faq.md) | 安装、调度、PVM、Proxy、Egress、升级排障 |

## 安装顺序（必读）

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

控制面为原生 Deployment，计算面为原生 `apps/v1` DaemonSet；安装只需标准 Kubernetes / Helm。

下一步 → [Helm 安装](./install.md)
