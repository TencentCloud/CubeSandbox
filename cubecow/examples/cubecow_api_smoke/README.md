# cubecow_api_smoke

An **end-to-end** smoke test that drives every one of the 16 subcommands
exposed by [`cubecow-cli`](../../src/bin/cubecow_cli.rs), which in turn
covers every method of the public `cubecow::Engine` trait.

This is the CLI counterpart to [`examples/s3_rpc_smoke`](../s3_rpc_smoke/):

| Smoke binary            | What it drives                                        |
|-------------------------|-------------------------------------------------------|
| `s3_rpc_smoke`          | The raw 11 JSON-RPC methods of the s3lvol daemon      |
| `cubecow_api_smoke`     | The 16 subcommands of `cubecow-cli` (Engine trait)    |

Just like `s3_rpc_smoke`, this crate carries its own `[workspace]`
marker in `Cargo.toml` so it is **isolated** from the outer `cubecow`
workspace. It only depends on `serde_json` (for parsing the JSON that
`cubecow-cli --json` prints on stdout), so a `cargo build --release`
here does *not* recompile the cubecow lib and the resulting binary can
be shipped independently — including cross-compiled to
`x86_64-unknown-linux-musl` for a "scp and run" workflow.

## What the test does

The 15 numbered steps mirror the lifecycle in
`docs/design/zh/cubecow-api.md` §5:

|  # | Subcommand                     | Notes                                                        |
|---:|--------------------------------|--------------------------------------------------------------|
|  1 | `create-volume`                | + idempotency probe (2nd create must fail with EEXIST)       |
|  2 | `get-volume-info`              | verify size_bytes matches request                            |
|  3 | `get-volume-block-info`        | dump num_blocks / block_size                                 |
|  4 | `resize-volume`                | grow the volume                                              |
|  5 | `list-volumes`                 | confirm the created volume appears in the listing            |
|  6 | `create-snapshot`              | activated snapshot off the volume                            |
|  7 | `list-snapshots`               | confirm the snapshot appears under the origin volume         |
|  8 | `create-volume-from-snapshot`  | clone the snapshot into a writable volume                    |
|  9 | `deactivate-volume`            | + idempotency probe (2nd deactivate must also succeed)       |
| 10 | `activate-volume`              | re-attach the snapshot                                       |
| 11 | `metrics`                      | dump backend metrics                                         |
| 12 | `export-snapshot`              | **S3 backend only**; skipped for reflink                     |
| 13 | `get-volume-info` (status)     | poll `export_status = DONE` when exported                    |
| 14 | `import-lvol`                  | **S3 backend only**; skipped for reflink                     |
| 15 | `delete-snapshot` + `delete-volume` | cleanup + idempotency (2nd delete must yield "not found") |

A best-effort cleanup pass runs on exit (both success and failure) so a
partial run does not leak names into the backend.

## Build

### 1. Build the `cubecow-cli` binary from the workspace root

```bash
cd <cube-sandbox>/cubecow
cargo build --release --bin cubecow-cli
# produces: target/release/cubecow-cli
```

### 2. Build the smoke binary

```bash
cd examples/cubecow_api_smoke
cargo build --release
# produces: target/release/cubecow_api_smoke
```

### Optional: static musl build

```bash
rustup target add x86_64-unknown-linux-musl                  # one-off
cd examples/cubecow_api_smoke
cargo build --release --target x86_64-unknown-linux-musl
# produces: target/x86_64-unknown-linux-musl/release/cubecow_api_smoke
```

The resulting musl binary is statically linked and has no shared-library
dependencies — copy it to any x86_64 Linux host that already has a
working `cubecow-cli` and run.

## Run

The typical invocation on a host that already carries a cubecow TOML
config at `/etc/cubecow/cubecow.toml`:

```bash
./target/release/cubecow_api_smoke \
    --cubecow-cli ../../target/release/cubecow-cli \
    --config /etc/cubecow/cubecow.toml
```

More options:

```bash
# Point at an alternate CLI binary + JSON-formatted config file:
cubecow_api_smoke \
    --cubecow-cli /usr/local/bin/cubecow-cli \
    --json-config /etc/cubecow/cubecow.json

# Inline config: minimal reflink backend at a custom root_dir:
cubecow_api_smoke \
    --cubecow-cli /usr/local/bin/cubecow-cli \
    --json-config-inline '{"log":{},"backend":{"kind":"reflink","reflink":{"root_dir":"/tmp/cbc"}}}'

# Exercise the S3 backend end-to-end, including export/status/import,
# waiting up to 300s for the async COS upload to reach DONE:
cubecow_api_smoke \
    --cubecow-cli /usr/local/bin/cubecow-cli \
    --config /etc/cubecow/cubecow-s3.toml \
    --backend s3 \
    --upload-timeout-secs 300

# Keep created artefacts for offline inspection:
cubecow_api_smoke --config /etc/cubecow/cubecow.toml --keep
```

## Exit codes

| Code | Meaning                                                        |
|------|----------------------------------------------------------------|
| 0    | Every non-skipped step reported `[ OK ]` in the summary        |
| 1    | At least one step failed; details in the `SUMMARY` block       |
| 2    | Invalid CLI arguments                                          |
| 3    | Could not reach the cubecow engine via `cubecow-cli metrics`   |
