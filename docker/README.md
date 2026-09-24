# docker/

Dockerfiles used by CubeSandbox CI.

## `Dockerfile.builder`

Toolchain image used to compile CubeSandbox components (Go, Rust, kernel
tooling, etc.). Also prebuilds CubeS3lvol's SPDK + AWS CRT under `/opt/s3lvol-*`
(see [`CubeS3lvol/deps/README.md`](../CubeS3lvol/deps/README.md)). Published as
`ghcr.io/tencentcloud/cubesandbox-builder`
by [`.github/workflows/build-builder-image.yml`](../.github/workflows/build-builder-image.yml).

## `Dockerfile.cube-base` (+ `cube-entrypoint.sh`)

Base image for user-supplied sandbox templates. It is `ubuntu:22.04`
with `envd` preinstalled on `:49983`, so any image built `FROM` it is
already ready for Cube's readiness probe. Published as a multi-arch
(`linux/amd64` + `linux/arm64`) manifest list
`ghcr.io/tencentcloud/cubesandbox-base` by
[`.github/workflows/build-envd-base-image.yml`](../.github/workflows/build-envd-base-image.yml),
which compiles `envd` in-place from
[`e2b-dev/infra`](https://github.com/e2b-dev/infra) at tag `2026.16`
(override via `workflow_dispatch` input `envd_ref`) on native amd64 and
arm64 runners, then combines the per-arch images into one tag.

Minimal consumer example:

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16
RUN pip install pandas
```

Full user-facing tutorial (path A vs path B, entrypoint contract,
troubleshooting) lives in the Cube docs site:

- English: [Custom Template Images](../docs/guide/tutorials/bring-your-own-image.md)
- 中文：[自定义模板镜像](../docs/zh/guide/tutorials/bring-your-own-image.md)

## Optional repository-built Rust daemon

Go remains the default in `Dockerfile.cube-base`, existing image tags, and
one-click deployments. Rust is selected by building `Dockerfile.cube-base-rust`
from the **repository root**. The commands below create local images; no
published Rust image is required or implied. The
[optional Rust image workflow](../.github/workflows/build-cube-envd-image.yml)
builds and tests locally on both runner architectures, without publishing.
The existing Go image workflow retains its original triggers and publish conditions.

```sh
# Run from a source checkout, with Docker Buildx installed.
make cube-base                  # upstream Go, cubesandbox-base:local
make cube-base-rust             # Rust, cubesandbox-base:rust-local
make cube-base-rust TARGET_ARCH=arm64 CUBE_BASE_RUST_IMAGE=cubesandbox-base:rust-local-arm64

# Equivalent direct Rust build (repeat with linux/arm64).
docker buildx build --platform linux/amd64 --load \
  -f docker/Dockerfile.cube-base-rust \
  --build-arg CUBE_ENVD_COMMIT="$(git rev-parse HEAD)" \
  -t cubesandbox-base:rust-local .
```

The Rust build reads `rust-toolchain.toml` and `cube-envd/Cargo.lock`, uses the
builder's native Rust/protoc tools and cross-compiles a static musl daemon for
amd64 or arm64. The final image contains only the selected daemon at
`/usr/bin/envd` (root-owned, mode 0755), the `user` account (UID 1000), tini,
socat, and runtime utilities. Building runtime layers for another architecture
requires a native builder or Docker's configured emulation support.
`ENVD_BUILD_ARGS` forwards ordinary Buildx flags for builder/cache selection;
no private filesystem path or prebuilt daemon is required.

To consume the locally built Rust image:

```dockerfile
FROM cubesandbox-base:rust-local
# Add your application and optional CMD here.
```

Both images retain `ENVD_BIN`, `ENVD_PORT` (49983), `ENVD_EXTRA_ARGS`, and
`ENVD_LOG_FILE` (`/var/log/envd.log`, or `-` for container stdout/stderr).
Extra arguments are whitespace-separated words, not shell expressions;
`-isnotfc` is appended when absent. CMD remains the user's application command.
Do not put another envd or entrypoint invocation in CMD: the image already
starts its daemon. No automatic provider switching or restart is performed.

The self-contained Bash entrypoint can still be copied as a single file into
custom images; those images must provide `/bin/bash` and standard coreutils.
The supervisor stops the container when the daemon exits, including an
unexpected zero exit with a user CMD still running. It preserves the first
termination cause, forwards external TERM/INT/HUP to the user leader, asks the
daemon to stop, and allows five seconds before forcing shutdown. The container
runtime removes residual descendants. Health requires HTTP 204 and reports a
nonresponding daemon as unhealthy. These lifecycle checks also cover Go;
Go's existing upstream build arguments and `docker/` build context remain valid.

### Rust runtime prerequisites and local verification

Rust uses a writable cgroup-v2 hierarchy with CPU and memory controllers for
its additional PTY, command and forwarding process groups. If initialization
fails, it logs the reason and continues with a no-op cgroup manager, matching Go.
In fallback mode, children inherit the daemon's existing cgroup placement;
existing VM and platform limits are separate. Once groups are enabled, a later placement failure
still rejects that child. The image tests exercise both managed and fallback
behavior in disposable private PID/mount/network/cgroup containers. Never remount
or reconfigure the host cgroups to make a test pass. The test bootstrap removes
SYS_TIME before starting the daemon; do not grant it against the host clock.
Port forwarding additionally requires the guest's `169.254.0.21` address.

```sh
# On a Linux Docker host supporting private cgroup v2, test the selected image.
# These tests require permission to create privileged, isolated containers.
CUBE_ENVD_IMAGE=cubesandbox-base:rust-local CUBE_ENVD_ARCH=amd64 \
CUBE_ENVD_COMMIT="$(git rev-parse HEAD)" \
NO_PROXY=localhost,127.0.0.1,::1 no_proxy=localhost,127.0.0.1,::1 \
python3 -B -m unittest discover -s docker/tests -v
```

This exercises the image's declared entrypoint and real daemon. It does not
validate Firecracker, platform-created VMs, or SDK/template integration.
Cross-compilation and full-system QEMU runs are distinct from native arm64
hardware validation. Rust remains an optional implementation requiring those
public integration checks before a production readiness claim.

### Export a daemon for existing package inputs

```sh
make cube-envd TARGET_ARCH=amd64
# Static binary: _output/bin/cube-envd/amd64/envd
# For an arm64 build host / bundle, select TARGET_ARCH=arm64 instead.
ENVD_LOCAL_PATH="$PWD/_output/bin/cube-envd/amd64/envd" \
  ./deploy/one-click/build-release-bundle-builder.sh
```

`OUTPUT_DIR` changes the export directory. The existing one-click builder
embeds this explicitly supplied binary into `cubemastercli`; the bundle build
must run on the matching architecture. Missing, empty, oversized, non-ELF, and
wrong-architecture inputs fail rather than substituting Go. Without
`ENVD_LOCAL_PATH`, the original behavior remains: no daemon is embedded, and
Go base images continue to supply the default daemon. The package's embedded
input does not by itself enable template injection or change a running system.
See [one-click build requirements](../deploy/one-click/README.md) for the other
components and assets needed to assemble a complete bundle.
