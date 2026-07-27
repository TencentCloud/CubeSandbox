# Deno 2 Sandbox Template

[中文](README_zh.md)

A reproducible Deno 2 + TypeScript runtime template for CubeSandbox. The
example builds an `amd64` image, starts a small file-backed HTTP service through
the Python SDK, and verifies that both application state and the Deno
dependency cache survive a sandbox pause/resume cycle. Both SDK scripts deny
outbound Internet access by default and require CubeProxy's per-sandbox traffic
token for service requests.

## What is included

- Deno `2.8.1`, pinned and downloaded from the official release assets
- `linux/amd64` image builds with official Deno release checksum validation
- Exact JSR versions plus a committed `deno.lock`
- Dependencies cached during the image build; runtime does not need to fetch
  packages
- A non-root `user` runtime and narrowly scoped Deno file/network permissions
- `/health` and file-backed `/counter` HTTP endpoints on port `8000`
- Format, lint, type-check, persistence, concurrency, and HTTP behavior tests
- Python SDK scripts for a normal run and a pause/resume recovery check

The Deno service starts on demand from the SDK examples. Cube's inherited
`envd` service remains the template readiness target on port `49983`, so the
template does not depend on the demo application being started during boot.

## Use cases

- Isolated, version-pinned Deno 2 execution for TypeScript or JavaScript
- Code execution whose dependencies are preloaded at build time and whose
  runtime must not access the public Internet
- Stateful Web tasks that pause while idle and continue with their memory and
  filesystem state after resume
- A reusable runtime foundation for agents or workflow systems without binding
  the template to one agent framework

## Architecture

```text
Python example
  |-- CubeSandbox SDK --> Cube API --> Deno MicroVM
  |                                    |-- envd :49983
  |                                    |-- Deno service :8000
  |                                    |     |-- GET  /health
  |                                    |     |-- GET  /counter
  |                                    |     `-- POST /counter
  `-- HTTPS via CubeProxy --------------------------^

/workspace/deno-app/data/counter.json   application state
/home/user/.cache/deno                  frozen dependency cache
```

## Prerequisites

- A working CubeSandbox deployment and `cubemastercli`
- Docker with BuildKit (or another OCI-compatible builder)
- A registry reachable by the CubeSandbox nodes
- Python 3.10 or newer for the host-side examples

Follow the [CubeSandbox quick start](../../docs/guide/quickstart.md) first if
the control plane and KVM/PVM nodes are not running yet.

## 1. Build and verify the image

Run these commands from the repository root:

```bash
docker build \
  --tag deno-sandbox:2.8.1 \
  examples/deno-sandbox

docker run --rm \
  --user 1000:1000 \
  --entrypoint deno \
  deno-sandbox:2.8.1 \
  task verify
```

`deno task verify` checks formatting, lint rules, types, and five focused
tests. The Docker build runs the same command and fails if any check fails.

To exercise the service locally:

```bash
docker run --rm --detach \
  --name cube-deno-local \
  --user 1000:1000 \
  --publish 8000:8000 \
  --entrypoint deno \
  deno-sandbox:2.8.1 \
  task start

curl --fail http://127.0.0.1:8000/health
curl --fail --request POST http://127.0.0.1:8000/counter
curl --fail http://127.0.0.1:8000/counter

docker stop cube-deno-local
```

To build and push the image for the platform currently published by the
official Cube base image:

```bash
docker buildx build \
  --platform linux/amd64 \
  --tag <your-registry>/deno-sandbox:2.8.1 \
  --push \
  examples/deno-sandbox
```

The pinned `cubesandbox-base:2026.16` image currently publishes only
`linux/amd64`. The Dockerfile also selects the official `aarch64` Deno asset
when `CUBE_BASE_IMAGE` is overridden with an arm64-compatible Cube base, but an
arm64 image cannot be produced from the default base tag until that tag gains
an arm64 manifest.

## 2. Register the Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/deno-sandbox:2.8.1 \
  --alias deno-2-sandbox \
  --writable-layer-size 1G \
  --cpu 2000 \
  --memory 2000 \
  --expose-port 8000 \
  --expose-port 49983 \
  --probe 49983 \
  --probe-path /health
