# Go Dev Sandbox

[中文文档](README_zh.md)

A ready-to-use **Go toolchain template** for Cube Sandbox, plus two runnable
demos: the basic `build → run → test` loop, and a long-running job that
**checkpoints with a snapshot and resumes after a crash by rolling back**.

## When to use this

| Scenario | Why this template |
|---|---|
| An agent that writes and compiles Go code | `go build` / `go test` / `go run` work out of the box through the SDK's command API |
| Untrusted Go code from an LLM | Each sandbox is a KVM MicroVM with its own kernel — a runaway `go run` cannot touch the host |
| Long compile-and-test loops | Take a snapshot once the module cache is warm, then start every later sandbox from that snapshot |
| Multi-step jobs that must survive failures | `create_snapshot()` marks a checkpoint; `rollback()` discards a bad step and resumes from it |
| Building for many platforms at once | One sandbox per `GOOS/GOARCH` target, sharing one source tree via a read-only host mount and one `dist/` via a read-write one |

Everything the demos run is **stdlib-only**, so they also work when the
sandbox has a restrictive egress policy and cannot reach `proxy.golang.org`.

## What is in here

| File | What it is |
|---|---|
| [`Dockerfile`](./Dockerfile) | The template image: official Go on top of `cubesandbox-base` |
| [`demo.py`](./demo.py) | Upload a small module, `go build` it, run it, `go test` it |
| [`snapshot_resume.py`](./snapshot_resume.py) | Checkpoint a job at step 5, crash at step 8, roll back, finish |
| [`fanout_build.py`](./fanout_build.py) | Cross-compile a matrix of `GOOS/GOARCH` targets in parallel sandboxes over shared host mounts |
| [`env.py`](./env.py) | Shared env-var loading and command-failure checking |

## Prerequisites

- A running Cube Sandbox deployment ([Quick Start](../../docs/guide/quickstart.md))
- `cubemastercli` on `$PATH`, with `CUBEMASTER_ADDR` set
- Docker with the **buildx** component (`apt-get install docker-buildx` on
  distributions that ship the CLI without it), and a registry your Cube cluster
  can pull from
- Python 3.8+

## 1. Build and push the image

```bash
docker build -t <your-registry>/go-dev-sandbox:latest examples/go-dev-sandbox
docker push <your-registry>/go-dev-sandbox:latest
```

To pin a different Go release, pass `--build-arg GO_VERSION=1.23.6`.

Optional local sanity check before pushing — envd must answer `204`:

```bash
docker run --rm -d -p 49983:49983 --name go-dev <your-registry>/go-dev-sandbox:latest
curl -s -o /dev/null -w "envd /health => %{http_code}\n" http://127.0.0.1:49983/health
docker exec go-dev go version
docker rm -f go-dev
```

## 2. Register the template

```bash
cubemastercli tpl create-from-image \
  --image       <your-registry>/go-dev-sandbox:latest \
  --writable-layer-size 1G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health
```

The image runs no application of its own, so envd's `:49983/health` is the
readiness probe. Watch the build until it reports `READY`:

```bash
cubemastercli tpl watch --job-id <job_id>
```

Note the `template_id` — see
[Creating Templates from OCI Images](../../docs/guide/tutorials/template-from-image.md)
for the full flow.

