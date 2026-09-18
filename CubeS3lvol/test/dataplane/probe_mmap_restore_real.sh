#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Real-endpoint measurement of hypervisor-shaped MAP_PRIVATE restore against an
# imported export.
#
# Cases 1-4 import with decouple=false so every fault stays on the export whole-
# object GET path. Same-bucket CopyObject decouple finishes in ~1s for a few
# dozen clusters, which races the mmap and attributes later faults to the dest
# range-GET path -- that contaminated the first run.
#
# three_lvol still uses decouple=true to exercise the production window.
#
# Expectations (1 MiB objects, GETTING=64, READY=128, prefetch window=8):
#   cold_seq_1t     whole ≈ mapped MiB, exact ≈ 0, prefetch hits > 0
#   cold_seq_16t    whole ≈ mapped MiB, exact ≈ 0, ready/coalesced reuse
#   cold_ch_vcpu_*  one synchronous fault stream per vCPU; shuffled 64 KiB
#                   guest-physical runs model startup locality without QD>1
#   cold_stampede   whole ≈ mapped MiB, coalesced ≈ whole*(threads-1)
#   cold_random_Nt  whole ≈ mapped MiB; READY=128 retains this working set
#   three_lvol      mmap succeeds while rootfs/metadata fio runs during decouple
#
# Usage:
#   sudo ./test/dataplane/probe_mmap_restore_real.sh
#   sudo SIZE_MIB=32 THREADS=16 ./test/dataplane/probe_mmap_restore_real.sh

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
BENCH_SRC="${ROOT}/test/tools/mmap_fault_bench.c"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

RUN_ID="${BASHPID}"
SRC_LVS="pmmap_src_${RUN_ID}"
DST_LVS="pmmap_dst_${RUN_ID}"
SRC_WAL=/tmp/pmmap_src_wal.img
DST_WAL=/tmp/pmmap_dst_wal.img
RPC_SOCK=/tmp/pmmap.sock
NQN="nqn.2026-08.io.spdk:pmmap"
PORT="4487"
SIZE_MIB="${SIZE_MIB:-64}"
THREADS="${THREADS:-32}"
CH_RUN_KIB="${CH_RUN_KIB:-64}"
MEMORY_GIB="${MEMORY_GIB:-1}"
IMPORT_ONLY="${IMPORT_ONLY:-0}"
IMPORT_THREADS="${IMPORT_THREADS:-${THREADS}}"
EXPORT_WAIT_SEC="${EXPORT_WAIT_SEC:-600}"
BENCH_TIMEOUT="${BENCH_TIMEOUT:-180}"
LVS_CAPACITY_GIB=$((MEMORY_GIB + 4))
# Host readahead in KiB. 0 isolates the per-fault path, which is what the
# whole-object GET cases want to measure. A production node runs
# RCOW_READ_AHEAD_KB=1024 so one fault pulls a whole chunk, so a restore
# estimate has to be taken at the production value, not at 0.
READ_AHEAD_KIB="${READ_AHEAD_KIB:-0}"
JOURNAL_MIB="${JOURNAL_MIB:-64}"
WAL_MIB="${WAL_MIB:-256}"
# Leave cache room for the whole memory volume plus slack, mirroring a
# production node whose cache region dwarfs any single volume.
WAL_IMG_MIB="${WAL_IMG_MIB:-$((JOURNAL_MIB + WAL_MIB + MEMORY_GIB * 1024 * 2 + 512))}"

WORKDIR="$(mktemp -d /tmp/pmmap.XXXXXX)"
TGT_LOG="${WORKDIR}/target.log"
OUT="${WORKDIR}/out"
mkdir -p "${OUT}"
BENCH="${OUT}/mmap_fault_bench"
RESULTS="${OUT}/results.jsonl"
: >"${RESULTS}"

TGT_PID=""
CONNECTED=0
UUIDS=""
PASS=0
FAIL=0
CASE_NO=0

