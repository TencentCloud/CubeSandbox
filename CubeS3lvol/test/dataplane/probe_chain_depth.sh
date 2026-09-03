#!/usr/bin/env bash
# 快照链深的可见性探针
#
# 为什么需要它：export_build_chain() 走本地快照链，深度超过
# S3LVOL_DEFAULT_MAX_CHAIN_DEPTH(32) 时返回 -E2BIG，于是 export_snapshot **静默**
# 退化成整卷拷贝。拷贝本身是正确的退路，问题是产出的 dense export 永远不会被
# reaper 回收（reaper 只收 REF），所以代价是永久的：一条注册表条目 + 一份全卷副本。
#
# 而在此之前，什么都不会告诉你链在变长。日志里只有一行 "exporting by copying
# instead"，和一次正常导出几乎无法区分——也就是说生产上已经发生过也无从得知。
#
# 快照是用户的，不该因为链长就拒绝创建。所以做法是：软阈值(24)告警 + 链深随
# rcow_get_lvstores 上报，让控制面能在悬崖之前决策，而不是事后发现。
#
# 判据：
#   [2] chain_depth 随每次快照 +1，且 RPC 真的报出来（不是恒 0 或恒定值）
#   [3] 跨过软阈值 24 时出现告警，且 24 之前没有
#   [4] 越过硬上限 32 之后告警换成更强的措辞
#   [5] 删掉一个中间快照，链深真的下降 —— 这是告警建议的动作，必须真的有效
#   [6] 链深 32 内导出仍是 ref（零拷贝没有被这些改动碰坏）
#
# [5] 是最要紧的一条。告警让用户去删中间快照，如果那个动作其实不管用，这个告警
# 就是在指使用户做无用功。删中间快照靠的是 blobstore 把被删层 merge 进它唯一的
# clone（clone_count == 1 才可删），纯元数据、对 S3 零 IO。
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
GET_MANIFEST="${ROOT}/test/tools/s3_get_manifest.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

LVS=pchain
WAL=/tmp/pchain_wal.img
RPC_SOCK=/tmp/pchain.sock
TGT_LOG=/tmp/pchain_target.log
WORKDIR="$(mktemp -d /tmp/pchain.XXXXXX)"

# 软阈值 24、硬上限 32，与 include/s3lvol/s3_types.h 一致。
SOFT=24
HARD=32
# 建到 34 层：越过硬上限两层，好让 [4] 的强告警确实被触发。
BUILD_TO=34

TGT_PID=""
FAILED=0

rpc() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" "$@"; }
raw() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" --raw "$1" ${2:+"$2"}; }
info() { echo "---- $*"; }
ok()   { echo "     [PASS] $*"; }
bad()  { echo "     [FAIL] $*"; FAILED=$((FAILED + 1)); }
want() { if [ "$2" = "$3" ]; then ok "$1 ($2)"; else bad "$1: got '$2', want '$3'"; fi; }

# chain_depth 字段，从 rcow_get_lvstores 的 JSON 里取一个 lvol 的。
#
# --raw 输出的是 JSON-RPC 的 result 成员本身，也就是顶层直接是 lvstore 数组，
# 而不是 {"result": [...]}。
depth_of()
{
	raw rcow_get_lvstores '' 2>/dev/null | python3 -c '
import json, sys
want = sys.argv[1]
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
if isinstance(d, dict):
    d = d.get("result") or []
for lvs in d:
    for l in (lvs.get("lvols") or []):
        if l.get("name") == want:
            print(l.get("chain_depth", ""))
            sys.exit(0)
' "$1"
}

