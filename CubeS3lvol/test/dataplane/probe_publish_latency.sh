#!/usr/bin/env bash
# 端到端发布延迟：import -> create_snapshot -> export_snapshot 各步花多久？
#
# 起因（用户指出，实测确认）：第 1 步让 create_snapshot 变成 O(1)，但真正的
# 发布链路是 import -> create_snapshot -> export_snapshot。快照 S 拿走了
# external parent，所以 export 走到 export_build_chain 时命中
# spdk_blob_is_esnap_clone()，返回 -ENOTSUP，然后**静默退化成拷贝**
# （export_ref() -> export_copy()）。拷贝要把已分配的字节读出来再上传一遍。
#
# 也就是说 O(1) 只是从一个 RPC 挪到了下一个，端到端仍是 O(size)。
# 这个探针要把三条路径的耗时和产出 layout 摆在一起：
#
#   路径 A  import -> snapshot -> export            （S 是 esnap clone）
#   路径 B  import -> snapshot -> decouple S -> export （先收敛再发布）
#   路径 C  import(decouple 完成) -> snapshot -> export （老办法，先物化后快照）
#
# 要回答：
#   1. 路径 A 的 export 是 REF 还是 DENSE？耗时多少？
#   2. 路径 B 的 export 是 REF 吗？（若是，说明收敛后可零拷贝再发布）
#   3. 三条路径的端到端总耗时，哪一条真的更快？
#   4. S3 上各自占多少对象（存储放大）
#
# === 上面描述的是 v3 之前的状态。结论已被 manifest v3 推翻（2026-09-02 实测）===
#
#   路径          publish      total     layout   export prefix 对象数
#   A （旧）       1781ms      2292ms     dense    64
#   A （v3 后）     366ms       800ms     ref       0
#   B （v3 后）     622ms      7551ms     ref       0
#   C （v3 后）     620ms      7424ms     ref       0
#
# export_build_chain() 遇到 esnap 已不再返回 -ENOTSUP：它把 esnap id 当作
# export uuid 读出来，交给 export_ref() 在 import 注册表里找到父 manifest，
# 由 s3_export_manifest_inherit() 把父的多源表摊平进来。所以路径 A 现在是
# 零拷贝的，而且比先收敛的 B/C 快近 10 倍 —— 那两条路径付的是 decouple 的
# 全卷物化钱。这正是设计要的 O(1) publish。
#
# 这个探针只打印、不断言，留作性能口径。**layout=ref 的正式回归在
# run_derived_test.sh 的步骤 [3]**（断言 version=3 且 layout=ref）；这里的数字
# 是给"发布到底快了多少"这个问题用的。
#
# 仍会退化成拷贝的残余情况（见 docs/manifest-v3-format.md 的判定表）：
# 跨 bucket/endpoint/region、父 manifest 本身是 dense、chunk_size 不一致、
# 本地快照链深度 > 32、import 注册表缺条目。
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
# 计时与观测辅助
# --------------------------------------------------------------------------
now_ms() { date +%s%3N; }

# 一个 export 的 layout。只写在 manifest 对象里（s3_export.c:581），没有 RPC
# 报告它，所以只能把 manifest 读回来。
layout_of()
{
	[ -n "${1:-}" ] || { echo "no-uuid"; return; }
	python3 "${ROOT}/test/tools/s3_get_manifest.py" -e "${EP}" -b "${BK}" \
		-r "${RG}" -u "$1" --field layout 2>/dev/null || echo "unreadable"
}

# 某个 prefix 下的对象数，用来看存储放大。
#
# 用 wc 而不是 `grep -c . || echo 0`：grep 没匹配时退出码非零，那个 fallback
# 就会往一个已经是 "0" 的值后面再追加一行，得到一个两行的"数字"。
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
# [2] 路径 A：import -> snapshot -> export（S 仍是 esnap clone）
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
# [3] 路径 B：import -> snapshot -> decouple S -> export（先收敛再发布）
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
# [4] 路径 C：等 import 的 decouple 自己跑完，再 snapshot -> export
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
echo "  怎么读这张表：A 的 layout 应当是 ref，publish 数百毫秒，export prefix"
echo "  对象数 0。若 A 出现 dense，说明 export_ref() 又退化成了拷贝 —— 查是不是"
echo "  跨 bucket/endpoint/region、父 manifest 已是 dense、chunk_size 不一致，"
echo "  或本地快照链深度超过 32。"
echo
echo "  A 比 B/C 快近 10 倍，差的是 decouple 的全卷物化：B/C 用等待换独立性，"
echo "  A 用一条跨 prefix 的引用换即时发布。"
