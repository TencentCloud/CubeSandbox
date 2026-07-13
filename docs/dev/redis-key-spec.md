# Redis Key Naming Convention

> Scope: the Redis instance shared by CubeMaster and CubeProxy; CubeAPI must follow this convention when it onboards in the future.

## 1. Overview

Multiple CubeSandbox services (CubeMaster, CubeProxy, and potentially CubeAPI in the future) share the same Redis instance. To prevent key collisions, accidental deletes/overwrites, and to enable `SCAN`-based governance and auditing, every Redis key must follow a unified namespace convention.

**Core principles**:

- Bare entity IDs as top-level keys are forbidden.
- All keys must be produced by each service's **unified key-builder module**; scattered literal concatenation is forbidden.
- Cross-service shared data uses the `shared` namespace; changes require multi-side coordination.

## 2. Naming format

```
cube:{ver}:{scope}:{resource}[:{sub}...]:{id}
```

| Segment | Meaning | Values |
| --- | --- | --- |
| `cube` | Fixed product prefix, prevents collisions with third-party keys | constant `cube` |
| `ver` | Schema version; bump on **breaking** value-structure changes | currently `v1` |
| `scope` | Ownership namespace (by ownership/sharing, not by language) | see §3 |
| `resource[:sub]` | Business entity, lowercase, colon-separated | e.g. `node:metric`, `instance:info` |
| `id` | Entity ID (sandboxID, nodeID, taskID, etc.) | must not be omitted |

**Example**: `cube:v1:shared:sandbox:proxy:7c8fbcd45ffe450fb8f7fb223ad45507`

## 3. Scope enumeration

| scope | Meaning | Readers / writers |
| --- | --- | --- |
| `master` | CubeMaster-private data | CubeMaster only |
| `proxy` | CubeProxy-private data | CubeProxy only |
| `api` | CubeAPI-private data (reserved) | CubeAPI only |
| `shared` | Cross-service contract | multiple services |

Keys under `shared` are cross-service contracts: adding, renaming, or changing the data structure requires **all readers and writers to change in lockstep**; no single service may change unilaterally.

## 4. Mandatory rules

1. **Centralized construction**: CubeMaster uses [`CubeMaster/pkg/base/rediskey/rediskey.go`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeMaster/pkg/base/rediskey/rediskey.go); CubeProxy uses [`CubeProxy/lua/redis_keys.lua`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeProxy/lua/redis_keys.lua). When adding a new key, register a builder there first (or in the CubeAPI equivalent), then call it from business code.
2. **Explicit TTL**: cache keys must set a TTL; keys whose lifecycle is managed by an explicit business `DEL` (e.g. sandbox routes) default to no TTL. A fallback TTL is only allowed when there is a refresh path, or when the TTL exceeds the entity's maximum lifetime.
3. **Reserved semantic segments** (use when adding locks, idempotency keys, etc.):
   - Distributed lock: `cube:v1:{scope}:lock:{resource}:{id}`
   - Idempotency key: `cube:v1:{scope}:idemp:{resource}:{id}`
4. **Governance friendly**: keys of the same kind must be enumerable via `SCAN cube:v1:{scope}:{resource}:*`; do not embed a variable ID in the middle of the prefix in a way that breaks bulk matching.
5. **Version evolution**: bump `ver` only on breaking value-structure changes; adding Hash fields or extending fields does not require a bump.
6. **ID safety**: the `id` segment must not contain unescaped `:`; validate entity IDs before construction to prevent key injection and unauthorized reads.

## 5. Registered key catalog

The following are the standard keys currently registered in the system (`v1`). New business keys must be added here.

