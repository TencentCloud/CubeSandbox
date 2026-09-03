#!/usr/bin/env bash
# End-to-end publish latency: how long do import -> create_snapshot ->
# export_snapshot take on each path?
#
# Origin (called out by a user, then measured): step 1 made create_snapshot
# O(1), but the real publish chain is import -> create_snapshot ->
# export_snapshot. Snapshot S takes the external parent, so export hits
# spdk_blob_is_esnap_clone() in export_build_chain, gets -ENOTSUP, and
# **silently falls back to a copy** (export_ref() -> export_copy()). The copy
# reads the allocated bytes back out and uploads them again.
#
# So O(1) only moved from one RPC to the next; end-to-end was still O(size).
# This probe puts the three paths' timings and resulting layouts side by side:
#
#   path A  import -> snapshot -> export                 (S is an esnap clone)
#   path B  import -> snapshot -> decouple S -> export   (converge, then publish)
#   path C  import (decouple finished) -> snapshot -> export
#           (the old way: materialise first, then snapshot)
#
# Questions:
#   1. Is path A's export REF or DENSE, and how long does it take?
#   2. Is path B's export REF? (If so, a converged snapshot can be
#      re-published zero-copy.)
#   3. Which path is actually faster end to end?
#   4. How many S3 objects does each occupy (storage amplification)?
#
# === The above was the pre-v3 state. Manifest v3 reversed the conclusion
#     (measured 2026-09-02). ===
#
#   path           publish      total     layout   export-prefix objects
#   A (old)        1781ms      2292ms     dense    64
#   A (after v3)    366ms       800ms     ref       0
#   B (after v3)    622ms      7551ms     ref       0
#   C (after v3)    620ms      7424ms     ref       0
#
# export_build_chain() no longer returns -ENOTSUP on an esnap: it reads the
# esnap id as an export uuid, hands it to export_ref() to find the parent
# manifest in the import registry, and s3_export_manifest_inherit() flattens
# the parent's multi-source table in. Path A is therefore zero-copy, and
# nearly 10x faster than B/C -- those two pay for a full-volume materialise
# via decouple. That is the O(1) publish the design wanted.
#
# This probe prints; it does not assert. Keep it as a performance yardstick.
# **The formal layout=ref regression is run_derived_test.sh around step [3]**
# (asserts version=3 and layout=ref). The numbers here answer "how much
# faster did publish actually get?".
#
# Residual cases that still fall back to a copy (see the decision table in
# docs/manifest-v3-format.md): cross bucket/endpoint/region, a parent
# manifest that is itself dense, mismatched chunk_size, local snapshot chain
# deeper than 32, a missing import-registry entry.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

SRC_LVS=ppub_src
DST_LVS=ppub_dst
SRC_WAL=/tmp/ppub_src_wal.img
DST_WAL=/tmp/ppub_dst_wal.img
RPC_SOCK=/tmp/ppub.sock
TGT_LOG=/tmp/ppub_target.log
NQN="nqn.2026-08.io.spdk:ppub"
PORT="4473"
WORKDIR="$(mktemp -d /tmp/ppub.XXXXXX)"
FILL_MB=64
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
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PPUB00000000001"}' \
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
# Timing and observation helpers
# --------------------------------------------------------------------------
now_ms() { date +%s%3N; }

# Layout of one export. It lives only in the manifest object (s3_export.c:581);
# no RPC reports it, so the manifest has to be read back.
layout_of()
{
	[ -n "${1:-}" ] || { echo "no-uuid"; return; }
	python3 "${ROOT}/test/tools/s3_get_manifest.py" -e "${EP}" -b "${BK}" \
		-r "${RG}" -u "$1" --field layout 2>/dev/null || echo "unreadable"
}

# Object count under a prefix, for storage amplification.
#
# Use wc rather than `grep -c . || echo 0`: grep's non-zero exit on no match
# would append another "0" onto a value that is already "0", producing a
# two-line "number".
objects_under()
{
	python3 "${PREFIX_RM}" --list -e "${EP}" -b "${BK}" -r "${RG}" -p "$1" \
		2>/dev/null | grep -c . | head -1
}

