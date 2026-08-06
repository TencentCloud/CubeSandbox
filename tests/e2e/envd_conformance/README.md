# envd 一致性套件 / envd Conformance Suite

Black-box conformance harness comparing an envd implementation against the
upstream Go envd baseline, fixture by fixture. Built for
[cube-envd](../../../cube-envd/) (issue
[#1227](https://github.com/TencentCloud/CubeSandbox/issues/1227)); the same
suite gates cube-envd changes, `ENVD_REF` bumps, and SDK-matrix updates.

## Layout

| File | Purpose |
|---|---|
| `capture.py` | Runs ~45 scenarios (REST / filesystem RPC / process streaming, covering success, error, timeout, disconnect paths) against a live envd and records raw wire fixtures |
| `conformance.py` | Normalizes two fixture directories (volatile values, header case, chunking) and diffs them; declared MVP differences are allowlisted with reasons |
| `perf.py` | Startup-to-/health latency, RSS, and command round-trip comparison |

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

# 4. Optional: performance comparison (writes perf-results.json).
python3 perf.py
```

## Reading results

- `PASS` — normalized fixtures identical.
- `DECLARED-DIFF` — allowlisted in `conformance.py` `DECLARED_DIFFERENT`
  with a reason; every entry maps to the "known differences" table in the
  cube-envd design doc / PR description (PTY, watch, `/files/compose`,
  gzip, nested-selector leniency, parser-specific error wording).
- `FAIL` — a real behavioral divergence; fix cube-envd or, if the change
  is intentional, move it to the allowlist **with a reason** in the same PR.

## Scenario coverage (issue #1227 test requirements)

| Path class | Scenarios |
|---|---|
| success | health, init→envs, metrics, upload/download (octet+multipart, absolute+relative), Stat/ListDir/MakeDir/Move/Remove, echo/stderr/env-merge/cwd/user switching, large output (2 MiB byte-exact), signal kill |
| error | bad user (REST 401 / RPC unauthenticated), missing paths, directory download, missing binary (127), malformed JSON |
| timeout | `Connect-Timeout-Ms` expiry → `deadline_exceeded` + process killed |
| cancellation | client disconnect mid-stream → process keeps running (List + side-effect check) |
| unimplemented | PTY / watch family / compose answer with stable protocol-correct errors |
