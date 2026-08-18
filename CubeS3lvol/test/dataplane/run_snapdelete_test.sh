#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Deleting a snapshot while the volume it was taken from is still alive.
#
#  === What this is about ===
#
#  Every snapshot of a live volume has exactly one clone: taking a snapshot turns
#  the origin blob into a clone of the new snapshot, and taking another inserts
#  itself into that chain. So for a volume L with snapshots s0, s1, s2 taken in
#  order, the chain is
#
#      s0 <- s1 <- s2 <- L        (each arrow: "is the parent of")
#
#  and every one of s0, s1, s2 has a clone_count of exactly 1.
#
#  blobstore supports deleting such a snapshot. bs_is_blob_deletable()
#  (blobstore.c:8714) refuses unconditionally only above one clone; at exactly one
#  it sets *update_clone and merges the snapshot into its clone, and
#  spdk_lvol_destroy() cooperates by resolving that clone's lvol first (lvol.c:1584)
#  so it can be fixed up afterwards.
#
#  A pre-flight check in s3lvol_lvol_destroy() used to refuse at clone_count > 0,
#  which rejected exactly what the layers below implement. The visible effect was
#  that no snapshot could be deleted until the volume itself was gone, so a chain of
#  N snapshots could only be dismantled from the leaf end -- each delete freeing the
#  next. This suite exists because that was measured as EBUSY on a perfectly
#  ordinary "delete an old snapshot" and mistaken for a blobstore property.
#
#  === Why the assertions are about data, not return codes ===
#
#  The delete is a *merge*: the snapshot's clusters have to end up owned by its
#  clone, and the clone's own writes must win where both have a cluster. A delete
#  that returns 0 and silently loses the merged clusters would pass any check that
#  only looks at status, and would show up much later as a volume reading zeroes
#  where it used to read data. So each step here writes a distinguishable pattern,
#  and after every delete the volume is re-read and compared against what it must
#  contain.
#
#  The layout is chosen so that a wrong merge cannot look right. The volume is
#  divided into four 4 MiB regions:
#
#      region 0: written before s0, never touched again  -> lives only in s0
#      region 1: written between s0 and s1               -> lives only in s1
#      region 2: written between s1 and s2               -> lives only in s2
#      region 3: written after s2                        -> lives only in L
#
#  Reading L must always return all four, whichever snapshots have been deleted:
#  region 0 can only be reached by walking the whole chain, so if a merge drops
#  clusters it is region 0 or 1 that breaks, not the ones L wrote itself.
#
#  Usage:
#    sudo -E ./test/dataplane/run_snapdelete_test.sh
#
#  Needs root, a readable /data/cubelet/cos.cfg and nvme-cli. Uses its own lvstore,
#  WAL image and registries, so a production instance is untouched.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
SCRIPTS="${ROOT}/scripts"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"

export RCOW_LVS_NAME=snapdelvs
export RCOW_WAL_IMG=/data/s3lvol_snapdel_wal.img
export RCOW_WAL_BDEV=snapdel_wal0
export RCOW_CAPACITY_GB=8
export RCOW_JOURNAL_MB=64
export RCOW_WAL_MB=256
export RCOW_TGT_MEM_MB=2048
export RCOW_RUN_DIR=/var/tmp/rcow_snapdel
export RCOW_LOG_DIR=/var/tmp/rcow_snapdel/log
export RCOW_ACTIVE_FILE=/var/tmp/rcow_snapdel/active_lvols
export RCOW_BSTORE_FILE=/var/tmp/rcow_snapdel/bstore.json
export RCOW_COS_CFG="${RCOW_COS_CFG:-/data/cubelet/cos.cfg}"

VOL=v
REGION_MB=4
NREGIONS=4

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "  ---- $*"; }

STARTED=0
WORKDIR=""

# shellcheck source=../../scripts/rcow_common.sh
. "${SCRIPTS}/rcow_common.sh"

