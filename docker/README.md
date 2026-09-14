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
already ready for Cube's readiness probe. The image ships **both** envd
implementations and lets the deployment pick one:

| Path | What it is |
|---|---|
| `/usr/bin/envd` | the selected implementation — `cube-envd` by default |
| `/usr/bin/envd-go` | the upstream Go envd, built from `e2b-dev/infra@ENVD_REF` |
| `/etc/cubesandbox-envd-impl` | `cube` or `go`: which one `/usr/bin/envd` is |
| `/etc/cubesandbox-envd-ref` | the upstream ref the Go binary was built from |

Selection is build-time (`--build-arg ENVD_IMPL=cube|go`) or runtime
(`ENVD_BIN=/usr/bin/envd-go`, honoured by `cube-entrypoint.sh`) — the latter needs
no rebuild, which is what makes the Go envd a real rollback. Installing the
selected implementation as the literal `/usr/bin/envd` matters: Cubelet collects
the envd version by exec'ing `envd --version`, so an `ENVD_BIN` override alone
would leave the template annotated with the other implementation's version.

Published as a multi-arch (`linux/amd64` + `linux/arm64`) manifest list
`ghcr.io/tencentcloud/cubesandbox-base` by
[`.github/workflows/build-envd-base-image.yml`](../.github/workflows/build-envd-base-image.yml).
The workflow compiles `cube-envd` from [`cube-envd/`](../cube-envd/) (Rust + musl
static, version/commit injected via build args) and the Go envd from the pinned
`ENVD_REF`, then runs a smoke test that asserts the `/health` probe returns 204
for **both** `ENVD_BIN` settings, that both binaries are executable and report
their versions, and that the implementation stamp matches `ENVD_IMPL`.

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
