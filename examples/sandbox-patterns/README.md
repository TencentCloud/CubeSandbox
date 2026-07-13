# CubeSandbox Scenario Demos

[中文文档](README_zh.md)

Four scenario demos showcasing **CubeSandbox platform patterns** for AI Agent
sandbox orchestration — stateful workspaces, egress policy enforcement,
checkpoint-driven development, and multi-sandbox collaboration.

The template uses a **Rust toolchain** as the workload vehicle, but the patterns
themselves are language-agnostic.

---

## What These Demos Simulate

### 1. `parallel_workspaces.py` — Stateful Workspace Lifecycle

**Simulates**: An AI Agent managing multiple concurrent workspaces (e.g., analyzing several code repositories simultaneously).

What happens:
- 3 sandbox workspaces are created in parallel.
- Each compiles a workload and reports creation timing and state.
- Lifecycle `on_timeout: pause` + `auto_resume: True` ensures workspaces survive idle periods.
- `get_info()` provides real-time state introspection.

### 2. `network_isolation.py` — Egress Network Policy Enforcement

**Simulates**: Two Agents with different security postures — one that can download dependencies from the internet, and one that must operate fully air-gapped.

What happens:
- Two sandboxes are created side-by-side with the same workload.
- sb-1 has `allow_internet_access=True` — pulls dependencies successfully.
- sb-2 has `allow_internet_access=False` — blocked by egress policy.
- The workload is identical; only the per-sandbox policy differs.

### 3. `snapshot_driven_dev.py` — Checkpoint-Driven Iterative Development

**Simulates**: An Agent debugging a codebase iteratively — making changes, hitting a dead end, and rolling back to a known good state.

What happens:
- Phase 1: Create a sandbox, scaffold a project, build it.
- Phase 2: Take a CubeCoW snapshot (checkpoint A).
- Phase 2b: Kill the source sandbox — the snapshot remains independently.
- Phase 3: Fork a new sandbox from checkpoint A (restore workspace state).
- Phase 4: Make changes in the fork, then rollback to checkpoint A (sub-second).
- Phase 5: One-shot `sb.clone(n=3)` forks 3 sandboxes from the current state.

### 4. `multi_container.py` — Multi-Sandbox Collaboration

**Simulates**: A CI/CD pipeline where a build service (with internet) compiles an artifact and a test service (air-gapped) runs it — defense-in-depth by role separation.

What happens:
- Builder sandbox (internet allowed) downloads crates and compiles a release binary.
- Host SDK reads the binary from the builder.
- Runner sandbox (air-gapped) receives the binary and executes it.
- The runner succeeds without ever touching the internet.

## CubeSandbox Capabilities Demonstrated

| Demo | Scenario | CubeSandbox Capabilities |
|------|----------|--------------------------|
| `parallel_workspaces.py` | Stateful workspace lifecycle | **Lifecycle** — auto-pause/resume via `lifecycle` config <br> **Introspection** — `get_info()` to query sandbox state <br> **Concurrent workspaces** — multiple sandboxes in parallel |
| `network_isolation.py` | Egress network policy enforcement | **Secure** — per-sandbox `allow_internet_access` <br> **Env injection** — `envs=` parameter at creation <br> **Side-by-side policy comparison** |
| `snapshot_driven_dev.py` | Checkpoint-driven iterative development | **CubeCoW snapshot** — checkpoints outlive source sandbox <br> **Instant rollback** — restore from checkpoint <br> **Clone** — `sb.clone(n=N)` one-shot fork <br> **Snapshot management** — `list_snapshots()` + `delete_snapshot()` |
| `multi_container.py` | Multi-sandbox collaboration | **Role-based isolation** — builder (online) vs runner (air-gapped) <br> **Cross-sandbox artifact transfer** — via host SDK |

---

## Directory layout

