# Rust Playground + CubeSandbox

[中文文档](README_zh.md)

Compile and run Rust code inside a CubeSandbox MicroVM — this example set
showcases CubeSandbox's **Instant, Concurrent, Secure & Lightweight** value
proposition through Rust workloads.

## CubeSandbox Features Demonstrated

| Demo | CubeSandbox Concepts |
|------|----------------------|
| `hello_world.py` | **Instant** + **Concurrent** — 3 sandboxes created in parallel, timed creation <br> **Lifecycle** — auto-pause/resume via `lifecycle` config <br> **Introspection** — `get_info()` to query sandbox state |
| `with_dependencies.py` | **Secure** — network isolation via `allow_internet_access` <br> **Concurrent builds** — online vs offline sandbox comparison <br> **Env injection** — `envs=` parameter at creation |
| `snapshot_rollback.py` | **CubeCoW snapshot** — checkpoints persist independently (outlive source sandbox) <br> **Instant rollback** — ~100ms restore <br> **Clone** — `sb.clone(n=N)` one-shot fork <br> **Snapshot management** — `list_snapshots()` + `delete_snapshot()` |

## Directory layout

```
rust-playground/
├── Dockerfile              # CubeSandbox template image (Rust toolchain)
├── .env.example            # Copy to .env and fill in
├── .gitignore
├── requirements.txt        # Host driver deps (e2b, cubesandbox, python-dotenv)
├── env_utils.py            # .env loading helper
├── hello_world.py          # Instant + Concurrent: 3 sandboxes, parallel compile
├── with_dependencies.py    # Secure: network isolation via allow_internet_access
├── snapshot_rollback.py    # CubeCoW snapshot outlives sandbox + clone + rollback
├── tests/
│   ├── mock_sdk.py         # Mock SDK for offline verification
│   └── run_verification.py # Runs all 3 demos with mocks, no cluster needed
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

### Instant + Concurrent (hello_world.py)

```bash
python hello_world.py
```

Creates 3 sandboxes concurrently, compiles a Rust program in each, and
reports creation/build timing:

```
  [sb-0] created in 0.87s  id=sb-xxx  state=running
  [sb-1] created in 0.92s  id=sb-yyy  state=running
  [sb-2] created in 1.01s  id=sb-zzz  state=running
  [sb-0] compile=2.34s  output=Hello from CubeSandbox!
Total: 3 sandboxes in 3.21s  (1.07s avg per sandbox)
```

### Network isolation (with_dependencies.py)

```bash
python with_dependencies.py
```

Compares two sandboxes side-by-side — one with internet access, one without:

```
  sb-1 (online)    : PASS — cargo fetched from crates.io
  sb-2 (offline)   : FAIL — cargo blocked (network isolation)
  Expected: sb-1=0, sb-2=1  (offline cannot fetch crates)
```

This demonstrates `allow_internet_access=False`, a key security feature.

### Snapshot outlives sandbox (snapshot_rollback.py)

```bash
python snapshot_rollback.py
```

Demonstrates CubeSandbox's most differentiated feature:

1. **Snapshot** — saves sandbox state mid-development.
2. **Snapshot outlives sandbox** — kill the source sandbox, then clone from
   the snapshot into a new sandbox. The checkpoint is independent.
3. **Rollback** — restores the clone to checkpoint A in ~100ms.
4. **Clone(n)** — `sb.clone(n=3)` creates 3 forks in one call.

## Verification (offline, no cluster needed)

```bash
python3 -m venv /tmp/rust-verify-venv
/tmp/rust-verify-venv/bin/pip install python-dotenv pexpect Pillow
/tmp/rust-verify-venv/bin/python tests/run_verification.py
ls verification-logs/
```

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `rustc: command not found` | Rust not installed in template | Rebuild the image, re-register the template |
| `cargo build` timeout | First build downloads many crates | Increase `--exec-timeout` or the sandbox timeout |
| Offline sandbox still fetches crates | `allow_internet_access` not set | Ensure `allow_internet_access=False` is passed to `Sandbox.create()` |
| `pause()`/`connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |
| Permission denied on cargo | Running as root instead of `user` | Ensure the Dockerfile uses `USER user` for rustup |

## References

- Template guide: [`docs/guide/tutorials/template-from-image.md`](../../docs/guide/tutorials/template-from-image.md)
- BYOI (envd): [`docs/guide/tutorials/bring-your-own-image.md`](../../docs/guide/tutorials/bring-your-own-image.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../../docs/guide/snapshot-rollback-clone.md)
- Lifecycle: [`docs/guide/lifecycle.md`](../../docs/guide/lifecycle.md)
- Network Policy: [`docs/guide/network-policy.md`](../../docs/guide/network-policy.md)
