# Go Sandbox

A ready-to-use **Go runtime template** for Cube Sandbox, with a minimal example
that drives it through the E2B-compatible Python SDK. Use it to compile and unit
test Go programs inside an isolated KVM MicroVM.

[中文文档](README_zh.md)

## 1. What's inside

| File | What it shows |
|------|---------------|
| `Dockerfile` | Builds the Go runtime image from `cubesandbox-base` (preinstalled `go` toolchain, envd kept on `:49983`). |
| `env_utils.py` | Shared `.env` loader — mirrors the `env_utils` helper used by other examples. |
| `go_test.py` | Write a Go module + unit test, run `go test -v`, assert pass. |
| `go_egress_restricted.py` | Run Go under a restricted egress policy: air-gapped zero-dependency build/test, proof that egress is enforced, allowlist recipe for module downloads. |

## 2. Prerequisites

- A running Cube Sandbox deployment (`cubemastercli` on `$PATH`, `CUBEMASTER_ADDR` set).
- Docker (to build/push the template image).
- Python 3.8+.

```bash
pip install -r requirements.txt
```

The script uses `python-dotenv` to best-effort load a `.env` file from this
directory or your current working directory (without overriding already-set
environment variables). Copy the template and fill it in:

```bash
cp .env.example .env
# edit .env: set E2B_API_URL and CUBE_TEMPLATE_ID
```


## 3. Build, push, and register the template

### Step 1 — Build the image

```bash
docker build -t cubesandbox-go:latest examples/go-sandbox
```

> **Cross-arch:** if your CubeMaster nodes are ARM64, add
> `--platform linux/arm64` to the `docker build` command above (the Go
> toolchain is copied from `golang:1.23.4-bookworm`, which is `amd64` by
> default).

### Step 2 — Push to a reachable registry

CubeMaster pulls the image from a registry the **cluster nodes** can reach. Use
your own Tencent Cloud Registry (TCR) namespace (or any registry your nodes can
pull from):

```bash
docker tag  cubesandbox-go:latest <your-registry>/cubesandbox-go:latest
docker push <your-registry>/cubesandbox-go:latest
```

### Step 3 — Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-go:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health
```

- `49983` is envd's readiness/probe port — **required** for the template to
  reach `READY`. Exposing it is what makes the probe (`GET /health` → 204) pass.
- `4G` writable layer: Go module + build caches are heavy; size it for your
  module graph.

Track the build:

```bash
cubemastercli tpl watch --job-id <job_id>   # exits when READY or FAILED
```

Note the `template_id` from the output.

### Step 4 — Configure environment variables

```bash
export E2B_API_KEY=e2b_000000
export E2B_API_URL=http://<your-node-ip>:3000
export CUBE_TEMPLATE_ID=<template-id>
```

## 4. Run the example

```bash
python go_test.py            # compile + unit test
```

The script reads `CUBE_TEMPLATE_ID` from the environment (or your `.env`),
connects via `E2B_API_URL` / `E2B_API_KEY`, and boots a sandbox from the Go
template.


The script prints a short `✓` line on success and a non-zero exit code on
failure.

## 5. Resource recommendations

| Setting | Suggested | Why |
|---------|-----------|-----|
| `writable-layer-size` | `4G` (more for big module graphs) | module cache (`GOPATH=/workspace/go`) + build cache (`GOCACHE=/workspace/.cache/go-build`) live in the writable layer. |
| Instance CPU/Mem | 1 vCPU / 1–2 GB minimum | `go build` is CPU-bound; tests parallelize with `GOMAXPROCS`. |

## 6. Known limitations

- **No pre-warmed official Go image.** Unlike `sandbox-code`, the Go image is
  built from this repo's `Dockerfile` (based on `cubesandbox-base`). Pin
  `GOLANG_VERSION` and the base tag for reproducibility.
- **Architecture.** The toolchain is copied from `golang:<ver>-bookworm` on the
  build host's arch. Match `--platform` to your node arch or the template boot
  may fail.
- **`go mod` needs egress.** Pure `go test`/`go build` of dependency-free code
  runs fully offline, but fetching modules requires either general internet or
  an egress allowlist.
- **`git` is not preinstalled.** To keep the image build independent of Ubuntu
  package mirrors, the `Dockerfile` does not install `git`. The example pins
  `GOPROXY=proxy.golang.org`, so modules are fetched as zips from the proxy (no
  VCS access needed). If you need `GOPROXY=direct` or private-repo VCS fetching,
  `apt-get install git` in a derived image.
- **`GOPROXY` default.** The template leaves the upstream default
  (`proxy.golang.org,direct`). For regulated environments, set `GOPROXY` to your
  private proxy and restrict egress accordingly.

## 7. Security alignment

- The template inherits the `cubesandbox-base` security baseline (envd
  auth, isolated kernel/network). No extra ports or services are opened at boot.
- No secrets are baked into the image; runtime config is passed via
  `env_vars`/`network` at `Sandbox.create` time.
- `GOTOOLCHAIN=local` is pinned in the image so the toolchain is never
  auto-downloaded from the network — a supply-chain safeguard that also makes
  air-gapped builds deterministic.

## 8. Running under restricted egress (differentiated scenario)

Go's standout property for regulated environments is that a **module with no
external dependencies compiles and unit-tests fully offline**. Cube Sandbox
enforces outbound policy at the Cubelet tap layer (kernel level), so it cannot
be bypassed from inside the VM. `go_egress_restricted.py` demonstrates this:

```bash
python go_egress_restricted.py
```

It runs three checks:

1. **Air-gapped zero-dependency build/test** — creates the sandbox with
   `allow_internet_access=False`, writes a stdlib-only module, and runs
   `go test -v`. Passes completely offline because `GOTOOLCHAIN=local` +
   `-mod=readonly` prevent any network touch.
2. **Egress is really cut** — the same air-gapped sandbox cannot
   `go mod download` an external module (`rsc.io/quote`); the command fails,
   proving the policy is enforced, not cosmetic.
3. **Allowlist recipe for deps** (opt-in) — set `CUBE_GOPROXY_CIDRS` to permit
   only your private module proxy (+ DNS) CIDR while keeping everything else
   blocked:

   ```bash
   export CUBE_GOPROXY_CIDRS="10.0.1.5/32,10.0.0.53/32"   # proxy + DNS
   export CUBE_GOPROXY_URL="http://10.0.1.5:8080"          # reachable proxy
   python go_egress_restricted.py
   ```

   With the allowlist, `go mod download` reaches only the proxy; public internet
   stays blocked. This is the regulated-environment pattern: keep `go mod` working
   via an internal proxy instead of opening general egress.

| Mode | `Sandbox.create` args | Effect |
|------|----------------------|--------|
| Air-gapped | `allow_internet_access=False` | Zero-dep Go builds/tests run; all egress blocked |
| Allowlist | `allow_internet_access=False`, `network={"allow_out":[...]}` | Only listed CIDRs (e.g. proxy+DNS) reachable |

## 9. Directory structure

```
go-sandbox/
├── Dockerfile                 # Go runtime image (cubesandbox-base + go toolchain)
├── README.md                  # This file
├── README_zh.md               # Chinese documentation
├── requirements.txt           # Python dependencies
├── env_utils.py               # shared .env loader (matches other examples)
├── .env.example               # Environment variable template
├── go_test.py                 # go test demo
└── go_egress_restricted.py    # air-gapped / allowlist egress demo
```


