# CubeShim

CubeShim is the containerd shim component of Cube Sandbox, implemented in Rust. It bridges containerd with the Cube sandbox VM lifecycle, implementing the [containerd Shim v2 API](https://github.com/containerd/containerd/blob/main/core/runtime/v2/README.md) via [`containerd-shim-rs`](https://github.com/containerd/containerd-shim-rs).

## Role in the System

```
containerd
    │  (Shim v2 API)
    ▼
containerd-shim-cube-rs   ← CubeShim (this component)
    │  (ttrpc)
    ▼
cube-agent                ← in-VM guest agent
    │
    ▼
container workload        ← running inside the MicroVM
```

When containerd needs to create a sandbox, it spawns `containerd-shim-cube-rs` as a subprocess. CubeShim then:

1. **Requests VM creation** — communicates with Cubelet (via the Cube runtime) to launch a KVM MicroVM with the appropriate resources (CPU, memory, disk).
2. **Bridges the shim API** — exposes the containerd Shim v2 interface upward, hiding all VM-level complexity from containerd.
3. **Manages container lifecycle** — forwards `Create / Start / Exec / Kill / Delete` calls down to the in-VM `cube-agent` over ttrpc/vsock.
4. **Handles I/O and signaling** — proxies stdio streams and forwards signals between the host and the container process inside the VM.
5. **Reports sandbox state** — tracks sandbox status and surfaces it back to containerd through the Shim v2 event model.

## Documentation

| Topic | Path |
|-------|------|
| Extended shim API (shimapi) | [`docs/shimapi/`](./docs/shimapi/README.md) |

## Sub-components

| Directory | Binary | Description |
|-----------|--------|-------------|
| `shim/` | `containerd-shim-cube-rs` | The shim process itself; implements Shim v2 |
| `cube-runtime/` | `cube-runtime` | CLI helper invoked by Cubelet to perform snapshot/restore operations on the VM |
| `protoc/` | — | Internal protobuf code-generation helper (build-time only) |

## Key Dependencies

| Crate | Purpose |
|-------|---------|
| [`containerd-shim`](https://crates.io/crates/containerd-shim) | Shim v2 framework (async) |
| [`containerd-shim-protos`](https://crates.io/crates/containerd-shim-protos) | Generated protobuf bindings for the Shim v2 API |
| [`ttrpc`](https://crates.io/crates/ttrpc) | ttrpc client for communicating with `cube-agent` inside the VM |
| `tokio` | Async runtime |

## Build

```bash
# Debug build
cargo build

# Release build (used in production)
cargo build --release --locked

# Or via the Makefile (builds inside Docker for reproducibility)
make all-docker
```

The release binary is output to `target/release/containerd-shim-cube-rs`.

## Development Notes

### Rust Toolchain

CubeShim uses **Rust 1.77.2** (pinned in `rust-toolchain.toml`). If `rust-analyzer` in VS Code reports a version mismatch, pin it explicitly:

```json
// .vscode/settings.json
{
    "rust-analyzer.server.path": "~/.rustup/toolchains/1.77.2-x86_64-unknown-linux-gnu/bin/rust-analyzer"
}
```

### Registration with containerd

After installation, containerd must be configured to route `cube` runtime requests to this shim:

```toml
# /etc/containerd/config.toml
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.cube]
  runtime_type = "io.containerd.cube.v2"
```

The shim binary must be on `$PATH` or explicitly configured so containerd can locate it.

## Third-party Protocol Definitions

The `.proto` files under `protoc/protos/` are derived from the
[Kata Containers](https://github.com/kata-containers/kata-containers) project
(`src/agent/libs/protocols/protos/`), originally authored by HyperHQ Inc.,
Intel Corporation, and Ant Group.

| File | Upstream source | Original authors |
|------|----------------|-----------------|
| `agent.proto` | [`kata-containers/kata-containers`](https://github.com/kata-containers/kata-containers/blob/main/src/agent/libs/protocols/protos/agent.proto) | HyperHQ Inc., Ant Group |
| `oci.proto` | [`kata-containers/kata-containers`](https://github.com/kata-containers/kata-containers/blob/main/src/agent/libs/protocols/protos/oci.proto) | Intel Corporation, Ant Group |
| `types.proto` | [`kata-containers/kata-containers`](https://github.com/kata-containers/kata-containers/blob/main/src/agent/libs/protocols/protos/types.proto) | Intel Corporation, Ant Group |
| `health.proto` | [`kata-containers/kata-containers`](https://github.com/kata-containers/kata-containers/blob/main/src/agent/libs/protocols/protos/health.proto) | HyperHQ Inc., Ant Group |
| `csi.proto` | [`kata-containers/kata-containers`](https://github.com/kata-containers/kata-containers/blob/main/src/agent/libs/protocols/protos/csi.proto) | — |

All of the above files are licensed under **Apache-2.0**. The original
copyright notices and `SPDX-License-Identifier: Apache-2.0` headers are
preserved in each file.

## License

Apache-2.0 — see [LICENSE](../LICENSE) for details.

This component incorporates protobuf protocol definitions from the
[Kata Containers](https://github.com/kata-containers/kata-containers) project,
© The Kata Containers Authors, licensed under Apache-2.0.
