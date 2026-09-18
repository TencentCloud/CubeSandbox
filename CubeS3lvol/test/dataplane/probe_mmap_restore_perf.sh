#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Hypervisor-shaped restore benchmark for an already activated imported memory
# lvol.  The helper maps the block device MAP_PRIVATE | MAP_NORESERVE and faults
# 4 KiB pages without MAP_POPULATE, matching create_ram_region(snap_file).
#
# This is deliberately a probe rather than a run_all.sh test: timings depend on
# the S3 endpoint and dropping the host page cache is machine-wide.
#
# The memory device is never modified.  --concurrent-writes is opt-in because
# its fio jobs DO modify the supplied disposable rootfs and metadata devices.
#
# Typical production-window setup:
#   1. import memory/rootfs/metadata with decouple=true and activate immediately
#   2. run this script before rcow_get_decouple drains
#   3. deactivate the imports so s3_export_bs_dev prints its GET/LRU counters
#
# Usage:
#   sudo ./test/dataplane/probe_mmap_restore_perf.sh \
#       --memory-dev /dev/nvmeXnY [--size-mib 64] [--output DIR]
#
#   sudo ... --memory-dev /dev/... --rootfs-dev /dev/... \
#       --metadata-dev /dev/... --concurrent-writes

set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
SOURCE="${ROOT}/test/tools/mmap_fault_bench.c"

MEMORY_DEV=""
ROOTFS_DEV=""
METADATA_DEV=""
SIZE_MIB=64
THREADS=32
OUTPUT=""
TARGET_LOG=""
CONCURRENT_WRITES=0
KEEP_BIN=0
READ_AHEAD=0
ORIGINAL_READ_AHEAD=""
ROOT_PID=""
META_PID=""

usage()
{
	sed -n '3,25p' "${BASH_SOURCE[0]}"
	echo
	echo "Options:"
	echo "  --threads N           mmap faulting threads (default: 32)"
	echo "  --read-ahead N        block read-ahead sectors (default: 0)"
	echo "  --target-log PATH     save matching export release statistics"
	echo "  --keep-bin            retain the compiled helper in output dir"
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--memory-dev|--rootfs-dev|--metadata-dev|--size-mib|--threads|--read-ahead|--output|--target-log)
		[ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 2; }
		case "$1" in
		--memory-dev) MEMORY_DEV="$2" ;;
		--rootfs-dev) ROOTFS_DEV="$2" ;;
		--metadata-dev) METADATA_DEV="$2" ;;
		--size-mib) SIZE_MIB="$2" ;;
		--threads) THREADS="$2" ;;
		--read-ahead) READ_AHEAD="$2" ;;
		--output) OUTPUT="$2" ;;
		--target-log) TARGET_LOG="$2" ;;
		esac
		shift 2
		;;
	--concurrent-writes) CONCURRENT_WRITES=1; shift ;;
	--keep-bin) KEEP_BIN=1; shift ;;
	-h|--help) usage; exit 0 ;;
	*) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
	esac
done

[ "$(id -u)" -eq 0 ] || { echo "must run as root (page-cache reset)" >&2; exit 1; }
for cmd in "${CC:-cc}" python3 blockdev; do
	command -v "${cmd}" >/dev/null 2>&1 ||
		{ echo "${cmd} is required" >&2; exit 1; }
done
[ -n "${MEMORY_DEV}" ] || { echo "--memory-dev is required" >&2; exit 2; }
[ -b "${MEMORY_DEV}" ] || { echo "${MEMORY_DEV} is not a block device" >&2; exit 1; }
case "${SIZE_MIB}:${THREADS}:${READ_AHEAD}" in
*[!0-9:]*|:*|*:) echo "size and threads must be positive integers" >&2; exit 2 ;;
esac
[ "${SIZE_MIB}" -gt 0 ] && [ "${THREADS}" -gt 0 ] || {
	echo "size and threads must be positive" >&2; exit 2; }

if [ "${CONCURRENT_WRITES}" -eq 1 ]; then
	[ -b "${ROOTFS_DEV}" ] && [ -b "${METADATA_DEV}" ] || {
		echo "--concurrent-writes requires two disposable block devices" >&2
		exit 2
	}
	command -v fio >/dev/null 2>&1 || {
		echo "fio is required for --concurrent-writes" >&2
		exit 1
	}
fi

