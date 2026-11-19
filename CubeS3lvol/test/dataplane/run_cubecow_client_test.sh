#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  The cubecow / Cubelet client contract, as one dataplane script.
#
#  === Why a new suite rather than another section in export / agent_template ===
#
#  Those two already have a thesis. export is "zero-copy across prefixes";
#  agent_template is "two real s3lvol_tgt processes sharing only the bucket".
#  Cubelet never calls either of those shapes directly. It talks through cubecow
#  (`backend.kind=s3`) and always uses the same 11 rcow_* methods in a fixed
#  order: create/clone → active → get_bdev → (ext4) → umount → snapshot
#  (inactive) → deactive → delete work volume; Pause exports three snapshots
#  from one lvstore; restore imports with decouple=true and activates before
#  any decouple waiter. Stuffing that sequence into export would make a red
#  run look like a manifest bug; stuffing it into agent_template would make it
#  look like a two-process state-file bug.
#
#  What this suite pins, and the existing ones do not:
#
#    [1]  __cbc_probe_* deactive is a no-op: success or a not-found-class
#         error, no registry entry, and two RPCs on one long-lived
#         connection stay in order
#    [2]  create → active → get_bdev returns a path that is a real block
#         device; mkfs.ext4 + umount + snapshot (snap is never activated) +
#         delete the work volume, which is Cubelet's metadata/rootfs seal
#    [3]  deactive then get_bdev is not-found; auto-reactivate skips the
#         just-freed nsid and still yields a path that open(2)s (the
#         host node number may stay nvmeXn1 even when the nsid changed)
#    [4]  N clones from one template snap, isolated writes, then resize one
#         clone (sandbox rootfs grow after CreateVolumeFromSnapshot)
#    [5]  a live ext4 clone is fsfreeze'd and snapshotted. The snapshot call
#         publishes that lvol's chunks and the blobstore metadata, leaving
#         exactly another lvol's dirty overlay behind. Export flushes the
#         snapshot again (inline when already clean) and
#         the restored snapshot must contain every write acknowledged before
#         the freeze (fs-only snapshot contract)
#    [6]  three snapshots exported at once (Pause/Commit: rootfs + memory +
#         metadata); busy is retried; progress is polled by snapshot_name,
#         which is what GetVolumeInfo does
#    [7]  deleting the template snap while clones exist is refused, and the
#         message must not contain "not found" (cubecow would treat that as
#         an idempotent success)
#    [8]  a failed import of a garbage uuid must not leave a named lvol
#    [9]  import(decouple=true) on a second lvstore → activate + ext4 mount
#         immediately, without waiting for decouple to finish
#
#  Usage:
#    sudo -E ./test/dataplane/run_cubecow_client_test.sh
#
#  Needs root, nvme-cli, e2fsprogs (mkfs.ext4), a readable /data/cubelet/s3.cfg.
#  Own lvstore / WAL / registries, so a production instance is untouched.
#  Set S3LVOL_FS_ONLY_ONLY=1 to stop after the focused fs-only regression.
#

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
SCRIPTS="${ROOT}/scripts"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"

export RCOW_LVS_NAME=cbcvs
export RCOW_WAL_IMG=/data/s3lvol_cbc_wal.img
export RCOW_WAL_BDEV=cbc_wal0
export RCOW_CAPACITY_GB=16
export RCOW_JOURNAL_MB=64
export RCOW_WAL_MB=256
export RCOW_TGT_MEM_MB=2048
export RCOW_RUN_DIR=/var/tmp/rcow_cbcclient
export RCOW_LOG_DIR=/var/tmp/rcow_cbcclient/log
export RCOW_ACTIVE_FILE=/var/tmp/rcow_cbcclient/active_lvols
export RCOW_BSTORE_FILE=/var/tmp/rcow_cbcclient/bstore.json
export RCOW_S3_CFG="${RCOW_S3_CFG:-/data/cubelet/s3.cfg}"

DST_LVS=cbcimp
DST_WAL_IMG=/data/s3lvol_cbc_dst.img
DST_WAL_BDEV=cbc_dst_wal0

N_CLONES=4

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "  ---- $*"; }

STARTED=0
WORKDIR=""
EXPORTS=""
DST_CREATED=0
MNT=""
CLONE1_DEV=""
FROZEN_MNT=0

# shellcheck source=../../scripts/rcow_common.sh
. "${SCRIPTS}/rcow_common.sh"

rpc() { python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" "$@"; }

err_is_not_found()
{
	printf '%s' "$1" | grep -qiE 'not found|no such'
}

err_is_busy()
{
	printf '%s' "$1" | grep -qiE 'busy|in use|resource'
}

err_is_already_exists()
{
	printf '%s' "$1" | grep -qiE 'already exists|already exist'
}

jget() { python3 -c 'import json,sys; print(json.loads(sys.argv[1])[sys.argv[2]])' "$1" "$2"; }

# Wait until get_bdev names a node that is a live block device. After deactive
# then auto-reactivate the host often reuses /dev/nvmeXn1 for a new nsid, and
# udev's REMOVE of the previous occupant can unlink that name after sysfs
# already shows the new uuid.
wait_bdev()
{
	local name="$1" deadline json path=""

	deadline=$((SECONDS + 30))
	while :; do
		json="$(rpc rcow_get_bdev "$(printf '{"device_name":"%s"}' "${name}")" \
			2>/dev/null)" || json=""
		path=""
		if [ -n "${json}" ]; then
			path="$(jget "${json}" device_path 2>/dev/null)" || path=""
		fi
		if [ -n "${path}" ] && [ -b "${path}" ]; then
			printf '%s' "${path}"
			return 0
		fi
		[ "${SECONDS}" -ge "${deadline}" ] && break
		sleep 0.1
	done
	echo "  ---- ${name}: device_path '${path}' is not a block device" >&2
	return 1
}

# Activate and echo the host device. Prints nothing on failure -- it runs in a
# command substitution, so the caller decides what an empty result means.
resolve()
{
	local name="$1" out

	if ! out="$(rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${name}")" 2>&1)"; then
		echo "  ---- activate ${name} failed: ${out}" >&2
		return 1
	fi
	if ! rcow_verify_active 30 >/dev/null 2>&1; then
		echo "  ---- ${name}: rcow_verify_active timed out" >&2
		return 1
	fi
	wait_bdev "${name}"
}

