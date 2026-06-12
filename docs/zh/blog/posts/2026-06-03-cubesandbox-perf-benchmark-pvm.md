---
title: "CubeSandbox 核心操作性能基准测试报告（PVM 云服务器）"
date: 2026-06-03
author: coolli
description: 本文公布 CubeSandbox 在腾讯云 SA9.4XLARGE32 标准型云服务器（PVM 内核）上的性能基准测试数据，涵盖基于 Template 创建沙箱（冷启动延迟、并发扩展、单机密度）以及 Snapshot 相关操作（Snapshot 制作、基于 Snapshot 启动、Rollback、Clone、Pause/Resume）。每节附完整复现命令，读者可按步骤在自己的环境中复现测试。
featured: false
weight: 0
---

# CubeSandbox 核心操作性能基准测试报告（PVM 云服务器）

## 一、前言

CubeSandbox 面向 AI Agent 代码执行场景设计，极速冷启动和高并发是最关键的两项指标。本文给出在腾讯云标准型 CVM（运行 PVM 内核）上测量的性能基准数据，分为两大部分：

- **第三章：基于 Template 创建沙箱** — 启动延迟、并发扩展能力、单机部署密度
- **第四章：Snapshot 相关操作** — Snapshot 制作、基于 Snapshot 启动沙箱、Rollback、Clone

每节均附有完整的测试命令，读者可直接照步骤在自己的环境中复现。

**重要说明：所有测试数据与测试环境、测试场景高度相关。** 影响因子包含但不限于：Host 的 CPU、内存、IO 性能，以及 Sandbox 内部负载（例如 Sandbox 中运行的程序越复杂、脏页越多，Snapshot 制作耗时也随之上升）。实际部署时请结合自身硬件和负载情况进行评估。

与[裸金属版测试报告](./2026-06-01-cubesandbox-perf-benchmark.md)相比，本文使用的是标准型虚拟化 CVM（SA9.4XLARGE32），CPU 核数和内存规模更小，可作为中小规模部署的参考基准。

---

## 二、测试环境

### 2.1 硬件信息