On a slow host, keep `--writable-layer-size` at 1 GB — see
[Troubleshooting](#troubleshooting).

## 3. Configure environment variables

```bash
pip install -r requirements.txt

cp .env.example .env
# edit .env: CUBE_API_URL and CUBE_TEMPLATE_ID
```

On a single-node deployment without wildcard DNS for `*.cube.app`, also set
`CUBE_PROXY_NODE_IP` to your CubeProxy node (`127.0.0.1` when everything runs
on one box). Without it the SDK resolves `<port>-<sandbox_id>.cube.app` through
the OS resolver and cannot reach the sandbox.

## 4. Run the demos

```bash
python demo.py
```

Expected output (`GOARCH` follows the host the template was built on):

```
sandbox: ab13e8af9d2f48bf8bcb8de2dee1d67a
go version go1.24.9 linux/arm64
build ok
go1.24.9 on linux/arm64
fib(30) = 832040
ok  	cubedemo	12.504s
OK
```

The reported test time is dominated by compilation, not by `Fib` — the run
above is from a nested-virtualisation host and is on the slow end.

```bash
python snapshot_resume.py
```

Expected output:

```
sandbox: <sandbox-id>
build ok
resuming from step 0
step 1 done
...
step 5 done
job finished at step 5
checkpoint at step 5: snap-xxxxxxxx
crashed as designed (exit_code=1): fatal: step 8 crashed
dirty progress: 7
rolled back — progress: 5
resuming from step 5
step 6 done
...
step 10 done
job finished at step 10
final log:
step 1 ok
...
step 10 ok
snapshot deleted: snap-xxxxxxxx
OK
```

The point of the second demo: after the crash the sandbox held `progress=7`
and a `CORRUPTED` record. A single `rollback()` restored **both memory and
filesystem** to the step-5 checkpoint — the same sandbox ID, no re-boot, no
re-compilation — and the job finished from there.

The third demo fans a cross-compile matrix out across parallel sandboxes —
one sandbox per `GOOS/GOARCH` target — with the source tree shared read-only
and a common `dist/` shared read-write through
[host mounts](../../docs/guide/persistent-storage.md). It must run **on the
sandbox host node** (host mounts map node-local paths), and `hostPath` must
sit under an allowed prefix — `/data/shared/` by default. One-time setup:

```bash
sudo install -d -o "$(id -u)" -g "$(id -g)" /data/shared/go-fanout
python fanout_build.py
```

Expected output (order varies — the builds really run concurrently):

```
workspace: /data/shared/go-fanout/b63b15ae
targets:   linux/amd64, linux/arm64
[linux/amd64] sandbox: 5fe18673bb5d4145af7552cfb8d3f23a
[linux/arm64] sandbox: 851d5ca4dc6e4423a85df1af93c04ede
[linux/amd64] touch: cannot touch '/mnt/src/should-fail': Read-only file system
read-only enforced
[linux/arm64] build ok
[linux/arm64] runs natively: built for linux/arm64 by go1.24.9
[linux/amd64] build ok

dist/ on the host:
  hello-linux-amd64          2.2 MB
  hello-linux-arm64          2.3 MB

per-target: 642s, 610s  wall total: 642s
artifacts left in /data/shared/go-fanout/b63b15ae/dist — clean up old runs when done
OK
```

Each first build compiles the standard library for its target from a cold
`GOCACHE`, so the timings above (a slow nested-virtualisation host) are the
worst case — note the wall total still equals the slowest single target, not
the sum. Warm the cache once and snapshot, as described under
[Resource guidance](#resource-guidance), and the fan-out drops to seconds.

Pick different targets with `FANOUT_TARGETS="linux/amd64,darwin/arm64,windows/amd64"` —
one sandbox is created per entry, so scale the list to what your node can boot
concurrently. No upload or download is involved: when the run finishes, the
per-platform binaries are already sitting in `dist/` on the host. Files written
by the sandboxes arrive root-owned, since sandbox commands run as root.

## Resource guidance

| Workload | Suggested sandbox spec | Writable layer |
|---|---|---|
| Running these demos | 1 vCPU / 1 GB | 1 GB |
| Compiling a small service | 2 vCPU / 2 GB | 4 GB |
| `go build` on a large module tree | 4 vCPU / 4 GB | 8 GB+ |

The image is roughly 600 MB (594 MB measured for `linux/arm64`; the Go
distribution alone accounts for most of it). Size the writable layer for the
build cache too — `GOCACHE` grows fast across repeated builds. Snapshot a
sandbox once its module cache is populated and create later sandboxes from that
snapshot to skip the download entirely. Layers above 1 GB are fine on
bare metal but can trip the shim's fixed 10 s boot budget on slow hosts.

## Known limitations

- **`sandbox.run_code()` does not work with this template.** That API needs a
  Jupyter kernel, which only the `sandbox-code` image ships. Use
  `sandbox.commands.run()` and `sandbox.files.write()` instead — that is what
  both demos do.
- **`cgo` is disabled** (`CGO_ENABLED=0`); the image carries no C toolchain.
  Add `gcc`/`libc6-dev` to the Dockerfile if you need cgo.
- **The published base image is `linux/amd64` only.** That is a publishing gap,
  not a portability one: CI builds `cubesandbox-base` with
  `platforms: linux/amd64` (`.github/workflows/build-envd-base-image.yml`),
  while `docker/Dockerfile.cube-base` itself is architecture-neutral. On an
  `aarch64` host, build the base image yourself and point this template at it —
  no repository changes are needed:

  ```bash
  docker build -f docker/Dockerfile.cube-base -t cubesandbox-base:local docker/
  docker build --build-arg CUBE_BASE_IMAGE=cubesandbox-base:local \
    -t <your-registry>/go-dev-sandbox:latest examples/go-dev-sandbox
  ```

  That path is verified end to end on `linux/arm64`. Note that
  `Dockerfile.cube-base` uses `RUN --mount=type=cache`, so buildx is required
  for this step too.
- **Fetching modules needs egress.** `go get` and any non-stdlib import require
  `proxy.golang.org` (or your own `GOPROXY`) to be reachable — allow it in the
  sandbox's network policy, or vendor your dependencies into the image at build
  time.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Template job fails at `CREATING_TEMPLATE` with `Receive event timeout after 10000ms` | The shim allows a fixed, non-configurable 10 s for the freshly booted MicroVM to signal `VsockServerReady`. A larger writable layer pushes boot past that budget on a slow host. | Lower `--writable-layer-size` to 1 GB and retry. Measured on a nested-virtualisation host: ~9.6 s with 1 GB, timeout with 2 GB. Bare metal has ample headroom. |
| Build downloads the Go tarball for the wrong architecture | Without buildx, the legacy builder never sets `TARGETARCH`. The Dockerfile falls back to `dpkg --print-architecture`, but only BuildKit-based builds honour `--platform`. | `apt-get install docker-buildx`. The final `go version` in the image build fails loudly rather than shipping a broken image. |
| `Sandbox.create()` succeeds but commands time out | The SDK addresses sandboxes as `<port>-<sandbox_id>.cube.app` and your resolver has no wildcard record for it. | Set `CUBE_PROXY_NODE_IP` to the CubeProxy node (`127.0.0.1` on a single-node install). |
| `go build` takes minutes on the first run | Since Go 1.20 the standard library is no longer shipped pre-compiled; the first build compiles the packages it needs. | Expected. Snapshot the sandbox once `GOCACHE` is warm and create later sandboxes from that snapshot. |
| `fanout_build.py` fails with `hostPath ... is not within an allowed mount prefix` | CubeMaster restricts `hostPath` to an allowlist of directory prefixes — `/data/shared/` by default. | Keep `FANOUT_WORK_DIR` under `/data/shared/`, or extend `allowed_host_mount_prefixes` in CubeMaster's config ([Persistent Storage](../../docs/guide/persistent-storage.md)). |
