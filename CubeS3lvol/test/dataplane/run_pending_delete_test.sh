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
#  Step [10] verifies the new lifetime contract: ttl_sec does not invalidate an
#  export while its snapshot exists; deleting the snapshot releases all exports.
#
#  Step [11] delays the pending-delete registry HEAD and GET around unload. A
#  late callback must not use a freed wrapper, and must not restore marks until
#  a later attach's own load (a same-name replacement has a new uuid and is
#  ignored).
#
#  Step [12] writes a lease after the first 404 and verifies that a late reader
#  is still recognised without any export deadline.
#
#  Step [13] restores a pre-lease (lease_aware=false) registry entry and checks
#  that an explicit snapshot delete still releases it -- Cubelet does not need
#  rcow_release_export for inherited exports.
#
#  Step [14] deletes the manifest out of band, then deletes the snapshot: a 404
#  on release must drop the registry entry and finish the destroy.
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

# An idle lease-aware export still pins until the first miss is older than
# S3LVOL_LEASE_MIN_GRACE_SEC, so a delete asked immediately after DONE is deferred.
wait_snapshot_deletable()
{
	local name="$1"
	local deadline=$(( $(date +%s) + 150 ))

	while :; do
		if [ "$(lvol_field "${name}" deletable)" = "YES" ]; then
			return 0
		fi
		if [ "$(date +%s)" -ge "${deadline}" ]; then
			fail "${name} still deletable=$(lvol_field "${name}" deletable) after 150s"
			return 1
		fi
		sleep 1
	done
}

pendel_ns()
{
	python3 -c '
import json, sys
try:
    print(json.load(open(sys.argv[1]))[sys.argv[2]]["ns_name"])
except Exception:
    print("")
' "${RCOW_BSTORE_FILE}" "${RCOW_LVS_NAME}" 2>/dev/null
}

pendel_attach()
{
	local ns="${LVS_NS:-$(pendel_ns)}"

	[ -n "${ns}" ] || ns="${BUCKET}"
	rpc rcow_attach_lvstore \
		"$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		   "${RCOW_LVS_NAME}" "${ns}" "${RCOW_WAL_BDEV}")"
}

pendel_unload()
{
	rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")"
}

pending_load_parked()
{
	rpc rcow_pending_load_hold '{}' 2>/dev/null | python3 -c '
import json, sys
try:
    print(int(json.load(sys.stdin).get("parked", 0)))
except Exception:
    print(0)
'
}

wait_pending_parked()
{
	local want="$1"
	for _ in $(seq 30); do
		[ "$(pending_load_parked)" = "${want}" ] && return 0
		sleep 1
	done
	return 1
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
		# Manifests live at the bucket root (exports/<uuid>.json), outside
		# this lvstore's prefix, so the sweep above does not reach them. The
		# reaper deletes the ones whose snapshot went; anything a failed
		# step left behind is cleaned here.
		for u in ${EXPORT_UUIDS_SEEN:-}; do
			python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" \
				-b "${BUCKET}" -r "$(rcow_cfg_get region)" \
				-p "exports/${u}" >/dev/null 2>&1
		done
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
	EXPORT_UUIDS_SEEN="${EXPORT_UUIDS_SEEN:-} ${EXPORT_UUID}"
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

exports_has()
{
	local uuid="$1"

	rpc rcow_get_exports 2>/dev/null | python3 -c '
import json, sys
want = sys.argv[1]
try:
    for e in json.load(sys.stdin):
        if e.get("export_uuid") == want:
            print(e.get("snapshot", ""))
            sys.exit(0)
except Exception:
    pass
sys.exit(1)
' "${uuid}"
}

export_field()
{
	local uuid="$1" field="$2"

	rpc rcow_get_exports 2>/dev/null | python3 -c '
import json, sys
want, field = sys.argv[1:3]
try:
    for e in json.load(sys.stdin):
        if e.get("export_uuid") == want:
            print(e.get(field, ""))
            sys.exit(0)
except Exception:
    pass
sys.exit(1)
' "${uuid}" "${field}"
}

if SNAP="$(exports_has "${EXPORT_UUID}")" && [ "${SNAP}" = "pinned0" ]; then
	pass "rcow_get_exports lists pinned0 as ${EXPORT_UUID}"
else
	fail "rcow_get_exports does not list the new export"
fi

# Give this export a live reader. An export with no lease remains importable,
# but an explicit snapshot delete may revoke it immediately.
rcow_load_credentials
python3 "${ROOT}/test/tools/s3_put_lease.py" \
	"$(rcow_cfg_get endpoint)" "${BUCKET}" "$(rcow_cfg_get region)" \
	"${RCOW_LVS_NAME}/meta/exports/${EXPORT_UUID}.lease" 20 0 \
	>"${WORKDIR}/put_initial_lease.log" 2>&1 \
	&& pass "a live importer lease was written" \
	|| fail "could not write the importer lease"
for _ in $(seq 30); do
	[ "$(export_field "${EXPORT_UUID}" pin)" = "lease" ] && break
	sleep 1
done
[ "$(export_field "${EXPORT_UUID}" pin)" = "lease" ] \
	&& pass "the source observed the live importer" \
	|| fail "the export never became lease-pinned"

# ==========================================================================
echo ""
echo "=== [2] the blocked delete is recorded, and the snapshot survives"

# The delete does not happen; how it is reported depends on whether this export's
# liveness can be decided yet (see step 6). Deferred while an importer may still
# arrive, refused once the export is known to have no lease at all. What has to
# hold either way -- and what the rest of this suite is built on -- is that the
# snapshot is still here and the intent was recorded.
DEL_OUT="$(python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" --raw \
	rcow_delete_lvol '{"lvol_name":"pinned0"}' 2>&1)"
if echo "${DEL_OUT}" | grep -q '"deferred": *true'; then
	pass "delete of pinned0 deferred while the export pins it"
