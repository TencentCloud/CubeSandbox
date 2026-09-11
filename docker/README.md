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
already ready for Cube's readiness probe. `envd` is the repository's
own Rust daemon `cube-envd` (installed as `/usr/bin/envd`, command name
kept for E2B compatibility). Published as a multi-arch
(`linux/amd64` + `linux/arm64`) manifest list
`ghcr.io/tencentcloud/cubesandbox-base` by
[`.github/workflows/build-envd-base-image.yml`](../.github/workflows/build-envd-base-image.yml):
it compiles `cube-envd` from [`cube-envd/`](../cube-envd/) inside
`docker/Dockerfile.cube-base` (Rust + musl static, version/commit
injected via build args), bakes it into the image as `/usr/bin/envd`,
and runs a `:49983/health` smoke test before pushing.

The runtime stage installs `util-linux` on purpose: `cube-envd` delegates
credential switching to `setpriv` when a request selects a user other than the
one running the daemon (upstream Go `envd` did this in-process, which stable
Rust cannot). Images that copy only `/usr/bin/envd` out of this image must
provide a usable `setpriv` themselves — see
[the BYO tutorial](../docs/guide/tutorials/bring-your-own-image.md#setpriv-is-required-when-the-requested-user-differs-from-root).

Minimal consumer example:

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:latest
RUN pip install pandas
```

Full user-facing tutorial (path A vs path B, entrypoint contract,
troubleshooting) lives in the Cube docs site:

- English: [Custom Template Images](../docs/guide/tutorials/bring-your-own-image.md)
- 中文：[自定义模板镜像](../docs/zh/guide/tutorials/bring-your-own-image.md)
