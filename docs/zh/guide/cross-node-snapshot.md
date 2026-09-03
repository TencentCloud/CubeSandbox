# 跨机快照（Pause / Resume / Snapshot）

::: tip 部署范围
CubeS3lvol 在 one-click 和 Kubernetes 上都**默认关闭**。开启方式见 [§2 配置后端 S3 服务](#2-如何配置后端-s3-服务)；资源与存储规划见 [§3 每台节点的存储需求](#3-每台节点的存储需求重点是-wal)。
:::

CubeSandbox 通过一份可持久化的「包对象」（rootfs / memory / metadata）实现沙箱的
**暂停（Pause）**、**恢复（Resume）** 与 **快照（Snapshot）**：

- **Pause / Resume**：把运行中沙箱的完整状态（内存 + 文件系统）冻结为一份暂停包，之后可原地或异地恢复。
- **Snapshot**：把状态持久化为一份可复用的镜像，既可用于从快照新建沙箱（FromSnap），也可用于回滚（Rollback）。

在默认的 `xfs` 后端下，这份包对象只落在**创建它的那一个节点**的本地磁盘上，因此
Resume / FromSnap **必须回到同一个节点**（节点亲和）。一旦该节点下线、被隔离或资源不足，
已暂停的沙箱无法被调度恢复，快照也无法在别处拉起。

引入 `s3` 后端后，包对象被上传到**集群共享的 S3**（由 [CubeS3lvol](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md) 服务管理），
**任意兼容节点都能按需拉取并恢复**，从而实现跨机的 Pause / Resume 与 FromSnap。
注意：XFS 根盘本身始终留在本地，跨机迁移的是「快照 / 暂停包」，而不是整个虚拟机磁盘。

SDK 侧的快照、回滚、克隆用法见 [快照、回滚与克隆](./snapshot-rollback-clone.md)；本文说明跨机恢复的条件、调度规则与 CLI 字段。

---

## 1. 跨机的条件

跨机恢复（Resume / FromSnap 落到非源节点）不是默认行为，必须同时满足以下条件。

### 1.1 必须基于 S3 后端（制作模板时指定）

跨机操作的前提是**沙箱本身运行在 S3 后端之上**。目前 S3 后端**不可在事后切换**，
必须在「制作模板」阶段就显式声明；一旦确定，该模板及其后续所有派生产物
（暂停包、快照、从快照创建的子沙箱）都会**继承并锁定为 S3 后端，不可修改**。

- 只有 `backend=s3` 的模板 / 沙箱，其 Pause / Snapshot 才会把包对象上传到共享 S3，
  从而获得跨机能力；`xfs` 模板天然无法跨机。
- 派生链路：`模板(s3)` → `沙箱(s3)` → `暂停包(s3)` / `快照(s3)` → `从快照新建的沙箱(s3)`，
  整条链路后端随模板锁定，无法中途改成 `xfs`，也无法把 `xfs` 产物改成 `s3`。

> 后续会提供 **xfs ↔ S3 转换工具**，用于在已存在的 xfs 模板 / 沙箱与 S3 之间迁移；
> 当前版本请务必在模板创建阶段就选好后端。

制作模板时指定 S3 后端：

```bash
# 省略 --backend 则沿用历史 xfs 路径
cubemastercli tpl create-from-image \
  --image <img> \
  --writable-layer-size 4Gi \
  --backend s3 \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

创建后可用 `cubemastercli cubebox template list` 确认 `BACKEND` 列显示为 `s3`，其下新建的沙箱与
快照也会自动继承 `s3`（见 [CLI 字段](#4-cubemastercli-跨机相关的子命令与新显示字段)）。

### 1.2 本机优先调度，本机无法调度才跨机

调度器（`restoreplace`）**优先把恢复任务放回源节点**。只有源节点无法被调度时，
才会在条件满足的前提下跨机：

```
┌─────────────────┐   ┌──────────────────┐  是  ┌──────────────────┐
│Resume/FromSnap  │──▶│ 源节点可调度?     │─────▶│ 本机恢复(源节点) │
└─────────────────┘   └────────┬─────────┘      └──────────────────┘
                               │ 否
                               ▼
                      ┌──────────────────┐  是  ┌──────────────────┐
                      │ CanCrossNode?    │─────▶│ 跨机恢复         │
                      │ backend=s3 ∧    │      │ (任意兼容节点)   │
                      │ remote=ready ∧  │      └──────────────────┘
                      │ kernel/cpu 一致 │  否
                      └────────┬─────────┘
                               ▼
                      ┌──────────────────┐
                      │ 报错: cannot     │
                      │ restore cross-   │
                      │ node             │
                      └──────────────────┘
```

即：源节点在、且能调度 → 永远本机；源节点不在或不可调度，且快照满足跨机条件 → 跨机；
否则直接报错，不会盲目落到不兼容的节点。

源节点被 [隔离](./node-operations.md) 时视为不可调度，因此隔离是验证跨机 Resume 的常用手段。
带 **host-mount** 的沙箱会钉在源节点（`PinToOrigin`），即使 `remote_status=ready` 也不会跨机。

### 1.3 快照必须在云端「就绪」

跨机的硬性门槛由 Master 依据 DB 持久化状态强制（非客户端输入）：

> `CanCrossNode(backend, remote_status)` 仅在 **`backend == s3` 且 `remote_status == ready`** 时返回 `true`。

- `backend` 必须为 `s3`（对象在共享 S3 上，别的节点才拉得到）。
- `remote_status` 必须为 `ready`：表示 Pause / Commit / AppSnapshot 已把
  rootfs / memory / metadata 三份对象成功导出并同步完成。状态机为
  `pending → inprogress → ready / failed`。**只有 `ready` 才允许跨机**。

> **未就绪也能用，只是不能跨机**：当 `remote_status` 不为 `ready`（包括 `pending` / `inprogress` /
> `failed`，以及 `xfs` 后端的空值）时，用户**仍然可以 resume，或基于该快照创建新沙箱**，
> 但 `CanCrossNode` 返回 `false`，调度器只会把它放回**源节点（本机）**执行，不会落到别的机器。
> 只有在 `remote_status` 变为 `ready` 后，才解锁跨机恢复。

`xfs` 后端的 `remote_status` 始终为空，因此 `CanCrossNode` 对其恒为 `false`——**xfs 快照只能本机恢复**。

### 1.4 跨机目标必须与源机 kernel / CPU 信息一致

跨机恢复的目标节点，其 **kernel 与 CPU 信息必须与源节点一致**，否则内存态（含 CPU 寄存器 /
特性位）无法正确还原，恢复会失败或不稳定。

> **当前匹配范围**：跨机兼容性判定**目前仅以 `cpuid_hash` 与 `host_kernel_release` 两个维度做相等匹配**。
> 其余字段（`cpu_vendor`、`host_kernel_fingerprint`、`kvm_api_version`）目前只是采集并展示、
> **尚未纳入相等匹配门禁**。目标节点若带非空的 `kvm_module_taint`（强制 / 树外 / 未签名的 `kvm.ko`），
> 会被拒绝作为跨机目标。后续版本可能收紧匹配维度，请以实际版本为准。

用 `cubeopscli node list --json` 查看节点的 `HostFacts`：

| JSON 字段 | 含义 | 当前是否参与匹配 |
|-----------|------|------------------|
| `cpuid_hash` | CPU 特征哈希（CPU 特性指纹） | 是（相等匹配） |
| `host_kernel_release` | 宿主机内核版本（`uname -r`） | 是（相等匹配） |
| `host_kernel_fingerprint` | 宿主机内核指纹哈希（release + 规范化 cmdline） | 暂仅展示，后续可能加入 |
| `cpu_vendor` | CPU 厂商（如 Intel / AMD / 鲲鹏） | 暂仅展示，后续可能加入 |
| `kvm_api_version` | KVM API 版本 | 暂仅展示，后续可能加入 |
| `kvm_module_taint` | KVM 模块 taint；空表示干净，非空表示加载了强制 / 树外 `kvm.ko` | 目标侧非空则拒绝跨机 |

> 因为多数展示字段当前未自动做相等校验，跨机前仍建议用 `cubeopscli node list --json`
> **人工比对全部 HostFacts**，确认目标节点与源节点一致。

#### 1.4.1 `cpuid_hash` 是如何计算的

`cpuid_hash` 由 Cubelet 读取节点的 `/proc/cpuinfo`，
将 CPU 身份与特性集经确定性 SHA-256 摘要得到（前缀 `sha256:`），
两台机器只有「身份 + 特性」完全一致时摘要才会相等。参与计算的字段如下：

- **x86**：`vendor_id`（厂商）、`cpu family`（家族）、`model`（型号）、`stepping`（步进）、`flags`（特性标志位，如 `vmx`/`avx2`/`smep`/`nx` 等）
- **ARM**：`CPU implementer`（实现者）、`CPU architecture`（架构）、`CPU variant`（变体）、`CPU part`（部件号）、`CPU revision`（修订）、`Features`（特性列表）

> 只取第一颗逻辑 CPU，默认整机同构；`flags` / `Features` 会先做字母序排序再哈希，
> 因此内核导出顺序不同不影响结果。混合架构（big.LITTLE、Intel P+E 核心）可能误判为兼容。

---

## 2. 如何配置后端 S3 服务

Cube 安装时默认安装 MinIO 作为 S3 服务，方便开箱体验。
若要接入自己的 S3，按 [CubeS3lvol 文档](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md) 配置即可。

### 2.1 Kubernetes（Helm）

设置 `cubeS3lvol.enabled=true` 即可，无需其他配置：sidecar 复用 chart MinIO 或 `volumeS3` 的 endpoint 与凭证，写入自己的 `cube-s3lvol` 桶——始终与 S3 volume 插件的 `cube-volumes` 桶分开。cubelet 的 entrypoint 会把 `[cow.s3] enable` 写成 `true`，并把 `socket_path` 指到共享 emptyDir socket；只写 socket、不写 `enable` 不会开启。开启会**重建该节点的 Big Pod 并中断其上正在运行的沙箱**，请在维护窗口操作。

身份来自完整的 Kubernetes 节点名，哈希成 `rcow-<8hex>`。Pod 重建仍是同一台机器。`cubeS3lvol.lvsName` 会给集群里每个节点钉同一个名字——多节点不要设。

只有以下情况需要额外设置：

- 外部 S3 端点为 path-style——显式设 `cubeS3lvol.s3.pathStyle: true`；
- CPU 核做了隔离——设置 `cubeS3lvol.cpuMask`（默认 rcow 的 `0x3`）。

```yaml
cubeS3lvol:
  enabled: true
  # 可选：覆盖 endpoint / 密钥。桶必须与 cube-volumes 分开。
  # s3:
  #   endpoint: https://s3.example.com
  #   accessKeyId: ...
  #   secretAccessKey: ...
  #   bucket: cube-s3lvol
```

首次启动时若 WAL 文件不存在会创建稀疏文件；`journalMB` / `walMB` / `cacheMB` 只在该文件出现之前生效。开启 sidecar 后，Big Pod 的 `terminationGracePeriodSeconds` 会提高到至少 180s，以便 `rcow_stop` 完成 disconnect 与 unload。

---

## 3. 每台节点的存储需求（重点是 WAL）

每台运行 s3lvol 的节点需预留约 **2 核 CPU + 18 GiB 内存**；x86_64 节点需要 AVX2（Haswell）。

快照对象存放在共享 S3 上，但**每台运行 s3lvol 的节点还需要一块本地 WAL 镜像盘**。
跨机恢复依赖它：对快照的写入会先落到本地盘，再异步刷写回 S3；没有这块盘的节点既无法制作快照，
也无法恢复快照。

### 3.1 WAL 镜像盘

- 路径：`/data/cubelet/rcow/wal_bdev.img`
- 逻辑大小：默认 **512 GiB**，由 one-click `install.sh` 或 Helm sidecar 入口在首次启动时以**稀疏文件**方式创建
- 只创建一次：journal / WAL / cache 三段的划分在创建时就固定，之后无法调整（只能重新创建镜像）

默认 512 GiB 由三段组成：

| 区域 | 默认大小 | 作用 |
|------|---------|------|
| Journal | 1024 MiB | 在途写操作记录，挂载 lvstore 时回放 |
| WAL | 32768 MiB（32 GiB） | 刷写 S3 之前暂存的本地写数据 |
| Chunk cache | 490496 MiB（≈479 GiB） | 最近写入 chunk 的本地缓存 |

> 镜像是**稀疏文件**：预留 512 GiB 逻辑空间并不会立即占用 512 GiB 物理磁盘。
> 物理占用按实际写入增长，容量规划应围绕写入工作集，而非逻辑大小。

### 3.2 集群规划

- **每台可能参与跨机恢复的节点都要在本地磁盘上准备自己的 WAL 镜像**——包括运行沙箱的计算节点，
  以及运行 s3lvol 的控制节点。
- 三段大小在安装时通过 `RCOW_JOURNAL_MB` / `RCOW_WAL_MB` / `RCOW_CACHE_MB`
  （一键安装）、chart 的 `cubeS3lvol.journalMB` / `walMB` / `cacheMB`（Helm）
  或 CubeS3lvol 运行时环境配置设置。调整只在**首次启动前**有意义——镜像一旦创建，布局即固定。
- 镜像不长期保存快照数据：它只是写缓冲加缓存，持久副本在 S3。

---

## 4. cubemastercli 跨机相关的子命令与新显示字段

为支持跨机能力，`cubemastercli` 在多个子命令中新增了 `backend` / `remote_status` /
`origin_node` 等显示列，并在模板创建时提供 `--backend` 标志。下面按子命令说明。

节点列表与隔离已迁到 `cubeopscli`（CubeOps，默认端口 `3010`），见 [节点相关操作](./node-operations.md) 与 [命令行工具](./cli-tools.md)。

### 4.1 `cubebox list`（沙箱列表）

沙箱列表新增两列，用于一眼看出某个沙箱是否走 S3、以及其暂停包在云端的同步状态：

| 列 | 含义 |
|----|------|
| `backend` | 该沙箱关联的 CoW 后端（`xfs` / `s3`）；`xfs` 显示为 `-` |
| `remote` | 暂停包的云端同步状态 `remote_status`（`pending` / `inprogress` / `ready` / `failed`）；非 S3 显示为 `-` |

```bash
cubemastercli cubebox list --all
```

非 paused 行按创建时间倒序；paused 行排在最后，并带 `pause_snap`。Resume 成功后这两列恢复为 `-`。

### 4.2 `cubebox snapshot list` / `snapshot info`（快照）

快照资源中的跨机相关字段：

| 字段 | 含义 |
|------|------|
| `backend` | CoW 后端（`xfs` / `s3`）；打印时优先用 `backend`，为空回退历史字段 `storage_backend` |
| `remote_status` | S3 同步状态；`xfs` 为空 |
| `origin_node_id` / `origin_node_ip` | **创建该快照的源节点**（跨机恢复时的「本机」参照） |
| `replicas` 表（`NODE_ID` / `NODE_IP` / `STATUS` / `PHASE` / `SPEC` / `ERROR`） | 每个节点副本的状态，用于观察快照在各节点的就绪情况 |

```bash
cubemastercli cubebox snapshot list
cubemastercli cubebox snapshot info --snapshot-id <snapshot-id>
```

### 4.3 `cubebox template list` / `template info`（模板）

模板列表新增 `BACKEND` 列；`template info` 会打印 `backend: <xfs|s3>`。
模板的 `backend` 决定其下沙箱与快照默认使用的 CoW 后端。

```bash
cubemastercli cubebox template list
cubemastercli cubebox template info <template-id>
```

### 4.4 `tpl create-from-image --backend xfs|s3`

```bash
# 创建模板时声明后端；省略则沿用历史 xfs 路径
cubemastercli tpl create-from-image \
  --image <img> \
  --writable-layer-size 4Gi \
  --backend s3
```

> 后端在**模板 / 沙箱创建**时确定；快照创建命令本身**不接受** backend 选择，
> 永远使用沙箱 / 模板已持久化的后端。

### 4.5 `cubeopscli node list`（校验跨机兼容性）

默认表格显示节点健康与隔离状态。HostFacts 在 JSON 里：

```bash
cubeopscli --address 127.0.0.1 --port 3010 node list
cubeopscli --address 127.0.0.1 --port 3010 node list --json
```

`--json` 中每个节点的 `HostFacts` 字段含义见 [1.4 跨机目标必须与源机 kernel / CPU 信息一致](#14-跨机目标必须与源机-kernel--cpu-信息一致)。
跨机前请确认目标节点与源节点的 `cpuid_hash` / `host_kernel_release` 一致，并核对其余 HostFacts。

---

## 5. 基准性能测试

单位 **ms**。表中 **avg** / **p95** 是**单实例**从发起到进入 `running` 的耗时，不是整批 wall 再除以并发。

下列数字测于 2026-08-25。结果随硬件、镜像和脏页负载变化，只适合作为同集群上 xfs 与 s3 的对照，不是 SLA。

### 5.1 测试环境

两台同规格腾讯云 CVM（嵌套 KVM）：一台控制面+计算，一台仅计算。

| 项 | 值 |
|----|-----|
| OS | TencentOS Server 4.4 |
| 内核 | `6.6.69-opencloudos9.cubesandbox.pvm.host` |
| CPU | AMD EPYC 9K65，16 vCPU，1 thread/core |
| 内存 | 30 GiB |
| 数据盘 | 约 1 TB virtio，`/data` 为 XFS |

### 5.2 模版

两个后端用**同一镜像、同一规格**。每份模版只在**一台**计算节点有副本（源节点）。同机用例隔离对端，让任务落在源节点；跨机 FromSnap 隔离源节点。

| 项 | 值 |
|----|-----|
| 镜像 | `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` |
| vCPU / 内存 | 2000 millicores（2 vCPU）/ 2048 MiB |
| 可写层 | 4Gi |
| Probe | 端口 `49983`，路径 `/health` |
| 后端 | `xfs` 与 `s3`，`tpl create-from-image --backend …` |

### 5.3 测试方法

复测时沿用此方法，不要改表格列或「一轮」语义。

1. **每轮清理：** 按并发数一次性拉起沙箱，**全部杀掉后再开下一轮**。上一轮还活着时不要叠下一轮。
2. **冷启动、从快照创建：** 每个 `(后端, 并发)` 格子累计 50 次启动。并发 1 = 50 轮各 1 个；并发 5 = 10 轮各 5 个。测前丢弃 1 轮 warmup。
3. **制作快照：** 串行 10 次（建沙箱 → `create_snapshot` → 杀掉）。S3 **同一时刻只能有一个 export**。
4. **快照共享（仅 S3）：** `create_snapshot` 返回后轮询到 `remote_status=ready`，这段等待计入共享，**不计入**「制作快照」。
5. **从快照创建（S3 本地）：** 隔离对端，源节点仍有副本。**S3 跨机：** 等快照 `ready` 后隔离源节点，在对端创建。
6. XFS 没有共享步骤，也不能跨机恢复。

### 5.4 冷启动（Cold Start）

从**模版**创建（`Sandbox.create(template=tpl-…)`）。

| 并发 | xfs avg | xfs p95 | s3 avg | s3 p95 |
|------|---------|---------|--------|--------|
| 1    | 50.9    | 57.2    | 430.6  | 471.4  |
| 5    | 59.5    | 81.2    | 747.4  | 885.8  |

### 5.5 快照 / 暂停 / 恢复（Snapshot / Pause / Resume）

| 操作 | xfs avg | xfs p95 | s3 本地 avg | s3 本地 p95 | s3 跨机 avg | s3 跨机 p95 |
|------|---------|---------|-------------|-------------|-------------|-------------|
| 制作快照 | 105.9 | 129.8 | 2314.9 | 2524.9 | N/A | N/A |
| 快照共享（推送至共享 S3，xfs 无此步骤） | N/A | N/A | 5579.0 | 5740.4 | N/A | N/A |
| 快照创建（1 并发） | 64.5 | 74.3 | 439.0 | 473.3 | 6495.5 | 7322.8 |
| 快照创建（5 并发） | 80.3 | 94.7 | 732.7 | 902.8 | 12285.1 | 14703.1 |

---

## 6. 已知问题

1. **S3 快照对象由 S3lvol 异步删除，被引用的快照会拒绝删除。** 删除 S3 快照后，CubeS3lvol 在后台回收对象。
   删除接口返回时，对象不一定已经从 S3 上消失。

   此外，快照在被引用时**无法删除**，CubeS3lvol 会以 `EBUSY` 拒绝：正在或已经导出（其他节点可能正
   在读取）、存在多个 clone、正在 decouple。此时 CubeS3lvol 会记录一条 **pending-delete 标记**
   （按 lvstore uuid + lvol uuid 记录，不按名字，避免同名快照被误删），可通过 `rcow_get_lvstores` 的
   `delete_pending` 字段查看，`deletable` 字段则表示当前是否可删。

   注意另一种拒绝：卷仍作为 NVMe-oF 命名空间处于 **active** 时，删除会在 RPC 层就被拒绝（提示先执行
   `rcow_deactive_bdev`），这一路径**不会**记录标记——它是调用方自己可以立即纠正的前置条件，而不是需要
   等待的阻塞原因。

   阻塞原因解除后（导出被释放或过期、多余 clone 被删除、decouple 结束、卷被 deactivate），需要在该节点上
   **手动重试**：

   ```sh
   # 在 CubeS3lvol 目录下
   test/tools/s3lvol_rpc.py --ls              # 查看 DEL / PEND 两列
   test/tools/s3lvol_rpc.py --retry-pending   # 重试所有已标记且当前可删的快照
   ```

   注意该机制的边界：标记**只存在于 s3lvol_tgt 进程内存中**，进程重启或 lvstore 卸载即丢失；**没有自动
   重试**（无后台轮询），也**无法取消**已记录的标记；并且集群侧的删除路径（Cubelet `S3Cow.DeleteByKind`）
   目前会把被拒绝的快照删除视为成功、且不会调用 `--retry-pending`，因此这类残留对象需要按上述方式在节点上
   处理。详见 `CubeS3lvol/README.md` 的 "Retrying a refused snapshot delete"。

2. **DB / FS 结构相较 0.7.0 之前版本变化较大，老数据适配仅覆盖 0.6.0**：本版本相比 0.7.0 之前的版本，
   DB 表结构与文件系统目录结构均有较大调整。新版本会对老版本的数据结构做适配，用于用户清理老数据的场景，
   但适配测试目前**只覆盖到 0.6.0 版本**。若遇到未被覆盖、适配失败的环境，需要用户**手动清理老的
   snapshot 文件与对应的 DB 数据**。

3. **跨机 Resume 或从快照创建的沙箱，短时间内无法再次 Pause 或制作快照。** 需要等待的时间取决于 snapshot 文件大小。下个版本会修复。

---

## 7. 参考

- [快照、回滚与克隆](./snapshot-rollback-clone.md)
- [沙箱生命周期](./lifecycle.md)
- [从 OCI 镜像制作模板](./tutorials/template-from-image.md)
- [节点相关操作](./node-operations.md)
- [CubeS3lvol README](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md)