```
sandbox-patterns/
├── Dockerfile                   # Template image — Rust toolchain on cubesandbox-base
├── .env.example                 # Copy to .env and fill in
├── .gitignore
├── requirements.txt             # Host driver deps (e2b, python-dotenv)
├── env_utils.py                 # .env loading helper
├── parallel_workspaces.py       # Scenario: stateful workspace lifecycle
├── network_isolation.py         # Scenario: egress network policy enforcement
├── snapshot_driven_dev.py       # Scenario: checkpoint-driven development
├── multi_container.py           # Scenario: multi-sandbox collaboration
├── tests/
│   ├── mock_sdk.py              # Mock SDK for offline verification
│   └── run_verification.py      # Runs all demos with mocks, no cluster needed
├── README.md                    # English docs (this file)
├── README_zh.md                 # Chinese docs
└── REAL_ENV_VERIFICATION.md     # Guide for verifying against a real cluster
```

## Quick Start

### Prerequisites

- A running CubeSandbox deployment; CubeAPI reachable at `http://<node>:3000`.
- `cubemastercli` on `$PATH`, connected to the cluster.
- Docker on the build workstation, plus a registry the Cube nodes can pull from.
- Python 3.10+ for the host driver scripts.

### 1. Build the template image

```bash
docker build --platform linux/amd64 \
  -t <your-registry>/sandbox-patterns:latest \
  examples/sandbox-patterns

docker push <your-registry>/sandbox-patterns:latest
```

The image installs the Rust toolchain (stable) via rustup, plus `gcc`, `git`,
`make`, `libssl-dev`, and other build dependencies. A dummy Cargo project is
pre-built during image creation to cache the crates.io index, reducing
first-build latency inside sandboxes.

### 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/sandbox-patterns:latest \
  --writable-layer-size 4G \
  --expose-port 49983 \
  --probe       49983 \
  --probe-path  /health

cubemastercli tpl watch --job-id <job_id>
```

Note the `template_id` once the job reaches `READY`.

### 3. Configure the host driver

```bash
cd examples/sandbox-patterns
cp .env.example .env
# fill in E2B_API_URL, CUBE_TEMPLATE_ID
pip install -r requirements.txt
```

| Variable | Where it flows | Notes |
|---|---|---|
| `E2B_API_URL` | Local process | CubeAPI address (`http://<node>:3000`) |
| `E2B_API_KEY` | Local process | Any non-empty string in local dev |
| `CUBE_TEMPLATE_ID` | `Sandbox.create(template=...)` | From step 2 |

### 4. Run the demos

```bash
# Stateful workspace lifecycle
python parallel_workspaces.py

# Egress network policy enforcement
python network_isolation.py

# Checkpoint-driven development
python snapshot_driven_dev.py

# Multi-sandbox collaboration
python multi_container.py
```

## Verification (offline, no cluster needed)

```bash
python3 -m venv /tmp/rust-verify-venv
/tmp/rust-verify-venv/bin/pip install python-dotenv pexpect Pillow
/tmp/rust-verify-venv/bin/python tests/run_verification.py
ls verification-logs/
```

## Real environment verification

See [REAL_ENV_VERIFICATION.md](REAL_ENV_VERIFICATION.md) for a step-by-step
guide to verify all demos against a real CubeSandbox cluster.

## Known Limitations

