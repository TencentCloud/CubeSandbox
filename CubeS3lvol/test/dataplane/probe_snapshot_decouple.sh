#!/usr/bin/env bash
# Phase 1 第 2 步探针：父快照被物化时，它的 clone 读得对吗？
#
# 这是 §7 计划里唯一没有测量支撑的假设，也是第 3 步（decouple 只读快照）的门禁。
# 上一次把未验证的假设当结论已经付过代价（§9.2），这次先测。
#
# !! 当初跑它需要两处临时补丁；**补丁现在已经是主干代码**（第 3 步），所以
#    直接跑即可。原来的补丁自检已经去掉——留着会让它拒绝在正确的代码上运行。
#
#    但那次踩的坑仍然成立，改 deps/spdk 时务必记住：顶层 make **不会**重编
#    deps/spdk，必须先在 deps/spdk 里 make。第一次跑这个探针时补丁没生效，
#    结果看起来像"物化只读快照静默无效"这个错误结论（§9.5）。
#
#    回归断言版见 test/dataplane/run_snapshot_converge_test.sh。
#
# 要回答：
#   1. decouple 一个只读快照 S 能否真的完成（不只是"没报错"——要验 S 不再是 esnap）
#   2. S 物化**期间**，clone C 持续读是否始终正确
#   3. 物化后 S / C / V 三者数据是否都对
#      （V 也是 S 的 clone：快照后 V 变成 S 的普通 clone）
#   4. 收敛是否真的达成：放掉源 export 之后三者是否仍可读
#   5. C 自己写过的 cluster 是否被物化影响（不该）
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

SRC_LVS=psdec_src
DST_LVS=psdec_dst
SRC_WAL=/tmp/psdec_src_wal.img
DST_WAL=/tmp/psdec_dst_wal.img
RPC_SOCK=/tmp/psdec.sock
TGT_LOG=/tmp/psdec_target.log
NQN="nqn.2026-08.io.spdk:psdec"
PORT="4472"
WORKDIR="$(mktemp -d /tmp/psdec.XXXXXX)"
FILL_MB=48
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
# The binary has to exist; the md_ro patch check that used to live here is gone,
# because the patch is upstream of this probe now (step 3).
if [ ! -x "${TGT_BIN}" ]; then
	echo "no target binary at ${TGT_BIN}"; exit 1
fi

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
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PSDEC0000000001"}' \
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
# [2] import V -> snapshot S -> clone C；再往 C 写一段，使 C 自己也有 cluster
# --------------------------------------------------------------------------
echo
info "[2] import V, snapshot S, clone C"
rpc rcow_import_lvol "$(printf '{"lvol_name":"V","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP_UUID}" "${DST_LVS}")" >/dev/null 2>&1 || { echo "import failed"; exit 1; }
rpc rcow_create_snapshot '{"lvol_name":"V","snapshot_name":"S"}' >/dev/null 2>&1 \
	|| { echo "snapshot failed"; exit 1; }
rpc rcow_create_clone '{"snapshot_name":"S","clone_name":"C"}' >/dev/null 2>&1 \
	|| { echo "clone failed"; exit 1; }

# C 写入自己的一段，使它同时拥有"自有 cluster"和"经 S 继承的 cluster"
NEW="${WORKDIR}/newdata.bin"
dd if=/dev/urandom of="${NEW}" bs=1M count="${WRITE_MB}" status=none
EXPECT="${WORKDIR}/expect_c.bin"
cp "${PAT}" "${EXPECT}"
dd if="${NEW}" of="${EXPECT}" bs=1M conv=notrunc status=none
EXPECT_MD5="$(md5sum "${EXPECT}" | cut -d' ' -f1)"

NSID_C="$(expose "${DST_LVS}/C")"
C_DEV="$(wait_dev "${NSID_C}")" || { echo "C never appeared"; exit 1; }
dd if="${NEW}" of="${C_DEV}" bs=1M oflag=direct status=none
sync
info "C device ${C_DEV} (kept exposed for the duration)"
rpc --ls rcow_get_lvstores

# --------------------------------------------------------------------------
# [3] 后台持续读 C，然后 decouple 只读快照 S
# --------------------------------------------------------------------------
echo
info "[3] start a reader loop on C, then decouple S"
READLOG="${WORKDIR}/creads.txt"
(
	while [ ! -f "${WORKDIR}/stop" ]; do
		m="$(dd if="${C_DEV}" bs=1M count="${FILL_MB}" iflag=direct status=none 2>/dev/null \
			| md5sum | cut -d' ' -f1)"
		echo "${m}" >> "${READLOG}"
	done
) &
READER_PID=$!

DEC_S="$(rpc rcow_decouple_lvol '{"lvol_name":"S"}' 2>&1)"
info "decouple S -> ${DEC_S}"

for i in $(seq 600); do
	busy="$(rpc rcow_get_decouple 2>/dev/null | python3 -c '