del_vol()
{
	rpc rcow_deactive_bdev "$(printf '{"device_name":"%s"}' "$1")" >/dev/null 2>&1
	rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "$1")" >/dev/null 2>&1
}

unmount_quietly()
{
	local mnt="$1"
	mountpoint -q "${mnt}" 2>/dev/null || return 0
	umount "${mnt}" 2>/dev/null && return 0
	sync
	sleep 1
	umount "${mnt}" 2>/dev/null && return 0
	umount -l "${mnt}" 2>/dev/null
}

wait_export_done_by_name()
{
	local name="$1"
	local deadline=$(( $(date +%s) + 90 ))
	local out st

	while :; do
		if ! out="$(rpc rcow_get_snapshot_status \
				"$(printf '{"snapshot_name":"%s"}' "${name}")" \
				2>/dev/null)"; then
			fail "status of ${name} became unqueryable"
			return 1
		fi
		st="$(printf '%s' "${out}" \
			| python3 -c 'import json,sys; print(json.load(sys.stdin).get("export_status",""))' \
				2>/dev/null)"
		if [ "${st}" = "DONE" ]; then
			return 0
		fi
		if [ "$(date +%s)" -ge "${deadline}" ]; then
			fail "${name} never reached DONE (last status='${st}')"
			return 1
		fi
		sleep 0.2
	done
}

# Cubelet issues three exports against one lvstore. Export no longer drains,
# so they do not contend for the flusher; a busy reply can still come from a
# snapshot that is mid-publish. The client retries until it has a uuid.
export_with_retry()
{
	local name="$1"
	local tries=0
	local out err

	while [ "${tries}" -lt 30 ]; do
		err=""
		if out="$(rpc rcow_export_snapshot \
				"$(printf '{"snapshot_name":"%s"}' "${name}")" 2>"${WORKDIR}/exp.${name}.err")"; then
			out="$(printf '%s' "${out}" | tr -d '"[:space:]')"
			if [ -n "${out}" ]; then
				printf '%s' "${out}"
				return 0
			fi
		fi
		err="$(cat "${WORKDIR}/exp.${name}.err" 2>/dev/null || true)"
		if err_is_busy "${err}"; then
			tries=$((tries + 1))
			sleep 1
			continue
		fi
		echo "${err}" >&2
		return 1
	done
	echo "export ${name} stayed busy" >&2
	return 1
}

lvs_stat()
{
	local field="$1"
	rpc rcow_get_lvstores \
		| python3 -c "import sys,json; stores=json.load(sys.stdin); wp=next((s.get('write_path',{}) for s in stores if s.get('lvs_name')=='${RCOW_LVS_NAME}'),{}); print(wp.get('${field}','missing'))" \
		2>/dev/null || echo "missing"
}

