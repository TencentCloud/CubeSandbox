#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Importing an export that came from this same lvstore.
#
#  === Why this degenerates, and why here rather than in export ===
#
#  Exporting a snapshot and importing it back into the same lvstore is a normal way
#  to get a writable copy of that snapshot -- the same API serves that and a
#  cross-node handoff. Built as an esnap clone it is both slower and more fragile
#  than it needs to be:
#
#    - a same-bucket export is REF layout by default, so the manifest references the
#      source's *live* chunk objects rather than copies. Measured: a self-import
#      built that way stops reading after the lvstore is unloaded and attached
#      again, with `<prefix>/data/<uuid>` returning 404 while the exported snapshot
#      is still present. Step [6] pins that a local clone does not have this
#      problem, which is the whole point.
#    - an esnap clone's parent is pinned by the export, which can be released or can
#      pass a TTL. A local clone's parent is pinned by blobstore, which refuses to
#      delete a snapshot that has clones. The local path is the safer one.
#
#  The decision cannot be made when the export is written: whether a manifest will
#  be consumed here or on another node is the caller's business and unknowable at
#  that point. So export_snapshot is untouched and the import decides, where the
#  answer is observable.
#
#  === What is actually asserted ===
#
#  The positive case is easy and not where the risk is. The risk is the guard
#  admitting something it should not, so each of the three conditions is falsified
#  in turn and the result must be the esnap path:
#
#    [3] the snapshot is gone            -> esnap (this is what DENSE exists for)
#    [4] the name resolves to a *different* blob -> esnap
#    [5] the source is writable again    -> esnap
#
#  [4] is the one that matters most and is easiest to leave out. Delete the snapshot
#  and create another with the same name, and a guard that matched on the name alone
#  would clone a volume the caller never asked for, silently and with the right
#  name. It is asserted by content, not just by mode: the clone must read the *new*
#  snapshot's data if it cloned locally, and the *exported* data if it went through
#  the export. Those differ, so the assertion can tell them apart.
#
#  Usage:
#    sudo -E ./test/dataplane/run_selfimport_test.sh
#
#  Needs root, a readable /data/cubelet/cos.cfg, and nvme-cli.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
SCRIPTS="${ROOT}/scripts"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"

export RCOW_LVS_NAME=selfvs
export RCOW_WAL_IMG=/data/s3lvol_selfimport_wal.img
export RCOW_WAL_BDEV=selfimport_wal0
export RCOW_CAPACITY_GB=8
export RCOW_JOURNAL_MB=64
export RCOW_WAL_MB=256
export RCOW_TGT_MEM_MB=2048
export RCOW_RUN_DIR=/var/tmp/rcow_selfimport
export RCOW_LOG_DIR=/var/tmp/rcow_selfimport/log
export RCOW_ACTIVE_FILE=/var/tmp/rcow_selfimport/active_lvols
export RCOW_BSTORE_FILE=/var/tmp/rcow_selfimport/bstore.json
export RCOW_COS_CFG="${RCOW_COS_CFG:-/data/cubelet/cos.cfg}"

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "  ---- $*"; }

STARTED=0
WORKDIR=""
EXPORTS=""

# shellcheck source=../../scripts/rcow_common.sh
. "${SCRIPTS}/rcow_common.sh"

