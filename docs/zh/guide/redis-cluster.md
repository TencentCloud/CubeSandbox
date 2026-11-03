# Redis Cluster 部署与迁移

CubeSandbox 支持用外部原生 Redis Cluster 作为 Redis 后端。单机和 Sentinel 模式不受影响。

## 前置要求

- **仅支持 DB 0。** 启用前先把数据迁出非零数据库。
- **仅支持密码认证。** 不支持 TLS 和 ACL 用户名。
- **仅支持 IPv4。** IPv6 集群的重定向地址各客户端处理不一致。
- **每个通告地址都必须从客户端网络直连可达。** 单个负载均衡入口通常不够。

## 配置

Helm——在 values 里提供种子节点列表，密码走现有私有配置：

```yaml
redis:
  host: ""
  masterName: ""
  sentinelNodes: ""
  clusterNodes: "redis-1.example:6379,redis-2.example:6379,redis-3.example:6379"
  db: 0
```

Cluster 模式不部署内置 Redis。Master 和 TemplateCenter 的配置变更会滚动其 Pod。

一键部署——在 `.env` 中设置，密码沿用 `CUBE_EXTERNAL_REDIS_PASSWORD`：

```bash
CUBE_EXTERNAL_REDIS_CLUSTER_NODES=redis-1.example:6379,redis-2.example:6379,redis-3.example:6379
CUBE_EXTERNAL_REDIS_DB=0
```

启用前先清空单机地址和 Sentinel 配置。

手动部署——各客户端配置：

| 组件 | 配置 |
| --- | --- |
| Master / TemplateCenter | `redis.cluster_nodes`，`redis.db_no: 0` |
| Ops | `REDIS_CLUSTER_NODES`，`REDIS_DB=0`，不设 `REDIS_URL` |
| 生命周期管理器 | `CUBE_LCM_REDIS_CLUSTER_NODES`，`CUBE_LCM_REDIS_DB=0` |
| Proxy 路由 | nginx `$redis_cluster_nodes`，`$redis_index=0` |
| Proxy 注册心跳 | `CUBE_PROXY_REGISTRY_REDIS_CLUSTER_NODES`，`CUBE_PROXY_REGISTRY_REDIS_DB=0` |

## 部署后验证

- 从客户端网络确认所有通告地址可达。
- 跑一遍完整沙箱生命周期：创建、路由、暂停、恢复、删除。
- 确认节点快照覆盖多个主节点上的节点。

## 运行行为与限制

- key 名称不变。热点 key（生命周期状态、事件流、代理注册表）仍是单 key，分片拆不开。
- 单 key 锁、解锁脚本和 WATCH 事务在所属主节点上仍然原子。
- 跨槽位的批量读取是近似视图，不是全局一致快照。
- 多 key 删除可能部分成功。失败后重试幂等清理即可。
- Proxy pipeline 逐条独立路由，保序执行，不重放已成功的命令。
- Master、Ops、Proxy 只重试服务器明确返回的重定向（MOVED/ASK/TRYAGAIN）。切主后收到 READONLY 会刷新路由并重试一次。传输错误一律返回调用方，不重放。Master 的 `max_retry` 在 Cluster 下不生效。
- 生命周期管理器保留 go-redis 的重定向和断连重试。EXEC 响应丢失时 WATCH 回调可能重复执行，不重放保证不适用于生命周期写入。
- 升级本 Chart 会一次性滚动重启全部控制面组件，即使不启用 Cluster。请预留容量。

## 迁移

1. 备份现有 Redis 数据。
2. 非零数据库先迁到 DB 0，检查 key 冲突。
3. 维护窗口内暂停写入和生命周期任务，等在途操作结束。
4. 所有组件同时切换到新集群。
5. 恢复流量前验证节点发现、路由和状态恢复。回滚前先停新写入并核对新增数据。
