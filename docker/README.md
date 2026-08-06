# docker/

Dockerfiles used by CubeSandbox CI.

## `Dockerfile.builder`

Toolchain image used to compile CubeSandbox components (Go, Rust, kernel
tooling, etc.). Published as `ghcr.io/tencentcloud/cubesandbox-builder`
by [`.github/workflows/build-builder-image.yml`](../.github/workflows/build-builder-image.yml).

## `Dockerfile.cube-base` (+ `cube-entrypoint.sh`)

Base image for user-supplied sandbox templates. It is `ubuntu:22.04`
with `envd` preinstalled on `:49983`, so any image built `FROM` it is
already ready for Cube's readiness probe. Published as
`ghcr.io/tencentcloud/cubesandbox-base` by
[`.github/workflows/build-envd-base-image.yml`](../.github/workflows/build-envd-base-image.yml).

The default data plane is [`cube-envd`](../cube-envd/) (this repo),
installed as `/usr/bin/envd`. The upstream Go envd from
[`e2b-dev/infra`](https://github.com/e2b-dev/infra) at tag `2026.16`
(override via `workflow_dispatch` input `envd_ref`) is still compiled and
shipped as `/usr/bin/envd-go`, so a deployment can roll back at runtime by
setting `ENVD_BIN=/usr/bin/envd-go` — no rebuild needed. Building with
`--build-arg ENVD_IMPL=go` flips the image default back to Go envd.
Note the build context is the repo root (not `docker/`), since the
cube-envd sources live in the repo.

Minimal consumer example:

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16
RUN pip install pandas
```

Full user-facing tutorial (path A vs path B, entrypoint contract,
troubleshooting) lives in the Cube docs site:

- English: [Bring Your Own Image (envd)](../docs/guide/tutorials/bring-your-own-image.md)
- 中文：[自带镜像接入 (envd)](../docs/zh/guide/tutorials/bring-your-own-image.md)
