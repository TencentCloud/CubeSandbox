---
title: "几十 GiB 快照秒回、克隆“零拷贝”：Cube 快照克隆回滚技术原理深层揭秘"
date: 2026-06-25
author: sionli
description: "磁盘快照秒回、内存快照只写少量页、克隆零拷贝——这三个看似魔法的现象背后，是 XFS reflink、/proc/pagemap 匿名页检测与 soft-dirty bit 三套内核机制在协作。本文层层拆解 v0.3.0 快照 / 克隆 / 回滚的底层原理。"
featured: true
weight: 3
---

# 几十 GiB 快照秒回、克隆"零拷贝"：Cube 快照克隆回滚技术原理深层揭秘

如果你在一台普通 Linux 服务器上跑过 Cube Sandbox v0.3.0，可能注意到几个反直觉的现象：磁盘快照"秒回"：对一个文件系统几十 GiB 的沙箱发起快照，命令几乎瞬间返回，磁盘没有发生数 GiB 的写入。内存快照只写"少量"页：一个跑着大模型推理、占着几十 GiB guest RAM 的沙箱，频繁 checkpoint 的转储量明显小于 guest 实际占用的内存——快照不再是把整块 RAM 重新落盘一遍。克隆"零拷贝"：对一个运行中的沙箱一次派生 10 份独立副本，磁盘空间几乎没增加，10 份副本却各自能在自己的内存和文件系统里写入而互不干扰。

这些"看起来像魔法"的现象，背后其实由三个互相咬合的底层机制共同支撑。本文从这三个谜题切入，一层一层揭开 v0.3.0 版本中快照 / 克隆 / 回滚三项核心能力的底层原理。

## 引言：三个让人意外的"现象"
如果你在一台普通的 Linux 服务器上跑过 Cube Sandbox v0.3.0，可能注意到几个反直觉的现象：

- **磁盘快照"秒回"**：对一个文件系统几十 GiB 的沙箱发起快照，命令几乎瞬间返回，磁盘没有发生数 GiB 的写入。
- **内存快照只写"少量"页**：一个跑着大模型推理、占着几十 GiB guest RAM 的沙箱，频繁 checkpoint 的转储量明显小于 guest 实际占用的内存——快照不再是把整块 RAM 重新落盘一遍。
- **克隆"零拷贝"**：对一个运行中的沙箱一次派生 10 份独立副本，磁盘空间几乎没增加，10 份副本却各自能在自己的内存和文件系统里写入而互不干扰。

这些"看起来像魔法"的现象，背后其实由三个互相咬合的底层机制共同支撑。本文从这三个谜题切入，一层一层揭开 v0.3.0 快照 / 克隆 / 回滚能力的底层原理。

## 一、揭秘的总入口：三个谜题与五层架构
整个快照 / 克隆 / 回滚体系的复杂度来自一个事实——**VM 状态 = 磁盘 + 内存**，二者必须保持一致地被捕获、被还原、被复制。Cube Sandbox 把它拆成两个独立又协作的子系统：

- **磁盘子系统**：基于 XFS reflink 的文件级 CoW 引擎。
- **内存子系统**：在传统 hypervisor 快照框架之上引入 pagemap_anon 与 soft-dirty 真增量。

它们各自跨越五层调用：

![五层调用架构图](./assets/2026-06-25-cubesandbox-snapshot-clone-rollback-deep-dive/01_architecture.png)

读完接下来三章，引言里的三个谜题就会变成三个清晰的内核机制：

| 谜题 | 核心机制 |
| --- | --- |
| 磁盘"秒回" | XFS Reflink + FICLONE ioctl |
| 内存"少写" | /proc/self/pagemap + soft-dirty bit55 |
| 克隆"零拷贝" | 把 clone(n) 拆成 snapshot + n 次基于快照的 create |

## 二、谜题一：磁盘快照为什么"秒回"
### 2.1 表象

对一个运行中的沙箱打快照时，磁盘在整个过程动作的本质是：一次 ioctl。

核心 ioctl：FICLONE，向目标文件发起源文件的 copy-on-write 克隆。它具有以下特性：

| 维度 | CubeCow Reflink |
| --- | --- |
| 工作层次 | 文件系统层 |
| 时间复杂度 | O(1)（单次 ioctl） |
| 持久化方式 | 文件系统本身即 source of truth，无独立 ledger |
| 崩溃恢复 | 每次操作是单个 fs 事务，天然 crash-safe |
| 内核依赖 | FICLONE ioctl，XFS -m reflink=1 |