elif echo "${DEL_OUT}" | grep -q '"bool_value": *false'; then
	pass "delete of pinned0 refused while the export pins it"
else
	fail "delete of the exported pinned0 was carried out: ${DEL_OUT}"
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

if exports_has "${EXPORT_UUID}" >/dev/null; then
	fail "rcow_get_exports still lists the released export"
else
	pass "rcow_get_exports no longer lists the released export"
fi

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
echo "=== [6] an unload keeps the intent, and a re-attach restores it"
#
# The queue is persisted to <prefix>/meta/pending-deletes.json, so a delete that
# is waiting for its blocker is not forgotten by a restart -- the caller was told
# it needed to do nothing else. What makes that safe is the uuid: a restored
# entry names the lvol it was recorded for and can never match a later object
# that happens to reuse the name.
#
# This step used to assert the opposite (the mark dropped on unload), which was
# correct while the queue lived only in memory.

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
EXPORT_UUIDS_SEEN="${EXPORT_UUIDS_SEEN:-} ${EXP2}"
for _ in $(seq 30); do
	[ "$(rpc rcow_get_snapshot_status '{"snapshot_name":"marked1"}' 2>/dev/null | \
		python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("export_status",""))
except Exception:
    print("")')" = "DONE" ] && break
	sleep 1
done

python3 "${ROOT}/test/tools/s3_put_lease.py" \
	"$(rcow_cfg_get endpoint)" "${BUCKET}" "$(rcow_cfg_get region)" \
	"${RCOW_LVS_NAME}/meta/exports/${EXP2}.lease" 20 0 \
	>"${WORKDIR}/put_marked_lease.log" 2>&1 \
	|| fail "could not write marked1's importer lease"
for _ in $(seq 30); do
	[ "$(export_field "${EXP2}" pin)" = "lease" ] && break
	sleep 1
done

rpc rcow_delete_lvol '{"lvol_name":"marked1"}' >/dev/null 2>&1
[ "$(lvol_field marked1 delete_pending)" = "true" ] \
	&& pass "marked1 is marked after the refused delete" \
	|| fail "marked1 was not marked (got '$(lvol_field marked1 delete_pending)')"

# Release the export first, so the restored intent is one that can actually be
# completed after the re-attach rather than one still sitting behind its blocker.
rpc rcow_release_export "$(printf '{"export_uuid":"%s"}' "${EXP2}")" >/dev/null 2>&1
for _ in $(seq 30); do
	[ "$(lvol_field marked1 deletable)" = "YES" ] && break
	sleep 1
done
[ "$(lvol_field marked1 deletable)" = "YES" ] \
	&& pass "marked1 is deletable again" \
	|| info "marked1 not deletable; the next assertions are weaker but still valid"

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

for _ in $(seq 30); do
	[ "$(lvol_field marked1 delete_pending)" = "true" ] && break
	sleep 1
done
[ "$(lvol_field marked1 delete_pending)" = "true" ] \
	&& pass "the intent survived the unload (restored from S3)" \
	|| fail "the intent was lost across the unload (delete_pending='$(lvol_field marked1 delete_pending)')"

# The restored entry has to name the same object and carry an export blocker.
# Both "export" (lease not yet checked, or a live/stale lease-aware pin) and
# "export_legacy" (a restored pre-lease entry) are accepted here -- this step
# is about the intent surviving unload, not about which pin the first HEAD
# produced.
if rpc rcow_get_pending_deletes 2>/dev/null | \
	python3 -c 'import json,sys
try:
    q = json.load(sys.stdin)
except Exception:
    sys.exit(1)
for e in q:
    if e.get("lvol_name") == "marked1":
        sys.exit(0 if e.get("reason") in ("export", "export_legacy") else 1)
sys.exit(1)'; then
	pass "rcow_get_pending_deletes reports marked1 as blocked by its export"
else
	fail "the restored entry is not the one that was recorded: $(rpc rcow_get_pending_deletes 2>&1 | head -c 200)"
fi

# And the retry finishes it, which is what the restored intent was for: the
# export is already released, so nothing blocks the delete any more.
python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" --retry-pending \
	>"${WORKDIR}/retry-after-unload.log" 2>&1
if grep -q "deleted marked1" "${WORKDIR}/retry-after-unload.log"; then
	pass "--retry-pending completed the delete the restart could have lost"
else
	fail "--retry-pending did not act on the restored intent: $(cat "${WORKDIR}/retry-after-unload.log")"
fi

lvol_exists marked1 && fail "marked1 is still there after the retry" \
	|| pass "marked1 is gone"

# ==========================================================================
echo ""
echo "=== [7] a blocker that clears on its own is completed without any retry"
#
# The other half of the queue: clone_count and decouple resolve with no decision
# to make, so rcow_delete_lvol accepts the intent (deferred) and the poller
# completes it once the blocker goes. This is the path that removes the manual
# retry entirely.

rpc rcow_create_snapshot \
	"$(printf '{"lvol_name":"%s","snapshot_name":"defsnap"}' "${VOL}")" \
	>/dev/null && pass "defsnap taken" || fail "could not take defsnap"

# A second clone is what makes it undeletable: blobstore can merge a snapshot
# into one clone, not two.
rpc rcow_create_clone '{"snapshot_name":"defsnap","clone_name":"defclone"}' \
	>/dev/null && pass "defclone created (defsnap now has two clones)" \
	|| fail "could not clone defsnap"

# The extra field, not the envelope: every existing caller still reads the name
# off stdout, so the deferral is only visible with --raw.
DEF_RAW="$(python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" --raw \
	rcow_delete_lvol '{"lvol_name":"defsnap"}' 2>&1)"
if echo "${DEF_RAW}" | grep -q '"deferred": *true'; then
	pass "rcow_delete_lvol reported the delete as deferred"