| Limitation | Details | Workaround |
|------------|---------|------------|
| **Dockerfile ENV PATH not inherited** | The `ENV PATH` set in Dockerfile is not carried over by CubeSandbox's MicroVM runtime. While `/home/user/.cargo/bin/cargo` exists, `$PATH` does not include it. | Demo scripts prefix `CARGO_HOME` / `RUSTUP_HOME` env vars to cargo/rustc commands automatically. |
| **Snapshot clone loses `$HOME` context** | When creating a sandbox from a snapshot, `rustup home` points to `/root/.rustup` instead of `/home/user/.rustup`, causing "no active toolchain" errors for cargo. | Demo scripts set `CARGO_HOME=/home/user/.cargo RUSTUP_HOME=/home/user/.rustup HOME=/home/user` before toolchain commands. |
| **`files.read()` defaults to text mode** | `files.read()` without `format="bytes"` decodes binary content as UTF-8, corrupting ELF executables and non-text files. | The multi-sandbox demo passes `format="bytes"` to preserve binary integrity. |
| **First `cargo build` is slow** | Even with the pre-built dummy project, the first real `cargo build` in a sandbox takes ~20-30s due to dependency resolution and compilation. | This is expected for Rust. The snapshot demo can pre-build, snapshot, and fork to reuse compiled artifacts. |
| **Writable layer sizing** | Rust compilation can produce large `target/` directories (1-2 GB). The default writable layer size may be insufficient. | Register the template with `--writable-layer-size 4G` (shown in the build instructions above). |

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `rustc: command not found` | Rust not installed in template | Rebuild the image, re-register the template |
| `cargo build` timeout | First build downloads many crates | Increase `--exec-timeout` or the sandbox timeout |
| Offline sandbox still fetches crates | `allow_internet_access` not set | Ensure `allow_internet_access=False` is passed to `Sandbox.create()` |
| `pause()`/`connect()` errors | Platform too old for snapshots | Upgrade the CubeSandbox platform |
| Permission denied on cargo | Running as root instead of `user` | Ensure the Dockerfile uses `USER user` for rustup |

## About CubeSandbox

CubeSandbox is an **instant, concurrent, secure, and lightweight sandbox service**
purpose-built for AI Agents. Built on **RustVMM + KVM**, it creates a hardware-isolated
MicroVM in under **60ms** with less than **5MB of memory overhead** per instance — dense
enough to run thousands of sandboxes on a single node.

### Key technical enablers

1. **RustVMM + KVM MicroVM** — a minimal VMM written in Rust, paired with KVM, boots a thin guest kernel in milliseconds. No shared kernel means hardware-level isolation without the overhead of a full virtual machine.
2. **Memory deduplication and density optimizations** — sandbox memory is deduplicated at the page level, reducing per-instance overhead from gigabytes to megabytes and enabling thousands of concurrent sandboxes on a single node.
3. **CubeCoW Copy-on-Write snapshot engine** — snapshots record only the diff since the last checkpoint, making them sub-100ms to create, restore, and clone (per CubeSandbox documented performance). Snapshots are fully independent objects — killing the source sandbox does not affect them.
4. **Per-sandbox network namespace** — each MicroVM gets its own network stack. `allow_internet_access` controls egress at the sandbox level, not globally.

### Pain points solved

| Pain Point | Traditional solutions | CubeSandbox |
|---|---|---|
| **Cold start is slow** | Docker ~1s, VM ~30s | **<60ms** — sub-second sandbox creation |
| **Weak isolation** | Docker shares host kernel — escape vulnerabilities are common | **Hardware-level isolation** — each sandbox has its own Guest OS kernel via KVM |
| **Resource overhead is high** | Docker ~100MB, VM ~GB | **<5MB per sandbox** — thousands per node |
| **State management is cumbersome** | No snapshot support, or snapshots take seconds | **CubeCoW sub-100ms snapshot/rollback** — snapshots are independent and outlive the source sandbox |
| **Network policy is coarse** | Global settings, hard to per-instance | **Per-sandbox `allow_internet_access`** — precise egress control per sandbox |

## References

- Template guide: [`docs/guide/tutorials/template-from-image.md`](../../docs/guide/tutorials/template-from-image.md)
- BYOI (envd): [`docs/guide/tutorials/bring-your-own-image.md`](../../docs/guide/tutorials/bring-your-own-image.md)
- Snapshot / Clone / Rollback: [`docs/guide/snapshot-rollback-clone.md`](../../docs/guide/snapshot-rollback-clone.md)
- Lifecycle: [`docs/guide/lifecycle.md`](../../docs/guide/lifecycle.md)
- Network Policy: [`docs/guide/network-policy.md`](../../docs/guide/network-policy.md)
