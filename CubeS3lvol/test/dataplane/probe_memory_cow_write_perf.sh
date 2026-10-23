#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Measure the pause-memory incremental write path. The default case creates a
# 16 GiB lvol, sparsely populates 2 GiB of its parent, snapshots and clones it,
# then writes 10 GiB at non-repeating random 1 MiB offsets in the clone. This
# matches a real memory image better than sequentially filling the parent and
# then overwriting the clone from LBA 0. Setup traffic is drained first.
#
# Parent-read counters are reported for diagnostics but are not a pass/fail gate:
# udev may probe the namespace while the measurement runs, producing a handful
# of legitimate reads unrelated to fio's full-cluster overwrite path.
#
# Usage:
#   sudo ./test/dataplane/probe_memory_cow_write_perf.sh
#   sudo ... --write-mib 1024 --output /tmp/memory-cow-1g
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

SIZE_MIB=10240
VOLUME_GIB=16
BASE_MIB=2048
WRITE_BS=256k
WRITE_PATTERN=ordered-sparse
PORT=4495
CPUMASK="${S3LVOL_TGT_CPUMASK:-0x3}"
OUTPUT=""
READ_AFTER_WRITE=0

usage()
{
	cat <<EOF
Usage: $0 [options]
  --write-mib N  sparse clone write amount (default: ${SIZE_MIB})
  --size-mib N   deprecated alias for --write-mib
  --volume-gib N lvol capacity and random-write address space (default: ${VOLUME_GIB})
  --base-mib N   sparse parent data written before snapshot (default: ${BASE_MIB})
  --write-bs N   clone write extent size (default: ${WRITE_BS})
  --write-pattern P
                 ordered-sparse (GPA order, default) or random
  --port N       loopback NVMe/TCP port (default: ${PORT})
  --output DIR   retained result directory
  --read-after-write
                 drain, verify the hot tail, then read the full clone once
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--size-mib|--write-mib|--volume-gib|--base-mib|--write-bs|--write-pattern|--port|--output)
		[ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 2; }
		case "$1" in
		--size-mib|--write-mib) SIZE_MIB="$2" ;;
		--volume-gib) VOLUME_GIB="$2" ;;
		--base-mib) BASE_MIB="$2" ;;
		--write-bs) WRITE_BS="$2" ;;
		--write-pattern) WRITE_PATTERN="$2" ;;
		--port) PORT="$2" ;;
		--output) OUTPUT="$2" ;;
		esac
		shift 2
		;;
	--read-after-write)
		READ_AFTER_WRITE=1
		shift
		;;
	-h|--help) usage; exit 0 ;;
	*) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
	esac
done

case "${WRITE_PATTERN}" in
ordered-sparse|random) ;;
*) echo "--write-pattern must be ordered-sparse or random" >&2; exit 2 ;;
esac

case "${SIZE_MIB}:${VOLUME_GIB}:${BASE_MIB}:${PORT}" in
*[!0-9:]*) echo "numeric options must be positive integers" >&2; exit 2 ;;
esac
[ "${SIZE_MIB}" -gt 0 ] && [ "${VOLUME_GIB}" -gt 0 ] &&
	[ "${BASE_MIB}" -gt 0 ] && [ "${PORT}" -gt 0 ] ||
	{ echo "write size, volume size and port must be positive" >&2; exit 2; }
VOLUME_MIB=$((VOLUME_GIB * 1024))
[ "${BASE_MIB}" -le "${VOLUME_MIB}" ] &&
	[ "${SIZE_MIB}" -le "${VOLUME_MIB}" ] ||
	{ echo "base and clone write amounts must fit in the volume" >&2; exit 2; }

for cmd in python3 fio nvme truncate blockdev; do
	command -v "${cmd}" >/dev/null 2>&1 ||
		{ echo "${cmd} is required" >&2; exit 1; }
done
[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }
[ -x "${TGT_BIN}" ] || { echo "target binary missing: ${TGT_BIN}" >&2; exit 1; }

RUN_ID="$(date +%Y%m%d%H%M%S)-$$"
LVS="pmemcow-${RUN_ID}"
WAL_IMG="/data/${LVS}.img"
RPC_SOCK="/tmp/${LVS}.sock"
NQN="nqn.2026-10.io.spdk:${LVS}"
[ -n "${OUTPUT}" ] || OUTPUT="/tmp/${LVS}"
mkdir -p "${OUTPUT}"
TGT_LOG="${OUTPUT}/target.log"
SUMMARY="${OUTPUT}/summary.json"