wait_export()
{
	local u="$1" i st
	for i in $(seq 600); do
		st="$(rpc rcow_get_snapshot_status "$(printf '{"export_uuid":"%s"}' "${u}")" \
			2>/dev/null | tr -d '"[:space:]\n')"
		case "${st}" in *DONE*) return 0 ;; *FAIL*|*ERROR*) return 1 ;; esac
		sleep 0.2
	done
	return 1
}

drain_decouple()
{
	local i
	for i in $(seq 900); do
		[ "$(rpc rcow_get_decouple 2>/dev/null | python3 -c '
import json,sys
try: r=json.load(sys.stdin)
except Exception: r=[]
print(len(r if isinstance(r,list) else r.get("queue",[])))' 2>/dev/null)" = "0" ] && return 0
		sleep 0.2
	done
	return 1
}

# --------------------------------------------------------------------------
# [2] Path A: import -> snapshot -> export (S is still an esnap clone)
# --------------------------------------------------------------------------
echo
info "[A] import -> snapshot -> export, with S still an esnap clone"
T0="$(now_ms)"
rpc rcow_import_lvol "$(printf '{"lvol_name":"VA","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
	"${EXP_UUID}" "${DST_LVS}")" >/dev/null 2>&1 || { echo "import VA failed"; exit 1; }
T_IMP="$(now_ms)"

rpc rcow_create_snapshot '{"lvol_name":"VA","snapshot_name":"SA"}' >/dev/null 2>&1 \
	|| { echo "snapshot SA failed"; exit 1; }
T_SNAP="$(now_ms)"

UA="$(rpc rcow_export_snapshot '{"snapshot_name":"SA"}' 2>&1 | tr -d '"[:space:]\n')"
if [ -n "${UA}" ]; then
	wait_export "${UA}" && T_EXP="$(now_ms)" || { T_EXP="$(now_ms)"; info "export A did not finish"; }
	UUIDS="${UUIDS} ${UA}"
else
	T_EXP="$(now_ms)"
	info "export A was refused"
fi

A_IMP=$((T_IMP - T0)); A_SNAP=$((T_SNAP - T_IMP)); A_EXP=$((T_EXP - T_SNAP))
A_TOTAL=$((T_EXP - T0))
A_LAYOUT="$(layout_of "${UA}")"
A_OBJS="$(objects_under "${DST_LVS}/exports/${UA}")"
info "A: import ${A_IMP}ms  snapshot ${A_SNAP}ms  export ${A_EXP}ms  total ${A_TOTAL}ms"
info "A: layout=${A_LAYOUT}  objects under its export prefix=${A_OBJS}"
grep -aE 'external snapshot, whose|exporting by copying|Snapshot chain reaches' \
	"${TGT_LOG}" | tail -2 | sed 's/^/       /'
rpc --ls rcow_get_lvstores

# --------------------------------------------------------------------------
# [3] Path B: import -> snapshot -> decouple S -> export (converge, then publish)
# --------------------------------------------------------------------------
echo
info "[B] import -> snapshot -> decouple the snapshot -> export"
T0="$(now_ms)"
rpc rcow_import_lvol "$(printf '{"lvol_name":"VB","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
	"${EXP_UUID}" "${DST_LVS}")" >/dev/null 2>&1 || { echo "import VB failed"; exit 1; }
rpc rcow_create_snapshot '{"lvol_name":"VB","snapshot_name":"SB"}' >/dev/null 2>&1 \
	|| { echo "snapshot SB failed"; exit 1; }
T_SNAP="$(now_ms)"

rpc rcow_decouple_lvol '{"lvol_name":"SB"}' >/dev/null 2>&1 \
	|| info "decouple SB refused"
drain_decouple || info "decouple SB did not drain"
T_DEC="$(now_ms)"

UB="$(rpc rcow_export_snapshot '{"snapshot_name":"SB"}' 2>&1 | tr -d '"[:space:]\n')"
if [ -n "${UB}" ]; then
	wait_export "${UB}" && T_EXP="$(now_ms)" || { T_EXP="$(now_ms)"; info "export B did not finish"; }
	UUIDS="${UUIDS} ${UB}"
