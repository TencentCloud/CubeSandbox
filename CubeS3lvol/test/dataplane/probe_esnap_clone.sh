#!/usr/bin/env bash
# Phase 1 探针 #2：从"引用式快照"克隆（clone of a reference snapshot）
#
# 形状与 probe_esnap_snapshot.sh 相同（一个 target、两个 lvstore 轮流、
# nvmf_subsystem_add_ns 暴露 bdev、/sys/class/nvme 定位设备）。
#
# 为什么要单独探一次 clone：
#
#   create_clone 的 derive_check 检查的是**被克隆的快照 S**，而不是 V ——
#   decouple 队列里排的是 V，所以 decouple_pending(S) 大概率为假，
#   clone 可能今天就能通过。真正要证的是数据正确性，而不是能否调用。
#
# 要回答：
#   1. 从 S（esnap clone V 的快照）克隆 C，当前代码会拒绝吗？
#   2. C 能否读到正确数据（C -> S -> esnap -> 源 export A）？
#   3. 写 C 之后：C 读到新数据、S 保持不变吗（跨 esnap 边界的 COW）？
#   4. C 是否零拷贝——写之前 ALLOC 为 0，只为写入的部分分配？
#   5. decouple V 之后、删除 V 之后，S 与 C 是否仍然正确？
#   6. 直接克隆 V（可写卷）是否仍被正确拒绝（应当拒绝，不是要改的行为）？
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

