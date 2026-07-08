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
make build   # local image, tag: cube-lifecycle-manager:v0.5.1-rc1-<arch>
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

- `IMAGE_TAG=<tag>` — override the release tag (default `v0.5.1-rc1`)
- `V=1` — verbose docker commands

## Configuration

All configuration is via environment variables (prefix `CUBE_LCM_`); see
`internal/config/config.go` for the authoritative list.
