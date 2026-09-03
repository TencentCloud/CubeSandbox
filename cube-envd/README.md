# cube-envd

> **中文文档**: [README_zh.md](./README_zh.md)

`cube-envd` is the E2B-compatible data-plane daemon that runs inside each
CubeSandbox sandbox. It provides the in-guest runtime used by the CubeSandbox
SDK and E2B SDK for running commands, reading and writing files, manipulating
the filesystem, opening PTY terminals, and initializing create-time
environment variables.

By default it listens on `0.0.0.0:49983`. Its `GET /health` endpoint returns
`204 No Content` once the service is ready, which makes it suitable as the
template readiness probe:

```bash
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:49983/health
# => 204
```

## Role in the System

```
User SDK / E2B SDK
        │  HTTPS through CubeProxy / direct sandbox data-plane route
        ▼
   container port 49983
        │
        ▼
   cube-envd (this component, inside the sandbox)
        │  ┌───────────────────────┐
        ├──│ process execution     │  commands, PTY, signals, stdio
        │  └───────────────────────┘
        │  ┌───────────────────────┐
        ├──│ file / filesystem I/O │  upload, download, stat, watch, mkdir, ...
        │  └───────────────────────┘
        │  ┌───────────────────────┐
        └──│ environment snapshot  │  /init create-time env vars
           └───────────────────────┘
```

`cube-envd` is usually installed at `/usr/bin/envd` in the
`cubesandbox-base` image and started by
[`docker/cube-entrypoint.sh`](../docker/cube-entrypoint.sh). It can also be
injected into a custom template with
`cubemastercli tpl create-from-image --enable-inject-envd`.

## API

