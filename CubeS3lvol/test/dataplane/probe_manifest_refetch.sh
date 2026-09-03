#!/usr/bin/env bash
# 重取探针：源端物化后，导入端的读能否透明恢复
#
# 要验证的是 B 方案 —— 404 不向上报错，而是挂起该 I/O、重取 manifest、
# 换用新的、再重试一次。客户端应当只看到一次变慢的读，而不是一次 I/O 错误。
#
# 场景是手工造的，因为源端的物化路径（第 5 步的写侧）还没接通。这里用
# 三步模拟"源端搬走了对象"之后的 S3 状态：
#
#   1. 正常导入一个 ref export，读一遍确认数据对；
#   2. 把 <src-lvs>/data/ 下的对象复制到另一个 prefix，发布一份
#      generation+1、source.prefix 指向新位置的 manifest，再删掉原对象；
#   3. 再读一遍。旧 manifest 指向的键已经不存在，所以每个 chunk 都会 404。
#
# 为什么是"换 prefix"而不是"改成 dense"：dense 会改变 manifest 必须携带的
# 字段和 crc 的分段方式，手工伪造的多半会被 parse() 拒掉 —— 那样探针失败在
# 自己的伪造上，对重取路径什么也没证明。换 prefix 保持了全部校验和与不变量，
# 同时让旧 manifest 里的每个键都 404，这正是要测的条件。
#
# 判据有两条，缺一不可：
#   - 读**成功**且 md5 与写入一致（B 方案生效，而不是把错误抛上去）；
#   - 日志里出现且只出现一次 refetch（去重生效 —— 一次大读会拆成很多
#     chunk GET，物化后它们同时 404，每个都触发一次重取的话就是一串
#     重复的 manifest GET 和几次互相竞争的 swap）。
#
# 注意：这里不 unload/reload lvstore。重取是内存里的行为，重启会重新
# 读 manifest 从而绕过它 —— 那样测的就不是这条路径了。
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

SRC_LVS=prefetch_src
DST_LVS=prefetch_dst
SRC_WAL=/tmp/prefetch_src_wal.img
DST_WAL=/tmp/prefetch_dst_wal.img
RPC_SOCK=/tmp/prefetch.sock
TGT_LOG=/tmp/prefetch_target.log
NQN="nqn.2026-09.io.spdk:prefetch"
PORT="4474"
WORKDIR="$(mktemp -d /tmp/prefetch.XXXXXX)"
FILL_MB=8

TGT_PID=""
CONNECTED=0
UUIDS=""
FAILED=0

rpc() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" "$@"; }
raw() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" --raw "$1" ${2:+"$2"}; }
info() { echo "---- $*"; }
ok()   { echo "     [PASS] $*"; }
bad()  { echo "     [FAIL] $*"; FAILED=$((FAILED + 1)); }
want() { if [ "$2" = "$3" ]; then ok "$1 ($2)"; else bad "$1: got '$2', want '$3'"; fi; }

cleanup()
{
	set +u
	[ "${CONNECTED}" = "1" ] && nvme disconnect -n "${NQN}" >/dev/null 2>&1
	[ -n "${TGT_PID}" ] && { kill "${TGT_PID}" 2>/dev/null; sleep 2
		kill -9 "${TGT_PID}" 2>/dev/null; }
	rcow_load_credentials
	for p in "${SRC_LVS}" "${SRC_LVS}-moved" "${DST_LVS}"; do
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" \
			-b "$(rcow_s3_buckets|head -1)" -r "$(rcow_cfg_get region)" \
			-p "${p}/" >/dev/null 2>&1
	done
	for u in ${UUIDS:-}; do
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" \
			-b "$(rcow_s3_buckets|head -1)" -r "$(rcow_cfg_get region)" \
			-p "exports/${u}" >/dev/null 2>&1
	done
	rm -f "${SRC_WAL}" "${DST_WAL}" "${RPC_SOCK}"
	[ -z "${KEEP:-}" ] && rm -rf "${WORKDIR}"
	echo "===== log: ${TGT_LOG}"
}
trap cleanup EXIT

pkill -9 -f s3lvol_tgt 2>/dev/null
sleep 2
rm -f "${RPC_SOCK}" "${SRC_WAL}" "${DST_WAL}"
truncate -s 320M "${SRC_WAL}"
truncate -s 320M "${DST_WAL}"

rcow_load_credentials
EP="$(rcow_cfg_get endpoint)"; BK="$(rcow_s3_buckets|head -1)"; RG="$(rcow_cfg_get region)"
for p in "${SRC_LVS}" "${DST_LVS}"; do
	python3 "${PREFIX_RM}" -e "${EP}" -b "${BK}" -r "${RG}" -p "${p}/" >/dev/null 2>&1
done

AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
	"${TGT_BIN}" -m 0x3 --no-huge -s 2048 -r "${RPC_SOCK}" >"${TGT_LOG}" 2>&1 &
