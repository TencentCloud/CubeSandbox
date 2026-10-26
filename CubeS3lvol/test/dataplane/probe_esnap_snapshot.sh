#!/usr/bin/env bash
# Phase 1 step-0 probe: create_snapshot semantics on an esnap clone.
#
# Shape copied from test/dataplane/probe_decouple_window.sh: one target, two
# lvstores in turn (one process holds one blobstore: unload the source after
# it is built and exported, then create the destination), expose the bdev
# with nvmf_subsystem_add_ns, then find the host device under
# /sys/class/nvme.
#
# Questions:
#   1. Does create_snapshot on esnap clone V succeed, and what does it
#      produce?
#   2. Can S read the right data (through to source export A)?
#   3. After then decoupling V, is S still correct?
#      -- is the hazard claimed at lvstore.c:2531-2535 real?
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

SRC_LVS=pesnap_src
DST_LVS=pesnap_dst
SRC_WAL=/tmp/pesnap_src_wal.img
DST_WAL=/tmp/pesnap_dst_wal.img
RPC_SOCK=/tmp/pesnap.sock
TGT_LOG=/tmp/pesnap_target.log
NQN="nqn.2026-08.io.spdk:pesnap"
PORT="4470"
WORKDIR="$(mktemp -d /tmp/pesnap.XXXXXX)"
MNT="${WORKDIR}/mnt"
FILL_MB=16

TGT_PID=""
CONNECTED=0

rpc() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" "$@"; }
# Arbitrary methods (nvmf_*) with the answer left whole.
raw() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" --raw "$1" ${2:+"$2"}; }
info() { echo "---- $*"; }

cleanup()
{
	set +u
	[ "${CONNECTED}" = "1" ] && nvme disconnect -n "${NQN}" >/dev/null 2>&1
	if [ -n "${TGT_PID}" ]; then
		kill "${TGT_PID}" 2>/dev/null
		sleep 2
		kill -9 "${TGT_PID}" 2>/dev/null
	fi
	rcow_load_credentials
	for p in "${SRC_LVS}" "${DST_LVS}"; do
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" -b "$(rcow_s3_buckets|head -1)" \
			-r "$(rcow_cfg_get region)" -p "${p}/" >/dev/null 2>&1
	done
	for u in ${UUIDS:-}; do
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" -b "$(rcow_s3_buckets|head -1)" \
			-r "$(rcow_cfg_get region)" -p "exports/${u}" >/dev/null 2>&1
	done
	rm -f "${SRC_WAL}" "${DST_WAL}" "${RPC_SOCK}"
	[ -z "${KEEP:-}" ] && rm -rf "${WORKDIR}"
	echo
	grep -aE 'external snapshot|esnap|not a clone|create snapshot|snapshot/clone|Decoupling|materialised' \
		"${TGT_LOG}" 2>/dev/null | tail -15
	echo "===== log: ${TGT_LOG}   workdir: ${WORKDIR}"
}
trap cleanup EXIT

# --------------------------------------------------------------------------
pkill -9 -f s3lvol_tgt 2>/dev/null
sleep 2
rm -f "${RPC_SOCK}" "${SRC_WAL}" "${DST_WAL}"
truncate -s 320M "${SRC_WAL}"
truncate -s 320M "${DST_WAL}"

rcow_load_credentials
EP="$(rcow_cfg_get endpoint)"; BK="$(rcow_s3_buckets|head -1)"; RG="$(rcow_cfg_get region)"
info "endpoint ${EP}  bucket ${BK}"
for p in "${SRC_LVS}" "${DST_LVS}"; do
	python3 "${PREFIX_RM}" -e "${EP}" -b "${BK}" -r "${RG}" -p "${p}/" >/dev/null 2>&1
done

# The target reads credentials from its own environment.
info "starting target"
AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
	"${TGT_BIN}" -m 0x3 --no-huge -s 2048 -r "${RPC_SOCK}" >"${TGT_LOG}" 2>&1 &
TGT_PID=$!
for _ in $(seq 80); do [ -S "${RPC_SOCK}" ] && break; sleep 0.25; done
[ -S "${RPC_SOCK}" ] || { echo "target failed to start"; tail -20 "${TGT_LOG}"; exit 1; }
sleep 1

rpc rcow_add_s3_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"}' \
	"${BK}" "${EP}" "${BK}" "${RG}")" >/dev/null || { echo "add_s3_config failed"; exit 1; }
raw bdev_aio_create "$(printf '{"filename":"%s","name":"src_wal0","block_size":4096}' "${SRC_WAL}")" \
	>/dev/null 2>&1
raw bdev_aio_create "$(printf '{"filename":"%s","name":"dst_wal0","block_size":4096}' "${DST_WAL}")" \
	>/dev/null 2>&1

rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"src_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${SRC_LVS}" "${BK}")" >/dev/null || { echo "create ${SRC_LVS} failed"; exit 1; }
info "source lvstore ready"

