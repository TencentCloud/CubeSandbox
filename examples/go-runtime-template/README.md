# Go Runtime Template

[中文文档](README_zh.md)

A Cube-ready Go runtime image built on top of
`ghcr.io/tencentcloud/cubesandbox-base:2026.16`. It is meant for general Go
development workloads: compiling small services, running tests, executing CLI
tools, and keeping a stateful workspace inside a Cube Sandbox.

This is not a Code Interpreter or Jupyter template. It exposes only `envd` on
`:49983`, which is enough for `Sandbox.commands.run()` and file APIs.

Most CubeSandbox examples show how to consume an existing template. This one
shows how to author a reusable template, so the `Dockerfile` is part of the
example: it is the template definition that `cubemastercli tpl create-from-image`
turns into a Cube template. For the generic image contract, see
[Bring Your Own Image (envd)](../../docs/guide/tutorials/bring-your-own-image.md).

## What You Get

- Go toolchain installed under `/usr/local/go`
- A sample workspace at `/workspace/hello-go`
- `envd` inherited from `cubesandbox-base`, listening on `:49983`
- `smoke.py`, an optional Cube SDK smoke test that runs `go version`,
  `go test ./...`, and `go run .` inside a sandbox

## Build the Image

```bash
docker build --platform linux/amd64 \
  -t cubesandbox-go-runtime:local \
  examples/go-runtime-template
```

Build arguments:

| Argument | Default | Purpose |
|----------|---------|---------|
| `CUBE_BASE_IMAGE` | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` | Base image with `envd` and the Cube entrypoint |
| `CUBE_PLATFORM` | `linux/amd64` | Platform used to pull `cubesandbox-base`; this base image is currently published for amd64 |
| `GO_VERSION` | `1.25.4` | Go toolchain version downloaded from `go.dev/dl` |

Example override:

```bash
docker build \
  --platform linux/amd64 \
  --build-arg GO_VERSION=1.25.4 \
  -t cubesandbox-go-runtime:local \
  examples/go-runtime-template
```

## Verify Locally

```bash
docker run --rm -d \
  --platform linux/amd64 \
  -p 49983:49983 \
  --name cube-go-runtime \
  cubesandbox-go-runtime:local

curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
  http://127.0.0.1:49983/health

docker exec cube-go-runtime sh -lc \
  'go version && cd /workspace/hello-go && go test ./... && go run .'

docker rm -f cube-go-runtime
```

Expected highlights:

```text
envd /health => 204
ok  	example.com/cubesandbox/hello-go
hello cube from Go inside CubeSandbox
```

## Register as a Cube Template

Push the image to a registry reachable by your Cube cluster, then create the
template:

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-go-runtime:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

Watch the job until it reaches `template_status: READY`:

```bash
cubemastercli tpl watch --job-id <job-id>
```

## Optional SDK Smoke

```bash
pip install -r requirements.txt

cp env.example .env
# edit .env and fill in E2B_API_URL and CUBE_TEMPLATE_ID

python smoke.py
```

The script creates a sandbox from `CUBE_TEMPLATE_ID` and runs:

```bash
go version
cd /workspace/hello-go && go test ./...
cd /workspace/hello-go && go run .
mkdir -p /workspace/runtime-smoke && printf '%s\n' go-runtime-ok > /workspace/runtime-smoke/marker.txt
cat /workspace/runtime-smoke/marker.txt
```

## Resource Suggestions

- Smoke tests: 1 vCPU, 1 GiB memory, 2 GiB writable layer
- Real module builds: 2+ vCPU, 2-4 GiB memory, larger writable layer for
  module cache and build artifacts
- Long-lived development sessions should use Cube snapshot or pause/resume
  flows from the quickstart examples instead of rebuilding dependencies every
  turn

## Network and Security Notes

- The included sample has no external Go dependencies, so runtime smoke works
  without outbound package access.
- Image build requires access to `ghcr.io`, Ubuntu APT mirrors such as
  `archive.ubuntu.com` and `security.ubuntu.com`, and `go.dev`.
- Real projects that run `go get` or download modules need CubeEgress rules for
  the domains they use, commonly `proxy.golang.org`, `sum.golang.org`, and the
  relevant source hosts.
- Do not bake API keys or source credentials into the image. Pass secrets at
  sandbox creation time or through your platform secret mechanism.
- This template does not require privileged containers, Docker socket mounts, or
  host mounts.

## Known Limitations

- It does not expose a web app port by default. Add and expose your own service
  port if your Go workload serves HTTP.
- It does not include Jupyter or the `e2b-code-interpreter` runtime.
- The default image is optimized as a reusable starting point, not as a minimal
  production Go container.

## Cleanup

```bash
docker rm -f cube-go-runtime 2>/dev/null || true
docker rmi cubesandbox-go-runtime:local

# If you registered a template:
cubemastercli tpl delete --template-id <template-id>
```
