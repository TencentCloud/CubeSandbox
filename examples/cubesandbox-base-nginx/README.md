# cubesandbox-base-nginx demo

[中文](README_zh.md)

A minimal image that stacks nginx on top of [`cubesandbox-base`](../../docker/Dockerfile.cube-base),
so you can test the "Custom Template Images" flow end-to-end without any
real application.

- envd listens on `:49983` (Cube readiness probe) — inherited from the base image.
- nginx listens on `:80` and serves a tiny static page.

See [Custom Template Images](../../docs/guide/tutorials/bring-your-own-image.md)
for the full tutorial.

## Build

```bash
docker build -t cubesandbox-demo-nginx:latest .
```

## Run & verify locally

```bash
docker run --rm -d \
    -p 8080:80 \
    -p 49983:49983 \
    --name cube-demo-nginx \
    cubesandbox-demo-nginx:latest

# nginx: should print the demo landing page HTML
curl -s http://127.0.0.1:8080/

# envd readiness probe: should return 204
curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
    http://127.0.0.1:49983/health

docker rm -f cube-demo-nginx
```

## Register as a Cube template

Push the image to a registry reachable by the Cube cluster, then run:

```bash
cubemastercli tpl create-from-image \
    --image       <your-registry>/cubesandbox-demo-nginx:latest \
    --writable-layer-size 1G \
    --expose-port 49983 \
    --expose-port 80 \
    --probe       49983 \
    --probe-path  /health
```

`--probe 49983 --probe-path /health` points Cube at envd (guaranteed to
return `204` when ready); nginx's `:80` stays exposed for your actual
traffic.

## Try it with the E2B SDK

After registering the template, [`test_files.py`](./test_files.py)
boots a sandbox from it and does two things:

1. reads `/etc/nginx/nginx.conf` via `sandbox.files.read(...)`
2. sends an HTTPS request to the sandbox's port `80` and prints the
   nginx response

```bash
pip install -r requirements.txt

cp env.example .env
# fill in E2B_API_URL and CUBE_TEMPLATE_ID

python3 test_files.py
```

## Explicit Rust selection

Go remains the default. From the repository root, build the optional Rust base
and select it with the existing example argument:

```bash
make cube-base-rust CUBE_BASE_RUST_IMAGE=cubesandbox-base:rust-local
docker build --build-arg CUBE_BASE_IMAGE=cubesandbox-base:rust-local \
  -t cubesandbox-demo-nginx:rust-local examples/cubesandbox-base-nginx
```

Rust uses writable cgroup v2 for its additional process resource groups. If
initialization fails, including in ordinary Docker with read-only cgroups, it
logs the reason and continues without those additional groups, matching Go.
Existing VM/platform limits remain separate. Test both paths with the isolated
image tests in [docker/README.md](../../docker/README.md); do not grant access to
host cgroups or the host clock to make the probe pass.
The entrypoint supervises envd and nginx under tini; nginx is not PID 1.
Template readiness probes **49983/health**, which must return 204; port 80 only
checks nginx. Startup duration depends on the platform and image.

For actual template creation, daemon identity, CubeSandbox SDK command/file
assertions and the optional Go/Rust timing comparison, see the
[existing SDK E2E entry points](../../tests/e2e/sdk_compat/README.md#selected-envd-acceptance).
A local Docker tag works only when the template builder can access that same
Docker image store; it is not automatically available on remote build nodes.
