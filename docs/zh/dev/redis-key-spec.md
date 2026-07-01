# Redis Key 命名规范

> 适用范围：CubeMaster、CubeProxy 共享的 Redis 实例；CubeAPI 未来接入时亦须遵循本规范。

## 1. 概述

CubeSandbox 多个服务（CubeMaster、CubeProxy，以及未来可能接入的 CubeAPI）共用同一 Redis 实例。为避免 key 碰撞、误删、误覆盖，以及便于 `SCAN` 治理与审计，所有 Redis key 必须遵循统一的命名空间约定。

**核心原则**：

- 禁止裸实体 ID 直接作为顶层 key。
- 所有 key 须经各服务统一的 **key 构造模块**生成，禁止散落字面量拼接。
- 跨服务共享的数据使用 `shared` 命名空间，变更须多端协同。

## 2. 命名格式

```
cube:{ver}:{scope}:{resource}[:{sub}...]:{id}
```

| 段 | 说明 | 取值 |
| --- | --- | --- |
| `cube` | 固定产品前缀，杜绝与第三方 key 碰撞 | 常量 `cube` |
| `ver` | schema 版本；value 结构发生**破坏性**变更时升版 | 当前 `v1` |
| `scope` | 归属命名空间（按 key 的归属/共享关系，而非编程语言） | 见 §3 |
| `resource[:sub]` | 业务实体，全小写，冒号分段 | 如 `node:metric`、`instance:info` |
| `id` | 实体 ID（sandboxID、nodeID、taskID 等） | 禁止省略 |

**示例**：`cube:v1:shared:sandbox:proxy:7c8fbcd45ffe450fb8f7fb223ad45507`

## 3. Scope 枚举

| scope | 含义 | 读写方 |
| --- | --- | --- |
| `master` | CubeMaster 私有数据 | 仅 CubeMaster |
| `proxy` | CubeProxy 私有数据 | 仅 CubeProxy |
| `api` | CubeAPI 私有数据（预留） | 仅 CubeAPI |
| `shared` | 跨服务共享契约 | 多个服务协同读写 |

`shared` 下的 key 是跨服务契约：新增、改名或变更数据结构时，**所有读写方必须同步变更**，不得单方面修改。

## 4. 强制规则

1. **集中构造**：CubeMaster 使用 [`CubeMaster/pkg/base/rediskey/rediskey.go`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeMaster/pkg/base/rediskey/rediskey.go)；CubeProxy 使用 [`CubeProxy/lua/redis_keys.lua`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeProxy/lua/redis_keys.lua)。新增 key 时先在这两处（或 CubeAPI 对应模块）注册构造函数，再在业务代码中调用。
2. **TTL 显式声明**：缓存类 key 必须设置 TTL；生命周期由业务显式 `DEL` 管理的 key（如沙箱路由）默认不设 TTL，仅在具备续期路径或 TTL 大于实体最大存活时间时才可启用兜底 TTL。
3. **保留语义段**（新增锁、幂等等 key 时沿用）：
   - 分布式锁：`cube:v1:{scope}:lock:{resource}:{id}`
   - 幂等键：`cube:v1:{scope}:idemp:{resource}:{id}`
4. **治理友好**：同类 key 必须可用 `SCAN cube:v1:{scope}:{resource}:*` 枚举；禁止把可变 ID 嵌入前缀中间导致无法批量匹配。
5. **版本演进**：仅当 value 的数据结构发生破坏性变化时升 `ver`；新增 Hash field、扩展字段不升版。
6. **ID 安全**：`id` 段不得包含未转义的 `:`；构造前应对实体 ID 做格式校验，防止 key 注入导致越权读取。

## 5. 已注册 Key 清单

以下为当前系统中已注册的标准 key（`v1`）。新增业务 key 须在本节补充登记。