rpc() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" "$@"; }
raw() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" --raw "$1" ${2:+"$2"}; }
info() { echo "---- $*"; }
pass() { PASS=$((PASS + 1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $*"; }

cleanup()
{
	set +u
	[ "${CONNECTED}" = "1" ] && nvme disconnect -n "${NQN}" >/dev/null 2>&1
	if [ -n "${TGT_PID}" ]; then
		kill "${TGT_PID}" 2>/dev/null
		sleep 2
		kill -9 "${TGT_PID}" 2>/dev/null
	fi
	rcow_load_credentials >/dev/null 2>&1 || true
	EP="$(rcow_cfg_get endpoint 2>/dev/null || true)"
	BK="$(rcow_s3_buckets 2>/dev/null | head -1 || true)"
	RG="$(rcow_cfg_get region 2>/dev/null || true)"
	if [ -n "${EP}" ] && [ -n "${BK}" ] && [ -n "${RG}" ]; then
		for p in "${SRC_LVS}" "${DST_LVS}"; do
			python3 "${PREFIX_RM}" -e "${EP}" -b "${BK}" -r "${RG}" \
				-p "${p}/" >/dev/null 2>&1 || true
		done
		for u in ${UUIDS:-}; do
			python3 "${PREFIX_RM}" -e "${EP}" -b "${BK}" -r "${RG}" \
				-p "exports/${u}" >/dev/null 2>&1 || true
		done
	fi
	rm -f "${SRC_WAL}" "${DST_WAL}" "${RPC_SOCK}"
	[ -z "${KEEP:-}" ] && rm -rf "${WORKDIR}"
	echo
	echo "===== summary: ${PASS} passed, ${FAIL} failed ====="
	echo "===== log: ${TGT_LOG} ====="
	[ "${FAIL}" -eq 0 ]
}
trap cleanup EXIT

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
		[ "$(cat "${c}/subsysnqn" 2>/dev/null)" = "${NQN}" ] && {
			basename "${c}"; return 0;
		}
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
		sleep 0.2
	done
	return 1
}

drop_caches()
{
	sync
	echo 3 >/proc/sys/vm/drop_caches
}

wait_decouple_idle()
{
	local i busy
	for i in $(seq 120); do
		busy="$(rpc rcow_get_decouple 2>/dev/null | python3 -c '
import json,sys
try:
    rows=json.load(sys.stdin)
except Exception:
    rows=[]
if isinstance(rows, dict):
    rows=rows.get("queue", [])
print(len(rows))' 2>/dev/null || echo 0)"
		[ "${busy}" = "0" ] && return 0
		sleep 0.5
	done
	return 1
}

lvstore_write_stat()
{
	local lvs_name="$1" field="$2"

	rpc rcow_get_lvstores | python3 -c '
import json, sys
rows = json.load(sys.stdin)
for row in rows:
    if row.get("lvs_name") == sys.argv[1]:
        print((row.get("write_path") or {}).get(sys.argv[2], 0))
        break
else:
    raise SystemExit("lvstore not found")
' "${lvs_name}" "${field}"
}

delete_lvol()
{
	local name="$1"
	if rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${name}")" >/dev/null 2>&1; then
		return 0
	fi
	# Decouple may still own the blob; wait, then retry.
	wait_decouple_idle || true
	rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${name}")" >/dev/null 2>&1 || true
}

parse_release_stats()
{
	local log="$1"
	python3 - "${log}" <<'PY'
import re, sys
text = open(sys.argv[1], encoding="utf-8", errors="replace").read()
# Last release line in this slice.
pat = re.compile(
    r"Releasing imported export [^:]+: "
    r"(?P<reads>\d+) read\(s\), "
    r"(?P<bytes>\d+) bytes from S3, "
    r"(?P<zeroes>\d+) served as zeroes, "
    r"(?P<whole>\d+) whole-object GET\(s\), "
    r"(?P<coalesced>\d+) coalesced read\(s\), "
    r"(?P<ready>\d+) RAM hit\(s\), "
    r"(?P<exact>\d+) exact fallback\(s\), "
    r"(?P<prefetch_gets>\d+) prefetch GET\(s\), "
    r"(?P<prefetch_hits>\d+) prefetch hit\(s\), "
    r"(?P<prefetch_ready>\d+) prefetch RAM hit\(s\), "
    r"(?P<prefetch_skip_token>\d+) prefetch token skip\(s\), "
    r"(?P<prefetch_skip_slot>\d+) prefetch slot skip\(s\), "
    r"(?P<prefetch_skip_stale>\d+) prefetch stale skip\(s\), "
    r"(?P<prefetch_skip_seq>\d+) prefetch seq skip\(s\), "
    r"(?P<prefetch_skip_full>\d+) prefetch full-demand skip\(s\), "
    r"(?P<prefetch_skip_host>\d+) prefetch host-readahead skip\(s\), "
    r"(?P<refetch>\d+) manifest refetch\(es\)"
)
matches = list(pat.finditer(text))
if not matches:
    raise SystemExit("no release counters")
# A delete can log a second empty release from a transient reopen. Prefer the
# line that actually observed I/O.
m = matches[0]
for cand in matches:
    if int(cand.group("reads")) > int(m.group("reads")):
        m = cand
print("{%s}" % ",".join(
    '"%s":%s' % (k, m.group(k))
    for k in ("reads", "bytes", "zeroes", "whole", "coalesced", "ready",
              "exact", "prefetch_gets", "prefetch_hits", "prefetch_ready",
              "prefetch_skip_token", "prefetch_skip_slot",
              "prefetch_skip_stale", "prefetch_skip_seq",
              "prefetch_skip_full", "prefetch_skip_host", "refetch")
))
PY
}

# Import one named clone of the shared export, run the mmap case, then destroy it
# so the release line is attributable to this case alone.
run_mmap_case()
{
	local name="$1" pattern="$2" threads="$3" write_pct="$4"
	local expect="$5"
	local lvol="mem_${CASE_NO}"
	local mark nsid dev result stats
local whole exact coalesced ready

	local decouple="${6:-false}"

	CASE_NO=$((CASE_NO + 1))
	info "[case ${CASE_NO}] ${name} pattern=${pattern} threads=${threads} decouple=${decouple}"

	rpc rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":%s}' \
		"${lvol}" "${MEM_UUID}" "${DST_LVS}" "${decouple}")" >/dev/null || {
		fail "${name}: import failed"; return 1; }

	nsid="$(expose "${DST_LVS}/${lvol}")"
	[ -n "${nsid}" ] || { fail "${name}: expose failed"; return 1; }
	dev="$(wait_dev "${nsid}")" || {
		unexpose "${nsid}"; fail "${name}: device missing"; return 1; }

	blockdev --setra $((READ_AHEAD_KIB * 2)) "${dev}" >/dev/null 2>&1 || true
	drop_caches
	mark="$(wc -c <"${TGT_LOG}")"

	# Bound the fault run so a dest-path regression cannot hang the probe for
	# minutes the way the first decouple=true sequential case did.
	if ! result="$(timeout "${BENCH_TIMEOUT}s" "${BENCH}" --device "${dev}" --offset-mib 0 \
			--size-mib "${SIZE_MIB}" --pattern "${pattern}" \
			--threads "${threads}" --run-kib "${CH_RUN_KIB}" \
			--write-percent "${write_pct}")"; then
		unexpose "${nsid}"
		delete_lvol "${lvol}"
		fail "${name}: mmap bench failed or timed out"
		return 1
	fi
	printf '%s\n' "${result}"

	unexpose "${nsid}"
	if [ "${decouple}" = "true" ]; then
		wait_decouple_idle || {
			fail "${name}: decouple did not finish after mmap"
			return 1
		}
	fi
	# Destroying the import releases the export bs_dev and prints counters.
	delete_lvol "${lvol}"
	# Give the async unregister path a moment to log.
	for _ in $(seq 50); do
		dd if="${TGT_LOG}" bs=1 skip="${mark}" status=none 2>/dev/null |
			grep -aq 'Releasing imported export' && break
		sleep 0.1
	done
	dd if="${TGT_LOG}" bs=1 skip="${mark}" status=none 2>/dev/null \
		>"${OUT}/${name}.log.slice" || true
	if ! stats="$(parse_release_stats "${OUT}/${name}.log.slice")"; then
		fail "${name}: missing export release counters"
		return 1
	fi
	echo "  export_stats ${stats}"

	python3 - "${name}" "${result}" "${stats}" "${expect}" "${SIZE_MIB}" \
		"${threads}" <<'PY' >>"${RESULTS}"
import json, sys
name, bench_s, stats_s, expect, size_mib, threads = sys.argv[1:7]
bench = json.loads(bench_s)
stats = json.loads(stats_s)
size_mib = int(size_mib)
threads = int(threads)
row = {"case": name, "expect": expect, "bench": bench, "export": stats}
elapsed_s = bench.get("elapsed_ms", 0) / 1000
touched = bench.get("touched_bytes", 0)
row["export_bytes_per_touched_byte"] = stats["bytes"] / touched if touched else 0
row["export_mib_per_sec"] = (
    stats["bytes"] / 1048576 / elapsed_s if elapsed_s else 0
)

whole = stats["whole"]
exact = stats["exact"]
coalesced = stats["coalesced"]
ready = stats["ready"]
ok = True
reasons = []

if expect == "seq_clean":
    # One whole GET per populated object; pages inside the object hit RAM.
    if whole < size_mib * 0.8 or whole > size_mib * 1.5:
        ok = False; reasons.append(f"whole={whole} want ~{size_mib}")
    if exact > max(2, size_mib // 8):
        ok = False; reasons.append(f"exact={exact} want near 0")
    if ready < size_mib * 100:
        ok = False; reasons.append(f"ready={ready} too low for in-object reuse")
elif expect == "seq_parallel":
    if whole < size_mib * 0.7:
        ok = False; reasons.append(f"whole={whole} too low")
    if exact > max(2, size_mib // 8):
        ok = False; reasons.append(f"exact={exact} want near 0")
    if ready < size_mib * 50:
        ok = False; reasons.append(f"ready={ready} too low")
elif expect == "ch_vcpu":
    # A small fixture can retain every object in the export LRU and fetch each
    # MiB once. A production-sized shuffled working set cannot: revisiting a
    # partially touched object after eviction legitimately causes another whole
    # GET. Record that as read amplification instead of treating it as failure.
    if whole < size_mib * 0.8:
        ok = False; reasons.append(f"whole={whole} want >= ~{size_mib}")
    if exact > max(2, size_mib // 8):
        ok = False; reasons.append(f"exact={exact} want near 0")
    if ready + coalesced < size_mib * 100:
        ok = False; reasons.append(
            f"ready+coalesced={ready + coalesced} too low")
elif expect == "ch_vcpu_live":
    # Production import: reads race decouple=true. Some faults hit the export
    # parent while later faults may hit the materialized destination, so an
    # export-only whole-GET count cannot describe or gate the mixed path.
    if bench.get("elapsed_ms", 0) <= 0:
        ok = False; reasons.append("mmap produced no timing")
elif expect == "stampede":
    if whole < size_mib * 0.8 or whole > size_mib * 1.5:
        ok = False; reasons.append(f"whole={whole} want ~{size_mib}")
    # Depending on scheduling, followers either join GETTING or arrive after
    # it became READY. Both prove one GET served multiple reads.
    if coalesced + ready < size_mib * max(1, threads - 1) * 0.5:
        ok = False; reasons.append(
            f"coalesced+ready={coalesced + ready} too low")
    if exact > size_mib // 2:
        ok = False; reasons.append(f"exact={exact} want low under stampede")
elif expect == "random_cap":
    if whole < size_mib * 0.8:
        ok = False; reasons.append(f"whole={whole} want >= ~{size_mib}")
    if whole > size_mib * 1.5:
        ok = False; reasons.append(f"whole={whole} shows READY churn")
    if exact > max(2, size_mib // 8):
        ok = False; reasons.append(f"exact={exact} want near 0")
elif expect == "three_lvol":
    if bench.get("elapsed_ms", 0) <= 0:
        ok = False; reasons.append("mmap produced no timing")

row["ok"] = ok
row["reasons"] = reasons
print(json.dumps(row, separators=(",", ":")))
if ok:
    print(f"PASS {name}", file=sys.stderr)
else:
    print(f"FAIL {name}: {'; '.join(reasons)}", file=sys.stderr)
    raise SystemExit(1)
PY
	if [ $? -eq 0 ]; then
		pass "${name}"
	else
		fail "${name}"
	fi
}

# --------------------------------------------------------------------------
[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }
[ -x "${TGT_BIN}" ] || { echo "target not built" >&2; exit 1; }
command -v nvme >/dev/null || { echo "nvme-cli required" >&2; exit 1; }
command -v fio >/dev/null || { echo "fio required" >&2; exit 1; }

rcow_load_credentials || { echo "could not load S3 credentials" >&2; exit 1; }
EP="$(rcow_cfg_get endpoint)"
BK="$(rcow_s3_buckets | head -1)"
RG="$(rcow_cfg_get region)"
[ -n "${EP}" ] && [ -n "${BK}" ] && [ -n "${RG}" ] || {
	echo "incomplete S3 config" >&2; exit 1; }
info "bucket ready; memory_gib=${MEMORY_GIB} dense_mib=${SIZE_MIB} threads=${THREADS} ch_run_kib=${CH_RUN_KIB}"
info "local device ${WAL_IMG_MIB} MiB: journal ${JOURNAL_MIB}, WAL ${WAL_MIB}, cache $((WAL_IMG_MIB - JOURNAL_MIB - WAL_MIB)) MiB"
info "host readahead ${READ_AHEAD_KIB} KiB (production default is 1024)"
[ "${SIZE_MIB}" -le $((MEMORY_GIB * 1024)) ] || {
	echo "SIZE_MIB exceeds the ${MEMORY_GIB} GiB memory lvol" >&2
	exit 1
}

pkill -9 -f s3lvol_tgt >/dev/null 2>&1 || true
sleep 2
rm -f "${RPC_SOCK}" "${SRC_WAL}" "${DST_WAL}"
# The chunk cache is whatever is left of the local device after the journal and
# the WAL, so the image size -- not a cache option -- is what decides whether the
# destination can retain a working set. A production node runs a 512 GiB image
# against a 1 GiB journal and a 32 GiB WAL, leaving ~479 GiB of cache; an image
# sized for the journal alone silently turns every revisited chunk into another
# whole-object GET.
truncate -s "${WAL_IMG_MIB}M" "${SRC_WAL}"
truncate -s "${WAL_IMG_MIB}M" "${DST_WAL}"
for p in "${SRC_LVS}" "${DST_LVS}"; do
	python3 "${PREFIX_RM}" -e "${EP}" -b "${BK}" -r "${RG}" -p "${p}/" >/dev/null 2>&1 || true
done

cc -O2 -g -Wall -Wextra -Werror -pthread "${BENCH_SRC}" -o "${BENCH}" || exit 1

info "starting target"
AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
	S3LVOL_READ_AHEAD_KB="${READ_AHEAD_KIB}" \
	"${TGT_BIN}" -m 0x3 --no-huge -s 2048 --wait-for-rpc \
	-r "${RPC_SOCK}" >"${TGT_LOG}" 2>&1 &
TGT_PID=$!
for _ in $(seq 80); do [ -S "${RPC_SOCK}" ] && break; sleep 0.25; done
[ -S "${RPC_SOCK}" ] || { echo "target failed"; tail -30 "${TGT_LOG}"; exit 1; }
raw iobuf_set_options \
	'{"large_pool_count":512,"large_bufsize":1048576}' >/dev/null ||
	{ echo "iobuf_set_options failed"; exit 1; }
raw framework_start_init >/dev/null ||
	{ echo "framework_start_init failed"; exit 1; }
sleep 1

rpc rcow_add_s3_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"}' \
	"${BK}" "${EP}" "${BK}" "${RG}")" >/dev/null || { echo "add_s3_config failed"; exit 1; }
raw bdev_aio_create "$(printf '{"filename":"%s","name":"src_wal0","block_size":4096}' "${SRC_WAL}")" \
	>/dev/null 2>&1
raw bdev_aio_create "$(printf '{"filename":"%s","name":"dst_wal0","block_size":4096}' "${DST_WAL}")" \
	>/dev/null 2>&1
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"src_wal0","journal_size_mb":%d,"wal_size_mb":%d,"force":true}' \
	"${SRC_LVS}" "${BK}" "${LVS_CAPACITY_GIB}" "${JOURNAL_MIB}" "${WAL_MIB}")" \
	>/dev/null || { echo "create src lvstore failed"; exit 1; }

raw nvmf_create_transport \
	'{"trtype":"TCP","max_io_size":1048576}' >/dev/null 2>&1
raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PMMAP0000000001"}' \
	"${NQN}")" >/dev/null 2>&1
raw nvmf_subsystem_add_listener "$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%s"}}' \
	"${NQN}" "${PORT}")" >/dev/null 2>&1

# --------------------------------------------------------------------------
info "[1] densely fill memory/rootfs/metadata templates and export them"
for name in memory rootfs metadata; do
	size_gib=1
	[ "${name}" = "memory" ] && size_gib="${MEMORY_GIB}"
	rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' "${name}" "${size_gib}")" \
		>/dev/null || { echo "create ${name}"; exit 1; }
done

NSID_MEM="$(expose "${SRC_LVS}/memory")"
nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1
CONNECTED=1
sleep 2
MEM_DEV="$(wait_dev "${NSID_MEM}")" || { echo "memory device missing"; exit 1; }

# Dense fill: every touched MiB becomes a present object in the export.
info "filling memory with ${SIZE_MIB} MiB of random data"
dd if=/dev/urandom of="${MEM_DEV}" bs=1M count="${SIZE_MIB}" oflag=direct status=none
# Smaller disposable fills for the concurrent-write companions.
NSID_ROOT="$(expose "${SRC_LVS}/rootfs")"
NSID_META="$(expose "${SRC_LVS}/metadata")"
ROOT_DEV="$(wait_dev "${NSID_ROOT}")" || exit 1
META_DEV="$(wait_dev "${NSID_META}")" || exit 1
dd if=/dev/urandom of="${ROOT_DEV}" bs=1M count=16 oflag=direct status=none
dd if=/dev/urandom of="${META_DEV}" bs=1M count=8 oflag=direct status=none
sync
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null

for name in memory rootfs metadata; do
	rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s-snap"}' \
		"${name}" "${name}")" >/dev/null || exit 1
done
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null
sleep 2

for name in memory rootfs metadata; do
	uuid="$(rpc rcow_export_snapshot "$(printf '{"snapshot_name":"%s-snap"}' "${name}")" \
		2>&1 | tr -d '"[:space:]\n')"
	[ -n "${uuid}" ] || { echo "export ${name} failed"; exit 1; }
	UUIDS="${UUIDS} ${uuid}"
	eval "UUID_${name}=${uuid}"
	info "exported ${name} as ${uuid}"
done
for name in memory rootfs metadata; do
	eval "uuid=\${UUID_${name}}"
	for _ in $(seq "${EXPORT_WAIT_SEC}"); do
		st="$(rpc rcow_get_snapshot_status "$(printf '{"export_uuid":"%s"}' "${uuid}")" \
			2>/dev/null | tr -d '"[:space:]\n')"
		case "${st}" in *DONE*) break ;; esac
		sleep 1
	done
	case "${st}" in *DONE*) ;; *) echo "export ${name} not DONE (${st})"; exit 1 ;; esac
