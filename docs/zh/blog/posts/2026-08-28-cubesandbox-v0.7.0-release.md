---
title: "CubeSandbox v0.7.0 发布：沙箱跨机流动，集群平滑演进"
date: 2026-08-28
author: Cube Sandbox 团队
description: "在 v0.6.0 中，我们把 CubeSandbox 送进了 Kubernetes，也交付了兼容 E2B 的 Volume 框架。v0.7.0 解决了生产用户最在意的两个问题：沙箱跨机器流动，以及升级不再打断已有资产。"
featured: true
weight: 1
---

# CubeSandbox v0.7.0 发布：沙箱跨机流动，集群平滑演进

在 v0.6.0 中，我们把 CubeSandbox 送进了 Kubernetes，也交付了兼容 E2B 的 Volume 框架。控制面能以标准工作负载运行，计算节点作为可调度资源纳入集群，存储的选择权回到了用户手里。

但不少团队真正把集群跑起来之后，会遇到一个最大的问题：沙箱只能待在一开始的那台机器上。暂停的沙箱换不了机器恢复，快照也只能在原节点用；更麻烦的是，组件一升级，之前做好的模板和快照就对不上了，只能重做。这两件事，正是生产用户最在意的。

于是，在 v0.7.0 中，我们将这些"需求"正式推上了研发日程：让沙箱跨机器流动，让升级不再打断已有资产。

## 1、跨机暂停恢复

过去，沙箱的 `pause/resume` 与基于快照的创建都绑定在单台宿主机上，在哪台机器暂停，就只能在哪台机器恢复。v0.7.0 **基于 S3 后端存储，将沙箱的内存与文件系统状态落到共享对象存储，从而支持在 A 节点暂停、在 B 节点恢复，也支持在任意已同步节点上用同一份快照拉起新沙箱。**

举个例子：一台计算节点要下线检修，上面还挂着一批暂停的沙箱。过去只能干等它们在本机恢复，或直接丢弃；现在可以把这些沙箱迁移到别的节点再恢复，节点排空不必再以牺牲沙箱为代价。再比如 Agent 训练这类需要大规模并行、频繁暂停恢复的场景，调度器可以按各节点的实时负载把沙箱放到最空闲的机器上。

该能力当前为预览版，默认由内置 MinIO 承载 S3 后端，用户也可自行指定对象存储。

## 2. 组件多版本共存

跨机流动会涉及一个连带问题：不同节点、不同时间构建的模板和快照，可能依赖不同版本的运行时组件；组件一旦升级，老的模板、快照就对不上，只能重做。**v0.7.0 让计算节点保留历史版本的组件，模板与快照不再因组件升级而失效，升级也不会打断已有实例的 pause/resume。**

在这之前，一个已经跑在生产上的集群，如果想升级到新版本修 bug，可能需要对现存的模板和快照集体作废，业务得停下来重新烤制。因此，对于需要长期迭代、持续升级的集群，v0.7.0 的这项特性更新，是从「每次升级都重做」到「升级和存量资产解耦」的重要转变。

## 3、网络子系统重构，加速沙箱网络创建流程

网络创建在沙箱冷启动链路上一直是关键路径，批量拉起时尤为明显。**v0.7.0 将原先独立的 NetworkAgent 整体合并到 Cubelet 中，减少沙箱创建流程中的 RPC 调用；同时优化 eBPF 网络策略下发路径，大幅降低带有网络规则的沙箱的创建延迟；并且优化高并发创建时 Tap 设备分配的稳定性。**

一个请求同时拉起上百个沙箱时，每个沙箱少两次 RPC、少一次 RCU 等待，这些开销随批量成比例叠加，冷启动时间因此下降。TAP 生命周期重构之后，节点重启、沙箱恢复时的网络异常也更少复现。

## 4、控制面、运维架构分离

**v0.7.0 将节点管理等运维能力从 CubeMaster 迁移至 CubeOps。** CubeOps 现在承载完整的节点管理，默认双副本部署，并用 cubeopscli 取代原先 cubemastercli 的 node 子命令；Web UI 同步提供节点隔离/解除隔离和节点操作记录查看。

那么，运维发现某台节点磁盘异常后，可以在 Web UI 上将这台节点隔离，让存量沙箱继续运行，新沙箱调度到别处。这样的优化能让组件职责边界更清楚：调度归 CubeMaster，运维归 CubeOps。

