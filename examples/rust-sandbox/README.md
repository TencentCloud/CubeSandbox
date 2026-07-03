# Rust Sandbox

[中文文档](README_zh.md)

Compile and run Rust code inside a Cube Sandbox — from a one-line snippet to a full Cargo project, all through the E2B Python SDK.

## 1. Background

**Cube Sandbox** is a lightweight MicroVM platform fully compatible with the [E2B SDK](https://e2b.dev). This example adds a complete Rust toolchain on top of the official `cubesandbox-base` image:

- **rustc** and **cargo** — the Rust compiler and package manager, available on `$PATH` for every user inside the sandbox
- A **pre-warmed crate registry** — common crates (`serde`, `serde_json`, `axum`, `tokio`) are downloaded and cached at image build time, so the first `cargo build` in a new sandbox only compiles your code

You interact with the sandbox through the E2B SDK — write Rust source files, compile with `sandbox.commands.run("rustc ...")`, and read back the output. The sandbox is a full KVM MicroVM with its own kernel, filesystem, and network. When the `with` block exits, the sandbox is automatically deleted.

```text
  Your Script (E2B SDK)
       │
       │  sandbox.commands.run("rustc ...")
       │  sandbox.commands.run("cargo build --release")
       │  sandbox.files.write(...) / sandbox.files.read(...)
       ▼
  ┌─────────────────────────────┐
  │        KVM MicroVM          │
  │                             │
  │  envd (:49983)              │
  │    │                        │
  │    ▼                        │
  │  rustc   cargo   rustup     │
  │  ~/.cargo/registry/ (cached)│
  │  target/                    │
  └─────────────────────────────┘
```

## 2. Prerequisites

- A running Cube Sandbox deployment
- Python 3.8+
- Docker

```bash
pip install -r requirements.txt
```

The example scripts use `python-dotenv` to load a `.env` file from the script directory. If no `.env` file exists, they continue with the current process environment variables.

## 3. Quick Start

### Step 1 — Build the Docker Image

```bash
docker build -t rust-sandbox:latest examples/rust-sandbox/

# Smoke test the image locally
docker run --rm -d -p 49983:49983 --name test-rust rust-sandbox:latest
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health  # → 204
docker exec test-rust rustc --version
docker exec test-rust cargo --version
docker rm -f test-rust
```

Push the image to a registry your Cube cluster can reach:

```bash
docker tag rust-sandbox:latest <your-registry>/rust-sandbox:latest
docker push <your-registry>/rust-sandbox:latest
```

### Step 2 — Create the Rust Template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/rust-sandbox:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --expose-port 8080 \
  --probe 49983 \
  --probe-path /health
```

> Rust `target/` directories can grow large. 2G is a sensible default; raise to 4G or more for projects with many dependencies.

Note the `template_id` printed on success.

### Step 3 — Configure Environment Variables

```bash
cp .env.example .env
# edit .env and fill in E2B_API_URL and CUBE_TEMPLATE_ID
```

After that, you can run any example script directly without manually exporting the variables first.

Or export directly:

```bash
export E2B_API_KEY=e2b_000000
export E2B_API_URL=http://<your-node-ip>:3000
export CUBE_TEMPLATE_ID=<template-id>

# Only needed when using Cube's built-in mkcert certificate:
# export SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem
```

### Step 4 — Run a Script

```bash
# Compile and run a example Rust snippet
python rust_compile_run.py
```

Expected output:

```
Hello from Rust inside CubeSandbox!
Fibonacci(10) = 55
```

## 4. All Scripts

| Script | What it shows |
| --- | --- |
| `rust_compile_run.py` | `sandbox.files.write()` + `sandbox.commands.run("rustc ...")` — write Rust source, compile it, run the binary |
| `rust_cargo_project.py` | `cargo new` → `cargo build --release` → `cargo run` — the full Cargo workflow inside a sandbox |
| `rust_snapshot_cache.py` | `sandbox.pause()` / `sandbox.connect()` — snapshot the VM after a cold build, resume later, and see incremental compilation finish in seconds |
| `rust_web_service.py` | `sandbox.get_host(8080)` — build an axum HTTP server, start it, and reach it through CubeProxy |
| `rust_secure_eval.py` | `allow_internet_access=False` — compile and run untrusted Rust code in a fully air-gapped sandbox |
| `test_rust_sandbox.py` | Local smoke tests against the Docker image — no Cube cluster needed |

### rust_compile_run.py — Compile and Run

```python
with Sandbox.create(template=template_id) as sandbox:
    sandbox.files.write("/tmp/main.rs", code)
    sandbox.commands.run("rustc -o /tmp/main /tmp/main.rs")
    result = sandbox.commands.run("/tmp/main")
    print(result.stdout)
```

### rust_cargo_project.py — Cargo Project

```python
with Sandbox.create(template=template_id) as sandbox:
    sandbox.commands.run("cd /home/user && cargo new hello-cube")
    sandbox.files.write("/home/user/hello-cube/src/main.rs", source)
    sandbox.commands.run("cd /home/user/hello-cube && cargo build --release")
    result = sandbox.commands.run("/home/user/hello-cube/target/release/hello-cube 10 42 99")
    print(result.stdout)
```

### rust_snapshot_cache.py — Snapshot & Resume

Create a Cargo project with dependencies, build it (cold, ~30–60s), pause the sandbox to free resources, then resume it later. The `target/` directory and crate cache survive the pause/resume cycle, so the next build only recompiles changed files:

```python
with Sandbox.create(template=template_id) as sandbox:
    # Cold build — download crates, compile everything
    sandbox.commands.run("cd /home/user/snapshot-demo && cargo build --release")

    sandbox.pause()       # save VM snapshot, release resources
    time.sleep(3)
    sandbox.connect()     # restore snapshot, resume execution

    # Hot build — only changed files recompile (2–5s instead of 30–60s)
    sandbox.commands.run("cd /home/user/snapshot-demo && cargo build --release")
```

### rust_web_service.py — HTTP Service on an Exposed Port

```python
with Sandbox.create(template=template_id) as sandbox:
    sandbox.commands.run("cd /opt/rust-demo && cargo build --release")
    sandbox.commands.run(
        "cd /opt/rust-demo && nohup ./target/release/rust-demo > /tmp/server.log 2>&1 &"
    )

    url = f"https://{sandbox.get_host(8080)}/"
    resp = requests.get(url, verify=False)
    print(resp.json())   # {"status":"ok","runtime":"rust",...}
```

### rust_secure_eval.py — Air-Gapped Code Evaluation

```python
with Sandbox.create(
    template=template_id,
    allow_internet_access=False,  # fully air-gapped
) as sandbox:
    # Write Cargo.toml and main.rs into the workspace
    sandbox.files.write("/tmp/secure-eval/Cargo.toml", cargo_toml)
    sandbox.files.write("/tmp/secure-eval/src/main.rs", user_code)
    sandbox.commands.run("cd /tmp/secure-eval && cargo build --release")

    result = sandbox.commands.run(
        "timeout 10 sh -c 'ulimit -v 524288 && /tmp/secure-eval/target/release/secure-eval'"
    )
```

## 5. Troubleshooting

| Symptom | Likely Cause | Fix |
| --- | --- | --- |
| `SSL: CERTIFICATE_VERIFY_FAILED` | HTTPS without CA cert | Set `SSL_CERT_FILE=/root/.local/share/mkcert/rootCA.pem` |
| `cargo: command not found` | PATH not set inside sandbox | Verify Dockerfile copied binaries to `/usr/local/bin/`; rebuild if missing |
| `cargo build` hangs on "Updating crates.io index" | No internet access in sandbox | Pre-populate all needed crates in the Docker image, or set `allow_internet_access=True` |
| `Template not found` | Wrong template ID | Re-run `cubemastercli tpl list` |
| `Connection refused` | CubeAPI not reachable | Check `E2B_API_URL` and port 3000 |
| "No space left on device" | Writable layer too small | Increase `--writable-layer-size` (2G minimum) |
| `error: linker 'cc' not found` | Missing build tools | The Dockerfile includes `build-essential`; rebuild if it was removed |
| `sandbox.pause()` takes too long | Large writable layer | Clean up old `target/` directories before pausing |

## 6. Directory Structure

```
rust-sandbox/
├── Dockerfile                    # Rust toolchain on top of cubesandbox-base
├── demo_project/                 # Pre-built axum demo (warms the crate cache)
│   ├── Cargo.toml
│   └── src/
│       └── main.rs
├── README.md                     # English documentation (this file)
├── README_zh.md                  # Chinese documentation
├── requirements.txt              # Python dependencies
├── .env.example                  # Environment variable template
├── rust_compile_run.py           # Write Rust source, rustc, run binary
├── rust_cargo_project.py         # cargo new → build → run
├── rust_snapshot_cache.py        # Cold build → pause → resume → hot build
├── rust_web_service.py           # Build + serve an axum HTTP service
├── rust_secure_eval.py           # Run untrusted code with network blocked
└── test_rust_sandbox.py          # Local Docker smoke tests
```