done
pass "three dense exports reached DONE"

unexpose "${NSID_MEM}"; unexpose "${NSID_ROOT}"; unexpose "${NSID_META}"
sleep 1
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" >/dev/null || {
	echo "unload source failed"; exit 1; }
sleep 1
rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"dst_wal0","journal_size_mb":%d,"wal_size_mb":%d,"force":true}' \
	"${DST_LVS}" "${BK}" "${LVS_CAPACITY_GIB}" "${JOURNAL_MIB}" "${WAL_MIB}")" \
	>/dev/null || { echo "create dst lvstore failed"; exit 1; }
pass "destination lvstore ready"
MEM_UUID="${UUID_memory}"

# --------------------------------------------------------------------------
if [ "${IMPORT_ONLY}" = "1" ]; then
	info "[2] production fresh imports racing decouple=true"
	for import_threads in ${IMPORT_THREADS}; do
		run_mmap_case "cold_ch_vcpu_${import_threads}t" ch-vcpu \
			"${import_threads}" 0 ch_vcpu_live true
	done
else
	info "[2] per-case fresh imports on the export path (decouple=false)"
	run_mmap_case cold_seq_1t sequential 1 0 seq_clean false
	run_mmap_case cold_seq_16t sequential 16 0 seq_parallel false
	run_mmap_case cold_ch_vcpu_1t ch-vcpu 1 0 ch_vcpu false
	run_mmap_case cold_ch_vcpu_2t ch-vcpu 2 0 ch_vcpu false
	run_mmap_case cold_ch_vcpu_4t ch-vcpu 4 0 ch_vcpu false
	run_mmap_case cold_ch_vcpu_8t ch-vcpu 8 0 ch_vcpu false
	run_mmap_case cold_ch_vcpu_16t ch-vcpu 16 0 ch_vcpu false
	run_mmap_case cold_stampede stampede "${THREADS}" 0 stampede false
	run_mmap_case cold_random_cap random "${THREADS}" 0 random_cap false