if [ -z "${OUTPUT}" ]; then
	OUTPUT="$(mktemp -d /tmp/s3lvol_mmap_perf.XXXXXX)"
else
	mkdir -p "${OUTPUT}"
fi
RESULTS="${OUTPUT}/results.jsonl"
BIN="${OUTPUT}/mmap_fault_bench"
LOG_MARK=0

cleanup()
{
	for pid in "${ROOT_PID}" "${META_PID}"; do
		[ -n "${pid}" ] || continue
		kill "${pid}" >/dev/null 2>&1 || true
		wait "${pid}" >/dev/null 2>&1 || true
	done
	if [ -n "${ORIGINAL_READ_AHEAD}" ]; then
		blockdev --setra "${ORIGINAL_READ_AHEAD}" "${MEMORY_DEV}" \
			>/dev/null 2>&1 || true
	fi
	if [ "${KEEP_BIN}" -eq 0 ]; then
		rm -f "${BIN}"
	fi
}
trap cleanup EXIT

if [ -n "${TARGET_LOG}" ] && [ -f "${TARGET_LOG}" ]; then
	LOG_MARK="$(wc -c <"${TARGET_LOG}")"
fi

"${CC:-cc}" -O2 -g -Wall -Wextra -Werror -pthread "${SOURCE}" -o "${BIN}" ||
	exit 1
: >"${RESULTS}"

ORIGINAL_READ_AHEAD="$(blockdev --getra "${MEMORY_DEV}")" || exit 1
blockdev --setra "${READ_AHEAD}" "${MEMORY_DEV}" || exit 1
python3 - "${MEMORY_DEV}" "${SIZE_MIB}" "${THREADS}" "${READ_AHEAD}" \
	"${ORIGINAL_READ_AHEAD}" <<'PY' >"${OUTPUT}/environment.json"
import json, os, platform, sys
print(json.dumps({
    "device": sys.argv[1],
    "size_mib": int(sys.argv[2]),
    "threads": int(sys.argv[3]),
    "read_ahead_sectors": int(sys.argv[4]),
    "original_read_ahead_sectors": int(sys.argv[5]),
    "kernel": platform.release(),
    "cpu_count": os.cpu_count(),
}, separators=(",", ":")))
PY

drop_page_cache()
{
	sync
	echo 3 >/proc/sys/vm/drop_caches
}

run_case()
{
	local name="$1" offset="$2" size="$3" pattern="$4" threads="$5" writes="$6"
	local result

	drop_page_cache
	echo "---- ${name}: offset=${offset}MiB size=${size}MiB ${pattern}/${threads}"
	if ! result="$("${BIN}" --device "${MEMORY_DEV}" --offset-mib "${offset}" \
			--size-mib "${size}" --pattern "${pattern}" --threads "${threads}" \
			--write-percent "${writes}")"; then
		echo "${name} failed" >&2
		exit 1
	fi
	python3 - "${name}" "${result}" <<'PY' >>"${RESULTS}"
import json, sys
row = json.loads(sys.argv[2])
row["case"] = sys.argv[1]
print(json.dumps(row, separators=(",", ":")))
PY
	printf '%s\n' "${result}"
}

# Only the first case starts from a clean export LRU, so activate a fresh import
# immediately before this script. Later disjoint regions avoid hits but still
# represent a steady-state LRU containing 16 unrelated objects. The final pair
# deliberately reuses one 16 MiB region.
REQUIRED_MIB=$((SIZE_MIB * 6 + 16))
DEVICE_MIB=$(( $(blockdev --getsize64 "${MEMORY_DEV}") / 1024 / 1024 ))
[ "${DEVICE_MIB}" -ge "${REQUIRED_MIB}" ] || {
	echo "memory device is ${DEVICE_MIB} MiB; ${REQUIRED_MIB} MiB required" >&2
	exit 1
}

