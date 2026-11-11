---
title: "用 CubeSandbox 打造轻量隔离工作区：多 Agent 并行编辑的分层实践"
date: 2026-11-06
author: 佐藤 良（mizuamedesu，日本筑波大学 · KakuriZai 项目负责人）
description: "多个 Agent 在同一个大项目上并行工作，文件变更会相互冲突；为每个 Agent 复制一份完整环境，对 UE 这种连源码带二进制的巨型项目也不现实。筑波大学 IPA-CyberLab 的 KakuriZai 项目给出了一套完整解法：宿主源码作为只读 lower 层被所有工作环境共享，每个 Agent 只持有自己的可写 upper 层，执行环境交给 CubeSandbox。"
featured: true
weight: 1
---

# 用 CubeSandbox 打造轻量隔离工作区：多 Agent 并行编辑的分层实践

作者｜日本筑波大学 · KakuriZai 项目负责人 · 佐藤 良（mizuamedesu）

**编者按｜** 多个 Agent 在同一个大项目上并行工作，是 AI 辅助开发越来越常见的形态，但文件变更会相互冲突；为每个 Agent 复制一份完整环境，类似 UE 这种连源码带二进制的巨型项目，也无法承担整份复制的成本。这篇实践的作者来自筑波大学，他在 IPA-CyberLab 的 KakuriZai 项目中给出了一套完整的解法：宿主源码作为只读层（lower）被所有工作环境共享，每个 Agent 只持有自己的可写层（upper），执行环境则交给 CubeSandbox。

KakuriZai 的定位是沙箱生命周期控制平面（附带 Studio Web 界面），CubeSandbox 是它引入的沙箱运行时之一。这篇文章记录了从模板资源匹配、guest 内 overlay 挂载、whiteout 处理，到带状态分叉复制的完整实现路径。其中"VM 快照不包含 host-mount 数据"这条边界认知，对所有同时使用挂载与快照的用户都有直接的参考价值。

## 一、问题与方案：以分层方式共享大型项目

让 AI 编写代码带来了效率提升，但在同一个项目里同时运行多个 Agent，文件变更会相互冲突。为每个 Agent 准备独立的工作环境可以解决冲突，但只要把 Unreal Engine 的源码和二进制计算在内，整份复制并不现实。

于是我们在构建工作流时就确定：所有 Agent 共同引用同一个大目录，而每个 Agent 只持有自己的变更；同时，命令也要在隔离环境中执行。KakuriZai 正是围绕这一用例开发的。它在既有的 IsolatedAgent / agentctl 环境管理基础上，新增了使用 CubeSandbox 的 cube-sandbox-overlay 后端，工作环境以 World 为单位管理，通过 CLI 工具 agctl 与 Web 端 Studio 操作。

我们的目标是让多个 Agent 并行编辑 UE 这类巨型引擎及其周边的工具与二进制：**让一方做功能追加，让另一方尝试另一种实现，最后查看各自的变更，把需要的部分采纳回原始源码。**

为此，我们将宿主机上的源码作为只读的 lower 层引用，为每个 World 准备一个写入用的 upper 层，在 guest 内将两者叠加呈现为 /workspace。举例来说：公共源码中存在 src/main.cpp，World A 对其做了编辑，而 World B 不应看到 A 的变更——A 的修改保存进 A 的 upper，B 继续读取公共的 lower。

agctl-overlay 模式下的文件操作规则如下：

| 操作 | 文件的处理 |
|:--|:--|
| 读取未修改的文件 | 从公共 lower 层读取 |
| 修改已有文件 | copy-up 到自己的 upper 层后，在 upper 上修改 |
| 新建文件 | 保存到自己的 upper 层 |
| 删除 lower 层中的文件 | 在可写层一侧留下表示删除的 whiteout 标记 |
| agctl apply 反映 | 将 upper 的变更与删除应用回原始源码 |

公共的大文件始终保持引用，各环境只持有自己修改过的部分——这正是我们期望达到的效果。

不过，可写层并非没有代价：第一，以文件为单位的 copy-up 意味着，哪怕只修改一个大二进制的一小部分，也可能需要复制整个文件，构建产物同样会堆积在 upper 一侧，因此实际要修改的容量与 I/O 需要预先测量。第二，lower 只读的含义是沙箱不能直接写入，并不代表引用源被固定在过去的某个状态——一旦 A 的变更通过 apply 更新了公共源码，World B 读到的内容也会随之更新。第三，目前的 apply 只做变更的复制与删除，不做 Git 那样的冲突检测与三方合并，并行作业时基于哪个时点、何时反映、是否存在冲突，都需要自行管理。

## 二、执行环境：选型理由与模板资源匹配

沙箱运行时，我们采用了 CubeSandbox。

