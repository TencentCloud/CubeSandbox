# CubeSandbox Scenario Demos

[中文文档](README_zh.md)

## What Is CubeSandbox?

CubeSandbox is an **instant, concurrent, secure, and lightweight sandbox service**
purpose-built for AI Agents. Built on **RustVMM + KVM**, it creates a hardware-isolated
MicroVM in under **60ms** with less than **5MB of memory overhead** per instance — dense
enough to run thousands of sandboxes on a single node.

It is compatible with the E2B SDK and supports **snapshot/clone/rollback** via the
CubeCoW Copy-on-Write engine, **per-sandbox egress policies**, and lifecycle management
(auto-pause/resume).

## What Problem Does It Solve?

| Pain Point | Traditional solutions | CubeSandbox |
|---|---|---|
| **Cold start is slow** | Docker ~1s, VM ~30s | **<60ms** — sub-second sandbox creation |
| **Weak isolation** | Docker shares host kernel — escape vulnerabilities are common | **Hardware-level isolation** — each sandbox has its own Guest OS kernel via KVM |
| **Resource overhead is high** | Docker ~100MB, VM ~GB | **<5MB per sandbox** — thousands per node |
| **State management is cumbersome** | No snapshot support, or snapshots take seconds | **CubeCoW ~100ms snapshot/rollback** — snapshots are independent and outlive the source sandbox |
| **Network policy is coarse** | Global settings, hard to per-instance | **Per-sandbox `allow_internet_access`** — precise egress control per sandbox |

## Application Scenarios

CubeSandbox is designed for **AI Agent execution environments**, including:

- **Code execution by LLM-generated code** — an Agent writes Python/Rust/JS code, CubeSandbox compiles and runs it in an isolated MicroVM. If the code is malicious, it cannot escape.
- **Security policy enforcement** — enterprise workloads need different network postures: one Agent may access the internet (to install packages), another must be fully air-gapped.
- **Stateful workspaces** — long-running Agent tasks (data cleaning, model training, iterative debugging) need checkpoint/resume. CubeCoW snapshots preserve the full workspace state across sandbox lifetimes.
- **Multi-Agent collaboration** — build services, test services, and inference services each run in their own sandbox with role-specific isolation policies.

## Why Can CubeSandbox Do This?

The key technical enablers are:

1. **RustVMM + KVM MicroVM** — a minimal VMM written in Rust, paired with KVM, boots a thin guest kernel in milliseconds. No shared kernel means hardware-level isolation without the overhead of a full virtual machine.
2. **Memory deduplication and density optimizations** — sandbox memory is deduplicated at the page level, reducing per-instance overhead from gigabytes to megabytes and enabling thousands of concurrent sandboxes on a single node.
3. **CubeCoW Copy-on-Write snapshot engine** — snapshots record only the diff since the last checkpoint, making them sub-100ms to create, restore, and clone. Snapshots are fully independent objects — killing the source sandbox does not affect them.
4. **Per-sandbox network namespace** — each MicroVM gets its own network stack. `allow_internet_access` controls egress at the sandbox level, not globally.

## What These Demos Simulate

The four demo scripts simulate real-world AI Agent scenarios using CubeSandbox:

### 1. parallel_workspaces.py — Stateful Workspace Lifecycle

**Simulates**: An AI Agent managing multiple concurrent workspaces (e.g., analyzing several code repositories simultaneously).

What happens:
- 3 sandbox workspaces are created in parallel.
- Each compiles a workload and reports creation timing and state.
- Lifecycle `on_timeout: pause` + `auto_resume: True` ensures workspaces survive idle periods.
- `get_info()` provides real-time state introspection.

### 2. network_isolation.py — Egress Network Policy Enforcement

**Simulates**: Two Agents with different security postures — one that can download dependencies from the internet, and one that must operate fully air-gapped.

What happens:
- Two sandboxes are created side-by-side with the same workload.
- sb-1 has `allow_internet_access=True` — cargo fetches crates successfully.
- sb-2 has `allow_internet_access=False` — cargo is blocked by egress policy.
- The workload is identical; only the per-sandbox policy differs.

### 3. snapshot_driven_dev.py — Checkpoint-Driven Iterative Development