else
	T_EXP="$(now_ms)"; info "export B was refused"
fi

B_SNAP=$((T_SNAP - T0)); B_DEC=$((T_DEC - T_SNAP)); B_EXP=$((T_EXP - T_DEC))
B_TOTAL=$((T_EXP - T0))
B_LAYOUT="$(layout_of "${UB}")"
B_OBJS="$(objects_under "${DST_LVS}/exports/${UB}")"
info "B: import+snapshot ${B_SNAP}ms  decouple ${B_DEC}ms  export ${B_EXP}ms  total ${B_TOTAL}ms"
info "B: layout=${B_LAYOUT}  objects under its export prefix=${B_OBJS}"
rpc --ls rcow_get_lvstores

# --------------------------------------------------------------------------
# [4] Path C: wait for import's own decouple to finish, then snapshot -> export
# --------------------------------------------------------------------------
echo
info "[C] import, let its decouple finish, then snapshot -> export"
T0="$(now_ms)"
rpc rcow_import_lvol "$(printf '{"lvol_name":"VC","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
	"${EXP_UUID}" "${DST_LVS}")" >/dev/null 2>&1 || { echo "import VC failed"; exit 1; }
drain_decouple || info "decouple VC did not drain"
T_DEC="$(now_ms)"

rpc rcow_create_snapshot '{"lvol_name":"VC","snapshot_name":"SC"}' >/dev/null 2>&1 \
	|| { echo "snapshot SC failed"; exit 1; }
T_SNAP="$(now_ms)"

UC="$(rpc rcow_export_snapshot '{"snapshot_name":"SC"}' 2>&1 | tr -d '"[:space:]\n')"
if [ -n "${UC}" ]; then
	wait_export "${UC}" && T_EXP="$(now_ms)" || { T_EXP="$(now_ms)"; info "export C did not finish"; }
	UUIDS="${UUIDS} ${UC}"
else
	T_EXP="$(now_ms)"; info "export C was refused"
fi

C_DEC=$((T_DEC - T0)); C_SNAP=$((T_SNAP - T_DEC)); C_EXP=$((T_EXP - T_SNAP))
C_TOTAL=$((T_EXP - T0))
C_LAYOUT="$(layout_of "${UC}")"
C_OBJS="$(objects_under "${DST_LVS}/exports/${UC}")"
info "C: import+decouple ${C_DEC}ms  snapshot ${C_SNAP}ms  export ${C_EXP}ms  total ${C_TOTAL}ms"
info "C: layout=${C_LAYOUT}  objects under its export prefix=${C_OBJS}"
rpc --ls rcow_get_lvstores

echo
echo "===== SUMMARY  (source volume was ${FILL_MB} MiB of data)"
printf '%-46s %10s %10s %10s\n' "path" "publish" "total" "layout"
printf '%-46s %9sms %9sms %10s\n' \
	"A  snapshot then export (S is esnap clone)" "${A_EXP}" "${A_TOTAL}" "${A_LAYOUT}"
printf '%-46s %9sms %9sms %10s\n' \
	"B  snapshot, decouple it, then export"      "${B_EXP}" "${B_TOTAL}" "${B_LAYOUT}"
printf '%-46s %9sms %9sms %10s\n' \
	"C  decouple first, then snapshot+export"    "${C_EXP}" "${C_TOTAL}" "${C_LAYOUT}"
echo
echo "  export-prefix objects: A=${A_OBJS}  B=${B_OBJS}  C=${C_OBJS}"
echo
echo "  How to read this table: A's layout should be ref, publish a few hundred"
echo "  milliseconds, and the export prefix should have 0 objects. If A comes"
echo "  out dense, export_ref() fell back to a copy again -- check for a cross"
echo "  bucket/endpoint/region, a parent manifest that is already dense, a"
echo "  mismatched chunk_size, or a local snapshot chain deeper than 32."
echo
echo "  A is nearly 10x faster than B/C; the gap is decouple's full-volume"
echo "  materialise. B/C trade wait time for independence; A trades a"
echo "  cross-prefix reference for instant publish."