SRC_LVS=pclone_src
DST_LVS=pclone_dst
SRC_WAL=/tmp/pclone_src_wal.img
DST_WAL=/tmp/pclone_dst_wal.img
RPC_SOCK=/tmp/pclone.sock
TGT_LOG=/tmp/pclone_target.log
NQN="nqn.2026-08.io.spdk:pclone"
PORT="4471"
WORKDIR="$(mktemp -d /tmp/pclone.XXXXXX)"
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
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PCLONE000000001"}' \
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
# [2] import V, snapshot it (probe #1 established both work)
# --------------------------------------------------------------------------
echo
info "[2] import V (decouple:false), snapshot it as S"
IMP_OUT="$(rpc rcow_import_lvol "$(printf '{"lvol_name":"V","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${DST_LVS}")" 2>&1)"
info "import reply: ${IMP_OUT}"
SNAP_OUT="$(rpc rcow_create_snapshot '{"lvol_name":"V","snapshot_name":"S"}' 2>&1)"
info "create_snapshot reply: ${SNAP_OUT}"

# --------------------------------------------------------------------------
# [3] the question: clone C from S -- refused by today's derive_check?
# --------------------------------------------------------------------------
echo
info "[3] create_clone C from S (derive_check runs against S, not V)"
CLONE_OUT="$(rpc rcow_create_clone '{"snapshot_name":"S","clone_name":"C"}' 2>&1)"
CLONE_RC=$?
info "create_clone reply: ${CLONE_OUT}  (rc=${CLONE_RC})"

# And the case that must stay refused: cloning a writable volume.
echo
info "[3b] create_clone from V (writable) -- must be refused"
CLONE_V_OUT="$(rpc rcow_create_clone '{"snapshot_name":"V","clone_name":"CV"}' 2>&1)"
info "create_clone-from-V reply: ${CLONE_V_OUT}"

rpc --ls rcow_get_lvstores

# --------------------------------------------------------------------------
# [4] does C read A's data, and is it zero-copy?
# --------------------------------------------------------------------------
echo
info "[4] read C, and check its ALLOC"
C_ALLOC_BEFORE="$(alloc_of C)"
C_MD5="$(read_vol C)" || true
info "C ALLOC before any write = ${C_ALLOC_BEFORE:-n/a}"
info "C read = ${C_MD5}  $([ "${C_MD5}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"

# --------------------------------------------------------------------------
# [5] write to C: C must change, S must not (COW across the esnap boundary)
# --------------------------------------------------------------------------
echo
info "[5] write ${WRITE_MB} MiB into C, then compare C and S"
NEW="${WORKDIR}/newdata.bin"
dd if=/dev/urandom of="${NEW}" bs=1M count="${WRITE_MB}" status=none

# What C must read afterwards: the new bytes over the first WRITE_MB of the
# pattern. Built locally so the comparison is exact rather than "different".
EXPECT="${WORKDIR}/expect_c.bin"
cp "${PAT}" "${EXPECT}"
dd if="${NEW}" of="${EXPECT}" bs=1M conv=notrunc status=none
EXPECT_MD5="$(md5sum "${EXPECT}" | cut -d' ' -f1)"

NSID_C="$(expose "${DST_LVS}/C")"
if [ -n "${NSID_C}" ] && C_DEV="$(wait_dev "${NSID_C}")"; then
	dd if="${NEW}" of="${C_DEV}" bs=1M oflag=direct status=none
	sync
	C2_MD5="$(dd if="${C_DEV}" bs=1M count="${FILL_MB}" iflag=direct status=none | md5sum | cut -d' ' -f1)"
	info "C after write = ${C2_MD5}  $([ "${C2_MD5}" = "${EXPECT_MD5}" ] && echo "MATCH ✓ (writes landed, rest still reads A)" || echo "MISMATCH ✗")"
else
	info "C could not be exposed for writing"
	C2_MD5=""
fi
unexpose "${NSID_C}"
sleep 1

S_MD5="$(read_vol S)" || true
info "S after C was written = ${S_MD5}  $([ "${S_MD5}" = "${PAT_MD5}" ] && echo "MATCH ✓ (S untouched)" || echo "MISMATCH ✗ (C's write leaked into S)")"

C_ALLOC_AFTER="$(alloc_of C)"
info "C ALLOC after writing ${WRITE_MB} MiB = ${C_ALLOC_AFTER:-n/a}  (cluster = 1 MiB here)"
rpc --ls rcow_get_lvstores

# --------------------------------------------------------------------------
# [6] decouple V, then delete V: S and C must both survive
# --------------------------------------------------------------------------
echo
info "[6] decouple V, then delete it"
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

S_AFTER_DEC="$(read_vol S)" || true
C_AFTER_DEC="$(read_vol C)" || true
info "S after V decoupled = ${S_AFTER_DEC}  $([ "${S_AFTER_DEC}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
info "C after V decoupled = ${C_AFTER_DEC}  $([ "${C_AFTER_DEC}" = "${EXPECT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"

DEL_V="$(rpc rcow_delete_lvol '{"lvol_name":"V"}' 2>&1)"
info "delete V reply: ${DEL_V}"
rpc --ls rcow_get_lvstores

S_AFTER_DEL="$(read_vol S)" || true
C_AFTER_DEL="$(read_vol C)" || true
info "S after V deleted = ${S_AFTER_DEL}  $([ "${S_AFTER_DEL}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
info "C after V deleted = ${C_AFTER_DEL}  $([ "${C_AFTER_DEL}" = "${EXPECT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"

echo
echo "===== SUMMARY"
echo "  pattern (what A holds)      : ${PAT_MD5}"
echo "  expected C after its write  : ${EXPECT_MD5}"
echo
echo "  clone C from S              : ${CLONE_OUT}"
echo "  clone from writable V       : ${CLONE_V_OUT}"
echo "  C ALLOC before / after write: ${C_ALLOC_BEFORE:-n/a} / ${C_ALLOC_AFTER:-n/a}"
echo
echo "  C read (before write)       : ${C_MD5:-not read}"
echo "  C read (after write)        : ${C2_MD5:-not read}"
echo "  S read (after C written)    : ${S_MD5:-not read}"
echo "  S / C after V decoupled     : ${S_AFTER_DEC:-not read} / ${C_AFTER_DEC:-not read}"
echo "  S / C after V deleted       : ${S_AFTER_DEL:-not read} / ${C_AFTER_DEL:-not read}"