**内核层面只共享 extent 元数据，写入时才分裂数据块**——这就是"秒回"的真相。

### 2.2 揭秘：XFS Reflink 内部到底做了什么

#### ① inode / Extent Map（BMBT）/ 物理块 三层结构

要理解 reflink，先得看 XFS 是怎么把"逻辑文件偏移"映射到"物理磁盘块"的：

![inode / BMBT / 物理块 三层结构](./assets/2026-06-25-cubesandbox-snapshot-clone-rollback-deep-dive/02_xfs_extent_structure.jpg)

- **BMBT（Block Mapping B-Tree）** 是 XFS 中 inode 内嵌的 B+ 树，存储逻辑偏移到物理块的映射表（即 Extent Map）。
- 每条 Extent 记录格式：`(logical_offset, physical_block, length, shared_flag)`。
- `shared_flag` 置位表示该物理块被多个文件 inode 引用，写入时必须触发 CoW unshare。

#### ② FICLONE ioctl 执行路径（O(1) 元数据操作）

FICLONE 之所以能在毫秒级完成，是因为它**只动元数据**：

![FICLONE ioctl 执行路径](./assets/2026-06-25-cubesandbox-snapshot-clone-rollback-deep-dive/03_ficlone_path.png)

**Refcount B-Tree**（xfs_rmap_btree）：XFS 维护一棵全局 B-Tree，记录每个物理块的引用计数。FICLONE 后被共享的块的 refcount ≥ 2；refcount 降为 1 时该块重新成为独占状态，可被直接写入无需 CoW。

#### ③ 写入时 CoW Unshare 路径

那共享之后再写入会发生什么？答案是**写时分裂**：

![写入时 CoW Unshare 路径](./assets/2026-06-25-cubesandbox-snapshot-clone-rollback-deep-dive/04_cow_unshare_path.png)

写入只影响**源卷**的 Extent Map；**快照**的 BMBT 和物理块完全不变，实现了快照隔离。

### 2.3 揭秘：CubeCow 在 reflink 之上额外做的事

裸 reflink 只解决"如何快"，不解决"如何管"。CubeCow 在引擎层叠加了三个工程化设计：

#### ① 快照链扁平化（避免 snap-of-snap 链式追踪）

标准 reflink 支持"快照的快照"，但 CubeCow 把所有快照的 `origin_volume` 统一记录为**最终祖先卷**，快照文件也物理上放在祖先卷的目录下。

![快照链扁平化：所有快照平级放在祖先卷目录](./assets/2026-06-25-cubesandbox-snapshot-clone-rollback-deep-dive/05_snapshot_chain_flattening.png)

**扁平化的好处**：

- 所有快照在文件系统视角是与主卷**平级的独立文件**——各自拥有独立的块映射，相互之间没有父子隶属关系，"血统"只是 CubeCow 内存索引里的一条逻辑信息。删除任意一个中间快照退化成对一个普通文件的 unlink：目录里去掉一个目录项，曾经共享的物理块由 XFS Refcount B-Tree 自动减一，其它快照完全不受影响。
- 目录结构与 `origin_volume` 一一对应，无需递归查找。
- 删除原卷主文件后，目录因快照文件存在而保留；最后一个快照删除时目录自动回收。

#### ② 文件系统即 Source of Truth（无 on-disk ledger）

**所有元数据均可从目录结构重建**：

- 卷列表 = `readdir(volumes/)`
- 快照列表 = `readdir(volumes/<vol>/)` 去掉主文件
- 大小 = `stat`，时间戳 = `mtime`

引擎启动时 `scan_and_rebuild_index()` 扫盘重建索引，按以下规则处理崩溃残留：

| 磁盘状态 | 处理方式 |
| --- | --- |
| `<vol>/<vol>` 主文件存在 | 注册为 Volume |
| 目录存在但主文件缺失且无子文件 | 删除空目录（孤儿） |
| 目录存在但主文件缺失且有子文件 | 警告，恢复子快照但不注册卷 |
| 零字节快照文件 | 删除（崩溃时 FICLONE 未完成） |
| 重名冲突 | 警告并跳过 |

