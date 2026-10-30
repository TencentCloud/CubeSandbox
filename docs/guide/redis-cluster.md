# Redis Cluster deployment and migration

CubeMaster, CubeTemplateCenter, CubeOps, CubeProxy, and the lifecycle manager can connect to an external native Redis Cluster. Standalone and Sentinel remain available. Deployment tooling configures clients; it does not create the Redis Cluster.

## Configuration

For Helm, supply comma-separated seed endpoints and provide the password through the existing private configuration:

```yaml
redis:
  host: ""
  masterName: ""
  sentinelNodes: ""
  clusterNodes: "redis-1.example:6379,redis-2.example:6379,redis-3.example:6379"
  db: 0
```

Cluster mode disables the bundled Redis deployment and passes the connection settings to the relevant components. Master and TemplateCenter configuration changes trigger Pod updates.

For one-click deployment, set the following in `.env`. Use the existing `CUBE_EXTERNAL_REDIS_PASSWORD` setting for authentication:

```bash
CUBE_EXTERNAL_REDIS_CLUSTER_NODES=redis-1.example:6379,redis-2.example:6379,redis-3.example:6379
CUBE_EXTERNAL_REDIS_DB=0
```

Clear fixed standalone endpoints and Sentinel settings before enabling Cluster mode. Only DB 0 is supported. Every seed and every address advertised by slot discovery, MOVED, or ASK must be directly reachable by the clients; a single load-balanced endpoint is usually insufficient. This change retains password authentication and does not add TLS or ACL username configuration.

For manual deployment, configure each client:

| Component | Configuration |
| --- | --- |
| Master / TemplateCenter | YAML `redis.cluster_nodes` and `redis.db_no: 0` |
| Ops | `REDIS_CLUSTER_NODES` and `REDIS_DB=0`; leave `REDIS_URL` unset |
| Lifecycle manager | `CUBE_LCM_REDIS_CLUSTER_NODES` and `CUBE_LCM_REDIS_DB=0` |
| Proxy routing | nginx `$redis_cluster_nodes` and `$redis_index=0` |
| Proxy registration heartbeat | `CUBE_PROXY_REGISTRY_REDIS_CLUSTER_NODES` and `CUBE_PROXY_REGISTRY_REDIS_DB=0` |

## Consistency and capacity limits

- Existing key names remain unchanged; clients do not force all keys into one slot.
- Single-key locks, unlock scripts, and lifecycle WATCH transactions remain atomic on the owning primary.
- Key scans visit every primary and deduplicate results. Cross-slot snapshot and lifecycle reads provide an approximate view rather than a globally consistent snapshot.
- Independent-key deletion can partially complete before an error. Callers may retry idempotent cleanup.
- Proxy pipelines route commands independently and preserve result order. Redirecting one command does not replay other successful commands.
- Master, Ops, and Proxy retry explicit MOVED, ASK, and TRYAGAIN server rejections. Transport timeouts and disconnections after connection establishment are returned to the caller instead of replaying an uncertain write.
- Go clients refresh slot information through reachable nodes after a primary disconnects, for subsequent requests. Proxy clients return to the seed endpoints when routing information needs refreshing.
- The ordinary connection facade does not support global KEYS, cross-key transactions, or cross-slot MSET. New atomic operations must define their key and slot boundaries explicitly.

The lifecycle hash, event stream, and proxy registry each remain a single key. Additional shards cannot split an individual hot key. Redis Cluster retains the existing asynchronous replication, lease-expiry, and in-flight operation limits; critical operations still require idempotency, state checks, and executor validation.

## Migration

1. Back up the existing Redis data. Identify rebuildable data and lifecycle state that must be migrated.
2. If using a nonzero database, plan the move to DB 0 and check for key collisions.
3. During a maintenance window, pause writers and lifecycle jobs. Wait for in-flight operations before migrating data.
4. Switch Master, TemplateCenter, Ops, Proxy, and the lifecycle manager together so they use the same backend.
5. Verify node discovery, routing, and state recovery before resuming traffic. Before rollback, stop new writes and reconcile new data rather than switching directly to stale state.

## Focused verification

Set `TEST_REDIS_CLUSTER_NODES` to a disposable cluster for the Go integration cases. They exercise cross-slot reads and writes, scans across primaries, Lua unlocks, and single-key WATCH. Never point these tests at a business Redis instance.

Proxy's `tests/redis_cluster_test.lua` covers MOVED, ASKING, cross-key pipelines, and uncertain writes. Existing Sentinel cases continue to exercise the previous mode.

The primary-promotion case in `pkgs/redisx` additionally requires `TEST_REDIS_CLUSTER_FAILOVER=1`. Its first seed must be a primary with a replica. This test actively promotes a replica and is intended only for disposable clusters.

Before deployment, verify reachability of all advertised endpoints from the target network and exercise a real sandbox lifecycle.