cleanup()
{
	echo ""
	echo "=== cleanup"

	if [ "${FROZEN_MNT}" -eq 1 ] && [ -n "${MNT}" ]; then
		fsfreeze -u "${MNT}" >/dev/null 2>&1 || true
		FROZEN_MNT=0
	fi
	[ -n "${MNT}" ] && unmount_quietly "${MNT}"

	if rcow_target_alive; then
		for n in tpl-work mem-work meta-work \
			 sb-0 sb-1 sb-2 sb-3 sb-live sb-live-restored \
			 sb-live-imported \
			 live-rootfs \
			 tpl-rootfs mem-snap meta-snap \
			 ghost imp-rootfs; do
			rpc rcow_deactive_bdev "$(printf '{"device_name":"%s"}' "${n}")" \
				>/dev/null 2>&1
		done
		if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
			if [ "${DST_CREATED}" -eq 1 ]; then
				rpc rcow_delete_lvstore \
					"$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
					>/dev/null 2>&1 || info "delete dst lvstore failed"
			fi
			rpc rcow_delete_lvstore \
				"$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
				>/dev/null 2>&1 || info "delete src lvstore failed"
		fi
	fi
	[ "${STARTED}" -eq 1 ] && "${SCRIPTS}/rcow_stop.sh" --force >/dev/null 2>&1

	if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
		rcow_load_credentials
		EP="$(rcow_cfg_get endpoint)"; RG="$(rcow_cfg_get region)"
		python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" \
			-p "${RCOW_LVS_NAME}/" 2>&1 | tail -1
		python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" \
			-p "${DST_LVS}/" 2>&1 | tail -1
		for u in ${EXPORTS}; do
			python3 "${PREFIX_RM}" -e "${EP}" -b "${BUCKET}" -r "${RG}" \
				-p "exports/${u}" 2>&1 | tail -1
		done
		rm -f "${RCOW_WAL_IMG}" "${DST_WAL_IMG}"
		rm -rf "${RCOW_RUN_DIR}" "${WORKDIR}"
	else
		info "state kept: ${RCOW_WAL_IMG}, ${DST_WAL_IMG}, ${RCOW_RUN_DIR}, ${WORKDIR}"
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
command -v mkfs.ext4 >/dev/null || { echo "mkfs.ext4 is required" >&2; exit 1; }
command -v fsfreeze >/dev/null || { echo "fsfreeze is required" >&2; exit 1; }
[ -n "$(rcow_target_instances)" ] && { echo "a target is already running" >&2; exit 1; }

if [ -z "${S3LVOL_SKIP_FRESH_CHECK:-}" ]; then
	"${ROOT}/test/tools/check_binary_fresh.sh" "${RCOW_TGT_BIN}" || exit 1
fi

BUCKET="$(rcow_s3_buckets | head -1)"
WORKDIR="$(mktemp -d /tmp/rcow_cbc.XXXXXX)"
MNT="${WORKDIR}/mnt"
mkdir -p "${MNT}"
rm -rf "${RCOW_RUN_DIR}"; mkdir -p "${RCOW_RUN_DIR}"
rm -f "${RCOW_WAL_IMG}" "${DST_WAL_IMG}"
truncate -s 1G "${RCOW_WAL_IMG}"

"${SCRIPTS}/rcow_start.sh" >"${WORKDIR}/start.log" 2>&1 && STARTED=1 || {
	fail "rcow_start.sh failed"; tail -20 "${WORKDIR}/start.log"; exit 1; }
pass "data plane up, lvstore ${RCOW_LVS_NAME} in ${BUCKET}"

# ==========================================================================
echo ""
echo "=== [1] cubecow liveness probe and a long-lived connection"

PROBE_OUT="$(rpc rcow_deactive_bdev \
	'{"device_name":"__cbc_probe_cbcclient"}' 2>&1)"
PROBE_RC=$?
if [ "${PROBE_RC}" -eq 0 ]; then
	pass "probe __cbc_probe_* is a successful no-op"
elif err_is_not_found "${PROBE_OUT}"; then
	pass "probe __cbc_probe_* is not-found-class"
else
	fail "probe deactive was a hard error: ${PROBE_OUT}"
	exit 1
fi
if rpc rcow_get_bdev '{"device_name":"__cbc_probe_cbcclient"}' >/dev/null 2>&1; then
	fail "probe name became an active bdev"
	exit 1
fi
pass "probe left no host device"

python3 - "${RCOW_RPC_SOCK}" <<'PY' >"${WORKDIR}/keepalive.out" 2>"${WORKDIR}/keepalive.err"
import json, socket, sys

sock_path = sys.argv[1]
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(10)
s.connect(sock_path)

def call(i):
    req = json.dumps({
        "jsonrpc": "2.0",
        "method": "rcow_deactive_bdev",
        "params": {"device_name": "__cbc_probe_keep"},
        "id": i,
    }) + "\n"
    s.sendall(req.encode())
    buf = b""
    while b"\n" not in buf:
        chunk = s.recv(4096)
        if not chunk:
            raise RuntimeError("server closed the connection mid-frame")
        buf += chunk
    return json.loads(buf.split(b"\n", 1)[0])

r1 = call(1)
r2 = call(2)
if r1.get("id") != 1 or r2.get("id") != 2:
    raise SystemExit("id mismatch: %r then %r" % (r1.get("id"), r2.get("id")))
print("ok")
PY
if [ "$(cat "${WORKDIR}/keepalive.out" 2>/dev/null)" = "ok" ]; then
	pass "two RPCs on one connection came back in send order"
else
	fail "long-lived connection: $(cat "${WORKDIR}/keepalive.err" 2>/dev/null)"
	exit 1
fi

# ==========================================================================
echo ""
echo "=== [2] seal path: ext4 volume → umount → inactive snapshot → delete work vol"

rpc rcow_create_lvol '{"lvol_name":"tpl-work","size_gib":1}' >/dev/null || {
	fail "create tpl-work"; exit 1; }
TPL_DEV="$(resolve tpl-work)"
[ -b "${TPL_DEV}" ] || { fail "tpl-work did not become a device"; exit 1; }
pass "tpl-work is ${TPL_DEV}"

mkfs.ext4 -F -b 4096 "${TPL_DEV}" >/dev/null 2>&1 || {
	fail "mkfs.ext4 tpl-work"; exit 1; }
mount "${TPL_DEV}" "${MNT}" || { fail "mount tpl-work"; exit 1; }
echo "tpl-v1" >"${MNT}/marker.txt"
mkdir -p "${MNT}/disk/tpl-test_0/upper/root/work" \
	"${MNT}/disk/tpl-test_0/work"
sync
unmount_quietly "${MNT}"

rpc rcow_create_snapshot \
	'{"lvol_name":"tpl-work","snapshot_name":"tpl-rootfs"}' >/dev/null || {
	fail "snapshot tpl-rootfs"; exit 1; }
pass "tpl-rootfs snap created (not activated)"

# Cubelet never activates the sealed package snap for IO.
if rpc rcow_get_bdev '{"device_name":"tpl-rootfs"}' >/dev/null 2>&1; then
	fail "inactive snap tpl-rootfs was already a bdev"
	exit 1
fi
pass "sealed snap has no host device"

rpc rcow_deactive_bdev '{"device_name":"tpl-work"}' >/dev/null 2>&1
rpc rcow_delete_lvol '{"lvol_name":"tpl-work"}' >/dev/null || {
	fail "delete work volume after seal"; exit 1; }
pass "work volume deleted; snap remains"

# ==========================================================================
echo ""
echo "=== [3] deactive → get_bdev not-found → reactivate still openable"

rpc rcow_create_lvol '{"lvol_name":"mem-work","size_gib":1}' >/dev/null || {
	fail "create mem-work"; exit 1; }
MEM_DEV="$(resolve mem-work)"
[ -b "${MEM_DEV}" ] || { fail "mem-work did not become a device"; exit 1; }
MEM_JSON="$(rpc rcow_get_bdev '{"device_name":"mem-work"}')" || {
	fail "get_bdev mem-work"; exit 1; }
MEM_SUB="$(jget "${MEM_JSON}" subsys)"
MEM_NSID="$(jget "${MEM_JSON}" nsid)"
dd if=/dev/urandom of="${WORKDIR}/mem.pat" bs=1M count=8 status=none
dd if="${WORKDIR}/mem.pat" of="${MEM_DEV}" bs=1M count=8 oflag=direct status=none
MEM_MD5="$(md5sum "${WORKDIR}/mem.pat" | cut -d' ' -f1)"

rpc rcow_deactive_bdev '{"device_name":"mem-work"}' >/dev/null || {
	fail "deactive mem-work"; exit 1; }
GET_ERR="$(rpc rcow_get_bdev '{"device_name":"mem-work"}' 2>&1)" && {
	fail "get_bdev after deactive succeeded"; exit 1; }
if err_is_not_found "${GET_ERR}"; then
	pass "get_bdev after deactive is not-found-class"
else
	fail "get_bdev after deactive: ${GET_ERR}"
	exit 1
fi

MEM_DEV2="$(resolve mem-work)"
[ -b "${MEM_DEV2}" ] || { fail "reactivate did not yield a block device"; exit 1; }
MEM_JSON2="$(rpc rcow_get_bdev '{"device_name":"mem-work"}')" || {
	fail "get_bdev mem-work after reactivate"; exit 1; }
MEM_SUB2="$(jget "${MEM_JSON2}" subsys)"
MEM_NSID2="$(jget "${MEM_JSON2}" nsid)"
info "mem-work re-activate -> subsys ${MEM_SUB2} nsid ${MEM_NSID2} (was ${MEM_SUB}/${MEM_NSID})"
[ "${MEM_SUB2}" = "${MEM_SUB}" ] &&
	pass "re-activate stayed on hashed subsys ${MEM_SUB}" ||
	fail "re-activate hashed to subsys ${MEM_SUB2}, wanted ${MEM_SUB}"
[ "${MEM_NSID2}" != "${MEM_NSID}" ] &&
	pass "auto-reactivate skipped the just-freed nsid ${MEM_NSID} (got ${MEM_NSID2})" ||
	fail "auto-reactivate reused nsid ${MEM_NSID}"
GOT="$(dd if="${MEM_DEV2}" bs=1M count=8 iflag=direct status=none 2>/dev/null | md5sum | cut -d' ' -f1)"
if [ "${GOT}" = "${MEM_MD5}" ]; then
	pass "reactivate path ${MEM_DEV2} (was ${MEM_DEV}) still holds the data"
else
	fail "reactivate readback mismatch"
	exit 1
fi

rpc rcow_create_snapshot \
	'{"lvol_name":"mem-work","snapshot_name":"mem-snap"}' >/dev/null || {
	fail "snapshot mem-snap"; exit 1; }
rpc rcow_deactive_bdev '{"device_name":"mem-work"}' >/dev/null 2>&1
rpc rcow_delete_lvol '{"lvol_name":"mem-work"}' >/dev/null || {
	fail "delete mem-work"; exit 1; }

rpc rcow_create_lvol '{"lvol_name":"meta-work","size_gib":1}' >/dev/null || {
	fail "create meta-work"; exit 1; }
META_DEV="$(resolve meta-work)"
mkfs.ext4 -F -b 4096 "${META_DEV}" >/dev/null 2>&1 || {
	fail "mkfs.ext4 meta-work"; exit 1; }
rpc rcow_create_snapshot \
	'{"lvol_name":"meta-work","snapshot_name":"meta-snap"}' >/dev/null || {
	fail "snapshot meta-snap"; exit 1; }
rpc rcow_deactive_bdev '{"device_name":"meta-work"}' >/dev/null 2>&1
rpc rcow_delete_lvol '{"lvol_name":"meta-work"}' >/dev/null || {
	fail "delete meta-work"; exit 1; }
pass "memory and metadata snaps sealed the same way as rootfs"

# ==========================================================================
echo ""
echo "=== [4] N clones from one snap, isolation, resize one clone"

for i in $(seq 0 $((N_CLONES - 1))); do
	rpc rcow_create_clone \
		"$(printf '{"snapshot_name":"tpl-rootfs","clone_name":"sb-%s"}' "${i}")" \
		>/dev/null || { fail "clone sb-${i}"; exit 1; }
	DEV="$(resolve "sb-${i}")"
	[ -b "${DEV}" ] || { fail "sb-${i} not a block device"; exit 1; }
	# Unique 1 MiB at 32 MiB so a shared-cluster write would collide.
	printf 'clone-%s\n' "${i}" | dd of="${DEV}" bs=1M seek=32 count=1 conv=sync \
		oflag=direct status=none 2>/dev/null
	eval "SB_${i}_DEV='${DEV}'"
	[ "${i}" = "1" ] && CLONE1_DEV="${DEV}"
done
pass "${N_CLONES} clones of tpl-rootfs activated"

# Resize must apply to the clone, not the snap. Cubelet grows sandbox rootfs
# after CreateVolumeFromSnapshot.
if ! rpc rcow_resize_lvol '{"lvol_name":"sb-0","size_gib":2}' >/dev/null; then
	fail "resize sb-0 after clone"
	exit 1
fi
pass "resized clone sb-0 to 2 GiB"

# The template snap must still mount as the original ext4 image, via a clone
# that only wrote past the filesystem. Use sb-1 (not resized).
unmount_quietly "${MNT}"
mount "${CLONE1_DEV}" "${MNT}" || { fail "mount clone sb-1"; exit 1; }
if [ "$(cat "${MNT}/marker.txt" 2>/dev/null)" = "tpl-v1" ]; then
	pass "clone still reads the sealed ext4 marker"
else
	fail "clone lost the template filesystem"
	exit 1
fi
unmount_quietly "${MNT}"

# Isolation: each clone's 32 MiB slot is its own.
for i in $(seq 0 $((N_CLONES - 1))); do
	eval "DEV=\${SB_${i}_DEV}"
	got="$(dd if="${DEV}" bs=1M skip=32 count=1 iflag=direct status=none 2>/dev/null \
		| tr -d '\0' | head -c 16)"
	echo "${got}" | grep -q "clone-${i}" || {
		fail "sb-${i} marker missing (got '${got}')"; exit 1; }
done
pass "clone writes did not leak across the fan-out"

# ==========================================================================
echo ""
echo "=== [5] fs-only contract: snapshot an active, frozen template clone"

rpc rcow_create_clone \
	'{"snapshot_name":"tpl-rootfs","clone_name":"sb-live"}' >/dev/null || {
	fail "clone sb-live"; exit 1; }
LIVE_DEV="$(resolve sb-live)"
[ -b "${LIVE_DEV}" ] || { fail "sb-live not a block device"; exit 1; }

# This host's ext4 leaves barriers off unless asked. Without them fsfreeze
# writes the journal as plain data and never sends NVMe FLUSH or FUA, so the
# per-lvol upload path is not exercised. A guest with the usual barrier
# default does send both.
mount -o barrier "${LIVE_DEV}" "${MNT}" || { fail "mount sb-live"; exit 1; }
# Pre-allocate sb-live's writable cluster before the baseline lvstore drain so
# the subsequent overwrite dirties only sb-live's own data chunks (not shared
# blobstore cluster-0 allocation metadata).
printf 'initial-marker\n' \
	>"${MNT}/disk/tpl-test_0/upper/root/work/state.txt"
sync
# Start from a clean overlay so the per-lvol flush boundary is directly
# observable in chunks_flushed and overlay_bytes.
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
	>/dev/null || { fail "baseline flush before fs-only test"; exit 1; }

# Dirty one 4 KiB block on another lvol (sb-0) at its already-allocated 32 MiB
# cluster without conv=fsync. Under the 45s hold-back policy this partial chunk
# sits in aging_q, and an NVMe FLUSH on sb-live must NOT flush sb-0's chunk.
dd if=/dev/urandom of="${SB_0_DEV}" bs=4k seek=8192 count=1 \
	oflag=direct status=none 2>/dev/null || {
	fail "dirty 4 KiB on sibling sb-0"; exit 1; }
SB0_OVERLAY_BYTES="$(lvs_stat overlay_bytes)"
if [ "${SB0_OVERLAY_BYTES}" = "4096" ]; then
	pass "sibling sb-0 holds 4096 unflushed bytes in overlay"
else
	fail "expected 4096 overlay bytes from sibling sb-0, got ${SB0_OVERLAY_BYTES}"
	exit 1
fi

FLUSHED_BEFORE="$(lvs_stat chunks_flushed)"
printf 'fs-only-live-marker\n' \
	>"${MNT}/disk/tpl-test_0/upper/root/work/state.txt"

# Guest fsfreeze issues NVMe FLUSH, then the journal commit is a WRITE with
# FUA. Both complete at shm durability and must not touch sibling sb-0.
#
# Reported, not asserted: the flusher's own policy (a full chunk, or the 45 s
# hold-back expiring on a slow run) can upload in this window with no FLUSH
# involved, so an assertion here failed at random. That FLUSH does not drain
# to S3 is asserted deterministically by run_recovery_test.sh round 2, which
# requires pending ring records after a conv=fsync. What is asserted here is
# the part only this test can see: sb-0 is still dirty after the snapshot.
fsfreeze -f "${MNT}" || { fail "freeze sb-live ext4"; exit 1; }
FROZEN_MNT=1
FLUSHED_AFTER_FREEZE="$(lvs_stat chunks_flushed)"
info "chunks_flushed across fsfreeze: ${FLUSHED_BEFORE} -> ${FLUSHED_AFTER_FREEZE}"

if ! rpc rcow_create_snapshot \
		'{"lvol_name":"sb-live","snapshot_name":"live-rootfs"}' >/dev/null; then
	fail "snapshot frozen active clone"
	fsfreeze -u "${MNT}" >/dev/null 2>&1 || true
	FROZEN_MNT=0
	exit 1
fi
FLUSHED_AFTER_SNAP="$(lvs_stat chunks_flushed)"
OVERLAY_AFTER_SNAP="$(lvs_stat overlay_bytes)"
if [ "${FLUSHED_AFTER_SNAP}" != "missing" ] && \
   [ "${FLUSHED_BEFORE}" != "missing" ] && \
   [ "${FLUSHED_AFTER_SNAP}" -gt "${FLUSHED_BEFORE}" ] 2>/dev/null; then
	pass "create snapshot uploaded sb-live chunks (${FLUSHED_BEFORE} -> ${FLUSHED_AFTER_SNAP}, overlay_bytes=${OVERLAY_AFTER_SNAP})"
else
	fail "create snapshot did not upload sb-live chunks (${FLUSHED_BEFORE} -> ${FLUSHED_AFTER_SNAP}, overlay_bytes=${OVERLAY_AFTER_SNAP})"
	fsfreeze -u "${MNT}" >/dev/null 2>&1 || true
	FROZEN_MNT=0
	exit 1
fi
# "Something was uploaded" does not say the snapshot was. The remainder does:
# the overlay was empty at the baseline, the frozen mount writes nothing, and
# the publish covers the snapshot's chain and the blobstore metadata region. So
# nothing but the sibling's bytes may remain. Before the metadata region was
# included this read 16384 -- sb-0's 4096 plus three bitmap and blob pages that
# existed only in the ring when the RPC returned.
#
# An upper bound, not an equality: the flusher's own policy (the 45 s hold-back
# expiring on a slow run, a forced flush) may upload sb-0 in this window, which
# can only lower the count. A publish that missed something can only raise it.
if [ "${OVERLAY_AFTER_SNAP}" -le "${SB0_OVERLAY_BYTES}" ] 2>/dev/null; then
	pass "nothing but sibling sb-0 was left unpublished when the snapshot returned (${OVERLAY_AFTER_SNAP} of ${SB0_OVERLAY_BYTES} bytes left)"
else
	fail "the snapshot returned with ${OVERLAY_AFTER_SNAP} bytes unpublished, more than sibling sb-0's ${SB0_OVERLAY_BYTES}"
	fsfreeze -u "${MNT}" >/dev/null 2>&1 || true
	FROZEN_MNT=0
	exit 1
fi
# That the publish was scoped to sb-live's chain -- sb-0 still dirty -- is only
# observable while sb-0 is still in the overlay. If policy already uploaded it,
# say so and move on: the scoping is covered deterministically by
# s3_flush_test (flush_chunks leaves unnamed chunks alone).
if [ "${OVERLAY_AFTER_SNAP}" != "${SB0_OVERLAY_BYTES}" ]; then
	info "sibling sb-0 was uploaded by flusher policy before the check (overlay_bytes=${OVERLAY_AFTER_SNAP}); publish scoping not observable this run"
else
# The byte count cannot tell sb-0 from some other 4 KiB, so read sb-0's block
# back: a full cover from the overlay increments overlay_hits and does not GET.
# A flushed chunk would miss.
HITS_BEFORE="$(lvs_stat overlay_hits)"
dd if="${SB_0_DEV}" of=/dev/null bs=4k skip=8192 count=1 iflag=direct \
	status=none 2>/dev/null || {
	fail "re-read sibling sb-0"; 
	fsfreeze -u "${MNT}" >/dev/null 2>&1 || true
	FROZEN_MNT=0
	exit 1
}
HITS_AFTER="$(lvs_stat overlay_hits)"
if [ "${HITS_AFTER}" != "missing" ] && [ "${HITS_BEFORE}" != "missing" ] && \
   [ "${HITS_AFTER}" -gt "${HITS_BEFORE}" ] 2>/dev/null; then
	pass "snapshot publish left sibling sb-0 dirty (overlay hit ${HITS_BEFORE} -> ${HITS_AFTER}, overlay_bytes=${OVERLAY_AFTER_SNAP})"
else
	fail "sibling sb-0 was not served from overlay after snapshot (${HITS_BEFORE} -> ${HITS_AFTER}, overlay_bytes=${OVERLAY_AFTER_SNAP})"
	fsfreeze -u "${MNT}" >/dev/null 2>&1 || true
	FROZEN_MNT=0
	exit 1
fi
fi

# A caller whose reply was lost -- a publish that outlasted its RPC timeout --
# asks again under the same name. The snapshot from the first attempt cannot be
# deleted (sb-live is already its clone), so -EEXIST here would strand it.
# Still frozen, so nothing was written since and the retry has to answer with
# the snapshot that exists.
if rpc rcow_create_snapshot \
		'{"lvol_name":"sb-live","snapshot_name":"live-rootfs"}' \
		>/dev/null 2>"${WORKDIR}/snap_retry.err"; then
	pass "same-name retry of an unchanged source answers the existing snapshot"
else
	fail "same-name retry of an unchanged source failed: $(cat "${WORKDIR}/snap_retry.err")"
	fsfreeze -u "${MNT}" >/dev/null 2>&1 || true
	FROZEN_MNT=0
	exit 1
fi
fsfreeze -u "${MNT}" || { fail "thaw sb-live ext4"; exit 1; }
FROZEN_MNT=0

# Fs-only resumes the source before CommitRootfs exports the snapshot. Make
# that interval observable by overwriting the marker on the live source.
printf 'post-snapshot-source-marker\n' \
	>"${MNT}/disk/tpl-test_0/upper/root/work/state.txt"
sync
unmount_quietly "${MNT}"

# Now the source has been written since, so the same name is no longer a retry:
# answering with live-rootfs would hand back content older than asked for.
if SNAP_AGAIN_ERR="$(rpc rcow_create_snapshot \
		'{"lvol_name":"sb-live","snapshot_name":"live-rootfs"}' 2>&1 >/dev/null)"; then
	fail "same-name snapshot after the source was written succeeded"
	exit 1
elif err_is_already_exists "${SNAP_AGAIN_ERR}"; then
	pass "same-name snapshot after a write is refused as already existing"
else
	fail "same-name snapshot after a write failed for another reason: ${SNAP_AGAIN_ERR}"
	exit 1
fi

rpc rcow_create_clone \
	'{"snapshot_name":"live-rootfs","clone_name":"sb-live-restored"}' >/dev/null || {
	fail "clone live-rootfs"; exit 1; }
RESTORED_DEV="$(resolve sb-live-restored)"
[ -b "${RESTORED_DEV}" ] || {
	fail "sb-live-restored not a block device"; exit 1; }
mount "${RESTORED_DEV}" "${MNT}" || {
	fail "mount sb-live-restored"; exit 1; }
if [ "$(cat "${MNT}/disk/tpl-test_0/upper/root/work/state.txt" 2>/dev/null)" = \
		"fs-only-live-marker" ]; then
	pass "frozen active clone snapshot contains acknowledged ext4 write"
else
	fail "frozen active clone snapshot lost acknowledged ext4 write"
	exit 1
fi
unmount_quietly "${MNT}"

if LIVE_EXPORT="$(export_with_retry live-rootfs)"; then
	EXPORTS="${EXPORTS} ${LIVE_EXPORT}"
	pass "exported frozen live-rootfs as ${LIVE_EXPORT}"
else
	fail "export frozen live-rootfs"
	exit 1
fi
wait_export_done_by_name live-rootfs || exit 1

if ! rpc rcow_import_lvol \
		"$(printf '{"lvol_name":"sb-live-imported","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
			"${LIVE_EXPORT}" "${RCOW_LVS_NAME}")" >/dev/null; then
	fail "import exported live-rootfs"
	exit 1