#### ③ 内存命名空间扁平化

ReflinkEngine 维护一个 `RwLock<HashMap<String, NameKind>>` 作为全局命名空间，卷名与快照名共享同一全局命名空间，写锁下原子预占防止并发命名冲突。这意味着任何一次新建卷或新建快照都不需要做"先查血统再确认无重名"的递归校验，直接一次锁内查重即可。

### 2.4 揭秘小结

![秒回的全部秘密：抢名 + 一次 FICLONE + 落盘目录项](./assets/2026-06-25-cubesandbox-snapshot-clone-rollback-deep-dive/06_snapshot_summary.png)

一次快照在引擎层只有三类不可省略的动作：**抢名 -> 一次 FICLONE -> 落盘目录项**。三者都是 O(1) 量级，全部不涉及数据块拷贝——这就是"秒回"的全部秘密。

## 三、谜题二：内存快照如何只写"少量"页
### 3.1 表象与挑战

VM 内存动辄几十 GiB，如果每次快照都把整块 guest RAM 写盘，IO 放大会让"频繁 checkpoint"完全不可用。v0.3.0 的内存快照同时引入了**两条优化路径**，配合磁盘 reflink，把稳态下的内存写入量压到最小。性能数据可参考：CubeSandbox v0.3.0：让 AI Agent 拥有"时光机"和"分身术"。

### 3.2 三种模式的定义

![三种内存快照模式](./assets/2026-06-25-cubesandbox-snapshot-clone-rollback-deep-dive/07_memory_snapshot_modes.png)

Cube Sandbox 这里针对内存快照有三种模式，对应三种"我到底要把哪些页写到镜像里"的策略：

| 模式 | 写入对象 | 适用场景 |
| --- | --- | --- |
| Full | 完整 guest 内存镜像，所有页都写 | 第一次快照、强一致性归档 |
| Incremental | 仅 CoW 匿名页（即 guest 真正"分配过、有内容"的页） | 大多数稳态快照 |
| SoftDirty | 真增量：仅自上次复位以来被写过的匿名页 | 高频 checkpoint，内核需要 CONFIG_MEM_SOFT_DIRTY |

### 3.3 揭秘：Incremental —— "匿名页"恰好就是"自启动以来写过的页"

#### 关键前提：v0.3.0 的沙箱是"基于快照启动"的

理解 Incremental 的精妙之处必须先认清一个前提：在 Cube Sandbox 里，**几乎所有的 VM 都是从一份内存快照恢复出来的**——首次创建沙箱时从模板的内存镜像启动；克隆出来的副本从临时快照启动；回滚之后的 VM 从目标快照启动。"冷启动到完全零状态"的场景在生产路径上几乎不存在。

VMM 在恢复时**不会**把整份内存镜像 `read()` 到一段匿名 mmap 里——那样既慢又浪费。它的做法是用 `mmap(MAP_PRIVATE, fd=memory_image)` 把内存镜像文件直接映射到 guest RAM 对应的虚拟地址空间。这一步只建立 VMA，不读任何数据；guest 跑起来后真正访问到哪一页，内核才按需把那一页从 page cache 填进来。

#### MAP_PRIVATE 的二分语义：文件页 vs 匿名页

![MAP_PRIVATE 二分语义](./assets/2026-06-25-cubesandbox-snapshot-clone-rollback-deep-dive/08_map_private_semantics.png)

MAP_PRIVATE 的核心语义是**copy-on-write of a file**：

| guest 对该页的行为 | 内核侧的页类型 | 物理占用 |
| --- | --- | --- |
| 从未访问 | 不存在 PTE，按需缺页 | 0 |
| 只读访问过 | 文件页（共享 page cache，PTE 只读指向 page cache 帧） | 与同进程内其它快照实例共享 |
| 写过至少一次 | 匿名页（CoW unshare 出的进程私有页） | 该 VM 进程独占 |

注意第二行：guest 只读访问过的页**仍然是文件页**——它们物理上停留在内存镜像文件的 page cache 里，与从同一份镜像启动的其它 VM 共享，不计入本进程的匿名页统计。只有当 guest 第一次往某页写入时，内核才会触发 CoW，把这一页从文件页"分裂"为本进程独占的匿名页。

这就给出了一个**白送的等价关系**：

