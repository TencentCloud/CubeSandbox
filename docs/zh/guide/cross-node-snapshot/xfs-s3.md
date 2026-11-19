# xfs-s3 后端

`backend=xfs-s3` 的沙箱和 `xfs` 一样运行在节点本地的 XFS reflink 存储上，另外会把每个**暂停包**作为自包含的检查点发布到 S3 兼容的对象存储。源节点无法承接 Resume 时，另一个节点下载检查点，在本机恢复沙箱。

运行期 I/O 始终在本地盘上。只有暂停包能跨节点：xfs-s3 沙箱的模板和用户快照仍是普通 `xfs`，只能在有副本的节点上恢复。调度和 CPU、kernel 兼容条件见 [跨机迁移](./requirements.md)；检查点格式、校验、Resume pin 和谱系清扫等设计见 [xfs-s3 存储架构](../../architecture/xfs-s3.md)。

| | `xfs` | `xfs-s3` | `jfs` | `s3` |
|---|---|---|---|---|
| 运行期 rootfs I/O | 本地 XFS | 本地 XFS | JuiceFS | CubeS3lvol 块设备 |
| 可以跨节点的产物 | 无 | 暂停包 | 暂停包、快照 | 暂停包、快照 |
| 每个节点额外的服务 | — | 无（只需对象存储） | JuiceFS 挂载 | s3lvol target 和 WAL 镜像 |

## 暂停包如何跨节点