| 项目 | 详情 |
|------|------|
| 机器类型 | 腾讯云[标准型云服务器 SA9.4XLARGE32](https://cloud.tencent.com/document/product/213/11518)（可在腾讯云控制台购买同款） |
| 可用区 | — |
| OS | OpenCloudOS 9.4 |
| 内核 | `6.6.69-opencloudos9.cubesandbox.pvm.host` |
| CPU 型号 | AMD EPYC 9K65 @ — |
| CPU 配置 | 1 Socket × 16 Core × 1 Thread = **16 逻辑核** |
| NUMA 节点 | 1（node0: 0-15） |
| 内存总量 | **32 GiB** |
| 系统盘 | `/dev/vda` 200 GiB 增强型 SSD 云硬盘，格式化为 XFS，挂载至 `/` |

> **SA9.4XLARGE32** 是腾讯云标准型第九代实例，搭载 AMD EPYC 9K65 处理器，适合通用计算场景。本文运行 PVM（Parallel Virtual Machine）内核，支持嵌套虚拟化，可在普通云服务器上运行 CubeSandbox。如需复现本文测试，可前往[腾讯云 CVM 购买页](https://buy.cloud.tencent.com/cvm)选购同款。

> 安装 CubeSandbox 请参照[快速开始指南](../../guide/quickstart.md)。

### 2.2 沙箱规格与模板制作

所有测试统一使用以下规格的沙箱：

| 项目 | 详情 |
|------|------|
| 规格 | 2 vCPU / 2 GiB 内存 |
| 测试镜像 | `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` |
| 存储 | CoW reflink（XFS，`/data/cubelet/storage/`） |
| 内存追踪 | soft-dirty（`/proc/PID/clear_refs`） |

测试前需先制作模板（国内使用 `cn` 镜像仓库，境外使用 `int`）：

```bash
cubemastercli tpl create-from-image \
  --image cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest \
  --writable-layer-size 1G \
  --expose-port 49999 \
  --expose-port 49983 \
  --probe 49999
```

等待构建完成后，记录模板 ID：

```bash
# 查看模板列表，取第一行 tpl- 开头的 ID
cubemastercli tpl list
```

### 2.3 指标说明

| 指标 | 含义 |
|------|------|
| **avg** | 多轮测试的平均值 |
| **min** | 最小值 |
| **p95** | 第 95 百分位（95% 的请求都在此时间内完成） |
| **max** | 最大值 |
| **wall** | 整批操作的端到端耗时（从第一个请求发出到最后一个完成），用于并发场景 |
| **per** | 单次操作的均摊耗时（wall ÷ 本批操作数），用于并发场景 |

所有时间单位均为**毫秒（ms）**。每轮测试开始前执行 **Warm-up**（首轮结果丢弃），消除 page cache 冷读干扰；并发测试的各轮次之间串行发起，无交叉并发。

---

## 三、基于 Template 创建沙箱

本章测试「从零启动一个可用沙箱」的端到端耗时，即调用 `POST /sandboxes`（指定 `template_id`）到沙箱进入 `running` 状态的时间。这是最常见的使用场景。

### 3.1 环境准备与验证

**第一步：安装 Python SDK 并设置环境变量**

```bash
pip install e2b-code-interpreter

export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000           # 本地部署填任意非空字符串
export CUBE_TEMPLATE_ID=<your-template-id>  # 上一步 cubemastercli tpl list 查到的 ID
export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem  # mkcert 证书路径
```

**第二步：跑一个 Hello World 验证环境正常**

在执行任何基准测试前，先运行以下脚本确认沙箱可以正常创建和执行代码：

```python
import os
from e2b_code_interpreter import Sandbox

with Sandbox.create(template=os.environ["CUBE_TEMPLATE_ID"]) as sandbox:
    result = sandbox.run_code("print('Hello from Cube Sandbox, safely isolated!')")
    print(result)
    print("✅ 环境验证通过，可以开始基准测试")
```

保存为 `hello.py` 并执行：

```bash
python hello.py
```

看到 `✅ 环境验证通过` 字样即说明 CubeSandbox 部署正常，可继续后续测试。如果报错，请先参考 [Quick Start](../../guide/quickstart.md) 排查环境问题。

### 3.2 启动延迟与并发扩展

使用 [`cube-bench`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/cube-bench) 工具测量不同并发度下的沙箱创建延迟。`cube-bench` 用 Go 协程并发驱动 CubeAPI，测量结果包含完整百分位统计。

**编译工具（需要 Go 1.21+）：**

```bash
cd examples/cube-bench
make
# 产出: ./bin/cube-bench
```

**执行测试：**

```bash
# 设置环境变量
export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000
export CUBE_TEMPLATE_ID=<your-template-id>

# 1 并发，共 20 次（创建后立即删除）
./bin/cube-bench -c 1 -n 20 -w 3

# 10 并发，共 200 次
./bin/cube-bench -c 10 -n 200 -w 3

# 20 并发，共 300 次
./bin/cube-bench -c 20 -n 300 -w 3
```

> `-w 3` 表示先做 3 轮热身，热身结果不计入统计。

**测试数据（腾讯云 SA9.4XLARGE32 PVM，2 vCPU / 2 GiB 沙箱）：**

| 并发 | 请求数 | avg | min | P50 | P90 | P95 | P99 | max |
|:----:|:------:|----:|----:|----:|----:|----:|----:|----:|
| 1    | 20  | 66.7 ms | 55.9 ms | 64.5 ms | 77.5 ms | 78.2 ms | 80.2 ms | 80.2 ms |
| 10   | 200 | 170.9 ms | 85.4 ms | 168.5 ms | 206.4 ms | 216.7 ms | 286.1 ms | 323.5 ms |
| 20   | 300 | 364.6 ms | 116.5 ms | 356.2 ms | 459.0 ms | 521.4 ms | 673.8 ms | 744.0 ms |

> 各档均独立测试，档间清空所有沙箱并留出资源池恢复时间，避免相互干扰。所有档位成功率均为 **100%**。

**关键结论：**
- 单并发创建延迟约 **67 ms**（min 55.9 / P95 78.2），延迟极低且非常稳定
- 10 并发 avg 171 ms，均摊单沙箱仅 **17.1 ms**，并发扩展势头良好
- 20 并发 avg 365 ms，均摊单沙箱 **18.2 ms**，P99 674 ms 显示少量长尾请求受队列压力影响

### 3.3 单机部署密度（内存开销）

CubeSandbox 通过内核共享与写时复制（CoW）将自身额外开销压缩到极低水平。本节通过「清空机器 → 分批启动沙箱 → 记录内存变化」的方式，实测单实例净开销。

> ⚠️⚠️⚠️ **重要安全提醒**
>
> **每次启动沙箱前，务必先用 `free -h` 确认机器剩余内存充足，每次只启动少量，边启动边观察内存余量，确认安全后再继续下一批，切勿一次性启动过多！** 内存耗尽会触发 OOM Killer，轻则进程被杀，重则损坏运行环境，需重新部署。请根据自己机器的实际内存容量决定每批启动的数量。

**第一步：记录基线（空机内存）**

```bash
export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000
export CUBE_TEMPLATE_ID=<your-template-id>

# 确保没有残留沙箱
cubemastercli list

# 记录空机内存用量
free -h
# 同时记录 shim 进程数（应为 0）
ps --no-headers -C containerd-shim-cube-rs | wc -l
```

**第二步：分批启动沙箱，每批结束后用 `free -h` 记录当前已用内存**

用 `cube-bench` 的 `create-only` 模式批量创建并保持沙箱存活：

```bash
# 设置环境变量（与 §3.2 一致；换新终端窗口需重新 export）
export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000              # 本地部署填任意非空字符串
export CUBE_TEMPLATE_ID=<your-template-id> # cubemastercli tpl list 查到的 ID

./bin/cube-bench -c 1  -n 1  -m create-only && free -m   # 累计 1
./bin/cube-bench -c 4  -n 4  -m create-only && free -m   # 累计 5
./bin/cube-bench -c 5  -n 5  -m create-only && free -m   # 累计 10
./bin/cube-bench -c 10 -n 10 -m create-only && free -m   # 累计 20
```

**第三步：计算单实例开销**

```
单 VM 均摊开销 = (当前 used - 基线 used) ÷ VM 数量
```

**测试数据（腾讯云 SA9.4XLARGE32 PVM，2 vCPU / 2 GiB 规格沙箱）：**

| 存活沙箱数 | 系统 available (MB) | 单 VM 均摊开销 |
|:---------:|:------------------:|:-------------:|
| 0（基线）  | 25570 MB           | — |
| 1          | 25536 MB           | **~34 MB** |
| 5          | 25436 MB           | **~27 MB** |
| 10         | 25252 MB           | **~32 MB** |
| 20         | 24990 MB           | **~29 MB** |

> 实测单 VM 均摊开销约 **27～34 MB**，CoW 按需分配效果显著——2 GiB 规格的沙箱空载时实际仅占用约 30 MB。

**单机可运行实例数估算（SA9.4XLARGE32，32 GiB 内存）：**

```
总内存：                32768 MB
系统基线占用（实测）：    7198 MB（= 32768 - 25570，空机 available 实测值）
安全水位预留（10%）：     3276 MB
可分配给沙箱：          22294 MB（= 32768 - 7198 - 3276）

空载/轻载场景（CoW 按需分配，均摊 ~30 MB/个）：
  22294 ÷ 30 ≈ 743 个

满载场景（每沙箱实际写满 2 GiB）：
  22294 ÷ (2048 + 30) ≈ 10 个
```

---

## 四、Snapshot 相关操作

Snapshot 是 CubeSandbox 的核心功能之一，支持在运行中的沙箱上制作内存 + 文件系统快照，后续可基于快照极速恢复（Clone / Rollback）。

**安装依赖：**

```bash
cd examples/snapshot-rollback-clone
pip install -r requirements.txt   # 安装 cubesandbox SDK

# 以下环境变量为所有 4.x 压测脚本的前置，每个新 shell 都需先 export（或写入 env.sh 后 source）
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>          # cubemastercli tpl list 查到的 ID
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>       # 在 CubeProxy 本机运行时可写 127.0.0.1
export CUBE_PROXY_PORT_HTTP=80                      # CubeProxy 监听端口, 默认 80
```

> 下文 4.1~4.5 各节命令块默认你已在当前 shell 完成上述 `export`（脚本通过 `env.py` 读取这些变量）。换新终端窗口请重新 export。

### 4.1 Snapshot 制作耗时与并发的关系

**测试方式：** 在运行中的沙箱上调用 `POST /sandboxes/{id}/snapshots`，N 并发时同时对 N 个沙箱各发起一次快照请求，测量整批完成的 wall time。

> CubeSandbox 对**同一个沙箱**的快照请求内部串行化，因此并发测试中每个沙箱对应独立的快照请求，实际成功数等于并发数。

**执行命令：**（脚本：[`bench_snapshot_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_snapshot_concurrency.py)）

```bash
cd examples/snapshot-rollback-clone
# 若是新开的终端，先设置环境变量（同 4.0 安装依赖一节）：
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

# 脚本只提供单档机制，并发档位在命令行控制；逐档串行调用：
python bench_snapshot_concurrency.py -c 1  -n 5
python bench_snapshot_concurrency.py -c 5  -n 5 --no-header
python bench_snapshot_concurrency.py -c 10 -n 5 --no-header
```

**测试数据**（全新沙箱原样打快照，实测脏页约 **8 MB**，由 `/data/log/CubeVmm/vmm.log` 中 `PagemapAnon snapshot saved` 记录确认；该值为沙箱基线匿名内存页大小，本节不作为变量）：

| 并发 | 轮数 | wall avg | wall min | wall p95 | wall max | per-snapshot avg |
|:----:|:----:|--------:|--------:|--------:|--------:|----------------:|
| 1    | 5    | 41.4 ms  | 37.6 ms  | 48.7 ms  | 48.7 ms  | 41.4 ms |
| 5    | 5    | 58.2 ms  | 51.0 ms  | 66.1 ms  | 66.1 ms  | **11.6 ms** |
| 10   | 5    | 114.1 ms | 66.2 ms  | 285.2 ms | 285.2 ms | **11.4 ms** |

串行单 Snapshot 约 **41 ms**；5 并发时整批 wall 约 **58 ms**，per-snapshot 均摊降至约 **11.6 ms**；10 并发时整批 wall 约 **114 ms**，均摊进一步降至约 **11.4 ms**，并发摊薄效果显著。

### 4.2 Snapshot 制作耗时与 Dirty Page 的关系

**背景：** CubeSandbox 使用 soft-dirty 机制，只保存自上次 Snapshot 以来被修改过的内存页。实际写入量 = 脏页数 × 4 KiB，通常远小于沙箱总内存（2 GiB）。

测试通过在 `/dev/shm`（tmpfs）预先写入数据来精确控制脏页大小。"Dirty Page" 列为从 `/data/log/CubeVmm/vmm.log` 读取到的实际写入量，与理论值因 Guest OS 自身活动存在差异。

**执行命令：**（脚本：[`bench_snapshot_dirty.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_snapshot_dirty.py)）

```bash
cd examples/snapshot-rollback-clone
# 若是新开的终端，先设置环境变量（同 4.0 安装依赖一节）：
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

# 脚本只提供单档机制，脏页大小在命令行控制（-d 即写入量 MB）；按需逐档调用：
python bench_snapshot_dirty.py -d 0    -n 3
python bench_snapshot_dirty.py -d 10   -n 3 --no-header
python bench_snapshot_dirty.py -d 50   -n 3 --no-header
python bench_snapshot_dirty.py -d 100  -n 3 --no-header
python bench_snapshot_dirty.py -d 200  -n 3 --no-header
python bench_snapshot_dirty.py -d 500  -n 3 --no-header
python bench_snapshot_dirty.py -d 800  -n 3 --no-header
python bench_snapshot_dirty.py -d 1024 -n 3 --no-header
```

> 测试在串行模式下进行，每个数据点先预热（丢弃首次结果），再正式测量 3 次取均值。"create sandbox avg" 列为基于该 Snapshot 创建新沙箱的耗时，反映 Dirty Page 大小对恢复速度的影响。

**测试数据：**

| 写入量 | Dirty Page | snapshot avg | snapshot min | snapshot p95 | snapshot max | create sandbox avg | create sandbox min | create sandbox p95 | create sandbox max |
|:------:|----------:|------------:|------------:|------------:|------------:|------------------:|------------------:|------------------:|------------------:|
| 0 MB    | 8.3 MB    | 42.1 ms  | 37.6 ms  | 45.9 ms  | 45.9 ms  | 71.6 ms | 65.4 ms | 77.7 ms | 77.7 ms |
| 10 MB   | 41.2 MB   | 55.3 ms  | 54.1 ms  | 56.6 ms  | 56.6 ms  | 73.1 ms | 60.4 ms | 82.5 ms | 82.5 ms |
| 50 MB   | 122.6 MB  | 67.7 ms  | 66.5 ms  | 69.6 ms  | 69.6 ms  | 70.3 ms | 63.9 ms | 81.4 ms | 81.4 ms |
| 100 MB  | 195.2 MB  | 85.7 ms  | 82.5 ms  | 88.7 ms  | 88.7 ms  | 68.3 ms | 62.3 ms | 71.6 ms | 71.6 ms |
| 200 MB  | 296.8 MB  | 100.9 ms | 98.5 ms  | 102.6 ms | 102.6 ms | 65.9 ms | 62.7 ms | 71.2 ms | 71.2 ms |
| 500 MB  | 602.6 MB  | 168.6 ms | 165.4 ms | 172.9 ms | 172.9 ms | 68.1 ms | 54.5 ms | 75.7 ms | 75.7 ms |
| 800 MB  | 908.3 MB  | 215.8 ms | 212.1 ms | 217.6 ms | 217.6 ms | 68.1 ms | 60.9 ms | 79.1 ms | 79.1 ms |
| 1024 MB | 1136.3 MB | 257.5 ms | 251.2 ms | 267.6 ms | 267.6 ms | 62.3 ms | 56.5 ms | 69.6 ms | 69.6 ms |

**关键结论：**
- **Snapshot 制作耗时与 Dirty Page 大小近线性相关**：基线（8.3 MB 脏页）约 42 ms，每增加 100 MB 脏数据约增加 22 ms，1024 MB 时约 258 ms
- **基于 Snapshot 创建新沙箱的耗时与 Dirty Page 大小无关**：恢复耗时稳定在 **54–83 ms**，因为恢复采用 CoW（写时复制）按需加载，不依赖 Dirty Page 的大小

### 4.3 基于 Snapshot 启动沙箱

**测试方式：** 先制作一个快照，然后并发调用 `POST /sandboxes`（指定 `snapshot_id`），测量从请求发出到所有沙箱进入 `running` 的端到端 wall time。

**执行命令：**（脚本：[`bench_create_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_create_concurrency.py)）

```bash
cd examples/snapshot-rollback-clone
# 若是新开的终端，先设置环境变量（同 4.0 安装依赖一节）：
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

# 脚本只提供单档机制，档位在命令行控制；按需逐档调用：
python bench_create_concurrency.py -c 1  -n 3
python bench_create_concurrency.py -c 10 -n 3 --no-header
python bench_create_concurrency.py -c 20 -n 3 --no-header
```

**测试数据：**

| 并发 | n 总数 | 轮数 | wall avg | wall min | wall p95 | wall max | per-sandbox avg |
|:----:|:------:|:----:|--------:|--------:|--------:|--------:|----------------:|
| 1    | 1   | 3 | 66.7 ms  | 65.8 ms  | 68.3 ms  | 68.3 ms  | 66.7 ms |
| 10   | 10  | 3 | 387.9 ms | 364.4 ms | 420.3 ms | 420.3 ms | **38.8 ms** |
| 20   | 20  | 3 | 701.3 ms | 660.5 ms | 742.4 ms | 742.4 ms | **35.1 ms** |

单沙箱启动约 **67 ms**；10 并发时 wall 约 **388 ms**，均摊仅 **38.8 ms/个**；20 并发时 wall 约 **701 ms**，均摊仅 **35.1 ms/个**，展现出良好的并发扩展能力。

### 4.4 Rollback

**测试方式：** 对运行中的沙箱调用 `POST /sandboxes/{id}/rollback`，将沙箱内存和文件系统状态原地恢复至指定 Snapshot，无需重建沙箱。

> **快照所有权约束：** CubeSandbox 只允许沙箱回滚到**自己创建**的 checkpoint，每个并发沙箱均需独立完成「打快照 + 回滚」全流程。

**执行命令：**（脚本：[`bench_rollback_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_rollback_concurrency.py)）

```bash
cd examples/snapshot-rollback-clone
# 若是新开的终端，先设置环境变量（同 4.0 安装依赖一节）：
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

# 脚本只提供单档机制，档位在命令行控制；按需逐档调用：
python bench_rollback_concurrency.py -c 1  -n 5
python bench_rollback_concurrency.py -c 5  -n 5 --no-header
python bench_rollback_concurrency.py -c 10 -n 5 --no-header
```

**测试数据：**

| 并发 | 轮数 | wall avg | wall min | wall p95 | wall max | per-rollback avg |
|:----:|:----:|--------:|--------:|--------:|--------:|-----------------:|
| 1    | 5 | 90.0 ms  | 82.0 ms  | 98.3 ms  | 98.3 ms  | 90.0 ms |
| 5    | 5 | 325.5 ms | 322.9 ms | 329.4 ms | 329.4 ms | **65.1 ms** |
| 10   | 5 | 821.4 ms | 778.7 ms | 858.1 ms | 858.1 ms | **82.1 ms** |

> 单次 Rollback 流程（`create_snapshot` 打点 → `rollback` 回滚到自身 checkpoint）约 **90 ms**；5 并发时整批 wall 约 **326 ms**，per-rollback 均摊降至约 **65 ms**；10 并发时整批 wall 约 **821 ms**，均摊约 **82 ms/次**。
>
> 注：因 CubeSandbox 要求沙箱只能回滚到自己创建的 checkpoint，故无法复用共享快照，每个并发沙箱均需独立完成「打快照 + 回滚」全流程。

### 4.5 Clone

**测试方式：** 调用 `POST /sandboxes/{id}/clone`，从一个**运行中**的沙箱派生出 N 个新沙箱，完整保留源沙箱的内存和文件系统状态（包含脏页）。

> **说明：** 本次 Clone 测试涉及磁盘文件时，相关数据均已在 Page Cache 中，测试结果不含冷读 IO 开销。

**执行命令：**（脚本：[`bench_clone_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_clone_concurrency.py)）

```bash
cd examples/snapshot-rollback-clone
# 若是新开的终端，先设置环境变量（同 4.0 安装依赖一节）：
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

# 脚本只提供单场景机制，n/并发/轮数在命令行控制；按需逐场景调用：
python bench_clone_concurrency.py -n 1  -c 1  --rounds 5
python bench_clone_concurrency.py -n 10 -c 5  --rounds 3 --no-header
python bench_clone_concurrency.py -n 20 -c 10 --rounds 3 --no-header
```

**测试数据（源沙箱脏页约 10 MB）：**

| 场景               | n   | 并发 | 轮数 | wall avg | wall min | wall p95 | wall max | per-clone avg |
|:------------------|:---:|:----:|:----:|--------:|--------:|--------:|--------:|--------------:|
| 1 个沙箱 1 并发    | 1   | 1  | 5 | 270.6 ms | 260.8 ms | 280.5 ms | 280.5 ms | 270.6 ms |
| 10 沙箱 5 并发    | 10  | 5  | 3 | 541.6 ms | 522.9 ms | 557.7 ms | 557.7 ms | **54.2 ms** |
| 20 沙箱 10 并发   | 20  | 10 | 3 | 789.7 ms | 757.2 ms | 815.3 ms | 815.3 ms | **39.5 ms** |

Clone 单沙箱约 **271 ms**；10 沙箱 5 并发时整批 wall 约 **542 ms**，per-clone 均摊降至约 **54 ms**；20 沙箱 10 并发时整批 wall 约 **790 ms**，均摊进一步降至约 **40 ms/个**，并发摊薄效果显著。


### 4.6 Pause / Resume

**测试方式：** 创建 `concurrency` 个沙箱，并发调用 `POST /sandboxes/{id}/pause` 暂停全部，再并发调用 `POST /sandboxes/{id}/resume` 恢复全部，分别记录 pause 和 resume 的 wall time 与均摊耗时。

> ⚠️ **当前实现说明：** Pause 当前采用 **full-memory-copy 模式**——暂停时会将沙箱的全部匿名内存页写入持久化存储，耗时与沙箱总内存量线性相关（2 GiB 规格约 371 ms/个）。后续版本将升级为 **soft-dirty 增量模式**，仅保存自上次 checkpoint 以来被修改的脏页，空载沙箱的 pause 耗时预计将大幅降低。

**执行命令：**（脚本：[`bench_pause_resume_concurrency.py`](https://github.com/TencentCloud/CubeSandbox/blob/master/examples/snapshot-rollback-clone/bench_pause_resume_concurrency.py)）

```bash
cd examples/snapshot-rollback-clone
# 若是新开的终端，先设置环境变量（同 4.0 安装依赖一节）：
export CUBE_API_URL=http://<your-server-ip>:3000
export CUBE_TEMPLATE_ID=<your-template-id>
export CUBE_PROXY_NODE_IP=<your-cubeproxy-ip>
export CUBE_PROXY_PORT_HTTP=80

# 脚本只提供单档机制，档位在命令行控制；按需逐档调用：
python bench_pause_resume_concurrency.py -c 1  -n 5
python bench_pause_resume_concurrency.py -c 10 -n 5 --no-header
```

**Pause 测试数据：**

| 并发 | 轮数 | wall avg | wall min | wall p95 | wall max | per-pause avg |
|:----:|:----:|--------:|--------:|--------:|--------:|-------------:|
| 1    | 5    | 370.8 ms | 351.0 ms | 384.0 ms | 384.0 ms | 370.8 ms |
| 10   | 5    | 1586.0 ms | 1529.5 ms | 1679.8 ms | 1679.8 ms | **158.6 ms** |

**Resume 测试数据：**

| 并发 | 轮数 | wall avg | wall min | wall p95 | wall max | per-resume avg |
|:----:|:----:|--------:|--------:|--------:|--------:|---------------:|
| 1    | 5    | 18.9 ms | 9.5 ms | 32.8 ms | 32.8 ms | 18.9 ms |
| 10   | 5    | 26.6 ms | 19.3 ms | 39.9 ms | 39.9 ms | **2.7 ms** |

**关键结论：**
- **Resume 极快且并发扩展良好**：单次约 19 ms，10 并发时均摊仅 **2.7 ms/个**，恢复速度不受 full-copy 影响
- **Pause 是当前瓶颈**：full-copy 模式下单次约 371 ms，10 并发均摊 **158.6 ms/个**
- **soft-dirty 版本上线后**，pause 耗时预计大幅降低

---

## 附录：测试脚本索引

本文涉及的所有测试脚本均位于仓库目录：

- **[`examples/cube-bench/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/cube-bench)** — 基于 Template 的并发创建基准工具（Go）
- **[`examples/snapshot-rollback-clone/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/snapshot-rollback-clone)** — Snapshot / Rollback / Clone / Pause-Resume 相关 Python 脚本