rpc() { python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" "$@"; }

# Echo the host device for a volume, or nothing. Runs in a command substitution, so
# it must not touch PASS/FAIL -- the caller decides what empty means.
resolve()
{
	local name="$1"

	rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${name}")" \
		>/dev/null 2>&1 || return 1
	rcow_verify_active 30 >/dev/null 2>&1 || {
		echo "  ---- ${name}: verify_active timed out" >&2
		return 1
	}
	rpc rcow_get_bdev "$(printf '{"device_name":"%s"}' "${name}")" 2>/dev/null | \
		python3 -c 'import json,sys; print(json.load(sys.stdin).get("device_path",""))' \
		2>/dev/null
}

# Refuses a target that is not a block device: an earlier probe turned a
# not-yet-existing /dev path into a 0-byte regular file this way, and the leftover
# then broke every later run.
write_region()
{
	local dev="$1" region="$2" src="$3"

	if [ -z "${dev}" ] || [ ! -b "${dev}" ]; then
		fail "refusing to write to '${dev}': not a block device"
		return 1
	fi
	dd if="${src}" of="${dev}" bs=1M count="${REGION_MB}" \
		seek=$((region * REGION_MB)) oflag=direct status=none
}

read_region()
{
	dd if="$1" bs=1M count="${REGION_MB}" skip=$(($2 * REGION_MB)) \
		iflag=direct status=none 2>/dev/null | md5sum | cut -d' ' -f1
}

drop_caches() { sync; echo 3 >/proc/sys/vm/drop_caches 2>/dev/null || true; }

# The whole point of the suite: every region the volume should hold still reads
# back. Called after each delete, so a merge that drops clusters is caught at the
# step that caused it rather than at the end.
check_all_regions()
{
	local dev="$1" label="$2" bad=0 i got

	drop_caches
	for i in $(seq 0 $((NREGIONS - 1))); do
		got="$(read_region "${dev}" "${i}")"
		if [ "${got}" != "${REGION_MD5[$i]}" ]; then
			fail "${label}: region ${i} changed (${got} != ${REGION_MD5[$i]})"
			bad=1
		fi
	done
	[ "${bad}" -eq 0 ] && pass "${label}: all ${NREGIONS} regions intact"
	return "${bad}"
}

del_vol()
{
	rpc rcow_deactive_bdev "$(printf '{"device_name":"%s"}' "$1")" >/dev/null 2>&1
	rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "$1")" >/dev/null 2>&1
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

	if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
		rcow_load_credentials
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get cos_endpoint)" -b "${BUCKET}" \
			-r "$(rcow_cfg_get region)" -p "${RCOW_LVS_NAME}/" 2>&1 | tail -1
		rm -f "${RCOW_WAL_IMG}"
		rm -rf "${RCOW_RUN_DIR}" "${WORKDIR}"
	else
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
[ -r "${RCOW_COS_CFG}" ] || { echo "no COS config" >&2; exit 1; }
command -v nvme >/dev/null || { echo "nvme-cli is required" >&2; exit 1; }
[ -n "$(rcow_target_instances)" ] && { echo "a target is already running" >&2; exit 1; }

BUCKET="$(rcow_cos_buckets | head -1)"
WORKDIR="$(mktemp -d /tmp/rcow_snapdel.XXXXXX)"
rm -rf "${RCOW_RUN_DIR}"; mkdir -p "${RCOW_RUN_DIR}"
rm -f "${RCOW_WAL_IMG}"; truncate -s 1G "${RCOW_WAL_IMG}"

"${SCRIPTS}/rcow_start.sh" >"${WORKDIR}/start.log" 2>&1 && STARTED=1 || {
	fail "rcow_start.sh failed"; tail -20 "${WORKDIR}/start.log"; exit 1; }
pass "data plane up, lvstore ${RCOW_LVS_NAME}"

# ==========================================================================
echo ""
echo "=== [1] a volume and a chain of three snapshots"
#
# One region written before each snapshot, so each snapshot in the chain is the
# only place one of the regions lives. See the header for why that matters.

rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":1}' "${VOL}")" \
	>/dev/null || { fail "create ${VOL}"; exit 1; }
DEV="$(resolve "${VOL}")"
[ -b "${DEV}" ] || { fail "${VOL} did not become a block device"; exit 1; }
info "${VOL} -> ${DEV}"

declare -a REGION_MD5
for i in $(seq 0 $((NREGIONS - 1))); do
	dd if=/dev/urandom of="${WORKDIR}/r${i}" bs=1M count="${REGION_MB}" status=none
	REGION_MD5[$i]="$(md5sum "${WORKDIR}/r${i}" | cut -d' ' -f1)"
