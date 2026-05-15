# Changelog

## 2026.05.14 Release v0.2.1

### 🌟 Major New Features

- **Official Python SDK (`cubesandbox` v0.1.0)**: A first-party Python SDK shipped under `sdk/python/`, fully aligned with the CubeAPI OpenAPI spec. Covers full sandbox lifecycle (create/connect/pause/kill/list/health), code execution with streaming stdout/stderr, filesystem access, direct-connect transport, and network policy. Includes 12 worked examples, a concurrency benchmark, and 76/76 tests passing.

### 🚀 Performance

- **Skip SHA256 on every Cubelet startup**: Split `SyncKernelFile` into `EnsureKernelFilePresent` (copy-if-missing, fast path) and `RefreshKernelFile` (force-refresh with verification), removing the expensive per-boot SHA256 comparison. Normal startup latency drops significantly on hosts with many templates.
- **Skip redundant `docker pull` in CubeMaster**: Source image pulls are now bypassed when the image already exists locally, removing unnecessary registry round-trips during template builds.

### 🛡️ Security Fixes

- **`shim`: protobuf bumped 3.4.0 → 3.7.2** (RUSTSEC, stack overflow on crafted unknown fields). Co-upgrades `containerd-shim-protos`, `containerd-shim`, and `nix`.
- **`cubeapi` / `agent` / `shim` / `hypervisor`: rand 0.8.5 → 0.8.6** (GHSA-cq8v-f236-94qc, soundness issue with `ThreadRng` reseeding).
- **`CubeVS`: `golang.org/x/net` → v0.38.0, `golang.org/x/sys` → v0.38.0**.
- **`network-agent`: `google.golang.org/grpc` → 1.79.3**.
- **`CubeAPI/examples`: `pygments` → 2.20.0**.

### 🛠️ Critical Fixes

- **Fixed Seccomp swallowing all syscalls**: `Seccomp` initialization now sets `DefaultAction = ActAllow`; an empty syscall list short-circuits as a no-op instead of silently blocking everything.
- **Fixed `shim` stderr being routed through stdout**: The `Exec` stream-forwarding path was incorrectly calling the stdout read method for stderr; stderr is now properly captured and forwarded.
- **Fixed `CubeProxy` workers sharing the same PRNG seed**: OpenResty workers now seed the RNG per-worker in `init_worker` with `(ngx.now() * 1000 + ngx.worker.id())`, preventing synchronized cache-expiration stampedes.
- **Fixed dev-env sync overwriting `cube-shim` symlinks**: `cube-runtime` and `containerd-shim-cube-rs` are now written to `${TOOLBOX_ROOT}/cube-shim/bin`, preserving the toolbox symlink layout.
- **Fixed Dockerfile breakage on HTTPS-only mirrors**: `ca-certificates` is now installed before apt sources are swapped to internal mirrors.

### ✨ Enhancements

- **`cubemastercli tpl watch` — phase-oriented output**: Replaced the old multi-line full-status dump with concise `[N/7] PHASE` progress lines plus a terminal summary; much friendlier in CI logs.
- **IPAM — comprehensive optimization and reliability overhaul** (Cubelet + network-agent): Validation rewritten on `net/netip`; IP ↔ index conversions via `encoding/binary.BigEndian`; bounds checks, safety limits, and `nil` guards added; reserved-address semantics documented; comprehensive table-driven and concurrency tests.

### ⚙️ Engineering Improvements

- **Examples reorganized into standalone top-level directories**: Moved from `CubeAPI/examples/` to top-level `examples/`, with dedicated `host-mount` and `network-policy` directories (each with its own README); comments translated to English.
- **`cube-bench` promoted to `examples/cube-bench`**: Now a standalone Go module with its own Makefile.
- **Go toolchain alignment**: `CubeVS` and `network-agent` upgraded to Go 1.24.8.
- **`cubecli` internationalization**: Remaining Chinese usage strings in `benchrun.go` translated to English.
- **Docker build context cleanup**: `Makefile` builder-image now builds from `./docker` instead of the repo root.
- **Alpine mirror swap**: APK repositories switched from `dl-cdn.alpinelinux.org` to `mirrors.tencent.com`.

