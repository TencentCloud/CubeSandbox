#!/usr/bin/env bash
# Guard: cube-s3lvol probes must be a cheap by-name lvstore lookup that passes
# only once this node's lvstore is attached. rcow_get_lvstores walks every
# lvol on the single RPC thread and answers [] before attach, so it must not
# come back. The rendered probe runs against a mocked s3lvol_rpc.py.
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

# run_probe <script> <rpc exit code> <rpc stderr> [env...]: run the probe with
# python3 replaced by a mock that records argv and answers like s3lvol_rpc.py.
run_probe() {
  local script="$1" rc="$2" err="$3"
  shift 3
  # shellcheck disable=SC2016 # expanded inside the probe shell, not here
  local prelude='python3() { printf "%s\n" "$@" > "$MOCK_ARGV"; printf "%s" "$MOCK_ERR" >&2; return "$MOCK_RC"; }'
  rm -f "$TMP_DIR/argv"
  env -i PATH="$PATH" MOCK_ARGV="$TMP_DIR/argv" MOCK_ERR="$err" MOCK_RC="$rc" "$@" \
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

helm template probe "$CHART_DIR" "${COMMON_SETS[@]}" > "$TMP_DIR/default.yaml"
if grep -q rcow_get_lvstores "$TMP_DIR/default.yaml"; then
  fail "s3lvol probes must not call rcow_get_lvstores"
fi

upstream_lvs="$(bash -c "$(sed -n '/^s3lvol_derived_lvs_name() {$/,/^}$/p' "$ENTRYPOINT")
s3lvol_derived_lvs_name $NODE")"
[[ "$upstream_lvs" == rcow-???????? ]] || fail "could not derive lvstore name from $ENTRYPOINT"

for kind in startupProbe readinessProbe; do
  script="$(probe_command "$TMP_DIR/default.yaml" "$kind")"

  expect_unready "$kind without socket" "$script" 0 "" NODE_NAME="$NODE"

  python3 -c 'import socket, sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' "$SOCK"

  expect_ready "$kind attached lvstore" "$script" 1 \
    "lvstore '$upstream_lvs' has no lvol '__k8s_probe__'" NODE_NAME="$NODE"
  grep -qx rcow_get_lvol "$TMP_DIR/argv" || fail "$kind must call rcow_get_lvol"
  grep -qx "{\"lvs_name\":\"$upstream_lvs\",\"lvol_name\":\"__k8s_probe__\"}" "$TMP_DIR/argv" \
    || fail "$kind must query the entrypoint-derived lvstore $upstream_lvs: $(cat "$TMP_DIR/argv")"

  expect_unready "$kind detached lvstore" "$script" 1 \
    "lvstore '$upstream_lvs' not found" NODE_NAME="$NODE"
  grep -q "s3lvol not ready" "$TMP_DIR/stderr" || fail "$kind must explain the failure"
  expect_unready "$kind before framework init" "$script" 1 \
    '{"code": -32604, "message": "Method may only be called after framework is initialized"}' \
    NODE_NAME="$NODE"
  expect_unready "$kind another node's lvstore" "$script" 1 \
    "lvstore 'rcow-00000000' has no lvol '__k8s_probe__'" NODE_NAME="$NODE"
  expect_unready "$kind RPC timeout" "$script" 1 "timed out" NODE_NAME="$NODE"
  expect_unready "$kind without NODE_NAME" "$script" 0 ""
  expect_ready "$kind lvol found" "$script" 0 "" NODE_NAME="$NODE"

  rm -f "$SOCK"
done
echo "ok: probes ready only for this node's attached lvstore, via rcow_get_lvol"

helm template probe "$CHART_DIR" "${COMMON_SETS[@]}" \
  --set-string cubeS3lvol.lvsName=rcow-pinned > "$TMP_DIR/pinned.yaml"
script="$(probe_command "$TMP_DIR/pinned.yaml" readinessProbe)"
python3 -c 'import socket, sys; socket.socket(socket.AF_UNIX).bind(sys.argv[1])' "$SOCK"
expect_ready "pinned lvsName" "$script" 1 "lvstore 'rcow-pinned' has no lvol '__k8s_probe__'"
grep -q '"lvs_name":"rcow-pinned"' "$TMP_DIR/argv" || fail "pinned lvsName must be queried"
echo "ok: cubeS3lvol.lvsName pins the probed lvstore"

if helm template probe "$CHART_DIR" "${COMMON_SETS[@]}" \
    --set cubeS3lvol.probes.readiness.timeoutSeconds=5 >/dev/null 2>"$TMP_DIR/err"; then
  fail "a kubelet timeout not above the RPC timeout must be rejected"
fi
grep -q "rpcTimeoutSeconds" "$TMP_DIR/err" || fail "unexpected error: $(cat "$TMP_DIR/err")"
echo "ok: kubelet timeoutSeconds must exceed the RPC timeout"