> **本 VM 进程的匿名页集合 ≡ 自该 VM 从快照启动以来真正写过的页集合**

这个等价关系不需要任何额外跟踪、没有任何运行时开销——它就是 MAP_PRIVATE 语义的自然产物。Linux 内核在每次写时缺页里早已为我们维护好了它。

#### Incremental 怎么读出这个集合

利用上面的等价，"哪些页需要写入快照"被简化成"哪些页是匿名页"。Linux 在 `/proc/<pid>/pagemap` 里以每页 8 字节暴露每条虚拟页的状态，关键位有：

| 位 | 含义 |
| --- | --- |
| bit 63 | 该 VPN 是否映射了物理页帧（present） |
| bit 62 | 是否被换出到 swap |
| bit 61 | 是否为匿名页（即被 CoW 分裂过的私有页） |
| bit 0–54 | 物理页帧号 PFN（present 时） |

Incremental 的过滤条件正是**present ∧ anonymous**——直接对应"自启动以来真正写入过的页"。每页只需 8 字节元数据判断，**无需读取页内容**。

![Incremental 判定流程](./assets/2026-06-25-cubesandbox-snapshot-clone-rollback-deep-dive/09_incremental_detection.png)

#### 完整性如何保证

Incremental 写出去的快照文件**保留完整的 guest 物理地址布局**——那些没写入的偏移**继承上一份快照**的内容。这要求：

- **目标文件已存在**且内容是 reflink-clone 自上一份快照——未被本次写入覆盖的偏移自动等于上一份快照对应位置。
- 文件页那部分（guest 只读访问过、或从未访问的页）在新快照文件里和上一份快照一字不差，因为它们本来就是同一份内存镜像的内容。

这就是 3.1 里说的"磁盘子系统反过来支撑内存子系统"——没有 reflink-clone 提供的"廉价基线文件"，Incremental 就没法用"只写一部分"得到"完整镜像"。

#### Incremental 解决了什么、还差什么

Incremental 用零运行时开销实现了一次性优化：**把"全部 guest RAM"收敛到"自启动以来写过的页"**。对短生命周期的沙箱（典型场景：一次性任务执行、短时克隆体）这已经足够。

但对**长期运行**的沙箱，这个集合只增不减——guest 跑得越久，被写过的页就越多，"自启动以来写过的页"会逐渐逼近"所有已分配页"。这就是 SoftDirty 要解决的问题。

### 3.4 揭秘：SoftDirty —— 用 bit55 抓"真正写过的页"

#### 动机：为什么 Incremental 不够

考虑一个长期运行的沙箱（比如一台跑着推理服务的 VM），它的生命周期里被多次定期 checkpoint：

| 时刻 | guest 累计写过的页 | 该次 Incremental 快照写入量 |
| --- | --- | --- |
| 启动后 t₁ | 200 MiB | 200 MiB |
| t₁ 后再跑 1 分钟，t₂ | 1 GiB | 1 GiB |
| t₂ 后再跑 10 分钟，t₃ | 5 GiB | 5 GiB |
| t₃ 后再跑 1 小时，t₄ | 15 GiB | 15 GiB |

虽然两次快照之间真正发生变化的可能只有几十 MiB，但 Incremental**每次都把"自启动以来全部写过的页"重新写一遍**。匿名页集合是单调递增的，转储量随运行时间线性增长，最终逼近 Full 模式。

要让"频繁 checkpoint"在长跑沙箱上仍然可用，必须能识别"上次快照之后才被写过的页"。这就是 SoftDirty 要补上的一块。

#### soft-dirty bit 的内核语义

Linux 在每个 PTE 里预留了 soft-dirty bit（在 `/proc/<pid>/pagemap` 里以 bit55 暴露给用户态），它的状态机非常简单：

| 触发动作 | 效果 |
| --- | --- |
| 进程往一页写入 | 该页 PTE 的 soft-dirty 被内核置位 |
| 用户态写 `/proc/<pid>/clear_refs`（值 4） | 内核遍历进程所有 PTE，清掉所有 soft-dirty 标记，并把对应 PTE 改成只读 |
| 复位之后再次写入 | 触发写保护缺页 -> 内核还原可写 + 重新置位 soft-dirty |

复位之后的 bit55=1 就精确等价于"自上次复位以来这一页有写过"。

#### 过滤条件：在匿名页之上加一层时间窗

