---
title: "CubeSandbox 核心操作性能基准测试报告"
date: 2026-06-01
author: coolli
description: 本文公布 CubeSandbox 在真实裸金属节点上的性能基准测试数据，涵盖 Snapshot 制作、基于 Snapshot 启动沙箱、Rollback、Clone 四大核心操作，并给出测试环境、测试方法的完整说明。
featured: false
weight: 0
---

# CubeSandbox 核心操作性能基准测试报告

## 一、前言

CubeSandbox 面向 AI Agent 代码执行场景设计，极速冷启动和高并发是最关键的两项指标。本文给出在真实裸金属节点上测量的性能基准数据，涵盖 Snapshot 制作、基于 Snapshot 启动沙箱、Rollback、Clone 四大核心操作。

**重要说明：所有测试数据与测试环境、测试场景高度相关。** 影响因子包含但不限于：Host 的 CPU、内存、IO 性能，以及 Sandbox 内部负载（例如 Sandbox 中运行的程序越复杂、脏页越多，Snapshot 制作耗时也随之上升）。实际部署时请结合自身硬件和负载情况进行评估。

---

## 二、测试环境

### 2.1 硬件信息

| 项目 | 详情 |
|------|------|
| 机器类型 | 腾讯云 BMI5 裸金属服务器 |
| OS | OpenCloudOS (TencentOS Server 4) kernel 6.6.119 x86_64 |
| CPU 型号 | Intel(R) Xeon(R) Platinum 8255C @ 2.50GHz |
| CPU 配置 | 2 Socket × 24 Core × 2 Thread = **96 逻辑核** |
| NUMA 节点 | 2（node0: 0-23,48-71 / node1: 24-47,72-95） |
| 内存总量 | **375 GiB** DDR4-2933 MT/s ECC |
| 数据盘 | `/dev/nvme0n1` 3.84 TB Intel SSDPE2KX040T8 NVMe SSD，格式化为 XFS，挂载至 `/data` |

### 2.2 沙箱规格

| 项目 | 详情 |
|------|------|
| 规格 | 2 vCPU / 2 GiB 内存 |
| 测试镜像 | `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` |
| 存储 | CoW reflink（XFS，`/data/cubelet/storage/`） |
| 内存追踪 | soft-dirty（`/proc/PID/clear_refs`） |

---

## 三、测试方法

### 3.1 通用约定

- **所有时间单位：毫秒（ms）**
- `wall`：整批操作的端到端耗时（从第一个请求发出到最后一个完成）
- `per`：单次操作的均摊耗时（wall ÷ 本轮成功数）
- 每轮测试开始前执行 **Warm-up**，结果不计入统计，消除首次 cache miss 的干扰
- 测试脚本串行发起各轮次，**不同轮次之间无并发**，避免相互干扰

### 3.2 Snapshot 制作

在沙箱运行状态下调用 `POST /sandboxes/{id}/snapshots`，记录从请求发出到快照写入完成的耗时。

并发测试：同时对同一沙箱发起 N 次并发 Snapshot 请求，`wall` 为所有请求完成的总耗时，`per-snapshot` 为均摊到每个成功 Snapshot 的耗时（CubeSandbox 对单个沙箱的快照请求内部串行化，因此实际成功数可能小于并发数）。

**脏页（Dirty Page）说明：** CubeSandbox 使用 soft-dirty 机制只保存自上次 Snapshot 以来被修改过的内存页。实际写入量 = 脏页数 × 4 KiB，通常远小于沙箱总内存（2 GiB）。测试中"写入量"指测试脚本在 `/dev/shm`（tmpfs）预先写入的数据量，用于精确控制脏页大小；"Dirty Page"列为从 vmm.log 读取到的实际写入量，与理论值因 Guest OS 自身活动存在差异。

### 3.3 基于 Snapshot 启动沙箱（Create Sandbox from Snapshot）

调用 `POST /sandboxes`（指定 snapshot_id），记录从请求发出到沙箱进入 `running` 状态的端到端耗时。

并发测试：同时创建 N 个沙箱，`wall` 为所有沙箱就绪的总耗时，`per-sandbox` 为均摊耗时。

### 3.4 Rollback