else
	fail "the delete was not reported as deferred: ${DEF_RAW}"
fi

PLAIN="$(rpc rcow_delete_lvol '{"lvol_name":"defsnap"}' 2>&1)"
[ "${PLAIN}" = "defsnap" ] \
	&& pass "the plain answer is unchanged for callers that just read the name" \
	|| fail "the plain answer changed shape: '${PLAIN}'"

lvol_exists defsnap && pass "defsnap is still there (the intent was queued, not executed)" \
	|| fail "defsnap was deleted while it still had two clones"

if rpc rcow_get_pending_deletes 2>/dev/null | \
	python3 -c 'import json,sys
try:
    q = json.load(sys.stdin)
except Exception:
    sys.exit(1)
for e in q:
    if e.get("lvol_name") == "defsnap":
        sys.exit(0 if e.get("reason") == "clone_count" and e.get("deferred") else 1)
sys.exit(1)'; then
	pass "the queue reports defsnap as clone_count and self-completing"
else
	fail "defsnap is not queued as expected: $(rpc rcow_get_pending_deletes 2>&1 | head -c 200)"
fi

# Asking twice must not enqueue twice.
rpc rcow_delete_lvol '{"lvol_name":"defsnap"}' >/dev/null 2>&1
N_DEF="$(rpc rcow_get_pending_deletes 2>/dev/null | python3 -c 'import json,sys
try:
    print(sum(1 for e in json.load(sys.stdin) if e.get("lvol_name") == "defsnap"))
except Exception:
    print(-1)')"
[ "${N_DEF}" = "1" ] \
	&& pass "asking again did not enqueue a second time" \
	|| fail "the queue has ${N_DEF} entries for defsnap"

# Now clear the blocker. One clone left means blobstore can merge, so the delete
# the poller retries will go through.
rpc rcow_delete_lvol '{"lvol_name":"defclone"}' >/dev/null 2>&1 \
	&& pass "defclone deleted (defsnap now has one clone)" \
	|| fail "could not delete defclone"

# The poller runs once a minute, so this is the one place the test has to wait.
info "waiting for the poller to complete the deferred delete (up to 120s)"
for _ in $(seq 120); do
	lvol_exists defsnap || break
	sleep 1
done

if lvol_exists defsnap; then
	fail "defsnap was still there after 120s; the poller did not complete it"
else
	pass "the poller completed the delete once the blocker cleared"
fi

if rpc rcow_get_pending_deletes 2>/dev/null | grep -q defsnap; then
	fail "the queue still lists defsnap after the delete completed"
else
	pass "the completed entry left the queue"
fi

grep -q "pending delete of 'defsnap' completed" "${RCOW_LOG}" \
	&& pass "the log names the queue as what finished it" \
	|| info "no completion line in the log (the delete still happened)"

# ==========================================================================
echo ""
echo "=== [8] an intent can be withdrawn"

rpc rcow_create_snapshot \
	"$(printf '{"lvol_name":"%s","snapshot_name":"cansnap"}' "${VOL}")" \
	>/dev/null && pass "cansnap taken" || fail "could not take cansnap"
rpc rcow_create_clone '{"snapshot_name":"cansnap","clone_name":"canclone"}' \
	>/dev/null && pass "canclone created" || fail "could not clone cansnap"

rpc rcow_delete_lvol '{"lvol_name":"cansnap"}' >/dev/null 2>&1
[ "$(lvol_field cansnap delete_pending)" = "true" ] \
	&& pass "cansnap is queued" || fail "cansnap was not queued"

rpc rcow_cancel_pending_delete '{"lvol_name":"cansnap"}' >/dev/null 2>&1 \
	&& pass "the intent was withdrawn" || fail "cancel failed"

[ "$(lvol_field cansnap delete_pending)" = "false" ] \
	&& pass "cansnap is no longer queued" \
	|| fail "cansnap is still queued after the cancel"

# Idempotent: cancelling what is not queued leaves the caller in the state they
# asked for, so it succeeds rather than making them handle a race.
rpc rcow_cancel_pending_delete '{"lvol_name":"cansnap"}' >/dev/null 2>&1 \
	&& pass "cancelling an entry that is not queued succeeds" \
	|| fail "the second cancel failed"

# Withdrawing the intent must not have touched the snapshot, and must stop the
# poller from acting on it once the blocker clears.
rpc rcow_delete_lvol '{"lvol_name":"canclone"}' >/dev/null 2>&1
sleep 3
lvol_exists cansnap \
	&& pass "cansnap survived: a withdrawn intent is not completed" \
	|| fail "cansnap was deleted after its intent was withdrawn"

# ==========================================================================
echo ""
echo "=== [9] an export whose importer stopped renewing is completed, and the"
echo "        snapshot delete releases its export internally"
#
# The cross-node case, and the one the deployment actually runs: the control
# plane deletes the snapshot and never calls rcow_release_export. For that to
# reclaim anything, two things have to happen by themselves.
#
#   1. The delete has to complete once the importer is gone. An importer renews
#      a lease object while it still reads the export; when it stops, the lease
#      goes stale, and a stale lease is *evidence* nobody is reading -- unlike a
#      TTL, which lapses on its own whether or not somebody is. So the poller
#      finishes the delete on that basis.
#   2. The delete path releases the export first: manifest deleted, entry
#      dropped, then snapshot destroyed.
#
# The importer here is s3_put_lease.py rather than a second target: what the
# source side consumes is the lease object, and writing it directly controls the
# timing. Real importers renew every 20 seconds.

IGNORED_TTL=30
rpc rcow_create_snapshot \
	"$(printf '{"lvol_name":"%s","snapshot_name":"leased"}' "${VOL}")" \
	>/dev/null && pass "leased taken" || fail "could not take leased"