rpc() { python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" "$@"; }

# rcow_export_snapshot answers the uuid before the manifest is published; poll
# rcow_get_snapshot_status until DONE before anything that needs the manifest.
# A refused query means the uuid names nothing, i.e. the export failed.
wait_export_done()
{
	local uuid="$1"
	local deadline=$(( $(date +%s) + 60 ))
	local out st

	while :; do
		if ! out="$(rpc rcow_get_snapshot_status \
				"$(printf '{"export_uuid":"%s"}' "${uuid}")" \
				2>/dev/null)"; then
			fail "export ${uuid} finished without a manifest"
			return 1
		fi
		st="$(printf '%s' "${out}" \
			| python3 -c 'import json,sys; print(json.load(sys.stdin).get("export_status",""))' \
				2>/dev/null)"
		if [ "${st}" = "DONE" ]; then
			return 0
		fi
		if [ "$(date +%s)" -ge "${deadline}" ]; then
			fail "export ${uuid} never reached DONE"
			return 1
		fi
		sleep 0.2
	done
}

# The import reply carries a third field saying which implementation ran. Read from
# the reply rather than guessed from side effects.
import_mode()
{
	python3 - "$1" <<'PY'
import json, sys
try:
    print(json.loads(sys.argv[1]).get("mode", "<absent>"))
except Exception:
    print("<unparseable>")
PY
}

# Activate and echo the host device. Prints nothing on failure -- it runs in a
# command substitution, so anything it did to PASS/FAIL would be lost with the
# subshell; the caller decides what an empty result means.
#
# On failure it dumps what the target thinks the state is, to stderr so it does not
# contaminate the captured device path. Without that, "did not become a device" is
# indistinguishable between "the target never activated it" and "it did, and the
# host has not discovered the namespace yet" -- which cost a run to tell apart.
resolve()
{
	local name="$1" out path

	if ! out="$(rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${name}")" 2>&1)"; then
		echo "  ---- activate ${name} failed: ${out}" >&2
		return 1
	fi

	if ! rcow_verify_active 30 >/dev/null 2>&1; then
		echo "  ---- ${name}: rcow_verify_active timed out; target state:" >&2
		rpc rcow_get_bdev '{}' 2>&1 | head -c 600 | sed 's/^/         /' >&2
		echo "         nvme list:" >&2
		nvme list 2>&1 | tail -n +3 | head -5 | sed 's/^/         /' >&2
		echo "         connected subsystems: \
$(rcow_connected_nqns 2>/dev/null | grep -c "${RCOW_NQN_PREFIX}")" >&2
		return 1
	fi

	path="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["device_path"])' \
		"$(rpc rcow_get_bdev "$(printf '{"device_name":"%s"}' "${name}")")" 2>/dev/null)"
	printf '%s' "${path}"
}

md5_of()
{
	dd if="$1" bs=1M count="${2:-16}" iflag=direct status=none 2>/dev/null | \
		md5sum | cut -d' ' -f1
}

# Every write to a volume goes through here, and it refuses a target that is not
# already a block device.
#
# Not defensive programming for its own sake: an earlier throwaway probe wrote to a
# path that had not appeared yet, and `dd ... oflag=direct` on the regular file it
# had just created failed and left a 0-byte /dev/nvme9n1 behind. That file then
# outlived the probe and made every later run of this suite fail at step [1] -- for
# a real reason, since rcow_verify_active checks [ -b ], but one that looked like a
# discovery timeout and cost two runs to attribute.
write_dev()
{
	local dev="$1" src="$2" count="${3:-16}" seek="${4:-0}"

	if [ -z "${dev}" ] || [ ! -b "${dev}" ]; then
		fail "refusing to write to '${dev}': not a block device"
		return 1
	fi
	dd if="${src}" of="${dev}" bs=1M count="${count}" seek="${seek}" \
		oflag=direct status=none
}

drop_caches() { sync; echo 3 >/proc/sys/vm/drop_caches 2>/dev/null || true; }

# Deactivate, then delete. The order is not optional: deleting a volume that is
# still activated removes its bdev but leaves the entry in the active registry with
# an empty device_path, and rcow_verify_active checks *every* entry -- so one such
# leftover makes every later resolve() in the run time out, thirty seconds at a
# time, with a message about whichever volume was being waited for rather than the
# one that was mishandled. That cost a run to work out.
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
		for n in c_local c_gone c_replaced c_writable vol0; do
			rpc rcow_deactive_bdev "$(printf '{"device_name":"%s"}' "$n")" \
				>/dev/null 2>&1
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
		EP="$(rcow_cfg_get cos_endpoint)"; RG="$(rcow_cfg_get region)"
		python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" \
			-p "${RCOW_LVS_NAME}/" 2>&1 | tail -1
		# exports/ is bucket-level and shared, so only this run's manifests.
		for u in ${EXPORTS}; do
			python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" \
				-p "exports/${u}" 2>&1 | tail -1
		done
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
WORKDIR="$(mktemp -d /tmp/rcow_selfimp.XXXXXX)"
rm -rf "${RCOW_RUN_DIR}"; mkdir -p "${RCOW_RUN_DIR}"
rm -f "${RCOW_WAL_IMG}"; truncate -s 1G "${RCOW_WAL_IMG}"

