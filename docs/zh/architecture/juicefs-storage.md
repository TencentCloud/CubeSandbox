---
aside: false
---

# Cube + JuiceFS 跨机存储架构

本文介绍 CubeSandbox 的基于 JuiceFS 的跨机存储方案架构和主要流程。

> 相关部署步骤见 [JuiceFS 服务配置](../guide/cross-node-snapshot/juicefs.md)
>
> 性能测试数据见 [JuiceFS 性能测试](/zh/blog/posts/2026-10-28-cubesandbox-juicefs-perf-benchmark)

## 设计目标

目前 Cube 的核心存储方案基于 xfs 文件系统实现，xfs 采用元数据和数据分离的设计，这使得克隆操作只需要复制 inode 和数据块引用，不需要实际复制数据块，从而实现快速克隆。

JuiceFS 方案希望保留同样的文件操作模型，即整体流程上和 xfs 方案一致，同时增加跨机的能力。这得益于 JuiceFS 本身提供的能力：

- **POSIX 兼容的分布式文件系统**，这意味着 JuiceFS 提供 POSIX 文件操作的接口。
- **分布式设计**：支持在多个节点上挂载相同的卷，强一致性保证各节点看到的文件视图相同。
- **元数据和数据分离**：支持类似 xfs 的快速 reflink 克隆。

> 关于 JuiceFS 的详细介绍，可以参考 [JuiceFS 官方文档](https://juicefs.com/docs/zh/community/introduction/)。

## 架构图

<JuiceFSArchitecture mode="architecture" />

Cube 的 JuiceFS 方案架构图如上所示，其中元数据引擎 Redis 和对象存储 MinIO 位于控制节点（也可以调整配置指向其他机器），JuiceFS 客户端运行在所有节点，并在所有节点挂载同一个 JuiceFS 卷。所有节点在 JuiceFS 卷上看到的文件视图相同，即节点 A 在 `/data/cubelet/jfs` 下创建的文件，节点 B 在 `/data/cubelet/jfs` 下也能看到。

每个节点单独设置的 `raw` 读缓存和 `rawstaging` 写缓存以优化性能，读写缓存的目录的后端可以是任何文件系统，只有 `/data/cubelet/jfs` 目录的后端是 JuiceFS 卷(JuiceFS 文件系统)。

## 方案流程

<JuiceFSArchitecture mode="flows" />

> 上面的流程图分别展示了模板制作、沙箱启动和暂停恢复的流程。选择步骤可查看当前参与的组件、文件和数据流；
>
> 沙箱启动与暂停恢复还可以切换本机和跨机场景。

得益于 JuiceFS 元数据和数据分离的设计，JuiceFS 可以使用 `copy_file_range` 快速克隆 rootfs 和 memory 文件。

得益于 JuiceFS 的分布式设计，在 A 节点创建的文件，在 B 节点也能看到，这使得跨机恢复时，B 机器可以像操作本地文件一样进行克隆操作。
