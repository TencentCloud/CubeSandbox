# 软删除墓碑定时清理

CubeSandbox 在很多表上使用 GORM 的*软删除*：删除一行时只是把它的 `deleted_at` 列置位，GORM 随后在普通查询里把它隐藏起来。此前没有任何机制回收这些“墓碑”行，于是表会无限增长，过期的墓碑还会一直占用**唯一键**（即软删除 + 唯一键陷阱）。墓碑清理器（tombstone purger）就是解决这个问题的定时“看门人”。

## 它做什么

一个共享包 `CubeDB/tombstone` 同时运行在共享数据库的**两个**二进制里：

- **CubeMaster** —— 清理 `t_cube_*` 软删除表。
- **CubeOps** —— 清理 `t_agenthub_*` 软删除表。

每个组件在启动时注册清理器。它按固定间隔获取一个集群级咨询锁（保证每个 tick 内只有一个 HA 副本干活），然后对每张配置的表，硬删除 `deleted_at` 早于保留窗口的行。单次 pass 是**有上限的**（`max_per_pass`），所以大的积压会分摊到很多个 tick 里慢慢清，而不是一次长事务把锁占满。硬删除是跨方言的（由当前 dialector 负责标识符引用），并且**对复活竞态安全**——`DELETE` 会重新校验墓碑谓词，因此应用层 UPSERT 复活（`deleted_at = NULL`）的行在 select 与 delete 之间不会被硬删。

## 配置

两个组件都暴露一个 `soft_delete_purge` 配置块。所有字段都是可选的；默认值是安全的，且特性**默认关闭**（需显式开启）：清理不可逆，请显式设置 `enable: true`。

```yaml
soft_delete_purge:
  enable: false       # 默认关闭：清理不可逆，需显式开启
  dry_run: false      # 只 select 并打印计数，不真正 DELETE（用于安全的首轮观察）
  retention: 168h     # deleted_at 早于 now-retention 的行会被清理。
                      #   <=0 -> 7 天默认值；(0, 1h) 之间的值会上调到 1h。
  interval: 1h        # 两次 pass 之间的间隔。
                      #   <=0 -> 1h 默认值；(0, 1m) 之间的值会上调到 1m。
```

`max_per_pass`（5000）和批次大小 batch size（500）是包级常量，不可配置。

## 覆盖的表

只有那些*确实会被软删除、且没有专属回收机制*的表才会被清理。这份分类是逐个核对每个 `.Delete()` 调用点得出的。

**CubeMaster**（始终）：`t_cube_sandbox_spec`、`t_cube_template_replica`。
**CubeMaster**（仅默认情况——见下方优先级）：`t_cube_instance_info`、`t_cube_instance_userdata`。
**CubeOps**：`t_agenthub_instance`、`t_agenthub_snapshot`、`t_agenthub_template`。

**排除**（有专属生命周期 / 不是软删除）：

- `t_cube_rootfs_artifact`、`t_cube_artifact_node_placement` —— 有“复活”逻辑 + 专属的 `artifact_gc`。
- `t_cube_snapshot_runtime_ref` —— append-only 历史表；`deleted_at` 从不写入（它的增长是另一个独立问题）。
- 仅硬删的表（`t_cube_template_definition`、`t_cube_template_image_job`、`t_cube_volume` 等）。

迁移 `20260731120000` 会给每个缺少前导 `deleted_at` 索引的清理目标表补上该索引，使 `deleted_at < cutoff` 的扫描走索引。

## 优先级：`disable_hard_delete` 与 `soft_delete_purge`

`common.disable_hard_delete`（CubeMaster）存在的目的是让运维可以**保留**实例记录用于审计/恢复：置位后，`t_cube_instance_info` 改为软删而非硬删（`t_cube_instance_userdata` 本就总是软删）。

**对于实例记录，`disable_hard_delete` 优先于 `soft_delete_purge`。** 当 `disable_hard_delete: true` 时，`t_cube_instance_info` 与 `t_cube_instance_userdata` **豁免清理**——否则清理器会硬删掉运维明确要求保留的记录。清理表清单由 `cubeMasterPurgeTables(disableHardDelete)` 依此构建。

| `disable_hard_delete` | `soft_delete_purge` | 实例记录 |
|---|---|---|
| `false`（默认） | 开 | `userdata` 被清理（本就软删）；`instance_info` 在删除路径硬删（不产生墓碑） |
| `false` | 关 | `userdata` 墓碑累积（不清理） |
| `true` | 开 | **保留** —— 实例表豁免清理 |
| `true` | 关 | 保留 —— 反正不清理 |

CubeMaster 的其他表（`sandbox_spec`、`template_replica`）始终被清理；它们不是实例记录，没有“保留”语义。

## 运维注意事项

- **默认关闭。** 清理不可逆，需显式开启（`soft_delete_purge.enable: true`）；升级不应静默硬删此前一直保留的墓碑。开启后首次 pass 会按 `max_per_pass` 分批回收已积压的墓碑。
- **先 dry-run。** 面对大的历史积压，可先把 `dry_run: true` 设一个间隔，观察“将要清理”的计数，再开启真正删除。
- **带外 DDL。** 设置了 `CUBE_AUTO_MIGRATION=false` 的部署，必须带外应用迁移 `20260731120000` 中的 `deleted_at` 索引；否则清理会是全表扫描。
- **PostgreSQL。** 迁移中的普通 `CREATE INDEX` 在构建每个索引期间会持有 ACCESS EXCLUSIVE 锁（阻塞写入）——对当前以 PG 为非主力的较小部署可接受；较大的 PostgreSQL 部署请在低峰期或维护窗口执行迁移。此外，一次大规模回收 pass 之后，相关表会出现短暂的 autovacuum 高峰（大规模 `DELETE` 的正常现象）。