| Meaning | Key pattern | Type | scope | Writer | Reader | TTL |
| --- | --- | --- | --- | --- | --- | --- |
| Node resource metrics | `cube:v1:master:node:metric:{nodeID}` | Hash | master | CubeMaster | CubeMaster | refresh-style (heartbeat, default 600s) |
| Sandbox proxy route | `cube:v1:shared:sandbox:proxy:{sandboxID}` | Hash | shared | CubeMaster | CubeProxy | none (lifecycle via `DEL`) |
| Instance info | `cube:v1:master:instance:info:{insID}` | Hash | master | CubeMaster | CubeMaster | none |
| Describe task result | `cube:v1:master:task:describe:{taskID}` | Hash | master | CubeMaster | CubeMaster / external | 86400s (configurable) |
| Instance metadata (reserved) | `cube:v1:master:instance:meta:{...}` | string / list | master | CubeMaster | CubeMaster | none |
| Sandbox lifecycle registry | `cube:v1:shared:sandbox:lifecycle:meta` | Hash | shared | CubeMaster | cube-lifecycle-manager | none (lifecycle via `HDEL`) |
| Sandbox lifecycle events | `cube:v1:shared:sandbox:lifecycle:events` | Stream | shared | CubeMaster | cube-lifecycle-manager | MAXLEN ~ 100000 |
| Sandbox lifecycle state | `cube:v1:shared:sandbox:lifecycle:state:{sandboxID}` | String | shared | cube-lifecycle-manager | cube-lifecycle-manager | SET TTL (default 60s) |
| CubeProxy replica registry | `cube:v1:shared:cube_proxy:registry` | Hash | shared | CubeProxy | cube-lifecycle-manager | none (evicted on heartbeat expiry via `HDEL`) |
| CubeProxy replica heartbeat | `cube:v1:shared:cube_proxy:heartbeat` | Sorted Set | shared | CubeProxy | cube-lifecycle-manager | none (`ZREMRANGEBYSCORE` on expiry, default 15s) |

### 5.1 Hash field conventions

**`node:metric`** (node metrics)

| field | Meaning |
| --- | --- |
| `ins_id` | Node ID |
| `update_at` | Metric update time |
| `quota_cpu_usage` | CPU quota usage (milli-cpu) |
| `quota_mem_mb_usage` | Memory quota usage (MB) |
| `mvm_num` | VM count |
| `nic_queues` | NIC queue count |
| `data_disk_usage_per` / `storage_disk_usage_per` / `sys_disk_usage_per` | Disk usage (%) |

**`sandbox:proxy`** (sandbox route, cross-service contract)

| field | Meaning |
| --- | --- |
| `HostIP` | Host IP |
| `SandboxIP` | Sandbox internal IP (optional) |
| `CreatedAt` | Creation time |
| `{containerPort}` | Container port → host mapped port (dynamic field) |

**`instance:info`** (instance info)

See the `redis` tags on `InstanceInfoMap` in [`CubeMaster/pkg/base/types/redis.go`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeMaster/pkg/base/types/redis.go).

**`task:describe`** (async task)

| field | Meaning |
| --- | --- |
| `task_id` | Task ID |
| `status` | Task status |
| `error_code` | Error code |
| `error_message` | Error message |

**`sandbox:lifecycle:meta`** (sandbox lifecycle registry, cross-service contract)

| field | Meaning |
| --- | --- |
| `{sandboxID}` | JSON-encoded `SandboxLifecycleMeta` (see [`CubeMaster/pkg/lifecycle/schema.go`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeMaster/pkg/lifecycle/schema.go)) |

**`sandbox:lifecycle:state`** (pause/resume coordination)

| value | Meaning |
| --- | --- |
| `running` | Sandbox is active |
| `pausing` | Pause transition in progress |
| `paused` | Sandbox is paused |
| `resuming` | Resume transition in progress |

## 6. TTL policy