fi
IMPORTED_DEV="$(resolve sb-live-imported)"
[ -b "${IMPORTED_DEV}" ] || {
	fail "sb-live-imported not a block device"; exit 1; }
mount "${IMPORTED_DEV}" "${MNT}" || {
	fail "mount sb-live-imported"; exit 1; }
if [ "$(cat "${MNT}/disk/tpl-test_0/upper/root/work/state.txt" 2>/dev/null)" = \
		"fs-only-live-marker" ]; then
	pass "imported fs-only export contains the pre-thaw marker"
else
	fail "imported fs-only export lost the pre-thaw marker"
	exit 1
fi
unmount_quietly "${MNT}"

if [ "${S3LVOL_FS_ONLY_ONLY:-0}" -eq 1 ]; then
	echo ""
	echo "=== focused fs-only regression done ==="
	exit 0
fi

# ==========================================================================
echo ""
echo "=== [6] three snapshots exported in parallel, polled by snapshot_name"

SNAPS="tpl-rootfs mem-snap meta-snap"
for n in ${SNAPS}; do
	(
		if uuid="$(export_with_retry "${n}")"; then
			printf '%s' "${uuid}" >"${WORKDIR}/export.${n}.uuid"
		else
			printf 'FAIL' >"${WORKDIR}/export.${n}.uuid"
		fi
	) &
