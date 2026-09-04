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
#    [3]  deactive then get_bdev is not-found; reactivate still yields a
#         path that open(2)s, even if the node number changed
#    [4]  N clones from one template snap, isolated writes, then resize one
#         clone (sandbox rootfs grow after CreateVolumeFromSnapshot)
#    [5]  three snapshots exported at once (Pause/Commit: rootfs + memory +
#         metadata); busy is retried; progress is polled by snapshot_name,
#         which is what GetVolumeInfo does
#    [6]  deleting the template snap while clones exist is refused, and the
#         message must not contain "not found" (cubecow would treat that as
#         an idempotent success)
#    [7]  a failed import of a garbage uuid must not leave a named lvol
#    [8]  import(decouple=true) on a second lvstore → activate + ext4 mount
#         immediately, without waiting for decouple to finish
#
#  Usage:
#    sudo -E ./test/dataplane/run_cubecow_client_test.sh
#
#  Needs root, nvme-cli, e2fsprogs (mkfs.ext4), a readable /data/cubelet/s3.cfg.
#  Own lvstore / WAL / registries, so a production instance is untouched.
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

# Activate and echo the host device. Prints nothing on failure -- it runs in a
# command substitution, so the caller decides what an empty result means.
resolve()
{
	local name="$1" out path

	if ! out="$(rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${name}")" 2>&1)"; then
		echo "  ---- activate ${name} failed: ${out}" >&2
		return 1
	fi
	if ! rcow_verify_active 30 >/dev/null 2>&1; then
		echo "  ---- ${name}: rcow_verify_active timed out" >&2
		return 1
	fi
	path="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["device_path"])' \
		"$(rpc rcow_get_bdev "$(printf '{"device_name":"%s"}' "${name}")")" 2>/dev/null)"
	if [ -z "${path}" ] || [ ! -b "${path}" ]; then
		echo "  ---- ${name}: device_path '${path}' is not a block device" >&2
		return 1
	fi
	printf '%s' "${path}"
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

# Cubelet issues three exports against one lvstore. s3lvol drains one at a
# time and a waiting export has a short window to win, so the first wave is
# allowed to return busy; the client retries until it has a uuid.
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

cleanup()
{
	echo ""
	echo "=== cleanup"

	[ -n "${MNT}" ] && unmount_quietly "${MNT}"

	if rcow_target_alive; then
		for n in tpl-work mem-work meta-work \
			 sb-0 sb-1 sb-2 sb-3 \
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
[ -n "$(rcow_target_instances)" ] && { echo "a target is already running" >&2; exit 1; }

"${ROOT}/test/tools/check_binary_fresh.sh" "${RCOW_TGT_BIN}" || exit 1

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
echo "=== [5] three snapshots exported in parallel, polled by snapshot_name"

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
echo "=== [6] delete of a snap with clones/export: must not vanish, must not look like not-found"

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
echo "=== [7] failed import must not leave a named shell lvol"

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
echo "=== [8] import(decouple=true) then activate immediately (no decouple wait)"

for i in $(seq 0 $((N_CLONES - 1))); do
	del_vol "sb-${i}"
done
rpc rcow_deactive_bdev '{"device_name":"tpl-rootfs"}' >/dev/null 2>&1
rpc rcow_deactive_bdev '{"device_name":"mem-snap"}' >/dev/null 2>&1
rpc rcow_deactive_bdev '{"device_name":"meta-snap"}' >/dev/null 2>&1

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

S3LVOL_EXTRA_JSON=""
[ "${S3LVOL_TEST_PATH_STYLE:-0}" -eq 1 ] && S3LVOL_EXTRA_JSON+=',"path_style":true'
[ "${S3LVOL_TEST_NO_TLS:-0}" -eq 1 ] && S3LVOL_EXTRA_JSON+=',"no_tls":true'

rpc rcow_create_lvstore \
	"$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"%s","journal_size_mb":%d,"wal_size_mb":%d%s}' \
		"${DST_LVS}" "${BUCKET}" "${RCOW_CAPACITY_GB}" "${DST_WAL_BDEV}" \
		"${RCOW_JOURNAL_MB}" "${RCOW_WAL_MB}" "${S3LVOL_EXTRA_JSON}")" >/dev/null || {
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