对运行中的沙箱调用 `POST /sandboxes/{id}/rollback`，将其状态恢复至指定 Snapshot，记录完成耗时。

### 3.5 Clone

调用 `POST /sandboxes/{id}/clone`，从运行中的沙箱派生出新沙箱（保留完整内存和文件系统状态），记录耗时。

**补充说明：** 本次 Clone 测试涉及磁盘文件时，相关数据均已在 Page Cache 中，测试结果不含冷读 IO 开销。

---

## 四、测试结果

### 4.1 Snapshot 制作耗时与并发的关系

| 并发 | 轮数 | wall avg | wall min | wall p50 | wall p80 | wall max | per-snapshot avg | Dirty Page |
|:----:|:----:|--------:|--------:|--------:|--------:|--------:|----------------:|----------:|
| 1    | 5    | 43.3 ms  | 42.1 ms  | 43.5 ms  | 44.3 ms  | 44.3 ms  | 43.3 ms          | 10.4 MB   |
| 5    | 5    | 288.0 ms | 162.1 ms | 288.4 ms | 417.3 ms | 417.3 ms | **94.4 ms**      | 10.4 MB   |

串行单 Snapshot 约 **43 ms**；5 并发时，因内部串行化实际约 3 个请求成功，per-snapshot 均摊约 **94 ms**，整批 wall 约 **288 ms**（p80 约 417 ms）。

### 4.2 Snapshot 制作耗时与 Dirty Page 的关系

> 测试在串行模式下进行，每个数据点先预热（丢弃首次结果，排除 cache miss），再正式测量 3 次取均值。  
> "create sandbox avg" 列为基于该 Snapshot 创建新沙箱的耗时，反映 Dirty Page 大小对恢复速度的影响。

| 写入量   | Dirty Page  | snapshot avg | snapshot min | snapshot max | create sandbox avg | create sandbox min | create sandbox max |
|:--------:|------------:|------------:|------------:|------------:|------------------:|------------------:|------------------:|
| 0 MB     | 6.8 MB      | 41.2 ms      | 40.1 ms      | 42.3 ms      | 59.8 ms            | 57.2 ms            | 62.4 ms            |
| 10 MB    | 33.9 MB     | 59.6 ms      | 59.3 ms      | 59.8 ms      | 60.4 ms            | 57.7 ms            | 63.7 ms            |
| 50 MB    | 115.2 MB    | 90.8 ms      | 88.0 ms      | 96.2 ms      | 60.5 ms            | 56.7 ms            | 66.3 ms            |
| 100 MB   | 180.6 MB    | 115.1 ms     | 114.1 ms     | 116.0 ms     | 63.9 ms            | 57.7 ms            | 67.1 ms            |
| 200 MB   | 282.5 MB    | 155.4 ms     | 150.2 ms     | 165.8 ms     | 67.3 ms            | 66.0 ms            | 68.7 ms            |
| 500 MB   | 587.9 MB    | 263.3 ms     | 255.8 ms     | 277.4 ms     | 68.7 ms            | 62.5 ms            | 79.1 ms            |
| 800 MB   | 893.0 MB    | 371.3 ms     | 359.3 ms     | 391.4 ms     | 70.5 ms            | 57.5 ms            | 85.6 ms            |
| 1024 MB  | 1121.1 MB   | 448.1 ms     | 446.1 ms     | 449.3 ms     | 66.7 ms            | 63.5 ms            | 68.6 ms            |

**关键结论：**
- **Snapshot 制作耗时与 Dirty Page 大小近线性相关**：基线约 41 ms，每增加 100 MB 脏数据约增加 40 ms。
- **基于 Snapshot 创建新沙箱的耗时与 Dirty Page 大小无关**：无论 Dirty Page 多少，恢复耗时稳定在 **61–71 ms**，这是因为恢复只需加载内存快照，不依赖 Dirty Page 的大小。

### 4.3 基于 Snapshot 启动沙箱

