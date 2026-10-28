# CubeS3lvol 服务配置

`backend=s3` 通过 CubeS3lvol 将暂停包和快照发布到共享 S3。本文保留原有的部署、WAL 规划、性能基准和排障说明。跨机调度与 CPU / kernel 条件见[跨机恢复条件](./requirements.md)。

::: tip 部署范围
CubeS3lvol 在 one-click 和 Kubernetes 上都**默认关闭**，需要显式启用。
:::

## 配置后端 S3 服务

Cube 安装时默认安装 MinIO 作为 S3 服务，方便开箱体验。
若要接入自己的 S3，按 [CubeS3lvol 文档](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md) 配置即可。

### one-click

在安装包的 `.env` 中启用服务；使用内置 MinIO 时可直接复用已有凭证：

```bash
ONE_CLICK_ENABLE_S3LVOL=1
CUBE_S3LVOL_BUCKET=cube-s3lvol
```

外部对象存储还需设置 `CUBE_S3_ENDPOINT`、`CUBE_S3_ACCESS_KEY_ID`、`CUBE_S3_SECRET_ACCESS_KEY` 和 `CUBE_S3_REGION`。MinIO 等 path-style 服务设置 `CUBE_S3LVOL_PATH_STYLE=true`。安装程序会生成 `/data/cubelet/s3.cfg`、创建 WAL，并启用 `cube-sandbox-s3lvol.service`。

### Kubernetes（Helm）

设置 `cubeS3lvol.enabled=true` 即可，默认无需其他配置：sidecar 复用 chart 内置 MinIO（或已配置的 `volumeS3`）的 endpoint 与凭证，写入独立的 `cube-s3lvol` 桶——始终与 S3 volume 插件的 `cube-volumes` 桶分开。

::: warning 会重建 Big Pod
开启该开关会重建计算节点的 Big Pod。请在维护窗口操作，最好在创建任何沙箱之前开启。
:::

身份来自完整的 Kubernetes 节点名，哈希成 `rcow-<8hex>`。Pod 重建仍是同一台机器。`cubeS3lvol.lvsName` 会给集群里每个节点钉同一个名字——多节点不要设。

只有以下情况需要额外设置：

- 外部 S3 端点为 path-style——显式设 `cubeS3lvol.s3.pathStyle: true`；
- CPU 核做了隔离——设置 `cubeS3lvol.cpuMask`（默认绑本进程 `Cpus_allowed_list` 里的最后两核）。

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

## 每台节点的存储需求（重点是 WAL）

每台运行 s3lvol 的节点需预留约 **2 核 CPU + 19 GiB 内存**；其中包含默认的 1 GiB 整对象 RAM cache（`RCOW_CACHE_HOT_BUFS=1024`），设为 `0` 可只使用磁盘 cache；x86_64 节点需要 AVX2（Haswell）。

快照对象存放在共享 S3 上，但**每台运行 s3lvol 的节点还需要一块本地 WAL 镜像盘**。
跨机恢复依赖它：对快照的写入会先落到本地盘，再异步刷写回 S3；没有这块盘的节点既无法制作快照，
也无法恢复快照。

### WAL 镜像盘

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

### 集群规划

- **每台可能参与跨机恢复的节点都要在本地磁盘上准备自己的 WAL 镜像**——包括运行沙箱的计算节点，
  以及运行 s3lvol 的控制节点。
- 三段大小在安装时通过 `RCOW_JOURNAL_MB` / `RCOW_WAL_MB` / `RCOW_CACHE_MB`
  （一键安装）、chart 的 `cubeS3lvol.journalMB` / `walMB` / `cacheMB`（Helm）
  或 CubeS3lvol 运行时环境配置设置。调整只在**首次启动前**有意义——镜像一旦创建，布局即固定。
- 镜像不长期保存快照数据：它只是写缓冲加缓存，持久副本在 S3。

---

## 基准性能测试

单位 **ms**。表中 **avg** / **p95** 是**单实例**从发起到进入 `running` 的耗时，不是整批 wall 再除以并发。

下列数字测于 2026-09-21。S3 对象存储为安装默认的集群内 **MinIO**（path-style）。
结果随硬件、镜像和脏页负载变化，只适合作为同集群上 xfs 与 s3 的对照，不是 SLA。