**Simulates**: An Agent debugging a codebase iteratively — making changes, hitting a dead end, and rolling back to a known good state.

What happens:
- Phase 1: Create a sandbox, scaffold a project, build it.
- Phase 2: Take a CubeCoW snapshot (checkpoint A).
- Phase 2b: Kill the source sandbox — the snapshot remains independently.
- Phase 3: Fork a new sandbox from checkpoint A (restore workspace state).
- Phase 4: Make changes in the fork, then rollback to checkpoint A in ~0ms.
- Phase 5: One-shot `sb.clone(n=3)` forks 3 sandboxes from the current state.

### 4. multi_container.py — Multi-Sandbox Collaboration

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
| `snapshot_driven_dev.py` | Checkpoint-driven iterative development | **CubeCoW snapshot** — checkpoints outlive source sandbox <br> **Instant rollback** — ~100ms restore <br> **Clone** — `sb.clone(n=N)` one-shot fork <br> **Snapshot management** — `list_snapshots()` + `delete_snapshot()` |
| `multi_container.py` | Multi-sandbox collaboration | **Role-based isolation** — builder (online) vs runner (air-gapped) <br> **Cross-sandbox artifact transfer** — via host SDK |

## Directory layout

```
rust-playground/
├── Dockerfile                   # CubeSandbox template image (Rust toolchain)
├── .env.example                 # Copy to .env and fill in
├── .gitignore
├── requirements.txt             # Host driver deps (e2b, cubesandbox, python-dotenv)
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

The image installs the Rust toolchain (stable) via rustup, plus `gcc`, `git`,
`make`, `libssl-dev`, and other build dependencies. A dummy Cargo project is
pre-built during image creation to cache the crates.io index, reducing
first-build latency inside sandboxes.

## 2. Register as a Cube template

```bash
cubemastercli tpl create-from-image \
  --image <your-registry>/rust-playground:latest \
  --writable-layer-size 4G \
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

### Stateful workspace lifecycle (parallel_workspaces.py)

```bash
python parallel_workspaces.py
```

Creates 3 sandbox workspaces concurrently, each compiling and running a
workload. Demonstrates lifecycle pause/resume and get_info() introspection:

```
CubeSandbox — Stateful Workspace Management Demo
  Scenario: 3 concurrent workspaces with lifecycle pause/resume

  [ws-0] created in 0.87s  id=sb-xxx  state=running
  [ws-1] created in 0.92s  id=sb-yyy  state=running
  [ws-2] created in 1.01s  id=sb-zzz  state=running
  [ws-0] build=2.34s  output=Hello from CubeSandbox workspace!

  Total: 3 workspaces in 3.21s  (1.07s avg per workspace)  failures=0
  Key takeaway: sandboxes survive idle timeout via lifecycle pause/resume.
```

### Egress network policy enforcement (network_isolation.py)

```bash
python network_isolation.py
```

Compares two sandboxes side-by-side — one with internet access, one without:

```
  sb-1 (online)    : build succeeded — cargo pulled dependencies from crates.io
  sb-2 (offline)   : build FAILED — cargo blocked by egress policy
  Expected: sb-1=0, sb-2=1  (offline cannot fetch external resources)
```

This demonstrates `allow_internet_access=False`, a key security feature for
air-gapped workloads.

### Checkpoint-driven development (snapshot_driven_dev.py)

```bash
python snapshot_driven_dev.py
```

Demonstrates CubeSandbox's most differentiated feature — iterative development
with checkpoint/restore:

1. **Checkpoint** — save workspace state mid-development.
2. **Checkpoint outlives workspace** — kill the source sandbox, then fork from
   the checkpoint into a new sandbox.
3. **Rollback** — restore to checkpoint A in ~100ms.
4. **Clone(n)** — `sb.clone(n=3)` creates 3 parallel forks in one call.

### Multi-sandbox collaboration (multi_container.py)

```bash
python multi_container.py
```

Demonstrates role-based network isolation across multiple sandboxes:

1. **Builder sandbox** (with internet) downloads crates and compiles a binary.
2. **Host SDK** reads the binary from the builder sandbox.
3. **Runner sandbox** (air-gapped) receives the binary and executes it.
4. The runner succeeds without ever touching the internet.

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
