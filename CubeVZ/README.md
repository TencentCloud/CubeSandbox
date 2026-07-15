# CubeVZ

CubeVZ is the macOS/Apple Silicon backend prototype for CubeSandbox. It runs an
ARM64 Linux sandbox directly on Apple `Virtualization.framework`:

```text
macOS process (cube-vz)
        |
        | Virtualization.framework / Apple Hypervisor
        v
ARM64 Linux sandbox VM
```

There is no Linux host VM and no nested KVM layer. The Linux guest remains the
CubeSandbox isolation boundary, so this is still hardware virtualization, but
it is exactly one layer from macOS to the sandbox guest.

## What is implemented

- Direct ARM64 Linux kernel boot with `VZLinuxBootLoader`.
- Raw root disk attached as virtio-blk.
- APFS `clonefile(2)` copy-on-write disks for cheap per-sandbox creation.
- Virtio console on the invoking terminal.
- Virtio entropy, memory balloon, vsock, and NAT networking.
- Stable generic machine identifiers.
- Per-sandbox cold cloning with a fresh machine identifier and a recycled,
  concurrency-safe MAC address.
- Guest-to-host vsock readiness/shutdown control.
- A loopback CubeAPI-compatible lifecycle server implementing the `POST` and
  `DELETE /sandboxes` contract used by the official `cube-bench` tool.
- A separate loopback data-plane listener that transparently relays the SDK's
  envd traffic over virtio-vsock. It reads the Host header only for routing and
  forwards the original bytes unchanged; CubeVZ does not implement envd HTTP.
- A minimal ARM64 guest artifact assembled from the repository's existing
  `docker/Dockerfile.cube-base`, containing the real upstream envd service.
- A host readiness check that includes architecture, framework support, and the
  required code-signing entitlement.

## Requirements

- Apple Silicon Mac (`arm64`).
- macOS 14 or newer.
- Xcode Command Line Tools with Swift 6.2 or newer.
- Docker for the reproducible CubeVZ guest build. It compiles the pinned ARM64
  Linux 6.12.95 kernel directly from kernel.org.
- A raw block image containing the guest root filesystem. The existing
  `cube-guest-image-cpu.img` is raw ext4 and is suitable; qcow2 is not.

The checked-in ARM64 kernel config enables the required virtio block, network,
console, vsock, PCI, and ext4 drivers as built-ins, so the lifecycle guest boots
its ext4 root disk without an initramfs.

## Build and verify

From the repository root:

```bash
make cube-vz-test
make cube-vz-doctor
make cube-vz-guest
make cube-vz-smoke
make cube-vz-benchmark
make cube-vz-lifecycle-benchmark
```

`make cube-vz` writes the signed release binary to `_output/bin/cube-vz`.
The Make target intentionally runs natively instead of inside the Linux builder
container because `Virtualization.framework` exists only on macOS.

`make cube-vz-guest` builds `_output/cube-vz/guest/{kernel,rootfs.raw}`.
`make cube-vz-smoke` then verifies the complete minimal path:

```text
CubeSandbox Go SDK -> cube-vz-api -> data-plane proxy -> vsock relay -> envd
```

The control plane listens on the configured API port. The data plane listens
on the next loopback port, so an API port of `3000` uses data-plane port `3001`.
For the Go SDK, set `CUBE_PROXY_NODE_IP=127.0.0.1`,
`CUBE_PROXY_PORT_HTTP=3001`, and `CUBE_SANDBOX_DOMAIN=cube.local`.

Do not run the raw SwiftPM executable directly for VM operations unless you
codesign it first. macOS requires the
`com.apple.security.virtualization` entitlement; the Make targets apply it with
an ad-hoc signature for local development.

## Run the M4 benchmark

`make cube-vz-benchmark` performs the complete path:

1. Builds a reproducible Alpine ARM64 guest with Docker when artifacts are
   missing. Docker is used only to assemble the Linux kernel, initramfs, and raw
   ext4 image.
2. Creates an APFS copy-on-write VM directory.
3. Boots that guest directly through `Virtualization.framework`, without
   Docker or a Linux host VM in the measured execution path.
4. Runs sysbench CPU, memory, and direct random file-I/O workloads, powers the
   guest down, and writes the console log plus a Markdown report under
   `_output/cube-vz/benchmark-results/`.

Force a guest artifact rebuild with:

```bash
CUBEVZ_BENCH_REBUILD_GUEST=1 make cube-vz-benchmark
```

The benchmark defaults to 2 vCPUs and 2048 MiB. Override them with
`CUBEVZ_BENCH_VCPUS` and `CUBEVZ_BENCH_MEMORY_MIB`. A measured baseline from
the implementation host is recorded in [Benchmark/RESULTS.md](Benchmark/RESULTS.md).

## Run the official lifecycle benchmark

`make cube-vz-lifecycle-benchmark` builds and signs both native binaries,
creates an immutable ARM64 Linux disk template containing the real CubeSandbox
envd service, cross-compiles the repository's official `examples/cube-bench`
binary for macOS ARM64, and runs these tiers:

- concurrency 1, 20 create/delete cycles, 3 warmups;
- concurrency 10, 200 create/delete cycles, 3 warmups.

The measured POST includes APFS template cloning, cold VM construction/start,
and readiness of the real envd service behind the guest vsock relay. Results
and per-phase timings are written under
`_output/cube-vz/lifecycle-results/<timestamp>/`. The measured path is native
macOS → `Virtualization.framework` → ARM64 Linux; Docker is only used before
measurement to assemble guest artifacts and cross-compile `cube-bench`.

Every create follows the same cold path: APFS clone, fresh machine identifier,
VM start, then envd readiness over vsock. There is no saved state, adaptive
branch, hot pool, or prewarmed VM. Sandboxes recycle inactive MAC addresses so
VZNAT does not accumulate an unbounded set of short-lived DHCP identities.

DHCP runs in the background because CubeVZ's control and data planes use vsock.
POST therefore guarantees that envd is usable, but an outbound Internet command
issued immediately after POST may need to retry briefly while VZNAT assigns an
address. Guest timing metadata records init, envd, and final READY milestones.

## Create and run a sandbox VM

```bash
_output/bin/cube-vz create \
  --vm-dir .workdir/cube-vz/demo \
  --kernel _output/kernel/aarch64/vmlinux \
  --disk /absolute/path/to/cube-guest-image-cpu.img \
  --cpus 2 \
  --memory-mib 2048

_output/bin/cube-vz run --vm-dir .workdir/cube-vz/demo
```

The default command line is `console=hvc0 root=/dev/vda rw`. Override it with
`--cmdline` if the guest image needs additional kernel parameters.

`Ctrl-C`, `SIGINT`, or `SIGTERM` stops the VM. A later `run` always performs a
fresh cold boot.

## Current boundary

CubeVZ owns only the Apple Virtualization.framework lifecycle and the transport
needed to reach envd. The real envd remains inside the CubeSandbox guest, so SDK
commands and filesystem APIs are not reimplemented in Swift. The minimal guest
exposes only envd port `49983`.

Jupyter/code-interpreter (`49999`), arbitrary exposed ports, network policy,
pause/rollback, authentication, AgentHub, persistence, OCI template management,
and distributed scheduling remain separate components and are intentionally not
part of this minimal backend. The Linux containerd/CubeShim implementation is
not run inside another Linux host VM; `cube-vz-api` replaces only its lifecycle
boundary with a native backend.
