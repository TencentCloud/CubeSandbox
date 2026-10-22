#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Offline coverage for active-registry recovery classification. The target RPC
# is mocked so open failures and version-skew cases are deterministic.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

export RCOW_LVS_NAME=replay-test
export RCOW_RUN_DIR="${TMP}/run"
export RCOW_ACTIVE_FILE="${TMP}/active.json"
export RCOW_REPLAY_FILE="${TMP}/active.json.replay"
mkdir -p "${RCOW_RUN_DIR}"

# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "[PASS] $*"; }
fail() { FAIL=$((FAIL + 1)); echo "[FAIL] $*"; }

RPC_LVSTORES='[]'
RPC_BATCH='{"restored":0,"already_active":0,"failed":[]}'
LOG="${TMP}/log"

rcow_rpc()
{
	case "$1" in
	rcow_get_lvstores) printf '%s\n' "${RPC_LVSTORES}" ;;
	rcow_active_bdev_batch) printf '%s\n' "${RPC_BATCH}" ;;
	*) return 1 ;;
	esac
}
rcow_log() { printf 'LOG %s\n' "$*" >>"${LOG}"; }
rcow_warn() { printf 'WARN %s\n' "$*" >>"${LOG}"; }
rcow_err() { printf 'ERR %s\n' "$*" >>"${LOG}"; }

write_plan()
{
	python3 - "${RCOW_REPLAY_FILE}" "$@" <<'PY'
import json, sys
path = sys.argv[1]
d = {}
for item in sys.argv[2:]:
    name, uuid = item.split("=", 1)
    d[name] = {"uuid": uuid, "subsys": 1, "nsid": len(d) + 1}
json.dump(d, open(path, "w"))
PY
}

plan_names()
{
	python3 - "${RCOW_REPLAY_FILE}" <<'PY'
import json, os, sys
if not os.path.exists(sys.argv[1]):
    print("")
else:
    print(" ".join(sorted(json.load(open(sys.argv[1])).keys())))
PY
}

UUID_A=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa
UUID_B=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb
UUID_C=cccccccc-cccc-cccc-cccc-cccccccccccc

write_plan "ok=${UUID_A}" "open-failed=${UUID_B}" "gone=${UUID_C}"
RPC_LVSTORES="$(cat <<JSON
[{"lvs_name":"replay-test","lvol_open_failures_complete":true,
  "lvols":[{"name":"ok","uuid":"${UUID_A}"}],
  "lvol_open_failures":[{"name":"open-failed","uuid":"${UUID_B}",
    "errno":-12,"error":"Cannot allocate memory","attempts":32}]}]
JSON
)"
RPC_BATCH='{"restored":1,"already_active":0,"failed":[]}'
: >"${LOG}"
if rcow_replay_registry; then
	fail "open failure made replay succeed"
elif [ "$(plan_names)" = "ok open-failed" ] &&
     grep -q "replay: 1 restored, 1 refused, 1 failed" "${LOG}" &&
     grep -q "Cannot allocate memory" "${LOG}"; then
	pass "open failure is named and the complete retry transaction is retained"
else
	fail "open failure classification or plan retention"
fi

write_plan "unknown=${UUID_A}"
RPC_LVSTORES='[{"lvs_name":"replay-test","lvols":[]}]'
RPC_BATCH='{"restored":0,"already_active":0,"failed":[]}'
: >"${LOG}"
if rcow_replay_registry; then
	fail "incomplete old-target inventory made replay succeed"
elif [ "$(plan_names)" = "unknown" ] &&
     grep -q "does not report a complete" "${LOG}"; then
	pass "old target absence is conservatively retained"
else
	fail "old-target compatibility classification"
fi

write_plan "missing-store=${UUID_A}"
RPC_LVSTORES="[{\"lvs_name\":\"other-store\",\"lvol_open_failures_complete\":true,\"lvols\":[{\"name\":\"missing-store\",\"uuid\":\"${UUID_A}\"}],\"lvol_open_failures\":[]}]"
: >"${LOG}"
if rcow_replay_registry; then
	fail "an unrelated lvstore made the inventory look complete"
elif [ "$(plan_names)" = "missing-store" ] &&
     grep -q "does not report a complete" "${LOG}"; then
	pass "missing expected lvstore retains the replay plan"
else
	fail "lvstore-scoped inventory handling"
fi

write_plan "replaced=${UUID_A}"
RPC_LVSTORES="[{\"lvs_name\":\"replay-test\",\"lvol_open_failures_complete\":true,\"lvols\":[{\"name\":\"replaced\",\"uuid\":\"${UUID_B}\"}],\"lvol_open_failures\":[]}]"
: >"${LOG}"
if rcow_replay_registry &&
   [ ! -e "${RCOW_REPLAY_FILE}" ] &&
   grep -q "different volume has taken the name" "${LOG}" &&
   grep -q "replay: 0 restored, 1 refused, 0 failed" "${LOG}"; then
	pass "UUID mismatch is terminally refused and removed from the plan"
else
	fail "UUID mismatch replay classification"
fi

write_plan "batch-failed=${UUID_A}"
RPC_LVSTORES="[{\"lvs_name\":\"replay-test\",\"lvol_open_failures_complete\":true,\"lvols\":[{\"name\":\"batch-failed\",\"uuid\":\"${UUID_A}\"}],\"lvol_open_failures\":[]}]"
RPC_BATCH='{"restored":0,"already_active":0,"failed":[{"device_name":"batch-failed","error":"attach ENOMEM"}]}'
: >"${LOG}"
if rcow_replay_registry; then
	fail "batch failure made replay succeed"
elif [ "$(plan_names)" = "batch-failed" ] &&
     grep -q "attach ENOMEM" "${LOG}"; then
	pass "batch error is printed and retained"
else
	fail "batch error reporting or retention"
fi

printf '{broken\n' >"${RCOW_REPLAY_FILE}"
cp "${RCOW_REPLAY_FILE}" "${TMP}/before"
if rcow_registry_keep "${RCOW_REPLAY_FILE}" keep-me; then
	fail "malformed replay plan rewrite succeeded"
elif cmp -s "${TMP}/before" "${RCOW_REPLAY_FILE}"; then
	pass "failed plan rewrite preserves the original"
else
	fail "failed plan rewrite damaged the original"
fi

write_plan "keep-me=${UUID_A}" "drop-me=${UUID_B}"
cp "${RCOW_REPLAY_FILE}" "${TMP}/before"
mkdir "${RCOW_REPLAY_FILE}.tmp.$$"
if rcow_registry_keep "${RCOW_REPLAY_FILE}" keep-me; then
	fail "temporary-file write failure was reported as success"
elif cmp -s "${TMP}/before" "${RCOW_REPLAY_FILE}"; then
	pass "temporary-file write failure preserves the original plan"
else
	fail "temporary-file write failure damaged the original plan"
fi
rmdir "${RCOW_REPLAY_FILE}.tmp.$$"

write_plan "keep-me=${UUID_A}" "drop-me=${UUID_B}"
cp "${RCOW_REPLAY_FILE}" "${TMP}/before"
mv() { return 1; }
if rcow_registry_keep "${RCOW_REPLAY_FILE}" keep-me; then
	fail "rename failure was reported as success"
elif cmp -s "${TMP}/before" "${RCOW_REPLAY_FILE}"; then
	pass "rename failure preserves the original plan"
else
	fail "rename failure damaged the original plan"
fi
unset -f mv

echo "result: ${PASS} passed, ${FAIL} failed"
[ "${FAIL}" -eq 0 ]