"${SCRIPTS}/rcow_start.sh" >"${WORKDIR}/start.log" 2>&1 && STARTED=1 || {
	fail "rcow_start.sh failed"; tail -20 "${WORKDIR}/start.log"; exit 1; }
pass "data plane up, lvstore ${RCOW_LVS_NAME} in ${BUCKET}"

# ==========================================================================
echo ""
echo "=== [1] a volume with known content, and a snapshot of it"

rpc rcow_create_lvol '{"lvol_name":"vol0","size_gib":1}' >/dev/null || {
	fail "create vol0"; exit 1; }
A="$(resolve vol0)"
[ -b "${A}" ] || { fail "vol0 did not become a device"; exit 1; }

dd if=/dev/urandom of="${WORKDIR}/pattern" bs=1M count=16 status=none
write_dev "${A}" "${WORKDIR}/pattern" 16 || exit 1
sync
ORIG_MD5="$(md5sum "${WORKDIR}/pattern" | cut -d' ' -f1)"
drop_caches
[ "$(md5_of "${A}")" = "${ORIG_MD5}" ] && pass "16 MiB written and verified" \
	|| fail "source readback mismatch"

rpc rcow_create_snapshot '{"lvol_name":"vol0","snapshot_name":"snap0"}' >/dev/null \
	&& pass "snap0 created" || { fail "snapshot failed"; exit 1; }

# Flushed so the export's referenced objects are actually in S3, which is the state
# a cross-node consumer would need. Not required by the local path -- and that
# asymmetry is part of why the local path is preferable.
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
	>/dev/null 2>&1 && pass "lvstore flushed to S3" || info "flush failed"

U1="$(rpc rcow_export_snapshot '{"snapshot_name":"snap0"}' | tr -d '"')"
EXPORTS="${EXPORTS} ${U1}"
[ -n "${U1}" ] && pass "exported snap0 as ${U1}" || { fail "export failed"; exit 1; }
wait_export_done "${U1}" || exit 1

# ==========================================================================
echo ""
echo "=== [2] importing it back degenerates into a local clone"

OUT="$(rpc rcow_import_lvol \
	"$(printf '{"lvol_name":"c_local","export_uuid":"%s"}' "${U1}")" 2>&1)" || {
	fail "import failed: ${OUT}"; exit 1; }

MODE="$(import_mode "${OUT}")"
if [ "${MODE}" = "local_clone" ]; then
	pass "mode=local_clone"
else
	fail "mode='${MODE}', expected local_clone"
	info "reply: ${OUT}"
fi

if grep -q "cloning it locally instead" "${RCOW_LOG}" 2>/dev/null; then
	pass "and the log says why"
else
	fail "no 'cloning it locally' line in the target log"
fi

B="$(resolve c_local)"
drop_caches
[ "$(md5_of "${B}")" = "${ORIG_MD5}" ] \
	&& pass "the clone reads exactly what the snapshot held" \
	|| fail "clone content mismatch"

# No registry entry is the observable difference, and what release_export keys off.
if rpc rcow_get_imports "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" 2>&1 | \
		grep -q "${U1}"; then
	fail "the local clone was recorded in the imports registry"
else
	pass "nothing was written to the imports registry"
fi

if rpc rcow_release_export \
		"$(printf '{"lvs_name":"%s","export_uuid":"%s"}' \
		"${RCOW_LVS_NAME}" "${U1}")" >/dev/null 2>&1; then
	pass "release_export is not held up by it, as a local clone implies"
else
	fail "release_export was refused, so something still claims a dependency"
fi

# The parent is pinned by the clone relationship instead -- a stronger guarantee
# than the export gave, since an export can be released or expire.
#
# Precisely: snap0 now has *two* clones, vol0 and c_local, and two is the one case
# blobstore refuses outright (bs_is_blob_deletable, blobstore.c:8714) because it can
# merge a snapshot into a single clone but not into two. Worth stating, because the
# earlier wording here said only "blobstore pins it" and was read as meaning any
# snapshot with a clone is undeletable -- which is not true and briefly became a bug
# when the pre-flight check in s3lvol_lvol_destroy refused at one clone as well.
# run_snapdelete_test.sh covers the one-clone case that must succeed.
if rpc rcow_delete_lvol '{"lvol_name":"snap0"}' >/dev/null 2>&1; then
	fail "snap0 was deleted while both vol0 and c_local clone it"