### 测试环境

两台同规格腾讯云 CVM（嵌套 KVM）：一台控制面+计算，一台仅计算。

| 项 | 值 |
|----|-----|
| OS | TencentOS Server 4.4 |
| 内核 | `6.6.69-opencloudos9.cubesandbox.pvm.host` |
| CPU | AMD EPYC 9K65，16 vCPU，1 thread/core |
| 内存 | 30 GiB |
| 磁盘 | 系统盘、数据盘各约 1 TB，腾讯云 **SSD 云硬盘**（`CLOUD_SSD`）。`/` 为 XFS，`/data` 为 ext4。`/data/cubelet` 是系统盘 XFS 的 bind-mount（one-click 的 cubelet 数据目录）。xfs/reflink 卷目录和 s3lvol WAL（`wal_bdev.img`，见上文“WAL 镜像盘”）在这里，不在 ext4 的 `/data` 上 |
| S3 对象存储 | 集群内 MinIO 跑在**控制面+计算**节点，也是模版源节点。S3 本地格子打的是这台机上的 MinIO。跨机格子隔离源节点，恢复落在仅计算节点；dest→MinIO 仍打到源节点（isolate 只是不让它被调度） |

### 模版

两个后端用**同一镜像、同一规格**。每份模版只在**一台**计算节点有副本（源节点）。同机用例隔离对端，让任务落在源节点；跨机 FromSnap 隔离源节点。

| 项 | 值 |
|----|-----|
| 镜像 | `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/sandbox-code:latest` |
| vCPU / 内存 | 2000 millicores（2 vCPU）/ 2048 MiB |
| 可写层 | 4Gi |
| Probe | 端口 `49983`，路径 `/health` |
| 后端 | `xfs` 与 `s3`，`tpl create-from-image --backend …`。`s3` 的对象存储为 MinIO |

### 测试方法

复测时沿用此方法，不要改表格列或「一轮」语义。

1. **每轮清理：** 按并发数一次性拉起沙箱，**全部杀掉后再开下一轮**。上一轮还活着时不要叠下一轮。
2. **冷启动、从快照创建：** 每个 `(后端, 并发)` 格子累计 50 次启动。并发 1 = 50 轮各 1 个；并发 5 = 10 轮各 5 个。测前丢弃 1 轮 warmup。
3. **制作快照：** 串行 10 次（建沙箱 → `create_snapshot` → 杀掉）。S3 **同一时刻只能有一个 export**。
4. **快照共享（仅 S3）：** `create_snapshot` 返回后轮询到 `remote_status=ready`，这段等待计入共享，**不计入**「制作快照」。
5. **暂停共享（仅 S3）：** 与第 4 条相同，对 Pause 包在 `pause(wait=True)` 返回之后轮询到 `ready`。这段等待是「暂停共享」行，**不计入**暂停。表里暂停共享用的是 c=1 串行样本。
6. **从快照创建（S3 本地）：** 隔离对端，源节点仍有副本，MinIO 也在这台。**S3 跨机：** 等快照 `ready` 后隔离源节点，在仅计算节点上创建。下表的计时都在进入 `running` 时停。计时前目标节点已有该模版的 guest 基础镜像和内核旁路文件，墙钟不含第一次拉约 1Gi 制品；`running` 之后包的惰性 / 后台拉取也不计入。从未跑过该模版的节点仍要付这一次镜像拉取。
7. **从快照创建（有 cache）：** 同一份 snapshot 先在目标节点做一次不计入样本的 FromSnap（包已 import），再按第 2 条测 50 次。XFS 无此行。前面那组 50 次已经丢过一轮 warmup FromSnap，dest import 同样是热的；这两行用来确认再测 50 次结果不变。
8. **暂停 / 恢复：** 同样 50 次、c=1 / c=5、丢 1 轮 warmup。Create 后立刻 Pause，guest 无额外写脏。暂停墙钟是**该沙箱** `pause(wait=True)` 从调用到返回，**不是**在队列里等别人的 export。S3 的 c=5 轮里 Pause + 暂停共享仍是一个接一个（上一个包 `ready` 后再 `pause()` 下一个）。恢复墙钟是 `Sandbox.connect`。跨机暂停为 `N/A`：Pause 总在源节点，和 S3 本地相同。跨机恢复：源节点 Pause 并 `ready` 后隔离源节点，在对端 connect。
9. XFS 没有共享步骤，也不能跨机恢复。

