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

## Why

`envd` runs inside every sandbox and is the compatibility boundary between the
E2B SDKs and the CubeSandbox runtime. Consuming it from `e2b-dev/infra` meant
the roadmap, the fix cadence and the release schedule were owned by another
project, and that the binary carried integration paths CubeSandbox never uses
(Firecracker MMDS, Hyperloop, NFS volume init).

`cube-envd` replaces it with a CubeSandbox-owned Rust implementation that keeps
the wire contract: the protocol definitions under [`proto/`](./proto) stay
SDK-compatible, and every place this implementation deliberately behaves
differently from the Go baseline is enumerated with its reasoning under
[Declared differences](#declared-differences) — a table generated from
[`tests/e2e/cube_envd/conformance/declared_differences.toml`](../tests/e2e/cube_envd/conformance/declared_differences.toml)
and checked in CI, so the claim cannot drift from the measurement. The upstream
Go envd stays in the base image as `envd-go` and remains a runtime rollback via
`ENVD_BIN` (see [Integration notes](#integration-notes)).

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
| `POST` | `/init` | Merges create-time env vars **key by key** into the environment (the reference implementation stores each key individually, so previously set variables survive). Body: `{"envVars": {"KEY": "value"}}`; 204. |
| `GET` | `/envs` | Returns the current environment as JSON (`Cache-Control: no-store`, Go-style trailing newline). Always contains `E2B_SANDBOX=false` (`true` for Firecracker mode, which cube-envd does not implement). |

### Files

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/files?path=...&username=...` | Streams a regular file from disk with the baseline's headers (`Content-Type: application/octet-stream`, `Content-Disposition: inline; filename=…`, `Accept-Ranges: bytes`, `Last-Modified`, `Vary: Accept-Encoding`). Range/conditional requests are **not** implemented yet (see below). |
| `POST` | `/files?path=...&username=...` | Uploads a file with `application/octet-stream` or `multipart/form-data`; answers `text/plain; charset=utf-8` with the uploaded entries, byte-identical to the baseline. Writes are atomic (same-directory temp file + rename) — that keeps interrupted uploads from leaving half a file, at the cost of extra `WatchDir` events for the temp file (declared below). |

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

#### Process end events

`Start` and `Connect` streams finish with an `EndEvent`. Its shape is part of the
SDK contract and is identical for pipe and PTY processes:

| Scenario | `exitCode` | `exited` | `status` | `error` |
|----------|-----------|----------|----------|---------|
| Clean exit (`0`) | omitted (proto3 zero) | `true` | `exit status 0` | omitted |
| Non-zero exit with code `N` | `N` | `true` | `exit status N` | `exit status N` |
| Killed by signal `N` | `128 + N` | `false` (omitted) | `terminated by signal N` | `terminated by signal N` |
| Reaping failed | `-1` | `false` (omitted) | `failed to reap process` | error text |

Notes:

- The reference implementation fills `error` with the `*exec.ExitError` text on
  non-zero exits (`"exit status N"`), and that text is what SDK result objects show
  as the failure reason — so a non-zero exit carries `error` here too. A clean exit
  leaves it empty.
- Follows shell convention: a process killed by signal `N` reports `128 + N`, so
  `SIGKILL` is `137` and `SIGTERM` is `143`. The reference envd instead reports
  `-1` for signal deaths; negative values are reserved here for reaping failures.
- proto3 JSON omits zero-valued fields, so `exitCode` is absent for a clean exit
  and `exited` is absent for signal deaths. Clients must rely on `status` (and
  `exited`) to tell "exited normally" from "killed".
- The three SDKs shipped in this repository parse `status` for the exit code when
  `exitCode` is unset, so the `status` wording is load-bearing.

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
- Maximum streaming frame size is 64 MiB (the CubeSandbox SDKs' `MAX_CONNECT_ENVELOPE_SIZE`);
  maximum unary JSON body is 4 MiB. Both are our own bounds: the Go baseline sets no limit
  (see the declared differences below).
- Request-level errors on **streaming** endpoints are reported in-band: HTTP 200,
  `Content-Type: application/connect+json`, and an EndStream frame (`0x02`) carrying
  `{"error":{"code":…,"message":…}}`. Only a media-type mismatch stays an HTTP error (415 with
  an empty body). Unary endpoints answer HTTP + a Connect JSON error body; the REST surfaces
  (`/files`, `/init`) use the numeric-code body of the baseline (`{"code":404,…}` with
  `X-Content-Type-Options: nosniff`).
- Responses to `Accept-Encoding: gzip` are compressed for buffered JSON surfaces (as the
  baseline does). Streaming responses stay identity, again matching the baseline.
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

Started processes run with a cleared environment, then receive a base
environment built the same way as the reference envd: `PATH` from `cube-envd`
itself, and `HOME`, `USER` and `LOGNAME` from the selected user's passwd entry.
The current `/init` environment snapshot and any per-process `envs` from the
request are applied on top, in that order, so a request can override any of
them. When the selected user differs from the user running `cube-envd`, it
switches credentials via `setpriv`.

A started process runs in the selected user's home directory unless the request
sets `cwd`; a relative or `~/...` `cwd` is resolved against that same home. The
directory must exist.

## Resource boundaries and cgroup policy

What the MVP does **not** do, stated explicitly so nobody has to infer it from the
absence of code:

- **No per-command cgroup v2 placement.** Commands are not confined to a leaf
  cgroup, so cube-envd enforces no per-command memory or CPU budget of its own.
  The sandbox's own limits come from CubeHypervisor (the microVM), not from here.
- **No OOM attribution.** `EndEvent.oomKilled` / `killedBy: "oom"` are never
  produced; a process killed by the kernel OOM killer is reported as an ordinary
  signal death (`exitCode: 128 + 9`, `status: "terminated by signal 9"`).
- **No descendant-escape guarantee beyond the process group.** Timeout
  (`Connect-Timeout-Ms`) and `SendSignal` target the command's **process group**,
  which reaches children the command forked — that part is stricter than the Go
  baseline, which signals only the direct child (see the process end-event notes).

What it does guarantee, and why it is enough for the base image: the reference envd
only enforces per-command cgroups when the guest exposes a writable cgroup v2 tree.
In the shipped base image it does not, and the baseline logs
`falling back to no-op cgroup manager` (verified against
`/usr/bin/envd-go` from `cubesandbox-base`), so both implementations run the same
`noop` policy in that environment. The difference is only observable on a host
with a writable cgroup v2 hierarchy.

If per-command confinement is added later, the intended contract is the baseline's:
allocation failure rejects `Start` with `resource_exhausted` (never a silent
fallback to unconfined execution), and the mode in use is reported rather than
inferred.

## Security Model

`cube-envd` executes requests with **its own credentials** (root in the
`cubesandbox-base` image) and the selected user is *not* an authorization
boundary. Be precise about what each mechanism does:

- **Process execution** runs as the selected user: when that user differs from
  the user running `cube-envd`, the child is started through
  `setpriv --reuid --regid --init-groups`, so the kernel constrains what the
  command itself can touch. `setpriv` comes from util-linux and is looked up in
  `/usr/bin`, `/bin`, `/sbin` and `/usr/sbin`; Alpine and busybox additionally
  need `apk add util-linux`, because their own `setpriv` applet rejects
  `--reuid`. Requests that select the daemon's own user skip it entirely.
- **Filesystem RPCs and `/files` execute as `cube-envd`** — that is, as root in
  the standard image. The selected user decides the path base (its home for
  relative paths) and the ownership applied to created files and directories; it
  does not restrict which paths can be read, written or deleted. `Stat`,
  `ListDir`, `Move` and `Remove` are not confined to the user's home, and
  `GET /files` can stream any file `cube-envd` can open.
- **There is no per-request token.** The `Authorization: Basic` header only
  names a user to act as; it does not authenticate the caller, and any process
  inside the sandbox can name any account.
- **Access control is expected at the network boundary.** `cube-envd` listens on
  `0.0.0.0:49983` and must not be reachable by untrusted clients: the sandbox IP
  lives on a private segment and `CubeProxy` is the only public entry point,
  where the per-sandbox traffic token is enforced. Anything that can reach
  `49983` directly (including any process inside the sandbox) effectively holds
  `cube-envd`'s own privileges.

Requests are resource-bounded so a single client cannot exhaust the sandbox:
unary JSON bodies are capped at 4 MiB and stream frames at 64 MiB,
`/files` uploads are streamed, connections are capped at 1024 with a header-read
timeout, and per-process subscriber queues are bounded with slow subscribers
dropped.

## Design

The tree is a contract index and a dependency graph: a path states what is
promised, an edge states who may call whom. Dependencies point one way only:

```text
app ──▶ {filesystem, process} ──▶ {connect, wire, rest, cors, compress} ──▶ {auth, paths, init, logging, version, compat}
                   └──────────────▶ generated (usable by anyone; it depends on nothing)
```

```
cube-envd/
├── Cargo.toml              # Rust package manifest
├── Cargo.lock
├── Makefile                # build/install/fmt/lint/test/proto-gen/proto-doc targets
├── build.rs                # generates Rust protobuf bindings at build time
├── rust-toolchain.toml     # pinned Rust toolchain (1.89)
├── proto/                  # protocol definitions the wire types mirror
│   ├── process/            # process.Process
│   └── filesystem/         # filesystem.Filesystem
├── src/
│   ├── main.rs             # CLI entry point, HTTP server bootstrap, accept loop
│   ├── lib.rs              # module declarations and #![forbid(unsafe_code)]
│   ├── app.rs              # Axum router and shared application state
│   ├── auth.rs             # Basic auth and local user resolution
│   ├── paths.rs            # safe path resolution anchored on the user home
│   ├── connect.rs          # Connect framing, error model, request limits
│   ├── wire.rs             # protobuf JSON <-> domain conversions
│   ├── rest.rs             # REST-surface error bodies (distinct from Connect)
│   ├── cors.rs             # CORS response headers matching the baseline
│   ├── compress.rs         # gzip negotiation for buffered JSON surfaces
│   ├── compat.rs           # Go-compatible error vocabulary and exit strings
│   ├── init.rs             # /init environment snapshot state
│   ├── logging.rs          # JSON structured logging setup
│   ├── version.rs          # the version constant (single source of truth)
│   ├── process/            # process lifecycle, PTY, input/output streaming
│   ├── filesystem/         # filesystem RPCs, file transfer, watchers
│   └── generated/          # generated protobuf Rust types (checked in)
├── tests/                  # CLI, HTTP, RPC, process and layer-rule tests
│   └── layer_rule.rs       # asserts the dependency directions above
└── doc/
    └── cube-envd-api.md    # generated protocol reference
```

The direction is asserted by
[`tests/layer_rule.rs`](./tests/layer_rule.rs), not by the type system:
`pub(crate)` is visible to every module at the same level, so a `use` pointing
the wrong way still compiles and still passes `clippy` and `rustfmt`. The gate
is a table of `(description, owning module, forbidden modules)` in that file —
**a new layer is unconstrained until it gets a row there**.

> **Run it with a `tests` target.** `cargo test --lib` / `--bins` skip
> `tests/layer_rule.rs` and the rule silently stops being enforced. Use
> `make cube-envd-test` (or a plain `cargo test`), which include it. The repo
> has a precedent for this trap: `make hypervisor-test` passes `--lib --bins`
> and therefore never runs that component's own `tests/integration.rs`.

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

## Integration notes

- **Entrypoint contract.** [`docker/cube-entrypoint.sh`](../docker/cube-entrypoint.sh)
  starts `${ENVD_BIN:-/usr/bin/envd} -port ${ENVD_PORT:-49983} ${ENVD_EXTRA_ARGS}`
  in the background, then either `exec`s the user `CMD` or waits on envd. It
  appends `-isnotfc` for E2B command-line compatibility; that flag is a no-op
  here (see the [CLI](#cli) table).
- **Install path.** The image ships both implementations as `/usr/bin/envd`
  (this crate, the default) and `/usr/bin/envd-go` (the pinned upstream), so
  `ENVD_BIN=/usr/bin/envd-go` is a runtime rollback that needs no rebuild. This
  must stay a literal `/usr/bin/envd`: Cubelet collects the template's envd
  version by exec'ing `envd --version`, so an `ENVD_BIN` override on its own
  would leave the template annotated with the *other* implementation's version.
  The per-template mechanics are in [`docker/README.md`](../docker/README.md).
- **Keepalive cadence.** A quiet streaming RPC emits `keepalive` events so that
  proxies and load balancers do not idle-close the connection while a long
  silent command runs. The default is **90 s**, matching the Go baseline;
  `Keepalive-Ping-Interval` (request header, in seconds) lowers it per call.
  If your LB's idle timeout is shorter than the default — 60 s is a common
  value — send that header rather than relying on the default. The baseline
  behaviour is kept deliberately: see the aligned rows in
  [Declared differences](#declared-differences).

## CLI

```
envd [OPTIONS]
```

| Option | Default | Description |
|--------|---------|-------------|
| `-port`, `--port` | `49983` | Port for the HTTP server. |
| `-isnotfc`, `--isnotfc` | — | Kept for E2B command-line compatibility only. `cube-envd` has no Firecracker MMDS code (CubeSandbox uses Cloud Hypervisor, so `169.254.169.254` does not exist), so the flag is a **no-op**: with or without it, behaviour is identical. |
| `-version`, `--version` | — | Print the version and exit. |
| `-commit`, `--commit` | — | Print the build commit and exit. |

Single-dash legacy flags (`-port`, `-isnotfc`, `-version`, `-commit`) are
normalized to their long forms for compatibility.

### CLI compatibility matrix

`cube-envd` is deliberately **stricter** than the Go baseline: Go's `flag` package
stops at the first non-flag token and silently ignores it, accepts an out-of-range
`-port` (it only fails when binding), and accepts `-isnotfc=false`. A typo in
`ENVD_EXTRA_ARGS` can therefore start the daemon on defaults. Everything the
baseline *accepts* is accepted here; everything it silently ignores is a usage
error (exit code 2, message + usage on stderr).

| Input | Go envd 0.5.13 | cube-envd | Note |
|---|---|---|---|
| `-version` / `--version` | prints the version, exit 0 | same | value differs by design (see Versioning) |
| `-commit` / `--commit` | prints the commit, exit 0 | same | |
| `-port N` / `-port=N` / `--port N` | accepted (`int64`; out of range fails only at bind time) | accepted, validated as `u16`; `--port=99999` → exit 2 | intentional |
| `-isnotfc` | accepted (disables the Firecracker branches) | accepted, no-op | cube-envd has no FC code |
| `-isnotfc=false` | accepted | **rejected** (exit 2) | intentional |
| positional argument (also after `--`) | ignored, daemon starts anyway | **rejected** (exit 2) | intentional |
| unknown flag | `flag provided but not defined` + usage, exit 2 | `unexpected argument` + usage, exit 2 | wording differs, status matches |
| `-h` / `--help` | usage, exit 0 | usage, exit 0 | |

This table is verified by [`tests/cli.rs`](./tests/cli.rs) (including
`rejects_the_arguments_the_baseline_silently_ignores`) rather than by the wire
conformance harness: the harness talks HTTP, while the CLI only exists before the
socket is bound.

### Versioning

`-version` prints the semver constant in
[`src/version.rs`](./src/version.rs), which is the single source of truth for
this component's version — the same shape as the reference envd's
`packages/envd/pkg/version.go`. It is deliberately **not** derived from the git
tag or the CI run, because downstream consumers parse it:

- Cubelet and CubeMaster extract it with `\d+\.\d+\.\d+` and persist it as the
  `cube.master.components.envd.version` annotation, which is surfaced as the
  public `envdVersion` field on sandbox info.
- The reference envd also compares this value against minimum-version gates and
  treats a malformed value as an error rather than as "older", so a build
  identifier such as `sha-1a2b3c4` is worse than useless here.

Build identity travels separately through `CUBE_ENVD_COMMIT` and is printed by
`-commit`. `CUBE_ENVD_VERSION` still exists as an explicit override for release
tooling, but nothing injects it by default and an empty value falls back to the
constant. To release, bump the constant; `make version-check` asserts it is a
semver and that the built binary agrees with it.

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

### Blocking strategy

The daemon runs one Tokio multi-thread runtime with **2 worker threads** and a
**64-thread blocking pool** (`src/main.rs::build_runtime`). Both numbers are
deliberate for a process that exists once per sandbox rather than once per host:

- the filesystem RPCs use `tokio::fs`, i.e. one blocking-pool crossing **per
  syscall**; a handful of paths that must hold a raw fd or do libc calls
  (ownership changes, user lookup, PTY setup/reap) use `spawn_blocking` directly;
- the default 512-thread blocking pool would mean multimegabyte thread stacks at
  full occupancy, out of proportion for a guest daemon.

Measured with `tests/blocking_strategy.rs` (manual, `--ignored`; this host):
`tokio::fs::metadata` median **34.3 µs/call** versus
`spawn_blocking` + synchronous `std::fs` median **24.2 µs/call** (~30% cheaper per
call, because it pays one pool crossing per request instead of per syscall).

**Revisit clause.** If the filesystem surface grows heavier or RSS measurements
show the pool matters, the intended replacement is the baseline's shape: one
blocking crossing per request, synchronous `std::fs` inside, and an in-flight
counter. Re-run the measurement above before and after that change — the numbers,
not this paragraph, decide it. Either way the pool stays bounded: unbounded
blocking was rejected because it multiplies per sandbox.

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
- [Conformance harness](./../tests/e2e/cube_envd/conformance/README.md) — how the
  Go-baseline comparison is captured, normalised and gated; source of the
  [Declared differences](#declared-differences) table
- [docker/README.md](../docker/README.md) — base image, the two envd
  implementations and the `ENVD_BIN` rollback

## License

Apache-2.0 — see [LICENSE](../LICENSE) for details.

## Declared differences

<!-- cube-envd-declared-differences:begin -->
<!-- 由 tests/e2e/cube_envd/conformance/conformance.py render-docs 生成；请勿手改。 -->

| 面 | Go envd 0.5.13 基线 | cube-envd | 依据 |
|---|---|---|---|
| process.Process/Start：被信号终止的进程 | exitCode = -1，status = "signal: killed" | exitCode = 128 + N，status = "terminated by signal N" | Go 的 os.ProcessState.ExitCode() 在信号终止时返回 -1、String() 返回 "signal: killed"。 CubeSandbox 自带的三个 SDK 都把 "terminated by signal N" 归一为 128 + N （sdk/go/pty.go、sdk/node/src/commands.ts、sdk/python/cubesandbox/_commands.py）， 因此 cube-envd 采用 shell 约定；负值在本实现里保留给"回收失败"这一独立语义， 不会与信号语义混用。 |
| 声明范围之外的三个面 | /files/compose、/metrics、filesystem.CreateWatcher 均已实现 | 全部返回 501 + Connect unimplemented 错误体 | issue #1227 的 MVP 边界不包含这三个面；仓库内没有 /metrics 的消费者（Cubelet 只解析 `envd --version`），/files/compose 与持久 watcher 也没有 SDK 调用方。按"声明之外必须 返回稳定、协议正确的错误"的要求，它们显式返回 501，而不是让路由缺失导致的 404。 |
| 流式 Connect 帧的单帧上限 | 无显式上限（envd 未设置 WithReadMaxBytes；connect-go v1.18.1 默认不限） | 64 MiB（= CubeSandbox SDK 的 MAX_CONNECT_ENVELOPE_SIZE） | 三个自带 SDK 的接收上限都是 64 MiB；daemon 侧取同一取值，就不会出现"SDK 允许发、 daemon 拒绝收"的不对称。超限帧在读取帧头时即被拒绝（resource_exhausted），不会分配 载荷缓冲；代价是比基线更严——基线对任意大小的单帧都不设限。 |
| 一元请求体上限 | 无显式上限（同一 connect-go 默认） | 4 MiB，超过返回 429 resource_exhausted | 一元 body 同样是无界输入面：若不设限，一个畸形请求就能让 daemon 按声明长度分配内存。 4 MiB 覆盖了 /init 注入大批环境变量与全部真实 RPC 载荷，同时把最坏情况限制在常量级。 |
| 畸形 JSON 的错误文案 | "unmarshal message: unmarshal into *process.ListRequest: proto: unexpected EOF" | "invalid List request: EOF while parsing an object at line 1 column 1" | 状态码与错误码完全一致（400 / invalid_argument），只有解析器产生的文案不同：基线用 protobuf-go，cube-envd 用 serde_json。SDK 按 code 分流，不匹配 message 文本。 |
| 上传的原子替换在 WatchDir 流里多出临时文件事件 | 就地写目标文件：订阅者看到 created.txt 的 CREATE/CHMOD/WRITE | 同目录临时文件 + rename：订阅者额外看到 .cube-envd-upload-* 的 CREATE/WRITE/CHMOD，随后才是目标文件事件 | cube-envd 用"同目录临时文件 + rename"保证上传要么完整落盘、要么完全不落盘；基线的 O_TRUNC 就地写在中途失败时会留下半截文件。原子性带来的代价是目录订阅者会看到临时 文件（名字带 .cube-envd-upload- 前缀且以随机后缀结尾），配合 watch 做同步的客户端 需要按前缀过滤。两条路只能选一条：要原子性就接受这些事件，要事件逐条一致就放弃原子性。 |
| /files 下载的 Range / 条件请求 | 206 + Content-Range（Go http.ServeContent 天然支持），另有 If-Range/If-None-Match 等条件请求语义 | 忽略 Range，返回完整 200（Accept-Ranges: bytes 已按基线给出） | MVP 的 SDK 调用方只做整文件下载，因此没有实现 Range/条件请求分支。差异是**声明过**的： `Accept-Ranges: bytes` 与 `Vary: Accept-Encoding` 已与基线一致，客户端若发 Range 会拿到 完整内容而不是 206——语义上仍然正确（HTTP 允许服务器忽略 Range），但不满足断点续传/ 缓存复用。实现时的建议是先评估 `tower-http::services::ServeFile`（自带 Range 与条件请求）， 而不是照抄 2000 行手写实现。 |
| 命令行解析的严格程度 | 忽略位置参数、接受 `-isnotfc=false`、接受超出 u16 的 `-port`（只在 bind 时失败） | 上述输入一律按用法错误处理：退出码 2 + stderr 用法提示 | Go 的 flag 包在遇到第一个非 flag 参数时停止解析并静默忽略，一个写错的 ENVD_EXTRA_ARGS 可能让 daemon 带着默认值启动；cube-envd 宁可启动失败也不接受看不懂的参数。被基线真正 拒绝的输入（未定义 flag、缺值等）在这里同样被拒绝，退出码与用法提示位置一致。 |
| 每条命令的 cgroup 放置与 OOM 归因 | cgroup v2 可写时给每条命令建 leaf、按 leaf 归因 OOM，并据此填 EndEvent 的 oomKilled/killedBy | MVP 不做 per-command cgroup：不设命令级资源上限，EndEvent 不产生 OOM 归因字段 | 在发布的基础镜像里这一差异**不可观察**：guest 没有可写的 cgroup v2 树，基线自己也会打印 `falling back to no-op cgroup manager`（已用镜像里的 /usr/bin/envd-go 实测），两边跑的是 同一套 noop 策略。只有在 cgroup v2 可写的宿主上，基线的命令级隔离与 OOM 归因才会体现。 若后续实现，契约沿用基线：分配失败即拒绝 Start（resource_exhausted），绝不静默降级。 对照套件不覆盖该面（无 OOM 场景），因此本条目只用于文档表。 |

已决定与基线保持一致的行为（不是差异，但同样是被评审过的取值）：

| 面 | 基线 | cube-envd | 说明 |
|---|---|---|---|
| 流式 keepalive 默认节奏 | 90 s | 90 s（可用 Keepalive-Ping-Interval 请求头调小） | 默认值属于可观察行为，改动会平白多出一条差异；需要更短节奏的客户端本来就有请求头可用。 |
| 一元请求体上限 | 4 MiB（connect-go 默认） | 4 MiB | 与基线一致，同时覆盖 /init 注入大批环境变量的场景。 |
| 进程信号退出的可观察字段 | exitCode/status 同时出现在 EndEvent | 同样的字段集合，仅取值约定不同（见上） | 字段形状保持一致，SDK 无需分支处理缺失字段。 |

完整清单（含机器可读的允许范围）在
[`tests/e2e/cube_envd/conformance/declared_differences.toml`](../tests/e2e/cube_envd/conformance/declared_differences.toml)；
对照套件的实测结果由 `conformance.py … --results RESULTS.md` 生成到同目录的
`RESULTS.md`（**生成物，不入库**；CI 把它作为 `cube-envd-conformance-results`
artifact 上传）。
<!-- cube-envd-declared-differences:end -->