done
wait

FAIL_EXPORT=0
for n in ${SNAPS}; do
	uuid="$(cat "${WORKDIR}/export.${n}.uuid" 2>/dev/null || true)"
	if [ -z "${uuid}" ] || [ "${uuid}" = "FAIL" ]; then
		fail "parallel export of ${n}"
		FAIL_EXPORT=1
		continue
	fi
	EXPORTS="${EXPORTS} ${uuid}"
	eval "UUID_$(printf '%s' "${n}" | tr '-' '_')=${uuid}"
	pass "exported ${n} as ${uuid}"
done
[ "${FAIL_EXPORT}" -eq 0 ] || exit 1

for n in ${SNAPS}; do
	wait_export_done_by_name "${n}" || exit 1
done
pass "all three reached DONE when queried by snapshot_name"

# cubecow's RPC doc says the uuid is stable for the snapshot's lifetime.
# s3lvol only coalesces while an export is still in flight (see
# run_export_test.sh 11d.3); after DONE a second call mints a new uuid.
# Cubelet persists the first uuid and polls that, so we do not re-export
# here -- a new uuid would be a different export, not a retry of this one.

# ==========================================================================
echo ""
echo "=== [7] delete of a snap with clones/export: must not vanish, must not look like not-found"

# s3lvol may refuse with EBUSY, or accept the delete as deferred (pending
# mark, bool_value true, extra field deferred=true). Cubelet's FFI unwraps
# that as success and would think the template is gone. The load-bearing
# check is therefore that the snapshot is still queryable, and that any
# error string does not contain "not found".
DEL_RAW="$(python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" --raw \
	rcow_delete_lvol '{"lvol_name":"tpl-rootfs"}' 2>&1)" || true