import json,sys
try: rows=json.load(sys.stdin)
except Exception: rows=[]
if isinstance(rows, dict): rows=rows.get("queue", [])
print(len(rows))' 2>/dev/null)"
	[ "${busy}" = "0" ] && break
	sleep 1
done
info "decouple queue drained after ${i}s"

touch "${WORKDIR}/stop"
wait "${READER_PID}" 2>/dev/null
READS="$(wc -l < "${READLOG}" 2>/dev/null || echo 0)"
BAD="$(grep -cv "^${EXPECT_MD5}$" "${READLOG}" 2>/dev/null || echo 0)"
info "C was read ${READS} time(s) during the decouple; ${BAD} mismatch(es)"
if [ "${BAD}" != "0" ]; then
	info "distinct wrong values:"
	grep -v "^${EXPECT_MD5}$" "${READLOG}" | sort -u | sed 's/^/       /' | head -5
fi

# --------------------------------------------------------------------------
# [4] 物化后：三者数据 + S 是否真的脱离了 export
# --------------------------------------------------------------------------
echo
info "[4] after materialisation"
C_AFTER="$(dd if="${C_DEV}" bs=1M count="${FILL_MB}" iflag=direct status=none | md5sum | cut -d' ' -f1)"
unexpose "${NSID_C}"; sleep 1
S_AFTER="$(read_vol S)" || true
V_AFTER="$(read_vol V)" || true
info "C = ${C_AFTER}  $([ "${C_AFTER}" = "${EXPECT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
info "S = ${S_AFTER}  $([ "${S_AFTER}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
info "V = ${V_AFTER}  $([ "${V_AFTER}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
rpc --ls rcow_get_lvstores

# S 若真的脱离了，再次 decouple 必须报 EINVAL（不再是 esnap clone）
REDEC="$(rpc rcow_decouple_lvol '{"lvol_name":"S"}' 2>&1)"
info "decouple S again -> ${REDEC}   (Invalid argument = 已脱离 export，收敛成功)"

# --------------------------------------------------------------------------
# [5] 真正的收敛证明：放掉源 export，三者仍须可读
# --------------------------------------------------------------------------
echo
info "[5] delete A's objects from S3, reload the lvstore, then re-read"
# release_export is not usable here: it runs on the *source* lvstore, which was
# unloaded so the destination could be created. Deleting the source prefix
# outright is the stronger test anyway -- if the bytes are gone from S3 and the
# reads still succeed, S really does hold them locally.
#
# Unload and re-attach afterwards so nothing can be served from an in-memory
# chunk cache: the answer has to come from what is actually persisted.
REL="$(python3 "${PREFIX_RM}" -e "${EP}" -b "${BK}" -r "${RG}" -p "${SRC_LVS}/" 2>&1 | tail -1)"
info "deleted prefix ${SRC_LVS}/ -> ${REL}"

rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" >/dev/null 2>&1
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" >/dev/null 2>&1 \
	&& info "unloaded ${DST_LVS}" || info "unload failed"
sleep 2
rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"dst_wal0"}' \
	"${DST_LVS}" "${BK}")" >/dev/null 2>&1 \
	&& info "re-attached ${DST_LVS}" || { info "RE-ATTACH FAILED"; }
sleep 2
rpc --ls rcow_get_lvstores

NSID_C2="$(expose "${DST_LVS}/C")"
C_DEV2="$(wait_dev "${NSID_C2}")" || C_DEV2=""
if [ -n "${C_DEV2}" ]; then
	C_REL="$(dd if="${C_DEV2}" bs=1M count="${FILL_MB}" iflag=direct status=none | md5sum | cut -d' ' -f1)"
else
	C_REL="NO_DEV"
fi
unexpose "${NSID_C2}"; sleep 1
S_REL="$(read_vol S)" || true
V_REL="$(read_vol V)" || true
info "C = ${C_REL}  $([ "${C_REL}" = "${EXPECT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
info "S = ${S_REL}  $([ "${S_REL}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"
info "V = ${V_REL}  $([ "${V_REL}" = "${PAT_MD5}" ] && echo "MATCH ✓" || echo "MISMATCH ✗")"

echo
echo "===== SUMMARY"
echo "  pattern (A holds)            : ${PAT_MD5}"
echo "  expected C (pattern+its write): ${EXPECT_MD5}"
echo
echo "  decouple S (read-only snap)  : ${DEC_S}"
echo "  reads of C during it         : ${READS}, mismatches: ${BAD}"
echo "  decouple S again             : ${REDEC}"
echo "  source prefix deleted        : ${REL}"
echo
echo "                                 after materialise / after A was erased"
echo "  C                            : ${C_AFTER:-n/a} / ${C_REL:-n/a}"
echo "  S                            : ${S_AFTER:-n/a} / ${S_REL:-n/a}"
echo "  V                            : ${V_AFTER:-n/a} / ${V_REL:-n/a}"
