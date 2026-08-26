# s3_rpc_smoke

A **standalone** smoke-test binary that drives every one of the 11 JSON-RPC
methods exposed by the s3lvol / RCOW server over its Unix domain socket, as
specified in [`docs/design/zh/s3lvol-rpc.md`](../../docs/design/zh/s3lvol-rpc.md).

This crate is intentionally isolated from the outer `cubecow` workspace
(see `[workspace]` in `Cargo.toml`) so it can be built independently of
the main library — in particular, it can be cross-compiled to
`x86_64-unknown-linux-musl` and shipped as a single **statically-linked**
binary to any x86_64 Linux host regardless of the host's glibc version.

## Build

### 1. Dynamic (glibc) release — small, needs matching glibc on target

```bash
cd examples/s3_rpc_smoke
cargo build --release
# produces: target/release/s3_rpc_smoke
```

### 2. Static musl release — recommended for "scp and run" workflow

Prerequisite (one-off):

```bash
rustup target add x86_64-unknown-linux-musl
```

Build:

```bash
cd examples/s3_rpc_smoke
cargo build --release --target x86_64-unknown-linux-musl
# produces: target/x86_64-unknown-linux-musl/release/s3_rpc_smoke
```

The resulting binary is `statically linked` (`ldd` reports
"statically linked"), stripped, ~630 KB. It has **no** shared-library
dependencies — copy it to any x86_64 Linux box and run.

## Deploy & run on the target host

```bash
# From your dev machine:
scp target/x86_64-unknown-linux-musl/release/s3_rpc_smoke \
    user@target-host:/tmp/

# On the target host (must be able to reach /var/run/s3lvol.sock):
chmod +x /tmp/s3_rpc_smoke
/tmp/s3_rpc_smoke --help
/tmp/s3_rpc_smoke                              # full 11-step run
/tmp/s3_rpc_smoke --socket /custom/path.sock   # non-default socket
/tmp/s3_rpc_smoke --skip-export                # no COS environment
/tmp/s3_rpc_smoke --keep                       # keep artefacts on exit
```

## Exit codes

| Code | Meaning                                                        |
|------|----------------------------------------------------------------|
| 0    | All 11 RPC steps reported `[ OK ]` in the summary              |
| 1    | At least one step failed; details in the summary block         |
| 2    | Invalid CLI arguments                                          |
| 3    | Could not connect to the s3lvol Unix socket                    |
