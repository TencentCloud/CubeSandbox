# cube-envd

CubeSandbox-maintained in-guest data-plane daemon, protocol-compatible with
the E2B envd that CubeSandbox previously consumed from `e2b-dev/infra`.

## Why

envd runs inside every sandbox and is the compatibility boundary between the
E2B SDKs and the CubeSandbox runtime. Consuming it from upstream meant the
roadmap, fix cadence and release schedule were owned elsewhere, and the
binary carried integration paths CubeSandbox never uses (Firecracker MMDS,
Hyperloop, NFS volume init). cube-envd replaces it with a small
CubeSandbox-owned Rust implementation; the upstream Go envd remains available
as an explicit rollback through the existing `ENVD_BIN` switch in
`docker/cube-entrypoint.sh`.

## Design

The tree is a contract index and a dependency graph: a path states what is
promised, an edge states who may call whom. Six layers, one allowed direction:

```
app -> {filesystem, process} -> platform -> protocol -> compat
```

```
cube-envd/                            the component directory; everything below is inside it
├── spec/                             protocol snapshots the wire types mirror
│   ├── filesystem/
│   │   └── filesystem.proto
│   ├── process/
│   │   └── process.proto
│   └── envd.yaml
├── src/
│   ├── app/                          HTTP surface and startup wiring
│   │   ├── middleware/               cors.rs, legacy.rs (X-E2B-Legacy-SDK)
│   │   ├── cli.rs                    Go-flag-compatible CLI
│   │   ├── handlers.rs               Connect + REST handler bodies
│   │   ├── lifecycle.rs              /init /health /envs, timestamps
│   │   ├── metrics.rs                /metrics
│   │   ├── mod.rs
│   │   ├── pool.rs                   blocking-thread pool
│   │   ├── routes.rs                 the whole URL surface
│   │   └── state.rs                  AppState { config, processes }
│   ├── compat/                       Go stdlib emulation, as data
│   │   ├── mod.rs
│   │   └── vocab.rs                  go1.26 errno text table
│   ├── filesystem/                   filesystem domain
│   │   ├── http/                     content_disposition, encoding, httpdate, preconditions, ranges
│   │   ├── watch/                    inotify.rs, pump.rs, tree.rs
│   │   ├── data_plane_tests.rs       data-plane tests
│   │   ├── download.rs
│   │   ├── entry.rs                  disk metadata -> EntryInfo
│   │   ├── errors.rs                 error -> gRPC status mapping
│   │   ├── mod.rs                    stat / listDir / makeDir / move / remove
│   │   ├── upload.rs
│   │   └── wire.rs                   filesystem.proto shapes (pure data)
│   ├── platform/                     host-facing services shared by both domains
│   │   ├── config.rs                 env defaults, token, /init time
│   │   ├── identity.rs               user/group lookup, path anchoring
│   │   ├── lock.rs                   poison-recovering lock helpers
│   │   └── mod.rs
│   ├── process/                      process domain
│   │   ├── cgroup/                   cgroup2.rs, noop.rs
│   │   ├── engine/                   child.rs, cleanup.rs, io.rs, pty.rs, spawn.rs
│   │   ├── command.rs                Start / Connect / List / SendInput / ...
│   │   ├── metadata.rs
│   │   ├── mod.rs
│   │   ├── pump.rs
│   │   ├── supervisor.rs             one process: spawn, signals, exit
│   │   ├── table.rs                  process table, output, cgroup leaves
│   │   └── wire.rs                   process.proto shapes (pure data)
│   ├── protocol/                     Connect wire layer, transport-independent
│   │   ├── error.rs                  error model + HTTP status mapping
│   │   ├── frames.rs                 envelope codec, unary + stream
│   │   ├── keepalive.rs
│   │   ├── mod.rs
│   │   ├── stream.rs
│   │   └── timeout.rs
│   └── main.rs                       entry: wiring, signals, exit code
├── tests/                            layer assertion (layer_rule.rs)
├── Cargo.lock                        locked dependency set
├── Cargo.toml                        crate manifest
├── Makefile                          component targets; the repo Makefile wraps them
└── rust-toolchain.toml               pinned toolchain
```

