# Redis Cluster 部署与迁移

CubeMaster、CubeTemplateCenter、CubeOps、CubeProxy 和生命周期管理器可以连接外部原生 Redis Cluster。单机和 Sentinel 模式仍然可用。部署工具负责配置客户端，不负责创建 Redis 集群。

## 配置

使用 Helm 时，提供以逗号分隔的种子节点列表，并通过现有的私有配置提供密码：

```yaml
redis:
  host: ""
  masterName: ""
  sentinelNodes: ""
  clusterNodes: "redis-1.example:6379,redis-2.example:6379,redis-3.example:6379"
  db: 0
```

Cluster 模式会禁用内置 Redis 部署，并向相关组件传递连接配置。Master 和 TemplateCenter 的配置变更会触发 Pod 更新。

使用一键部署时，在 `.env` 中设置以下配置。认证继续使用 `CUBE_EXTERNAL_REDIS_PASSWORD`：

```bash
CUBE_EXTERNAL_REDIS_CLUSTER_NODES=redis-1.example:6379,redis-2.example:6379,redis-3.example:6379
CUBE_EXTERNAL_REDIS_DB=0
```

启用 Cluster 模式前，清空固定的单机 Redis 地址和 Sentinel 配置。Cluster 仅支持 DB 0。客户端必须能直接访问所有种子节点，以及槽位发现、MOVED 和 ASK 返回的所有地址；单个负载均衡入口通常不足以满足要求。本次变更保留密码认证，不新增 TLS 或 ACL 用户名配置。

手动部署时，分别配置各客户端：

| 组件 | 配置 |
| --- | --- |
| Master / TemplateCenter | YAML `redis.cluster_nodes` 和 `redis.db_no: 0` |
| Ops | `REDIS_CLUSTER_NODES` 和 `REDIS_DB=0`；不设置 `REDIS_URL` |
| 生命周期管理器 | `CUBE_LCM_REDIS_CLUSTER_NODES` 和 `CUBE_LCM_REDIS_DB=0` |
| Proxy 路由 | nginx `$redis_cluster_nodes` 和 `$redis_index=0` |
| Proxy 注册心跳 | `CUBE_PROXY_REGISTRY_REDIS_CLUSTER_NODES` 和 `CUBE_PROXY_REGISTRY_REDIS_DB=0` |

## 一致性与容量限制

- 保持现有 key 名称，不强制将所有 key 放入同一槽位。
- 单 key 锁、解锁脚本和生命周期 WATCH 事务，在所属主节点上仍然具有原子性。
- key 扫描遍历所有主节点并对结果去重。跨槽位的快照读取和生命周期状态读取提供近似视图，不构成全局一致性快照。
- 多个独立 key 的删除可能在部分完成后失败。调用方可以重试幂等清理。
- Proxy pipeline 独立路由各条命令，并保持结果顺序。某条命令发生重定向时，不会重放其他已经成功的命令。
- Master、Ops 和 Proxy 会重试明确的 MOVED、ASK 和 TRYAGAIN 服务端拒绝。连接建立后的传输超时和断连会返回给调用方，不重放执行结果不确定的写入。
- Go 客户端在 READONLY 恢复或扫描时刷新槽位信息。普通传输错误直接返回；后续请求可通过 MOVED 更新路由。Proxy 在需要刷新路由信息时重新访问种子节点。
- 普通连接接口不支持全局 KEYS、跨 key 事务或跨槽位 MSET。新增原子操作必须明确其 key 和槽位边界。

生命周期 hash、事件 stream 和代理注册表仍分别是单个 key。增加分片无法拆分单个热点 key。Redis Cluster 仍受现有异步复制、租约过期和在途操作限制；关键操作仍需幂等性、状态检查和执行端校验。

## 迁移

### 客户端与升级行为

- 当前不支持 IPv6 Redis Cluster。配置可能接受带方括号的种子地址，但各客户端对槽位发现和重定向通告的 IPv6 地址处理尚不一致，请使用 IPv4 地址。
- Master 的 `max_retry` 不控制 Cluster 传输重试。Master、Ops 和 Proxy 会直接返回执行结果不确定的传输错误，不自动重放。
- 生命周期管理器使用 go-redis ClusterClient，保留已有的重试行为。`MaxRetries: -1` 禁用单节点重试，但 Cluster 层仍会处理重定向并重试部分断连；EXEC 响应丢失后，WATCH 回调可能再次执行。前述不重放保证不适用于生命周期写入，验证报告也不声称执行过 LCM 响应丢失测试。
- 升级到此 Chart 会使 Master、TemplateCenter、Ops、生命周期管理器和 Proxy 进行一次滚动更新，即使 `clusterNodes` 为空也如此，原因是 Pod 配置校验和与 Proxy 入口发生变化。请提前为滚动更新预留容量。

### 迁移步骤

1. 备份现有 Redis 数据，区分可重建数据和必须迁移的生命周期状态。
2. 如果当前使用非零数据库，规划迁移到 DB 0，并检查 key 冲突。
3. 在维护窗口内暂停写入方和生命周期任务，等待在途操作结束后再迁移数据。
4. 同时切换 Master、TemplateCenter、Ops、Proxy 和生命周期管理器，使其使用同一后端。
5. 恢复流量前验证节点发现、路由和状态恢复。回滚前先停止新写入并核对新增数据，不要直接切回过期状态。

## 针对性验证

运行 Go 集成用例时，将 `TEST_REDIS_CLUSTER_NODES` 指向可丢弃的测试集群；启用认证时，通过私有环境来源提供 `TEST_REDIS_CLUSTER_PASSWORD`。用例覆盖跨槽位读写、跨主节点扫描、Lua 解锁和单 key WATCH。不要将测试指向业务 Redis 实例。

Proxy 的 `tests/redis_cluster_test.lua` 覆盖 MOVED、ASKING、跨 key pipeline 和结果不确定的写入。已有 Sentinel 用例继续验证原有模式。

`pkgs/redisx` 的主节点切换用例还要求设置 `TEST_REDIS_CLUSTER_FAILOVER=1`。第一个种子节点必须是有副本的主节点。该测试会主动提升副本，仅适用于可丢弃的测试集群。

真实恢复测试覆盖切主期间写入、原生 READONLY 拒绝，以及写入已生效后的响应丢失。对应的 OpenResty 测试是 `CubeProxy/tests/redis_cluster_live_test.lua`。必须显式提供 Cluster 节点和切主开关；切主后应重新核对种子节点顺序。已执行的命令、结果和诊断限制见 [验证报告](../../guide/redis-cluster-validation.md)。

部署前，应从目标网络验证所有通告地址的可达性，并执行真实沙箱生命周期检查。

验证报告及其原始结果附件有意仅保留英文，以便命令和结果共用一份参考；部署与迁移指南同时维护 [英文版](../../guide/redis-cluster.md) 和本中文版。

降级主节点返回 READONLY 时，客户端会使其缓存路由失效。Go 客户端刷新拓扑，使用新连接重试一次；每次尝试仍保留原有的八步重定向上限。Proxy 在八次尝试的路由循环内处理 READONLY。两条路径都不会在结果不确定的传输超时后自动重放命令。