### 🤖 CI / DevOps

- **DCO check workflow**: A dedicated PR gate now blocks merges if any non-merge commit is missing a valid `Signed-off-by` trailer.
- **GitHub ARC (Actions Runner Controller) support**: Self-hosted ARC runners wired up for kernel/package build workflows.
- **No more duplicate PR checks**: `push` triggers on several workflows now scoped to `master` only; PR validation runs exclusively via `pull_request` — halving CI cost.
- **`sync-to-cnb`**: Uses the `CNB_GIT_PASSWORD` secret.

### 📚 Documentation

- **Deployment guide reworked**: PVM and bare-metal are now presented as the preferred deployment paths.
- **PVM rapid-deploy on OpenCloudOS 9**: New step-by-step section added to `pvm-deploy.md`.
- **"About us" page**: English and Chinese versions added, with corresponding VitePress navigation.
- **X (Twitter) link** added to project READMEs.
- **Docs polish**: Python import paths and architecture-diagram spacing corrected.
- **WeChat / assistant QR codes** refreshed in `README_zh.md`.

---

## 2026.05.07 Release v0.2.0

### 🌟 Major New Features

- **Web Management Console (Dashboard)**: A brand-new visual management UI with cluster overview, node and sandbox status, template management, and API key management; new CubeAPI web endpoints added to back the Dashboard.

