#!/usr/bin/env bash
# Phase 1 probe #3: how snapshot/clone interact with decouple.
#
# !! Historical probe; conclusions are in
#    docs/import-reference-snapshot-design.md §9.2.
#    It needed derive_check's two refuse branches #if 0'd out to even reach
#    scenario 2, and **the code has changed**: create_snapshot now cancels
#    an in-flight decouple (§3.1), so scenario 2 no longer needs a patch
#    and no longer reproduces the failure it measured then --
#    grepping for "blob is not a clone of an external snapshot" should now
#    find nothing, which is what the fix looks like.
#    For current behaviour use test/dataplane/run_snapshot_cancel_test.sh.
#
# Origin: probe #2 found that after create_snapshot, `decouple V` is refused
# with
#   "lvol 'V' does not read through to an export; there is nothing to decouple"
# -- i.e. snapshot S took the esnap identity and V became an ordinary clone
# of S.
#
# That directly affects whether the sentence in import-reference-snapshot-design
# §9 that "decouple finishes normally" still holds; it has to be checked.
#
# Questions:
#   A. After the snapshot, who is still an esnap clone -- V or S?
#   B. After `decouple V` is refused, can S (the read-only snapshot) be
#      decoupled instead?
#   C. import(decouple:true) with decouple already in flight, then snapshot --
#      does that decouple succeed or fail, and with what message?
#      (The derive_check comment predicted "blob is not a clone of an
#      external snapshot": materialise finished, then detaching the parent
#      as the last step failed.)
#   D. Whatever decouple does, is the data always correct?
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

SRC_LVS=pdec_src
DST_LVS=pdec_dst
SRC_WAL=/tmp/pdec_src_wal.img
DST_WAL=/tmp/pdec_dst_wal.img
RPC_SOCK=/tmp/pdec.sock
TGT_LOG=/tmp/pdec_target.log
NQN="nqn.2026-08.io.spdk:pdec"
PORT="4472"
WORKDIR="$(mktemp -d /tmp/pdec.XXXXXX)"
FILL_MB=16
WRITE_MB=4

TGT_PID=""
CONNECTED=0

rpc() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" "$@"; }
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
	grep -aE 'external snapshot|esnap|not a clone|only a read-only|create snapshot|snapshot/clone|Decoupling|materialised|Deleted lvol' \
		"${TGT_LOG}" 2>/dev/null | tail -18
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
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PDEC00000000001"}' \
	"${NQN}")" >/dev/null 2>&1
raw nvmf_subsystem_add_listener "$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%s"}}' \
	"${NQN}" "${PORT}")" >/dev/null 2>&1

expose()
{
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

# Read the first FILL_MB of a volume by name: expose, read, unexpose.
read_vol()
{
	local name="$1" nsid dev md5
	nsid="$(expose "${DST_LVS}/${name}")"
	[ -n "${nsid}" ] || { echo "EXPOSE_FAILED"; return 1; }
	dev="$(wait_dev "${nsid}")" || { unexpose "${nsid}"; echo "NO_DEV"; return 1; }
	md5="$(dd if="${dev}" bs=1M count="${FILL_MB}" iflag=direct status=none | md5sum | cut -d' ' -f1)"
	unexpose "${nsid}"
	sleep 1
	printf '%s' "${md5}"
}

# The ALLOC column of rcow_get_lvstores --ls, for one volume. SIZE prints as
# two fields ("1.0 GiB"), so ALLOC is $5.
alloc_of()
{
	rpc --ls rcow_get_lvstores 2>/dev/null | awk -v n="$1" '$1 == n { print $5; exit }'
}

# --------------------------------------------------------------------------
# [1] source: a volume with known content -> snapshot -> REF export
# --------------------------------------------------------------------------
info "[1] source volume, filled"
rpc rcow_create_lvol '{"lvol_name":"src","size_gib":1}' >/dev/null \
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
info "pattern ${PAT_MD5}; read back $([ "${SRC_READ}" = "${PAT_MD5}" ] && echo match || echo MISMATCH)"

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

unexpose "${NSID_SRC}"
sleep 2
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null 2>&1 \
	|| { echo "unload source failed"; exit 1; }
sleep 2
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"dst_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${DST_LVS}" "${BK}")" >/dev/null || { echo "create ${DST_LVS} failed"; exit 1; }
info "destination lvstore ready"

# --------------------------------------------------------------------------
# [2] Scenario 1: import(decouple:false) -> snapshot -> who holds the esnap?
# --------------------------------------------------------------------------
echo
info "[2] scenario 1: import with decouple:false, then snapshot"
rpc rcow_import_lvol "$(printf '{"lvol_name":"V","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${DST_LVS}")" >/dev/null 2>&1 || { echo "import failed"; exit 1; }
rpc rcow_create_snapshot '{"lvol_name":"V","snapshot_name":"S"}' >/dev/null 2>&1 \
	|| { echo "snapshot failed"; exit 1; }
rpc --ls rcow_get_lvstores

echo
info "[A] who still reads through to the export?"
DEC_V="$(rpc rcow_decouple_lvol '{"lvol_name":"V"}' 2>&1)"
info "decouple V  -> ${DEC_V}"
DEC_S="$(rpc rcow_decouple_lvol '{"lvol_name":"S"}' 2>&1)"
info "decouple S  -> ${DEC_S}"