| 业务含义 | Key 模式 | 数据结构 | scope | 写入方 | 读取方 | TTL |
| --- | --- | --- | --- | --- | --- | --- |
| 节点资源指标 | `cube:v1:master:node:metric:{nodeID}` | Hash | master | CubeMaster | CubeMaster | 刷新式（心跳续期，默认 600s） |
| 沙箱代理路由 | `cube:v1:shared:sandbox:proxy:{sandboxID}` | Hash | shared | CubeMaster | CubeProxy | 无（生命周期由 `DEL` 管理） |
| 实例信息 | `cube:v1:master:instance:info:{insID}` | Hash | master | CubeMaster | CubeMaster | 无 |
| Describe 任务结果 | `cube:v1:master:task:describe:{taskID}` | Hash | master | CubeMaster | CubeMaster / 外部 | 86400s（可配置） |
| 实例 metadata（预留） | `cube:v1:master:instance:meta:{...}` | string / list | master | CubeMaster | CubeMaster | 无 |
| 沙箱 lifecycle 注册表 | `cube:v1:shared:sandbox:lifecycle:meta` | Hash | shared | CubeMaster | cube-lifecycle-manager | 无（生命周期由 `HDEL` 管理） |
| 沙箱 lifecycle 事件流 | `cube:v1:shared:sandbox:lifecycle:events` | Stream | shared | CubeMaster | cube-lifecycle-manager | MAXLEN ~ 100000 |
| 沙箱 lifecycle 状态 | `cube:v1:shared:sandbox:lifecycle:state:{sandboxID}` | String | shared | cube-lifecycle-manager | cube-lifecycle-manager | SET TTL（默认 60s） |
| CubeProxy 副本注册表 | `cube:v1:shared:cube_proxy:registry` | Hash | shared | CubeProxy | cube-lifecycle-manager | 无（心跳超时后由 `HDEL` 清理） |
| CubeProxy 副本心跳 | `cube:v1:shared:cube_proxy:heartbeat` | Sorted Set | shared | CubeProxy | cube-lifecycle-manager | 无（`ZREMRANGEBYSCORE` 清理，默认 15s 过期） |

### 5.1 Hash 字段约定

**`node:metric`**（节点指标）

| field | 含义 |
| --- | --- |
| `ins_id` | 节点 ID |
| `update_at` | 指标更新时间 |
| `quota_cpu_usage` | CPU 配额用量（milli-cpu） |
| `quota_mem_mb_usage` | 内存配额用量（MB） |
| `mvm_num` | 虚拟机数量 |
| `nic_queues` | 网卡队列数 |
| `data_disk_usage_per` / `storage_disk_usage_per` / `sys_disk_usage_per` | 磁盘使用率（%） |

**`sandbox:proxy`**（沙箱路由，跨服务契约）

| field | 含义 |
| --- | --- |
| `HostIP` | 宿主机 IP |
| `SandboxIP` | 沙箱内网 IP（可选） |
| `CreatedAt` | 创建时间 |
| `{containerPort}` | 容器端口 → 宿主机映射端口（动态 field） |

**`instance:info`**（实例信息）

见 [`CubeMaster/pkg/base/types/redis.go`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeMaster/pkg/base/types/redis.go) 中 `InstanceInfoMap` 的 `redis` tag 定义。

**`task:describe`**（异步任务）

| field | 含义 |
| --- | --- |
| `task_id` | 任务 ID |
| `status` | 任务状态 |
| `error_code` | 错误码 |
| `error_message` | 错误信息 |

**`sandbox:lifecycle:meta`**（沙箱 lifecycle 注册表，跨服务契约）

| field | 含义 |
| --- | --- |
| `{sandboxID}` | JSON 编码的 `SandboxLifecycleMeta`（见 [`CubeMaster/pkg/lifecycle/schema.go`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeMaster/pkg/lifecycle/schema.go)） |

**`sandbox:lifecycle:state`**（暂停/恢复协调）

| value | 含义 |
| --- | --- |
| `running` | 沙箱运行中 |
| `pausing` | 暂停过渡中 |
| `paused` | 沙箱已暂停 |
| `resuming` | 恢复过渡中 |

## 6. TTL 策略

