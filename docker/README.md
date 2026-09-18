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
[`.github/workflows/build-envd-base-image.yml`](../.github/workflows/build-envd-base-image.yml).

The default data plane is [`cube-envd`](../cube-envd/) (this repo),
installed as `/usr/bin/envd`. The upstream Go envd from
[`e2b-dev/infra`](https://github.com/e2b-dev/infra) at tag `2026.16`
(override via `workflow_dispatch` input `envd_ref`) is still compiled and
shipped as `/usr/bin/envd-go`, so a deployment can roll back at runtime by
setting `ENVD_BIN=/usr/bin/envd-go` — no rebuild needed. Building with
`--build-arg ENVD_IMPL=go` flips the image default back to Go envd.
The build context is the repo root (not `docker/`), since the cube-envd
sources live in the repo; both implementations are built on native amd64 and
arm64 runners, then the per-arch images are combined into one tag.

Minimal consumer example:

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16
RUN pip install pandas
```

Full user-facing tutorial (path A vs path B, entrypoint contract,
troubleshooting) lives in the Cube docs site:

- English: [Custom Template Images](../docs/guide/tutorials/bring-your-own-image.md)
- 中文：[自定义模板镜像](../docs/zh/guide/tutorials/bring-your-own-image.md)