# Payload recovery now comes from the 4 GiB shm write-back ring; the disk WAL
# carries ordering metadata only, so sizing it to the measured payload would
# waste almost the entire local bdev. Keep a fixed metadata WAL and leave enough
# clean-cache space for the clone's complete address space. The extra
# 128 MiB absorbs the 4 KiB super and setup traffic from the parent.
WAL_MIB=256
CACHE_MIB=$((VOLUME_MIB + 128))
IMAGE_MIB=$((64 + WAL_MIB + CACHE_MIB))
CAPACITY_GIB=$((2 * VOLUME_GIB + 4))
LVOL_GIB="${VOLUME_GIB}"

TGT_PID=""
CONNECTED=0
CREDENTIALS_READY=0
rpc() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" "$@"; }
raw() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" --raw "$1" ${2:+"$2"}; }

cleanup()
{
	local unload_ok=1

	set +e
	[ "${CONNECTED}" -eq 0 ] || nvme disconnect -n "${NQN}" >/dev/null 2>&1
	if [ -n "${TGT_PID}" ]; then
		if [ -S "${RPC_SOCK}" ] && kill -0 "${TGT_PID}" >/dev/null 2>&1; then
			# Let the write-back ring reach WAL/S3 and unlink cleanly.
			# Killing first strands a 4 GiB shm object after this probe has
			# already deleted the only WAL image capable of recovering it.
			raw nvmf_delete_subsystem \
				"$(printf '{"nqn":"%s"}' "${NQN}")" >/dev/null 2>&1 || true
			rpc rcow_delete_lvstore \
				"$(printf '{"lvs_name":"%s"}' "${LVS}")" >/dev/null 2>&1 ||
				unload_ok=0
		fi
		kill "${TGT_PID}" >/dev/null 2>&1
		for _ in $(seq 20); do
			kill -0 "${TGT_PID}" >/dev/null 2>&1 || break
			sleep 0.1
		done
		kill -9 "${TGT_PID}" >/dev/null 2>&1 || true
		wait "${TGT_PID}" >/dev/null 2>&1 || true
	fi
	if [ "${CREDENTIALS_READY}" -eq 1 ] && [ "${unload_ok}" -eq 1 ]; then
		python3 "${PREFIX_RM}" -e "${EP}" -b "${BK}" -r "${RG}" \
			-p "${LVS}/" >/dev/null 2>&1 || true
	fi
	if [ "${unload_ok}" -eq 1 ]; then
		rm -f "${WAL_IMG}"
	else
		echo "unload failed; retaining recovery image ${WAL_IMG}" >&2
	fi
	rm -f "${RPC_SOCK}"
}
trap cleanup EXIT

stats()
{
	rpc rcow_get_lvstores | python3 -c '
import json,sys
name=sys.argv[1]
for row in json.load(sys.stdin):
    if row.get("lvs_name") == name:
        print(json.dumps(row["write_path"], separators=(",", ":")))
        raise SystemExit
raise SystemExit("lvstore stats not found")
' "${LVS}"
}

wait_drain()
{
	local deadline=$(( $(date +%s) + 1800 )) s
	while [ "$(date +%s)" -lt "${deadline}" ]; do
		s="$(stats)"
		if python3 -c 'import json,sys
s=json.loads(sys.argv[1])
raise SystemExit(0 if s["overlay_bytes"] == 0 and s["flush_in_flight"] == 0 else 1)
' "${s}"; then
			return 0
		fi
		sleep 1
	done
	return 1
}

wait_dev()
{
	local nsid="$1" deadline=$(( $(date +%s) + 30 )) c dev
	while [ "$(date +%s)" -lt "${deadline}" ]; do
		for c in /sys/class/nvme/nvme*; do
			[ -e "${c}" ] || continue
			[ "$(cat "${c}/subsysnqn" 2>/dev/null)" = "${NQN}" ] || continue
			dev="/dev/$(basename "${c}")n${nsid}"
			[ -b "${dev}" ] && { printf '%s' "${dev}"; return 0; }
		done
		sleep 0.2
	done
	return 1
}

expose()
{
	raw nvmf_subsystem_add_ns \
		"$(printf '{"nqn":"%s","namespace":{"bdev_name":"%s"}}' "${NQN}" "$1")" |
		tr -d '[:space:]'
}

echo "results: ${OUTPUT}"
echo "case: ${VOLUME_GIB} GiB lvol; ${BASE_MIB} MiB sparse parent; ${SIZE_MIB} MiB ${WRITE_PATTERN} clone write (${WRITE_BS} extents)"