`cube-envd` exposes a small HTTP API on port `49983`. Most RPC methods use
the [Connect protocol](https://connectrpc.com/) with protobuf JSON payloads;
the protocol definitions live under [`proto/`](./proto) and the generated
interface reference is in [`doc/cube-envd-api.md`](./doc/cube-envd-api.md).

### Health

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Returns `204` when `cube-envd` can accept SDK/data-plane requests. |

### Environment

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/init` | Atomically replaces the default environment snapshot. Body: `{"envVars": {"KEY": "value"}}`. |
| `GET` | `/envs` | Returns the current environment snapshot as JSON. |

### Files

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/files?path=...&username=...` | Streams a regular file from disk. |
| `POST` | `/files?path=...&username=...` | Uploads a file with `application/octet-stream` or `multipart/form-data`. Writes are atomic (temp file + rename). |

### Process RPCs

These endpoints implement the `process.Process` service:

| Endpoint | Kind | Description |
|----------|------|-------------|
| `/process.Process/Start` | streaming | Start a command or PTY and stream output/exit events. |
| `/process.Process/List` | unary | List live processes managed by `cube-envd`. |
| `/process.Process/Connect` | streaming | Subscribe to a live process or replay a recently ended process (by PID or tag). |
| `/process.Process/Update` | unary | Resize a PTY. |
| `/process.Process/StreamInput` | streaming | Multi-frame client input stream to a selected process. |
| `/process.Process/SendInput` | unary | Write one stdin or PTY input chunk. |
| `/process.Process/SendSignal` | unary | Send `SIGNAL_SIGTERM` or `SIGNAL_SIGKILL` to a process group. |
| `/process.Process/CloseStdin` | unary | Close a normal process stdin (EOF); not valid for PTY processes. |

### Filesystem RPCs

These endpoints implement the `filesystem.Filesystem` service:

| Endpoint | Kind | Description |
|----------|------|-------------|
| `/filesystem.Filesystem/Stat` | unary | Return metadata for a file/directory/symlink. |
| `/filesystem.Filesystem/MakeDir` | unary | Create a directory and missing parents. |
| `/filesystem.Filesystem/Move` | unary | Rename/move a file or directory. |
| `/filesystem.Filesystem/ListDir` | unary | List a directory with optional recursive depth. |
| `/filesystem.Filesystem/Remove` | unary | Delete a file or recursively delete a directory. |
| `/filesystem.Filesystem/WatchDir` | streaming | Watch a directory and stream create/write/remove/rename/chmod events. |
| `/filesystem.Filesystem/CreateWatcher` | unary | **Not implemented** — returns an unimplemented RPC error. |
| `/filesystem.Filesystem/GetWatcherEvents` | unary | **Not implemented** — returns an unimplemented RPC error. |
| `/filesystem.Filesystem/RemoveWatcher` | unary | **Not implemented** — returns an unimplemented RPC error. |

### Protocol notes

- Unary RPCs require `Content-Type: application/json` and
  `Connect-Protocol-Version: 1`; the JSON message is sent directly in the
  HTTP body.
- Streaming RPCs require `Content-Type: application/connect+json` and
  `Connect-Protocol-Version: 1`.
- Streaming Connect frames use a 1-byte flag header, a 4-byte big-endian
  payload length, and the JSON payload. The end-stream flag is `0x02`.
- Maximum streaming frame size is 16 MiB; maximum unary JSON body is 1 MiB.
- `Connect-Timeout-Ms` can be used to set an optional process timeout.
- `Keepalive-Ping-Interval` controls server keepalive frames for idle
  streaming RPCs.

## User and Path Resolution

- For RPC endpoints, a Basic `Authorization` header selects the local Unix
  user that owns the operation. If the header is absent, `root` is used.
  The password portion of the Basic header is ignored.
- For `/files`, the local user can be selected with the `username` query
  parameter (default: `root`).
- Relative paths and `~/...` paths are resolved against the selected user's
  home directory. Absolute paths are used as-is. `~otheruser/...` is
  rejected.

Started processes run with a cleared environment, then receive the current
`/init` environment snapshot merged with any per-process `envs` from the
request. When the selected user differs from the user running `cube-envd`,
it switches credentials via `setpriv`.

## Repository Layout

```
cube-envd/
├── Cargo.toml              # Rust package manifest
├── Cargo.lock
├── Makefile                # build/install/fmt/lint/test/proto-doc targets
├── build.rs                # generates Rust protobuf bindings at build time
├── rust-toolchain.toml     # pinned Rust toolchain (1.89)
├── proto/
│   ├── process/            # process.Process protobuf definitions
│   └── filesystem/         # filesystem.Filesystem protobuf definitions
├── src/
│   ├── main.rs             # CLI entry point and HTTP server bootstrap
│   ├── app.rs              # Axum router and shared application state
│   ├── auth.rs             # Basic auth and local user resolution
│   ├── paths.rs            # safe path resolution
│   ├── connect.rs          # Connect protocol framing and errors
│   ├── wire.rs             # protobuf JSON/domain conversions
│   ├── logging.rs          # JSON structured logging setup
│   ├── process/            # process lifecycle, PTY, input/output streaming
│   ├── filesystem/         # filesystem RPCs and file transfer
│   └── generated/          # generated protobuf Rust types
├── tests/                  # CLI, HTTP, RPC, and process integration tests
└── doc/
    └── cube-envd-api.md    # generated protocol reference
```

## Build

`cube-envd` is a Rust binary compiled as a static musl release.

### From this directory

```bash
# Build static release
make build

# Run tests
make test

# Format check / lint
make fmt
make lint

# Install into a custom directory
make install BINDIR=/path/to/bin

# Regenerate doc/cube-envd-api.md (requires protoc-gen-doc)
make proto-doc
```

### From the repository root

```bash
make cube-envd
```

This builds the static `cube-envd` inside the CubeSandbox builder container
and installs it to `_output/bin/cube-envd`.

### Base image

The `cubesandbox-base` image is built from
[`docker/Dockerfile.cube-base`](../docker/Dockerfile.cube-base); that
Dockerfile compiles this crate and installs the resulting binary as
`/usr/bin/envd`.

## CLI

```
envd [OPTIONS]
```

| Option | Default | Description |
|--------|---------|-------------|
| `-port`, `--port` | `49983` | Port for the HTTP server. |
| `-isnotfc`, `--isnotfc` | — | Kept for Firecracker compatibility. In CubeSandbox it tells envd to skip the Firecracker MMDS lookup at `169.254.169.254`; use it when starting manually. |
| `-version`, `--version` | — | Print the version and exit. |
| `-commit`, `--commit` | — | Print the build commit and exit. |

Single-dash legacy flags (`-port`, `-isnotfc`, `-version`, `-commit`) are
normalized to their long forms for compatibility.

Example manual start:

```bash
/usr/bin/envd -port 49983 -isnotfc >/var/log/envd.log 2>&1 &
```

## Development Notes

### Rust toolchain

The repository pins Rust `1.89` in `rust-toolchain.toml` with
`x86_64-unknown-linux-musl` and `aarch64-unknown-linux-musl` targets.

### Logging

Set `RUST_LOG` to control the log filter (default: `info`). Logs are emitted
as structured JSON.

### Tests

```bash
make test
```

The test suite covers CLI compatibility, health checks, Connect framing,
process startup/PTY/input/signal handling, filesystem RPCs, file uploads,
directory watching, auth/path resolution, and graceful shutdown.

## Related Documentation

- [Custom Template Images](../docs/guide/tutorials/bring-your-own-image.md)
- [Templates Overview](../docs/guide/templates.md)
- [Protocol Documentation](./doc/cube-envd-api.md)

## License

Apache-2.0 — see [LICENSE](../LICENSE) for details.
