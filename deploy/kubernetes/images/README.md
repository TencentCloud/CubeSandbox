# CubeSandbox delivery images

This directory contains image build definitions used by the Kubernetes/TKE chart.

## Build entrypoint

```bash
PUSH=1 REGISTRY=cube-sandbox-int.tencentcloudcr.com/cube-sandbox IMAGE_TAG=v0.6.0-rc1 ./deploy/kubernetes/images/build-cube-images.sh
```

Before a real release, run `scripts/bump-image.sh vX.Y.Z` so hard-coded tags
(including chart `deploy/kubernetes/chart/values.yaml` component image tags)
match the release. `release-docker-images` runs `bump-image.sh --check` by
default; for a manual/dev image-only build, set workflow input
`skip_version_check=true`.

Use `NO_CACHE=1` when every Docker image layer must be rebuilt instead of
using Docker's build cache:

```bash
NO_CACHE=1 PUSH=1 REGISTRY=cube-sandbox-int.tencentcloudcr.com/cube-sandbox IMAGE_TAG=v0.6.0-rc1 ./deploy/kubernetes/images/build-cube-images.sh
```

The script defaults its temporary `BUILD_ROOT` to
`/tmp/cube-kubernetes-images-<version>` so large image contexts and downloads do
not land in the Git worktree. Override `BUILD_ROOT` only when you intentionally
want a different cache location.

The script reuses valid artifacts already present under `${BUILD_ROOT}/downloads`
and does not require a `.complete` marker.

Pass one or more image names to build only those images (package download and
`SOURCE_REF` export are skipped when not needed):

```bash
./deploy/kubernetes/images/build-cube-images.sh cubelet
./deploy/kubernetes/images/build-cube-images.sh cubelet cube-shim
```

Run `./deploy/kubernetes/images/build-cube-images.sh --help` for the full image
list and option summary.

## Local binaries for development

`--local` / `LOCAL_BIN=1` remains for package-based binary overlays into the
temporary docker context (it does not mutate `PACKAGE_DIR`). Currently no
package image uses an overlay.

For source-built images (`cube-master`, `cubemastercli`, `cubelet`,
`network-agent`, `cube-shim`, `cube-api`, `cube-ops`, `cube-proxy`, …), use
`SOURCE_REF=""` to compile from the current worktree (see below).

`cube-kernel` does not use the one-click package. It stages guest kernels from:

1. `CUBE_KERNEL_VMLINUX` (required) and optional `CUBE_KERNEL_PVM_VMLINUX`, or
2. Release assets `vmlinux-${arch}` (required) and `vmlinux-pvm-${arch}`
   (`arch` from `ONE_CLICK_ARCH`, default `amd64`).

**PVM policy:** required on `amd64`; optional on `arm64` (no PVM guest kernel
today — arm64 images are BM-only when `vmlinux-pvm-arm64` is absent).

```bash
CUBE_KERNEL_VMLINUX=/path/to/vmlinux \
  CUBE_KERNEL_PVM_VMLINUX=/path/to/vmlinux-pvm \
  IMAGE_TAG=dev ./deploy/kubernetes/images/build-cube-images.sh cube-kernel

# arm64 BM-only
ONE_CLICK_ARCH=arm64 CUBE_KERNEL_VMLINUX=/path/to/vmlinux \
  IMAGE_TAG=dev ./deploy/kubernetes/images/build-cube-images.sh cube-kernel

IMAGE_TAG=v0.6.0-rc1 ./deploy/kubernetes/images/build-cube-images.sh cube-kernel
```

`cube-guest` does not use the one-click package. It stages guest rootfs from:

1. `CUBE_GUEST_IMAGE_DIR` (directory containing `cube-guest-image-cpu.img`,
   `version`, `agent-version`), or
2. `CUBE_GUEST_IMAGE_TAR` (`.tar.gz` of those files), or
3. Release asset `cube-guest-image-${arch}.tar.gz` (same `IMAGE_TAG` when
   present, otherwise latest GitHub Release).