rcow_load_credentials
CREDENTIALS_READY=1
EP="$(rcow_cfg_get endpoint)"
BK="$(rcow_s3_buckets | awk 'NR == 1 {print; exit}')"
RG="$(rcow_cfg_get region)"
[ -n "${EP}" ] && [ -n "${BK}" ] && [ -n "${RG}" ] ||
	{ echo "incomplete S3 configuration" >&2; exit 1; }

truncate -s "${IMAGE_MIB}M" "${WAL_IMG}"
AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" \
AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
	"${TGT_BIN}" -m "${CPUMASK}" --no-pci --no-huge -s 4096 \
	--wait-for-rpc -r "${RPC_SOCK}" >"${TGT_LOG}" 2>&1 &
TGT_PID=$!
for _ in $(seq 120); do
	[ -S "${RPC_SOCK}" ] && break
	kill -0 "${TGT_PID}" >/dev/null 2>&1 || break
	sleep 0.25
done
[ -S "${RPC_SOCK}" ] || { echo "target failed; see ${TGT_LOG}" >&2; exit 1; }

raw iobuf_set_options \
	'{"large_pool_count":512,"large_bufsize":1048576}' >/dev/null
raw framework_start_init >/dev/null
rpc rcow_add_s3_config \
	"$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"}' \
	"${BK}" "${EP}" "${BK}" "${RG}")" >/dev/null
raw bdev_aio_create \
	"$(printf '{"filename":"%s","name":"pmemcow0","block_size":4096}' "${WAL_IMG}")" \
	>/dev/null
rpc rcow_create_lvstore \
	"$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"pmemcow0","journal_size_mb":64,"wal_size_mb":%d,"cache_hot_bufs":0,"force":true}' \
	"${LVS}" "${BK}" "${CAPACITY_GIB}" "${WAL_MIB}")" >/dev/null

raw nvmf_create_transport '{"trtype":"TCP","max_io_size":1048576}' >/dev/null
raw nvmf_create_subsystem \
	"$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"PMC%013d"}' \
	"${NQN}" "$$")" >/dev/null
raw nvmf_subsystem_add_listener \
	"$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"127.0.0.1","trsvcid":"%d"}}' \
	"${NQN}" "${PORT}")" >/dev/null

rpc rcow_create_lvol \
	"$(printf '{"lvol_name":"base","size_gib":%d}' "${LVOL_GIB}")" >/dev/null
BASE_NSID="$(expose "${LVS}/base")"
nvme connect -t tcp -a 127.0.0.1 -s "${PORT}" -n "${NQN}" >/dev/null
CONNECTED=1
BASE_DEV="$(wait_dev "${BASE_NSID}")" ||
	{ echo "base namespace did not appear" >&2; exit 1; }
blockdev --setra 0 "${BASE_DEV}" >/dev/null 2>&1 || true

echo "preparing ${BASE_MIB} MiB sparse parent in a ${VOLUME_GIB} GiB lvol..."
fio --name=prepare --filename="${BASE_DEV}" --ioengine=libaio --direct=1 \
	--rw=randwrite --bs=1m --iodepth=16 --size="${VOLUME_MIB}m" \
	--io_size="${BASE_MIB}m" --randrepeat=0 \
	--group_reporting=1 --output-format=json \
	--output="${OUTPUT}/prepare.fio.json"
rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS}")" >/dev/null
wait_drain || { echo "parent did not drain" >&2; exit 1; }

rpc rcow_create_snapshot \
	'{"lvol_name":"base","snapshot_name":"base-snap"}' >/dev/null
rpc rcow_create_clone \
	'{"snapshot_name":"base-snap","clone_name":"memory-clone"}' >/dev/null
CLONE_NSID="$(expose "${LVS}/memory-clone")"
CLONE_DEV="$(wait_dev "${CLONE_NSID}")" ||
	{ echo "clone namespace did not appear" >&2; exit 1; }
blockdev --setra 0 "${CLONE_DEV}" >/dev/null 2>&1 || true

stats >"${OUTPUT}/stats.before.json"
if [ "${WRITE_PATTERN}" = "ordered-sparse" ]; then
	IOLOG="${OUTPUT}/memory-cow.iolog"
	python3 - "${CLONE_DEV}" "${VOLUME_MIB}" "${SIZE_MIB}" \
		"${WRITE_BS}" >"${IOLOG}" <<'PY'
import random
import re
import sys

device, volume_mib, write_mib, block = sys.argv[1:]
match = re.fullmatch(r"([1-9][0-9]*)([kKmM])", block)
if not match:
    raise SystemExit("ordered-sparse --write-bs must use k or m units")
