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
already ready for Cube's readiness probe. Built and smoke-tested on
native amd64 and arm64 runners by
[`.github/workflows/build-envd-base-image.yml`](../.github/workflows/build-envd-base-image.yml);
publishing is `linux/amd64`-only for now, with arm64 as a build + smoke
gate. The image embeds the local Rust `cube-envd` from this repository. Pass
`ENVD_IMPL=upstream-e2b` to build `Dockerfile.cube-base-upstream` and restore
the upstream Go `envd` from
[`e2b-dev/infra`](https://github.com/e2b-dev/infra) at tag `2026.16`
(override via `workflow_dispatch` input `envd_ref`).

Build locally from the repository root:

```bash
make build-cube-base-image
make smoke-cube-base-image
```

The entrypoint writes envd logs to `/var/log/envd.log` by default. Set
`ENVD_LOG_FILE=-` to send them to the container output, and use
`ENVD_LOG_LEVEL=warn` or `ENVD_LOG_FORMAT=json` to control verbosity and
format. The read-only `GET /status` endpoint reports readiness, version,
commit, port, and uptime; `GET /health` remains the `204` readiness probe.
Requests may provide `X-Request-ID`; envd echoes it in the response and adds
it to request logs, or generates a safe `cube-envd-<pid>-<sequence>` value.
When a user command is running, the entrypoint monitors envd and logs an
unexpected envd exit before terminating the user command.

The workflow dispatch input `envd_impl` selects the same two implementations.

Minimal consumer example:

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16
RUN pip install pandas
```

Full user-facing tutorial (path A vs path B, entrypoint contract,
troubleshooting) lives in the Cube docs site:

- English: [Custom Template Images](../docs/guide/tutorials/bring-your-own-image.md)
- 中文：[自定义模板镜像](../docs/zh/guide/tutorials/bring-your-own-image.md)