if [ "${CONCURRENT_WRITES}" -eq 1 ]; then
	echo "---- cold_three_lvol: mmap plus two destructive fio jobs"
	echo "NOTE: start immediately after import so this overlaps decouple."
	drop_page_cache
	fio --name=rootfs --filename="${ROOTFS_DEV}" --direct=1 --rw=randrw \
		--rwmixread=70 --bs=4k --iodepth=16 --ioengine=libaio \
		--size="${SIZE_MIB}m" --time_based=1 --runtime=15 \
		--group_reporting=1 --output-format=json \
		--output="${OUTPUT}/rootfs-fio.json" &
	ROOT_PID=$!
	fio --name=metadata --filename="${METADATA_DEV}" --direct=1 --rw=randwrite \
		--bs=4k --iodepth=8 --ioengine=libaio --size="${SIZE_MIB}m" \
		--time_based=1 --runtime=15 --group_reporting=1 --output-format=json \
		--output="${OUTPUT}/metadata-fio.json" &
	META_PID=$!

	# Start mmap immediately: production does not wait for either guest disk I/O
	# or decouple to settle after activation.
	if ! result="$("${BIN}" --device "${MEMORY_DEV}" \
			--offset-mib "$((SIZE_MIB * 5))" --size-mib "${SIZE_MIB}" \
			--pattern random --threads "${THREADS}")"; then
		echo "concurrent mmap benchmark failed" >&2
		exit 1
	fi
	python3 - "${result}" <<'PY' >>"${RESULTS}"
import json, sys
row = json.loads(sys.argv[1])
row["case"] = "cold_three_lvol"
print(json.dumps(row, separators=(",", ":")))
PY
	wait "${ROOT_PID}"; ROOT_RC=$?
	ROOT_PID=""
	wait "${META_PID}"; META_RC=$?
	META_PID=""
	[ "${ROOT_RC}" -eq 0 ] && [ "${META_RC}" -eq 0 ] || {
		echo "fio failed: rootfs=${ROOT_RC}, metadata=${META_RC}" >&2
		exit 1
	}
fi

if [ "${CONCURRENT_WRITES}" -eq 0 ]; then
	echo "NOTE: cold_restore_seq requires a newly activated import with an empty export LRU."
	run_case cold_restore_seq 0 "${SIZE_MIB}" sequential "${THREADS}" 0
else
	run_case steady_restore_seq 0 "${SIZE_MIB}" sequential "${THREADS}" 0
fi
run_case steady_seq1 "${SIZE_MIB}" "${SIZE_MIB}" sequential 1 0
# stampede touches one page per thread per object. It is a single-flight
# microbenchmark, not a full-memory throughput result.
run_case steady_stampede "$((SIZE_MIB * 2))" "${SIZE_MIB}" stampede "${THREADS}" 0
run_case steady_random_cap "$((SIZE_MIB * 3))" "${SIZE_MIB}" random "${THREADS}" 0
run_case private_cow_10pct "$((SIZE_MIB * 4))" "${SIZE_MIB}" random "${THREADS}" 10

WARM_OFFSET=$((SIZE_MIB * 6))
run_case lru_prime "${WARM_OFFSET}" 16 sequential "${THREADS}" 0
run_case lru_reuse "${WARM_OFFSET}" 16 sequential "${THREADS}" 0

python3 - "${RESULTS}" <<'PY'
import json, sys

rows = [json.loads(line) for line in open(sys.argv[1], encoding="utf-8")]
print()
print("case                         mapped/touched(MiB) elapsed(ms)  MiB/s   p95(us)   max(us)")
for r in rows:
    mapped = r["mapped_bytes"] / 1048576
    touched = r["touched_bytes"] / 1048576
    print(f"{r['case']:<28} {mapped:>6.1f}/{touched:<6.3f} "
          f"{r['elapsed_ms']:>10.3f} "
          f"{r['mib_per_sec']:>7.1f} {r['latency_us']['p95']:>9.3f} "
          f"{r['latency_us']['max']:>9.3f}")
PY

if [ -n "${TARGET_LOG}" ] && [ -f "${TARGET_LOG}" ]; then
	dd if="${TARGET_LOG}" bs=1 skip="${LOG_MARK}" status=none 2>/dev/null |
		grep -a 'Releasing imported export' >"${OUTPUT}/export-stats.log" || true
	if [ ! -s "${OUTPUT}/export-stats.log" ]; then
		echo
		echo "No export release counters yet. Deactivate/delete the imported lvol,"
		echo "then collect 'Releasing imported export' from ${TARGET_LOG}."
	else
		echo "Export counters are aggregate for this activation, not per case:"
		cat "${OUTPUT}/export-stats.log"
	fi
fi

echo
echo "Results: ${RESULTS}"
echo "Environment: ${OUTPUT}/environment.json"
[ "${CONCURRENT_WRITES}" -eq 0 ] ||
	echo "fio JSON: ${OUTPUT}/rootfs-fio.json, ${OUTPUT}/metadata-fio.json"