LEASE_UU="$(rpc rcow_export_snapshot \
	"$(printf '{"snapshot_name":"leased","ttl_sec":%d}' "${IGNORED_TTL}")" \
	2>/dev/null | tr -d ' \t\r\n"')"
[ -n "${LEASE_UU}" ] && pass "leased exported (${LEASE_UU})" \
	|| fail "export of leased failed"
EXPORT_UUIDS_SEEN="${EXPORT_UUIDS_SEEN:-} ${LEASE_UU}"
for _ in $(seq 60); do
	[ "$(rpc rcow_get_snapshot_status '{"snapshot_name":"leased"}' 2>/dev/null | \
		python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("export_status",""))
except Exception:
    print("")')" = "DONE" ] && break
	sleep 1
done

# One renewal is enough: the source caches what it reads, and the point is that a
# lease exists and then stops advancing.
#
# The credentials have to be in the environment for this: everything else here
# talks to the target over RPC, so nothing has loaded them yet.
rcow_load_credentials
if python3 "${ROOT}/test/tools/s3_put_lease.py" \
		"$(rcow_cfg_get endpoint)" "${BUCKET}" "$(rcow_cfg_get region)" \
		"${RCOW_LVS_NAME}/meta/exports/${LEASE_UU}.lease" 10 0 \
		>"${WORKDIR}/put_lease.log" 2>&1; then
	pass "an importer's lease was written for the export"
else
	fail "could not write the lease object: $(tail -1 "${WORKDIR}/put_lease.log")"
fi

# Wait for the source to read it, so the delete below sees a fresh lease.
info "waiting for the source to pick the lease up"
for _ in $(seq 40); do
	[ "$(export_field "${LEASE_UU}" pin)" = "lease" ] && break
	sleep 1
done
[ "$(export_field "${LEASE_UU}" pin)" = "lease" ] \
	&& pass "the source observed the fresh lease" \
	|| fail "the source did not observe the fresh lease"

# A fresh lease must still record the intent -- that is what makes the delete
# happen later without anyone asking again.
DEL_OUT="$(python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" --raw \
	rcow_delete_lvol '{"lvol_name":"leased"}' 2>&1)"
lvol_exists leased \
	&& pass "leased was not deleted while its importer holds the lease" \
	|| fail "leased was deleted with a fresh lease on its export"
[ "$(lvol_field leased delete_pending)" = "true" ] \
	&& pass "the delete was recorded even though the lease is fresh" \
	|| fail "no intent was recorded for leased: ${DEL_OUT}"

if echo "${DEL_OUT}" | grep -q '"deferred": *true'; then
	pass "and it was reported as self-completing"
else
	info "reported as a refusal (${DEL_OUT}); the poller decides either way"
fi

# Now stop renewing: nothing writes the lease again, and nothing releases the
# export. Everything from here has to happen on its own.
#
# The wait covers the grace period plus one poll. Grace is 3x the renew_s in the
# lease, floored at S3LVOL_LEASE_MIN_GRACE_SEC -- the lease above says 10, so the
# floor decides and it is 60 s, not 30. The source then has to notice, which it
# does at its own poll cadence (also floored, at S3LVOL_LEASE_RENEW_MIN_SEC).
info "letting the lease go stale (grace is the 60 s floor, not 3x10) and waiting"
for _ in $(seq 180); do
	lvol_exists leased || break
	sleep 1
done
if lvol_exists leased; then
	fail "leased was still there after 180 s; the stale lease did not complete it"
else
	pass "the poller completed the delete once the lease went stale"
fi

# The delete must already have released the export before removing the snapshot.
info "checking that the snapshot delete released the export"
for _ in $(seq 150); do
	rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s"}' "${LEASE_UU}")" >/dev/null 2>&1 || break
	sleep 1
done
if rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s"}' "${LEASE_UU}")" >/dev/null 2>&1; then
	fail "the export entry outlived the snapshot delete"
else
	pass "the export was released from the registry"
fi

if python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" -b "${BUCKET}" \
		-r "$(rcow_cfg_get region)" -p "exports/${LEASE_UU}.json" \
		--list 2>/dev/null | grep -q .; then
	fail "the released export's manifest is still in the bucket"
else
	pass "and its manifest is gone from the bucket"
fi

grep -q "Released export ${LEASE_UU}" "${RCOW_LOG}" \
	&& pass "the log names the internal export release" \
	|| info "no release line in the log (the entry went all the same)"

# ==========================================================================
echo ""
echo "=== [10] an export remains valid while its snapshot exists,"
echo "         and snapshot delete releases it"
#
# ttl_sec is accepted for wire compatibility but no longer limits a snapshot
# export. If nobody asks to delete the snapshot, both it and its export remain
# usable. The delete request is the lifecycle event that releases the export.

UNIMP_TTL=20
rpc rcow_create_snapshot \
	"$(printf '{"lvol_name":"%s","snapshot_name":"unimported"}' "${VOL}")" \
	>/dev/null && pass "unimported taken" || fail "could not take unimported"

UNIMP_UU="$(rpc rcow_export_snapshot \
	"$(printf '{"snapshot_name":"unimported","ttl_sec":%d}' "${UNIMP_TTL}")" \
	2>/dev/null | tr -d ' \t\r\n"')"
[ -n "${UNIMP_UU}" ] && pass "unimported exported (${UNIMP_UU})" \
	|| fail "export of unimported failed"
EXPORT_UUIDS_SEEN="${EXPORT_UUIDS_SEEN:-} ${UNIMP_UU}"

unimported_status()
{
	rpc rcow_get_snapshot_status '{"snapshot_name":"unimported"}' \
		2>/dev/null | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("export_status",""))
except Exception:
    print("")'
}

for _ in $(seq 60); do
	[ "$(unimported_status)" = "DONE" ] && break
	sleep 1
done
[ "$(unimported_status)" = "DONE" ] \
	&& pass "unimported export is DONE" \
	|| fail "unimported export did not reach DONE"

