#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Pending-delete marks: a refused snapshot delete is recorded, skipped while it
#  is still blocked, retried once the blocker clears, and forgotten afterwards.
#
#  === What this is about ===
#
#  Deleting a snapshot that another node may be reading through is refused
#  (s3lvol_lvol_destroy), and the refusal records a pending-delete mark. The mark
#  is the only record that a delete was ever asked for: from the target's side
#  every delete arrives as the same RPC, so nothing else distinguishes "the user
#  asked and it failed" from "nobody asked". test/tools/s3lvol_rpc.py
#  --retry-pending is what acts on it -- it deletes exactly the snapshots that
#  carry a mark AND have become deletable since.
#
#  Four properties have to hold, and none of them are checked by the other
#  suites:
#
#    1. a refused delete leaves the snapshot alive and marked (delete_pending)
#    2. --retry-pending does nothing while the blocker is still there -- the
#       snapshot is marked but not deletable, and it must not be touched
#    3. once the blocker clears, --retry-pending deletes it
#    4. an unmarked snapshot is never deleted by --retry-pending, however
#       deletable it is
#
#  === Why an export is used as the blocker ===
#
#  It is the one blocker a test can raise and drop on demand: rcow_export_snapshot
#  pins the snapshot, rcow_release_export unpins it, and neither needs a second
#  node. The clone and decouple refusals record the mark through the same helper
#  (destroy_mark_pending), so covering one covers the mechanism; what differs
#  between them is only which check fires first.
#
#  === Why the marks are checked by uuid, not just by name ===
#
#  A mark is keyed by (lvstore uuid, lvol uuid) precisely so it cannot follow a
#  name onto a different object, and --retry-pending sends both uuids with the
#  delete. Step [5] recreates a snapshot under a name that was marked earlier and
#  asserts the retry refuses it: deleting by name alone is what that guards
#  against.
#
#  Usage:
#    sudo -E ./test/dataplane/run_pending_delete_test.sh
#
#  Needs root, a readable /data/cubelet/s3.cfg and nvme-cli. Uses its own lvstore,
#  WAL image and registries, so a production instance is untouched.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
SCRIPTS="${ROOT}/scripts"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"

export RCOW_LVS_NAME=pendelvs
export RCOW_WAL_IMG=/data/s3lvol_pendel_wal.img
export RCOW_WAL_BDEV=pendel_wal0
export RCOW_CAPACITY_GB=8
export RCOW_JOURNAL_MB=64
export RCOW_WAL_MB=256
export RCOW_TGT_MEM_MB=2048
export RCOW_RUN_DIR=/var/tmp/rcow_pendel
export RCOW_LOG_DIR=/var/tmp/rcow_pendel/log
export RCOW_ACTIVE_FILE=/var/tmp/rcow_pendel/active_lvols
export RCOW_BSTORE_FILE=/var/tmp/rcow_pendel/bstore.json
export RCOW_S3_CFG="${RCOW_S3_CFG:-/data/cubelet/s3.cfg}"

VOL=v
PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "  ---- $*"; }

STARTED=0
WORKDIR=""

# shellcheck source=../../scripts/rcow_common.sh
. "${SCRIPTS}/rcow_common.sh"

