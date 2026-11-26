#!/usr/bin/env bash
# Guard: the cube-s3lvol startup probe waits only for the target to answer, and
# readiness passes only when rcow_lvstore_attached answers true for this node's
# lvstore. rcow_get_lvstores walks every lvol on the single RPC thread and
# answers [] before attach, so it must not come back. The rendered probes run
# against a mocked s3lvol_rpc.py.
set -euo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
CHART_DIR="$(dirname "$SCRIPT_DIR")"
REPO_ROOT="$(CDPATH= cd -- "$CHART_DIR/../../.." && pwd)"
ENTRYPOINT="$REPO_ROOT/deploy/kubernetes/images/scripts/cube-s3lvol-entrypoint.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

COMMON_SETS=(--set-string mysql.password=test --set-string mysql.rootPassword=test
  --set-string redis.password=test --set cubeS3lvol.enabled=true)
NODE=10.0.0.7
SOCK="$TMP_DIR/s3lvol.sock"

fail() { echo "FAIL: $*" >&2; exit 1; }

# Print the rendered command of cube-s3lvol's <probe> (startupProbe/readinessProbe).
probe_command() {
  python3 - "$1" "$2" <<'PY'
import json
import pathlib
import re
import sys

text = pathlib.Path(sys.argv[1]).read_text()
block = re.search(r"- name: cube-s3lvol\n(.*?)(?:\n          volumeMounts:)", text, re.S)
if not block:
    raise SystemExit("cube-s3lvol container block not found")
probe = re.search(sys.argv[2] + r":\n\s+exec:\n\s+command:\n\s+- /bin/bash\n\s+- -ec\n\s+- (\".*\")\n",
                  block.group(1))
if not probe:
    raise SystemExit(sys.argv[2] + " exec command not found")
# helm's quote emits a Go %q string, which is valid JSON for this ASCII script.
print(json.loads(probe.group(1)))
PY
}