if err_is_not_found "${DEL_RAW}"; then
	fail "delete of live template contained not-found: ${DEL_RAW}"
	exit 1
fi
if ! rpc rcow_get_snapshot_status '{"snapshot_name":"tpl-rootfs"}' >/dev/null; then
	fail "tpl-rootfs vanished after delete RPC"
	exit 1
fi
if printf '%s' "${DEL_RAW}" | grep -q '"deferred"[[:space:]]*:[[:space:]]*true'; then
	pass "delete deferred; snapshot still queryable"
elif printf '%s' "${DEL_RAW}" | grep -qiE 'busy|clone|referenced|in use|export'; then
	pass "delete refused; snapshot still queryable"
else
	info "delete reply: ${DEL_RAW}"
	pass "snapshot still queryable after delete RPC"
fi

# ==========================================================================
echo ""
echo "=== [8] failed import must not leave a named shell lvol"

GHOST_ERR="$(rpc rcow_import_lvol \
	'{"lvol_name":"ghost","export_uuid":"00000000-0000-0000-0000-ffffffffffff","decouple":true}' \
	2>&1)" && {
	fail "import of a garbage uuid succeeded"; exit 1; }
pass "garbage import refused: $(printf '%s' "${GHOST_ERR}" | head -c 80)"

if rpc rcow_get_bdev '{"device_name":"ghost"}' >/dev/null 2>&1; then
	fail "ghost became a bdev after a failed import"
	exit 1
