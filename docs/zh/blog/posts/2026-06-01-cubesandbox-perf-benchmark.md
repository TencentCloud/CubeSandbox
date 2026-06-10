---
title: "CubeSandbox 核心操作性能基准测试报告"
date: 2026-06-01
author: coolli
description: 本文公布 CubeSandbox 在真实裸金属节点上的性能基准测试数据，涵盖基于 Template 创建沙箱（冷启动延迟、并发扩展、单机密度）以及 Snapshot 相关操作（Snapshot 制作、基于 Snapshot 启动、Rollback、Clone）。每节附完整复现命令，读者可按步骤在自己的环境中复现测试。
featured: false
weight: 0
---

# CubeSandbox 核心操作性能基准测试报告

## 一、前言

CubeSandbox 面向 AI Agent 代码执行场景设计，极速冷启动和高并发是最关键的两项指标。本文给出在真实裸金属节点上测量的性能基准数据，分为两大部分：

- **第三章：基于 Template 创建沙箱** — 启动延迟、并发扩展能力、单机部署密度
- **第四章：Snapshot 相关操作** — Snapshot 制作、基于 Snapshot 启动沙箱、Rollback、Clone

每节均附有完整的测试命令，读者可直接照步骤在自己的环境中复现。

**重要说明：所有测试数据与测试环境、测试场景高度相关。** 影响因子包含但不限于：Host 的 CPU、内存、IO 性能，以及 Sandbox 内部负载（例如 Sandbox 中运行的程序越复杂、脏页越多，Snapshot 制作耗时也随之上升）。实际部署时请结合自身硬件和负载情况进行评估。

---

## 二、测试环境

### 2.1 硬件信息