else
	pass "and snap0 cannot be deleted while it has two clones"
fi

# ==========================================================================
echo ""
echo "=== [3] first negative: the source snapshot is gone -> esnap"
#
# The case the DENSE layout exists for. The export's own data is all that is left,
# so the import has to read through it.

rpc rcow_create_lvol '{"lvol_name":"tmp1","size_gib":1}' >/dev/null 2>&1
T="$(resolve tmp1)"
[ -b "${T}" ] || { fail "tmp1 did not become a device"; exit 1; }
write_dev "${T}" "${WORKDIR}/pattern" 16 || exit 1
sync
rpc rcow_create_snapshot '{"lvol_name":"tmp1","snapshot_name":"snap_gone"}' >/dev/null
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" >/dev/null 2>&1

# Getting to "the snapshot is gone" takes a detour, and the reason is worth stating:
# a live REF export *pins* its source snapshot, so the delete is refused outright
# ("is the snapshot behind export ..., which another node may be reading through").
# The layout cannot be chosen over RPC either -- the decoders take snapshot_name,
# export_id and ttl_sec, and same-bucket always means REF.
#
# What does work is the TTL. s3lvol_export_pinning() reports an expired export as
# not pinning, which is exactly so that this delete can proceed, and import does not
# reject an expired manifest. So: export with a two-second TTL, let it lapse, then
# delete. The manifest is still in the bucket because nothing released it.
U2="$(rpc rcow_export_snapshot '{"snapshot_name":"snap_gone","ttl_sec":2}' | tr -d '"')"
EXPORTS="${EXPORTS} ${U2}"
[ -n "${U2}" ] && pass "exported snap_gone as ${U2} with a 2s TTL" \
	|| { fail "export of snap_gone failed"; exit 1; }
wait_export_done "${U2}" || exit 1

sleep 4

# tmp1 clones snap_gone, so it has to go first -- and deactivated, not just deleted.
del_vol tmp1
if rpc rcow_delete_lvol '{"lvol_name":"snap_gone"}' >/dev/null 2>&1; then
	pass "with the export expired, the source snapshot could be deleted"
else
	fail "could not delete snap_gone even after its export expired"
	grep "snap_gone" "${RCOW_LOG}" | tail -2 | sed 's/^/       /'
fi

OUT="$(rpc rcow_import_lvol \
	"$(printf '{"lvol_name":"c_gone","export_uuid":"%s"}' "${U2}")" 2>&1)"
MODE="$(import_mode "${OUT}")"
if [ "${MODE}" = "esnap" ]; then
	pass "mode=esnap: with no local snapshot it reads through the export"
else
	fail "mode='${MODE}', expected esnap -- it cloned something it should not have"
	info "reply: ${OUT}"
fi

# ==========================================================================
echo ""
echo "=== [4] second negative: same name, different blob -> esnap"
#
# The condition most easily left out and worst to get wrong. A guard matching on the
# name alone would clone the replacement, and it would look right: correct name,
# plausible size, no error. Asserted by content as well as mode, because the two
# snapshots hold different data and that is what distinguishes the paths.

rpc rcow_create_lvol '{"lvol_name":"tmp2","size_gib":1}' >/dev/null 2>&1
T="$(resolve tmp2)"
[ -b "${T}" ] || { fail "tmp2 did not become a device"; exit 1; }
write_dev "${T}" "${WORKDIR}/pattern" 16 || exit 1
sync
rpc rcow_create_snapshot '{"lvol_name":"tmp2","snapshot_name":"snap_dup"}' >/dev/null
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" >/dev/null 2>&1
# Two-second TTL for the same reason as step [3]: the export would otherwise pin
# snap_dup and the replacement below could not happen.
U3="$(rpc rcow_export_snapshot '{"snapshot_name":"snap_dup","ttl_sec":2}' | tr -d '"')"
wait_export_done "${U3}" || exit 1
sleep 4
EXPORTS="${EXPORTS} ${U3}"
pass "exported snap_dup as ${U3}"