SoftDirty 模式不是抛弃 Incremental，而是**在它的输出集合上再叠加一层 soft-dirty 过滤**：

```
要写入快照的页 = { p | present(p) ∧ anonymous(p) ∧ soft_dirty(p) }
                        └────── Incremental 集合 ──────┘ └ 增量过滤 ┘
```

anonymous 把范围收敛到"自启动以来写过的页"（参考 3.3）；soft_dirty 再把范围收敛到"自上次复位以来写过的页"。前者是**累积窗**，后者是**滑动窗**——两者交集就是"既属于该 VM 私有内存、又是这次快照真正需要刷新"的页。

落到代码视角就是同一段 pagemap 扫描里多读一位、多比一次按位与，**几乎无额外成本**。也正因为这一项是叠加而非替换，SoftDirty 在内核不支持时可以"静默降级"为 Incremental——丢掉的只是最后那个 ∧ soft_dirty 项，正确性不受影响。

![SoftDirty 时序：累积窗 + 滑动窗](./assets/2026-06-25-cubesandbox-snapshot-clone-rollback-deep-dive/10_softdirty_timeline.png)

#### 首次快照：为什么"自动"等价于 Incremental

soft-dirty 的初始状态是关键：内核在为页表建立 PTE 的瞬间会把**soft-dirty 默认置 1**——内核语义本身就是"如果这页存在 PTE 但你从没复位过，那就当它是脏的"。

所以第一次拍 SoftDirty 快照时，过滤条件**自动退化**为 Incremental——把所有匿名页都写一遍，正好就是这次快照需要的完整基线。然后我们才在写完之后调用 clear_refs 把 soft-dirty 全部清零，给"下一次稳态快照"建立基准。

这是个很优雅的性质：**SoftDirty 的首次快照不需要任何特殊分支**，过滤公式 anonymous ∧ soft_dirty 在两种状态下都是正确的——首次时 dirty 全 1 退化成 Incremental，之后每次都拿到真增量。

#### 一致性的两条铁律

要让上面这套机制正确，必须保证两点：

- **复位与写入不能交错**。如果在"复位 -> 拍下一份快照"之间允许 guest 自由写入，会出现"我写了但 bit 已被清"的窗口。v0.3.0 的策略是：每次打 SoftDirty 快照时 guest 已被 pause，在 pause 状态下完成"读 pagemap -> 写出快照 -> 复位 bit -> resume"。
- **复位必须发生在"快照之后"，而不是"快照之前"**。本次快照消费的是**上一次复位以来**累计的脏标记；本次写完后再复位，下一次快照才有正确的基准。首次快照同样遵循这条铁律——只不过它消费的是"内核给的初始全 1"，写完之后第一次 clear_refs 才让真正的"增量计时"开始。

#### 副作用：复位的代价被合理摊销

clear_refs 不是免费的——内核要做一次**全量页表项扫描**，把每个 PTE 改写为只读并清位。对多 GiB guest，这是数百毫秒级的开销，并且扫描期间 guest 后续的写入会引发额外的写保护缺页。

得益于上面"首次快照即 Incremental"的性质，v0.3.0 不需要为了 SoftDirty 在 VM 启动 / 恢复时提前付一次复位代价（那会让 VM ready 之后第一次进入用户空间时卡住几百毫秒）；而是把第一次 clear_refs 自然地推迟到首次快照的"写出之后"——此时用户已经在等待快照命令返回，复位的代价摊在了用户预期会消耗时间的操作里，对体验是无感的。

#### Incremental vs SoftDirty 的取舍

| 维度 | Incremental | SoftDirty |
| --- | --- | --- |
| 内核要求 | /proc/pagemap（普遍可用） | 额外要求 CONFIG_MEM_SOFT_DIRTY |
| 过滤强度 | 已分配的匿名页 | 已分配 ∧ 真写过 的匿名页 |
| 状态副作用 | 无（每次都现读 pagemap） | 有（需要复位 PTE，影响写保护缺页路径） |
| 适合的频率 | 中低频快照、首次快照、降级兜底 | 高频 checkpoint |
| 失败时行为 | 永远可用 | 自动降级到 Incremental |

### 3.5 外部内存卷支持

