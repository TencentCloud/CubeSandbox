# Rust Runtime Template

[中文文档](README_zh.md)

A Cube-ready Rust runtime image built on top of
`ghcr.io/tencentcloud/cubesandbox-base:2026.16`. It is meant for general Rust
development workloads: compiling crates, running tests, executing CLI tools,
and keeping a stateful workspace inside a Cube Sandbox.

This is not a Code Interpreter or Jupyter template. It exposes only `envd` on
`:49983`, which is enough for `Sandbox.commands.run()` and file APIs.

Most CubeSandbox examples show how to consume an existing template. This one
shows how to author a reusable template, so the `Dockerfile` is part of the
example: it is the template definition that `cubemastercli tpl create-from-image`
turns into a Cube template. For the generic image contract, see
[Bring Your Own Image (envd)](../../docs/guide/tutorials/bring-your-own-image.md).

## What You Get

- Rust toolchain installed by `rustup`
- Cargo configured under `/usr/local/cargo`
- A sample workspace at `/workspace/hello-rust`
- `envd` inherited from `cubesandbox-base`, listening on `:49983`
- `smoke.py`, an optional Cube SDK smoke test that runs `rustc --version`,
  `cargo --version`, `cargo test`, and `cargo run --quiet` inside a sandbox

## Build the Image

```bash
docker build --platform linux/amd64 \
  -t cubesandbox-rust-runtime:local \
  examples/rust-runtime-template
```

Build arguments:

| Argument | Default | Purpose |
|----------|---------|---------|
| `CUBE_BASE_IMAGE` | `ghcr.io/tencentcloud/cubesandbox-base:2026.16` | Base image with `envd` and the Cube entrypoint |
| `CUBE_PLATFORM` | `linux/amd64` | Platform used to pull `cubesandbox-base`; this base image is currently published for amd64 |
| `RUST_TOOLCHAIN` | `1.89` | Rust toolchain installed by `rustup` |

Example override:

```bash
docker build \
  --platform linux/amd64 \
  --build-arg RUST_TOOLCHAIN=1.89 \
  -t cubesandbox-rust-runtime:local \
  examples/rust-runtime-template
```

## Verify Locally

```bash
docker run --rm -d \
  --platform linux/amd64 \
  -p 49983:49983 \
  --name cube-rust-runtime \
  cubesandbox-rust-runtime:local

curl -s -o /dev/null -w "envd /health => %{http_code}\n" \
  http://127.0.0.1:49983/health

docker exec cube-rust-runtime sh -lc \
  'rustc --version && cargo --version && cd /workspace/hello-rust && cargo test && cargo run --quiet'

docker rm -f cube-rust-runtime
```

Expected highlights:

```text
envd /health => 204
test result: ok
hello cube from Rust inside CubeSandbox
```

## Register as a Cube Template

Push the image to a registry reachable by your Cube cluster, then create the
template:

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/cubesandbox-rust-runtime:latest \
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
rustc --version
cargo --version
cd /workspace/hello-rust && cargo test
cd /workspace/hello-rust && cargo run --quiet
mkdir -p /workspace/runtime-smoke && printf '%s\n' rust-runtime-ok > /workspace/runtime-smoke/marker.txt
cat /workspace/runtime-smoke/marker.txt
```

## Resource Suggestions

- Smoke tests: 1 vCPU, 1-2 GiB memory, 2 GiB writable layer
- Real crate builds: 2+ vCPU, 4+ GiB memory for larger dependency graphs,
  larger writable layer for `target/` and Cargo caches
- Long-lived development sessions should use Cube snapshot or pause/resume
  flows from the quickstart examples instead of rebuilding dependencies every
  turn

## Network and Security Notes

- The included sample has no external crates, so runtime smoke works without
  outbound package access.
- Image build requires access to `ghcr.io`, Ubuntu APT mirrors such as
  `archive.ubuntu.com` and `security.ubuntu.com`, `sh.rustup.rs`, and Rust
  release distribution hosts such as `static.rust-lang.org`.
- Real projects that download crates or git dependencies need CubeEgress rules
  for the domains they use, commonly `crates.io`, `static.crates.io`,
  `index.crates.io`, and the relevant git hosts.
- Do not bake API keys or source credentials into the image. Pass secrets at
  sandbox creation time or through your platform secret mechanism.
- This template does not require privileged containers, Docker socket mounts, or
  host mounts.

## Known Limitations

- It does not expose a web app port by default. Add and expose your own service
  port if your Rust workload serves HTTP.
- It does not include Jupyter or the `e2b-code-interpreter` runtime.
- The default image is optimized as a reusable starting point, not as a minimal
  production Rust container.

## Cleanup

```bash
docker rm -f cube-rust-runtime 2>/dev/null || true
docker rmi cubesandbox-rust-runtime:local

# If you registered a template:
cubemastercli tpl delete --template-id <template-id>
```