# Replace it: same name, different blob, different content.
del_vol tmp2
rpc rcow_delete_lvol '{"lvol_name":"snap_dup"}' >/dev/null 2>&1
rpc rcow_create_lvol '{"lvol_name":"tmp3","size_gib":1}' >/dev/null 2>&1
T="$(resolve tmp3)"
[ -b "${T}" ] || { fail "tmp3 did not become a device"; exit 1; }
dd if=/dev/urandom of="${WORKDIR}/other" bs=1M count=16 status=none
OTHER_MD5="$(md5sum "${WORKDIR}/other" | cut -d' ' -f1)"
write_dev "${T}" "${WORKDIR}/other" 16 || exit 1
sync
rpc rcow_create_snapshot '{"lvol_name":"tmp3","snapshot_name":"snap_dup"}' >/dev/null \
	&& pass "recreated a *different* snapshot under the same name snap_dup" \
	|| fail "could not recreate snap_dup"

rpc rcow_deactive_bdev '{"device_name":"tmp3"}' >/dev/null 2>&1

OUT="$(rpc rcow_import_lvol \
	"$(printf '{"lvol_name":"c_replaced","export_uuid":"%s"}' "${U3}")" 2>&1)"
MODE="$(import_mode "${OUT}")"
if [ "${MODE}" = "esnap" ]; then
	pass "mode=esnap: the blob id did not match, so the name was not trusted"
else
	fail "mode='${MODE}': it cloned the replacement, which is not what was exported"
	info "reply: ${OUT}"
fi

if grep -q "it was replaced, so reading through the export" "${RCOW_LOG}" 2>/dev/null; then
	pass "and the log names the blob id mismatch"
else
	fail "no blob id mismatch message in the log"
fi

# What the content can and cannot show here. Getting "same name, different blob"
# requires deleting the original snapshot, and that frees the very chunk objects a
# REF export references -- confirmed in the log as
# "Failed to read export chunk 'selfvs/data/<uuid>': No such file or directory".
# So reading the exported data back is impossible in this setup, and demanding it
# would be asserting something the construction rules out.
#
# What matters is still checkable, and it is the part that would hurt: the clone must
# not be serving the replacement's data. Reading nothing is the correct outcome for
# an export whose referenced objects are gone; reading the replacement would mean
# the guard cloned a volume nobody asked for.
C="$(resolve c_replaced)"
if [ -b "${C}" ]; then
	drop_caches
	GOT="$(md5_of "${C}")"
	EMPTY_MD5="$(printf '' | md5sum | cut -d' ' -f1)"
	if [ "${GOT}" = "${OTHER_MD5}" ]; then
		fail "it serves the REPLACEMENT's data: the wrong volume was cloned"
	elif [ "${GOT}" = "${ORIG_MD5}" ]; then
		pass "and it serves the exported data"
	elif [ "${GOT}" = "${EMPTY_MD5}" ]; then
		pass "and it serves neither: the export's objects went with the deleted"
		pass "snapshot, so reads fail, which is correct for a dangling REF export"
	else
		fail "unexpected content ${GOT}: neither export, replacement, nor empty"
	fi
fi

# ==========================================================================
echo ""
echo "=== [5] third negative: the source is writable again -> esnap"
#
# A read-only parent is not optional: s3lvol_lvol_create_clone refuses a writable
# one, because parent and clone would then share clusters both could modify. The
# export's source name can come to point at a writable volume, and this has to take
# the esnap path rather than fail.

rpc rcow_create_lvol '{"lvol_name":"tmp4","size_gib":1}' >/dev/null 2>&1
T="$(resolve tmp4)"
[ -b "${T}" ] || { fail "tmp4 did not become a device"; exit 1; }
write_dev "${T}" "${WORKDIR}/pattern" 16 || exit 1
sync
rpc rcow_create_snapshot '{"lvol_name":"tmp4","snapshot_name":"snap_rw"}' >/dev/null
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" >/dev/null 2>&1
U4="$(rpc rcow_export_snapshot '{"snapshot_name":"snap_rw","ttl_sec":2}' | tr -d '"')"
wait_export_done "${U4}" || exit 1
sleep 4
EXPORTS="${EXPORTS} ${U4}"