scale = 1024 if match.group(2).lower() == "k" else 1024 * 1024
block_bytes = int(match.group(1)) * scale
volume_bytes = int(volume_mib) * 1024 * 1024
write_bytes = int(write_mib) * 1024 * 1024
if volume_bytes % block_bytes or write_bytes % block_bytes:
    raise SystemExit("volume and write size must be divisible by --write-bs")

total = volume_bytes // block_bytes
selected = sorted(random.Random(0xC0A).sample(
    range(total), write_bytes // block_bytes))
print("fio version 2 iolog")
print(f"{device} add")
print(f"{device} open")
for index in selected:
    print(f"{device} write {index * block_bytes} {block_bytes}")
print(f"{device} close")
PY
	fio --name=memory-cow --filename="${CLONE_DEV}" --ioengine=libaio \
		--direct=1 --iodepth=1 --read_iolog="${IOLOG}" --replay_no_stall=1 \
		--group_reporting=1 --output-format=json \
		--output="${OUTPUT}/memory-cow.fio.json"
else
	fio --name=memory-cow --filename="${CLONE_DEV}" --ioengine=libaio --direct=1 \
		--rw=randwrite --bs="${WRITE_BS}" --iodepth=1 --size="${VOLUME_MIB}m" \
		--io_size="${SIZE_MIB}m" --randrepeat=0 \
		--group_reporting=1 --output-format=json \
		--output="${OUTPUT}/memory-cow.fio.json"
fi
stats >"${OUTPUT}/stats.after.json"

if [ "${READ_AFTER_WRITE}" -eq 1 ]; then
	echo "draining before post-write reads..."
	wait_drain || { echo "clone did not drain" >&2; exit 1; }
	# Cache fills are asynchronous to the chunk-map update. The overlay drain
	# proves S3 durability; this short grace period lets the final local writes
	# publish their residency bits before measuring the hot tail.
	sleep 1
	READ_TAIL_MIB=64
	[ "${VOLUME_MIB}" -lt "${READ_TAIL_MIB}" ] && READ_TAIL_MIB="${VOLUME_MIB}"
	READ_TAIL_OFFSET=$((VOLUME_MIB - READ_TAIL_MIB))
	stats >"${OUTPUT}/stats.before-tail-read.json"
	fio --name=memory-cow-tail-read --filename="${CLONE_DEV}" \
		--ioengine=libaio --direct=1 --rw=read --bs=1m --iodepth=16 \
		--offset="${READ_TAIL_OFFSET}m" --size="${READ_TAIL_MIB}m" \
		--group_reporting=1 --output-format=json \
		--output="${OUTPUT}/tail-read.fio.json"
	stats >"${OUTPUT}/stats.after-tail-read.json"
	fio --name=memory-cow-full-read --filename="${CLONE_DEV}" \
		--ioengine=libaio --direct=1 --rw=read --bs=1m --iodepth=16 \
		--size="${VOLUME_MIB}m" --group_reporting=1 --output-format=json \
		--output="${OUTPUT}/full-read.fio.json"
	stats >"${OUTPUT}/stats.after-full-read.json"
else
	echo "draining clone before cleanup..."
	wait_drain || { echo "clone did not drain" >&2; exit 1; }
fi

python3 - "${OUTPUT}" "${SIZE_MIB}" "${VOLUME_MIB}" "${BASE_MIB}" \
	"${WRITE_BS}" "${WRITE_PATTERN}" "${READ_AFTER_WRITE}" >"${SUMMARY}" <<'PY'
import json, os, sys
out = sys.argv[1]
size = int(sys.argv[2])
volume_size = int(sys.argv[3])
base_size = int(sys.argv[4])
write_bs = sys.argv[5]
write_pattern = sys.argv[6]
read_after_write = int(sys.argv[7])
def load(name):
    with open(os.path.join(out, name), encoding="utf-8") as f:
        return json.load(f)
before, after = load("stats.before.json"), load("stats.after.json")
fio = load("memory-cow.fio.json")["jobs"][0]["write"]
def delta(key):
    return int(after.get(key, 0)) - int(before.get(key, 0))
summary = {
    "size_mib": size,
    "volume_mib": volume_size,
    "base_mib": base_size,
    "pattern": f"{write_pattern} {write_bs} writes",
    "seconds": fio["runtime"] / 1000.0,
    "mib_per_sec": fio["bw_bytes"] / 1048576.0,
    "iops": fio["iops"],
    "latency_ms_mean": fio["lat_ns"]["mean"] / 1e6,
    "dest_whole_gets": delta("dest_whole_gets"),
    "dest_direct_gets": delta("dest_direct_gets"),
    "dest_get_mib": delta("dest_direct_get_bytes") / 1048576.0,
    "wal_appends": delta("wal_appends"),
    "wal_batches": delta("wal_batches"),
    "wal_mib": delta("wal_bytes_written") / 1048576.0,
    "writeback_full_events": delta("writeback_full_events"),
    "rmw_count": delta("rmw_count"),
    "flushes_partial": delta("flushes_partial"),
    "flushes_repeated": delta("flushes_repeated"),
    "flushes_repeated_partial": delta("flushes_repeated_partial"),
    "flushed_ring_head": delta("flushed_ring_head"),
}
if read_after_write:
    before_tail = load("stats.before-tail-read.json")
    after_tail = load("stats.after-tail-read.json")
    after_full = load("stats.after-full-read.json")
    tail_fio = load("tail-read.fio.json")["jobs"][0]["read"]
    full_fio = load("full-read.fio.json")["jobs"][0]["read"]
    def stat_delta(first, last, key):
        return int(last.get(key, 0)) - int(first.get(key, 0))
    summary["tail_read"] = {
        "mib": min(volume_size, 64),
        "mib_per_sec": tail_fio["bw_bytes"] / 1048576.0,
        "cache_hits": stat_delta(before_tail, after_tail, "cache_hits"),
        "cache_disk_hits": stat_delta(
            before_tail, after_tail, "cache_disk_hits"),
        "cache_misses": stat_delta(
            before_tail, after_tail, "cache_misses"),
        "s3_gets": stat_delta(
            before_tail, after_tail, "dest_whole_gets"),
        "direct_gets": stat_delta(
            before_tail, after_tail, "dest_direct_gets"),
    }
    summary["full_read"] = {
        "mib": volume_size,
        "seconds": full_fio["runtime"] / 1000.0,
        "mib_per_sec": full_fio["bw_bytes"] / 1048576.0,
        "cache_hits": stat_delta(after_tail, after_full, "cache_hits"),
        "cache_disk_hits": stat_delta(
            after_tail, after_full, "cache_disk_hits"),
        "cache_misses": stat_delta(
            after_tail, after_full, "cache_misses"),
        "s3_gets": stat_delta(
            after_tail, after_full, "dest_whole_gets"),
        "direct_gets": stat_delta(
            after_tail, after_full, "dest_direct_gets"),
    }
print(json.dumps(summary, indent=2, sort_keys=True))
PY

python3 - "${SUMMARY}" "${S3LVOL_MIN_WRITE_MIB_S:-80}" <<'PY'
import json, sys
s=json.load(open(sys.argv[1], encoding="utf-8"))
minimum=float(sys.argv[2])
print(f"memory clone overwrite: {s['mib_per_sec']:.1f} MiB/s, "
      f"{s['seconds']:.2f} s, mean latency {s['latency_ms_mean']:.2f} ms")
print(f"parent reads: whole_gets={s['dest_whole_gets']}, "
      f"direct_gets={s['dest_direct_gets']}, get_mib={s['dest_get_mib']:.1f}")
print(f"WAL: appends={s['wal_appends']}, batches={s['wal_batches']}, "
      f"written={s['wal_mib']:.1f} MiB")
print(f"write-back full events: {s['writeback_full_events']}")
print(f"flush rounds: partial={s['flushes_partial']}, "
      f"repeated={s['flushes_repeated']}, "
      f"repeated_partial={s['flushes_repeated_partial']}, "
      f"ring_head={s['flushed_ring_head']}")
if "tail_read" in s:
    r=s["tail_read"]
    print(f"hot-tail read: {r['mib_per_sec']:.1f} MiB/s, "
          f"cache_hits={r['cache_hits']}, cache_misses={r['cache_misses']}, "
          f"S3_GETs={r['s3_gets']}")
    r=s["full_read"]
    print(f"full post-write read: {r['mib_per_sec']:.1f} MiB/s, "
          f"{r['seconds']:.2f} s, cache_hits={r['cache_hits']}, "
          f"cache_misses={r['cache_misses']}, S3_GETs={r['s3_gets']}")
print(f"details: {sys.argv[1]}")
if s["mib_per_sec"] < minimum:
    raise SystemExit(f"throughput {s['mib_per_sec']:.1f} MiB/s is below "
                     f"the {minimum:.1f} MiB/s regression floor")
PY