UNIMP_UU2="$(rpc rcow_export_snapshot '{"snapshot_name":"unimported"}' \
	2>/dev/null | tr -d ' \t\r\n"')"
[ -n "${UNIMP_UU2}" ] && [ "${UNIMP_UU2}" = "${UNIMP_UU}" ] \
	&& pass "a second export of the snapshot returns the same uuid" \
	|| fail "a second export answered '${UNIMP_UU2}', expected ${UNIMP_UU}"

# The first lease check is immediate and 404s. That means no importer is
# currently known; it does not make the export expire.
sleep 3
if rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s"}' "${UNIMP_UU}")" >/dev/null 2>&1; then
	pass "the export is still in the registry"
else
	fail "the export disappeared while its snapshot exists"
fi
if SNAP="$(exports_has "${UNIMP_UU}")" && [ "${SNAP}" = "unimported" ]; then
	pass "rcow_get_exports lists the unimported export"
else
	fail "rcow_get_exports does not list the unimported export"
fi
lvol_exists unimported \
	&& pass "the snapshot is still there" \
	|| fail "the snapshot disappeared"

# Do not ask to delete it. That is the production path: delete_pending stays
# false, and --retry-pending would not see this snapshot.
[ "$(lvol_field unimported delete_pending)" = "false" ] \
	&& pass "no pending delete was recorded" \
	|| fail "unimported was queued without anyone asking"

info "waiting past the deprecated ttl_sec (${UNIMP_TTL}s)"
sleep "$((UNIMP_TTL + 3))"
if rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s"}' "${UNIMP_UU}")" >/dev/null 2>&1; then
	pass "the export remains valid past ttl_sec"
else
	fail "the export expired while its snapshot still exists"
fi

if rpc rcow_import_lvol \
		"$(printf '{"lvol_name":"unimported_clone","export_uuid":"%s","decouple":false}' \
		   "${UNIMP_UU}")" >/dev/null 2>&1; then
	pass "the export can still be imported after the old ttl_sec"
else
	fail "the post-ttl_sec import failed"
fi
rpc rcow_delete_lvol '{"lvol_name":"unimported_clone"}' >/dev/null 2>&1 \
	|| fail "could not remove the post-ttl_sec import"

wait_snapshot_deletable unimported || exit 1

if rpc rcow_delete_lvol '{"lvol_name":"unimported"}' >/dev/null 2>&1; then
	pass "snapshot delete completed"
else
	fail "snapshot delete did not release its export"
fi
lvol_exists unimported \
	&& fail "the snapshot survived its delete" \
	|| pass "the snapshot was deleted"
if rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s"}' "${UNIMP_UU}")" >/dev/null 2>&1; then
	fail "the export survived its snapshot delete"
else
	pass "snapshot delete released the export"
fi

# ==========================================================================
echo ""
echo "=== [11] a late registry load cannot use a freed lvstore"
#
# Attach starts HEAD then GET without waiting. The wrappers used to live in
# the load ctx; unload freed them while CRT still owed a callback. Now the ctx
# holds a uuid and an extra client ref, and the callback re-resolves. Parking
# each stage lets this script unload first.

rpc rcow_create_snapshot \
	"$(printf '{"lvol_name":"%s","snapshot_name":"loadhold"}' "${VOL}")" \
	>/dev/null && pass "loadhold taken" || fail "could not take loadhold"
rpc rcow_create_clone '{"snapshot_name":"loadhold","clone_name":"loadholdc"}' \
	>/dev/null && pass "loadholdc created" || fail "could not create loadholdc"
rpc rcow_delete_lvol '{"lvol_name":"loadhold"}' >/dev/null 2>&1
[ "$(lvol_field loadhold delete_pending)" = "true" ] \
	&& pass "loadhold is marked" \
	|| fail "loadhold was not marked (got '$(lvol_field loadhold delete_pending)')"

# Give the registry PUT a moment so the next attach has something to HEAD.
sleep 2

LVS_NS="$(pendel_ns)"
[ -n "${LVS_NS}" ] || LVS_NS="${BUCKET}"

rpc rcow_pending_load_hold '{"stage":"head"}' >/dev/null \
	&& pass "HEAD completions will park" \
	|| fail "rcow_pending_load_hold head failed"

pendel_unload >/dev/null 2>&1 && pass "unloaded before the held attach" \
	|| fail "unload before held attach failed"

if pendel_attach >/dev/null 2>&1; then
	pass "attached with HEAD parked"
else
	fail "attach with HEAD parked failed"
fi

if wait_pending_parked 1; then
	pass "the registry HEAD is parked"
else
	fail "HEAD did not park (parked=$(pending_load_parked))"
fi
[ "$(lvol_field loadhold delete_pending)" = "true" ] \
	&& fail "HEAD-parked attach already restored the mark" \
	|| pass "the mark is not restored while HEAD is parked"

pendel_unload >/dev/null 2>&1 && pass "unloaded while HEAD is parked" \
	|| fail "unload while HEAD parked failed"

REL="$(rpc rcow_pending_load_hold '{"release":true}' 2>/dev/null | python3 -c '
import json,sys
try:
    print(int(json.load(sys.stdin).get("released", 0)))
except Exception:
    print(0)
')"
[ "${REL}" = "1" ] \
	&& pass "late HEAD was released after unload" \
	|| fail "expected one parked HEAD, released ${REL}"

if rpc rcow_get_pending_deletes >/dev/null 2>&1; then
	pass "the target survived the late HEAD"
else
	fail "the target did not answer after the late HEAD"
fi

if pendel_attach >/dev/null 2>&1; then
	pass "re-attached after the dropped HEAD"
else
	fail "re-attach after dropped HEAD failed"
fi
for _ in $(seq 30); do
	[ "$(lvol_field loadhold delete_pending)" = "true" ] && break
	sleep 1