TGT_PID=$!
for _ in $(seq 80); do [ -S "${RPC_SOCK}" ] && break; sleep 0.25; done
[ -S "${RPC_SOCK}" ] || { echo "target failed to start"; tail -20 "${TGT_LOG}"; exit 1; }
sleep 1

rpc rcow_add_s3_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"}' \
	"${BK}" "${EP}" "${BK}" "${RG}")" >/dev/null || { echo "add_s3_config"; exit 1; }
raw bdev_aio_create "$(printf '{"filename":"%s","name":"src_wal0","block_size":4096}' \
	"${SRC_WAL}")" >/dev/null 2>&1
raw bdev_aio_create "$(printf '{"filename":"%s","name":"dst_wal0","block_size":4096}' \
	"${DST_WAL}")" >/dev/null 2>&1
raw nvmf_create_transport '{"trtype":"TCP"}' >/dev/null 2>&1
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PREFETCH0000001"}' \
	"${NQN}")" >/dev/null 2>&1
raw nvmf_subsystem_add_listener "$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%s"}}' \
	"${NQN}" "${PORT}")" >/dev/null 2>&1

expose() { raw nvmf_subsystem_add_ns "$(printf '{"nqn":"%s","namespace":{"bdev_name":"%s"}}' \
	"${NQN}" "$1")" 2>/dev/null | tr -d '[:space:]'; }
unexpose() { [ -n "$1" ] && raw nvmf_subsystem_remove_ns \
	"$(printf '{"nqn":"%s","nsid":%s}' "${NQN}" "$1")" >/dev/null 2>&1; return 0; }
ctrl_of_nqn() { local c; for c in /sys/class/nvme/nvme*; do
	[ "$(cat "${c}/subsysnqn" 2>/dev/null)" = "${NQN}" ] && { basename "${c}"; return 0; }
	done; return 1; }
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

rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"src_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${SRC_LVS}" "${BK}")" >/dev/null || { echo "create src"; exit 1; }

echo
info "[1] source: write, snapshot, export"
rpc rcow_create_lvol '{"lvol_name":"v","size_gib":1}' >/dev/null || { echo "lvol"; exit 1; }
NSID="$(expose "${SRC_LVS}/v")"
nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1
CONNECTED=1
sleep 2
DEV="$(wait_dev "${NSID}")" || { echo "src device"; exit 1; }

PAT="${WORKDIR}/pat.bin"
dd if=/dev/urandom of="${PAT}" bs=1M count="${FILL_MB}" status=none
PAT_MD5="$(md5sum "${PAT}" | cut -d' ' -f1)"
dd if="${PAT}" of="${DEV}" bs=1M count="${FILL_MB}" oflag=direct conv=fsync status=none
sync
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
rpc rcow_create_snapshot '{"lvol_name":"v","snapshot_name":"s"}' >/dev/null || exit 1
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
sleep 3

EXP="$(rpc rcow_export_snapshot '{"snapshot_name":"s"}' 2>&1 | tr -d '"[:space:]\n')"
[ -n "${EXP}" ] || { echo "export"; exit 1; }
UUIDS="${EXP}"
for _ in $(seq 90); do
	st="$(rpc rcow_get_snapshot_status "$(printf '{"export_uuid":"%s"}' "${EXP}")" \
		2>/dev/null | tr -d '"[:space:]\n')"
	case "${st}" in *DONE*) break ;; esac
	sleep 1
done
info "export ${EXP}"

unexpose "${NSID}"; sleep 2
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null 2>&1
sleep 2
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":4,"wal_bdev":"dst_wal0","journal_size_mb":64,"wal_size_mb":128,"force":true}' \
	"${DST_LVS}" "${BK}")" >/dev/null || { echo "create dst"; exit 1; }

echo
info "[2] import and read once through the ref manifest"
rpc rcow_import_lvol "$(printf '{"lvol_name":"i","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
	"${EXP}" "${DST_LVS}")" >/dev/null 2>&1 || { echo "import"; exit 1; }
NSID="$(expose "${DST_LVS}/i")"
sleep 2
DEV="$(wait_dev "${NSID}")" || { echo "dst device"; exit 1; }
sync; echo 3 >/proc/sys/vm/drop_caches 2>/dev/null || true
want "the import reads correctly to begin with" \
	"$(dd if="${DEV}" bs=1M count="${FILL_MB}" iflag=direct status=none | md5sum | cut -d' ' -f1)" \
	"${PAT_MD5}"

echo
info "[3] simulating the source materialising it"
# Copy the chunks to the dense layout's keys, publish a dense manifest one
# generation newer, and delete the originals -- which is the S3 state a
# materialisation leaves behind.
python3 - <<PY >"${WORKDIR}/materialise.log" 2>&1
import json, os, sys
sys.path.insert(0, "${ROOT}/test/tools")
# Two Clients with the same name and different capabilities: s3_prefix_rm's can
# list and knows _base_path(), s3_bucket's is the only one whose request() takes
# a body. Both are used rather than extending either, since this is a probe.
from s3_prefix_rm import Client as RmClient
from s3_bucket import Client as PutClient, path_for

