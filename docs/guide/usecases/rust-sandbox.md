---
title: Rust Code Execution Sandbox with Compilation Cache Persistence
author: Shizuku-in
date: 2026-07-03
tags:
  - rust
  - code-execution
  - compilation-cache
  - snapshot
lang: en-US
---

# Rust Code Execution Sandbox with Compilation Cache Persistence

## Business Context

Rust is increasingly adopted across systems programming, WebAssembly, CLI tooling,
and blockchain ecosystems. As the language grows, so does the demand for safe,
isolated environments to compile and run Rust code. Common scenarios include:

- **CI/CD pipelines** that need to build and test untrusted third-party crates
  without risking host compromise
- **Online coding platforms** (like LeetCode, Exercism) that evaluate
  user-submitted Rust solutions
- **LLM coding agents** that generate, compile, and iterate on Rust code in an
  agentic loop
- **Documentation sites** that offer live, runnable Rust code examples

Traditional container-based isolation (Docker, gVisor) provides OS-level
separation, but Rust compilation produces native binaries that can exploit
kernel vulnerabilities or escape the container boundary. True hardware-level
isolation is needed for untrusted-code scenarios.

## Key Challenges

1. **Native code execution is dangerous.** A compiled Rust binary can make
   arbitrary syscalls, attempt privilege escalation, or try to access host
   resources. Container runtimes alone do not provide sufficient isolation.

2. **Compilation is slow and stateful.** `cargo build` downloads crates from
   the network and populates a `target/` directory that can grow to hundreds of
   megabytes. Starting from scratch every time adds minutes of latency,
   unacceptable for interactive or CI use cases.

3. **Network access is a double-edged sword.** Crate downloads require internet
   access, but an untrusted binary must not be allowed to exfiltrate data or
   connect to arbitrary hosts.

4. **Toolchain size matters.** A full Rust installation with common crates can
   exceed 2 GB. Pre-building and snapshotting the environment is essential for
   fast cold starts.

## Solution with Cube Sandbox

We built a **Rust sandbox template** on CubeSandbox that addresses all four
challenges:

### Architecture

```
User Script (Python, E2B SDK)
        │
        ▼
CubeAPI ──► CubeMaster ──► Cubelet ──► KVM MicroVM
                                            │
                                     ┌──────┴──────┐
                                     │  envd :49983 │
                                     │  rustc/cargo │
                                     │  ~/.cargo/   │ (pre-warmed)
                                     │  target/     │ (snapshotable)
                                     └──────────────┘
```

### Key design decisions

| Concern | Approach |
|---------|----------|
| **Isolation** | KVM-level MicroVM — compiled binaries cannot escape, regardless of what they do |
| **Cold start** | Docker image with pre-installed Rust toolchain + pre-warmed crate registry (`serde`, `serde_json`, `axum`, `tokio`) |
| **Incremental builds** | `sandbox.pause()` snapshots the entire VM state (memory + `target/` + `~/.cargo/`); `sandbox.connect()` restores it in under a second |
| **Network control** | `allow_internet_access=True` for crate downloads; `allow_internet_access=False` for evaluating untrusted code |
| **Resource limits** | `ulimit -v` and `timeout` wrappers prevent runaway compilation or execution |

### Template creation

```bash
# Build the custom Docker image
docker build -t rust-sandbox:latest examples/rust-sandbox/

# Register as a Cube template
cubemastercli tpl create-from-image \
  --image <registry>/rust-sandbox:latest \
  --writable-layer-size 2G \
  --expose-port 49983 \
  --expose-port 8080 \
  --probe 49983 \
  --probe-path /health
```

### Compilation cache snapshot flow

The flagship capability: **snapshot-preserved compilation cache**.

1. Create a Cargo project with dependencies → `cargo build --release` (30–60s,
   downloads + full compilation)
2. `sandbox.pause()` — freeze the VM, release CPU/memory resources
3. Hours or days later: `sandbox.connect()` — restore from snapshot in under a
   second
4. Modify source code → `cargo build --release` (2–5s, incremental only)
5. **Speedup: 10–20×** compared to a cold build

This is uniquely possible because CubeSandbox snapshots the **entire VM state**,
not just the filesystem. The in-memory compilation caches and mmap'd crate
sources survive the pause/resume cycle intact.

## Results and Benefits

| Metric | Without CubeSandbox | With CubeSandbox |
|--------|-------------------|------------------|
| Isolation level | Container (shared kernel) | KVM (dedicated kernel) |
| Cold build time | 30–60s (download + compile) | 30–60s (one-time, pre-warmed deps) |
| Hot build time | N/A (containers are ephemeral) | 2–5s (snapshot resume + incremental) |
| Network isolation | Namespace-level (iptables) | Per-sandbox egress policy (enforced at proxy) |
| Binary escape risk | Moderate | Near-zero (hardware boundary) |

The Rust sandbox template is available as a self-contained example in the
CubeSandbox repository with five demo scripts, bilingual documentation, and
local Docker smoke tests.

## References

- Example: [examples/rust-sandbox](https://github.com/TencentCloud/CubeSandbox/tree/master/examples/rust-sandbox)
- Docs: [Bring Your Own Image](https://github.com/TencentCloud/CubeSandbox/blob/master/docs/guide/tutorials/bring-your-own-image.md)
- E2B SDK: [https://e2b.dev](https://e2b.dev)