done
[ "$(lvol_field loadhold delete_pending)" = "true" ] \
	&& pass "a new load restored the mark after the dropped HEAD" \
	|| fail "the mark did not come back after the new attach"

rpc rcow_pending_load_hold '{"stage":"get"}' >/dev/null \
	&& pass "GET completions will park" \
	|| fail "rcow_pending_load_hold get failed"

pendel_unload >/dev/null 2>&1 && pass "unloaded before the GET-held attach" \
	|| fail "unload before GET-held attach failed"

if pendel_attach >/dev/null 2>&1; then
	pass "attached with GET parked"
else
	fail "attach with GET parked failed"
fi

if wait_pending_parked 1; then
	pass "the registry GET is parked"
else
	fail "GET did not park (parked=$(pending_load_parked))"
fi
[ "$(lvol_field loadhold delete_pending)" = "true" ] \
	&& fail "GET-parked attach already restored the mark" \
	|| pass "the mark is not restored while GET is parked"

pendel_unload >/dev/null 2>&1 && pass "unloaded while GET is parked" \
	|| fail "unload while GET parked failed"

REL="$(rpc rcow_pending_load_hold '{"release":true}' 2>/dev/null | python3 -c '
import json,sys
try:
    print(int(json.load(sys.stdin).get("released", 0)))
except Exception:
    print(0)
')"
[ "${REL}" = "1" ] \
	&& pass "late GET was released after unload" \
	|| fail "expected one parked GET, released ${REL}"

if rpc rcow_get_pending_deletes >/dev/null 2>&1; then
	pass "the target survived the late GET"
else
	fail "the target did not answer after the late GET"
fi

# Same uuid re-attach after a dropped GET is a new load, not the parked
# callback filling a wrapper that no longer exists. A same-name store with a
# different uuid would also miss pending_lvs_by_uuid and stay empty.
rpc rcow_pending_load_hold '{"stage":"none"}' >/dev/null 2>&1

if pendel_attach >/dev/null 2>&1; then
	pass "re-attached after the dropped GET"
else
	fail "re-attach after dropped GET failed"
fi
for _ in $(seq 30); do
	[ "$(lvol_field loadhold delete_pending)" = "true" ] && break
	sleep 1
done
[ "$(lvol_field loadhold delete_pending)" = "true" ] \
	&& pass "a new load restored the mark after the dropped GET" \
	|| fail "the mark did not come back after the GET-dropped attach"

# ==========================================================================
echo ""
echo "=== [12] a late importer keeps its lease protection"
#
# The first lease HEAD is immediate and 404s. An importer can still arrive
# later because the export remains valid for the snapshot's lifetime. Once its
# lease appears, a snapshot delete must treat it as a live reader.

LATE_TTL=20
rpc rcow_create_snapshot \
	"$(printf '{"lvol_name":"%s","snapshot_name":"lateimp"}' "${VOL}")" \
	>/dev/null && pass "lateimp taken" || fail "could not take lateimp"

LATE_UU="$(rpc rcow_export_snapshot \
	"$(printf '{"snapshot_name":"lateimp","ttl_sec":%d}' "${LATE_TTL}")" \
	2>/dev/null | tr -d ' \t\r\n"')"
[ -n "${LATE_UU}" ] && pass "lateimp exported (${LATE_UU})" \
	|| fail "export of lateimp failed"
EXPORT_UUIDS_SEEN="${EXPORT_UUIDS_SEEN:-} ${LATE_UU}"