```bash
CUBE_GUEST_IMAGE_DIR=/path/to/cube-image IMAGE_TAG=dev \
  ./deploy/kubernetes/images/build-cube-images.sh cube-guest

IMAGE_TAG=v0.6.0-rc1 ./deploy/kubernetes/images/build-cube-images.sh cube-guest
```

## Pinning source to a release tag

`cube-master`, `cubemastercli`, `cubelet`, `network-agent`, `cube-shim`,
`cube-api`, `cube-ops`, `cube-proxy`, `cube-egress`, `cube-lifecycle-manager`, and
`cube-webui` are compiled from repository source (rather than binaries in the
release tarball). By default the script pins those source trees to
`${SOURCE_REF}` (defaulting to `${VERSION}`, so `v0.5.1` for the default
build). It exports `CubeMaster/`, `CubeAPI/`, `CubeProxy/`, `CubeEgress/`,
`cube-lifecycle-manager/`, `web/`, and `deploy/one-click/webui/` at that git
ref into `${BUILD_ROOT}/source-tree/` via `git archive` and points `REPO_ROOT`
there for the duration of the build. When building `cube-master` or
`cubemastercli`, it also exports `cubelog/`, `CubeDB/`, and `Cubelet/`;
`cube-master` additionally exports `deploy/scripts/` (volume-deps installer) and
`examples/volume/cos/` (Controller plugin binary + example conf).
When building `cubelet`, it also exports `Cubelet/`, `cubelog/`, `cubecow/`,
`deploy/scripts/`, `deploy/kubernetes/images/scripts/`, and
`examples/volume/cos/`. When building `network-agent`, it also exports
`network-agent/`, `CubeNet/`, `cubelog/`, `Cubelet/pkg/networkagentclient/`,
`deploy/kubernetes/images/scripts/`, and `configs/single-node/`. When building
`cube-shim`, it also exports `CubeShim/`, `hypervisor/`,
`deploy/one-click/config-cube.toml`, and `deploy/kubernetes/images/scripts/`.
When building `cube-ops`, it also exports `CubeOps/` and `CubeDB/` (required by
`CubeOps/Dockerfile`; not present on older release tags such as `v0.5.1` — use
`SOURCE_REF=""` for worktree builds).
This guarantees the images match the release tag even when the current worktree
is ahead of it.

### COS volume plugin (image vs Secret)

| Content | Delivery |
| --- | --- |
| Plugin binary `cube-volume-cos` | Baked into `cube-master` (`…/CubeMaster/plugin/`) and `cubelet` (`…/Cubelet/plugin/`). Images also ship `volume-cos.conf.example` for reference only — **not** runtime credentials. |
| Plugin registration `volume_plugins` | Chart renders into `files/cube-master/conf.yaml` (mounted as the Master config Secret). Cubelet `config.toml` already registers the Node-side plugin from the staged image. |
| Credentials `volume-cos.conf` | Chart `volumeCos` Secret mount (default off). Mount paths: Master `…/CubeMaster/plugin/volume-cos.conf`, Cubelet `…/Cubelet/plugin/volume-cos.conf` (file overlay on the hostPath toolbox). Enable with `volumeCos.enabled=true` and either `existingSecret` or inline `secretId` / `secretKey` / `bucket` / `region`. |

Do **not** bake `SECRET_ID` / `SECRET_KEY` into images.

To build from the current worktree instead (typically for development), set
`SOURCE_REF=""`:

```bash
SOURCE_REF="" PUSH=1 REGISTRY=<...> IMAGE_TAG=dev \
  ./deploy/kubernetes/images/build-cube-images.sh
```

To build from a different ref (branch, tag, or commit SHA):

```bash
SOURCE_REF=some-feature-branch PUSH=1 REGISTRY=<...> IMAGE_TAG=featureX \
  ./deploy/kubernetes/images/build-cube-images.sh
```

When the release package is older than the verified Kubernetes node runtime,
build `cube-node` by rebasing a known-good node image and copying the current
entrypoint into it:

