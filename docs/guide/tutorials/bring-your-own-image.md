# Bring Your Own Image (envd)

This tutorial shows how to add `envd` to **your own application or container image** for use with the CubeSandbox SDK and E2B SDK.

For the general workflow to create templates from OCI images and configure application ports and readiness probes, see [Create Templates from OCI Image](./template-from-image.md).

---

## 1. When does my image need `envd`?

`envd` is the data-plane service that the CubeSandbox SDK and E2B SDK use for sandbox operations such as running commands, reading and writing files, and opening PTY sessions:

| Capability | `envd` interface inside the sandbox | Without `envd` |
| --- | --- | --- |
| `envd` health check (can be used as the template probe) | `GET :49983/health` → 204 | This probe endpoint is unavailable |
| `Sandbox.commands.run()` | Process API on `:49983` | Command APIs are unavailable |
| `Sandbox.files.read/write()` | Files API on `:49983` | File APIs are unavailable |
| Create-time environment variable initialization | `POST :49983/init` | Sandbox creation with environment variables fails |

For interactive development and code-execution sandboxes, keeping `envd` is recommended so you can use the SDK to run commands, work with files, and troubleshoot the sandbox. An image that only serves its own application and does not use these capabilities can omit `envd`; configure its template probe to use the application's own HTTP health endpoint.

## 2. Quick start: build on top of `cubesandbox-base`

`cubesandbox-base` is a plain `ubuntu:22.04` with `envd` preinstalled at
`/usr/bin/envd` and a generic entrypoint that runs `envd` in the
background while honoring any `CMD` you supply. Three steps get you to a
working template: **write a Dockerfile → build and push → create the
template**.

> Prefer to read a runnable end-to-end example? See
> [`examples/cubesandbox-base-nginx`](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/cubesandbox-base-nginx)
> in the repo — a minimal demo that stacks nginx on top of
> `cubesandbox-base`.

### 2.1 Write a Dockerfile

```dockerfile
FROM ghcr.io/tencentcloud/cubesandbox-base:2026.16

# Install your own tooling
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir pandas matplotlib numpy

# Optional: if your app needs to be the foreground process, set CMD here.
# envd stays alive in the background.
# CMD ["python3", "/srv/app.py"]
```

### 2.2 Build and push

```bash
docker build -t my-registry.example.com/my-team/my-sandbox:v1 .
docker push   my-registry.example.com/my-team/my-sandbox:v1
```

The registry must be reachable from your Cube cluster.

::: tip Plain HTTP registry
Prefix the image with `http://`, for example `http://my-registry.example.com/my-team/my-sandbox:v1`.
:::

### 2.3 Create a Cube template

Expose `49983` (envd) plus whatever ports your own application listens on:

```bash
cubemastercli tpl create-from-image \
  --image       my-registry.example.com/my-team/my-sandbox:v1 \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --expose-port <your-custom-port> \
  --probe       49983 \
  --probe-path  /health
```

Once you have a `template_id`, you can use the CubeSandbox SDK or E2B SDK to create sandboxes from it. See [Create Templates from OCI Image](./template-from-image.md) for an example.

### Available base image tags

| Tag                       | Base OS       | envd version |
| ------------------------- | ------------- | ------------ |
| `2026.16` / `latest`      | `ubuntu:22.04` | `2026.16`    |
| `2026.16-ubuntu22.04`     | `ubuntu:22.04` | `2026.16`    |

Pin the exact envd version (`2026.16`) for reproducible builds.

## 3. Alternative: inject `envd` into an existing image

When you want to bring your own custom image, copy `envd` and the
entrypoint **out of** `cubesandbox-base` with a `COPY --from=` stage:

```dockerfile
FROM e2bdev/code-interpreter:latest

USER root

# Pull envd and the generic entrypoint from cubesandbox-base.
COPY --from=ghcr.io/tencentcloud/cubesandbox-base:2026.16 \
     /usr/bin/envd /usr/bin/envd
COPY --from=ghcr.io/tencentcloud/cubesandbox-base:2026.16 \
     /usr/local/bin/cube-entrypoint.sh /usr/local/bin/cube-entrypoint.sh

# The upstream image already has its own entrypoint/CMD. Either wrap it
# with cube-entrypoint.sh (preferred), or start envd manually from your
# own script — see section 4 for the manual pattern.
ENTRYPOINT ["/usr/local/bin/cube-entrypoint.sh"]
CMD ["/bin/sh", "-c", "sudo --preserve-env=E2B_LOCAL /root/.jupyter/start-up.sh"]
```

A second example, starting from a slim Python image:

```dockerfile
FROM python:3.11-slim

COPY --from=ghcr.io/tencentcloud/cubesandbox-base:2026.16 \
     /usr/bin/envd /usr/bin/envd
COPY --from=ghcr.io/tencentcloud/cubesandbox-base:2026.16 \
     /usr/local/bin/cube-entrypoint.sh /usr/local/bin/cube-entrypoint.sh

RUN pip install --no-cache-dir fastapi uvicorn

COPY app.py /srv/app.py

EXPOSE 49983 8000
ENTRYPOINT ["/usr/local/bin/cube-entrypoint.sh"]
CMD ["uvicorn", "app:app", "--app-dir", "/srv", "--host", "0.0.0.0", "--port", "8000"]
```