选择它的首要理由是轻量。与其他沙箱方案相比，它对宿主的要求更低，在迷你 PC 这样的小型机器上也能运行；对于需要准备多个工作环境的用途而言，增加环境的成本可以压得很小，这是一个显著的优势。我们不希望以"一开始就准备大型服务器"为前提，而是希望从小的宿主机起步、需要时再增加执行节点——Cube 的设计思路与此相符。此外，将执行环境交给 Cube 之后，我们可以专注于工作环境管理与变更采纳的实现，这也是选型的一个加分项。

启动时间与内存开销，官方 README 的基准数据可以作为参考：50 并发创建的平均耗时为 66.7ms；内存方面，即使改变 CPU 与内存配置，VMM 侧的额外开销（PSS）也维持在小范围内。需要说明的是，这些数值来自官方在裸金属上的测量，CPU、内存、I/O 与沙箱内的处理都会影响结果，官方性能报告也明确要求以实际环境和负载进行评估；手头的迷你 PC 与 KakuriZai 的开发环境，都需要另行测量。当然，编译 UE 所需的 CPU、内存与磁盘 I/O 仍按实际作业准备，使用 KVM 的构型也以运行 Cube 的 Linux 可用 KVM 为前提。

实际创建 World 时，先确定 Cube 的基础模板与所需资源，再生成包含挂载和网络等配置的创建请求。常规的 CubeMaster 路径下，通过 `cubemastercli multirun --norm request.json` 提交创建。

这里最先遇到的麻烦，是模板与磁盘容量的关系。从模板派生沙箱时，出现过以下报错导致创建失败：

```
'rootfs-gen0' is a snapshot; cannot resize
```

我们原本预期修改启动请求中的磁盘大小，模板即可按新容量完成创建，结果却阻塞在快照的 resize 环节。对于需要安装开发工具和大型源码的场景，在启动前就被阻塞构成了实际障碍——这个问题对所有需要按需调整磁盘容量的用户都可能遇到。

目前的 resolveTemplateForResources() 会对可写磁盘容量、CPU、内存、instance type 和 network type 五项进行核对。原始模板不匹配时，它会查找从同一原始镜像制作的兼容模板；存在则复用，不存在则从原始镜像新建。兼容模板的构建是异步任务，客户端会轮询任务状态直到 READY 或 FAILED，超时上限为 20 分钟。

另一个对开发体验有价值的设计是：宿主机上未安装 cubecli 时，World 的创建也不会失败。沙箱会被正常记录（含元数据、upper/work/whiteout 路径以及生成的完整 Cube 请求），运行时状态标记为 planned，待 Cube 可用后再真正创建。环境定义与实际执行因此解耦，这一降级策略也让无 Cube 环境下的配置管理成为可能。

## 三、在 guest 内叠加 lower 与 upper

沙箱创建完成后，需要将宿主机传入的 lower 与 upper 变成 guest 内的工作目录。原始文件可读、写入内容进入自己的 upper、对原始源码的反映等待 apply 完成——要实现这一语义，仅有目录可见是不够的，叠加后的文件系统必须能真正支撑实际的文件操作。

最初的实现从 kernel overlayfs 起步，随后加入了 FUSE 支持。目前在常规 CubeMaster 路径下，创建后会运行 setupOverlay()，按以下顺序尝试候选：

1. kernel overlayfs
2. unionfs-fuse
3. fuse-overlayfs

所有挂载点必须位于 guest 内的 /workspace 之下，越界的路径直接报错。这条约束将沙箱可访问的文件范围限定得非常严格。

### 删除操作的处理

在沙箱内删除 lower 层的文件时，并不直接删除原文件，而是留下一个 whiteout 标记；之后采纳变更时，这条删除记录也需要被正确读取。

这里存在一个容易踩中的陷阱：overlay 系的 whiteout 是以 `.wh.` 开头的文件名，而 unionfs-fuse 是在 `.unionfs-fuse` 目录下的文件名末尾追加 `_HIDDEN~`，两者的表现形式并不一致。若认为将 upper 原样复制即可，删除操作不会被反映，管理用的文件也会被一并混入源码。

为此，我们为 KakuriZai 的变更收集增加了读取 unionfs-fuse whiteout 的处理：将其作为删除传递给 apply，并将管理用文件排除在采纳范围之外。挂载方式一旦增加，读取变更的那一侧也必须同步适配——这是维护多挂载后端时的隐性成本，值得提前规划。

此外，根据宿主机文件的不同用途，挂载方式也可以选择：

| 方式 | 处理 |
|:--|:--|
| agctl-overlay | 在只读源码上叠加各 World 的可写层 |
| cubesandbox-readonly | 直接以只读方式呈现源码 |
| unsafe-rw | 直接写入原始源码，变更不等待 apply |

建议先在小型目录上，对已有文件的读取、修改、新建、删除、反映整个流程进行完整验证。将实际会用到的文件操作预先确认一遍，迁移大型项目之后出现问题时，也更容易定位。