```bash
CUBE_NODE_BASE_IMAGE=ccr.ccs.tencentyun.com/pavleli/cube-node:v0.4.0-cubevsfix-20260627 \
  PUSH=1 REGISTRY=cube-sandbox-int.tencentcloudcr.com/cube-sandbox IMAGE_TAG=v0.6.0-rc1 \
  ./deploy/kubernetes/images/build-cube-images.sh
```

This keeps the CubeVS/network-agent runtime fix while preserving the chart-side
entrypoint behavior.

## Image source policy

- `cube-node` continues to use `deploy/kubernetes/images/cube-node/Dockerfile`.
  It is a Kubernetes delivery image that bundles the node-side runtime components required by the Cube Node Big Pod, including `Cubelet`, `network-agent`, `cube-shim`, `cube-kernel-scf`, `cube-image`, `cube-vs`, and `cube-snapshot`. `cube-egress` is intentionally not bundled in this image because it is delivered as a separate sidecar image.
  If `CUBE_NODE_BASE_IMAGE` is set, the build script rebases that image instead
  and only replaces `/usr/local/bin/cube-node-entrypoint.sh`.
- `cube-node-init` (`wait-pvm-host` + `cube-node-init`) runs on the **`cube-node-bootstrap`** DaemonSet; `cube-pvm-host-bootstrap` runs on **`cube-node-pvm`** (placement.pvm only).
- `cube-wait-node-prep` is the Big Pod `wait-node-prep` **initContainer** and the bootstrap `write-node-prep-ready` hold container.
- `cube-master` is built exactly like CI (`.github/workflows/release-docker-images.yml`):
  context = repository root, file = `CubeMaster/docker/Dockerfile`, with
  `CUBE_VERSION` / `CUBE_COMMIT` / `CUBE_BUILD_TIME`. Requires BuildKit
  (`DOCKER_BUILDKIT=1`) for the adjacent `Dockerfile.dockerignore`. No duplicate
  Dockerfile is kept under `deploy/kubernetes/images/`.
- `cubelet` is built exactly like CI: context = repository root, file =
  `Cubelet/Dockerfile` (multi-stage CGO + cubecow via `CUBE_BUILDER_IMAGE`),
  with `CUBE_VERSION` / `CUBE_COMMIT` / `CUBE_BUILD_TIME`. Requires BuildKit for
  the adjacent `Dockerfile.dockerignore`. No duplicate Dockerfile is kept under
  `deploy/kubernetes/images/`.
- `network-agent` is built exactly like CI: context = repository root, file =
  `network-agent/Dockerfile` (multi-stage cubevs gen + `make proto` via
  `CUBE_BUILDER_IMAGE`, packages `network-agent` + `cubevsmapdump`), with
  `CUBE_VERSION` / `CUBE_COMMIT` / `CUBE_BUILD_TIME`. Requires BuildKit for the
  adjacent `Dockerfile.dockerignore`. No duplicate Dockerfile is kept under
  `deploy/kubernetes/images/`.
- `cube-shim` is built exactly like CI: context = repository root, file =
  `CubeShim/Dockerfile` (multi-stage `cargo build --release --locked` via
  `CUBE_BUILDER_IMAGE`, packages `containerd-shim-cube-rs` + `cube-runtime` +
  `conf/config-cube.toml`), with `CUBE_VERSION` / `CUBE_COMMIT` /
  `CUBE_BUILD_TIME`. Requires BuildKit for the adjacent
  `Dockerfile.dockerignore`. No duplicate Dockerfile is kept under
  `deploy/kubernetes/images/`.
- `cube-kernel` is built from pre-built Release (or local) vmlinux artifacts:
  context stages `artifacts/vmlinux-bm` (required) and `artifacts/vmlinux-pvm`
  (required on amd64, optional on arm64); file =
  `deploy/kubernetes/images/cube-kernel/Dockerfile`. Same as CI
  `release-docker-images.yml` (multi-arch; arm64 is BM-only when no PVM asset).
- `cube-guest` is built from pre-built Release (or local) guest rootfs
  artifacts: context stages `package/cube-image/` (`cube-guest-image-cpu.img`,
  `version`, `agent-version`); file =
  `deploy/kubernetes/images/cube-guest/Dockerfile`. Same as CI
  `release-docker-images.yml` (downloads `cube-guest-image-${arch}.tar.gz`).
