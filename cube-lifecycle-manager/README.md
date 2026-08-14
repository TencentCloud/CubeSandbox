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
make build   # local image, tag: cube-lifecycle-manager:v0.6.0-<arch>
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

- `IMAGE_TAG=<tag>` — override the release tag (default `v0.6.0`)
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
  stream consumer, idle sweeper, last-active poller, and the periodic
  reconciler.
- Standbys keep serving HTTP: `/readyz` returns 503 so the Kubernetes
  Service routes `/internal/resume` only to the leader, but a standby can
  still answer a resume that reaches it by looking the sandbox up directly
  in the authoritative meta hash (registry-miss fallback).
- On failover (lease expiry, at most one `CUBE_LCM_LEADER_TTL`) the new
  leader re-bootstraps from the meta hash, replays the snapshot to every
  CubeProxy, claims the dead consumer's pending stream entries via
  `XAUTOCLAIM` (idle ≥ `CUBE_LCM_RECONCILE_INTERVAL`), and lets the
  reconciler converge any remaining drift between the meta hash, the
  in-memory registry, and the proxy meta dicts.

HA-specific variables:

| Variable | Default | Meaning |
| --- | --- | --- |
| `CUBE_LCM_HA_ENABLED` | `false` | Enable active-standby leader election |
| `CUBE_LCM_INSTANCE_ID` | hostname | Unique replica identity written into the lease |
| `CUBE_LCM_LEADER_KEY` | `cube:v1:shared:lock:lifecycle_manager:leader` | Lease key |
| `CUBE_LCM_LEADER_TTL` | `15s` | Lease expiry; upper bound on failover time |
| `CUBE_LCM_LEADER_RENEW_INTERVAL` | `5s` | Lease renewal / acquisition retry cadence |
| `CUBE_LCM_RECONCILE_INTERVAL` | `60s` | Reconciler cadence; also the min idle time for `XAUTOCLAIM` takeover |

The Helm chart enables this by default (`lifecycleManager.ha.enabled: true`,
`replicas: 2`) and derives `CUBE_LCM_INSTANCE_ID` from the pod name.
