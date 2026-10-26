# cube-agent

The in-VM guest agent for Cube Sandbox. It runs as PID 1 inside each MicroVM
(after `cube-init` execs it with `wrapper_mode=on`) and manages the full
container lifecycle within the sandbox.

## Upstream

cube-agent is derived from the **[kata-containers/kata-containers](https://github.com/kata-containers/kata-containers)** agent (`src/agent/`), originally authored by the Kata Containers community and licensed under [Apache-2.0](../LICENSE).

Cube-specific modifications include:
- Replaced the Kata runtime protocol with the Cube ttrpc API (`libs/protocols/`)
- Added `cube/` sub-crate for Cube-specific sandbox extensions (custom device management, configuration handling)
- Adapted vsock port assignments and boot flow to match the Cube hypervisor and shim

The original Kata Containers copyright notices and license headers are preserved in all modified source files.

## Role in the System

```
Host
┌────────────────────────────────┐
│  containerd-shim-cube-rs       │
│  pmem0 = guest OS (cube-init)  │
│  pmem1 = cube-agent.ext4       │
│       │  ttrpc over vsock      │
└───────┼────────────────────────┘
        │  (vsock channel)
   MicroVM boundary
        │
┌───────▼────────────────────────┐
│  cube-init (/sbin/init)        │
│    mounts /dev/pmem1 →         │
│      /run/support (ext4,ro,dax)│
│    exec /run/support/cube-agent│
│       │                        │
│  cube-agent (PID 1, wrapper)   │
│  ┌────▼──────────────────────┐ │
│  │  container workload       │ │
│  └───────────────────────────┘ │
└────────────────────────────────┘
```

cube-agent is packaged as an independent **`cube-agent.ext4`** plane file
(containing only `/cube-agent`). CubeShim injects it as virtio-pmem1. Guest
Image contains lightweight `cube-init` as `/sbin/init`, which mounts pmem1 and
execs the agent with `wrapper_mode=on` so the agent skips duplicate
`general_mount`.

When the MicroVM boots, the agent:

1. **Skips init mounts when `wrapper_mode=on`** — `cube-init` already prepared `/proc`, `/sys`, `/run`, etc.
2. **Notifies shim via SysCtrl** — writes `VsockServerReady` (x86 PIO `0x680` / aarch64 MMIO `0x0903_0000`). This is **not** the template `SysStart` handshake; open-source `cube-init` does not perform snapshot-mode SysCtrl handshake.
3. **Listens for ttrpc commands** — exposes the Cube agent API over a vsock channel; the shim (`containerd-shim-cube-rs`) connects to this channel to drive the agent.
4. **Manages container lifecycle** — handles `CreateContainer / StartContainer / ExecProcess / SignalProcess / RemoveContainer` requests, delegating to `rustjail` for OCI-compliant container execution.
5. **Forwards I/O** — proxies container stdio streams back to the shim over vsock.
6. **Exposes metrics** — exports Prometheus-compatible metrics for guest CPU, memory, and container health.

## Repository Layout

```
agent/
├── src/             # Agent binary (main entry point, ttrpc server, sandbox/container state)
│   ├── main.rs      # Startup, vsock listener, ttrpc server init
│   ├── rpc.rs       # ttrpc service handlers (implements the Cube agent API)
│   ├── sandbox.rs   # Sandbox state management
│   ├── mount.rs     # Filesystem mount handling
│   ├── network.rs   # Guest network configuration
│   └── ...
├── rustjail/        # OCI container runtime primitives (namespaces, cgroups, seccomp)
├── cube/            # Cube-specific extensions (device model, config)
├── libs/
│   ├── protocols/   # ttrpc API definitions (protobuf) and generated Rust bindings
│   ├── oci/         # OCI spec types
│   ├── logging/     # slog-based logging setup
│   └── safe-path/   # Safe filesystem path utilities
├── vsock-exporter/  # Prometheus metrics exporter over vsock
├── bootstrap.sh     # One-time musl toolchain setup
└── build.sh         # Release build script (musl static binary)
```

See also `../guest-init/` for the lightweight Guest PID 1 (`cube-init`).

## Build

cube-agent is built as a **statically linked musl binary**, then packaged into
`cube-agent.ext4` by `deploy/one-click/build-agent-ext4.sh`.

### Prerequisites

```bash
# Install the musl target and bootstrap system dependencies (run once)
arch=$(uname -m)
rustup target add "${arch}-unknown-linux-musl"
sudo ln -s /usr/bin/g++ /bin/musl-g++
sudo bash bootstrap.sh
```

### Compile

```bash
bash build.sh
```

The output binary is placed at `target/<arch>-unknown-linux-musl/release/cube-agent`.

### Build via Docker (recommended for CI)

```bash
make all-docker
# or from repo root:
make agent
```

### Package cube-agent.ext4 via root Makefile (recommended)

```bash
# From repo root — builds musl-static cube-agent inside the builder image,
# then packages _output/cube-agent/{cube-agent.ext4,version}:
make agent-ext4
# aliases:
make cube-agent-ext4
# with guest PID1 as well:
make pmem-assets
```

Or call the script directly after a prebuilt binary is available:

```bash
OUTPUT_DIR=/tmp/cube-agent ./deploy/one-click/build-agent-ext4.sh
```

## API

cube-agent exposes a [ttrpc](https://github.com/containerd/ttrpc) API over vsock. The protocol is defined in protobuf files under `libs/protocols/protos/` and is shared with the Cube runtime (`CubeShim`).

To regenerate protocol bindings after changing `.proto` files:

```bash
# Rust bindings (auto-generated at build time by build.rs)
cargo build

# Go bindings (used by the Cube runtime side)
make generate-protocols   # requires protoc in $PATH
```

To install `protoc`:

```bash
# Debian/Ubuntu
sudo apt-get install -y protobuf-compiler

# Fedora/CentOS/RHEL
sudo dnf install -y protobuf-compiler
```

## License

Apache-2.0 — see [LICENSE](../LICENSE) for details.

This component incorporates code from the [Kata Containers](https://github.com/kata-containers/kata-containers) project, © The Kata Containers Authors, licensed under Apache-2.0.