fi

# --------------------------------------------------------------------------
if [ "${IMPORT_ONLY}" != "1" ]; then
info "[3] three-lvol production window: mmap memory + fio rootfs/metadata"
CASE_NO=$((CASE_NO + 1))
name=three_lvol
rpc rcow_import_lvol "$(printf '{"lvol_name":"mem_live","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
	"${UUID_memory}" "${DST_LVS}")" >/dev/null || { fail "${name}: import memory"; exit 1; }
rpc rcow_import_lvol "$(printf '{"lvol_name":"root_live","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
	"${UUID_rootfs}" "${DST_LVS}")" >/dev/null || { fail "${name}: import rootfs"; exit 1; }
rpc rcow_import_lvol "$(printf '{"lvol_name":"meta_live","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
	"${UUID_metadata}" "${DST_LVS}")" >/dev/null || { fail "${name}: import metadata"; exit 1; }

NS_MEM="$(expose "${DST_LVS}/mem_live")"
NS_ROOT="$(expose "${DST_LVS}/root_live")"
NS_META="$(expose "${DST_LVS}/meta_live")"
DEV_MEM="$(wait_dev "${NS_MEM}")" || exit 1
DEV_ROOT="$(wait_dev "${NS_ROOT}")" || exit 1
DEV_META="$(wait_dev "${NS_META}")" || exit 1
blockdev --setra 0 "${DEV_MEM}" >/dev/null 2>&1 || true
drop_caches
MARK="$(wc -c <"${TGT_LOG}")"

