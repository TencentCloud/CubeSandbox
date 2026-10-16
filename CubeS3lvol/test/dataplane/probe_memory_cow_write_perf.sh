#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Measure the pause-memory incremental write path.  The measured object is a
# writable clone of a fully allocated snapshot, matching the volume prepared by
# Cubelet before PauseToSnapshot.  Setup traffic is drained before measurement.
#
# The important discriminator is dest_whole_gets: one GET per written MiB means
# blobstore copied every parent cluster before overwriting it.
#
# Usage:
#   sudo ./test/dataplane/probe_memory_cow_write_perf.sh
#   sudo ... --size-mib 1024 --output /tmp/memory-cow-1g
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"
TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"
# shellcheck source=../../scripts/rcow_common.sh
. "${ROOT}/scripts/rcow_common.sh"

SIZE_MIB=10240
PORT=4495
CPUMASK="${S3LVOL_TGT_CPUMASK:-0x3}"
OUTPUT=""

usage()
{
	cat <<EOF
Usage: $0 [options]
  --size-mib N   allocated base and overwrite size (default: ${SIZE_MIB})
  --port N       loopback NVMe/TCP port (default: ${PORT})
  --output DIR   retained result directory
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--size-mib|--port|--output)
		[ "$#" -ge 2 ] || { echo "$1 requires a value" >&2; exit 2; }
		case "$1" in
		--size-mib) SIZE_MIB="$2" ;;
		--port) PORT="$2" ;;
		--output) OUTPUT="$2" ;;
		esac
		shift 2
		;;
	-h|--help) usage; exit 0 ;;
	*) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
	esac
done

case "${SIZE_MIB}:${PORT}" in
*[!0-9:]*) echo "numeric options must be positive integers" >&2; exit 2 ;;
esac
[ "${SIZE_MIB}" -gt 0 ] && [ "${PORT}" -gt 0 ] ||
	{ echo "size and port must be positive" >&2; exit 2; }

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

# Keep the complete measured write in WAL so S3 drain never backpressures the
# foreground. Leave a small cache only; the parent should remain predominantly
# cold, as it is after a cross-node restore or process restart.
WAL_MIB=$((SIZE_MIB + 1024))
IMAGE_MIB=$((64 + WAL_MIB + 128))
CAPACITY_GIB=$(( 2 * ((SIZE_MIB + 1023) / 1024) + 4 ))
LVOL_GIB=$(( (SIZE_MIB + 1023) / 1024 ))

TGT_PID=""
CONNECTED=0
CREDENTIALS_READY=0
rpc() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" "$@"; }
raw() { python3 "${RPC_PY}" --sock "${RPC_SOCK}" --raw "$1" ${2:+"$2"}; }

cleanup()
{
	set +e
	[ "${CONNECTED}" -eq 0 ] || nvme disconnect -n "${NQN}" >/dev/null 2>&1
	if [ -n "${TGT_PID}" ]; then
		kill "${TGT_PID}" >/dev/null 2>&1
		for _ in $(seq 20); do
			kill -0 "${TGT_PID}" >/dev/null 2>&1 || break
			sleep 0.1
		done
		kill -9 "${TGT_PID}" >/dev/null 2>&1 || true
		wait "${TGT_PID}" >/dev/null 2>&1 || true
	fi
	if [ "${CREDENTIALS_READY}" -eq 1 ]; then
		python3 "${PREFIX_RM}" -e "${EP}" -b "${BK}" -r "${RG}" \
			-p "${LVS}/" >/dev/null 2>&1 || true
	fi
	rm -f "${WAL_IMG}" "${RPC_SOCK}"
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
echo "case: ${SIZE_MIB} MiB full overwrite of a cold snapshot clone"

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

echo "preparing ${SIZE_MIB} MiB allocated parent..."
fio --name=prepare --filename="${BASE_DEV}" --ioengine=libaio --direct=1 \
	--rw=write --bs=1m --iodepth=16 --size="${SIZE_MIB}m" \
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
fio --name=memory-cow --filename="${CLONE_DEV}" --ioengine=libaio --direct=1 \
	--rw=write --bs=1m --iodepth=1 --size="${SIZE_MIB}m" \
	--group_reporting=1 --output-format=json \
	--output="${OUTPUT}/memory-cow.fio.json"
stats >"${OUTPUT}/stats.after.json"

python3 - "${OUTPUT}" "${SIZE_MIB}" >"${SUMMARY}" <<'PY'
import json, os, sys
out, size = sys.argv[1], int(sys.argv[2])
def load(name):
    with open(os.path.join(out, name), encoding="utf-8") as f:
        return json.load(f)
before, after = load("stats.before.json"), load("stats.after.json")
fio = load("memory-cow.fio.json")["jobs"][0]["write"]
def delta(key):
    return int(after.get(key, 0)) - int(before.get(key, 0))
summary = {
    "size_mib": size,
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
    "rmw_count": delta("rmw_count"),
}
print(json.dumps(summary, indent=2, sort_keys=True))
PY

python3 - "${SUMMARY}" <<'PY'
import json, sys
s=json.load(open(sys.argv[1], encoding="utf-8"))
print(f"memory clone overwrite: {s['mib_per_sec']:.1f} MiB/s, "
      f"{s['seconds']:.2f} s, mean latency {s['latency_ms_mean']:.2f} ms")
print(f"parent reads: whole_gets={s['dest_whole_gets']}, "
      f"direct_gets={s['dest_direct_gets']}, get_mib={s['dest_get_mib']:.1f}")
print(f"WAL: appends={s['wal_appends']}, batches={s['wal_batches']}, "
      f"written={s['wal_mib']:.1f} MiB")
print(f"details: {sys.argv[1]}")
PY