raw nvmf_create_transport '{"trtype":"TCP"}' >/dev/null 2>&1
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PESNAP000000001"}' \
	"${NQN}")" >/dev/null 2>&1
raw nvmf_subsystem_add_listener "$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%s"}}' \
	"${NQN}" "${PORT}")" >/dev/null 2>&1

expose()
{
	# --raw prints the result member as it arrived, which for
	# nvmf_subsystem_add_ns is the nsid number itself -- not an object with a
	# "result" key, so there is nothing to parse out of it.
	raw nvmf_subsystem_add_ns "$(printf '{"nqn":"%s","namespace":{"bdev_name":"%s"}}' \
		"${NQN}" "$1")" 2>/dev/null | tr -d '[:space:]'
}

unexpose()
{
	local nsid="$1"
	[ -n "${nsid}" ] && raw nvmf_subsystem_remove_ns \
		"$(printf '{"nqn":"%s","nsid":%s}' "${NQN}" "${nsid}")" >/dev/null 2>&1
	return 0
}

ctrl_of_nqn()
{
	local c
	for c in /sys/class/nvme/nvme*; do
		[ "$(cat "${c}/subsysnqn" 2>/dev/null)" = "${NQN}" ] && { basename "${c}"; return 0; }
	done
	return 1
}

wait_dev()
{
	local nsid="$1" deadline ctrl dev
	deadline=$(( $(date +%s) + 30 ))
	while [ "$(date +%s)" -lt "${deadline}" ]; do
		if ctrl="$(ctrl_of_nqn)"; then
			dev="/dev/${ctrl}n${nsid}"
			[ -b "${dev}" ] && { printf '%s' "${dev}"; return 0; }
		fi
		sleep 0.3
	done
	return 1
}

# --------------------------------------------------------------------------
# [1] source: a volume with known content -> snapshot -> REF export
# --------------------------------------------------------------------------
info "[1] source volume, filled"
rpc rcow_create_lvol "$(printf '{"lvol_name":"src","size_gib":1}')" >/dev/null \
	|| { echo "create_lvol failed"; exit 1; }
NSID_SRC="$(expose "${SRC_LVS}/src")"
nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1
CONNECTED=1
sleep 2
SRC_DEV="$(wait_dev "${NSID_SRC}")" || { echo "source device never appeared"; exit 1; }
info "source device ${SRC_DEV}"

PAT="${WORKDIR}/pattern.bin"
dd if=/dev/urandom of="${PAT}" bs=1M count="${FILL_MB}" status=none
PAT_MD5="$(md5sum "${PAT}" | cut -d' ' -f1)"
dd if="${PAT}" of="${SRC_DEV}" bs=1M oflag=direct status=none
sync
SRC_READ="$(dd if="${SRC_DEV}" bs=1M count="${FILL_MB}" iflag=direct status=none | md5sum | cut -d' ' -f1)"
info "pattern ${PAT_MD5}; read back ${SRC_READ} $([ "${SRC_READ}" = "${PAT_MD5}" ] && echo match || echo MISMATCH)"

rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
rpc rcow_create_snapshot '{"lvol_name":"src","snapshot_name":"src-snap"}' >/dev/null \
	|| { echo "snapshot failed"; exit 1; }
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
sleep 3

EXP_UUID="$(rpc rcow_export_snapshot '{"snapshot_name":"src-snap"}' 2>&1 | tr -d '"[:space:]\n')"
UUIDS="${EXP_UUID}"
[ -n "${EXP_UUID}" ] || { echo "export failed"; exit 1; }
info "exported as ${EXP_UUID}"
for _ in $(seq 120); do
	st="$(rpc rcow_get_snapshot_status "$(printf '{"export_uuid":"%s"}' "${EXP_UUID}")" 2>/dev/null \
		| tr -d '"[:space:]\n')"
	case "${st}" in *DONE*) break ;; esac
	sleep 1
done
info "export status: ${st:-unknown}"

# The source is done with: its namespace goes, then the lvstore, so the
# destination can take the one blobstore this process can hold.
unexpose "${NSID_SRC}"
sleep 2
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null 2>&1 \
	|| { echo "unload source failed"; exit 1; }
sleep 2

rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"dst_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${DST_LVS}" "${BK}")" >/dev/null || { echo "create ${DST_LVS} failed"; exit 1; }
info "destination lvstore ready"