fi
# delete of a name that must not exist: cubecow treats not-found as success.
GHOST_DEL="$(rpc rcow_delete_lvol '{"lvol_name":"ghost"}' 2>&1)" && {
	fail "ghost existed after failed import (delete succeeded)"
	exit 1
}
if err_is_already_exists "${GHOST_DEL}"; then
	fail "ghost occupied the namespace after failed import: ${GHOST_DEL}"
	exit 1
fi
if err_is_not_found "${GHOST_DEL}"; then
	pass "failed import left no ghost lvol"
else
	# Some servers refuse delete of a missing name with a different wording.
	# As long as it is not already-exists, the namespace is free.
	info "delete ghost: ${GHOST_DEL}"
	pass "failed import left no ghost lvol (delete was not already-exists)"
fi

# ==========================================================================
echo ""
echo "=== [9] import(decouple=true) then activate immediately (no decouple wait)"

for i in $(seq 0 $((N_CLONES - 1))); do
	del_vol "sb-${i}"
done
rpc rcow_deactive_bdev '{"device_name":"tpl-rootfs"}' >/dev/null 2>&1
rpc rcow_deactive_bdev '{"device_name":"mem-snap"}' >/dev/null 2>&1
rpc rcow_deactive_bdev '{"device_name":"meta-snap"}' >/dev/null 2>&1
# [5] left these activated. Unload drops their bdevs but not the active
# registry, and the next rcow_verify_active waits for every registry entry.
for n in sb-live sb-live-restored sb-live-imported; do
	rpc rcow_deactive_bdev "$(printf '{"device_name":"%s"}' "${n}")" \
		>/dev/null 2>&1