| Key type | Policy | Notes |
| --- | --- | --- |
| `node:metric` | Refresh-style TTL | `EXPIRE` after each heartbeat write (default 600s); live nodes keep refreshing, offline nodes auto-expire |
| `sandbox:proxy` | No TTL | Written once at sandbox creation with no refresh path; lifecycle managed by CubeMaster `DEL` on teardown. **Do not** set a TTL shorter than the max sandbox lifetime |
| `task:describe` | Fixed TTL | `EXPIRE` on write, default 86400s (`describe_task_expire_time`) |
| `instance:info` / `instance:meta` | No TTL | Managed per instance lifecycle; may be augmented later |
| `sandbox:lifecycle:meta` | No TTL | Written on sandbox create, `HDEL` on destroy |
| `sandbox:lifecycle:events` | MAXLEN ~ | Stream trimmed on each `XADD` (default ~100000) |
| `sandbox:lifecycle:state` | SET TTL | `EX` on each write (cube-lifecycle-manager default 60s); released on rollback or sandbox delete |
| `cube_proxy:registry` | No TTL (heartbeat-derived) | Written by each CubeProxy replica on startup; entries are `HDEL`'d by cube-lifecycle-manager once the corresponding heartbeat expires |
| `cube_proxy:heartbeat` | Sorted Set expiry | Score = last heartbeat unix ms; entries older than `heartbeat_ttl` (default 15s) are removed via `ZREMRANGEBYSCORE` |
| Cache keys (future) | TTL required | Must be declared on write and registered in this document |

## 7. Per-service implementation

### CubeMaster (Go)

- Key builders: [`pkg/base/rediskey`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeMaster/pkg/base/rediskey/rediskey.go)
- Business read/write: [`pkg/localcache`](https://github.com/tencentcloud/CubeSandbox/tree/master/CubeMaster/pkg/localcache), [`pkg/instancecache`](https://github.com/tencentcloud/CubeSandbox/tree/master/CubeMaster/pkg/instancecache)
- Config (`redis:` block): `node_metric_ttl_sec` (node-metric TTL in seconds)

### CubeProxy (OpenResty / Lua)

- Key builders: [`lua/redis_keys.lua`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeProxy/lua/redis_keys.lua)
- Business reads: [`lua/sandbox_backend.lua`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeProxy/lua/sandbox_backend.lua), [`lua/utils.lua`](https://github.com/tencentcloud/CubeSandbox/blob/master/CubeProxy/lua/utils.lua)
- CubeProxy is **read-only** for keys under `shared`; it does not write them

### CubeAPI (Rust, reserved)

CubeAPI does not use Redis today. If it onboards in the future, it must follow:

- Private data: `cube:v1:api:{resource}:{id}`
  - Session: `cube:v1:api:session:{token}`
  - Rate limit: `cube:v1:api:ratelimit:{apikey}`
  - Config cache: `cube:v1:api:setting:{setting_key}`
- Shared data: reuse `cube:v1:shared:*`; never create prefix-less keys
- Implement a key-builder module equivalent to CubeMaster / CubeProxy

## 8. Adding a new key

1. Register the key pattern, data type, scope, readers/writers, and TTL in §5 of this document.
2. Add a builder function in the service's key-builder module (Go `rediskey` / Lua `redis_keys`).
3. If `shared` scope, update all readers and writers together and add integration tests.
4. Confirm `SCAN cube:v1:{scope}:{resource}:*` enumerates correctly.

---

## Appendix: Legacy key crosswalk

The table below maps pre-standardization keys to the keys defined in this convention. Use it when investigating legacy data, logs, or ops scripts. **New code must use only the standard keys on the right.**

| Meaning | Legacy key (deprecated) | Standard key | Type | scope |
| --- | --- | --- | --- | --- |
| Node resource metrics | `{nodeID}` (bare ID) | `cube:v1:master:node:metric:{nodeID}` | Hash | master |
| Sandbox proxy route | `bypass_host_proxy:{sandboxID}` | `cube:v1:shared:sandbox:proxy:{sandboxID}` | Hash | shared |
| Instance info | `cube_instance_info:{insID}` | `cube:v1:master:instance:info:{insID}` | Hash | master |
| Describe task result | `describetask:{taskID}` | `cube:v1:master:task:describe:{taskID}` | Hash | master |
| Instance metadata | `instance:metadata:{...}` | `cube:v1:master:instance:meta:{...}` | string / list | master |