### 冷启动（Cold Start）

从**模版**创建（`Sandbox.create(template=tpl-…)`）。

| 并发 | xfs avg | xfs p95 | s3 avg | s3 p95 |
|------|---------|---------|--------|--------|
| 1    | 56.1    | 64.2    | 242.9  | 270.9  |
| 5    | 68.0    | 79.6    | 663.8  | 839.6  |

### 快照 / 暂停 / 恢复（Snapshot / Pause / Resume）

| 操作 | xfs avg | xfs p95 | s3 本地 avg | s3 本地 p95 | s3 跨机 avg | s3 跨机 p95 |
|------|---------|---------|-------------|-------------|-------------|-------------|
| 制作快照 | 97.0 | 118.9 | 2242.2 | 2365.7 | N/A | N/A |
| 快照共享（推送至共享 S3，xfs 无此步骤） | N/A | N/A | 4336.4 | 6267.5 | N/A | N/A |
| 暂停（1 并发） | 106.2 | 122.7 | 2509.9 | 2709.3 | N/A | N/A |
| 暂停（5 并发） | 152.7 | 170.4 | 3415.8 | 4154.5 | N/A | N/A |
| 暂停共享（推送至共享 S3，xfs 无此步骤） | N/A | N/A | 5318.8 | 5763.4 | N/A | N/A |
| 恢复（1 并发） | 153.6 | 171.3 | 410.8 | 448.4 | 836.2 | 920.8 |
| 恢复（5 并发） | 403.3 | 474.6 | 1755.4 | 2033.7 | 3191.3 | 3837.2 |
| 快照创建（1 并发） | 63.2 | 75.4 | 253.7 | 301.0 | 729.6 | 788.5 |
| 快照创建（5 并发） | 93.4 | 118.5 | 706.9 | 887.3 | 3194.4 | 3912.3 |
| 快照创建（1 并发，有 cache） | N/A | N/A | 250.2 | 272.8 | 722.5 | 767.6 |
| 快照创建（5 并发，有 cache） | N/A | N/A | 709.6 | 910.1 | 2829.3 | 3814.5 |

跨机 **恢复** / **从快照创建** 是 **warm dest**：计时前目标节点已有该模版 guest 基础镜像（约 1 Gi）和内核旁路文件（约 50 MiB）。共享行是源端 export 到 `remote=ready`（声明约 210 MiB）；下面的 dest GET 只是发起到 `running` 之间打 MinIO 的字节，不是把整包下完。

跨机 FromSnap / Resume 的数据量（同一规格、Create 后立刻 Snapshot 或 Pause，guest **没有**额外写脏）：

| 项 | 大约多少 |
|----|----------|
| 沙箱规格 | 2 vCPU / 2048 MiB 内存，可写层 4Gi |
| 源端 export（reference 声明） | 约 210 MiB / ~230 个 object。memory 约占 205 MiB，rootfs / metadata 各约 4 MiB。这是共享在等的量，不是 dest 在 `running` 之前拉的量 |
| 目标节点实际 GET（MinIO） | 约 10–20 MiB，**从 FromSnap/Resume 发起到 `running`**，一次恢复（`dest_direct_get_bytes`）。import 是登记 + lease + activate，**不是**把上面 210 MiB 整包拉完。guest 在 `running` 之后还可能继续按页拉取（不计时），总传输更大 |
| guest 基础镜像（不计时） | 约 1 Gi 的 `.ext4` + 约 50 MiB 内核旁路文件；计时前已在目标节点，不是 S3 包 |

**有 cache** 的 FromSnap 行是 dest 已 import 过同一份 snapshot 之后再测的第二组 50 次。这个样本量下和第一组分不出差别（c=1 大约差 1%）。留下来表示空结果：dest import / dest cache 热了，墙钟并不动。快照共享 n=10 的 p95 是一个慢样本，不是很宽的分布。

---

## 已知问题

