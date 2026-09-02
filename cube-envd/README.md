# cube-envd

CubeSandbox-maintained in-guest data-plane daemon, protocol-compatible with
the E2B envd that CubeSandbox previously consumed from `e2b-dev/infra`.
Implements the MVP scope agreed in
[issue #1227](https://github.com/TencentCloud/CubeSandbox/issues/1227).

## Why

envd runs inside every sandbox and is the compatibility boundary between the
E2B SDKs and the CubeSandbox runtime. Consuming it from upstream meant the
roadmap, fix cadence and release schedule were owned elsewhere, and the
binary carried integration paths CubeSandbox never uses (Firecracker MMDS,
Hyperloop, NFS volume init). cube-envd replaces it with a small
CubeSandbox-owned Rust implementation; the upstream Go envd remains available
as an explicit rollback through the existing `ENVD_BIN` switch in
`docker/cube-entrypoint.sh`.

## Compatibility scope

The protocol surface was locked against a recorded behavior baseline of Go
envd 0.5.13 (`e2b-dev/infra@2026.16`, the ref the base image pins) — see
[tests/e2e/envd_conformance](../tests/e2e/envd_conformance/) for the
baseline capture and diff harness.

Implemented (behavior matched fixture-by-fixture against the baseline):

| Surface | Detail |
|---|---|
| REST | `GET /health` (204), `POST /init` (envVars merge + optional accessToken), `GET /envs`, `GET /metrics`, `GET/POST /files` (octet-stream + multipart, relative paths, ownership, error vocabulary) |
| `process.Process` | `Start` (Connect JSON streaming: start/data/end events; `cwd` validated — a missing/non-directory cwd is rejected with `invalid_argument` and the working directory is entered *after* the privilege drop so the target user's permissions apply; `Connect-Timeout-Ms` kills the child's whole process group and ends with `deadline_exceeded`; client disconnect leaves the child running), `Connect` (attach to a running process by pid/tag; emits start/data/end from the attach point onward — no history replay; never kills or reaps), `List`, `SendSignal` (whole-group signal) |
| `filesystem.Filesystem` | `Stat`, `ListDir` (BFS depth), `MakeDir` (ownership on every created component), `Move`, `Remove` (idempotent) |
| CLI | Go `flag` compatible: `-port` (u16, `-port N` or `-port=N`), `-isnotfc` (accepted and ignored; `-isnotfc=false` is **rejected** — only the non-FC mode is implemented), `-version`/`--version`, `-commit`, `-h`/`-help` (usage, exit 0); `-cmd`/`-cgroup-root` are recognized but not implemented yet (warned and skipped); **any other flag or positional argument is a usage error — Go's message + usage on stderr + exit 2** |
| Auth | `Authorization: Basic base64("<user>:")` / `username` query, `/etc/passwd` resolution, default user `root`, privilege drop per operation, `X-Access-Token` enforced only after /init provides one |

Out of MVP scope — these return stable, protocol-correct `unimplemented`
errors (HTTP 501 on unary surfaces, EndStream error frames on streaming
surfaces), never panics or silent success:

- PTY (`Start.pty`, `Update`), interactive stdin (`SendInput`,
  `StreamInput`, `CloseStdin`, `Start.stdin=true`)
- watch family (`WatchDir`, `CreateWatcher`, `GetWatcherEvents`,
  `RemoveWatcher`)
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
  event stream, exit codes and `deadline_exceeded` framing are unchanged.
- **Stricter-input handling is more lenient (documented).** For malformed
  unary requests Go rejects with 415/400 (missing/`text/plain` content-type,
  zero-length body, trailing bytes or multiple stream envelopes — cube-envd
  decodes the first envelope and ignores trailing bytes); cube-envd
  accepts the common shapes and executes. It never *executes a side effect* on
  a shape Go refuses — the cases that did (nested `SendSignal`/`Connect`
  selectors) resolve to `not_found` without signalling or attaching to any
  process.
- **Uploads buffer in memory (bounded).** Both upload paths hold the payload
  (≤ 64 MiB) in memory before the atomic temp-file write; Go streams to disk.
  Worst case is bounded and rejected cleanly with 413 above the cap, but very
  memory-tight sandboxes doing several concurrent max-size uploads should be
  aware. Overwriting an existing file preserves its mode bits. Multipart
  parts without a filename are ignored as form fields (only the raw
  octet-stream path uses the `?path` query target).
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
make cube-envd        # → _output/bin/cube-envd (static musl, ~2.6 MB)
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
  rejection (verified across CubeAPI and the SDKs — only feature gates), and
  0.1.0 keeps e2b SDK watch-related feature gates safely disabled, matching
  the MVP surface.

## Design

Module layout mirrors the protocol split:

```
src/
├── main.rs        CLI + runtime bootstrap
├── server.rs      single-port router (REST + two Connect services)
├── connect.rs     Connect JSON codec: unary bodies + 5-byte stream envelopes
├── auth.rs        Basic auth / username query → /etc/passwd, path anchoring
├── state.rs       /init env store, access token, process table
├── exec.rs        spawn + privilege drop + stdout/stderr pump
├── rest/          /health /init /envs /metrics /files
├── services/      process.Process, filesystem.Filesystem
└── msg/           hand-written proto3-JSON serde types (spec/ snapshots)
```

Notable protocol details preserved from the baseline: proto3 JSON emits
camelCase and omits default values (`exitCode:0` disappears; SDKs recover it
from `status`), int64 fields serialize as strings, oneofs are flat
(`{"process":{"pid":1}}`), streaming errors always ride the EndStream frame
on HTTP 200, and a signal-killed process reports `exitCode:-1` with
`status:"signal: killed"`.
