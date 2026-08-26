#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Offline tests for the rpc.py 3.8 launcher. Does not need SPDK or a target:
# BooleanOptionalAction is stripped from argparse in-process so a 3.9 builder
# still exercises the backfill that Ubuntu 20.04 needs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LAUNCHER="${ROOT}/scripts/rpc.py"
COMPAT="${ROOT}/scripts/rpc_compat.py"

fail() {
	echo "FAIL: $*" >&2
	echo "result: 0 passed, 1 failed"
	exit 1
}

[[ -f "${LAUNCHER}" ]] || fail "missing ${LAUNCHER}"
[[ -f "${COMPAT}" ]] || fail "missing ${COMPAT}"

python3 - "${ROOT}" <<'PY' || fail "rpc_compat backfill"
import argparse
import importlib
import sys

root = sys.argv[1]
sys.path.insert(0, root + "/scripts")

if hasattr(argparse, "BooleanOptionalAction"):
    delattr(argparse, "BooleanOptionalAction")

import rpc_compat
importlib.reload(rpc_compat)

if not hasattr(argparse, "BooleanOptionalAction"):
    raise SystemExit("rpc_compat did not install BooleanOptionalAction")

parser = argparse.ArgumentParser()
parser.add_argument("--batch-mode", action=argparse.BooleanOptionalAction)
ns = parser.parse_args(["--batch-mode"])
if ns.batch_mode is not True:
    raise SystemExit("--batch-mode must set True")
ns = parser.parse_args(["--no-batch-mode"])
if ns.batch_mode is not False:
    raise SystemExit("--no-batch-mode must set False")
print("rpc_compat backfill OK")
PY

tmp="$(mktemp -d)"
cleanup() { rm -rf "${tmp}"; }
trap cleanup EXIT

cp "${LAUNCHER}" "${tmp}/rpc.py"
cp "${COMPAT}" "${tmp}/rpc_compat.py"
cat >"${tmp}/spdk_rpc.py" <<'PY'
#!/usr/bin/env python3
import argparse
import sys

parser = argparse.ArgumentParser(prog="spdk_rpc_fake")
parser.add_argument("-b", "--batch-mode", action=argparse.BooleanOptionalAction,
                    help="batch")
args = parser.parse_args()
if "--help" in sys.argv:
    parser.print_help()
    sys.exit(0)
print("fake-upstream-ok batch_mode=%s" % args.batch_mode)
PY

cat >"${tmp}/run_as_py38.py" <<'PY'
"""Start rpc.py after removing BooleanOptionalAction, like CPython 3.8."""
import argparse
import runpy
import sys

if hasattr(argparse, "BooleanOptionalAction"):
    delattr(argparse, "BooleanOptionalAction")
sys.argv = sys.argv[1:]
runpy.run_path(sys.argv[0], run_name="__main__")
PY

python3 - "${tmp}" <<'PY' || fail "launcher under stripped BooleanOptionalAction"
import os
import subprocess
import sys

tmp = sys.argv[1]
env = os.environ.copy()
env.pop("SPDK_ROOT", None)
env.pop("SPDK_RPC_PY", None)
runner = [sys.executable, os.path.join(tmp, "run_as_py38.py"),
          os.path.join(tmp, "rpc.py")]

help_out = subprocess.check_output(
    runner + ["--help"], env=env, stderr=subprocess.STDOUT, text=True)
if "batch" not in help_out:
    raise SystemExit("launcher --help did not reach fake upstream: %r" % help_out)

out = subprocess.check_output(
    runner + ["--batch-mode"], env=env, stderr=subprocess.STDOUT, text=True)
if "fake-upstream-ok batch_mode=True" not in out:
    raise SystemExit("launcher did not exec sibling spdk_rpc.py: %r" % out)

os.remove(os.path.join(tmp, "spdk_rpc.py"))
env["S3LVOL_RPC_DISABLE_FALLBACKS"] = "1"
proc = subprocess.run(
    runner + ["--help"], env=env, stdout=subprocess.PIPE,
    stderr=subprocess.STDOUT, text=True)
if proc.returncode == 0:
    raise SystemExit("launcher must fail when no upstream rpc.py is present")
if "cannot find SPDK rpc.py" not in proc.stdout:
    raise SystemExit("missing-upstream error unclear: %r" % proc.stdout)
print("launcher sibling + missing-upstream OK")
PY

# Repo layout must use the launcher, not SPDK's rpc.py directly.
# Pin RCOW_LVS_NAME so sourcing does not depend on hostname -s (empty in
# some builder containers, and rcow_common.sh then exits 1).
# shellcheck source=../../scripts/rcow_common.sh
RCOW_LAYOUT_OUT="$(cd "${ROOT}/scripts" && RCOW_LVS_NAME=rpc-py38-compat bash -c '
	source ./rcow_common.sh
	echo "${RCOW_LAYOUT}|${RCOW_SPDK_RPC_PY}"
')" || fail "sourcing rcow_common.sh failed"
[[ "${RCOW_LAYOUT_OUT}" == "repo|${ROOT}/scripts/rpc.py" ]] \
	|| fail "repo layout must resolve RCOW_SPDK_RPC_PY to the launcher (got ${RCOW_LAYOUT_OUT})"

echo "rpc.py 3.8 compat tests OK"
echo "result: 4 passed, 0 failed"