1. **S3 快照对象由 S3lvol 异步删除，被引用的快照会拒绝删除。** 删除 S3 快照后，CubeS3lvol 在后台回收对象。
   删除接口返回时，对象不一定已经从 S3 上消失。

   此外，快照在被引用时**不能立刻删除**，CubeS3lvol 会以 `EBUSY` 拒绝：export 仍在发布、有 live lease、lease miss 还不到 source grace（约 60 秒）、本地 esnap clone 仍在读、存在多个 clone、正在 decouple。此时 CubeS3lvol 会记录一条 **pending-delete 标记**
   （按 lvstore uuid + lvol uuid 记录，不按名字，避免同名快照被误删），可通过 `rcow_get_lvstores` 的
   `delete_pending` 字段查看，`deletable` 字段则表示当前是否可删（依据最近一次 lease poll，查询本身不去 HEAD）。空闲的 lease-aware export 以及 pre-lease
   export **不 pin**：删除快照会在内部释放 export。已经 idle 超过 grace 的 export，新 importer 在 source 下一次 20 秒 poll 看到 PUT 之前不受保护。

   注意另一种拒绝：卷仍作为 NVMe-oF 命名空间处于 **active** 时，删除会在 RPC 层就被拒绝（提示先执行
   `rcow_deactive_bdev`），这一路径**不会**记录标记——它是调用方自己可以立即纠正的前置条件，而不是需要
   等待的阻塞原因。

   之后约 60 秒一次的 poller 会在阻塞消失后自动完成删除，覆盖：**带 lease 的 export**（importer 停止续约、
   miss 超过 grace）、多余 clone、已经结束的 decouple、仍在 publish 的 export，以及已经记下删除意图的
   **pre-lease export**。异步 destroy 已经失败的，不会每分钟重试。

   标记会写入 `<prefix>/meta/pending-deletes.json`，下次 attach 时恢复。删除 RPC **先返回、再 PUT**；若在
   窗口内崩溃，意图会丢失，需要再发一次删除。poller 能自行完成的阻塞会返回成功，并带 **`deferred: true`**。
   live 或仍未知的 lease 仍返回 EBUSY；stale 或 pre-lease 的 export 由删除路径自己释放。
   用 `rcow_cancel_pending_delete` 撤回意图（必填 `lvol_name`，可选 `lvs_name` 消歧义），该 RPC 幂等。
   若 poller 已经提交了 destroy，cancel 只丢掉标记，快照仍可能被删掉。

   对仍在从 import decouple 的卷做快照会保留外部 parent，`rcow_create_snapshot` 此时返回
   **`decouple_cancelled: true`**。

   `--retry-pending` 仍用于失败的 destroy，以及不想等 poller 的场景：

   ```sh
   # 在 CubeS3lvol 目录下
   test/tools/s3lvol_rpc.py --ls              # 查看 DEL / PEND 两列
   test/tools/s3lvol_rpc.py --retry-pending   # 重试所有已标记且当前可删的快照
   ```

   集群侧删除路径（Cubelet `S3Cow.DeleteByKind`）目前仍把被拒绝的快照删除视为成功、且不会调用
   `--retry-pending`，因此那条路径上的残留对象仍需按上面方式在节点上处理。详见
   `CubeS3lvol/README.md` 的 "Retrying a refused snapshot delete"。

2. **DB / FS 结构相较 0.7.0 之前版本变化较大，老数据适配仅覆盖 0.6.0**：本版本相比 0.7.0 之前的版本，
   DB 表结构与文件系统目录结构均有较大调整。新版本会对老版本的数据结构做适配，用于用户清理老数据的场景，
   但适配测试目前**只覆盖到 0.6.0 版本**。若遇到未被覆盖、适配失败的环境，需要用户**手动清理老的
   snapshot 文件与对应的 DB 数据**。

3. **跨机 Resume 或从快照创建的沙箱，短时间内无法再次 Pause 或制作快照。** 需要等待的时间取决于 snapshot 文件大小。下个版本会修复。

---

## 参考

- [快照、回滚与克隆](../snapshot-rollback-clone.md)
- [沙箱生命周期](../lifecycle.md)
- [从 OCI 镜像制作模板](../tutorials/template-from-image.md)
- [节点相关操作](../node-operations.md)
- [CubeS3lvol README](https://github.com/TencentCloud/CubeSandbox/blob/master/CubeS3lvol/README.md)