fio --name=rootfs --filename="${DEV_ROOT}" --direct=1 --rw=randrw --rwmixread=70 \
	--bs=4k --iodepth=16 --ioengine=libaio --size=16m --time_based=1 --runtime=20 \
	--group_reporting=1 --output-format=json --output="${OUT}/rootfs-fio.json" &
ROOT_PID=$!
fio --name=metadata --filename="${DEV_META}" --direct=1 --rw=randwrite \
	--bs=4k --iodepth=8 --ioengine=libaio --size=8m --time_based=1 --runtime=20 \
	--group_reporting=1 --output-format=json --output="${OUT}/metadata-fio.json" &
META_PID=$!

if ! result="$("${BENCH}" --device "${DEV_MEM}" --offset-mib 0 --size-mib "${SIZE_MIB}" \
		--pattern ch-vcpu --threads "${THREADS}" --run-kib "${CH_RUN_KIB}" \
		--write-percent 0)"; then
	kill "${ROOT_PID}" "${META_PID}" >/dev/null 2>&1 || true
	wait >/dev/null 2>&1 || true
	fail "${name}: mmap failed"
else
	printf '%s\n' "${result}"
	wait "${ROOT_PID}"; ROOT_RC=$?
	wait "${META_PID}"; META_RC=$?
	if [ "${ROOT_RC}" -ne 0 ] || [ "${META_RC}" -ne 0 ]; then
		fail "${name}: fio failed root=${ROOT_RC} meta=${META_RC}"
	else
		# Once decouple is idle, repeat the memory walk against the destination
		# map/cache. This is the Phase 2a case: no export and no live overlay,
		# so cache hits should stay on their submitting nvmf threads.
		wait_decouple_idle || true
		# Decouple one dense lvol by itself so its destination clusters are
		# physically sequential. The three production imports above are
		# intentionally interleaved and are not a deterministic sequence
		# detector fixture.
		rpc rcow_import_lvol "$(printf '{"lvol_name":"prefetch_live","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
			"${UUID_rootfs}" "${DST_LVS}")" >/dev/null || {
			fail "dest_prefetch_seq: fixture import failed"; exit 1; }
		wait_decouple_idle || {
			fail "dest_prefetch_seq: fixture decouple failed"; exit 1; }
		rpc rcow_flush_lvstore \
			"$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" >/dev/null || {
			fail "warm_dest: destination flush failed"
		}

		# Restart with the persistent destination still active. Cache metadata
		# is intentionally RAM-only, so the first post-restore walk is a cold
		# destination-map read and must exercise Phase 2b submit-thread fills.
		unexpose "${NS_MEM}"; unexpose "${NS_ROOT}"; unexpose "${NS_META}"
		nvme disconnect -n "${NQN}" >/dev/null 2>&1 || true
		CONNECTED=0
		# Deliberately emulate a crash. A graceful shutdown removes the active
		# registry entry and therefore cannot exercise restore with an empty
		# in-memory cache.
		kill -9 "${TGT_PID}" 2>/dev/null || true
		wait "${TGT_PID}" 2>/dev/null || true
		TGT_PID=""
		rm -f "${RPC_SOCK}"

		AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
			S3LVOL_READ_AHEAD_KB="${READ_AHEAD_KIB}" \
			"${TGT_BIN}" -m 0x3 --no-huge -s 2048 --wait-for-rpc \
			-r "${RPC_SOCK}" >>"${TGT_LOG}" 2>&1 &
		TGT_PID=$!
		for _ in $(seq 80); do [ -S "${RPC_SOCK}" ] && break; sleep 0.25; done
		[ -S "${RPC_SOCK}" ] || { fail "cold_dest: target restart failed"; exit 1; }
		raw iobuf_set_options \
			'{"large_pool_count":512,"large_bufsize":1048576}' >/dev/null ||
			{ fail "cold_dest: iobuf_set_options failed"; exit 1; }
		raw framework_start_init >/dev/null ||
			{ fail "cold_dest: framework_start_init failed"; exit 1; }
		rpc rcow_add_s3_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"}' \
			"${BK}" "${EP}" "${BK}" "${RG}")" >/dev/null || exit 1
		raw bdev_aio_create "$(printf '{"filename":"%s","name":"dst_wal0","block_size":4096}' \
			"${DST_WAL}")" >/dev/null 2>&1 || exit 1
		rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"dst_wal0","cache_bdev":"dst_wal0","force":true}' \
			"${DST_LVS}" "${BK}")" >/dev/null || {
			fail "cold_dest: destination restore failed"; exit 1; }
		rpc rcow_flush_lvstore \
			"$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" >/dev/null || {
			fail "cold_dest: replay flush failed"; exit 1; }

		raw nvmf_create_transport \
			'{"trtype":"TCP","max_io_size":1048576}' >/dev/null 2>&1
		raw nvmf_create_subsystem "$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PMMAP0000000001"}' \
			"${NQN}")" >/dev/null 2>&1
		raw nvmf_subsystem_add_listener "$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%s"}}' \
			"${NQN}" "${PORT}")" >/dev/null 2>&1
		NS_MEM="$(expose "${DST_LVS}/mem_live")"
		NS_ROOT="$(expose "${DST_LVS}/root_live")"
		NS_META="$(expose "${DST_LVS}/meta_live")"
		NS_PREFETCH="$(expose "${DST_LVS}/prefetch_live")"
		nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null 2>&1
		CONNECTED=1
		DEV_MEM="$(wait_dev "${NS_MEM}")" || exit 1
		DEV_ROOT="$(wait_dev "${NS_ROOT}")" || exit 1
		DEV_META="$(wait_dev "${NS_META}")" || exit 1
		DEV_PREFETCH="$(wait_dev "${NS_PREFETCH}")" || exit 1
		blockdev --setra 0 "${DEV_MEM}" >/dev/null 2>&1 || true
		blockdev --setra 0 "${DEV_PREFETCH}" >/dev/null 2>&1 || true

		stampede_size=$((SIZE_MIB / 4))
		[ "${stampede_size}" -gt 0 ] || stampede_size=1
		direct_size=$((SIZE_MIB / 4))
		[ "${direct_size}" -gt 0 ] || direct_size=1
		cold_seq_size=$((SIZE_MIB - stampede_size - direct_size))
		[ "${cold_seq_size}" -gt 0 ] || cold_seq_size=1
		stampede_offset=0
		stampede_starts_before="$(lvstore_write_stat "${DST_LVS}" dest_submit_fill_starts)"
		stampede_joins_before="$(lvstore_write_stat "${DST_LVS}" dest_submit_fill_joins)"
		stampede_hits_before="$(lvstore_write_stat "${DST_LVS}" dest_submit_cache_hits)"
		drop_caches
		if stampede_result="$("${BENCH}" --device "${DEV_MEM}" \
				--offset-mib "${stampede_offset}" \
				--size-mib "${stampede_size}" --pattern stampede \
				--threads "${THREADS}" --write-percent 0)"; then
			printf '%s\n' "${stampede_result}"
			stampede_starts_after="$(lvstore_write_stat "${DST_LVS}" dest_submit_fill_starts)"
			stampede_joins_after="$(lvstore_write_stat "${DST_LVS}" dest_submit_fill_joins)"
			stampede_hits_after="$(lvstore_write_stat "${DST_LVS}" dest_submit_cache_hits)"
			stampede_starts=$((stampede_starts_after - stampede_starts_before))
			stampede_joins=$((stampede_joins_after - stampede_joins_before))
			stampede_hits=$((stampede_hits_after - stampede_hits_before))
			stampede_ok=false
			if [ "${stampede_starts}" -gt 0 ] && [ "${stampede_joins}" -gt 0 ]; then
				stampede_ok=true
				pass "cold_dest_stampede: ${stampede_starts} fills, ${stampede_joins} joins"
			elif [ "${stampede_hits}" -ge $((stampede_size * THREADS)) ]; then
				stampede_ok=true
				pass "cold_dest_stampede: ${stampede_hits} reads already prefetched"
			else
				fail "cold_dest_stampede: fills=${stampede_starts} joins=${stampede_joins} hits=${stampede_hits}"
			fi
			python3 - "${stampede_result}" "${stampede_starts}" "${stampede_joins}" "${stampede_ok}" <<'PY' >>"${RESULTS}"