# Replace the snapshot with a *writable* lvol of the same name.
del_vol tmp4
rpc rcow_delete_lvol '{"lvol_name":"snap_rw"}' >/dev/null 2>&1
if rpc rcow_create_lvol '{"lvol_name":"snap_rw","size_gib":1}' >/dev/null 2>&1; then
	pass "created a writable lvol named snap_rw"
else
	fail "could not create a writable snap_rw"
fi

OUT="$(rpc rcow_import_lvol \
	"$(printf '{"lvol_name":"c_writable","export_uuid":"%s"}' "${U4}")" 2>&1)"
MODE="$(import_mode "${OUT}")"
if [ "${MODE}" = "esnap" ]; then
	pass "mode=esnap: a writable source is not clonable, so it read through"
else
	fail "mode='${MODE}': it cloned a writable parent, which shares mutable clusters"
	info "reply: ${OUT}"
fi

# ==========================================================================
echo ""
echo "=== [6] the local clone survives a restart; this is the payoff"
#
# The reason the degeneration is worth having. A REF self-import broke here --
# reads returned EIO because <prefix>/data/<uuid> had 404'd while the snapshot was
# still present. A local clone depends on nothing in the export.

# The esnap clones from steps [3] to [5] are deliberately dangling -- their exports
# were expired and their source snapshots deleted, so their backing objects are gone.
# They are removed here rather than carried into the restart, for two reasons: this
# step is about the *local* clone surviving, and an lvstore holding dangling esnap
# clones fails to unload with "not every lvol could be closed (-22)". That may well
# be worth looking at on its own, but it is a different question from this one and
# the state was manufactured here; smuggling it in would make this step fail for a
# reason it is not testing.
for n in c_gone c_replaced c_writable tmp3; do
	del_vol "${n}"
done
# The snapshots these clones referenced are still pinned by the *lease* their
# dangling imports renewed -- deleting the clones stopped the renewer, but the
# source keeps the export pinned for a grace period (3x the renew interval)
# before it considers the lease stale. That grace is the point of the liveness
# lease: the source must not delete a snapshot while an importer *might* still
# be reading it. Wait it out, then the delete goes through -- the same sequence
# the control plane follows: stop the reader, let the lease lapse, then remove
# the source.
sleep 8
for n in snap_rw snap_dup; do
	del_vol "${n}"
done

# Everything still activated, read from the registry rather than a list written by
# hand. An lvol
# that is still activated cannot be closed, and unload then fails with
# "not every lvol could be closed (-22)" naming none of them -- which is what
# happened when this was a fixed list and the run had created tmp3 and snap_rw
# along the way.
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
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
	>/dev/null 2>&1 && pass "lvstore unloaded" || fail "unload failed"

rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
	"${RCOW_LVS_NAME}" "${BUCKET}" "${RCOW_WAL_BDEV}")" >/dev/null 2>&1 \
	&& pass "and attached again" || { fail "attach failed"; exit 1; }

B="$(resolve c_local)"
if [ -b "${B}" ]; then
	drop_caches
	if [ "$(md5_of "${B}")" = "${ORIG_MD5}" ]; then
		pass "the local clone still reads correctly after the restart"
	else
		fail "the local clone broke across the restart"
	fi
else
	fail "c_local could not be reactivated"
fi

# Writable, and its writes must not reach the snapshot it clones.
if [ -b "${B}" ]; then
	write_dev "${B}" /dev/zero 2
	sync
	V="$(resolve vol0)"
	drop_caches
	[ "$(md5_of "${V}")" = "${ORIG_MD5}" ] \
		&& pass "writing the clone left the origin untouched" \
		|| fail "the clone's write reached the origin"
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

# A local clone must not have gone anywhere near the export chunk reader.
if grep -q "Failed to read export chunk" "${RCOW_LOG}" 2>/dev/null; then
	info "export chunk read failures present (expected for the esnap cases):"
	grep -c "Failed to read export chunk" "${RCOW_LOG}" | sed 's/^/       count=/'
else
	pass "no export chunk read failures"
fi