done

write_region "${DEV}" 0 "${WORKDIR}/r0" || exit 1
sync
rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"s0"}' "${VOL}")" \
	>/dev/null && pass "s0 taken (region 0 lives only here)" || fail "s0 failed"

write_region "${DEV}" 1 "${WORKDIR}/r1" || exit 1
sync
rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"s1"}' "${VOL}")" \
	>/dev/null && pass "s1 taken (region 1 lives only here)" || fail "s1 failed"

write_region "${DEV}" 2 "${WORKDIR}/r2" || exit 1
sync
rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"s2"}' "${VOL}")" \
	>/dev/null && pass "s2 taken (region 2 lives only here)" || fail "s2 failed"

write_region "${DEV}" 3 "${WORKDIR}/r3" || exit 1
sync
info "chain is now  s0 <- s1 <- s2 <- ${VOL}"

check_all_regions "${DEV}" "before any delete"

# ==========================================================================
echo ""
echo "=== [2] delete the middle snapshot with the volume still alive"
#
# s1 has exactly one clone (s2), so blobstore merges it into s2. This is the delete
# that used to be refused, and refusing it is what forced leaf-to-root order.

if OUT="$(rpc rcow_delete_lvol '{"lvol_name":"s1"}' 2>&1)"; then
	pass "s1 deleted while ${VOL} is alive"
else
	fail "s1 refused: $(echo "${OUT}" | tr -d '\n' | head -c 160)"
	info "if this says 'snapshot with 1 clone', the pre-flight threshold is wrong"
fi

# The merge has to have moved region 1's clusters into s2. Region 0 is behind s0,
# still further up the chain, so it also proves the chain was relinked and not just
# truncated.
check_all_regions "${DEV}" "after deleting s1"

# ==========================================================================
echo ""
echo "=== [3] delete the oldest snapshot, which is now the chain root"

if OUT="$(rpc rcow_delete_lvol '{"lvol_name":"s0"}' 2>&1)"; then
	pass "s0 deleted"
else
	fail "s0 refused: $(echo "${OUT}" | tr -d '\n' | head -c 160)"
fi

check_all_regions "${DEV}" "after deleting s0"

# ==========================================================================
echo ""
echo "=== [4] the surviving snapshot still reads what it captured"
#
# s2 was taken before region 3 was written, so it must show regions 0-2 and *not*
# region 3. After two merges, regions 0 and 1 are only readable through s2 if the
# merges attached their clusters correctly -- so this is the strongest single check
# that the merge preserved history rather than just leaving the volume readable.

SDEV="$(resolve s2)"
if [ -b "${SDEV}" ]; then
	blockdev --setro "${SDEV}" 2>/dev/null || true
	drop_caches
	for i in 0 1 2; do
		got="$(read_region "${SDEV}" "${i}")"
		[ "${got}" = "${REGION_MD5[$i]}" ] \
			&& pass "s2 region ${i} matches what it captured" \
			|| fail "s2 region ${i} wrong (${got} != ${REGION_MD5[$i]})"
	done
	ZERO="$(dd if=/dev/zero bs=1M count="${REGION_MB}" status=none | md5sum | cut -d' ' -f1)"
	got="$(read_region "${SDEV}" 3)"
	[ "${got}" = "${ZERO}" ] \
		&& pass "s2 region 3 is zeroes: it predates that write" \
		|| fail "s2 region 3 is not zeroes, so it captured a later write"
	rpc rcow_deactive_bdev '{"device_name":"s2"}' >/dev/null 2>&1
else
	fail "s2 could not be activated"
fi

# ==========================================================================
echo ""
echo "=== [5] a snapshot with two clones is still refused"
#
# The one case bs_is_blob_deletable() refuses unconditionally, and the reason the
# pre-flight check exists at all: blobstore can merge a snapshot into one clone, not
# into two. Refused before anything is torn down, so the volume is untouched
# afterwards -- which is asserted, not assumed.

rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"sx"}' "${VOL}")" \
	>/dev/null && pass "sx taken" || fail "sx failed"