- **PVM Deployment Mode**: Powered by PVM (Pagetable-based Virtual Machine), **ordinary cloud servers can now run CubeSandbox without bare-metal or nested virtualization**. Tencent Cloud has deployed and validated PVM instances at scale in production, with improvements open-sourced in the [OpenCloudOS kernel](https://gitee.com/OpenCloudOS/OpenCloudOS-Kernel.git).

### ✨ Enhancements

- **Custom DNS for template creation**: `cubemastercli template` gains a `--dns` flag, allowing a custom DNS server address to be specified when creating a template image.

### 🛠️ Critical Fixes

- **Fixed** disk QoS (blk_qos) having no effect: Cubelet was reading the QoS annotation with the wrong key, silently ignoring IOPS/bandwidth limits; limits now apply as configured.

- **Fixed** host-mount requests being silently dropped: CubeAPI wrote the annotation with key `host-mount` while CubeMaster read with `hostdir-mount`; the mismatch caused all host directory mounts to be ignored. Keys are now aligned and host-mount works correctly.

- **Fixed** Cubelet mount namespace not receiving host mount events: Cubelet created its mount namespace in private mode, blocking propagation of subsequent host mounts; changed to slave mode so host mount events propagate one-way into the Cubelet namespace without affecting the host.

- **Fixed** DeadGC permanently freezing paused sandboxes: `scanDeadContainer` issued a `state()` call to the shim while the sandbox held its mutex (during pausing/paused), causing a 5 s timeout, Cubelet marking the sandbox UNKNOWN, and CubeMaster giving up on resume. DeadGC now skips sandboxes in pausing/paused states.

### 🌐 Networking

- **Disabled virtio-net TAP offloads (TSO/UFO/CSUM)**: The hypervisor previously advertised hardware offload features to the guest; CHECKSUM_PARTIAL packets emitted by the guest could cause network errors or even disable tx-checksumming on the host NIC, affecting other tenants. The hypervisor no longer advertises these features; the guest handles checksumming and segmentation itself.

### ⚙️ Engineering Improvements

- **Cubelet CLI logging standardization**: Migrated legacy `myPrint` output in `cubecli` sub-commands (`cubebox`, `network`, `storage`, `volume`, etc.) to structured logging.
- **Dead code removal**: Removed the unused `AppId` field from CubeMaster affinityutil tests.

### 📚 Documentation Updates

- **New PVM Deployment guide** (Chinese & English): full walkthrough covering PVM host kernel installation, GRUB configuration, module loading, and verification.
- **Quick Start updated**: ordinary cloud servers can now be used via PVM — no bare-metal required.
- **Updated code-sandbox-quickstart example README** (Chinese & English).

---

## 2026.04.27 Release v0.1.2

### 🛠️ Critical Fixes

- **Fixed** the issue where CubeMaster returned a 5xx error instead of a 4xx error when the sandbox template does not exist.
- **Fixed** the missing SSL RootCA certificate issue during one-click deployment in v0.1.1.
- **Fixed** the cube proxy image build failure during deployment in v0.1.1.

## 2026.04.24 Release v0.1.1

### 🛠️ Critical Fixes

- **Fixed** the issue where the latest vmlinux was not used during template reconstruction, improving the stability of the sandbox environment.
- **Updated** the one-click installation script to support non-eth0 network interfaces, resolving stability issues with CubeProxy CA certificates.
- **Disabled** GRO on the primary network interface during initialization to enhance network stability.
- **Fixed** incorrect error handling when the target was not found during template cleanup, ensuring proper error returns.

### ✨ New Features

- **Added** the `cubebox destroy` command, enabling sandbox deletion via the CLI.
- **Added** integration examples for the OpenAI Agents SDK (including a code interpreter).

### 📚 Documentation Updates

- **Rewrote** the HTTPS and domain configuration documentation, adding explanations for wildcard DNS records.

### ⚙️ Engineering Improvements

- **Implemented** a parallel CI build pipeline for multiple components to optimize build efficiency.
- **Added** support for automatic synchronization of GitHub Release assets to `cnb.cool/CubeSandbox/CubeSandbox`.

## 2026.04.20 Release v0.1.0

### Initial open-source release of Cube Sandbox

**Instant, Concurrent, Secure & Lightweight Sandbox for AI Agents.**

### Core Highlights

Cube Sandbox is a high-performance, out-of-the-box secure sandbox
service built on RustVMM and KVM. It supports both single-node
deployment and can be easily scaled to a multi-node cluster. It is
compatible with the E2B SDK, capable of creating a hardware-isolated
sandbox environment with full service capabilities in under 60ms,
while maintaining less than 5MB memory overhead.

- Blazing-fast cold start: built on resource pool pre-provisioning
  and snapshot cloning technology, average end-to-end cold start
  time for a fully serviceable sandbox is < 60ms.

- High-density deployment on a single node: extreme memory reuse via
  CoW technology combined with a Rust-rebuilt, aggressively trimmed
  runtime keeps per-instance memory overhead below 5MB — run
  thousands of Agents on a single machine.

- True kernel-level isolation: each Agent runs with its own dedicated
  Guest OS kernel, eliminating container escape risks and enabling
  safe execution of any LLM-generated code.

- Zero-cost migration (E2B drop-in replacement): natively compatible
  with the E2B SDK interface. Just swap one URL environment variable
  — no business logic changes needed.

- Network security: CubeVS, powered by eBPF, enforces strict
  inter-sandbox network isolation at the kernel level with
  fine-grained egress traffic filtering policies.

### Production-ready 

**Cube Sandbox has been validated at scale in Tencent Cloud production
environments, proven stable and reliable** — before this day it ever
existed as open source, it had already quietly run behind real AI
Agent workloads, serving real users, at production load.

In real production deployments, a single physical machine can spin up
tens of thousands of sandboxes within minutes.

We open-source it today not as a prototype, but as production-hardened
infrastructure that has already stood the test of real-world scale.

### A Note to Every Contributor — Past, Present, and Future

Before this code was ever public, it was already doing its job:
spinning up sandboxes in milliseconds, isolating Agent workloads
at the kernel level, and holding up under real production load
at Tencent Cloud. None of that happened by accident.

Today we open the door. The high-performance Agent infrastructure
you shaped now belongs to the world — to every developer who believes
that safe, instant, and lightweight code execution should be open
and self-hostable.

To those who contributed before this day: you built the foundation.
To those who will contribute after: you are what turns a foundation
into an ecosystem.

Open source shines because of you!