| Key 类型 | 策略 | 说明 |
| --- | --- | --- |
| `node:metric` | 刷新式 TTL | 每次心跳写入后 `EXPIRE`（默认 600s）；活跃节点持续续期，下线节点自动过期清理 |
| `sandbox:proxy` | 无 TTL | 仅在沙箱创建时写入、无续期路径；生命周期由 CubeMaster 在沙箱销毁时 `DEL`。**禁止**设置短于沙箱最大存活时间的 TTL |
| `task:describe` | 固定 TTL | 写入后 `EXPIRE`，默认 86400s（`describe_task_expire_time`） |
| `instance:info` / `instance:meta` | 无 TTL | 随实例生命周期管理；后续可按需补充 |
| `sandbox:lifecycle:meta` | 无 TTL | 沙箱创建时写入，销毁时 `HDEL` |
| `sandbox:lifecycle:events` | MAXLEN ~ | 每次 `XADD` 时裁剪（默认 ~100000） |
| `sandbox:lifecycle:state` | SET TTL | 每次写入带 `EX`（cube-lifecycle-manager 默认 60s）；回滚或沙箱删除时释放 |
| `cube_proxy:registry` | 无 TTL（依赖心跳） | 每个 CubeProxy 副本启动时写入；对应心跳过期后由 cube-lifecycle-manager 通过 `HDEL` 清理 |
| `cube_proxy:heartbeat` | Sorted Set 过期 | Score 为最近一次心跳的 unix ms，超过 `heartbeat_ttl`（默认 15s）的条目由 `ZREMRANGEBYSCORE` 清理 |
| 缓存类（未来新增） | 必须设 TTL | 写入时显式声明，并在文档中登记 |

## 7. 各服务实现约定

### CubeMaster（Go）

- Key 构造：[`pkg/base/rediskey`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeMaster/pkg/base/rediskey/rediskey.go)
- 业务读写：[`pkg/localcache`](https://github.com/tencentcloud/CubeSandbox/tree/master/CubeMaster/pkg/localcache)、[`pkg/instancecache`](https://github.com/tencentcloud/CubeSandbox/tree/master/CubeMaster/pkg/instancecache)
- 配置项（`redis:` 段）：`node_metric_ttl_sec`（节点指标 TTL，秒）

### CubeProxy（OpenResty / Lua）

- Key 构造：[`lua/redis_keys.lua`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeProxy/lua/redis_keys.lua)
- 业务读取：[`lua/sandbox_backend.lua`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeProxy/lua/sandbox_backend.lua)、[`lua/utils.lua`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeProxy/lua/utils.lua)
- CubeProxy **只读** `shared` 命名空间下的 key，不负责写入

### CubeAPI（Rust，预留）

CubeAPI 当前不使用 Redis。若未来接入，须遵循：

- 私有数据：`cube:v1:api:{resource}:{id}`
  - 会话：`cube:v1:api:session:{token}`
  - 限流：`cube:v1:api:ratelimit:{apikey}`
  - 配置缓存：`cube:v1:api:setting:{setting_key}`
- 共享数据：复用 `cube:v1:shared:*`，不得新造无前缀 key
- 须实现与 CubeMaster / CubeProxy 对等的 key 构造模块

## 8. 新增 Key 流程

1. 在本文档 §5 登记 key 模式、数据结构、scope、读写方、TTL。
2. 在对应服务的 key 构造模块中新增构造函数（Go `rediskey` / Lua `redis_keys`）。
3. 若为 `shared` scope，同步修改所有读写方并补充集成测试。
4. 确认 `SCAN cube:v1:{scope}:{resource}:*` 可正确枚举。

---

## 附录：历史 Key 对照表

以下为从不规范命名迁移到本规范时的对照关系，供排查存量数据、日志或运维脚本时参考。**新代码只允许使用右侧的标准 key。**

| 业务含义 | 历史 key（已废弃） | 标准 key | 数据结构 | scope |
| --- | --- | --- | --- | --- |
| 节点资源指标 | `{nodeID}`（裸 ID） | `cube:v1:master:node:metric:{nodeID}` | Hash | master |
| 沙箱代理路由 | `bypass_host_proxy:{sandboxID}` | `cube:v1:shared:sandbox:proxy:{sandboxID}` | Hash | shared |
| 实例信息 | `cube_instance_info:{insID}` | `cube:v1:master:instance:info:{insID}` | Hash | master |
| Describe 任务结果 | `describetask:{taskID}` | `cube:v1:master:task:describe:{taskID}` | Hash | master |
| 实例 metadata | `instance:metadata:{...}` | `cube:v1:master:instance:meta:{...}` | string / list | master |