# What is in the queue now (if S was accepted, wait for it to finish).
for i in $(seq 240); do
	busy="$(rpc rcow_get_decouple 2>/dev/null | python3 -c '
import json,sys
try: rows=json.load(sys.stdin)
except Exception: rows=[]
if isinstance(rows, dict): rows=rows.get("queue", [])
print(len(rows))' 2>/dev/null)"
	[ "${busy}" = "0" ] && break
	sleep 1
done
info "decouple queue drained (waited ${i}s)"

S_MD5_1="$(read_vol S)" || true
V_MD5_1="$(read_vol V)" || true
info "S = ${S_MD5_1}  $([ "${S_MD5_1}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
info "V = ${V_MD5_1}  $([ "${V_MD5_1}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
rpc --ls rcow_get_lvstores

# Drop scenario 1 to make room for scenario 2.
rpc rcow_delete_lvol '{"lvol_name":"V"}' >/dev/null 2>&1
rpc rcow_delete_lvol '{"lvol_name":"S"}' >/dev/null 2>&1
sleep 2

# --------------------------------------------------------------------------
# [3] Scenario 2: import(decouple:true), snapshot while decouple is in flight
#     -- the window the derive_check comment predicted would go wrong
#     Originally needed derive_check's two refuse branches temporarily
#     removed (#if 0) to even reach it
# --------------------------------------------------------------------------
echo
info "[3] scenario 2: import with decouple:true, snapshot while it is in flight"
rpc rcow_import_lvol "$(printf '{"lvol_name":"V2","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
	"${EXP_UUID}" "${DST_LVS}")" >/dev/null 2>&1 || { echo "import V2 failed"; exit 1; }

DEC_NOW="$(rpc rcow_get_decouple 2>/dev/null | python3 -c '
import json,sys
try: rows=json.load(sys.stdin)
except Exception: rows=[]
if isinstance(rows, dict): rows=rows.get("queue", [])
print(len([r for r in rows if r.get("lvol_name")=="V2"]))' 2>/dev/null)"
info "decouple entries for V2 at this moment: ${DEC_NOW}"

SNAP2="$(rpc rcow_create_snapshot '{"lvol_name":"V2","snapshot_name":"S2"}' 2>&1)"
info "create_snapshot during decouple -> ${SNAP2}"

LOG_MARK="$(wc -l < "${TGT_LOG}")"
for i in $(seq 300); do
	busy="$(rpc rcow_get_decouple 2>/dev/null | python3 -c '
import json,sys
try: rows=json.load(sys.stdin)
except Exception: rows=[]
if isinstance(rows, dict): rows=rows.get("queue", [])
print(len([r for r in rows if r.get("lvol_name")=="V2"]))' 2>/dev/null)"
	[ "${busy}" = "0" ] && break
	sleep 1
done
info "V2 left the decouple queue after ${i}s"

echo
info "[C] what the decouple actually did (log lines since the snapshot):"
tail -n +"${LOG_MARK}" "${TGT_LOG}" | grep -aE 'Decoupl|decouple|materialis|no longer reads|not a clone|external snapshot' \
	| sed 's/^/       /' | tail -10

S2_MD5="$(read_vol S2)" || true
V2_MD5="$(read_vol V2)" || true
info "S2 = ${S2_MD5}  $([ "${S2_MD5}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
info "V2 = ${V2_MD5}  $([ "${V2_MD5}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
rpc --ls rcow_get_lvstores

echo
info "[B2] after that, can either of them be decoupled?"
info "decouple V2 -> $(rpc rcow_decouple_lvol '{"lvol_name":"V2"}' 2>&1)"
info "decouple S2 -> $(rpc rcow_decouple_lvol '{"lvol_name":"S2"}' 2>&1)"
for i in $(seq 240); do
	busy="$(rpc rcow_get_decouple 2>/dev/null | python3 -c '
import json,sys
try: rows=json.load(sys.stdin)
except Exception: rows=[]
if isinstance(rows, dict): rows=rows.get("queue", [])
print(len(rows))' 2>/dev/null)"
	[ "${busy}" = "0" ] && break
	sleep 1
done
S2_MD5_B="$(read_vol S2)" || true
info "S2 after that = ${S2_MD5_B}  $([ "${S2_MD5_B}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"

echo
echo "===== SUMMARY"
echo "  pattern                       : ${PAT_MD5}"
echo
echo "  -- scenario 1: snapshot after import (no decouple in flight)"
echo "     decouple V (post-snapshot) : ${DEC_V}"
echo "     decouple S (post-snapshot) : ${DEC_S}"
echo "     S / V data                 : ${S_MD5_1:-n/a} / ${V_MD5_1:-n/a}"
echo
echo "  -- scenario 2: snapshot taken while a decouple was in flight"
echo "     create_snapshot            : ${SNAP2}"
echo "     S2 / V2 data               : ${S2_MD5:-n/a} / ${V2_MD5:-n/a}"
echo "     S2 after further decouples : ${S2_MD5_B:-n/a}"