`spec/` is the protocol snapshot the `wire.rs` files mirror, and `tests/` holds
the layer assertion below. Directories come first within each level, then files,
each group alphabetically — the order an editor or GitHub renders the directory
in. Every path in the block resolves, and leaf directories with one uniform
purpose are summarised in the annotation rather than expanded. Not listed: `target/`
(cargo's build directory, ignored by `cube-envd/.gitignore`), `.gitignore`
itself, and this README.

`filesystem/` and `process/` never reference each other, and nothing below
`app/` reaches back into it. That is enforced rather than merely intended:
`tests/layer_rule.rs` reads `src/` and fails the suite on a forbidden module
path, so an inverted edge cannot land while `cargo test`, `clippy` and `fmt`
stay green.

Notable protocol details preserved from the baseline: proto3 JSON emits
camelCase and omits default values (`exitCode:0` disappears; SDKs recover it
from `status`), int64 fields serialize as strings, oneofs are flat
(`{"process":{"pid":1}}`), streaming errors always ride the EndStream frame
on HTTP 200, and a signal-killed process reports `exitCode:-1` with
`status:"signal: killed"`.

## Compatibility scope

The protocol surface was locked against a recorded behavior baseline of Go
envd 0.5.13 (`e2b-dev/infra@2026.16`, the ref the base image pins) — see
[tests/e2e/envd_conformance](../tests/e2e/envd_conformance/) for the
baseline capture and diff harness.

Implemented (behavior matched fixture-by-fixture against the baseline):

| Surface | Detail |
|---|---|
| REST | `GET /health` (204), `POST /init` (envVars merge + optional accessToken), `GET /envs`, `GET /metrics`, `GET/POST /files` (octet-stream + multipart, relative paths, ownership, error vocabulary) |
| `process.Process` | `Start` (Connect JSON streaming: start/data/end events; optional pipe stdin defaults on; `pty` allocates a real pty with merged `data.pty` output, CRLF line discipline and initial window size; `cwd` validation and privilege drop; whole-group deadline cleanup; a client disconnect leaves the child running), `Connect` (attach by pid/tag from the current output head), `List`, `SendSignal`, `SendInput`, `StreamInput`, `CloseStdin` and `Update` |
| `filesystem.Filesystem` | `Stat`, `ListDir` (depth-limited; lexical, depth-first `filepath.WalkDir` order), `MakeDir` (ownership on every created component), `Move`, `Remove` (idempotent), `WatchDir` (Connect server streaming: `start`/`keepalive`/`filesystem` events; fsnotify-faithful op mapping with the fixed expansion order; per-directory inotify watches with optional full recursion incl. synthetic creates for pre-existing subtrees and cookie-paired rename path rewrites), `CreateWatcher` / `GetWatcherEvents` / `RemoveWatcher` (pull watchers with id lifecycle) |
| CLI | Go `flag` compatible: `-port` (u16, `-port N` or `-port=N`), `-isnotfc` (accepted and ignored; `-isnotfc=false` is **rejected** — only the non-FC mode is implemented), `-version`/`--version`, `-commit`, `-h`/`-help` (usage, exit 0); `-cmd`/`-cgroup-root` are recognized but not implemented yet (warned and skipped); **any other flag or positional argument is a usage error — Go's message + usage on stderr + exit 2** |
| Auth | `Authorization: Basic base64("<user>:")` / `username` query, `/etc/passwd` resolution, default user `root`, privilege drop per operation, `X-Access-Token` enforced only after /init provides one |

Out of scope — these return stable, protocol-correct `unimplemented`
errors (HTTP 501 on unary surfaces, EndStream error frames on streaming
surfaces), never panics or silent success:

- `/files/compose`, gzip download encoding, `/files` signature verification
- Connect binary-protobuf codec — every known client (the repo Python/Node/Go
  SDKs and the official e2b Python/JS SDKs) uses the JSON codec

Known behavioral differences against Go envd 0.5.13 are enumerated with
reasons in the conformance suite allowlist
(`tests/e2e/envd_conformance/conformance.py`, `DECLARED_DIFFERENT`). The most
load-bearing ones, and why cube-envd differs:

- **Process-group cleanup on timeout/SendSignal (intentional improvement).**
  cube-envd starts each command in its own process group and signals the whole
  group, so a `Connect-Timeout-Ms` expiry or `SendSignal` reaps the shell *and*
  the descendants it forked. Go envd 0.5.13 signals only the direct child,
  leaking grandchildren (e.g. a backgrounded `sleep`) as orphans. This is a
  deliberate divergence: a sandbox data-plane should not leak processes. The
  exit codes and `deadline_exceeded` trailer are preserved; cube-envd also
  publishes a terminal EndEvent carrying the actual signal before that trailer.
- **Watch-family deviations (documented).** The pull-watch event buffer is
  capped at 10 000 events — upstream accumulates without bound
  (`watch_sync.go:107`), so a client that never polls grows the daemon's
  memory forever. At the cap cube-envd fails the watcher with an error
  surfaced through `GetWatcherEvents` (the same channel upstream uses for
  watcher errors) instead of silently dropping or silently growing. The
  keepalive cadence defaults to 30 s rather than the filesystem watch's 90 s
  (same rationale as the process stream below); the
  `Keepalive-Ping-Interval` header tunes it identically.
- **Stricter-input handling is more lenient (documented).** For malformed
  unary requests Go rejects with 415/400 (missing/`text/plain` content-type,
  zero-length body, trailing bytes or multiple stream envelopes — cube-envd
  decodes the first envelope and ignores trailing bytes); cube-envd
  accepts the common shapes and executes. It never *executes a side effect* on
  a shape Go refuses. Selector decoding matches upstream exactly: unknown
  fields are discarded (connect-go's `DiscardUnknown`), so the legacy nested
  `{"selector":{...}}` shape decodes to an empty selector that fails with the
  same `unimplemented` as the upstream service default branch — nothing is
  signalled, attached to, or otherwise side-effected (#1227).
- **Uploads stream to disk in place (matching upstream).** Both upload paths
  stream the request body straight into the target file
  (`O_WRONLY|O_CREATE|O_TRUNC`, `upload.go:68`) — the payload never sits in
  memory (a 256 MiB upload adds ~6 MiB to the daemon's RSS). As in upstream,
  the write is **not atomic**: an interrupted upload leaves partial content
  in the target, concurrent readers see it grow, and a failed upload is not
  rolled back. Above the 256 MiB cap the upload stops mid-stream with 413.
  Overwriting an existing file preserves its mode bits (`O_TRUNC` never
  touches the mode). Multipart parts without a filename are ignored as form
  fields (only the raw octet-stream path uses the `?path` query target).
- **CLI parsing is stricter than Go's `flag` (documented).** *Unlike the
  upstream Go envd, cube-envd strictly validates every command-line argument:
  an invalid flag, a positional argument or a malformed value terminates
  startup immediately with exit code 2 instead of being silently ignored.*
  Go stops parsing at the first non-flag token and silently ignores it, so a
  typo in
  `ENVD_EXTRA_ARGS` could leave envd running on defaults; cube-envd rejects
  positional arguments, including anything trailing a bare `--`. It also
  validates `-port` as `u16` at parse time,
  where Go's `int64` accepts `99999` and only fails when binding, and it
  rejects a value attached to an output flag (`-version=false`) instead of
  parsing it as a boolean. Everything Go *does* reject — undefined flags,
  `bad flag syntax`, missing or invalid values — is rejected here too, with
  Go's message followed by the usage block on stderr and exit code 2.
- **Cosmetic HTTP:** Go appends a trailing `\n` to REST error/JSON bodies and
  sends `Vary`/`Allow` headers cube-envd omits; `HEAD` is auto-served by axum.
  None affect the SDKs.
- **Slow output consumers are cut off, not back-pressured.** The process
  output bus is a bounded broadcast (capacity 64); a connection that falls
  behind the ring gets its own `Lagged` error, which cube-envd frames as a
  terminal `resource_exhausted` EndStream error and closes only that stream.
  The child and every other subscriber keep running untouched. This is the
  cancel-on-overflow shape upstream #3292 recommends; upstream Go envd's
  lock-step fan-out has no equivalent error code, so a `resource_exhausted`
  stream is a cube-envd-only signal a client never sees from Go.
- **`Keepalive-Ping-Interval` outside the sane range degrades instead of
  crashing.** Upstream parses the header straight into `time.NewTicker`, so
  `0`, negative, or int64-duration-overflowing values panic the daemon;
  cube-envd parses as `u32` and falls back to the 30 s default for any
  absent, non-numeric, non-positive, or oversized value.

Behaviors that look like divergences but are deliberately aligned with the
baseline (asserted by the conformance fixtures, not allowlisted):

- **Symlink `Stat`/`ListDir` follows upstream `GetEntryInfo`.** A link's `type`
  and `mode` describe its followed target (`permissions` still describe the link
  itself, rendered `L…` like Go's `os.FileMode.String()`), and a dangling link
  reports the proto3-zero `FILE_TYPE_UNSPECIFIED` with no `type`/`mode` keys.
  `ListDir` resolves its root with a following stat but never descends into a
  symlinked child — matching upstream's `followSymlink` + `filepath.WalkDir`.

## Build & test

Everything runs inside the repo builder container:

```bash
make cube-envd        # → _output/bin/cube-envd (static musl, ~3.2 MiB)
make cube-envd-test   # cargo test + clippy -D warnings
```

Conformance against the Go baseline and the performance comparison are
documented in [tests/e2e/envd_conformance](../tests/e2e/envd_conformance/).

## Integration notes

- The guest contract is unchanged: `cube-entrypoint.sh` starts
  `${ENVD_BIN:-/usr/bin/envd} -port 49983 -isnotfc`.
- `docker/Dockerfile.cube-base` installs cube-envd **as** `/usr/bin/envd`
  (the default; `--build-arg ENVD_IMPL=go` flips it) and ships the upstream
  Go envd as `/usr/bin/envd-go`, so `ENVD_BIN=/usr/bin/envd-go` is a
  runtime rollback that needs no rebuild. Installing as the literal
  `/usr/bin/envd` matters: Cubelet's version collection execs `envd
  --version`, so an `ENVD_BIN` override alone would leave the template
  annotated with the other implementation's version.
- A quiet `Start` stream emits a `keepalive` event so proxies and LBs don't
  idle-close the connection while a long silent command runs. The default
  cadence is 30 s — upstream uses 90 s, but 30 s stays safely under the
  unknown idle timeout (typically 60 s) of the LB in front of CubeProxy. A
  client can tune the cadence with the `Keepalive-Ping-Interval` request
  header (integer seconds); an absent, non-numeric, non-positive, or
  oversized value falls back to 30 s (see "Known behavioral differences").
- Reported version: `0.1.0`. The control plane has no minimum-version
  rejection (verified across CubeAPI and the SDKs — only feature gates). The
  daemon surface now implements the watch family; 0.1.0 keeps the e2b SDK
  watch-related feature gates safely disabled — enabling them is an SDK-side
  change outside the daemon's scope.

## Process termination metadata and cgroup policy

EndEvent optionally includes `signal` (numeric terminating signal),
`oomKilled`, and `killedBy` (`user`, `timeout`, or `oom`). Normal exits omit
these fields unless an envd-side cause was explicitly recorded. SDK result
objects default absent OOM metadata to false.

OOM attribution requires all of: main-process SIGKILL, an increased
per-command `memory.events.oom_kill` counter, and no recorded user/timeout
cause. A descendant-only OOM followed by a successful parent exit is not
reported as a main-process OOM. Explicit user/timeout causes take precedence.
The counter is command-leaf scoped, not PID scoped: an unrelated external
SIGKILL concurrent with a descendant OOM remains inherently ambiguous.

Each command gets a cgroup v2 leaf under `user` or `ptys`. With an active
manager, leaf-allocation errors reject Start with `resource_exhausted`;
pre-exec placement errors reject Start without running user code. Neither
path retries without confinement or permanently disables the manager.
Subsequent requests can recover once the underlying resource problem clears.
Startup inability to initialize cgroups still selects the existing logged
Noop fallback. Commands in that mode do not claim per-command OOM evidence
or escape-proof descendant cleanup.

There is no direct fallback into the type parent: these parents distribute
memory/cpu to child leaves and cannot also accept internal processes under
cgroup v2's no-internal-process constraint. There is no PID-0 migration probe.

Configuration:

- `CUBE_ENVD_CGROUP_ROOT`: cgroup v2 root, default `/sys/fs/cgroup`; nested
  daemon membership is resolved when visible below that root.
- `CUBE_ENVD_CGROUP_MEMORY_MAX_BYTES`: positive requested memory cap, clamped
  by the safe guest/enclosing-parent limit. Without it, the budget reserves
  `min(total/8, 128 MiB)` from the effective guest/parent memory ceiling.

A Start timeout kills the command, publishes the real EndEvent, then emits a
`deadline_exceeded` trailer. `Connect-Timeout-Ms` bounds the attachment, not
the process: on expiry the stream ends with a `deadline_exceeded` trailer while
the command keeps running, and without the header the attachment remains until
the process ends or the client disconnects. The client SDKs may still apply
their own idle/request timeout. Python Commands always uses the in-house
Connect-JSON decoder, regardless of whether the E2B package is installed;
Go Commands copies termination fields into the public CommandResult.