rpc rcow_create_clone '{"snapshot_name":"sx","clone_name":"cx"}' >/dev/null 2>&1 \
	&& pass "a second clone cx created from sx" \
	|| info "rcow_create_clone unavailable; skipping the two-clone case"

# Unconditional, not guarded by a probe for cx. It used to be wrapped in
# `if rpc rcow_get_bdev '{"device_name":"cx"}'`, which fails for a volume that was
# never activated -- so the whole two-clone case silently skipped and the one
# assertion the pre-flight check exists for was never run.
# The reason is checked in the target log, not in the RPC reply. The reply carries
# only strerror(-EBUSY) -- "Device or resource busy" -- which is what a dozen other
# refusals would also say, so matching on it would accept the wrong cause. The log
# is where the clone count appears.
LOG_BEFORE="$(grep -c 'is a snapshot with' "${RCOW_LOG}" 2>/dev/null || echo 0)"

if OUT="$(rpc rcow_delete_lvol '{"lvol_name":"sx"}' 2>&1)"; then
	fail "sx was deleted despite having two clones (${VOL} and cx)"
	info "blobstore cannot merge into two clones, so this had to be refused"
elif [ "$(grep -c 'is a snapshot with 2 clones' "${RCOW_LOG}" 2>/dev/null || echo 0)" \
		-gt 0 ]; then
	pass "sx refused by the pre-flight check, which names the clone count"
elif [ "$(grep -c 'more than one clone' "${RCOW_LOG}" 2>/dev/null || echo 0)" -gt 0 ]; then
	fail "the refusal came from blobstore, i.e. too late: the bdev is already gone"
	info "the pre-flight check in s3lvol_lvol_destroy should have caught this"
else
	fail "sx refused, but nothing in the log says why: \
$(echo "${OUT}" | tr -d '\n' | head -c 120)"
fi

# The refusal must have happened before any teardown: same data, and the volume is
# still writable. A late refusal would have unregistered the bdev already.
check_all_regions "${DEV}" "after the refused delete"
if write_region "${DEV}" 3 "${WORKDIR}/r3"; then
	sync
	pass "and ${VOL} is still writable, so nothing was torn down"
else
	fail "${VOL} became unwritable after the refused delete"
fi
del_vol cx

# ==========================================================================
echo ""
echo "=== [6] the lvstore still unloads"
#
# The failure mode the pre-flight check was added for: a delete that gets its
# refusal too late leaves an lvol closed with no bdev, and unload then fails with
# "not every lvol could be closed". A clean unload is the proof that no delete on
# this run left a half-torn-down lvol behind.

for n in $(rpc rcow_get_bdev '{}' 2>/dev/null | python3 -c '
import json, sys
try:
    for e in json.load(sys.stdin):
        print(e["device_name"])
except Exception:
    pass'); do
	rpc rcow_deactive_bdev "$(printf '{"device_name":"%s"}' "$n")" >/dev/null 2>&1
done

rpc rcow_checkpoint_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
	>/dev/null 2>&1
if rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
		>/dev/null 2>&1; then
	pass "lvstore unloaded cleanly"
else
	fail "unload failed: a delete left an lvol in a bad state"
	grep -E "not every lvol|has no bdev" "${RCOW_LOG}" | tail -4 | sed 's/^/       /'
fi

if rpc rcow_attach_lvstore \
		"$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${RCOW_LVS_NAME}" "${BUCKET}" "${RCOW_WAL_BDEV}")" >/dev/null 2>&1; then
	pass "and attached again"
	DEV="$(resolve "${VOL}")"
	[ -b "${DEV}" ] && check_all_regions "${DEV}" "after unload and attach" \
		|| fail "${VOL} could not be reactivated"
else
	fail "attach failed"
fi

# ==========================================================================
echo ""
echo "=== [7] target log"

rcow_target_alive && pass "target alive" || fail "target died"

if grep -qE 'Assertion|SIGSEGV|panic:' "${RCOW_LOG}" 2>/dev/null; then
	fail "assertion or fault in the target log"
	grep -nE 'Assertion|SIGSEGV|panic:' "${RCOW_LOG}" | head -5 | sed 's/^/       /'
else
	pass "no asserts, no faults"
fi