import json, sys
row = {"case": "cold_dest_stampede", "expect": "cold_dest_stampede",
       "bench": json.loads(sys.argv[1]), "export": {},
       "submit_fills": int(sys.argv[2]), "submit_joins": int(sys.argv[3]),
       "ok": sys.argv[4] == "true",
       "reasons": [] if sys.argv[4] == "true" else ["fill single-flight not exercised"]}
print(json.dumps(row, separators=(",", ":")))
PY
		else
			fail "cold_dest_stampede: destination fill walk failed"
		fi

		fill_before="$(lvstore_write_stat "${DST_LVS}" dest_submit_fill_starts)"
		fill_joins_before="$(lvstore_write_stat "${DST_LVS}" dest_submit_fill_joins)"
		fill_after="${fill_before}"
		fill_joins_after="${fill_joins_before}"
		drop_caches
		if cold_dest_result="$("${BENCH}" --device "${DEV_MEM}" \
				--offset-mib "$((stampede_offset + stampede_size))" \
				--size-mib "${cold_seq_size}" --pattern sequential \
				--threads "${THREADS}" --write-percent 0)"; then
			printf '%s\n' "${cold_dest_result}"
			fill_after="$(lvstore_write_stat "${DST_LVS}" dest_submit_fill_starts)"
			fill_joins_after="$(lvstore_write_stat "${DST_LVS}" dest_submit_fill_joins)"
			fill_delta=$((fill_after - fill_before))
			fill_joins_delta=$((fill_joins_after - fill_joins_before))
			cold_dest_ok=false
			if [ "${fill_delta}" -gt 0 ]; then
				cold_dest_ok=true
				pass "cold_dest: ${fill_delta} submit-thread whole GETs, ${fill_joins_delta} joins"
			else
				fail "cold_dest: no submit-thread whole GETs"
			fi
			python3 - "${cold_dest_result}" "${fill_delta}" "${cold_dest_ok}" "${fill_joins_delta}" <<'PY' >>"${RESULTS}"
import json, sys
row = {"case": "cold_dest", "expect": "cold_dest",
       "bench": json.loads(sys.argv[1]), "export": {},
       "submit_fills": int(sys.argv[2]), "ok": sys.argv[3] == "true",
       "submit_joins": int(sys.argv[4]),
       "reasons": [] if sys.argv[3] == "true" else ["no submit-thread fills"]}