## 四、进阶用法：多沙箱网络与带状态分叉

执行环境跑通之后，Cube 的网络能力也接入了进来。KakuriZai 在创建请求中直接使用 Cube 的 `network_type=tap`，暴露端口、DNS 配置和 cube_network_config 出站策略都通过请求下发；出站控制在宿主机一侧以 iptables 链（KAKURIZAI-EGRESS / KAKURIZAI-INGRESS）落地，无外网访问的沙箱会按公网 CIDR 全集构造 drop 规则。Studio 的网络视图基于各沙箱的 Cube 运行时 IP，从每个已配置的沙箱发起 ICMP/TCP 探测，将沙箱之间的可达与阻断关系渲染为拓扑。

这套网络能力还支撑了一个进阶用例：多沙箱 Kubernetes 实验室。Studio 的 Create K8s Lab 流程会批量创建控制面与 worker 沙箱，共享同一套 TAP 出站策略、暴露的 API/NodePort、CNI、pod/service CIDR 和 join token，生成的工作环境携带 kakurizai.kubernetes.\* 注解与标签，供运行时引导程序使用。一个由多个 Cube 沙箱组成的小型 K8s 集群，可以像单个 World 一样被创建和管理。

环境层面，准备好开发工具和依赖之后，自然会希望从该状态直接展开多个实验。这里用到的是快照与带状态复制：省去每次的环境准备，又能尝试不同的变更。`agctl replicate` 默认使用 stateful 模式，即先取出运行中环境的状态，再创建复制品。注册好放置节点后，按如下方式发起请求：

```
agctl replicate experiment-a --replicas 2 --state-mode stateful --json
```

### 快照保存的范围

这里有一条对所有同时使用 host mount 与快照的用户都适用的认知：VM 内外的数据保存位置不同。lower 与 upper 位于宿主机一侧，属于 VM 外侧的数据——即使创建了 VM 快照，这些数据也未必能作为独立的备份被保存下来。

因此，当前的带状态复制在存在 host mount 时，会先将 /workspace 呈现的内容复制到 guest 内的 /kakurizai/replication-state/workspace，再将该状态纳入快照或模板，并在复制目标端恢复。

恢复方式分为以下几种：

| 模式 | 实现 |
|:--|:--|
| runtime-snapshot | 在原节点上恢复，包含 rootfs 与内存状态 |
| template-snapshot | 将 commit 过的 AppSnapshot 模板分发到其他节点。常规路径不继承运行中的 RAM |
| stateful | 根据放置目标，在运行时快照与可搬运模板之间切换使用 |
| definition | 从保存的环境定义创建，不继承运行中的状态 |

rootfs、RAM、host mount、外部服务——各自能够保存和恢复的内容不同。查看恢复结果时，需要确认所选模式与内存获取的元数据；处理阻塞时，可查看 cube-replication-materialize.log 或快照、模板相关日志。

## 写在最后

这套方案的可写层并非没有代价，大二进制的 copy-up 成本与构建产物的容量需要预先测量；apply 不做三方合并，并行作业的基线选择与反映时机需要自行管理；官方的启动性能数据仅作参考，自己的宿主机需要另行测量。

对希望复现这套工作区的读者，我们的建议是从小处开始：先在小型目录上将读取、修改、新建、删除、反映的完整流程验证一遍，确认每一种文件操作的行为都符合预期，再迁移大型项目。排查问题的成本，会随项目体量一起放大。

## 参考链接

- [KakuriZai](https://github.com/IPA-CyberLab/IPA-RS-KakuriZai)：项目定位、CLI、挂载、带状态复制
- [CubeSandboxOverlayBackend](https://github.com/IPA-CyberLab/IPA-RS-KakuriZai/blob/4aaac549910715eaaf03a2eac6f936e3bc0f434e/src/backends/cubeSandboxOverlay.ts)：启动前的模板解析、创建结果与 overlay 状态管理
- [CubeSandboxClient](https://github.com/IPA-CyberLab/IPA-RS-KakuriZai/blob/4aaac549910715eaaf03a2eac6f936e3bc0f434e/src/cube/client.ts)：CLI / API 连接、exec、overlay 探测、快照与模板分发
- [挂载配置（mounts.ts）](https://github.com/IPA-CyberLab/IPA-RS-KakuriZai/blob/4aaac549910715eaaf03a2eac6f936e3bc0f434e/src/core/mounts.ts)：挂载模式与 /workspace 路径约束
- [CubeSandbox 官方 README](https://github.com/TencentCloud/CubeSandbox) 与[性能报告](https://github.com/TencentCloud/CubeSandbox/blob/e02976ae54729fb1684c5b51535381e54df1aea2/docs/blog/posts/2026-06-01-cubesandbox-perf-benchmark.md)：基准数据与评估条件