```

Record the returned template ID. Port `49983` is the inherited `envd`
readiness endpoint; port `8000` is the Deno application routed by CubeProxy.

### Resource recommendations

The following baseline is used to validate this example:

| Resource | Recommended value | Notes |
|---|---:|---|
| CPU | `2000` millicores (2 cores) | Sufficient for verification and a light HTTP service; raise it for concurrent compilation or execution |
| Memory | `2000` MB | Covers Deno, envd, and MicroVM overhead; raise it for large dependency graphs |
| Writable layer | `1G` | Suitable for example code, cached dependencies, and small state; increase it for long-running or file-heavy tasks |
| Exposed ports | `8000`, `49983` | Demo application and platform readiness probe, respectively |

`--alias deno-2-sandbox` supplies a stable kebab-case template name. The scripts
still recommend the returned template ID to avoid alias collisions between
environments.

## 3. Configure the Python examples

```bash
cd examples/deno-sandbox
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
cp .env.example .env
```

Edit `.env`:

```dotenv
E2B_API_URL=http://<cube-api-host>:3000
E2B_API_KEY=e2b_000000
CUBE_TEMPLATE_ID=<template-id>
```

Cube's default local key only needs to be non-empty. For a production
deployment, use the credentials and TLS configuration required by that
deployment. If CubeProxy uses a private CA, set `REQUESTS_CA_BUNDLE` or
`SSL_CERT_FILE`; the examples never disable certificate verification.

## 4. Run the smoke test

```bash
python run_example.py
```

The script creates a sandbox, checks the pinned Deno runtime, runs the full
Deno verification task inside the MicroVM, starts the service, performs two
increments, and proves that a later read returns the persisted value. It also
proves that a public TCP connection is blocked and that CubeProxy rejects a
request without the per-sandbox traffic token. The shared helper attaches that
token to normal requests automatically. A successful run ends with output
similar to:

```text
Default-deny egress PASS: public egress blocked
Service ready (pid=...): {'status': 'ok', 'runtime': 'deno', ...}
Restricted public access PASS: HTTP 403 without token
Counter persistence PASS: {'counter': 2}
Restricted counter URL (traffic token required): https://<cubeproxy-host>/counter
```

Use `--template` to override `CUBE_TEMPLATE_ID`, or `--poll-timeout` when a
remote deployment needs a longer application startup window.

## 5. Verify pause/resume recovery

```bash
python resume_example.py
```

This second script:

1. creates a sandbox and increments the counter;
2. confirms `/home/user/.cache/deno` contains at least one file, then hashes the
   complete cache;
3. pauses the sandbox and reconnects by sandbox ID;
4. retains the create-time traffic token and uses the original handle for the
   resumed data plane;
5. compares the counter and dependency-cache hash after resume;
6. performs another write, then always kills the sandbox in `finally`.

Expected final lines:

```text
State restore PASS: {'counter': 1}
Dependency cache restore PASS: <sha256>
Post-resume write PASS: {'counter': 2}
Sandbox <id> killed.
```

Pause/resume preserves the MicroVM filesystem and processes. Killing a
sandbox is terminal and is not a persistence mechanism.

## Security and reproducibility

- The release number, JSR package versions, and transitive lockfile hashes are
  pinned. Update `deno.json` and `deno.lock` together, then rebuild the image.
- The image validates the Deno release asset against its matching official
  `.sha256sum` before installing the binary.
- The service runs as UID `1000` and receives network access only for
  `0.0.0.0:8000`, plus read/write access only to its data directory.
- Both SDK scripts set `allow_internet_access=False`, so the Deno runtime cannot
  reach the public Internet by default.
- Both SDK scripts set `network={"allow_public_traffic": False}`. CubeProxy
  requires the temporary `e2b-traffic-access-token` on every request, and the
  helper never logs that token.
- No API secrets are baked into the image. `.env` and local virtual
  environments are ignored by Git and excluded from the Docker context.
- The demo API intentionally has no application-level user authentication. The
  platform traffic token protects access to the sandbox, but production use
  still needs authentication and authorization for the application's identity
  model.

## Known limitations

- The official `cubesandbox-base:2026.16` image currently has only a
  `linux/amd64` manifest, so the default build cannot produce an arm64 image.
- `/counter` is a single-process file store whose writes are serialized only
  inside the current Deno process. It is not a database and is unsuitable for
  multi-process or high-throughput production workloads.
- The application is started on demand by the SDK scripts. Template readiness
  proves that `envd` is available, not that port `8000` is already listening.
- Pause/resume depends on the deployed version and node backend. TCP connections
  to external peers are not guaranteed to survive a pause and must reconnect.
- The platform traffic token does not replace application user authentication;
  the endpoints are intended only to verify template behavior.
- A `Sandbox.connect()` response does not return the create-time traffic token.
  Callers resuming a restricted sandbox must retain that token securely. The
  example keeps the original sandbox handle so the token is never written to
  disk.

## Troubleshooting

| Symptom | Check |
|---|---|
| Template readiness times out | Probe `49983/health`, not the on-demand Deno port; confirm the image is based on `cubesandbox-base`. |
| `Template not found` | Run `cubemastercli tpl list` and update `CUBE_TEMPLATE_ID`. |
| HTTPS certificate verification fails | Point `REQUESTS_CA_BUNDLE` or `SSL_CERT_FILE` at CubeProxy's private CA. Do not set `verify=False`. |
| Deno service does not become ready | Inspect `/tmp/cube-deno-app.log`; the Python helper includes its last 80 lines on timeout. |
| No `traffic_access_token` is returned | Confirm CubeMaster/CubeProxy support restricted public access and keep `allow_public_traffic=False` in the scripts. |
| Dependency download occurs at runtime | Confirm `deno.lock` is committed, rebuild the image, and keep `--frozen` in all tasks. |
| Pause/resume is unavailable | Confirm that the deployed CubeSandbox version and node backend support pause/resume. |

## Files

```text
deno-sandbox/
|-- Dockerfile             pinned, checksum-verified runtime image
|-- deno.json              tasks, permissions, and exact JSR imports
|-- deno.lock              frozen transitive dependency graph
|-- main.ts                HTTP service and file-backed store
|-- main_test.ts           Deno behavior and concurrency tests
|-- common.py              shared Cube SDK and HTTP helpers
|-- run_example.py         create/verify/serve smoke test
|-- resume_example.py      pause/resume state and cache verification
|-- tests/test_common.py   host-side helper unit tests
|-- requirements.txt       Python dependencies
`-- .env.example           local configuration template
```
