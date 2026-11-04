# Redis Cluster deployment and migration

CubeSandbox supports an external native Redis Cluster as its Redis backend. Standalone and Sentinel modes are unaffected.

## Requirements

- **DB 0 only.** Move data off nonzero databases before enabling Cluster mode.
- **Password authentication only.** No TLS or ACL usernames in this change.
- **IPv4 only.** IPv6 addresses advertised by slot discovery and redirects are not handled consistently across clients.
- **Direct reachability to every advertised address.** Clients dial the addresses returned by slot discovery, MOVED, and ASK. A single load-balanced endpoint is usually not enough.

## Configuration

Helm — set the seed list in values; the password goes through the existing private configuration:

```yaml
redis:
  host: ""
  masterName: ""
  sentinelNodes: ""
  clusterNodes: "redis-1.example:6379,redis-2.example:6379,redis-3.example:6379"
  db: 0
```

Cluster mode does not deploy the bundled Redis. Master and TemplateCenter configuration changes roll their Pods.

One-click — set in `.env`; authentication reuses `CUBE_EXTERNAL_REDIS_PASSWORD`:

```bash
CUBE_EXTERNAL_REDIS_CLUSTER_NODES=redis-1.example:6379,redis-2.example:6379,redis-3.example:6379
CUBE_EXTERNAL_REDIS_DB=0
```

Clear the standalone host and Sentinel settings before enabling Cluster mode.

Manual deployment — per-client settings:

| Component | Configuration |
| --- | --- |
| Master / TemplateCenter | `redis.cluster_nodes`, `redis.db_no: 0` |
| Ops | `REDIS_CLUSTER_NODES`, `REDIS_DB=0`, leave `REDIS_URL` unset |
| Lifecycle manager | `CUBE_LCM_REDIS_CLUSTER_NODES`, `CUBE_LCM_REDIS_DB=0` |
| Proxy routing | nginx `$redis_cluster_nodes`, `$redis_index=0` |
| Proxy registration heartbeat | `CUBE_PROXY_REGISTRY_REDIS_CLUSTER_NODES`, `CUBE_PROXY_REGISTRY_REDIS_DB=0` |

## Post-deployment verification

- From the client network, confirm every advertised address is reachable.
- Run one full sandbox lifecycle: create, route, pause, resume, delete.
- Confirm node snapshots cover nodes on more than one primary.

## Operational behavior and limits

- Key names are unchanged. Hot keys (lifecycle state, event stream, proxy registry) stay on a single shard.
- Single-key locks, unlock scripts, and WATCH transactions remain atomic on the owning primary.
- Cross-slot batch reads are a best-effort view, not a globally consistent snapshot.
- Multi-key deletes can partially complete. Retry idempotent cleanup on failure.
- Proxy pipelines route each command independently, preserve order, and never replay a completed command.
- Master, Ops, and Proxy retry only server-declared redirects (MOVED/ASK/TRYAGAIN), plus one refreshed retry after a READONLY from a demoted primary. Ambiguous transport errors are returned to the caller, never replayed. Master's `max_retry` is inert under Cluster mode.
- The lifecycle manager keeps go-redis redirect and disconnect retries. A lost EXEC response may re-run a WATCH callback — the no-replay guarantee does not cover lifecycle writes.
- Upgrading this chart rolls every control-plane component once, even with Cluster disabled. Plan capacity for the rollout.

## Migration

1. Back up the existing Redis data.
2. Move nonzero databases to DB 0 and check for key collisions.
3. In a maintenance window, pause writers and lifecycle jobs; let in-flight operations finish.
4. Switch all components to the new cluster at the same time.
5. Verify discovery, routing, and state recovery before resuming traffic. Stop new writes and reconcile data before any rollback.