内存镜像可以写到独立存储介质（独立卷或独立路径），而不是和状态 JSON 同目录。这种模式对在不同存储池之间分担内存镜像的 I/O 压力很有帮助：内部模式倾向于"截断重建"以保证快照之间的独立性；外部卷模式倾向于"打开后原地写入"，从而让外部卷可以在多次快照之间被 reflink 复用。

## 四、谜题三：克隆 N 份为何"零拷贝"
### 4.1 表象

对一个跑着的源沙箱一次派生 N 个克隆，磁盘空间几乎没增加，每个克隆都能独立读写。每个克隆都满足三性质：

| 性质 | 含义 |
| --- | --- |
| 继承性 | 每个副本的初始状态与源沙箱在 clone 时刻完全一致 |
| 隔离性 | 副本之间的写入互不可见，与源沙箱也互相隔离 |
| 连续性 | 源沙箱在 clone() 返回后仍在运行，状态不受影响 |

### 4.2 揭秘：clone(n) 不是新原语，而是三个旧原语的组合

`clone(n)` 在协议层根本没有新增任何"克隆 RPC"，它在概念上等价于：

```python
def clone(self, n=1, *, concurrency=1):
  snap = self.create_snapshot()      # ① 一次源沙箱快照
  try:
    new_sbs = [Sandbox.create(template=snap.snapshot_id)
         for _ in range(n)]   # ② n 次基于快照的 create
  finally:
    Sandbox.delete_snapshot(snap.snapshot_id)  # ③ best-effort 清理
  return new_sbs
```

### 4.3 揭秘：为什么"零拷贝"和"完全隔离"能同时成立

这是磁盘子系统和内存子系统两套机制叠加的结果：

| 维度 | 共享什么 | 写入时怎么隔离 |
| --- | --- | --- |
| 磁盘 rootfs | 共享 XFS extent（refcount ≥ N+1） | XFS reflink CoW unshare |
| guest 内存 | 共享 reflink-clone 的 memory-ranges 文件 + 物理页 | 进程私有匿名页 + 内核 CoW（fork 一样的语义） |

所以 clone(n=10) 之后磁盘几乎没增长——10 份 rootfs 共享同一批 XFS 物理块；内存镜像也共享同一批 extent。任意副本写入时由内核负责"分裂"，互不干扰。

### 4.4 揭秘：并发 clone 的 fail-safe 语义

```python
clones = src.clone(n=10, concurrency=5)
```

参数 concurrency=C 会把第一步快照与最后一步删除快照仍只各做一次，只有中间的 N 次基于快照创建沙箱被并行化。

**全有或全无契约**：任一子任务失败时，已成功创建的克隆会被自动销毁，临时快照会被删除。调用方拿到的要么是 N 个沙箱，要么是异常，**不留孤儿资源**。

### 4.5 揭秘：源沙箱的连续性

派生临时快照时，VM 内部走的是 pause -> snapshot -> resume 三步，整个 pause 时长通常不到 100 毫秒。返回后源沙箱继续以原 PID、原内存映射运行——这正是"连续性"的来源。

## 最终章：把三个机制串起来 —— Cubelet 的三层降级策略
![CubeSandbox 快照端到端数据流 & 三层降级策略](./assets/2026-06-25-cubesandbox-snapshot-clone-rollback-deep-dive/11_degradation_strategy.png)

每一次提交快照时，节点端会决定两件事：内存模式选哪种 + reflink 基础卷指向哪一份历史。三层降级保证可用性：soft-dirty → pagemap_anon → full 的自动降级链，确保任何异常（基础快照被删除、快照断链、内核不支持 soft-dirty）都不会让用户操作失败，而是静默升级为正确但略大的快照。

## 写在最后
如果你正在构建需要代码执行、工具调用或多 Agent 协作的系统，欢迎了解和试用 Cube Sandbox。

如果觉得有帮助，欢迎点个 ⭐ Star，也欢迎提 Issue、PR 一起共建。你的每一个反馈都是项目持续演进的动力。

**Cube Sandbox 项目地址：https://github.com/TencentCloud/CubeSandbox**

---

Cube Sandbox 是腾讯云开源的一款基于 RustVMM 与 KVM 构建的高性能、开箱即用的安全沙箱服务。它既支持单机部署，也能方便地扩展到多机集群。对外兼容 E2B SDK，可在 60ms 内创建具备完整服务能力的硬件隔离沙箱，并将内存开销控制在 5MB 以内。
