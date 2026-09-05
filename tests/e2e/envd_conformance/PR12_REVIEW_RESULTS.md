# PR #12 review-fix validation — 2026-09-05

## Provenance and scope

Executed on September 5, 2026 (Asia/Shanghai), with final live runs around
19:02–19:04. These are new results, not the historical results in RESULTS.md.
The tested source is the local review-fix worktree based on `ab7fa3b3`;
these fixes have not yet been committed or pushed. Do not attribute these
results to the unchanged remote PR head. The same final release binary was
used for QEMU, full CubeSandbox SDK E2E, and Docker protocol/lifecycle tests.

- QEMU guest kernel: `6.6.119-49.21.oc9.x86_64`.
- Build target: `x86_64-unknown-linux-musl`, Rust 1.89.
- Binary SHA-256: `cacfb639d97a6ae5179bca051ace62db93fd3c72cffa47e7354f62b2454815ef`.
- Protocol baseline image: `ghcr.io/tencentcloud/cubesandbox-base:2026.16`.
- Full SDK test template: `tpl-b695a55c588840078cdf5de5`.

## Unit/build checks

| Command | Actual result |
| --- | --- |
| `cargo test --locked --offline` | 159 passed, 0 failed, 1 ignored |
| `cargo clippy --all-targets --locked --offline -- -D warnings` | exit 0 |
| `cargo fmt --check` | exit 0 |
| `cargo build --release --locked --offline --target x86_64-unknown-linux-musl` | exit 0 |
| `go test ./...` in sdk/go | `ok github.com/tencentcloud/CubeSandbox/sdk/go 0.310s` |
| `npm test` in sdk/node | 188 passed, 1 skipped; 15 test files passed, 1 skipped |
| `python -m pytest sdk/python/tests -q` | 227 passed |
| `python3 -m unittest discover -s tests/e2e/envd_conformance -p test_conformance.py -v` | 3 passed |
| `git diff --check` | exit 0 |

Rust's ignored test requires root and writable cgroupfs. The separate QEMU
suite below exercises real cgroupfs rather than counting that ignored test as
passed. Python command-module Ruff F checks pass; test_sandbox.py still has
two pre-existing unused exception imports, also reproduced on the base commit.
No claim is made that the whole Python repository passes lint.

## Real QEMU termination/cgroup E2E: raw output

The suite runs a separate daemon and bounded, private test cgroup subtree.
OOM tests allocate inside 64 MiB command leaves, under a 256 MiB parent with
swap disabled. Allocation failures are induced through real cgroupfs
`cgroup.max.descendants=0`, not mocked files. The two consecutive failures
must not execute user code, and restoring capacity must recover placement
without restarting the daemon.

```text
kernel=6.6.119-49.21.oc9.x86_64 binary_sha256=cacfb639d97a6ae5179bca051ace62db93fd3c72cffa47e7354f62b2454815ef
PASS cgroup memory limit active
PASS normal exit omits termination metadata
PASS self SIGTERM metadata
PASS pipe placed in dedicated leaf
PASS pipe user SIGKILL metadata
PASS PTY placed in dedicated leaf
PASS PTY user SIGKILL metadata
PASS timeout EndEvent precedes deadline trailer
PASS real main-process OOM
PASS descendant OOM does not mislabel successful parent
PASS timeout wins over descendant OOM
PASS user kill wins over descendant OOM
PASS setsid descendant reaped from leaf
PASS leaf allocation failure rejects request 1
PASS leaf allocation failure rejects request 2
PASS allocation recovers without restart or bypass
PASS empty selector matches upstream unimplemented
PASS Python SDK command roundtrip
PASS Python SDK reads real signal
PASS Python SDK reads real OOM fields
PASS Python PTY SDK reads real user-kill fields
PASS all command leaves cleaned up
RESULT 22 passed, 0 failed
```

## Full CubeSandbox → CubeProxy → candidate daemon: raw output excerpt

A disposable sandbox boots from an existing template, then receives the
SHA-256-verified candidate daemon at port 49984. SDK process/filesystem calls
are routed through CubeProxy to that daemon. This covers command output,
exit codes, env/user/cwd, timeout, file operations, PTY input and resize.
Template boot/readiness still uses its original port-49983 daemon: this is
not a claim that a new template image was built or its boot path validated.
The disposable sandbox was deleted after the test.

```text
sandbox: d55790ec9e4144bd8bbcd07cbb1166df
test binary sha256=cacfb639d97a6ae5179bca051ace62db93fd3c72cffa47e7354f62b2454815ef
  [PASS] new daemon reachable through CubeProxy 
  [PASS] new daemon termination metadata through CubeProxy 
sandbox killed
===== E2E RESULT: 21 passed, 0 failed =====
```

## Fresh-container protocol comparison: raw output

Both sides captured 98 fixtures. The comparator projects only the documented
termination extension fields and the exact timeout EndEvent/trailer pair;
exit/error values and unrelated metadata still compare normally. Its tests
also cover nested streams and PID-dependent encoded frame lengths.

```text
============================================================
PASS 90  FAIL 0  DECLARED-DIFF 8  SKIP 0  MISSING 0
```

The eight declared differences are the existing watch/gzip/compose,
nested-selector error, parser wording, and timestamp semantics; they are not
new blanket exemptions for termination tests.

## Live lifecycle regression: raw output

```text
PASS pipe input/EOF
PASS PTY input
PASS fragmented StreamInput
PASS slow-client deadline cleanup
```

## Reproduction and retained logs

See README.md for the QEMU runner and the `ENVD_TEST_BINARY` full-SDK mode.
Logs from this session are retained locally as:

- `/tmp/pr12-rust-validation.log`
- `/tmp/pr12-go-and-lifecycle.log`
- `/tmp/pr12-node-validation.log`
- `/tmp/pr12-python-validation.log`
- `/tmp/pr12-conformance-unit.log`
- `/tmp/pr12-qemu-e2e.log`
- `/tmp/pr12-cubesandbox-e2e.log`
- `/tmp/pr12-conformance.log`
- `/tmp/pr12-lifecycle.log`
- `/tmp/pr12-final-fixtures-go/` and `/tmp/pr12-final-fixtures-rust/`

Existing QEMU/CubeSandbox services and template images were not replaced.
No live Go-envd rollback-template run is claimed for this session.