Build, push and template creation are identical to sections 2.2 / 2.3.

## 4. The entrypoint contract

`cube-entrypoint.sh` implements a simple "envd-in-the-background, your
app in the foreground" pattern:

1. It always starts `envd -port "${ENVD_PORT:-49983}"` in the background
   so that `/health` is reachable within about a second of container
   startup.
2. If the container was started **with** a user `CMD`, the script
   `exec`s that command. `envd` keeps running in the background; the
   user process owns `stdout`/`stderr` and receives `SIGTERM` on stop.
3. If the container was started **without** a `CMD`, the script simply
   waits on `envd`, keeping it as the foreground process.

Environment variables:

| Variable           | Default             | Purpose                                              |
| ------------------ | ------------------- | ---------------------------------------------------- |
| `ENVD_PORT`        | `49983`             | Port `envd` listens on.                              |
| `ENVD_EXTRA_ARGS`  | *(empty)*           | Extra flags passed after `-port`. `-isnotfc` is appended automatically if not already present, to skip Firecracker MMDS lookup. |
| `ENVD_LOG_FILE`    | `/var/log/envd.log` | File that captures envd stdout/stderr. Use `-` to inherit the container stdio. |
| `ENVD_BIN`         | `/usr/bin/envd`     | Override if you install envd elsewhere.              |

### Starting envd manually

If you already have a non-trivial entrypoint of your own and don't want
to delegate to `cube-entrypoint.sh`, just add one line before handing
control to your main process:

```bash
#!/bin/bash
# your-entrypoint.sh

# Start envd in the background.
# -isnotfc is REQUIRED: it tells envd to skip the Firecracker MMDS lookup
# at 169.254.169.254. CubeSandbox does not use Firecracker, so the MMDS
# service does not exist. Without this flag envd will attempt to access
# the non-existent MMDS, which may cause various problems such as network
# timeouts, /init delays, or env_vars injection failures.
/usr/bin/envd -port 49983 -isnotfc >/var/log/envd.log 2>&1 &

# ... your usual startup sequence ...
exec "$@"
```

## 5. Verifying the image locally (optional)

Before creating a template you can run the same smoke test that CI runs
on the base image:

```bash
IMG=my-registry.example.com/my-team/my-sandbox:v1
cid=$(docker run -d --rm "$IMG")

docker exec "$cid" curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
    http://127.0.0.1:49983/health
# => envd /health => 204

docker exec "$cid" /usr/bin/envd -version
# => 2026.16

docker rm -f "$cid"
```

If `/health` does not reach `204` within a few seconds, inspect
`/var/log/envd.log` inside the container:

```bash
docker exec "$cid" cat /var/log/envd.log
```

## 6. Troubleshooting

| Symptom                                       | Likely cause                                                          | Fix                                                                                 |
| --------------------------------------------- | --------------------------------------------------------------------- | ----------------------------------------------------------------------------------- |
| Template creation fails the readiness probe   | envd did not start / started on the wrong port                        | Ensure `ENTRYPOINT` invokes `cube-entrypoint.sh` **or** your own script runs `envd -port 49983 &` before `exec`. |
| `curl :49983/health` returns `000`            | Nothing is listening; entrypoint replaced                             | Check <code v-pre>docker inspect --format '{{json .Config.Entrypoint}}'</code>; keep `cube-entrypoint.sh` as the wrapper. |
| envd exits immediately                        | Version mismatch between binary and kernel/init expectations          | Verify with `docker exec ... /usr/bin/envd -version`; re-copy from the pinned base tag. |
| envd `/init` slow or `create_time env_vars` fail | `-isnotfc` flag missing; envd attempts invalid MMDS access at `169.254.169.254` | Use `cube-entrypoint.sh` (appends `-isnotfc` automatically), or add `-isnotfc` to the command line when starting envd manually. |
| Port 49983 conflicts with your own service    | Your app also listens on 49983                                        | Move your app to a different port and expose both with `--expose-port`.             |
| `sudo: command not found` in your CMD         | You started `FROM` a `-slim` / `-alpine` image without sudo           | Either `apt-get install -y sudo`, or drop `sudo` from your entrypoint — `cube-entrypoint.sh` doesn't require it. |
| Template creation times out in `PULLING`      | Registry unreachable from Cube nodes                                  | Push to a registry the cluster can reach, or supply `--registry-username` / `--registry-password`. |

## 7. Advanced — rebuild the base image yourself

The base image is produced by a single GitHub Actions workflow in this
repository: [`.github/workflows/build-envd-base-image.yml`](https://github.com/TencentCloud/CubeSandbox/blob/master/.github/workflows/build-envd-base-image.yml).
It checks out `e2b-dev/infra` at the chosen tag (default `2026.16`),
compiles `envd` with Go 1.25.4 in-place, builds
`docker/Dockerfile.cube-base`, runs a `:49983/health` smoke test, then
pushes to `ghcr.io/tencentcloud/cubesandbox-base`.