## 其他值得一提的特性

四条核心特性之外，v0.7.0 还重点实现了一批功能增强和 bug 修复。

- **三语言 SDK 能力拉齐。** Volume CRUD 与 volumeMounts 挂载此前只有 Python 支持，v0.7.0 在 Go 与 Node SDK 上补齐，三语言能力拉齐。模板别名此前覆盖不完整，这一版补齐了构建期别名与现有模板别名管理两个阶段，覆盖 Go / Node / Python。此外，Python SDK 支持用 `distribution_scope` 显式指定沙箱放到哪些节点或区域，Node SDK 支持运行时设置 NEVER_TIMEOUT，Go SDK 新增按用户身份隔离的文件视图 `Files.ForUser`。

- **存储块设备与模板来源扩展。** 新增基于 SPDK 的 CubeS3lvol COW：一个以 S3 对象存储为后端的远程写时复制块设备，本地写时分配、异步落 S3，经 `NVMe-oF/TCP` Loopback 导出给宿主，支持快照/克隆与跨节点导入导出，靠本地 WAL + journal 保证崩溃一致性。v0.7.0 内置 MinIO 作为默认 S3 卷后端，开箱即用；私有 HTTP 镜像仓库也能直接作为模板来源，不必再为内网镜像单独搭 HTTPS。

- **网络策略动态化与转发可配置。** 沙箱支持在运行时动态更新网络策略，无需重建即可调整规则；CubeEgress 的七层转发规则支持自定义端口，CubeProxy 支持自定义管理端口并新增明文 gRPC ingress，CubeVS 引入同子网 MAC 地址学习，避开非 hairpin 通信的黑洞。passfd 改用 bare-pipe vsock 直连，提升业务进程的 IO 效率。内核与 guest 镜像产物改为独立发布，发版时从固定的 kernel-release-\* / guest-image-\* 拉取，不再每次重新编译。

- **高频问题、稳定性问题等 bug 修复。** v0.7.0 集中处理了一批生产环境里出现频率较高的问题：暂停/恢复引发的状态不一致、僵尸进程与删除异常（PR [#978](https://github.com/TencentCloud/CubeSandbox/pull/978) / [#985](https://github.com/TencentCloud/CubeSandbox/pull/985) / [#1137](https://github.com/TencentCloud/CubeSandbox/pull/1137) / [#1274](https://github.com/TencentCloud/CubeSandbox/pull/1274)），快照性能问题（PR [#1300](https://github.com/TencentCloud/CubeSandbox/pull/1300) / [#1504](https://github.com/TencentCloud/CubeSandbox/pull/1504)），并发创建沙箱时的模板缓存数据竞争（PR [#1366](https://github.com/TencentCloud/CubeSandbox/pull/1366)），以及节点重启场景下 TAP 设备恢复异常（PR [#930](https://github.com/TencentCloud/CubeSandbox/pull/930) / [#987](https://github.com/TencentCloud/CubeSandbox/pull/987) / [#1207](https://github.com/TencentCloud/CubeSandbox/pull/1207)）。

## Coming Soon……

跨机能力落地后，Cube Sandbox 会继续沿着「云原生 + 高可用」方向，向更深处挖掘：

- **沙箱异常恢复**：自动检测并恢复 VM Crash、shim 进程卡死、网络分区等状态，恢复策略可配置（重启 / 回滚快照 / 向调用方报错）；

- **调度与运维能力增强**：资源感知调度、亲和/反亲和规则、优先级类，以及在线资源均衡和带沙箱迁移的节点排空。

更近的迭代版本（v0.7.1 或 0.8.0）中，有两项我们正在推进的：1）**控制链路全路径高可用**：模版制作管理从 CubeMaster 分离；CLM（cube-lifecycle-manager）支持多副本部署； 2）**沙箱异常恢复，节点故障沙箱可以跨节点恢复。**

欢迎给 Cube 提 PR 或 Issue，参与 Roadmap 设计讨论，一起推动 Cube Sandbox 的演进。

完整 Changelog：https://github.com/TencentCloud/CubeSandbox/blob/master/docs/zh/changelog/v0.7.0.md

Cube Sandbox 开源仓库地址：https://github.com/TencentCloud/CubeSandbox