print(json.dumps(row, separators=(",", ":")))
PY
		else
			fail "cold_dest: destination cache-fill walk failed"
		fi

		direct_offset=$((stampede_offset + stampede_size + cold_seq_size))
		direct_before="$(lvstore_write_stat "${DST_LVS}" dest_direct_gets)"
		direct_bytes_before="$(lvstore_write_stat "${DST_LVS}" dest_direct_get_bytes)"
		direct_hits_before="$(lvstore_write_stat "${DST_LVS}" dest_submit_cache_hits)"
		drop_caches
		if dd if="${DEV_MEM}" of=/dev/null bs=1M skip="${direct_offset}" \
				count="${direct_size}" iflag=direct status=none; then
			direct_after="$(lvstore_write_stat "${DST_LVS}" dest_direct_gets)"
			direct_bytes_after="$(lvstore_write_stat "${DST_LVS}" dest_direct_get_bytes)"
			direct_hits_after="$(lvstore_write_stat "${DST_LVS}" dest_submit_cache_hits)"
			direct_delta=$((direct_after - direct_before))
			direct_bytes_delta=$((direct_bytes_after - direct_bytes_before))
			direct_hits_delta=$((direct_hits_after - direct_hits_before))
			direct_ok=false
			if [ $((direct_delta + direct_hits_delta)) -eq "${direct_size}" ] &&
			   [ "${direct_bytes_delta}" -eq $((direct_delta * 1024 * 1024)) ]; then
				direct_ok=true
				pass "cold_dest_1m: ${direct_delta} direct GETs, ${direct_hits_delta} prefetched"
			else
				fail "cold_dest_1m: direct_gets=${direct_delta}, bytes=${direct_bytes_delta}, hits=${direct_hits_delta}"
			fi
			python3 - "${direct_delta}" "${direct_bytes_delta}" "${direct_ok}" <<'PY' >>"${RESULTS}"
import json, sys
row = {"case": "cold_dest_1m", "expect": "cold_dest_1m",
       "direct_gets": int(sys.argv[1]), "direct_bytes": int(sys.argv[2]),
       "ok": sys.argv[3] == "true",
       "reasons": [] if sys.argv[3] == "true" else ["whole reads used staging"]}
print(json.dumps(row, separators=(",", ":")))
PY
		else
			fail "cold_dest_1m: direct read failed"
		fi

		# Run prefetch assertions after the demand-path cases: read-ahead may
		# populate physically adjacent clusters owned by another lvol.
		prefetch_before="$(lvstore_write_stat "${DST_LVS}" dest_prefetch_gets)"
		if dd if="${DEV_PREFETCH}" of=/dev/null bs=4K \
				skip=$((64 * 256)) count=1 iflag=direct status=none; then
			prefetch_after_hole="$(lvstore_write_stat "${DST_LVS}" \
				dest_prefetch_gets)"
			if [ "${prefetch_after_hole}" -eq "${prefetch_before}" ]; then
				pass "dest_prefetch_holes: unallocated window issued no GET"
			else
				fail "dest_prefetch_holes: $((prefetch_after_hole - prefetch_before)) unexpected GETs"
			fi
		else
			fail "dest_prefetch_holes: sparse read failed"
		fi

		# Host readahead of at least one chunk suppresses dest software
		# prefetch; demand still fills the cache, so later 4K pages hit RAM.
		drop_caches
		prefetch_before="$(lvstore_write_stat "${DST_LVS}" dest_prefetch_gets)"
		prefetch_hits_before="$(lvstore_write_stat "${DST_LVS}" dest_prefetch_hits)"
		skip_host_before="$(lvstore_write_stat "${DST_LVS}" dest_prefetch_skip_host)"
		cache_hits_before="$(lvstore_write_stat "${DST_LVS}" dest_submit_cache_hits)"
		ram_hits_before="$(lvstore_write_stat "${DST_LVS}" cache_ram_hits)"
		if dd if="${DEV_PREFETCH}" of=/dev/null bs=4K count=$((2 * 256)) \
				iflag=direct status=none; then
			prefetch_after="$(lvstore_write_stat "${DST_LVS}" dest_prefetch_gets)"
			prefetch_delta=$((prefetch_after - prefetch_before))
			skip_host_after="$(lvstore_write_stat "${DST_LVS}" \
				dest_prefetch_skip_host)"
			if [ "${READ_AHEAD_KIB}" -ge 1024 ]; then
				if [ "${prefetch_delta}" -eq 0 ] &&
				   [ "${skip_host_after}" -gt "${skip_host_before}" ]; then
					pass "dest_prefetch_seq: host readahead suppressed software prefetch"
				else
					fail "dest_prefetch_seq: gets=${prefetch_delta} skip_host=$((skip_host_after - skip_host_before))"
				fi
			elif [ "${prefetch_delta}" -gt 0 ]; then
				pass "dest_prefetch_seq: ${prefetch_delta} low-priority whole GETs"
			elif [ "${prefetch_after}" -gt 0 ]; then
				pass "dest_prefetch_seq: range was already prefetched"
			else
				fail "dest_prefetch_seq: no prefetch GETs"
			fi
			dd if="${DEV_PREFETCH}" of=/dev/null bs=4K skip=$((2 * 256)) \
				count=$((1 * 256)) iflag=direct status=none || true
			prefetch_hits_after="$(lvstore_write_stat "${DST_LVS}" \
				dest_prefetch_hits)"
			cache_hits_after="$(lvstore_write_stat "${DST_LVS}" \
				dest_submit_cache_hits)"
			ram_hits_after="$(lvstore_write_stat "${DST_LVS}" \
				cache_ram_hits)"
			prefetch_hit_delta=$((prefetch_hits_after - prefetch_hits_before))
			cache_hit_delta=$((cache_hits_after - cache_hits_before))
			ram_hit_delta=$((ram_hits_after - ram_hits_before))
			if [ "${ram_hit_delta}" -gt 0 ]; then
				pass "dest_prefetch_hit: ${prefetch_hit_delta} joins, ${cache_hit_delta} cache hits, ${ram_hit_delta} RAM hits"
			else
				fail "dest_prefetch_hit: no RAM hit (${prefetch_hit_delta} joins, ${cache_hit_delta} cache hits)"
			fi
		else
			fail "dest_prefetch_seq: sequential read failed"
		fi

		prefetch_before="$(lvstore_write_stat "${DST_LVS}" dest_prefetch_gets)"
		skip_seq_before="$(lvstore_write_stat "${DST_LVS}" dest_prefetch_skip_seq)"
		skip_host_before="$(lvstore_write_stat "${DST_LVS}" dest_prefetch_skip_host)"
		dd if="${DEV_PREFETCH}" of=/dev/null bs=4K skip=$((12 * 256)) \
			count=1 iflag=direct status=none || true
		prefetch_after="$(lvstore_write_stat "${DST_LVS}" dest_prefetch_gets)"
		skip_seq_after="$(lvstore_write_stat "${DST_LVS}" dest_prefetch_skip_seq)"
		skip_host_after="$(lvstore_write_stat "${DST_LVS}" dest_prefetch_skip_host)"
		if [ "${READ_AHEAD_KIB}" -ge 1024 ]; then
			if [ "${prefetch_after}" -eq "${prefetch_before}" ] &&
			   [ "${skip_host_after}" -gt "${skip_host_before}" ]; then
				pass "dest_prefetch_random: host readahead skipped software window"
			else
				fail "dest_prefetch_random: gets=$((prefetch_after - prefetch_before)) skip_host=$((skip_host_after - skip_host_before))"
			fi
		elif [ "${prefetch_after}" -eq "${prefetch_before}" ] &&
		     [ "${skip_seq_after}" -gt "${skip_seq_before}" ]; then
			pass "dest_prefetch_random: jump suppressed read-ahead"
		else
			fail "dest_prefetch_random: gets=$((prefetch_after - prefetch_before)) skip_seq=$((skip_seq_after - skip_seq_before))"
		fi

		fast_hits_before="$(lvstore_write_stat "${DST_LVS}" \
			dest_submit_cache_hits)"
		drop_caches
		if warm_result="$("${BENCH}" --device "${DEV_MEM}" --offset-mib 0 \
				--size-mib "${SIZE_MIB}" --pattern sequential \
				--threads "${THREADS}" --write-percent 0)"; then
			printf '%s\n' "${warm_result}"
			fast_hits_after="$(lvstore_write_stat "${DST_LVS}" \
				dest_submit_cache_hits)"
			fast_hits_delta=$((fast_hits_after - fast_hits_before))
			submit_fills_total="$(lvstore_write_stat "${DST_LVS}" \
				dest_submit_fill_starts)"
			submit_fills=$((submit_fills_total - fill_after))
			warm_ok=false
			if [ "${fast_hits_delta}" -gt 0 ]; then
				warm_ok=true
				pass "warm_dest: ${fast_hits_delta} off-owner cache hits"
			else
				fail "warm_dest: no off-owner cache hits"
			fi
			python3 - "${warm_result}" "${fast_hits_delta}" "${warm_ok}" "${submit_fills}" <<'PY' >>"${RESULTS}"