# run_probe <script> <rpc exit code> <rpc stdout> <rpc stderr> [env...]: run the
# probe with python3 replaced by a mock that records argv and answers like
# s3lvol_rpc.py.
run_probe() {
  local script="$1" rc="$2" out="$3" err="$4"
  shift 4
  # shellcheck disable=SC2016 # expanded inside the probe shell, not here
  local prelude='python3() { printf "%s\n" "$@" > "$MOCK_ARGV"; printf "%s" "$MOCK_OUT"; printf "%s" "$MOCK_ERR" >&2; return "$MOCK_RC"; }'
  rm -f "$TMP_DIR/argv"
  env -i PATH="$PATH" MOCK_ARGV="$TMP_DIR/argv" MOCK_OUT="$out" MOCK_ERR="$err" MOCK_RC="$rc" "$@" \
    bash -ec "$prelude
${script//\/var\/run\/s3lvol\/s3lvol.sock/$SOCK}" >/dev/null 2>"$TMP_DIR/stderr"
}

expect_ready() {
  local label="$1"
  shift
  run_probe "$@" || fail "$label: probe failed: $(cat "$TMP_DIR/stderr")"
}

expect_unready() {
  local label="$1"
  shift
  if run_probe "$@"; then
    fail "$label: probe passed"
  fi
}

make_sock() { python3 -c 'import socket, sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' "$SOCK"; }

NOT_INIT='{"code": -32604, "message": "Method may only be called after framework is initialized"}'
NO_METHOD='{"code": -32601, "message": "Method not found"}'
TIMEOUT='no reply within 5s; 0 bytes received'

helm template probe "$CHART_DIR" "${COMMON_SETS[@]}" > "$TMP_DIR/default.yaml"
if grep -q rcow_get_lvstores "$TMP_DIR/default.yaml"; then
  fail "s3lvol probes must not call rcow_get_lvstores"
fi

upstream_lvs="$(bash -c "$(sed -n '/^s3lvol_derived_lvs_name() {$/,/^}$/p' "$ENTRYPOINT")
s3lvol_derived_lvs_name $NODE")"
[[ "$upstream_lvs" == rcow-???????? ]] || fail "could not derive lvstore name from $ENTRYPOINT"

script="$(probe_command "$TMP_DIR/default.yaml" startupProbe)"
expect_unready "startup without socket" "$script" 0 "" "" NODE_NAME="$NODE"
make_sock
expect_unready "startup before framework init" "$script" 1 "" "$NOT_INIT" NODE_NAME="$NODE"
expect_unready "startup RPC timeout" "$script" 1 "" "$TIMEOUT" NODE_NAME="$NODE"
expect_ready "startup target answers" "$script" 0 '{"version":"test"}' "" NODE_NAME="$NODE"
grep -qx rcow_get_build_info "$TMP_DIR/argv" || fail "startup must call rcow_get_build_info"
expect_ready "startup without NODE_NAME" "$script" 0 '{"version":"test"}' ""
rm -f "$SOCK"
echo "ok: startup waits only for the target to answer"

script="$(probe_command "$TMP_DIR/default.yaml" readinessProbe)"
expect_unready "readiness without socket" "$script" 0 "true" "" NODE_NAME="$NODE"
make_sock
expect_ready "readiness attached" "$script" 0 "true" "" NODE_NAME="$NODE"
grep -qx rcow_lvstore_attached "$TMP_DIR/argv" || fail "readiness must call rcow_lvstore_attached"
grep -qx "{\"lvs_name\":\"$upstream_lvs\"}" "$TMP_DIR/argv" \
  || fail "readiness must query the entrypoint-derived lvstore $upstream_lvs: $(cat "$TMP_DIR/argv")"
expect_unready "readiness not attached" "$script" 0 "false" "" NODE_NAME="$NODE"
grep -q "lvstore $upstream_lvs is not attached" "$TMP_DIR/stderr" || fail "readiness must explain the failure"
expect_unready "readiness empty answer" "$script" 0 "" "" NODE_NAME="$NODE"
expect_unready "readiness before framework init" "$script" 1 "" "$NOT_INIT" NODE_NAME="$NODE"
expect_unready "readiness on an image without the RPC" "$script" 1 "" "$NO_METHOD" NODE_NAME="$NODE"
expect_unready "readiness RPC timeout" "$script" 1 "" "$TIMEOUT" NODE_NAME="$NODE"
expect_unready "readiness without NODE_NAME" "$script" 0 "true" ""
rm -f "$SOCK"
echo "ok: readiness passes only when this node's lvstore is attached"

helm template probe "$CHART_DIR" "${COMMON_SETS[@]}" \
  --set-string cubeS3lvol.lvsName=rcow-pinned > "$TMP_DIR/pinned.yaml"
script="$(probe_command "$TMP_DIR/pinned.yaml" readinessProbe)"
make_sock
expect_ready "pinned lvsName" "$script" 0 "true" ""
grep -qx '{"lvs_name":"rcow-pinned"}' "$TMP_DIR/argv" || fail "pinned lvsName must be queried"
rm -f "$SOCK"
echo "ok: cubeS3lvol.lvsName pins the probed lvstore"

if helm template probe "$CHART_DIR" "${COMMON_SETS[@]}" \
    --set cubeS3lvol.probes.readiness.timeoutSeconds=5 >/dev/null 2>"$TMP_DIR/err"; then
  fail "a kubelet timeout not above the RPC timeout must be rejected"
fi
grep -q "rpcTimeoutSeconds" "$TMP_DIR/err" || fail "unexpected error: $(cat "$TMP_DIR/err")"
echo "ok: kubelet timeoutSeconds must exceed the RPC timeout"

if helm template probe "$CHART_DIR" "${COMMON_SETS[@]}" \
    --set cubeS3lvol.probes.rpcTimeoutSeconds=-3 >/dev/null 2>"$TMP_DIR/err"; then
  fail "a negative RPC timeout must be rejected"
fi
grep -q "rpcTimeoutSeconds must be a positive" "$TMP_DIR/err" || fail "unexpected error: $(cat "$TMP_DIR/err")"
echo "ok: the RPC timeout must be positive"
