# Rust Playground + CubeSandbox

[中文文档](README_zh.md)

Compile and run Rust code inside a CubeSandbox MicroVM — write a one-off script with `rustc` or build a full `cargo` project with dependencies in an isolated, reproducible environment.

This example ships:

- A `Dockerfile` that stacks the Rust toolchain (rustup, rustc, cargo) on top of the CubeSandbox base image.
- `hello_world.py` — minimal demo: write a `.rs` file, compile with `rustc`, run the binary. Demonstrates `get_info()` for sandbox introspection and `lifecycle` with auto-pause/auto-resume.
- `with_dependencies.py` — scaffold a `cargo` project with external crates (`serde_json`, `chrono`), build, and run. Demonstrates `envs=` for injecting environment variables at sandbox creation alongside `get_info()` and `lifecycle`.
- `snapshot_rollback.py` — showcase CubeCoW snapshot, clone, and rollback during iterative Rust development. Demonstrates `Sandbox.list_snapshots()`, `sb.clone(n=N)` for one-shot cloning, and `Sandbox.delete_snapshot()` for cleanup.

## Directory layout

```
rust-playground/
├── Dockerfile              # CubeSandbox template image (Rust toolchain)
├── .env.example            # Copy to .env and fill in
├── .gitignore
├── requirements.txt        # Host driver deps (e2b, cubesandbox, python-dotenv)
├── env_utils.py            # .env loading helper
├── hello_world.py          # Minimal rustc compile-and-run demo
├── with_dependencies.py    # Cargo project with external crates
├── snapshot_rollback.py    # Snapshot / clone / rollback workflow
├── README.md               # English docs (this file)
└── README_zh.md            # Chinese docs
```

## Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- Python 3.10+ for the host driver scripts.

## 1. Build the template image

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/rust-playground:latest \
  examples/rust-playground

docker push <your-registry>/rust-playground:latest
```

The image installs the Rust toolchain (stable) via rustup, plus `gcc`, `git`, `make`, `libssl-dev`, and other build dependencies. The Rust version is pinned via `--build-arg RUST_TOOLCHAIN=stable`.

## 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/rust-playground:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

Note the `template_id` once the job reaches `READY`.

## 3. Configure the host driver

```bash
cd examples/rust-playground
cp .env.example .env
# fill in E2B_API_URL, CUBE_TEMPLATE_ID
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |

## 4. Run the demos

### Hello world (rustc)

```bash
python hello_world.py
```

Writes a Rust source file, compiles with `rustc`, and executes the binary. Expected output:

```
--- Running hello ---
stdout: Hello from CubeSandbox Rust playground!
Current time: <unix-timestamp>
```

### Cargo project with dependencies

```bash
python with_dependencies.py
```

Scaffolds a Cargo project, fetches `serde_json` and `chrono` from crates.io, builds with `cargo build --release`, and runs the binary — all inside the sandbox. The first run downloads crates; cargo's registry cache persists in the sandbox.

### Snapshot, clone, and rollback

```bash
python snapshot_rollback.py
```

This script uses the native `cubesandbox` SDK to demonstrate CubeSandbox's most differentiated feature:

1. **Snapshot** — saves the sandbox state mid-development (checkpoint A).
2. **Modify** — changes the Rust code and rebuilds (checkpoint B).
3. **Rollback** — restores the sandbox to checkpoint A in ~100ms.
4. **Clone** — forks a new sandbox from checkpoint A while the original keeps running.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `rustc: command not found` | Rust not installed in template | Rebuild the image, re-register the template |
| `cargo build` timeout | First build downloads many crates | Increase `--exec-timeout` or the sandbox timeout |
| Readiness probe timeout | Image without envd | Ensure `FROM ghcr.io/tencentcloud/cubesandbox-base:...` |
| `pause()`/`connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |
| Permission denied on cargo | Running as root instead of `user` | Ensure the Dockerfile uses `USER user` for rustup |

## References

- Template guide: [`docs/guide/tutorials/template-from-image.md`](../../docs/guide/tutorials/template-from-image.md)
- BYOI (envd): [`docs/guide/tutorials/bring-your-own-image.md`](../../docs/guide/tutorials/bring-your-own-image.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../../docs/guide/snapshot-rollback-clone.md)