import json, sys
row = {"case": "warm_dest", "expect": "warm_dest",
       "bench": json.loads(sys.argv[1]), "export": {},
       "fast_hits": int(sys.argv[2]), "ok": sys.argv[3] == "true",
       "submit_fills": int(sys.argv[4]),
       "reasons": [] if sys.argv[3] == "true" else ["no off-owner cache hits"]}
print(json.dumps(row, separators=(",", ":")))
PY
		else
			fail "warm_dest: destination cache walk failed"
		fi

		# Keep the memory import long enough to print release stats.
		unexpose "${NS_MEM}"; unexpose "${NS_ROOT}"; unexpose "${NS_META}"
		unexpose "${NS_PREFETCH}"
		delete_lvol mem_live
		delete_lvol root_live
		delete_lvol meta_live
		delete_lvol prefetch_live
		for _ in $(seq 50); do
			dd if="${TGT_LOG}" bs=1 skip="${MARK}" status=none 2>/dev/null |
				grep -aq 'Releasing imported export' && break
			sleep 0.1
		done
		dd if="${TGT_LOG}" bs=1 skip="${MARK}" status=none 2>/dev/null \
			>"${OUT}/${name}.log.slice" || true
		stats="$(parse_release_stats "${OUT}/${name}.log.slice" 2>/dev/null || echo '{}')"
		python3 - "${name}" "${result}" "${stats}" <<'PY' >>"${RESULTS}"
import json, sys
row = {"case": sys.argv[1], "expect": "three_lvol",
       "bench": json.loads(sys.argv[2]), "export": json.loads(sys.argv[3]),
       "ok": True, "reasons": []}
print(json.dumps(row, separators=(",", ":")))
PY
		pass "${name}: mmap + concurrent rootfs/metadata IO completed"
	fi
fi
fi

# --------------------------------------------------------------------------
# Remove the destination from the persistent registry before killing the target.
# A fixed test lvstore left there makes the next run restore a chunk map whose
# S3 prefix the previous cleanup deliberately deleted.
if ! rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" >/dev/null; then
	fail "destination lvstore unload"
fi

python3 - "${RESULTS}" <<'PY'
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1], encoding="utf-8")]
print()
print("case                  elapsed(ms)  MiB/s expMiB/s exp/t  whole  coalsc  ready  exact  pf_get pf_hit pf_ram tok_sk slot_sk seq_sk full_sk host_sk fast_hit sb_fill sb_join direct directMiB verdict")
for r in rows:
    b = r.get("bench") or {}; e = r.get("export") or {}
    elapsed = f"{b['elapsed_ms']:.1f}" if "elapsed_ms" in b else "-"
    mibps = f"{b['mib_per_sec']:.1f}" if "mib_per_sec" in b else "-"
    export_mibps = f"{r['export_mib_per_sec']:.1f}" \
                   if "export_mib_per_sec" in r else "-"
    export_ratio = f"{r['export_bytes_per_touched_byte']:.2f}" \
                   if "export_bytes_per_touched_byte" in r else "-"
    direct_mib = r.get("direct_bytes", 0) // (1024 * 1024) \
                 if "direct_bytes" in r else "-"
    print(f"{r['case']:<21} {elapsed:>10} {mibps:>6} "
          f"{export_mibps:>8} {export_ratio:>5} "
          f"{e.get('whole','-'):>6} {e.get('coalesced','-'):>6} "
          f"{e.get('ready','-'):>6} {e.get('exact','-'):>6} "
          f"{e.get('prefetch_gets','-'):>6} {e.get('prefetch_hits','-'):>6} "
          f"{e.get('prefetch_ready','-'):>6} "
          f"{e.get('prefetch_skip_token','-'):>6} "
          f"{e.get('prefetch_skip_slot','-'):>7} "
          f"{e.get('prefetch_skip_seq','-'):>6} "
          f"{e.get('prefetch_skip_full','-'):>7} "
          f"{e.get('prefetch_skip_host','-'):>7} "
          f"{r.get('fast_hits','-'):>8} "
          f"{r.get('submit_fills','-'):>7} "
          f"{r.get('submit_joins','-'):>7} "
          f"{r.get('direct_gets','-'):>6} "
          f"{direct_mib:>9} "
          f"{'PASS' if r.get('ok') else 'FAIL'}")
print()
print(f"results: {sys.argv[1]}")
PY

echo "workdir kept while script exits via trap; KEEP=1 to retain: ${WORKDIR}"
[ "${FAIL}" -eq 0 ]