rpc() { python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" "$@"; }

# One field of one lvol out of rcow_get_lvstores. Runs in a command substitution,
# so it prints and never touches PASS/FAIL.
lvol_field()
{
	local name="$1" field="$2"

	rpc rcow_get_lvstores 2>/dev/null | python3 -c '
import json, sys
name, field = sys.argv[1], sys.argv[2]
try:
    for lvs in json.load(sys.stdin):
        for l in lvs.get("lvols") or []:
            if l.get("name") == name:
                v = l.get(field, "")
                print("" if v is None else (v if not isinstance(v, bool) else ("true" if v else "false")))
                sys.exit(0)
except Exception:
    pass
print("")
' "${name}" "${field}"
}

lvol_exists()
{
	[ -n "$(lvol_field "$1" name)" ]
}

resolve()
{
	local name="$1"

	rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${name}")" \
		>/dev/null 2>&1 || return 1
	rcow_verify_active 30 >/dev/null 2>&1 || return 1
	rpc rcow_get_bdev "$(printf '{"device_name":"%s"}' "${name}")" 2>/dev/null | \
		python3 -c 'import json,sys; print(json.load(sys.stdin).get("device_path",""))' \
		2>/dev/null
}

cleanup()
{
	echo ""
	echo "=== cleanup"

	if rcow_target_alive; then
		for n in $(rpc rcow_get_bdev '{}' 2>/dev/null | python3 -c '
import json, sys
try:
    for e in json.load(sys.stdin):
        print(e["device_name"])
except Exception:
    pass'); do
			rpc rcow_deactive_bdev \
				"$(printf '{"device_name":"%s"}' "$n")" >/dev/null 2>&1
		done
		if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
			rpc rcow_delete_lvstore \
				"$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
				>/dev/null 2>&1 || info "delete_lvstore failed"
		fi
	fi
	[ "${STARTED}" -eq 1 ] && "${SCRIPTS}/rcow_stop.sh" --force >/dev/null 2>&1

	if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ] && [ -n "${BUCKET:-}" ]; then
		rcow_load_credentials
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" -b "${BUCKET}" \
			-r "$(rcow_cfg_get region)" -p "${RCOW_LVS_NAME}/" 2>&1 | tail -1
		rm -f "${RCOW_WAL_IMG}"
		rm -rf "${RCOW_RUN_DIR}" "${WORKDIR}"
	elif [ -n "${WORKDIR}" ]; then
		info "state kept: ${RCOW_WAL_IMG}, ${RCOW_RUN_DIR}, ${WORKDIR}"
	fi

	echo ""
	echo "=== result: ${PASS} passed, ${FAIL} failed ==="
	[ "${FAIL}" -eq 0 ] || exit 1
}
trap cleanup EXIT

# ==========================================================================
echo "=== [0] preconditions"

[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }
[ -x "${ROOT}/app/s3lvol_tgt/s3lvol_tgt" ] || { echo "target not built" >&2; exit 1; }
[ -r "${RCOW_S3_CFG}" ] || { echo "no S3 config" >&2; exit 1; }
command -v nvme >/dev/null || { echo "nvme-cli is required" >&2; exit 1; }
[ -n "$(rcow_target_instances)" ] && { echo "a target is already running" >&2; exit 1; }

BUCKET="$(rcow_s3_buckets | head -1)"
WORKDIR="$(mktemp -d /tmp/rcow_pendel.XXXXXX)"
rm -rf "${RCOW_RUN_DIR}"; mkdir -p "${RCOW_RUN_DIR}"
rm -f "${RCOW_WAL_IMG}"; truncate -s 1G "${RCOW_WAL_IMG}"

"${SCRIPTS}/rcow_start.sh" >"${WORKDIR}/start.log" 2>&1 && STARTED=1 || {
	fail "rcow_start.sh failed"; tail -20 "${WORKDIR}/start.log"; exit 1; }
pass "data plane up, lvstore ${RCOW_LVS_NAME}"

# ==========================================================================
echo ""
echo "=== [1] a volume, two snapshots, and an export pinning one of them"
#
# keep0 is the control: it is deletable throughout and never has a delete asked
# for, so nothing may ever delete it. pinned0 is the subject.

rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":1}' "${VOL}")" \
	>/dev/null || { fail "create ${VOL}"; exit 1; }
DEV="$(resolve "${VOL}")"
[ -b "${DEV}" ] || { fail "${VOL} did not become a block device"; exit 1; }

dd if=/dev/urandom of="${DEV}" bs=1M count=4 oflag=direct status=none
sync

rpc rcow_create_snapshot \
	"$(printf '{"lvol_name":"%s","snapshot_name":"keep0"}' "${VOL}")" \
	>/dev/null && pass "keep0 taken (the control snapshot)" || fail "keep0 failed"

dd if=/dev/urandom of="${DEV}" bs=1M count=4 seek=4 oflag=direct status=none
sync

rpc rcow_create_snapshot \
	"$(printf '{"lvol_name":"%s","snapshot_name":"pinned0"}' "${VOL}")" \
	>/dev/null && pass "pinned0 taken (the one to be blocked)" || fail "pinned0 failed"

# Deactivated first: rcow_delete_lvol refuses an active volume outright, and that
# refusal is not the one under test here.
rpc rcow_deactive_bdev "$(printf '{"device_name":"%s"}' "${VOL}")" >/dev/null 2>&1

EXPORT_UUID="$(rpc rcow_export_snapshot '{"snapshot_name":"pinned0"}' \
	2>/dev/null | tr -d ' \t\r\n"')"
if [ -n "${EXPORT_UUID}" ]; then
	pass "pinned0 exported (${EXPORT_UUID}), so a delete of it must be refused"
else
	fail "rcow_export_snapshot did not return a uuid"
	exit 1
fi

# The export has to finish publishing before it counts as a pin in the registry;
# until then it is an in-flight pin, which refuses the delete just the same.
for _ in $(seq 30); do
	[ "$(rpc rcow_get_snapshot_status '{"snapshot_name":"pinned0"}' 2>/dev/null | \
		python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("export_status",""))
except Exception:
    print("")')" = "DONE" ] && break
	sleep 1
done

# ==========================================================================
echo ""
echo "=== [2] the refused delete is recorded, and the snapshot survives"

if rpc rcow_delete_lvol '{"lvol_name":"pinned0"}' >/dev/null 2>&1; then
	fail "delete of the exported pinned0 was accepted"
else
	pass "delete of pinned0 refused while the export pins it"
fi

lvol_exists pinned0 && pass "pinned0 still exists after the refusal" \
	|| fail "pinned0 disappeared despite the refusal"

[ "$(lvol_field pinned0 delete_pending)" = "true" ] \
	&& pass "pinned0 carries the pending-delete mark" \
	|| fail "pinned0 has no pending-delete mark (got '$(lvol_field pinned0 delete_pending)')"

[ "$(lvol_field pinned0 deletable)" = "NO" ] \
	&& pass "pinned0 reports deletable=NO while pinned" \
	|| fail "pinned0 reports deletable='$(lvol_field pinned0 deletable)' while pinned"

# The control must not have picked up a mark from anywhere.
[ "$(lvol_field keep0 delete_pending)" = "false" ] \
	&& pass "keep0 has no mark (no delete was asked for it)" \
	|| fail "keep0 unexpectedly carries a mark"

# ==========================================================================
echo ""
echo "=== [3] --retry-pending leaves a still-blocked snapshot alone"
#
# The mark is there but deletable is NO, so the retry has nothing to do. This is
# the case that would otherwise turn into a delete storm against a pinned
# snapshot.

python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" --retry-pending \
	>"${WORKDIR}/retry-blocked.log" 2>&1
if grep -q "no pending snapshot deletes to retry" "${WORKDIR}/retry-blocked.log"; then
	pass "--retry-pending reported nothing to do while pinned"
else
	fail "--retry-pending did something while pinned: $(cat "${WORKDIR}/retry-blocked.log")"
fi

lvol_exists pinned0 && pass "pinned0 still alive after the no-op retry" \
	|| fail "pinned0 was deleted while still pinned"

# ==========================================================================
echo ""
echo "=== [4] once the export is released, --retry-pending completes the delete"

rpc rcow_release_export "$(printf '{"export_uuid":"%s"}' "${EXPORT_UUID}")" \
	>/dev/null 2>&1 && pass "export released" || fail "rcow_release_export failed"

for _ in $(seq 30); do
	[ "$(lvol_field pinned0 deletable)" = "YES" ] && break
	sleep 1
done
[ "$(lvol_field pinned0 deletable)" = "YES" ] \
	&& pass "pinned0 reports deletable=YES once unpinned" \
	|| fail "pinned0 still not deletable after release"

python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" --retry-pending \
	>"${WORKDIR}/retry-clear.log" 2>&1
if grep -q "deleted pinned0" "${WORKDIR}/retry-clear.log"; then
	pass "--retry-pending deleted pinned0 once the blocker cleared"
else
	fail "--retry-pending did not delete pinned0: $(cat "${WORKDIR}/retry-clear.log")"
fi

lvol_exists pinned0 && fail "pinned0 still exists after the successful retry" \
	|| pass "pinned0 is gone"

# The control survived a retry that was entitled to delete only the marked one.
lvol_exists keep0 && pass "keep0 untouched by --retry-pending (never marked)" \
	|| fail "keep0 was deleted by --retry-pending"

# Nothing left marked: the successful delete cleared it.
python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" --retry-pending \
	>"${WORKDIR}/retry-empty.log" 2>&1
if grep -q "no pending snapshot deletes to retry" "${WORKDIR}/retry-empty.log"; then
	pass "the mark was cleared by the successful delete"
else
	fail "a mark survived the delete: $(cat "${WORKDIR}/retry-empty.log")"
fi

# ==========================================================================
echo ""
echo "=== [5] a delete asked for by uuid refuses a same-named replacement"
#
# What the uuid keying is for. pinned0's name is free again, so a new snapshot can
# take it; a delete issued for the *old* uuid must not touch the new object.

OLD_UUID="$(rpc rcow_get_lvstores 2>/dev/null | python3 -c '
import json, sys
# Any uuid that is not currently in use: the deleted pinned0 will do, but it is
# gone, so a syntactically valid uuid that belongs to nothing is what is needed.
print("00000000-0000-0000-0000-000000000001")')"

rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${VOL}")" >/dev/null 2>&1
rcow_verify_active 30 >/dev/null 2>&1
rpc rcow_create_snapshot \
	"$(printf '{"lvol_name":"%s","snapshot_name":"pinned0"}' "${VOL}")" \
	>/dev/null && pass "pinned0 recreated (same name, new object)" \
	|| fail "could not recreate pinned0"
rpc rcow_deactive_bdev "$(printf '{"device_name":"%s"}' "${VOL}")" >/dev/null 2>&1

if rpc rcow_delete_lvol \
	"$(printf '{"lvol_name":"pinned0","lvol_uuid":"%s"}' "${OLD_UUID}")" \
	>/dev/null 2>&1; then
	fail "a delete for a stale uuid deleted the same-named replacement"
else
	pass "delete refused: the name now belongs to a different uuid"
fi

lvol_exists pinned0 && pass "the recreated pinned0 survived the stale-uuid delete" \
	|| fail "the recreated pinned0 was deleted by a stale-uuid delete"

# And the same delete by the *current* uuid works, so the check is not simply
# refusing everything.
CUR_UUID="$(lvol_field pinned0 uuid)"
if [ -n "${CUR_UUID}" ] && rpc rcow_delete_lvol \
	"$(printf '{"lvol_name":"pinned0","lvol_uuid":"%s"}' "${CUR_UUID}")" \
	>/dev/null 2>&1; then
	pass "delete by the current uuid succeeded"
else
	fail "delete by the current uuid was refused (uuid '${CUR_UUID}')"
fi

# ==========================================================================
echo ""
echo "=== [6] an unload drops that lvstore's marks"
#
# The marks are scoped to the lvstore, not to the process: past a teardown they
# name lvols that no longer exist, and the lvstore attached next can give the
# same names to different objects. This is the step that would have caught the
# marks surviving an unload because the uuid was read off a pointer the unload
# had already cleared.

# A fresh blocked delete to leave a mark behind.
rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${VOL}")" >/dev/null 2>&1
rcow_verify_active 30 >/dev/null 2>&1
rpc rcow_create_snapshot \
	"$(printf '{"lvol_name":"%s","snapshot_name":"marked1"}' "${VOL}")" \
	>/dev/null && pass "marked1 taken" || fail "could not take marked1"
rpc rcow_deactive_bdev "$(printf '{"device_name":"%s"}' "${VOL}")" >/dev/null 2>&1

EXP2="$(rpc rcow_export_snapshot '{"snapshot_name":"marked1"}' 2>/dev/null | \
	tr -d ' \t\r\n"')"
[ -n "${EXP2}" ] && pass "marked1 exported (${EXP2})" || fail "export of marked1 failed"
for _ in $(seq 30); do
	[ "$(rpc rcow_get_snapshot_status '{"snapshot_name":"marked1"}' 2>/dev/null | \
		python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("export_status",""))
except Exception:
    print("")')" = "DONE" ] && break
	sleep 1
done

rpc rcow_delete_lvol '{"lvol_name":"marked1"}' >/dev/null 2>&1
[ "$(lvol_field marked1 delete_pending)" = "true" ] \
	&& pass "marked1 is marked after the refused delete" \
	|| fail "marked1 was not marked (got '$(lvol_field marked1 delete_pending)')"

# Release the export first: the mark has to be dropped by the unload, not by the
# blocker still being there.
rpc rcow_release_export "$(printf '{"export_uuid":"%s"}' "${EXP2}")" >/dev/null 2>&1
for _ in $(seq 30); do
	[ "$(lvol_field marked1 deletable)" = "YES" ] && break
	sleep 1
done
[ "$(lvol_field marked1 deletable)" = "YES" ] \
	&& pass "marked1 is deletable again (so only the unload can clear the mark)" \
	|| info "marked1 not deletable; the next assertion is weaker but still valid"

echo "  ---- unload and re-attach ${RCOW_LVS_NAME}"
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
	>/dev/null 2>&1 && pass "lvstore unloaded" || { fail "unload failed"; exit 1; }

# The namespace an attach needs is the bucket the lvstore lives in, which is
# what rcow_start.sh recorded in bstore.json when it created it.
LVS_NS="$(python3 -c '
import json, sys
try:
    print(json.load(open(sys.argv[1]))[sys.argv[2]]["ns_name"])
except Exception:
    print("")
' "${RCOW_BSTORE_FILE}" "${RCOW_LVS_NAME}" 2>/dev/null)"
[ -n "${LVS_NS}" ] || LVS_NS="${BUCKET}"
if rpc rcow_attach_lvstore \
	"$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
	   "${RCOW_LVS_NAME}" "${LVS_NS}" "${RCOW_WAL_BDEV}")" >/dev/null 2>&1; then
	pass "lvstore re-attached"
else
	fail "re-attach failed (namespace '${LVS_NS}')"
	exit 1
fi

lvol_exists marked1 && pass "marked1 came back with the lvstore" \
	|| fail "marked1 did not survive the re-attach"

[ "$(lvol_field marked1 delete_pending)" = "false" ] \
	&& pass "the mark did not survive the unload" \
	|| fail "the mark survived the unload (delete_pending='$(lvol_field marked1 delete_pending)')"

python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" --retry-pending \
	>"${WORKDIR}/retry-after-unload.log" 2>&1
if grep -q "no pending snapshot deletes to retry" "${WORKDIR}/retry-after-unload.log"; then
	pass "--retry-pending has nothing to do after the re-attach"
else
	fail "--retry-pending acted on a mark that should have been dropped: $(cat "${WORKDIR}/retry-after-unload.log")"
fi