| 项目 | 详情 |
|------|------|
| 机器类型 | 腾讯云[内存型裸金属云服务器 BMI5](https://cloud.tencent.com/document/product/386/63404)（可在腾讯云控制台购买同款） |
| OS | OpenCloudOS (TencentOS Server 4) kernel 6.6.119 x86_64 |
| CPU 型号 | Intel(R) Xeon(R) Platinum 8255C @ 2.50GHz |
| CPU 配置 | 2 Socket × 24 Core × 2 Thread = **96 逻辑核** |
| NUMA 节点 | 2（node0: 0-23,48-71 / node1: 24-47,72-95） |
| 内存总量 | **375 GiB** DDR4-2933 MT/s ECC |
| 数据盘 | `/dev/nvme0n1` 3.84 TB Intel SSDPE2KX040T8 NVMe SSD，格式化为 XFS，挂载至 `/data` |

> **BMI5**（内存型裸金属，Memory-optimized Bare Metal Instance 5）是腾讯云面向大内存、高密度部署场景推出的裸金属实例规格，提供物理机级别的隔离与性能，无虚拟化损耗，支持嵌套虚拟化，特别适合 CubeSandbox 这类需要运行大量轻量级 VM 的场景。如需复现本文测试，可前往[腾讯云裸金属购买页](https://buy.cloud.tencent.com/bm)选购同款。

> 安装 CubeSandbox 请参照[裸金属部署指南](../../guide/bare-metal-deploy.md)。

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

**执行测试（以 1 并发、50 并发为例）：**

```bash
# 设置环境变量
export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000
export CUBE_TEMPLATE_ID=<your-template-id>

# 1 并发，共 20 次，仅创建（不删除）
./bin/cube-bench -c 1 -n 20 -w 3 -m create-only

# 50 并发，共 500 次
./bin/cube-bench -c 50 -n 500 -w 3 -m create-only

# 导出 JSON 报告
./bin/cube-bench -c 50 -n 500 -w 3 -m create-only -o report_c50.json
```

> `-w 3` 表示先做 3 轮热身，热身结果不计入统计。

**测试数据（腾讯云 BMI5 裸金属，2 vCPU / 2 GiB 沙箱）：**

| 并发 | 请求数 | avg | min | p95 | max | 单沙箱均摊 | 吞吐 |
|:----:|:------:|----:|----:|----:|----:|----------:|-----:|
| 1    | 20  | 47.8 ms | 43.5 ms | 57.4 ms  | 60.4 ms  | 55.8 ms | 17.9 个/s |
| 10   | 200 | 88.7 ms | 45.8 ms | 116.9 ms | 119.1 ms | 9.9 ms  | 101.4 个/s |
| 20   | 300 | 98.1 ms | 47.7 ms | 175.8 ms | 232.6 ms | 5.5 ms  | 180.9 个/s |
| 50   | 500 | 276.1 ms | 60.6 ms | 508.4 ms | 681.3 ms | 6.8 ms  | 147.6 个/s |

> 「单沙箱均摊」= 该档总耗时（wall time）÷ 请求数；「吞吐」为对应的每秒创建沙箱数。各档均独立测试，档间清空所有沙箱并留出资源池恢复时间，避免相互干扰。所有档位成功率均为 **100%**。

**关键结论：**
- 单并发创建延迟约 **48 ms**（min 43.5 / p95 57.4），全程在百毫秒级以内
- 并发提升带来明显吞吐增益：20 并发吞吐达 **180.9 个/s**，单沙箱均摊从 1 并发的 55.8 ms 降至 5.5 ms
- 50 并发下单条创建延迟随队列加深拉长（p95 508 ms、max 681 ms），但吞吐仍维持在 147.6 个/s；在并发与单条延迟之间，20 并发是该机型的吞吐甜点

### 3.3 单机部署密度（内存开销）

CubeSandbox 通过内核共享与写时复制（CoW）将自身额外开销压缩到极低水平。本节通过「清空机器 → 分批启动沙箱 → 记录内存变化」的方式，实测单实例净开销。

> ⚠️⚠️⚠️ **重要安全提醒**
> 
> **每次启动沙箱前，务必先用 `free -h` 确认机器剩余内存充足，每次只启动少量，边启动边观察内存余量，确认安全后再继续下一批，切勿一次性启动过多！** 内存耗尽会触发 OOM Killer，轻则进程被杀，重则损坏运行环境，需重新部署。请根据自己机器的实际内存容量决定每批启动的数量。

**第一步：记录基线（空机内存）**

```bash
# 设置环境变量（与 §3.2 一致；换新终端窗口需重新 export）
export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000              # 本地部署填任意非空字符串
export CUBE_TEMPLATE_ID=<your-template-id> # cubemastercli tpl list 查到的 ID

# 确保没有残留沙箱（SANDBOX_COUNT 应为 0）
cubemastercli list

# 记录空机内存用量
free -h
# 同时记录 shim 进程数（应为 0）
ps --no-headers -C containerd-shim-cube-rs | wc -l
```

**第二步：分批启动沙箱，每批结束后用 `free -h` 记录当前已用内存**

用 `cube-bench` 的 `create-only` 模式批量创建并保持沙箱存活。根据机器内存量，累计启动到 100 / 300 / 500 / 1000 个（以下示例供参考，请按实际情况调整）：

```bash
# 设置环境变量（与 §3.2 一致；换新终端窗口需重新 export）
export E2B_API_URL=http://<your-server-ip>:3000
export E2B_API_KEY=e2b_000000              # 本地部署填任意非空字符串
export CUBE_TEMPLATE_ID=<your-template-id> # cubemastercli tpl list 查到的 ID

# 启动到 100 个
./bin/cube-bench -c 50 -n 100 -m create-only
free -h   # 记录此时已用内存

# 再启动 200 个（累计 300）
./bin/cube-bench -c 50 -n 200 -m create-only
free -h

# 再启动 200 个（累计 500）
./bin/cube-bench -c 50 -n 200 -m create-only
free -h

# 如机器内存充足，再启动 500 个（累计 1000）
./bin/cube-bench -c 50 -n 500 -m create-only
free -h
```

**第三步：计算单实例开销**

用「当前已用内存 - 基线已用内存」除以沙箱数即可：

```
单 VM 均摊开销 = (当前 used - 基线 used) ÷ VM 数量
```

**测试数据（腾讯云 BMI5，2 vCPU / 2 GiB 规格沙箱，create-only 累积存活）：**

| 存活沙箱数 | 系统可用内存（free available） | 单 VM 均摊开销 |
|:---------:|:----------------------------:|:-------------:|
| 0（基线）  | 359.5 GiB                    | — |
| 100        | 357.4 GiB                    | **~21.5 MB** |
| 300        | 352.5 GiB                    | **~23.8 MB** |
| 500        | 347.3 GiB                    | **~25.0 MB** |
| 1000       | 334.3 GiB                    | **~25.7 MB** |

> 实测 0→1000 全部创建成功、零回滚，单 VM 均摊开销稳定收敛在 ~25 MB（随密度从 21 MB 缓升至 26 MB），始终在数十 MB 量级——这正是 CoW + 内核页共享的效果：2 GiB 规格的沙箱空载时**并不预占满 2 GiB**，只在实际写入时按需分配。1000 个沙箱仅耗约 25 GiB 内存，距 375 GiB 内存上限仍有极大余量。

**调大 tap 预建池的操作步骤：**

tap 池目标数由 cubelet 配置中的 `tap_init_num` 指定（默认 **500**），但该参数实际由 **network-agent**（启动时带 `--cubelet-config` 读取同一份 cubelet 配置）消费、预建 tap 设备，因此修改后需重启 **network-agent**（而非 cubelet）使其生效。目标密度需在该值以内——例如要压测到 1000 个沙箱，需先将 `tap_init_num` 调到 1000（或更高）。

```bash
# 1. 编辑 cubelet 配置，调大 [plugins."io.cubelet.internal.v1.network"] 段下的 tap_init_num
vi /usr/local/services/cubetoolbox/Cubelet/config/config.toml
#   [plugins."io.cubelet.internal.v1.network"]
#     tap_init_num = 1000        # 默认 500；压测 1000 需调到 1000 以上

# 2. 重启 network-agent，使其按新目标重新预建 tap 池
systemctl restart cube-sandbox-network-agent.service
```

**单机可运行实例数估算（BMI5，375 GiB 内存）：**

```
满载场景（每沙箱实际写满 2 GiB）：375 GiB ÷ (2 GiB + ~25 MB) ≈ 185 个
空载/轻载场景（CoW 按需分配）：受均摊开销 ~25 MB 主导，可达数千个，实测已稳定承载 1000 个仅耗 ~25 GiB
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

**测试数据**（全新沙箱原样打快照，实测脏页约 **7 MB**，由 `/data/log/CubeVmm/vmm.log` 中 `PagemapAnon snapshot saved` 记录确认；该值为沙箱基线匿名内存页大小，本节不作为变量）：

| 并发 | 轮数 | wall avg | wall min | wall p95 | wall max | per-snapshot avg |
|:----:|:----:|--------:|--------:|--------:|--------:|----------------:|
| 1    | 5    | 49.8 ms  | 47.3 ms  | 54.1 ms  | 54.1 ms  | 49.8 ms          |
| 5    | 5    | 71.0 ms  | 62.7 ms  | 81.0 ms  | 81.0 ms  | **14.2 ms**      |
| 10   | 5    | 127.2 ms | 79.6 ms  | 155.6 ms | 155.6 ms | **12.7 ms**      |

串行单 Snapshot 约 **50 ms**；5 并发时整批 wall 约 **71 ms**，per-snapshot 均摊降至约 **14 ms**；10 并发时整批 wall 约 **127 ms**，均摊进一步降至约 **13 ms**，并发摊薄效果显著。

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
| 0 MB   | 7.1 MB    | 45.7 ms      | 43.9 ms      | 47.4 ms      | 47.4 ms      | 64.8 ms            | 61.5 ms            | 68.6 ms            | 68.6 ms            |
| 10 MB  | 38.9 MB   | 75.7 ms      | 73.2 ms      | 79.2 ms      | 79.2 ms      | 60.7 ms            | 57.3 ms            | 66.1 ms            | 66.1 ms            |
| 50 MB  | 120.7 MB  | 107.7 ms     | 104.8 ms     | 112.3 ms     | 112.3 ms     | 64.4 ms            | 60.4 ms            | 70.6 ms            | 70.6 ms            |
| 100 MB | 195.0 MB  | 138.6 ms     | 136.7 ms     | 139.9 ms     | 139.9 ms     | 66.5 ms            | 60.9 ms            | 71.1 ms            | 71.1 ms            |
| 200 MB | 296.7 MB  | 174.2 ms     | 173.1 ms     | 176.2 ms     | 176.2 ms     | 63.7 ms            | 60.6 ms            | 66.8 ms            | 66.8 ms            |
| 500 MB | 602.5 MB  | 289.4 ms     | 285.0 ms     | 293.1 ms     | 293.1 ms     | 64.0 ms            | 61.6 ms            | 66.5 ms            | 66.5 ms            |
| 800 MB | 908.4 MB  | 392.8 ms     | 392.1 ms     | 394.1 ms     | 394.1 ms     | 60.9 ms            | 54.6 ms            | 65.8 ms            | 65.8 ms            |
| 1024 MB | 1136.4 MB | 486.9 ms    | 471.5 ms     | 510.8 ms     | 510.8 ms     | 68.4 ms            | 58.9 ms            | 84.6 ms            | 84.6 ms            |

**关键结论：**
- **Snapshot 制作耗时与 Dirty Page 大小近线性相关**：基线（7 MB 脏页）约 47 ms，每增加 100 MB 脏数据约增加 40 ms，1024 MB 时约 487 ms
- **基于 Snapshot 创建新沙箱的耗时与 Dirty Page 大小无关**：恢复耗时稳定在 **60–85 ms**，因为恢复采用 CoW（写时复制）按需加载，不依赖 Dirty Page 的大小

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
python bench_create_concurrency.py -c 50 -n 3 --no-header
```

**测试数据：**

| 并发 | n 总数 | 轮数 | wall avg | wall min | wall p95 | wall max | per-sandbox avg |
|:----:|:------:|:----:|--------:|--------:|--------:|--------:|----------------:|
| 1    | 1      | 3    | 63.9 ms  | 62.5 ms  | 66.1 ms  | 66.1 ms  | 63.9 ms          |
| 10   | 10     | 3    | 89.9 ms  | 84.0 ms  | 93.6 ms  | 93.6 ms  | **9.0 ms**       |
| 20   | 20     | 3    | 118.9 ms | 92.7 ms  | 167.1 ms | 167.1 ms | **5.9 ms**       |
| 50   | 50     | 3    | 180.3 ms | 135.1 ms | 260.7 ms | 260.7 ms | **3.6 ms**       |

单沙箱启动约 **64 ms**；20 并发时 wall 约 **119 ms**，均摊仅 **5.9 ms/个**；50 并发时 wall 约 **180 ms**，均摊仅 **3.6 ms/个**，展现出极强的并发扩展能力。

### 4.4 Rollback

**测试方式：** 对运行中的沙箱调用 `POST /sandboxes/{id}/rollback`，将沙箱内存和文件系统状态原地恢复至指定 Snapshot，无需重建沙箱。

> **快照所有权约束：** CubeSandbox 只允许沙箱回滚到**自己创建**的 checkpoint。因此每个并发沙箱各自 `create_snapshot()` 打点、再 `rollback()` 到自身 checkpoint，结束后删除各自快照。

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
| 1    | 5    | 81.6 ms  | 74.7 ms  | 97.4 ms  | 97.4 ms  | 81.6 ms          |
| 5    | 5    | 189.6 ms | 161.8 ms | 243.2 ms | 243.2 ms | **37.9 ms**      |
| 10   | 5    | 266.1 ms | 236.1 ms | 305.1 ms | 305.1 ms | **26.6 ms**      |

> 单次 Rollback 流程（`create_snapshot` 打点 → `rollback` 回滚到自身 checkpoint）约 **82 ms**；5 并发时整批 wall 约 **190 ms**，per-rollback 均摊降至约 **38 ms**；10 并发时整批 wall 约 **266 ms**，均摊降至约 **27 ms/次**。
>
> 注：因 CubeSandbox 要求沙箱只能回滚到自己创建的 checkpoint，故无法复用共享快照，每个并发沙箱均需独立完成「打快照 + 回滚」全流程。

### 4.5 Clone

**测试方式：** 调用 `POST /sandboxes/{id}/clone`，从一个**运行中**的沙箱派生出 N 个新沙箱，完整保留源沙箱的内存和文件系统状态。

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
python bench_clone_concurrency.py -n 1   -c 1  --rounds 5
python bench_clone_concurrency.py -n 100 -c 10 --rounds 2 --no-header
python bench_clone_concurrency.py -n 100 -c 20 --rounds 2 --no-header
python bench_clone_concurrency.py -n 100 -c 50 --rounds 2 --no-header
```

**测试数据：**

| 场景               | n   | 并发 | 轮数 | wall avg | wall min | wall p95 | wall max | per-clone avg |
|:------------------|:---:|:----:|:----:|--------:|--------:|--------:|--------:|--------------:|
| 1 个沙箱 1 并发    | 1   | 1    | 5    | 219.6 ms | 213.6 ms | 234.7 ms  | 234.7 ms  | 219.6 ms       |
| 100 沙箱 10 并发   | 100 | 10   | 2    | 870.4 ms | 860.6 ms | 880.2 ms  | 880.2 ms  | **8.7 ms**    |
| 100 沙箱 20 并发   | 100 | 20   | 2    | 638.6 ms | 620.8 ms | 656.3 ms  | 656.3 ms  | **6.4 ms**     |
| 100 沙箱 50 并发   | 100 | 50   | 2    | 540.9 ms | 491.3 ms | 590.5 ms  | 590.5 ms  | **5.4 ms**     |

Clone（含完整内存 + 文件系统状态）单次约 **220 ms**；100 个沙箱场景下，提高并发可显著缩短整批 wall time（10 并发约 870 ms → 50 并发约 541 ms），均摊耗时从 **8.7 ms** 降至 **5.4 ms/个**。

> **关于并发档位的反直觉现象：** 10 并发的整批 wall（870 ms）反而**慢于** 20 / 50 并发（639 / 541 ms）。这是因为本测试固定 n=100，10 并发需串行排 10 批、调度轮转开销累积更多；而更高并发下批次更少、源沙箱的内存页在 Page Cache 中复用更充分，整体反而更快。说明在该规模下 Clone 并未受限于并发瓶颈，提高并发是有益的。

### 4.6 Pause / Resume

**测试方式：** 创建 `concurrency` 个沙箱，并发调用 `POST /sandboxes/{id}/pause` 暂停全部，再并发调用 `POST /sandboxes/{id}/resume` 恢复全部，分别记录 pause 和 resume 的 wall time 与均摊耗时。

> ⚠️ **当前实现说明：** Pause 当前采用 **full-memory-copy 模式**——暂停时将沙箱的全部匿名内存页写入持久化存储，耗时与沙箱总内存量线性相关（2 GiB 规格单次约 558 ms）。后续版本将升级为 **soft-dirty 增量模式**，仅保存自上次 checkpoint 以来被修改的脏页，空载沙箱的 pause 耗时预计将大幅降低至与 Snapshot 制作相当（约 **60 ms**）。

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
python bench_pause_resume_concurrency.py -c 5  -n 5 --no-header
python bench_pause_resume_concurrency.py -c 10 -n 5 --no-header
```

**Pause 测试数据：**

| 并发 | 轮数 | wall avg | wall min | wall p95 | wall max | per-pause avg |
|:----:|:----:|--------:|--------:|--------:|--------:|-------------:|
| 1    | 5    | 558.4 ms | 530.8 ms | 590.3 ms | 590.3 ms | 558.4 ms |
| 5    | 5    | 656.9 ms | 621.9 ms | 683.2 ms | 683.2 ms | **131.4 ms** |
| 10   | 5    | 682.1 ms | 674.1 ms | 699.3 ms | 699.3 ms | **68.2 ms** |

**Resume 测试数据：**

| 并发 | 轮数 | wall avg | wall min | wall p95 | wall max | per-resume avg |
|:----:|:----:|--------:|--------:|--------:|--------:|---------------:|
| 1    | 5    | 41.8 ms  | 18.7 ms  | 65.1 ms  | 65.1 ms  | 41.8 ms |
| 5    | 5    | 28.2 ms  | 17.6 ms  | 34.2 ms  | 34.2 ms  | **5.6 ms** |
| 10   | 5    | 35.7 ms  | 30.6 ms  | 41.7 ms  | 41.7 ms  | **3.6 ms** |

**关键结论：**
- **Resume 极快且并发扩展良好**：单次约 42 ms，10 并发时均摊仅 **3.6 ms/个**
- **Pause 并发扩展能力强**：full-copy 模式下 wall time 随并发提升增加有限（1 并发 558 ms → 10 并发 682 ms），per-pause 均摊从 558 ms 降至 **68 ms/个**，体现出良好的 IO 并行度
- **soft-dirty 版本上线后**，pause 耗时预计降至与 Snapshot 制作（约 60 ms）相当，10 并发 per-pause 均摊将进一步降至个位数毫秒级

> **full-copy → soft-dirty 优化说明：** 当前 full-copy 模式每次 pause 需将 VM 全部匿名内存（最多 2 GiB）写入磁盘，IO 压力大且耗时长。soft-dirty 增量模式通过 `/proc/PID/clear_refs` 跟踪自上次 checkpoint 以来的脏页，pause 时只需写入实际被修改的页面（空载沙箱通常仅数 MB），可将 pause 耗时降低 **80–90%**，同时大幅提升高并发下的吞吐能力。

---


## 附录：测试脚本索引

本文涉及的所有测试脚本均位于仓库目录：

- **[`examples/cube-bench/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/cube-bench)** — 基于 Template 的并发创建基准工具（Go）
- **[`examples/snapshot-rollback-clone/`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/snapshot-rollback-clone)** — Snapshot / Rollback / Clone / Pause-Resume 相关 Python 脚本