ak = os.environ["AWS_ACCESS_KEY_ID"]
sk = os.environ["AWS_SECRET_ACCESS_KEY"]
rm = RmClient("${EP}", "${BK}", "${RG}", False, ak, sk)
put = PutClient("${EP}", "${BK}", "${RG}", False, ak, sk)
base = rm._base_path()

def get(key):
    st, body = rm.request("GET", base + "/" + key)
    return st, body

def put_obj(key, data):
    return put.request("PUT", path_for("${BK}", False, "/" + key), body=data)[0]

st, body = get("exports/${EXP}.json")
assert st == 200, "manifest GET %d" % st
m = json.loads(body)
print("layout=%s generation=%s chunks=%s present=%s" %
      (m.get("layout"), m.get("generation"), m.get("num_chunks"),
       m.get("present_chunks")))
assert m["layout"] == "ref", "expected a ref export to start from"

# The objects move to a *different prefix*, and the manifest keeps its ref
# layout with source.prefix pointing at the new one.
#
# Not rewritten as dense, deliberately. Dense changes which fields the manifest
# must carry and how the crc is staged, so a hand-built one would most likely be
# rejected by parse() -- and a probe that fails on its own forgery proves
# nothing about the refetch. Moving the prefix keeps every checksum and every
# invariant intact while still making every key in the old manifest 404, which
# is the condition being tested.
NEW_PREFIX = "${SRC_LVS}-moved"
src_keys = list(rm.list_keys("${SRC_LVS}/data/"))
print("source data objects: %d" % len(src_keys))
assert src_keys, "no source objects to move"

for k in src_keys:
    st, data = get(k)
    assert st == 200, "GET %s -> %d" % (k, st)
    dst = NEW_PREFIX + "/data/" + k.rsplit("/", 1)[1]
    st = put_obj(dst, data)
    assert st in (200, 204), "PUT %s -> %d" % (dst, st)
print("copied %d object(s) to %s/data/" % (len(src_keys), NEW_PREFIX))

m["source"]["prefix"] = NEW_PREFIX
m["generation"] = int(m.get("generation", 0)) + 1
st = put_obj("exports/${EXP}.json", json.dumps(m).encode())
assert st in (200, 204), "manifest PUT %d" % st
print("published generation %d pointing at %s" % (m["generation"], NEW_PREFIX))

for k in src_keys:
    rm.delete(k)
print("deleted %d original object(s)" % len(src_keys))
PY
if [ $? -ne 0 ]; then
	bad "could not simulate the materialisation"
	sed 's/^/       /' "${WORKDIR}/materialise.log"
	echo; echo "===== SUMMARY"; echo "  ${FAILED} 项失败"; exit "${FAILED}"
fi
sed 's/^/       /' "${WORKDIR}/materialise.log"
ok "the source's objects are gone and a newer manifest is published"

echo
info "[4] reading again: every chunk now 404s against the held manifest"
MARK="$(wc -l <"${TGT_LOG}")"
sync; echo 3 >/proc/sys/vm/drop_caches 2>/dev/null || true
GOT="$(dd if="${DEV}" bs=1M count="${FILL_MB}" iflag=direct status=none 2>"${WORKDIR}/read.err" \
	| md5sum | cut -d' ' -f1)"

# The whole point of B: the read succeeds rather than reporting an error.
want "the read still returns the right data" "${GOT}" "${PAT_MD5}"

NEW="$(tail -n "+${MARK}" "${TGT_LOG}")"
if printf '%s' "${NEW}" | grep -q 'refetching the manifest'; then
	ok "it went through a refetch"
else
	bad "no refetch in the log; the 404 was not recognised"
fi
# One large read splits into many chunk GETs and they all miss at once, so this
# is what says the dedup works rather than issuing one manifest GET per miss.
NREF="$(printf '%s\n' "${NEW}" | grep -c 'refetching the manifest' || true)"
want "and only one, however many chunks missed" "${NREF}" "1"

if printf '%s' "${NEW}" | grep -q 'now reading generation'; then
	ok "$(printf '%s' "${NEW}" | grep -o 'now reading generation.*' | head -1)"
else
	bad "the manifest was never swapped"
fi

echo
info "[5] and it stays on the new manifest"
MARK="$(wc -l <"${TGT_LOG}")"
sync; echo 3 >/proc/sys/vm/drop_caches 2>/dev/null || true
want "a second read is still correct" \
	"$(dd if="${DEV}" bs=1M count="${FILL_MB}" iflag=direct status=none | md5sum | cut -d' ' -f1)" \
	"${PAT_MD5}"
NREF2="$(tail -n "+${MARK}" "${TGT_LOG}" | grep -c 'refetching the manifest' || true)"
want "with no further refetch" "${NREF2}" "0"

echo
echo "===== SUMMARY"
if [ "${FAILED}" = "0" ]; then
	echo "  物化后读透明恢复：404 触发一次重取，换用新 manifest，数据正确。"
else
	echo "  ${FAILED} 项失败 —— 见上方 [FAIL]"
fi
exit "${FAILED}"
