# envd 一致性套件 / envd Conformance Suite

Black-box conformance harness comparing an envd implementation against the
upstream Go envd baseline, fixture by fixture. Built for
[cube-envd](../../../cube-envd/) (issue
[#1227](https://github.com/TencentCloud/CubeSandbox/issues/1227)); the same
suite gates cube-envd changes, `ENVD_REF` bumps, and SDK-matrix updates.

## Layout

| File | Purpose |
|---|---|
| `capture.py` | Runs REST, filesystem RPC, and process streaming scenarios against a live envd, including pipe/PTY input, fragmented StreamInput, CloseStdin, stdin-default, timeout, and disconnect paths; records raw wire fixtures |
| `conformance.py` | Normalizes two fixture directories (volatile values, header case, chunking) and diffs them; declared MVP differences are allowlisted with reasons |
| `lifecycle_smoke.go` | Assertion-based black-box regression for interactive input and slow-client process cleanup against one live envd; these checks complement, but do not replace, Go-vs-Rust fixture capture |
| `perf.py` | Startup-to-/health latency, RSS, and command round-trip comparison |
| `termination_e2e.py` | Real cgroupfs OOM, fail-closed allocation/recovery, escaped descendants, wire metadata and Python SDK checks in an isolated Linux/QEMU guest |
| `test_conformance.py` | Unit coverage ensuring extension normalization does not hide unrelated wire differences |

## Running

Requires Docker, Python 3.10+ (stdlib only), and the cube-envd musl binary
(`make cube-envd` → `_output/bin/cube-envd`).

```bash
BASE_IMAGE=ghcr.io/tencentcloud/cubesandbox-base:2026.16

# 1. Go baseline container (:49985) and cube-envd container (:49984).
#    cube-envd is injected through the stock entrypoint's ENVD_BIN switch,
#    which doubles as the rollback-path verification.
docker run -d --name envd-go2  -p 127.0.0.1:49985:49983 $BASE_IMAGE
docker run -d --name envd-rust -p 127.0.0.1:49984:49983 \
  -v $PWD/../../../_output/bin/cube-envd:/usr/bin/cube-envd:ro \
  -e ENVD_BIN=/usr/bin/cube-envd $BASE_IMAGE

# 2. Capture fixtures from both (fresh containers matter: scenarios mutate
#    the filesystem, and both sides must see identical starting state).
ENVD_BASE=http://127.0.0.1:49985 OUTDIR=fixtures-go   python3 capture.py all
ENVD_BASE=http://127.0.0.1:49984 OUTDIR=fixtures-rust python3 capture.py all

# 3. Diff. Exit code 0 = conformant (declared differences excluded).
python3 conformance.py fixtures-go fixtures-rust

# 4. Lifecycle regression against cube-envd. The default is :49984; ENVD_BASE
#    can point at another live instance.
ENVD_BASE=http://127.0.0.1:49984 go run lifecycle_smoke.go

# 5. Optional: performance comparison (writes perf-results.json).
python3 perf.py
```

## Reading results

- `PASS` — normalized fixtures identical.
- `DECLARED-DIFF` — allowlisted in `conformance.py` `DECLARED_DIFFERENT`
  with a reason; every entry maps to the "known differences" table in the
  cube-envd design doc / PR description (watch, `/files/compose`,
  gzip, nested-selector error differences, parser-specific error wording).
- `FAIL` — a real behavioral divergence; fix cube-envd or, if the change
  is intentional, move it to the allowlist **with a reason** in the same PR.

## Scenario coverage (issue #1227 test requirements)

| Path class | Scenarios |
|---|---|
| success | health, init→envs, metrics, upload/download (octet+multipart, absolute+relative), Stat/ListDir/MakeDir/Move/Remove, echo/stderr/env-merge/cwd/user switching, large output (2 MiB byte-exact), signal kill, pty, pipe/PTY input, fragmented StreamInput, CloseStdin EOF |
| error | bad user (REST 401 / RPC unauthenticated), missing paths, directory download, missing binary (127), malformed JSON |
| timeout | `Connect-Timeout-Ms` expiry → `deadline_exceeded` + process killed, including an unread response whose output queue is full |
| cancellation | client disconnect mid-stream → process keeps running (List + side-effect check) |
| unimplemented | watch family / compose answer with stable protocol-correct errors |

## Termination metadata extension

`signal`, `oomKilled` and `killedBy` are CubeSandbox extensions to upstream's
EndEvent. The comparison removes only these fields from EndEvent objects,
including nested captured streams, while still comparing exit status/error.
On timeout, cube-envd additionally emits the exact SIGKILL/timeout EndEvent
before `deadline_exceeded`; the comparator recognizes only that specific
event/trailer pair. Unexpected causes, exit codes and trailers remain failures.
`termination_e2e.py` independently asserts the extension values against real
process deaths; this is not a blanket allowlist for signal/timeout scenarios.

```bash
python3 -m unittest discover -s tests/e2e/envd_conformance -p test_conformance.py -v
```

## Real QEMU/cgroupfs tests

Copy the newly built musl binary, `termination_e2e.py`, and `sdk/python` into
the existing test guest. Run as root with Python SDK dependencies installed:

```bash
sudo env PYTHONPATH=/path/to/sdk/python python3 -u \
  termination_e2e.py /path/to/cube-envd
```

The test creates a unique cgroup subtree with a 256 MiB parent cap, no swap,
64 MiB command limits, and a separate daemon on a dynamically chosen port.
It checks real main-process OOM, descendant-only OOM, timeout/user attribution,
pipe/PTY placement, escaped-descendant cleanup and the Python command/PTY SDKs.
Setting `user/cgroup.max.descendants=0` forces real allocation failures; two
requests must fail without executing user code. Restoring `max` must allow
confined execution without restarting the daemon. All test leaves and the
test daemon are removed afterwards. Existing services are not restarted.

For the full CubeAPI → CubeProxy → daemon path, `e2e_sdk.py` can inject a
candidate binary into a **new disposable sandbox** without rebuilding or
modifying an existing template:

```bash
CUBE_API_URL=... CUBE_PROXY_NODE_IP=... CUBE_PROXY_PORT_HTTP=... \
  TEMPLATE_CUBE=<existing-ready-template> \
  ENVD_TEST_BINARY=/path/to/new/cube-envd python3 -u e2e_sdk.py
```

The upload is SHA-256 checked. Only that test sandbox routes its SDK process
and filesystem calls to the new daemon on port 49984. Template boot/readiness
still uses the original daemon on 49983: this validates the new binary's live
data plane, not a rebuilt image's boot path. The sandbox is deleted at exit.