cleanup()
{
	set +u
	[ -n "${TGT_PID}" ] && { kill "${TGT_PID}" 2>/dev/null; sleep 2
		kill -9 "${TGT_PID}" 2>/dev/null; }
	rcow_load_credentials
	python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" \
		-b "$(rcow_s3_buckets|head -1)" -r "$(rcow_cfg_get region)" \
		-p "${LVS}/" >/dev/null 2>&1
	for u in ${UUIDS:-}; do
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" \
			-b "$(rcow_s3_buckets|head -1)" -r "$(rcow_cfg_get region)" \
			-p "exports/${u}" >/dev/null 2>&1
	done
	rm -f "${WAL}" "${RPC_SOCK}"
	[ -z "${KEEP:-}" ] && rm -rf "${WORKDIR}"
	echo "===== log: ${TGT_LOG}"
}
trap cleanup EXIT

pkill -9 -f s3lvol_tgt 2>/dev/null
sleep 2
rm -f "${RPC_SOCK}" "${WAL}"
truncate -s 320M "${WAL}"

rcow_load_credentials
EP="$(rcow_cfg_get endpoint)"; BK="$(rcow_s3_buckets|head -1)"; RG="$(rcow_cfg_get region)"
python3 "${PREFIX_RM}" -e "${EP}" -b "${BK}" -r "${RG}" -p "${LVS}/" >/dev/null 2>&1

AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
	"${TGT_BIN}" -m 0x3 --no-huge -s 2048 -r "${RPC_SOCK}" >"${TGT_LOG}" 2>&1 &
TGT_PID=$!
for _ in $(seq 80); do [ -S "${RPC_SOCK}" ] && break; sleep 0.25; done
[ -S "${RPC_SOCK}" ] || { echo "target failed to start"; tail -20 "${TGT_LOG}"; exit 1; }
sleep 1

rpc rcow_add_s3_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"}' \
	"${BK}" "${EP}" "${BK}" "${RG}")" >/dev/null || { echo "add_s3_config"; exit 1; }
raw bdev_aio_create "$(printf '{"filename":"%s","name":"pc_wal0","block_size":4096}' \
	"${WAL}")" >/dev/null 2>&1

echo
info "[1] 一个卷，准备把它的快照链拉长"
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"pc_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${LVS}" "${BK}")" >/dev/null || { echo "create lvstore"; exit 1; }
rpc rcow_create_lvol '{"lvol_name":"v","size_gib":1}' >/dev/null || { echo "lvol"; exit 1; }
D0="$(depth_of v)"
want "一个没有父的卷，链深为 1" "${D0}" "1"

echo
info "[2] 每建一个快照，链深应当 +1"
MARK="$(wc -l <"${TGT_LOG}")"
MISMATCH=0
SOFT_FIRST=""
for i in $(seq 1 "${BUILD_TO}"); do
	rpc rcow_create_snapshot "$(printf '{"lvol_name":"v","snapshot_name":"s%d"}' "$i")" \
		>/dev/null 2>&1 || { bad "快照 s${i} 创建失败"; break; }
	# 卷本身 + i 个快照 = i+1 层
	D="$(depth_of v)"
	EXPECT=$((i + 1))
	if [ "${D}" != "${EXPECT}" ]; then
		bad "建了 ${i} 个快照后，v 的链深是 '${D}'，应为 ${EXPECT}"
		MISMATCH=$((MISMATCH + 1))
		[ "${MISMATCH}" -ge 3 ] && break
	fi
	[ "${EXPECT}" = "${SOFT}" ] && SOFT_FIRST="$(wc -l <"${TGT_LOG}")"
done
[ "${MISMATCH}" = "0" ] && ok "链深逐级递增，一直到 $(depth_of v)"
want "最终链深" "$(depth_of v)" "$((BUILD_TO + 1))"

echo
info "[3] 软阈值 ${SOFT}：之前不该吵，到了要说"
NEW="$(tail -n "+${MARK}" "${TGT_LOG}")"
# 告警文本里带着链深，所以可以精确地问"第一次告警是在哪个深度"。
FIRST_WARN_DEPTH="$(printf '%s\n' "${NEW}" \
	| grep -ao 'is [0-9]* snapshot(s) deep' | head -1 | grep -o '[0-9]*')"
