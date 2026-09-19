# cube-envd: CubeSandbox-maintained in-guest data-plane daemon (Rust)

## Summary

This change adds `cube-envd`, a Rust implementation of the in-guest `envd`
data plane, and switches the default CubeSandbox base image to it. The upstream
Go envd remains available through the `ENVD_IMPL=upstream-e2b` rollback path.

It implements the E2B data-plane protocol so the existing Python/Go/Node SDKs
and the template flow keep working unchanged, and it removes the hard dependency
on compiling envd from `e2b-dev/infra`.

## Design

- Rust + axum serves HTTP and the Connect JSON streaming RPCs with low
  per-request overhead (no interpreter, static binary).
- `connect.rs` encodes/decodes the 5-byte Connect envelope and end-stream
  frames; process/PTY/filesystem use it.
- Modules: `main` (router/CLI), `process`, `pty`, `filesystem`, `files`,
  `watch`, `metrics`, `init`, `defaults`, `auth`, `processes`, `termination`,
  `cors`, `connect`.
- `POST /init` stores process-wide defaults (`envVars`, `defaultUser`,
  `defaultWorkdir`, `accessToken`) applied to later process/PTY spawns.
- `auth.rs` enforces `X-Access-Token` once `/init` provides one.
- Commands run in their own process group; timeout/`SendSignal` kill the whole
  tree so grandchildren are reaped.
- PTY sessions are a PID-indexed map supporting Start/Connect/SendInput/Update/
  SendSignal; `process.Process/List` reports running processes.
- WatchDir streams inotify events; `CreateWatcher`/`GetWatcherEvents`/
  `RemoveWatcher` provide the non-streaming family.
- `/files` includes CORS, single-range `206`, unsatisfiable `416`, conditional
  `304`, mode-preserving overwrite, and a 64 MiB body cap.

## Topology

`cube-envd` runs inside the workload/template container (installed as
`/usr/bin/envd`). CubeMaster probes the workload's `49983/health` endpoint and
data-plane requests route to `49983-<sandboxID>.<domain>`.

## Compatibility

Implemented: `GET /health` (204), `GET /status`, `POST /init`, `GET /envs`,
`GET /metrics`, `GET/POST /files`, `process.Process/{Start,Connect,SendSignal,
SendInput,StreamInput,CloseStdin,Update,List}`, and
`filesystem.Filesystem/{Stat,ListDir,MakeDir,Move,Remove,WatchDir,CreateWatcher,
GetWatcherEvents,RemoveWatcher}`.

`EntryInfo` uses upstream field names and `FILE_TYPE_{FILE,DIRECTORY,SYMLINK}`;
symlinks are reported with `lstat` semantics plus `symlinkTarget`.

REST `/files`: raw and multipart writes, CORS preflight, Range, `Last-Modified`,
`If-Modified-Since`, `304`, `416`, 64 MiB upload cap. Multi-range requests serve
the whole file with `200`; only identity encoding is offered, and an
`Accept-Encoding` that refuses identity gets `406`. `/files/compose` stays in the
route table and answers `501 unimplemented` rather than a misleading `404`.

## Known differences

- `Process.Start` allocates a piped stdin by default (`stdin:false` opts out);
  `StreamInput` and `CloseStdin` (and the `stdin` arm of `SendInput`) drive it.
- PTY signal exits report `exitCode = -1` with `status`/`error` of
  `signal: <name>` and `exited=false`, matching upstream; a signal-killed
  process never reports `128 + signal`.
- End events additionally carry `termination{reason,signal,signalName,
  coreDumped}`; cgroup `memory.events` / `memory.oom_control` deltas distinguish
  OOM kills from ordinary SIGKILLs (`memory.failcnt` fallback).
- The Connect binary-protobuf codec is not implemented: requests that ask for it
  (`application/proto`, `application/connect+proto`, `application/grpc*`) are
  rejected up front with `501 unimplemented` naming the JSON codec. The SDKs in
  this repository send `application/connect+json`; the official E2B clients are
  not covered by that statement.
- gzip download encoding and `/files` signature verification are not
  implemented.
- WatchDir covers inotify create/remove/write/rename/chmod mappings;
  `CreateWatcher` performs an initial recursive scan but does not auto-watch
  directories created later.
- PTY/inotify need a Linux host with `/dev/pts` and inotify support.

## Validation

- `cargo test --release`: **107 passed**.
- `cargo clippy --release --all-targets -- -D warnings`: clean.
- `cargo fmt --check`: clean.
- `scripts/e2e_smoke.py`: all checks green (health/status, `/init`/`/envs`,
  `/metrics`, process exit/signal/timeout/env/cwd, `/files` raw+multipart+Range
  `206`/`416`/`304`, filesystem lifecycle, WatchDir, watch family, PTY+List,
  24-way concurrency, token auth, CLI flag tolerance).
- `scripts/conformance.py`: 18 normalized cases match the checked-in baseline
  (`scripts/conformance.baseline.json`). This is a **regression snapshot**, not a
  diff against real upstream envd output, so it shows "no regression against the
  recorded baseline" rather than upstream byte-parity.
- `scripts/scenarios.py`: the three required scenarios pass, with results in
  [`docs/SCENARIOS.md`](SCENARIOS.md).
- Base image: `docker/Dockerfile.cube-base` builds and `/health` returns `204`
  on `linux/amd64`; the `linux/arm64` build was exercised under QEMU emulation
  (`arch=aarch64`, `/health=204`).
- Real SDK against a local daemon: `sdk/go/envd_local_test.go` runs the actual
  Go SDK data plane against a cube-envd container and passes (commands with exit
  code/stderr, file write/read/stat). Run with
  `CUBE_ENVD_LOCAL_ADDR=127.0.0.1:49983 go test -run TestDataPlaneAgainstLocalEnvd`.
- Python SDK hermetic suite: 261 passed, 4 skipped (`sdk/python`, run in CI).

## Acceptance criteria (实战任务二)

| # | Criterion | Status |
|---|---|---|
| 1 | Starts in the sandbox, health check passes, behavior close to upstream envd | ✅ Local (107 tests, e2e, regression snapshot, base-image smoke) |
| 2 | Commands and file read/write via the CubeSandbox SDK | ✅ Local (`sdk/go/envd_local_test.go` drives the real SDK against a local cube-envd container; live-cluster run also covered by the SDK contract tests) |
| 3 | A template built from cube-envd creates and becomes usable | ✅ Local single-node CubeSandbox (WSL2): build → READY → sandbox → commands/files via the Go SDK (`docs/TEMPLATE_VALIDATION.md`) |
| 4 | At least 3 scenarios with results + logs | ✅ `scripts/scenarios.py` → `docs/SCENARIOS.md` |
| 5 | Build path defaults to cube-envd with a clear switch/rollback | ✅ `Makefile` + Dockerfiles + `ENVD_IMPL` |
| 6 | PR describing design, compatibility scope, known differences | ✅ this document |

## Build and rollback

```bash
make build-cube-base-image ENVD_IMPL=cube-envd      # default: Rust cube-envd
make build-cube-base-image ENVD_IMPL=upstream-e2b   # rollback: upstream Go envd
```

## ARM64 follow-up

The implementation is Linux-oriented; validate on a native ARM64 runner before
publishing a multi-arch base image. The QEMU-emulated `linux/arm64` build here
is evidence only for compilation/startup, not for PTY/inotify timing.

## Attribution

Assisted-by: opencode:deepseek-v4.1-flash
