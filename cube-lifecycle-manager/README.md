# cube-lifecycle-manager

Standalone service that owns sandbox auto-pause / auto-resume coordination
between CubeMaster, CubeProxy and Redis.

- Consumes lifecycle events from `cube:v1:shared:sandbox:lifecycle:events`
- Discovers every live CubeProxy replica in real time via
  `cube:v1:shared:cube_proxy:{registry,heartbeat}` and broadcasts sandbox
  metadata + state through their `/admin/*` endpoints
- Handles the synchronous `/internal/resume` callback CubeProxy invokes when
  a paused sandbox receives a request

## Local development

```sh
make test    # go test ./...
make build   # local image, tag: cube-lifecycle-manager:v0.7.0-<arch>
```

## Publishing images

Multi-arch release flow (mirrors `CubeEgress/Makefile`). Runs against both
`cube-sandbox-int.tencentcloudcr.com` and `cube-sandbox-cn.tencentcloudcr.com`.

```sh
# On an amd64 host:
make build push ARCH=amd64

# On an arm64 host:
make build push ARCH=arm64

# On either host (both arch-specific images must already be pushed):
make manifest
```

Overrides:

- `IMAGE_TAG=<tag>` — override the release tag (default `v0.7.0`)
- `V=1` — verbose docker commands

## Configuration

All configuration is via environment variables (prefix `CUBE_LCM_`); see
`internal/config/config.go` for the authoritative list.

## Active-standby HA (issue #1211)

By default (`CUBE_LCM_HA_ENABLED` unset) the process runs every loop
unconditionally, which is the right mode for the single-replica one-click
deployment. When `CUBE_LCM_HA_ENABLED=1`, multiple replicas can run against
the same Redis in active-standby mode:

- Replicas elect a leader through a Redis lease
  (`cube:v1:shared:lock:lifecycle_manager:leader`, registered in
  `docs/zh/dev/redis-key-spec.md`). Only the leader runs the stateful loops:
  stream consumer (create/delete/update/state events), idle sweeper,
  last-active poller, and stale-pending-entry takeover (`XAUTOCLAIM`).
- Every replica — leader and standby alike — returns 200 on `/readyz`.
  Readiness only means "process healthy"; nothing gates it on leadership.
  The JSON body still carries a `role` field (`leader` / `standby` /
  `standalone`) for observability. The Kubernetes Service therefore routes
  `/internal/resume` traffic to any Ready pod.
- A standby can genuinely serve resume: on a registry miss it answers
  straight from the authoritative meta hash (meta-hash fallback), even
  though its in-memory registry is empty — so resume traffic keeps flowing
  during a failover instead of hitting a zero-ready-endpoints window.
  Cross-replica resume ownership is atomic: a Redis Lua compare-and-set on
  the sandbox state key with a per-process random owner token (transition
  values look like `resuming@<owner>`) means two replicas receiving the
  same resume concurrently result in exactly one actual CubeMaster resume
  RPC.
- On failover (lease expiry, at most one `CUBE_LCM_LEADER_TTL`) the new
  leader bootstraps its registry from the meta hash, replays the snapshot
  to every CubeProxy asynchronously (per-proxy goroutines, 5-minute total
  bound — promotion is never blocked by an unreachable proxy), then claims
  the dead consumer's pending stream entries via `XAUTOCLAIM` (idle ≥
  `CUBE_LCM_RECONCILE_INTERVAL`).

There is intentionally **no periodic reconciler** in this change. An
earlier revision ran a full meta-hash resync that pushed corrections to
the proxies, but CubeMaster's meta-hash writes are not authoritative in
all failure modes (an `HSET`/`HDEL` can fail while the stream `XADD`
succeeds), so treating the hash as absolute truth could delete or
resurrect sandboxes. Full reconciliation — hash-authority hardening plus
guaranteed eventual proxy/meta convergence — is deferred to a follow-up
change. The failover + resume path does not depend on it.

HA-specific variables:

| Variable | Default | Meaning |
| --- | --- | --- |
| `CUBE_LCM_HA_ENABLED` | `false` | Enable active-standby leader election |
| `CUBE_LCM_INSTANCE_ID` | hostname | Unique replica identity written into the lease |
| `CUBE_LCM_LEADER_KEY` | `cube:v1:shared:lock:lifecycle_manager:leader` | Lease key |
| `CUBE_LCM_LEADER_TTL` | `15s` | Lease expiry; upper bound on failover time |
| `CUBE_LCM_LEADER_RENEW_INTERVAL` | `5s` | Lease renewal / acquisition retry cadence |
| `CUBE_LCM_RECONCILE_INTERVAL` | `60s` | Cadence of the leader's stale-pending-entry takeover passes; also the `XAUTOCLAIM` min idle time (must be ≥ `CUBE_LCM_LEADER_TTL` in HA mode). No longer drives a reconciler — none exists |

Two failure-handling notes:

- If a leader loop fails, the process exits so the pod supervisor restarts
  it — instead of stepping down and re-electing in a hot loop. Failover
  still completes within one `CUBE_LCM_LEADER_TTL`.
- `CUBE_LCM_RECONCILE_INTERVAL` must be ≥ `CUBE_LCM_LEADER_TTL` (enforced
  at startup in HA mode): it doubles as the `XAUTOCLAIM` min-idle, and a
  smaller value could steal pending stream entries from a merely
  partitioned — still alive — old leader.

The Helm chart enables this by default (`lifecycleManager.ha.enabled: true`,
`replicas: 2`) and derives `CUBE_LCM_INSTANCE_ID` from the pod name.

### Upgrading an existing single-replica deployment to HA

Roll the new version out with `replicas: 1` first and verify the pod
becomes Ready, then scale to 2. Never scale out from a build where
standbys report NotReady on `/readyz`: with the chart's default
`maxUnavailable: 0` strategy the new standby never becomes available and
the rollout deadlocks.