for _ in $(seq 60); do
	[ "$(rpc rcow_get_snapshot_status '{"snapshot_name":"lateimp"}' 2>/dev/null | \
		python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("export_status",""))
except Exception:
    print("")')" = "DONE" ] && break
	sleep 1
done
[ "$(rpc rcow_get_snapshot_status '{"snapshot_name":"lateimp"}' 2>/dev/null | \
	python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("export_status",""))
except Exception:
    print("")')" = "DONE" ] \
	&& pass "lateimp export is DONE" || fail "lateimp export did not reach DONE"

export_lease_absent()
{
	rpc rcow_get_exports 2>/dev/null | python3 -c '
import json, sys
want = sys.argv[1]
try:
    for e in json.load(sys.stdin):
        if e.get("export_uuid") == want:
            print("true" if e.get("lease_absent") else "false")
            sys.exit(0)
except Exception:
    pass
print("missing")
' "${1}"
}

for _ in $(seq 30); do
	[ "$(export_lease_absent "${LATE_UU}")" = "true" ] && break
	sleep 1
done
[ "$(export_lease_absent "${LATE_UU}")" = "true" ] \
	&& pass "the first lease check missed before the importer arrived" \
	|| fail "lease_absent never became true (got '$(export_lease_absent "${LATE_UU}")')"

rcow_load_credentials
if python3 "${ROOT}/test/tools/s3_put_lease.py" \
		"$(rcow_cfg_get endpoint)" "${BUCKET}" "$(rcow_cfg_get region)" \
		"${RCOW_LVS_NAME}/meta/exports/${LATE_UU}.lease" 20 0 \
		>"${WORKDIR}/put_late_lease.log" 2>&1; then
	pass "a lease was written after that 404"
else
	fail "could not write the late lease: $(tail -1 "${WORKDIR}/put_late_lease.log")"
fi

# The deprecated ttl_sec must not reap the export, regardless of the historical
# miss or the later lease.
info "waiting past the deprecated ttl_sec (${LATE_TTL}s)"
sleep "$((LATE_TTL + 3))"
if rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s"}' "${LATE_UU}")" >/dev/null 2>&1; then
	pass "the export remains valid after the old ttl_sec"
else
	fail "the export was reaped while its snapshot still exists"
fi
lvol_exists lateimp \
	&& pass "the snapshot was left in place" \
	|| fail "the snapshot was deleted with the late importer's lease"

if SNAP="$(exports_has "${LATE_UU}")" && [ "${SNAP}" = "lateimp" ]; then
	pass "rcow_get_exports still lists the late-imported export"
else
	fail "rcow_get_exports lost the late-imported export"
fi

info "waiting for the source to pick the late lease up"
for _ in $(seq 40); do
	[ "$(export_field "${LATE_UU}" pin)" = "lease" ] && break
	sleep 1
done
[ "$(export_field "${LATE_UU}" pin)" = "lease" ] \
	&& pass "the source observed the late lease" \
	|| fail "the source did not observe the late lease (pin='$(export_field "${LATE_UU}" pin)')"

LATE_DEL="$(python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" --raw \
	rcow_delete_lvol '{"lvol_name":"lateimp"}' 2>&1)"
lvol_exists lateimp \
	&& pass "snapshot delete was refused while the late importer's lease is live" \
	|| fail "lateimp was deleted under a live late lease: ${LATE_DEL}"
[ "$(lvol_field lateimp delete_pending)" = "true" ] \
	&& pass "the refused delete was recorded" \
	|| fail "no pending delete for lateimp: ${LATE_DEL}"
python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" \
	rcow_cancel_pending_delete '{"lvol_name":"lateimp"}' >/dev/null 2>&1 \
	&& pass "lateimp pending delete cancelled" \
	|| fail "could not cancel the lateimp pending delete"

rpc rcow_release_export "$(printf '{"export_uuid":"%s"}' "${LATE_UU}")" \
	>/dev/null 2>&1 \
	&& pass "lateimp export released" \
	|| fail "could not release the lateimp export"

# ==========================================================================
echo ""
echo "=== [13] an explicit snapshot delete releases a pre-lease export"
#
# A registry entry written before lease_aware existed restores with
# lease_aware=false. Status reports pin=legacy because absence proves nothing
# about a reader that never wrote a lease. The snapshot delete is still the
# revocation: Cubelet does not call rcow_release_export.

rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${VOL}")" >/dev/null 2>&1
rcow_verify_active 30 >/dev/null 2>&1
rpc rcow_create_snapshot \
	"$(printf '{"lvol_name":"%s","snapshot_name":"legsnap"}' "${VOL}")" \
	>/dev/null && pass "legsnap taken" || fail "could not take legsnap"
rpc rcow_deactive_bdev "$(printf '{"device_name":"%s"}' "${VOL}")" >/dev/null 2>&1

LEG_UU="$(rpc rcow_export_snapshot '{"snapshot_name":"legsnap"}' \
	2>/dev/null | tr -d ' \t\r\n"')"
[ -n "${LEG_UU}" ] && pass "legsnap exported (${LEG_UU})" \
	|| fail "export of legsnap failed"
EXPORT_UUIDS_SEEN="${EXPORT_UUIDS_SEEN:-} ${LEG_UU}"
for _ in $(seq 60); do
	[ "$(rpc rcow_get_snapshot_status '{"snapshot_name":"legsnap"}' 2>/dev/null | \
		python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("export_status",""))
except Exception:
    print("")')" = "DONE" ] && break
	sleep 1
done
[ "$(rpc rcow_get_snapshot_status '{"snapshot_name":"legsnap"}' 2>/dev/null | \
	python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("export_status",""))
except Exception:
    print("")')" = "DONE" ] \
	&& pass "legsnap export is DONE" || fail "legsnap export did not reach DONE"

echo "  ---- unload, rewrite the registry as pre-lease, re-attach"
pendel_unload >/dev/null 2>&1 && pass "lvstore unloaded to rewrite the registry" \
	|| { fail "unload failed"; exit 1; }

rcow_load_credentials
S3_ENDPOINT="$(rcow_cfg_get endpoint)" \
S3_BUCKET="${BUCKET}" \
S3_REGION="$(rcow_cfg_get region)" \
S3_EXPORTS_KEY="${RCOW_LVS_NAME}/meta/exports.json" \
S3_TOOLS="${ROOT}/test/tools" \
python3 - "${LEG_UU}" <<'PY'
import datetime, hashlib, hmac, http.client, json, os, sys

uuid = sys.argv[1]
endpoint = os.environ["S3_ENDPOINT"]
bucket = os.environ["S3_BUCKET"]
region = os.environ["S3_REGION"]
key = os.environ["S3_EXPORTS_KEY"]
ak = os.environ["AWS_ACCESS_KEY_ID"]
sk = os.environ["AWS_SECRET_ACCESS_KEY"]
host = "%s.%s" % (bucket, endpoint)
sys.path.insert(0, os.environ["S3_TOOLS"])
from s3_prefix_rm import Client  # noqa: E402

s3 = Client(endpoint, bucket, region, False, ak, sk, False)
status, body = s3.request("GET", s3._base_path() + "/" + key)
if status != 200:
    sys.exit("GET %s -> HTTP %s" % (key, status))
reg = json.loads(body)
found = False
for e in reg.get("exports") or []:
    if e.get("export_uuid") == uuid:
        e["lease_aware"] = False
        found = True
if not found:
    sys.exit("export %s not in %s" % (uuid, key))
payload = json.dumps(reg, separators=(",", ":")).encode()
payload_sha = hashlib.sha256(payload).hexdigest()
now = datetime.datetime.now(datetime.timezone.utc)
amzdate = now.strftime("%Y%m%dT%H%M%SZ")
datestamp = now.strftime("%Y%m%d")
path = "/" + key

def _sign(k, msg):
    return hmac.new(k, msg.encode(), hashlib.sha256).digest()

canonical_headers = ("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n"
                     % (host, payload_sha, amzdate))
signed_headers = "host;x-amz-content-sha256;x-amz-date"
canonical_request = "\n".join(["PUT", path, "", canonical_headers,
                               signed_headers, payload_sha])
scope = "%s/%s/s3/aws4_request" % (datestamp, region)
to_sign = "\n".join(["AWS4-HMAC-SHA256", amzdate, scope,
                     hashlib.sha256(canonical_request.encode()).hexdigest()])
k = _sign(("AWS4" + sk).encode(), datestamp)
k = _sign(k, region)
k = _sign(k, "s3")
k = _sign(k, "aws4_request")
signature = hmac.new(k, to_sign.encode(), hashlib.sha256).hexdigest()
auth = ("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s"
        % (ak, scope, signed_headers, signature))
conn = http.client.HTTPSConnection(host, timeout=60)
try:
    conn.request("PUT", path, body=payload, headers={
        "Host": host,
        "x-amz-date": amzdate,
        "x-amz-content-sha256": payload_sha,
        "Authorization": auth,
        "Content-Type": "application/json",
    })
    resp = conn.getresponse()
    if resp.status not in (200, 204):
        sys.exit("PUT %s -> HTTP %s" % (key, resp.status))
finally:
    conn.close()
PY
if [ $? -eq 0 ]; then
	pass "registry rewritten with lease_aware=false for ${LEG_UU}"
else
	fail "could not rewrite exports.json as pre-lease"
	exit 1
fi

LVS_NS="$(pendel_ns)"
[ -n "${LVS_NS}" ] || LVS_NS="${BUCKET}"
if pendel_attach >/dev/null 2>&1; then
	pass "lvstore re-attached with the pre-lease registry"
else
	fail "re-attach failed"
	exit 1
fi

for _ in $(seq 40); do
	[ "$(export_field "${LEG_UU}" pin)" = "legacy" ] && break
	sleep 1
done
[ "$(export_field "${LEG_UU}" pin)" = "legacy" ] \
	&& pass "the restored export reports pin=legacy" \
	|| fail "pin is '$(export_field "${LEG_UU}" pin)', expected legacy"
[ "$(export_field "${LEG_UU}" lease_aware)" = "False" ] \
	&& pass "lease_aware is false after restore" \
	|| fail "lease_aware='$(export_field "${LEG_UU}" lease_aware)', expected False"

[ "$(lvol_field legsnap deletable)" = "YES" ] \
	&& pass "legsnap reports deletable=YES despite pin=legacy" \
	|| fail "legsnap deletable='$(lvol_field legsnap deletable)', expected YES"

LEG_DEL="$(python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" --raw \
	rcow_delete_lvol '{"lvol_name":"legsnap"}' 2>&1)"
if echo "${LEG_DEL}" | grep -q '"deferred": *true'; then
	fail "legacy snapshot delete was deferred: ${LEG_DEL}"
elif ! lvol_exists legsnap; then
	pass "snapshot delete of a pre-lease export completed without release_export"
else
	fail "legsnap is still there: ${LEG_DEL}"
fi

if rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s"}' "${LEG_UU}")" >/dev/null 2>&1; then
	fail "the pre-lease export is still in the registry"
else
	pass "snapshot delete released the pre-lease export"
fi

# ==========================================================================
echo ""
echo "=== [14] a missing manifest does not wedge snapshot delete"
#
# Bucket lifecycle, another node's release, or a fire-and-forget registry
# rewrite can leave a registry entry whose manifest is already gone. HEAD 404
# used to record PENDING_FAILED without forgetting the entry, so neither the
# poller nor a retry could finish. A 404 means the obligation is already
# discharged: drop the entry and destroy the snapshot.

rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${VOL}")" >/dev/null 2>&1
rcow_verify_active 30 >/dev/null 2>&1
rpc rcow_create_snapshot \
	"$(printf '{"lvol_name":"%s","snapshot_name":"missman"}' "${VOL}")" \
	>/dev/null && pass "missman taken" || fail "could not take missman"
rpc rcow_deactive_bdev "$(printf '{"device_name":"%s"}' "${VOL}")" >/dev/null 2>&1

MISS_UU="$(rpc rcow_export_snapshot '{"snapshot_name":"missman"}' \
	2>/dev/null | tr -d ' \t\r\n"')"
[ -n "${MISS_UU}" ] && pass "missman exported (${MISS_UU})" \
	|| fail "export of missman failed"
EXPORT_UUIDS_SEEN="${EXPORT_UUIDS_SEEN:-} ${MISS_UU}"
for _ in $(seq 60); do
	[ "$(rpc rcow_get_snapshot_status '{"snapshot_name":"missman"}' 2>/dev/null | \
		python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("export_status",""))
except Exception:
    print("")')" = "DONE" ] && break
	sleep 1
done
[ "$(rpc rcow_get_snapshot_status '{"snapshot_name":"missman"}' 2>/dev/null | \
	python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("export_status",""))
except Exception:
    print("")')" = "DONE" ] \
	&& pass "missman export is DONE" || fail "missman export did not reach DONE"

rcow_load_credentials
if python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" -b "${BUCKET}" \
	-r "$(rcow_cfg_get region)" -p "exports/${MISS_UU}.json" \
	>/dev/null 2>&1; then
	pass "manifest removed out of band"
else
	fail "could not delete exports/${MISS_UU}.json"
fi

wait_snapshot_deletable missman || exit 1

MISS_DEL="$(python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" --raw \
	rcow_delete_lvol '{"lvol_name":"missman"}' 2>&1)"
if echo "${MISS_DEL}" | grep -q '"deferred": *true'; then
	fail "missing-manifest snapshot delete was deferred: ${MISS_DEL}"
elif ! lvol_exists missman; then
	pass "snapshot delete completed after the manifest 404"
else
	fail "missman is still there: ${MISS_DEL}"
fi

if rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s"}' "${MISS_UU}")" >/dev/null 2>&1; then
	fail "the export is still in the registry after a missing manifest"
else
	pass "the registry entry was dropped on the missing manifest"
fi