done

# One blobstore per node on create: unload the source so the destination
# lvstore can be created. The export lives in S3.
rpc rcow_unload_lvstore \
	"$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" >/dev/null || {
	fail "unload src lvstore"; exit 1; }
pass "src lvstore unloaded; exports remain in S3"

truncate -s 1G "${DST_WAL_IMG}"
python3 "${RCOW_SPDK_RPC_PY}" -s "${RCOW_RPC_SOCK}" bdev_aio_create \
	"${DST_WAL_IMG}" "${DST_WAL_BDEV}" 4096 >/dev/null || {
	fail "bdev_aio_create ${DST_WAL_BDEV}"; exit 1; }

# TLS and path style belong to rcow_add_s3_config, which rcow_start.sh
# already applied from s3.cfg. rcow_create_lvstore rejects those fields.
rpc rcow_create_lvstore \
	"$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"%s","journal_size_mb":%d,"wal_size_mb":%d}' \
		"${DST_LVS}" "${BUCKET}" "${RCOW_CAPACITY_GB}" "${DST_WAL_BDEV}" \
		"${RCOW_JOURNAL_MB}" "${RCOW_WAL_MB}")" >/dev/null || {
	fail "create dst lvstore"; exit 1; }
DST_CREATED=1
pass "dst lvstore ${DST_LVS} created"

# cubecow always sends decouple=true and then ActivateVolume without a waiter.
if ! rpc rcow_import_lvol \
	"$(printf '{"lvol_name":"imp-rootfs","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
		"${UUID_tpl_rootfs}" "${DST_LVS}")" >/dev/null; then
	fail "import imp-rootfs"
	exit 1
fi
pass "import(decouple=true) returned"

IMP_DEV="$(resolve imp-rootfs)"
[ -b "${IMP_DEV}" ] || { fail "imp-rootfs did not become a device"; exit 1; }
pass "activated imp-rootfs immediately as ${IMP_DEV}"

unmount_quietly "${MNT}"
mount "${IMP_DEV}" "${MNT}" || { fail "mount imp-rootfs right after import"; exit 1; }
if [ "$(cat "${MNT}/marker.txt" 2>/dev/null)" = "tpl-v1" ]; then
	pass "imported volume mounted ext4 and read the sealed marker without waiting for decouple"
else
	fail "imported ext4 did not contain marker.txt"
	exit 1
fi
unmount_quietly "${MNT}"
