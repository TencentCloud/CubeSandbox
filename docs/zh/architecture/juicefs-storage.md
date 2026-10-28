---
aside: false
---

# Cube + JuiceFS 跨机存储架构

本文将 `jfs-data.html` 中的 JuiceFS 方案架构和主要流程整理为文档内可直接渲染的图。部署步骤见 [JuiceFS 服务配置](../guide/cross-node-snapshot/juicefs.md)，调度门禁见[跨机恢复条件](../guide/cross-node-snapshot/requirements.md)。

## 设计目标

默认 `xfs` 后端依靠本机 reflink 实现快速克隆，但模板、暂停包和快照只能在源节点使用。JuiceFS 方案希望保留同样的文件操作模型，并增加以下能力：

- 模板、沙箱 rootfs、内存镜像和快照位于一个共享 POSIX 文件系统；
- 克隆继续使用 `copy_file_range`，只复制 JuiceFS slice 引用；
- 持久 block 存入对象存储，元数据和引用计数存入 Redis；
- 每个节点使用本地读缓存和 writeback staging；
- 数据上传完成后，暂停包或快照才进入 `remote_status=ready`，允许跨机调度。

## 架构图

<JuiceFSArchitecture mode="architecture" />

Cubelet、VMM 和 JuiceFS 客户端运行在每个计算节点。所有节点挂载同一个 JuiceFS 卷，通过 Redis 共享目录、inode、slice 和引用计数，通过 MinIO、S3 或 COS 持久化实际 block。每个节点的 `raw` 读缓存和 `rawstaging` 写缓存位于本机，不随沙箱迁移。

CubeMaster 负责模板任务、调度和远端就绪状态，TemplateCenter 负责拉取 OCI 镜像并生成 ext4 artifact。它们只参与控制流程，不转发 rootfs 或内存数据。

## 方案流程

<JuiceFSArchitecture mode="flows" />

流程图分别展示模板制作、沙箱启动和暂停恢复。选择步骤可查看当前参与的组件、文件和数据流；沙箱启动与暂停恢复还可以切换本机和跨机场景。

模板和派生文件可以引用相同的对象 block。`copy_file_range` 主要复制 slice 引用，只有修改区域生成新 slice 和 block。暂停包始终包含完整的 rootfs、memory 和 metadata 文件，不创建 Cube 管理的 `.delta` 文件。

跨机恢复前，CubeMaster 等待暂停包相关 staging block 上传完成并进入 `remote_status=ready`。目标节点随后克隆共享文件、以 `MAP_PRIVATE` 映射内存，并在本地缓存未命中时按需从对象存储读取。

## 内存脏页写入粒度

JuiceFS 将文件按 64 MiB chunk 组织；连续写入形成 slice，slice 再切成不超过 4 MiB 的对象 block。若 VMM 按 4 KiB 脏页逐次写入，会产生大量 slice 和 Redis 事务，并触发 compaction。`jfs-data.html` 中约 38 MiB 脏页的测试如下：

| 写法 | 暂停耗时 | 写入量 | 新 slice | Redis 事务 | compaction 重写 |
|---|---:|---:|---:|---:|---:|
| 4 KiB 逐页写 | 15.0 s | 20.6 MB | 2588 | 2739 | 1416 MB |
| 4 MiB 窗口 | 1.4–2.5 s | 约 750 MB | 53–88 | — | 0 |
| 1 MiB 窗口 | 1.33 s | 241 MB | 48 | 151 | 0 |

当前 VMM 将 FUSE 目标上的脏区间扩展到 1 MiB 对齐窗口，并合并相邻窗口后写入。它用可控的写放大换取更少的 slice、Redis 往返和 compaction；XFS 路径仍保持页粒度写入。

## 一致性与生命周期

- **共享可见性**：各节点通过同一元数据引擎看到相同文件树。
- **数据就绪**：`remote_status=ready` 表示包依赖的 writeback block 已离开源节点 staging。
- **本机优先**：源节点可调度时优先本机恢复，可继续利用页缓存、读缓存和 staging。
- **私有导入**：跨机恢复时，目标节点为 rootfs 和 memory 创建自己的克隆文件，避免直接把运行实例绑定到源包路径。
- **节点隔离**：本地 cache 不参与共享，也不能作为持久副本；丢失某个节点的 cache 不影响已上传的数据。
- **兼容性门禁**：存储共享不能替代 VM 兼容检查，目标节点仍需满足 CPU、kernel 和 KVM 条件。

## 主要取舍

| 优势 | 代价或限制 |
|---|---|
| 共享 POSIX 文件视图，Cube 改造集中在存储后端 | FUSE、Redis 和对象存储都会进入 I/O 路径 |
| `copy_file_range` 元数据克隆，模板和快照只存一份 | 随机小写会增加 slice，需要控制写入粒度 |
| 本机缓存加速常用模板和恢复 | 跨机冷读仍受对象存储延迟和带宽影响 |
| writeback 缩短前台写入路径 | 跨机必须等待 staging 上传并进入 `ready` |
| 容量和持久性由对象存储提供 | 元数据引擎成为高频克隆、写入时的重要性能依赖 |

JuiceFS 自身机制可参考官方的[技术架构](https://juicefs.com/docs/zh/community/architecture/)、[克隆语义](https://juicefs.com/docs/zh/community/guide/clone/)和[缓存说明](https://juicefs.com/docs/zh/community/guide/cache/)。
