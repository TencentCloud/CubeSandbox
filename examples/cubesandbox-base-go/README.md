# cubesandbox-base-go demo

A minimal image that stacks a **Go toolchain** and a tiny HTTP server on top of
[`cubesandbox-base`](../../docker/Dockerfile.cube-base), so you can test the
"Bring Your Own Image" flow end-to-end with a real Go workload — and use it as
a ready-made starting point for Go-based sandboxes.

- envd listens on `:49983` (Cube readiness probe) — inherited from the base image.
- A Go HTTP server (standard library `net/http`) listens on `:8080` and serves a
  tiny landing page that echoes the Go runtime version, so you can eyeball that
  Go really served the request.

See [Bring Your Own Image (envd)](../../docs/guide/tutorials/bring-your-own-image.md)
for the full tutorial, and [`../cubesandbox-base-nginx/`](../cubesandbox-base-nginx/)
for a sibling example that follows the exact same pattern with nginx.

## What's inside

- **Go toolchain** — official tarball from `go.dev`, newer than Ubuntu 22.04's
  apt `golang` (1.18). Installed under `/usr/local/go`, with `GOROOT`/`GOPATH`/`PATH`
  wired up so `go` is on `$PATH`.
- **`main.go`** — a single-file HTTP server built on the standard library
  `net/http` package, so the image needs **no third-party dependencies** and no
  network access to build. Compiled to `/app/helloserver` at build time and
  launched as the foreground process via `CMD ["/app/helloserver"]`.

## Build

```bash
docker build -t cubesandbox-demo-go:latest .
```

The Dockerfile runs `go test` during the build, so a failing unit test
will abort the image build.

> Pin a different Go version with `--build-arg GO_VERSION=1.22.0`.

## Run unit tests locally

```bash
go test -v .
```

Tests use only the standard library `net/http/httptest` — no external
dependencies or running server required.

## Run & verify locally

```bash
docker run --rm -d \
    -p 8080:8080 \
    -p 49983:49983 \
    --name cube-demo-go \
    cubesandbox-demo-go:latest

# Go server: should print the demo landing page (with the Go runtime version)
curl -s http://127.0.0.1:8080/

# envd readiness probe: should return 204
curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
    http://127.0.0.1:49983/health

# Sanity-check the toolchain inside the container
docker exec cube-demo-go go version
docker exec cube-demo-go go env GOROOT GOPATH

docker rm -f cube-demo-go
```

## Register as a Cube template

```bash
cubemastercli tpl create-from-image \
    --image       <your-registry>/cubesandbox-demo-go:latest \
    --writable-layer-size 1G \
    --expose-port 49983 \
    --expose-port 8080 \
    --probe       49983 \
    --probe-path  /health
```

`--probe 49983 --probe-path /health` points Cube at envd (guaranteed to
return `204` within ~1s); the Go server's `:8080` stays exposed for your
actual traffic. See
[Creating Templates from OCI Images](../../docs/guide/tutorials/template-from-image.md)
for monitoring (`tpl watch`) and troubleshooting.

## Try it with the E2B SDK

After registering the template, [`test_sandbox.py`](./test_sandbox.py) boots
a sandbox from it and verifies four things:

1. `go version` runs inside the sandbox (toolchain is on `$PATH`)
2. `go env GOROOT GOPATH` reports the expected paths
3. `/app/main.go` is readable via `sandbox.files.read(...)`
4. An HTTPS request to the sandbox's port `8080` returns the Go server's
   landing page

```bash
pip install -r requirements.txt

cp env.example .env
# fill in E2B_API_URL and CUBE_TEMPLATE_ID

python3 test_sandbox.py
```

## Files

```
cubesandbox-base-go/
├── Dockerfile              # FROM cubesandbox-base, installs Go toolchain, builds & runs the server
├── main.go                 # Standard-library HTTP server (net/http), no third-party deps
├── main_test.go            # Unit tests for the root / health handlers (go test, no external deps)
├── test_sandbox.py         # E2B SDK smoke test: go version/env, file read, HTTP GET :8080
├── env.example             # E2B_API_URL / E2B_API_KEY / CUBE_TEMPLATE_ID
├── requirements.txt        # e2b + python-dotenv
└── README.md               # This file
```

## Use cases

- **Go code execution sandbox** — boot a sandbox and run `go build` / `go test`
  via `sandbox.commands.run(...)` for isolated build or test jobs.
- **Go Web service base** — replace `main.go` with your own Gin / Echo / chi /
  vanilla `net/http` app and expose its port with `--expose-port`.
- **CGo / multi-module build runner** — clone a repo into the sandbox and run
  `go build ./...` against an isolated, disposable environment.

## Known limitations

- The image ships a **single Go version** (default `1.23.4`). Override with
  `--build-arg GO_VERSION=<x.y.z>` for a different release.
- **`GOPATH` is `/go`** with no pre-created `src` tree. Go modules are the
  default and recommended workflow; if you need legacy `GOPATH` mode, `mkdir -p
  /go/src` in your downstream Dockerfile.
- `main.go` is a demo, not a production server — it has no TLS or graceful
  shutdown. It includes basic HTTP timeouts, but production services should
  tune them for their workload. Swap it for your real application's binary.
- The Go binary is built with `CGO_ENABLED=0` (pure Go). If your project needs
  CGo (e.g. links a C library), drop that flag and ensure the toolchain image
  has a C compiler (`apt-get install -y gcc`).

## Related

- [`../cubesandbox-base-nginx/`](../cubesandbox-base-nginx/) — the sibling
  example this template is modelled on (nginx instead of Go).
- [`../cubesandbox-base-java/`](../cubesandbox-base-java/) — another sibling,
  Java 17 + Maven on the same base image.
- [Bring Your Own Image (envd)](../../docs/guide/tutorials/bring-your-own-image.md)
  — the entrypoint contract and `envd` requirements.
- [Creating Templates from OCI Images](../../docs/guide/tutorials/template-from-image.md)
  — full `cubemastercli tpl create-from-image` reference.