1. **暂停（源节点）。** 冻结 guest 之前，Cubelet 先检查本节点的对象存储可用，并且沙箱没有使用检查点带不走的资源（见 [使用限制](#使用限制)）。冻结之后，Cubelet 封存暂停包、记录导出任务，然后返回。Pause 不等待上传完成。
2. **导出（源节点）。** 上传在后台进行，Cubelet 重启后会继续。检查点最后写入固定的 `candidate.json` 才算提交，因此读取方要么看到完整的检查点，要么看不到。
3. **校验（其他节点）。** Master 请源节点以外的一个节点为检查点作证。`verify = full` 时，该节点下载全部分块、校验 SHA-256，并在隔离的网络命名空间里试恢复（`cube-runtime verify-restore`）。`verify = existence` 时，只检查每个分块都已存储且大小正确。校验通过后，暂停记录变为 `remote_status = ready`，Master 记下校验过的清单摘要。
4. **恢复（目标节点）。** 源节点离线或被隔离时，Master 选择一个兼容节点，并把这次恢复固定（pin）到该节点。目标节点下载固定的检查点，把磁盘导入本机 reflink 存储，然后走普通的 xfs Resume。之后检查点从对象存储中删除。

单节点集群永远到不了 `ready`：没有其他节点可以校验。在源节点上恢复与 `xfs` 相同。

开启 `incremental = on` 后，每个节点会为沙箱保留一份 reflink **基准**，即它最近一次导出或导入的检查点。下一次导出只上传变化的分块；目标节点上有基准时，导入也只下载缺少的分块。

## 部署要求

- 每个节点的 `/data/cubelet` 位于支持 reflink 的 XFS 上，与 `xfs` 后端相同。
- 所有节点都能访问同一个 S3 兼容的桶。对象存储应当支持 PUT 的 `If-None-Match: *`；不支持时设置 `conditional_writes = optional`（见下文）。
- 至少有两个 CPU 和 kernel 兼容的节点，源节点以外才有节点可以校验。
- 所有 Cubelet 和 CubeMaster 都运行支持 xfs-s3 的版本，并且已执行数据库迁移 `20261116120000_pause_resume_attempt`（MySQL 和 PostgreSQL）；CubeMaster 启动时会和其他迁移一起执行它。
- 所有节点读取**相同的**存储配置：任何节点都可能被要求校验或恢复其他节点写入的检查点。

## 配置对象存储

Cubelet 按以下顺序查找配置文件，使用第一个：

1. 环境变量 `CUBE_XFSS3_CONFIG` 指定的文件；
2. 环境变量 `RCOW_S3_CFG` 指定的文件；
3. `/data/cubelet/s3.cfg`，即 one-click 为 CubeS3lvol 写入的文件。

文件格式为每行一个 `key = value`。xfs-s3 读取 CubeS3lvol 的存储配置项，并增加自己的配置项；检查点使用独立的前缀，两种后端可以共用一个桶。

```ini
# 对象存储（与 CubeS3lvol 共用）
endpoint = s3.example.com
region = us-east-1
buckets = ["cube-s3lvol"]
access_key_id = <id>
secret_access_key = <secret>
path_style = true

# xfs-s3
checkpoint_prefix = cube-xfs-s3
conditional_writes = required
chunk_profile = remote
incremental = on
verify = full
import_verify = full
max_bandwidth_mbps = 0
```

| 配置项 | 默认值 | 含义 |
|---|---|---|
| `checkpoint_prefix` | `cube-xfs-s3` | 检查点对象在桶内的前缀。 |
| `conditional_writes` | `required` | `required`：拒绝不支持 `If-None-Match` 的对象存储，此时 xfs-s3 暂停会在冻结前被拒绝。`optional`：在这类存储上用 HEAD、PUT 和逐字节回读提交。这不是互斥，依赖的前提是只有源节点导出暂停包。 |
| `chunk_profile` | `remote` | 默认分块大小。`remote`（内存 4 MiB、rootfs 16 MiB）适合每个请求耗时几十毫秒的对象存储；`low_latency`（1 MiB / 4 MiB）适合部署在节点本机或近旁的存储，较小的分块让增量检查点更小。 |
| `memory_chunk_mib`、`rootfs_chunk_mib` | 取自 profile | 1–256，覆盖 profile 的值。不同大小的分块不能共用，修改大小后每个沙箱的下一个检查点都会全量上传。 |
| `export_workers`、`read_workers` | `32` | 1–64。导出、回读或导入时同时传输的分块数，每个并发占用一个分块的内存。单个节点可以用 `CUBE_XFSS3_EXPORT_WORKERS` / `CUBE_XFSS3_READ_WORKERS` 覆盖。 |
| `incremental` | `off` | 复用沙箱上一个检查点和模板内存中未变化的分块。 |
| `verify` | `full` | `full`：回读全部分块并试恢复。`existence`：只检查分块存在且大小正确；对象存储中的数据损坏或目标节点不兼容要到 Resume 时才会暴露，Resume 仍会校验每个分块的摘要。 |
| `import_verify` | `full` | 导入时如何校验取自本地基准的分块。`full`：读出并计算哈希。`fiemap`：如果克隆仍与基准共享块，且基准自保存以来没有移动，就直接采信；更快，但发现不了原地的介质损坏。 |
| `max_bandwidth_mbps` | `0`（不限） | 限制单个节点的检查点流量，上传和下载合计。可用来让每个节点都低于按账号配额分到的份额。 |
| `sweep_requests_per_second` | `30` | 1–1000。后台删除被新检查点取代的分块时的请求速率（仅增量模式）。 |

被对象存储限流的请求（429/503）最多重试 10 次，退避间隔从 1 秒到 30 秒，并遵守 1 分钟以内的 `Retry-After`。每个被限流的请求，Cubelet 都会打一条 WARN 日志。

## 创建模板

```bash
cubemastercli tpl create-from-image \
  --image <img> \
  --writable-layer-size 4Gi \
  --backend xfs-s3 \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

由该模板创建的沙箱继承 `xfs-s3`，它们的用户快照按 `xfs` 提交。

## 使用限制

检查点只携带沙箱自己的磁盘。CubeMaster 拒绝创建带 host-mount、`host_dir` 或 Plugin Volume 的 xfs-s3 沙箱。沙箱有以下情况时，Cubelet 在冻结 guest 之前拒绝暂停：

- host-mount 或宿主机路径、`host_dir`，或目录形式的 `sandbox_path`；
- Plugin Volume；
- 多于一个基于磁盘的 `empty_dir`；
- 没有设置 `discard_writes` 的 pmem。

暂停被拒绝时，沙箱继续运行。

## 运维

```bash
# 暂停包的后端和远端状态
cubemastercli cubebox list --all
cubemastercli cubebox snapshot info --snapshot-id <snapshot-id>
```

- 暂时无法校验的检查点保持 pending。CubeMaster 会把校验节点给出的原因记录为 `remote_status xfs-s3 <snapshot-id>: ... retry(<stage>)`，例如 CPU 型号不同的节点报 `retry(compat)`，然后换一个节点。只有 30 秒后再次读回仍然损坏的数据、任何节点都无法恢复的配置，或者两个不同节点都恢复失败时，检查点才会失败。
- **恢复固定（resume pin）。** 一次跨节点 Resume 的结果未知时，该沙箱的后续 Resume 都发往同一个节点，因此丢失的应答不会让沙箱启动两次。如果该节点永久下线，先确认它不可能再运行这个沙箱（已关机，或其 Cubelet 和虚拟机都已停止），再释放固定：

  ```bash
  cubemastercli cubebox release-resume-pin <sandbox-id>
  ```

  节点仍有应答时，Master 会拒绝。也可以调用 `POST /cube/sandbox/resume-pin/release`。
- **清理。** 跨节点 Resume 之后，以及暂停包被清理时，检查点会从对象存储中删除；删除失败每五分钟重试一次。Master 每两分钟清理一次不再被任何沙箱引用的 xfs-s3 暂停包，宽限期十分钟。

## 已知限制

- 删除沙箱不会删除它在对象存储中的增量谱系（`<checkpoint_prefix>/lineage/<sandbox-id>/`）和各节点上的基准（各节点 XFS 存储中的 `objects/checkpoint-base/<sandbox-id>/`）；删除模板也不会删除它的内存基准（`<checkpoint_prefix>/template/<template-id>/`）。需要回收空间时请手工删除。
- 同一个沙箱不能在支持和不支持 xfs-s3 的 Cubelet 之间迁移。
- `chunk_profile` 需要与对象存储匹配；远端对象存储改用 `low_latency` 之前请先实测。
