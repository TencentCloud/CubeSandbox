# CubeSandbox delivery images

This directory contains image build definitions used by the Kubernetes/TKE chart.

## Build entrypoint

```bash
PUSH=1 REGISTRY=ccr.ccs.tencentyun.com/cubesandbox-chart IMAGE_TAG=v0.5.1 ./deploy/kubernetes/images/build-cube-images.sh
```

Use `NO_CACHE=1` when every Docker image layer must be rebuilt instead of
using Docker's build cache:

```bash
NO_CACHE=1 PUSH=1 REGISTRY=ccr.ccs.tencentyun.com/cubesandbox-chart IMAGE_TAG=v0.5.1 ./deploy/kubernetes/images/build-cube-images.sh
```

The script defaults its temporary `BUILD_ROOT` to
`/tmp/cube-kubernetes-images-<version>` so large image contexts and downloads do
not land in the Git worktree. Override `BUILD_ROOT` only when you intentionally
want a different cache location.

The script reuses valid artifacts already present under `${BUILD_ROOT}/downloads`
and does not require a `.complete` marker.

## Pinning source to a release tag

`cube-api`, `cube-proxy`, `cube-egress`, `cube-lifecycle-manager`, and
`cube-webui` are compiled from repository source (rather than binaries in the
release tarball). By default the script pins those source trees to
`${SOURCE_REF}` (defaulting to `${VERSION}`, so `v0.5.1` for the default
build). It exports `CubeMaster/`, `CubeAPI/`, `CubeProxy/`, `CubeEgress/`,
`cube-lifecycle-manager/`, `web/`, and `deploy/one-click/webui/` at that git
ref into `${BUILD_ROOT}/source-tree/` via `git archive` and points `REPO_ROOT`
there for the duration of the build. This guarantees the images match the
release tag even when the current worktree is ahead of it.

To build from the current worktree instead (typically for development), set
`SOURCE_REF=""`:

```bash
SOURCE_REF="" PUSH=1 REGISTRY=<...> IMAGE_TAG=v0.5.1-dev \
  ./deploy/kubernetes/images/build-cube-images.sh
```

To build from a different ref (branch, tag, or commit SHA):

```bash
SOURCE_REF=some-feature-branch PUSH=1 REGISTRY=<...> IMAGE_TAG=v0.5.1-featureX \
  ./deploy/kubernetes/images/build-cube-images.sh
```

When the release package is older than the verified Kubernetes node runtime,
build `cube-node` by rebasing a known-good node image and copying the current
entrypoint into it:

```bash
CUBE_NODE_BASE_IMAGE=ccr.ccs.tencentyun.com/pavleli/cube-node:v0.4.0-cubevsfix-20260627 \
  PUSH=1 REGISTRY=ccr.ccs.tencentyun.com/cubesandbox-chart IMAGE_TAG=v0.5.1 \
  ./deploy/kubernetes/images/build-cube-images.sh
```

This keeps the CubeVS/network-agent runtime fix while preserving the chart-side
entrypoint behavior.

## Image source policy

- `cube-node` continues to use `deploy/kubernetes/images/cube-node/Dockerfile`.
  It is a Kubernetes delivery image that bundles the node-side runtime components required by the Cube Node Big Pod, including `Cubelet`, `network-agent`, `cube-shim`, `cube-kernel-scf`, `cube-image`, `cube-vs`, and `cube-snapshot`. `cube-egress` is intentionally not bundled in this image because it is delivered as a separate sidecar image.
  If `CUBE_NODE_BASE_IMAGE` is set, the build script rebases that image instead
  and only replaces `/usr/local/bin/cube-node-entrypoint.sh`.
- `cube-node-init` and `cube-pvm-host-bootstrap` run on the **`cube-node-bootstrap`** DaemonSet (REV3.2; not Big Pod init).
- `cube-wait-node-prep` is the Big Pod `wait-node-prep` **sidecar** (Kruise container launch priority) and the bootstrap `write-node-prep-ready` hold container. Bumping only the wait **image** on Big Pod may InPlace; do not change wait env/mounts routinely.
- `cube-master` is built directly from `CubeMaster/docker/Dockerfile`. The build script prepares a temporary Docker context with the release-package `cubemaster` binary and the `CubeMaster/docker/tools` directory expected by that Dockerfile.
- `cube-api` is built from `CubeAPI/Dockerfile`; no duplicate Dockerfile is kept here.
- `cubemastercli` is an operational CLI image. It packages only the
  release-package `CubeMaster/bin/cubemastercli` binary and minimal runtime
  dependencies. It is separate from `cube-master` and `cube-node` so runtime
  image responsibilities remain clean.
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
- The template builder sidecar uses a dind image by default; no duplicate Dockerfile is kept here.

The Helm chart stays under `deploy/kubernetes/chart`; image build logic stays here to avoid coupling chart templates with image construction.

`build-cube-images.sh` copies only the scripts required by each image into that image's build context. Do not add generic helper scripts here unless they are referenced by a Dockerfile or explicitly copied by the build script.

CubeMaster runtime configuration is delivered by the Helm chart from `deploy/kubernetes/chart/files/cube-master/conf.yaml` as a Secret mounted at `/usr/local/services/cubemaster/conf.yaml`. CubeMaster schema migrations are embedded in the `cubemaster` binary at compile time from `CubeMaster/pkg/base/dao/migrate/migrations/mysql`; this image build does not package a second SQL copy.