- `cube-api` is built exactly like CI (`.github/workflows/release-docker-images.yml`):
  context = `CubeAPI`, file = `CubeAPI/Dockerfile`, with
  `CUBE_VERSION` / `CUBE_COMMIT` / `CUBE_BUILD_TIME`. No duplicate Dockerfile is
  kept under `deploy/kubernetes/images/`.
- `cube-ops` is built from `CubeOps/Dockerfile` with context = repository root
  (needs sibling `CubeDB/` via `CubeOps/Dockerfile.dockerignore`); same as CI
  `release-docker-images.yml`. No duplicate Dockerfile is kept here.
- `cubemastercli` is built exactly like CI (`.github/workflows/release-docker-images.yml`):
  context = repository root, file = `CubeMaster/docker/Dockerfile.cubemastercli`,
  with `CUBE_VERSION` / `CUBE_COMMIT` / `CUBE_BUILD_TIME`. Requires BuildKit for
  the adjacent `Dockerfile.cubemastercli.dockerignore`. No duplicate Dockerfile
  is kept under `deploy/kubernetes/images/`. It is separate from `cube-master`
  and `cube-node` so runtime image responsibilities remain clean.
- `cube-proxy` is built from `CubeProxy/Dockerfile`; no duplicate Dockerfile is kept here. Auto-pause/resume is **not** baked into this image — use the standalone `cube-lifecycle-manager` image instead of the retired `cube-proxy-sidecar`.
- `cube-lifecycle-manager` is built from `cube-lifecycle-manager/Dockerfile`; no duplicate Dockerfile is kept here.
- `cube-egress` is built from `CubeEgress/Dockerfile`; no duplicate Dockerfile is kept here. Its `cube-egress/openresty:1.29.2.5-tproxy` base image is built first from `CubeEgress/openresty/Dockerfile`, because that patched OpenResty base is part of the upstream CubeEgress build chain rather than a public pull-only dependency.
  The build script also tags that local base as
  `cube-sandbox-cn.tencentcloudcr.com/cube-sandbox/openresty-tproxy`, matching
  the upstream `CubeEgress/Dockerfile` `FROM` line, so the final egress image
  uses the just-built base instead of pulling a drifting external image.
- `cube-egress-net` is a Kubernetes helper image that owns the host TPROXY
  iptables/ip-rule setup for CubeEgress. It packages the upstream
  `CubeEgress/scripts/cube-proxy-iptables-init.sh` plus a small idempotent
  entrypoint that waits for `cube-dev`, applies rules, and removes them on
  termination.
- `cube-webui` is built exactly like CI (`.github/workflows/release-docker-images.yml`):
  context = repository root, file = `deploy/one-click/webui/Dockerfile`, with
  `OPENRESTY_BASE_IMAGE` / `CUBE_VERSION` / `CUBE_COMMIT` / `CUBE_BUILD_TIME`.
  Requires BuildKit (`DOCKER_BUILDKIT=1`) for the adjacent
  `Dockerfile.dockerignore`. The chart may still mount a ConfigMap nginx.conf
  over the image default at runtime.

The Helm chart stays under `deploy/kubernetes/chart`; image build logic stays here to avoid coupling chart templates with image construction.

`build-cube-images.sh` copies only the scripts required by each image into that image's build context. Do not add generic helper scripts here unless they are referenced by a Dockerfile or explicitly copied by the build script.

CubeMaster runtime layout matches one-click under `/usr/local/services/cubetoolbox/CubeMaster/` (`bin/cubemaster`, `plugin/`, `conf.yaml`). Runtime configuration is delivered by the Helm chart from `deploy/kubernetes/chart/files/cube-master/conf.yaml` as a Secret mounted at `/usr/local/services/cubetoolbox/CubeMaster/conf.yaml`. CubeMaster schema migrations are embedded in the `cubemaster` binary at compile time from `CubeMaster/pkg/base/dao/migrate/migrations/mysql`; this image build does not package a second SQL copy.
