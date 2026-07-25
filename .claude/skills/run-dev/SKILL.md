---
name: run-dev
description: Build, test, format, and develop CubeSandbox components. Use when asked to build, compile, run tests, format code, or do development work on this project.
---

# CubeSandbox Development Skill

CubeSandbox is a monorepo sandbox service (Go + Rust + Web). All component
builds run **inside a Docker builder container** via `make` targets at the
repo root. The builder image provides Go 1.24, Rust 1.89, protoc, and all
build dependencies.

Paths in this document are relative to the repo root

## Prerequisites

```bash
# Docker must be running
docker info > /dev/null 2>&1 || { echo "Docker is not running"; exit 1; }
```

The builder image is built once (or when toolchains change):

```bash
make builder-image
```

## Build

All builds happen inside the `cube-sandbox-builder:ubuntu2004` Docker image.
Binaries land in `_output/bin/`.

### Build all Go components

```bash
make all
```

This builds: cubemaster, cubelet, cubevsmapdump, network-agent, agent, cubeapi, shim.

### Build individual components

| Command | Component | Language | Time |
|---|---|---|---|
| `make cubemaster` | Master scheduler + CLI | Go | ~30s |
| `make cubelet` | Per-node sandbox agent + CLI | Go + Rust (cubecow SDK) | ~90s |
| `make agent` | In-guest agent daemon | Rust | ~2min |
| `make cubeapi` or `make cube-api` | E2B-compatible REST API | Rust (musl) | ~3min |
| `make shim` | containerd shim + runtime | Rust | ~2min |
| `make network-agent` | Network management | Go | ~30s |
| `make cubevsmapdump` | eBPF map dump tool | Go | ~20s |
| `make cube-proxy-sidecar` | Proxy sidecar (dev only) | Go | ~20s |
| `make cubecow-smoke` | CubeCoW smoke test | Go + Rust | ~60s |

### Verify a build

```bash
# Check files in `_output/bin/`
file _output/bin/cubemaster
```

## Test

Tests run inside the builder container. Some tests need external services
(Redis, KVM) and will fail without them — see **Gotchas**.

### Run component tests

```bash
# Agent tests (Rust) — 101+ tests, most pass without KVM
make builder-run BUILDER_CMD='cd /workspace/agent && make test'

# CubeMaster tests (Go) — needs Redis at minimum
make builder-run BUILDER_CMD='cd /workspace/CubeMaster && make proto && CI=true CUBE_MASTER_CONFIG_PATH=/workspace/CubeMaster/test/conf.yaml go test -short ./api/... ./pkg/...'

# Cubelet tests (Go)
make builder-run BUILDER_CMD='cd /workspace/Cubelet && make proto && go test -short ./pkg/...'

# CubeCoW native tests (Go + CGO, needs cubecow SDK built)
make cubecow-test-native

# Integration tests — require a running CubeSandbox cluster (dev-env or one-click deploy)
```

### Run a single test

```bash
# CubeMaster: single package with verbose output
make builder-run BUILDER_CMD='cd /workspace/CubeMaster && CI=true CUBE_MASTER_CONFIG_PATH=/workspace/CubeMaster/test/conf.yaml go test -v -run TestSomething ./pkg/base/...'

# Agent: single test
make builder-run BUILDER_CMD='cd /workspace/agent && cargo test -p cube-agent test_name'
```

## Format and Lint

```bash
# Format all components (Go + Rust)
make fmt

# Web UI lint
make web-lint
```

For a single component:

```bash
# Go component
make builder-run BUILDER_CMD='cd /workspace/CubeMaster && make fmt'

# Rust component — fmt automatically runs in builder
make builder-run BUILDER_CMD='cd /workspace/CubeAPI && cargo fmt --check'
```

## Web UI

The web dashboard lives in `web/` (Vite + Vue + TypeScript).

```bash
make web-install     # Install npm dependencies (~30s)
make web-build       # Build static assets → web/dist/ (~2s)
make web-dev         # Start Vite dev server (localhost:5173)
make web-lint        # Lint check
make web-preview     # Preview built assets
```

**Sync OpenAPI types** after CubeAPI changes:
```bash
make web-api-sync
```


## Gotchas

1. **Tests need Redis/KVM/MicroVM context.**
   - `CubeMaster/pkg/base/wrapredis` tests panic if Redis is not reachable.
   - `agent` sandbox tests (9 of 110) fail without a running KVM MicroVM.
   - Run with `-short` or filter to the relevant package when iterating.

2. **CubeMaster tests use `CUBE_MASTER_CONFIG_PATH`.**
   Must be set to `test/conf.yaml` inside the container (absolute path from `/workspace`).

3. **`go covdata` not available.**
   The builder's Go toolchain (1.25) removed `covdata`. Use `-coverprofile` directly
   or skip coverage:
   ```bash
   go test -short ./pkg/...    # no coverage flag
   ```

4. **Builder container runs as host UID:GID.**
   The `builder-run` target mounts the workspace with `--user $(UID):$(GID)`. Files
   written by the builder (e.g., `_output/bin/*`) are owned by the host user.

5. **All builds are sequential.**
   The Makefile uses `builder-run` which spawns one Docker container per target.
   Building `make all` runs each component sequentially. For parallel builds,
   use separate terminals or background processes.

6. **Agent build runs embedded unit tests before linking.**
   `make agent` includes `cargo test` for the logging crate as part of the build.
   These are pure unit tests and always pass.

7. **CubeCoW SDK must be built before cubelet.**
   `make cubelet` handles this automatically via the `cubecow-sdk` dependency,
   but if you build cubelet outside the make flow, run `make cubecow-sdk` first.

8. **KVM required for running sandboxes.**
   Building works without KVM, but actually creating/starting sandboxes needs
   `/dev/kvm` with nested virtualization enabled. This container has it (`/dev/kvm`
   exists with nested=Y).

## Troubleshooting

| Symptom | Fix |
|---|---|
| `docker: Cannot connect` | Docker daemon not running. `sudo systemctl start docker` |
| `make: *** [builder-image] Error` | Docker build failed. Check network, retry with `make builder-image BUILDER_FORCE_REBUILD=1`. From China, add `MIRROR=cn` to fetch the llvm.sh installer and clang-14 apt packages from a China mirror (the LLVM GPG key still comes from apt.llvm.org) |
| `error: protoc not installed` inside builder | Builder image is outdated. Rebuild: `make builder-image BUILDER_FORCE_REBUILD=1` |
| `go: no such tool "covdata"` | Don't use `-coverprofile` flag with `make test` in CubeMaster. Use raw `go test` instead. |
| `cargo: command not found` on host | Rust builds run inside Docker. Use `make <target>`, not raw `cargo`. |
| Build hangs on "Download modules" | Go module download is slow. The builder persists `GOPATH` at `~/.cache/cube-sandbox-builder/go`, so retry is faster. |
| `panic: nil pointer dereference` in Redis test | Expected — no Redis in builder. Filter: `-run 'Test[^D]'` or skip `wrapredis` package. |