# --------------------------------------------------------------------------
# [2] import without decoupling: V is a real esnap clone, nothing queued
# --------------------------------------------------------------------------
info "[2] import with decouple:false"
IMP_OUT="$(rpc rcow_import_lvol "$(printf '{"lvol_name":"V","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${DST_LVS}")" 2>&1)"
info "import reply: ${IMP_OUT}"

echo
info "[3] create snapshot of V  (does derive_check allow it?)"
SNAP_OUT="$(rpc rcow_create_snapshot '{"lvol_name":"V","snapshot_name":"S"}' 2>&1)"
info "create_snapshot reply: ${SNAP_OUT}"
rpc --ls rcow_get_lvstores

# --------------------------------------------------------------------------
# [4] can S be read, and does it hold A's data?
# --------------------------------------------------------------------------
echo
info "[4] read S"
NSID_S="$(expose "${DST_LVS}/S")"
info "S exposed as nsid ${NSID_S}"
if [ -n "${NSID_S}" ] && S_DEV="$(wait_dev "${NSID_S}")"; then
	S_MD5="$(dd if="${S_DEV}" bs=1M count="${FILL_MB}" iflag=direct status=none | md5sum | cut -d' ' -f1)"
	info "S read = ${S_MD5}  $([ "${S_MD5}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
else
	info "S could not be exposed/read"
	S_MD5=""
fi
unexpose "${NSID_S}"
sleep 2

# --------------------------------------------------------------------------
# [5] V itself, then decouple it, then S again
# --------------------------------------------------------------------------
echo
info "[5] read V, decouple it, then read S again"
NSID_V="$(expose "${DST_LVS}/V")"
V_MD5=""
if [ -n "${NSID_V}" ] && V_DEV="$(wait_dev "${NSID_V}")"; then
	V_MD5="$(dd if="${V_DEV}" bs=1M count="${FILL_MB}" iflag=direct status=none | md5sum | cut -d' ' -f1)"
	info "V read (before decouple) = ${V_MD5}  $([ "${V_MD5}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
else
	info "V could not be exposed/read"
fi

rpc rcow_decouple_lvol '{"lvol_name":"V"}' >/dev/null 2>&1 \
	&& info "decouple submitted" || info "decouple submit failed"
for i in $(seq 240); do
	busy="$(rpc rcow_get_decouple 2>/dev/null | python3 -c '
import json,sys
try: rows=json.load(sys.stdin)
except Exception: rows=[]
if isinstance(rows, dict): rows=rows.get("queue", [])
print(len([r for r in rows if r.get("lvol_name")=="V"]))' 2>/dev/null)"
	[ "${busy}" = "0" ] && break
	sleep 1
done
info "decouple settled (waited ${i}s)"

if [ -n "${NSID_S}" ] && S_DEV2="$(wait_dev "${NSID_S}")" || NSID_S="$(expose "${DST_LVS}/S")" && S_DEV2="$(wait_dev "${NSID_S}")"; then
	S2_MD5="$(dd if="${S_DEV2}" bs=1M count="${FILL_MB}" iflag=direct status=none | md5sum | cut -d' ' -f1)"
	info "S read (after V decoupled) = ${S2_MD5}  $([ "${S2_MD5}" = "${PAT_MD5}" ] && echo "MATCH ✓ S still correct" || echo "MISMATCH ✗ S was broken by the decouple")"
else
	info "S could not be re-read after the decouple"
fi
unexpose "${NSID_S}"

# --------------------------------------------------------------------------
# [6] delete V, then read S again.
#
# The distinction that matters for the pin table: does S hold the esnap
# parent itself (a), in which case V can go at any time, or does S read
# through V (b), so that deleting V would strand it? Only a delete of V
# separates them -- while V exists, both readings give the same bytes.
# --------------------------------------------------------------------------
echo
info "[6] delete V, then read S"
unexpose "${NSID_V}"
sleep 2
DEL_OUT="$(rpc rcow_delete_lvol '{"lvol_name":"V"}' 2>&1)"
info "delete V reply: ${DEL_OUT}"
rpc --ls rcow_get_lvstores

S3_MD5=""
if rpc --ls rcow_get_lvstores 2>/dev/null | grep -qE '^S '; then
	NSID_S3="$(expose "${DST_LVS}/S")"
	if [ -n "${NSID_S3}" ] && S_DEV3="$(wait_dev "${NSID_S3}")"; then
		S3_MD5="$(dd if="${S_DEV3}" bs=1M count="${FILL_MB}" iflag=direct status=none \
			| md5sum | cut -d' ' -f1)"
		info "S read (after V deleted) = ${S3_MD5}  $([ "${S3_MD5}" = "${PAT_MD5}" ] \
			&& echo "MATCH ✓ S is independent of V (holds its own esnap parent)" \
			|| echo "MISMATCH ✗ S depends on V and broke after V was deleted")"
	else
		info "S could not be read after deleting V"
	fi
	unexpose "${NSID_S3}"
else
	info "S is gone (deleting V took it with it)"
fi

echo
rpc --ls rcow_get_lvstores

echo
echo "===== SUMMARY"
echo "  pattern        : ${PAT_MD5}"
echo "  V before decouple: ${V_MD5:-not read}"
echo "  S before decouple: ${S_MD5:-not read}"
echo "  S after  decouple: ${S2_MD5:-not read}"
echo "  S after  V deleted: ${S3_MD5:-not read}"
