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
| `process.Process` | `Start` (Connect JSON streaming: start/data/end events; `cwd` validated — a missing/non-directory cwd is rejected with `invalid_argument` and the working directory is entered *after* the privilege drop so the target user's permissions apply; `Connect-Timeout-Ms` kills the child's whole process group and ends with `deadline_exceeded`; client disconnect leaves the child running), `List`, `SendSignal` (whole-group signal) |
| `filesystem.Filesystem` | `Stat`, `ListDir` (BFS depth), `MakeDir` (ownership on every created component), `Move`, `Remove` (idempotent) |
| CLI | `-port`, `-isnotfc` (ignored), `-version`/`--version`, `-commit`; unknown flags are warned and skipped so `ENVD_EXTRA_ARGS` written for Go envd keep working |
| Auth | `Authorization: Basic base64("<user>:")` / `username` query, `/etc/passwd` resolution, default user `root`, privilege drop per operation, `X-Access-Token` enforced only after /init provides one |

Out of MVP scope — these return stable, protocol-correct `unimplemented`
errors (HTTP 501 on unary surfaces, EndStream error frames on streaming
surfaces), never panics or silent success:

- PTY (`Start.pty`, `Update`), interactive stdin (`SendInput`,
  `StreamInput`, `CloseStdin`, `Start.stdin=true`), `Process/Connect`
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
- **Symlink `Stat`/`ListDir` (lstat vs follow).** cube-envd reports the link
  itself (`type: FILE_TYPE_SYMLINK`, `permissions: "l…"`, `symlinkTarget` = the
  real target). Go follows the link (target's type/mode, `permissions: "L…"`,
  and for a dangling link `symlinkTarget` = the link's own path). SDKs branch
  on the RPC `code`, not link type, so this is a wire-shape difference rather
  than an SDK break; it is allowlisted and called out here.
- **Stricter-input handling is more lenient (documented).** For malformed
  unary requests Go rejects with 415/400 (missing/`text/plain` content-type,
  zero-length body, trailing bytes or multiple stream envelopes — cube-envd
  decodes the first envelope and ignores trailing bytes); cube-envd
  accepts the common shapes and executes. It never *executes a side effect* on
  a shape Go refuses — the one case that did (nested `SendSignal` selector) was
  fixed to resolve to `not_found` without signalling any process.
- **Uploads buffer in memory (bounded).** Both upload paths hold the payload
  (≤ 64 MiB) in memory before the atomic temp-file write; Go streams to disk.
  Worst case is bounded and rejected cleanly with 413 above the cap, but very
  memory-tight sandboxes doing several concurrent max-size uploads should be
  aware. Overwriting an existing file preserves its mode bits. Multipart
  parts without a filename are ignored as form fields (only the raw
  octet-stream path uses the `?path` query target).
- **Missing-binary start event reports `pid: 0`.** Nothing is spawned, so
  there is no pid; upstream reports the pid of its nice(1) wrapper, which is
  equally unusable for `Connect`/`SendSignal` after the exec failure. Event
  order, stderr suffix and exit code 127 match the baseline.
- **Cosmetic HTTP:** Go appends a trailing `\n` to REST error/JSON bodies and
  sends `Vary`/`Allow` headers cube-envd omits; `HEAD` is auto-served by axum.
  None affect the SDKs.

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
- A quiet `Start` stream emits a `keepalive` event every 30 s (like
  upstream) so proxies and LBs don't idle-close the connection while a
  long silent command runs.
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