| 并发 | n 总数 | 轮数 | wall avg | wall min | wall p50 | wall p80 | wall max | per-sandbox avg |
|:----:|:------:|:----:|--------:|--------:|--------:|--------:|--------:|----------------:|
| 1    | 1      | 3    | 69.9 ms  | 66.2 ms  | 70.2 ms  | 73.2 ms  | 73.2 ms  | 69.9 ms          |
| 10   | 10     | 3    | 89.7 ms  | 78.1 ms  | 88.6 ms  | 102.4 ms | 102.4 ms | **9.0 ms**       |
| 20   | 20     | 3    | 97.3 ms  | 86.9 ms  | 90.7 ms  | 114.4 ms | 114.4 ms | **4.9 ms**       |

单沙箱串行启动约 **70 ms**；20 并发时 wall 约 **97 ms**，均摊仅 **4.9 ms/个**，展现出极强的并发扩展能力。

### 4.4 Rollback

| 并发 | 轮数 | wall avg | wall min | wall p50 | wall p80 | wall max | per-rollback avg |
|:----:|:----:|--------:|--------:|--------:|--------:|--------:|-----------------:|
| 1    | 5    | 71.1 ms  | 40.2 ms  | 76.4 ms  | 102.6 ms | 102.6 ms | 71.1 ms           |
| 10   | 5    | 126.8 ms | 88.4 ms  | 105.1 ms | 194.2 ms | 194.2 ms | **12.7 ms**       |

单次 Rollback 约 **71 ms**；10 并发时均摊约 **12.7 ms/次**。

### 4.5 Clone

| 场景              | n   | 并发 | 轮数 | wall avg | wall min | wall p50 | wall p80 | wall max | per-clone avg | Dirty Page |
|:-----------------|:---:|:----:|:----:|--------:|--------:|--------:|--------:|--------:|--------------:|----------:|
| 1 个沙箱 1 并发   | 1   | 1    | 5    | 284.5 ms | 217.1 ms | 224.2 ms | 506.4 ms | 506.4 ms | 284.5 ms       | 10.4 MB   |
| 100 沙箱 10 并发  | 100 | 10   | 2    | 856.0 ms | 847.4 ms | 864.7 ms | 864.7 ms | 864.7 ms | **8.6 ms**     | 10.4 MB   |

Clone（含完整内存 + 文件系统状态）单次约 **285 ms**；100 个沙箱 10 并发时 wall 约 **856 ms**，均摊仅 **8.6 ms/个**。

---

## 五、小结

| 操作 | 串行单次 | 10 并发均摊 | 20 并发均摊 |
|------|--------:|----------:|----------:|
| Snapshot 制作（~10 MB 脏页） | ~43 ms | — | — |
| 基于 Snapshot 启动沙箱 | ~70 ms | ~9.0 ms | ~4.9 ms |
| Rollback | ~71 ms | ~12.7 ms | — |
| Clone（~10 MB 脏页）| ~285 ms | ~8.6 ms（10并发） | — |

CubeSandbox 的并发扩展性源于**资源池化 + 单机闭环**的设计（详见[架构文章](./2026-05-22-from-serverless-to-agent.md)）：Snapshot 恢复、CoW 磁盘克隆、cgroup/TAP 设备预分配均可并行进行，使得 wall time 随并发数增加的幅度极小。

如有疑问或希望分享你的测试结果，欢迎在 [GitHub Discussions](https://github.com/TencentCloud/CubeSandbox/discussions) 交流。

---

## 附录：测试脚本

本文所有测试均使用以下开源脚本完成，可在 [`examples/snapshot-rollback-clone/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/snapshot-rollback-clone) 目录找到：

| 脚本 | 对应章节 |
|------|---------|
| [`bench_snapshot_dirty.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_snapshot_dirty.py) | 4.2 Snapshot 制作耗时与 Dirty Page 的关系 |
| [`bench_snapshot_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_snapshot_concurrency.py) | 4.1 Snapshot 制作耗时与并发的关系 |
| [`bench_create_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_create_concurrency.py) | 4.3 基于 Snapshot 启动沙箱 |
| [`bench_rollback_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_rollback_concurrency.py) | 4.4 Rollback |
| [`bench_clone_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_clone_concurrency.py) | 4.5 Clone |
| [`rollback_demo.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/rollback_demo.py) | 功能验证 |
| [`clone_demo.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/clone_demo.py) | 功能验证 |