if [ -z "${FIRST_WARN_DEPTH}" ]; then
	bad "整个过程没有任何链深告警"
elif [ "${FIRST_WARN_DEPTH}" = "${SOFT}" ]; then
	ok "第一条告警恰好出现在深度 ${SOFT}"
else
	bad "第一条告警出现在深度 ${FIRST_WARN_DEPTH}，应为 ${SOFT}"
fi
# 告警必须指出可以做什么，否则运维只知道有问题、不知道怎么办。
if printf '%s' "${NEW}" | grep -q 'Deleting an intermediate snapshot'; then
	ok "并且说明了该怎么办（删中间快照）"
else
	bad "告警没有给出可执行的建议"
fi

echo
info "[4] 越过硬上限 ${HARD} 之后，措辞应当更强"
if printf '%s' "${NEW}" | grep -q "past the limit of ${HARD}"; then
	ok "$(printf '%s' "${NEW}" | grep -ao "is [0-9]* snapshot(s) deep, past the limit of ${HARD}" | head -1)"
else
	bad "越过 ${HARD} 之后没有出现更强的告警"
fi
if printf '%s' "${NEW}" | grep -q 'copy the whole volume'; then
	ok "并且点明了后果是整卷拷贝"
else
	bad "强告警没有说明后果"
fi

echo
info "[5] 删一个中间快照，链深必须真的下降"
# 这是 [3]/[4] 建议的动作。如果它不管用，那两条告警就是在指使用户做无用功。
BEFORE="$(depth_of v)"
# s2 是中间层：它有父(s1)也有子(s3)，clone_count 恰好为 1，所以可删。
DEL="$(rpc rcow_delete_lvol "$(printf '{"lvol_name":"s2","lvs_name":"%s"}' "${LVS}")" 2>&1)"
info "delete s2: ${DEL}"
sleep 2
AFTER="$(depth_of v)"
if [ -n "${AFTER}" ] && [ "${AFTER}" -lt "${BEFORE}" ] 2>/dev/null; then
	ok "删掉一个中间快照后链深 ${BEFORE} -> ${AFTER}"
else
	bad "链深没有下降（${BEFORE} -> ${AFTER:-<空>}）：告警建议的动作无效"
fi

echo
info "[6] ${HARD} 层以内导出仍应是零拷贝"
# 这些改动只增加了观测，不该影响路由。用一个浅链的快照来验。
rpc rcow_create_lvol '{"lvol_name":"w","size_gib":1}' >/dev/null 2>&1
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS}")" >/dev/null 2>&1
rpc rcow_create_snapshot '{"lvol_name":"w","snapshot_name":"ws"}' >/dev/null 2>&1
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS}")" >/dev/null 2>&1
sleep 2
EXP="$(rpc rcow_export_snapshot '{"snapshot_name":"ws"}' 2>&1 | tr -d '"[:space:]\n')"
if [ -n "${EXP}" ]; then
	UUIDS="${EXP}"
	for _ in $(seq 60); do
		ST="$(rpc rcow_get_snapshot_status "$(printf '{"export_uuid":"%s"}' "${EXP}")" \
			2>/dev/null | tr -d '"[:space:]\n')"
		case "${ST}" in *DONE*) break ;; esac
		sleep 1
	done
	LAYOUT="$(python3 "${GET_MANIFEST}" -e "${EP}" -b "${BK}" -r "${RG}" -u "${EXP}" \
		--field layout 2>/dev/null)"
	want "浅链快照的导出仍是 ref" "${LAYOUT}" "ref"
else
	bad "导出 ws 失败"
fi

echo
echo "===== SUMMARY"
if [ "${FAILED}" = "0" ]; then
	echo "  链深可查、跨软阈值告警、且告警建议的补救动作确实有效。"
else
	echo "  ${FAILED} 项失败 —— 见上方 [FAIL]"
fi
exit "${FAILED}"
