#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Snapshot and clone, end to end against real S3.
#
#  === What this test is actually for ===
#
#  A snapshot is only worth anything if it stops changing. Everything else about
#  it -- that the RPC succeeds, that a bdev appears, that it reports read_only --
#  can be true while the data silently follows the origin, which would make the
#  feature worse than not having it.
#
#  So the load-bearing assertions here are the isolation ones:
#
#    - write A to the origin, snapshot it, then overwrite the origin with B:
#      the snapshot must still read A;
#    - clone the snapshot: the clone must initially read A (reads fall through to
#      the parent), and writing C to the clone must leave both the snapshot at A
#      and the origin at B.
#
#  Three volumes sharing clusters, each having to disagree with the others in the
#  right way. On this backend that is the copy-on-write path in blobstore driving
#  our bs_dev: a write to an unallocated cluster reads the whole cluster from the
#  parent and writes it to a *newly allocated* cluster, i.e. new LBAs, new chunks,
#  new UUIDs, new S3 objects. It never rewrites the parent's chunks -- verified in
#  blob_can_copy() rather than assumed, see the comment in
#  vrcow_lvstore.c. This test is what keeps that true.
#
#  Each of the three is exported as its own NVMe namespace, because reading them
#  through the guest path is the only way to check what a consumer would see.
#
#  Requires: root, nvme-cli, a real bucket with credentials in the environment.
#
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TOOLS_DIR="${REPO_ROOT}/test/tools"

TGT_BIN="${REPO_ROOT}/app/s3lvol_tgt/s3lvol_tgt"
RPC_PY="${SPDK_ROOT:-${REPO_ROOT}/deps/spdk}/scripts/rpc.py"
# Every SPDK RPC needs the socket named explicitly: rpc.py defaults to
# /var/tmp/spdk.sock, while the target now listens on /var/run/s3lvol.sock.
RPC="${RPC_PY} -s /var/run/s3lvol.sock"
RPC_SOCK="/var/run/s3lvol.sock"

LVS_NAME="snapvs"
LVOL_NAME="vol0"
SNAP_NAME="snap0"
CLONE_NAME="clone0"

NQN="nqn.2026-08.io.spdk:s3snap"
LISTEN_ADDR="127.0.0.1"
LISTEN_PORT="4420"

CAPACITY_GIB=8
# GiB for the RPC (size_gib), bytes for the MiB figures printed below.
LVOL_GIB=1
LVOL_SIZE=$((LVOL_GIB * 1024 * 1024 * 1024))
JOURNAL_MB=64
WAL_MB=128
WAL_FILE="${S3LVOL_WAL_FILE:-/data/s3lvol_snap_wal.img}"
WAL_BDEV="snap_wal0"
# Journal + WAL + super, plus the room local_dev carves out for its chunk cache
# region from whatever is left. Sizing this at just journal+WAL gets -ENOSPC out
# of create_lvstore, which is what the first run of this script did.
WAL_FILE_MB=$((JOURNAL_MB + WAL_MB + 128))

# Where the three patterns are written. 8 MiB at 8 MiB: past the blobstore
# metadata region, and large enough to span several 1 MiB chunks and more than one
# cluster, so the copy-on-write path is actually exercised rather than a single
# cluster being copied once.
IO_OFF_MB=8
IO_LEN_MB=8

ENDPOINT=""
BUCKET=""
REGION="ap-nanjing"

PASS=0
FAIL=0
TGT_PID=""
TGT_LOG=""
WORKDIR=""
CONNECTED=0
LVS_CREATED=0
LVS_WAS_CREATED=0
TRANSPORT_READY=0
WAL_FILE_CREATED=0
TEARDOWN_ANOMALY=0
# Mirror of dataplane's flag: stays 0 because check_target() here only calls
# fail(); the lvstore-deletion block is itself guarded by `target_alive`, so a
# dead target short-circuits the surrounding `if`. Referenced in cleanup so it
# must be bound under `set -u`.
TGT_CRASHED=0

pass() { PASS=$((PASS + 1)); echo "[PASS] $*"; }
fail() { FAIL=$((FAIL + 1)); echo "[FAIL] $*"; }
info() { echo "---- $*"; }

usage()
{
	cat <<EOF
Usage: $0 -e <endpoint> -b <bucket> [-r <region>]

  -e   S3/COS endpoint host, e.g. cos.ap-nanjing.myqcloud.com
  -b   bucket (must be a test bucket: a clean run deletes its own prefix)
  -r   region (default: ${REGION})

Credentials are read from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.

Environment:
  S3LVOL_KEEP_S3    keep the S3 objects even after a clean run
  S3LVOL_KEEP_LOGS  keep the target log even after a clean run
EOF
}

while getopts "e:b:r:h" opt; do
	case "${opt}" in
	e) ENDPOINT="${OPTARG}" ;;
	b) BUCKET="${OPTARG}" ;;
	r) REGION="${OPTARG}" ;;
	h) usage; exit 0 ;;
	*) usage; exit 1 ;;
	esac
done

if [ -z "${ENDPOINT}" ] || [ -z "${BUCKET}" ]; then
	usage
	exit 1
fi
if [ -z "${AWS_ACCESS_KEY_ID:-}" ] || [ -z "${AWS_SECRET_ACCESS_KEY:-}" ]; then
	echo "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY must be set" >&2
	exit 1
fi
if [ "$(id -u)" -ne 0 ]; then
	echo "must run as root (hugepages, nvme connect)" >&2
	exit 1
fi
for tool in nvme dd md5sum python3; do
	command -v "${tool}" >/dev/null 2>&1 || {
		echo "${tool} is required" >&2; exit 1; }
done

WORKDIR="$(mktemp -d /tmp/s3lvol_snap.XXXXXX)"
TGT_LOG="${WORKDIR}/target.log"

raw_rpc()
{
	python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" "$1" "${2:-}"
}

target_alive()
{
	[ -n "${TGT_PID}" ] && kill -0 "${TGT_PID}" 2>/dev/null
}

# Checks the target survived the previous step. Every I/O assertion below is
# meaningless if the process died, and a dead target produces confusing downstream
# failures rather than an obvious one.
check_target()
{
	local where="$1"

	if ! target_alive; then
		fail "target died during ${where}"
		tail -25 "${TGT_LOG}" | sed 's/^/       /'
		return 1
	fi
	if grep -qE 'Assertion|SIGSEGV|panic:' "${TGT_LOG}" 2>/dev/null; then
		fail "target hit an assertion during ${where}"
		grep -nE 'Assertion|SIGSEGV|panic:' "${TGT_LOG}" | head -5 | \
			sed 's/^/       /'
		return 1
	fi
	return 0
}

nvme_settle()
{
	# See run_dataplane_test.sh: disconnecting while udev is still probing
	# leaves harmless-but-confusing Buffer I/O errors in dmesg.
	udevadm settle --timeout=5 >/dev/null 2>&1 || true
}

cleanup()
{
	local rc=$?

	echo
	echo "=== cleanup ==="

	if [ "${CONNECTED}" -eq 1 ]; then
		nvme_settle
		nvme disconnect -n "${NQN}" >/dev/null 2>&1 && \
			info "nvme disconnected" || info "nvme disconnect failed"
		CONNECTED=0
	fi

	if target_alive; then
		if [ "${TRANSPORT_READY}" -eq 1 ]; then
			${RPC} nvmf_delete_subsystem "${NQN}" >/dev/null 2>&1 || true
		fi

		# Deleting in dependency order, because the point of the whole
		# exercise is that these three are not independent: the snapshot
		# cannot go while the clone still needs it.
		if [ "${LVS_CREATED}" -eq 1 ]; then
			for name in "${CLONE_NAME}" "${LVOL_NAME}" "${SNAP_NAME}"; do
				raw_rpc rcow_delete_lvol \
					"$(printf '{"lvol_name":"%s"}' \
						"${name}")" \
					>/dev/null 2>&1 || true
			done

			# delete rather than unload on a clean run: unload now keeps
			# the bstore.json entry, which is correct in production but
			# would leave a record pointing at objects this cleanup is
			# about to remove. After a failure keep both as evidence.
			CLEANUP_RPC=rcow_unload_lvstore
			if [ "${FAIL}" -eq 0 ] && [ "${TGT_CRASHED}" -eq 0 ] && \
			   [ -z "${S3LVOL_KEEP_S3:-}" ]; then
				CLEANUP_RPC=rcow_delete_lvstore
			fi
			if raw_rpc "${CLEANUP_RPC}" \
					"$(printf '{"lvs_name":"%s"}' "${LVS_NAME}")" \
					>/dev/null 2>&1; then
				info "lvstore ${CLEANUP_RPC#rcow_}ed"
				LVS_CREATED=0
			else
				info "lvstore ${CLEANUP_RPC#rcow_} failed"
				TEARDOWN_ANOMALY=1
			fi
		fi

		kill -TERM "${TGT_PID}" 2>/dev/null
		for _ in $(seq 50); do
			kill -0 "${TGT_PID}" 2>/dev/null || break
			sleep 0.1
		done
		if kill -0 "${TGT_PID}" 2>/dev/null; then
			# The only moment the process is both wedged and alive.
			info "target ignored SIGTERM: it is stuck, not slow"
			TEARDOWN_ANOMALY=1
			command -v gdb >/dev/null 2>&1 && \
				gdb -p "${TGT_PID}" -batch -ex "set pagination off" \
					-ex "thread apply all bt" \
					>"${WORKDIR}/gdb_teardown.txt" 2>&1 || true
			kill -KILL "${TGT_PID}" 2>/dev/null
		else
			info "target stopped cleanly"
		fi
	fi

	if [ "${LVS_WAS_CREATED}" -eq 1 ]; then
		if [ "${FAIL}" -eq 0 ] && [ "${rc}" -eq 0 ] && \
		   [ "${TEARDOWN_ANOMALY}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
			if python3 "${TOOLS_DIR}/s3_prefix_rm.py" \
					-e "${ENDPOINT}" -b "${BUCKET}" -r "${REGION}" \
					-p "${LVS_NAME}/" >"${WORKDIR}/s3_rm.log" 2>&1; then
				info "S3: $(cat "${WORKDIR}/s3_rm.log")"
			else
				info "S3 cleanup failed:"
				sed 's/^/       /' "${WORKDIR}/s3_rm.log"
			fi
		else
			info "S3 objects kept under '${LVS_NAME}/' in ${BUCKET}"
			info "remove: ${TOOLS_DIR}/s3_prefix_rm.py -e ${ENDPOINT} -b ${BUCKET} -r ${REGION} -p ${LVS_NAME}/"
		fi
	fi

	if [ "${WAL_FILE_CREATED}" -eq 1 ]; then
		if [ "${FAIL}" -eq 0 ] && [ "${rc}" -eq 0 ] && \
		   [ "${TEARDOWN_ANOMALY}" -eq 0 ]; then
			rm -f "${WAL_FILE}"
		else
			info "local device image kept at ${WAL_FILE}"
		fi
	fi

	if [ "${FAIL}" -gt 0 ] || [ "${rc}" -ne 0 ] || \
	   [ "${TEARDOWN_ANOMALY}" -eq 1 ] || [ -n "${S3LVOL_KEEP_LOGS:-}" ]; then
		info "logs kept in ${WORKDIR}"
	else
		rm -rf "${WORKDIR}"
	fi

	echo
	echo "=== result: ${PASS} passed, ${FAIL} failed ==="
	[ "${FAIL}" -eq 0 ] || exit 1
}
trap cleanup EXIT

# Identifies the namespace that appeared, rather than assuming n2/n3: the host may
# have unrelated controllers, and namespace numbering is not ours to predict.
#
# Prints the device or nothing, and deliberately does not call fail() itself: it
# runs inside a command substitution, so anything it did to PASS/FAIL would happen
# in a subshell and be lost. The first version did exactly that and reported
# "0 failed" while exiting on a failure.
wait_for_new_ns()
{
	local before="$1" found=""

	nvme ns-rescan "/dev/$(basename "${NVME_DEV}" | sed 's/n[0-9]*$//')" \
		>/dev/null 2>&1 || true

	for _ in $(seq 30); do
		found="$(comm -13 <(echo "${before}") \
				<(ls /dev/nvme*n* 2>/dev/null | sort || true) | head -1)"
		[ -n "${found}" ] && break
		sleep 0.5
	done

	echo "${found}"
}

# Writes a pattern and returns its md5, so later comparisons are against what was
# actually written rather than against a re-read of the same device.
write_pattern()
{
	local dev="$1" file="$2"

	dd if=/dev/urandom of="${file}" bs=1M count="${IO_LEN_MB}" status=none
	dd if="${file}" of="${dev}" bs=1M count="${IO_LEN_MB}" seek="${IO_OFF_MB}" \
		oflag=direct conv=fsync status=none 2>"${WORKDIR}/write.err"
}

read_md5()
{
	local dev="$1" out="$2"

	dd if="${dev}" of="${out}" bs=1M count="${IO_LEN_MB}" skip="${IO_OFF_MB}" \
		iflag=direct status=none 2>/dev/null && \
		md5sum "${out}" | cut -d' ' -f1
}

md5_of()
{
	md5sum "$1" | cut -d' ' -f1
}

echo "=== s3lvol snapshot/clone test ==="
echo "    endpoint : ${ENDPOINT}"
echo "    bucket   : ${BUCKET}"
echo "    workdir  : ${WORKDIR}"
echo

# ==========================================================================
# [1] target + local device
# ==========================================================================
echo "[1] starting the target"

# -x, not -f: matching the whole command line makes any process that merely
# mentions the target count, and `tail -f .../s3lvol_tgt.log` is exactly that. -x
# compares the process name, so only the binary itself matches.
if pgrep -x s3lvol_tgt >/dev/null 2>&1; then
	fail "another s3lvol_tgt is running; stop it first (pkill -f s3lvol_tgt)"
	exit 1
fi

# A pass against a stale binary is evidence pointing the wrong way -- it has
# happened, and nothing in the output showed it. See the script for the details.
if [ -z "${S3LVOL_SKIP_FRESH_CHECK:-}" ]; then
	"${TOOLS_DIR}/check_binary_fresh.sh" "${TGT_BIN}" || exit 1
fi

rm -f "${WAL_FILE}"
truncate -s "${WAL_FILE_MB}M" "${WAL_FILE}"
WAL_FILE_CREATED=1

"${TGT_BIN}" -m "${S3LVOL_TGT_CPUMASK:-0x3}" --no-huge -s 2048 -r /var/run/s3lvol.sock \
	>"${TGT_LOG}" 2>&1 &
TGT_PID=$!

for _ in $(seq 80); do
	[ -e "${RPC_SOCK}" ] && break
	sleep 0.25
done
if [ ! -e "${RPC_SOCK}" ]; then
	fail "target did not create ${RPC_SOCK}"
	tail -20 "${TGT_LOG}" | sed 's/^/       /'
	exit 1
fi
sleep 1
pass "target is up (pid ${TGT_PID})"

${RPC} bdev_aio_create "${WAL_FILE}" "${WAL_BDEV}" 4096 >/dev/null 2>&1 || {
	fail "bdev_aio_create"; exit 1; }
pass "local device ${WAL_BDEV} attached"

# ==========================================================================
# [2] lvstore + origin volume
# ==========================================================================
echo
echo "[2] creating the lvstore and the origin volume"

S3LVOL_EXTRA_JSON=""
[ "${S3LVOL_TEST_PATH_STYLE:-0}" -eq 1 ] && S3LVOL_EXTRA_JSON+=',"path_style":true'
[ "${S3LVOL_TEST_NO_TLS:-0}" -eq 1 ] && S3LVOL_EXTRA_JSON+=',"no_tls":true'
# The same flags in the form s3_prefix_rm.py takes, for count_objects and
# remove_prefix. run_all.sh sets this for a full-suite run; a direct run
# generates it here so only PATH_STYLE / NO_TLS need to be set.
if [ -z "${S3LVOL_TEST_S3FLAGS:-}" ]; then
	S3LVOL_TEST_S3FLAGS=""
	[ "${S3LVOL_TEST_PATH_STYLE:-0}" -eq 1 ] && S3LVOL_TEST_S3FLAGS+=" --path-style"
	[ "${S3LVOL_TEST_NO_TLS:-0}" -eq 1 ] && S3LVOL_TEST_S3FLAGS+=" --no-tls"
fi

if ! raw_rpc rcow_add_s3_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"%s}' \
		"${BUCKET}" "${ENDPOINT}" "${BUCKET}" "${REGION}" "${S3LVOL_EXTRA_JSON}")" \
		>/dev/null 2>"${WORKDIR}/cos_config.err"; then
	fail "rcow_add_s3_config"
	sed 's/^/       /' "${WORKDIR}/cos_config.err"
	exit 1
fi
pass "COS namespace registered"

if ! raw_rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"%s","journal_size_mb":%d,"wal_size_mb":%d}' \
		"${LVS_NAME}" "${BUCKET}" "${CAPACITY_GIB}" \
		"${WAL_BDEV}" "${JOURNAL_MB}" "${WAL_MB}")" \
		>"${WORKDIR}/lvs.json" 2>"${WORKDIR}/lvs.err"; then
	fail "rcow_create_lvstore"
	sed 's/^/       /' "${WORKDIR}/lvs.err"
	exit 1
fi
LVS_CREATED=1
LVS_WAS_CREATED=1
pass "lvstore created"

if ! raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
		"${LVOL_NAME}" "${LVOL_GIB}")" \
		>/dev/null 2>"${WORKDIR}/lvol.err"; then
	fail "rcow_create_lvol"
	sed 's/^/       /' "${WORKDIR}/lvol.err"
	exit 1
fi
pass "origin volume created ($((LVOL_SIZE / 1024 / 1024)) MiB)"
check_target "step 2" || exit 1

# ==========================================================================
# [3] export the origin and connect
# ==========================================================================
echo
echo "[3] exporting the origin over nvmf-tcp"

${RPC} nvmf_create_transport -t TCP >/dev/null 2>&1 || true
${RPC} nvmf_create_subsystem "${NQN}" -a -s S3SNAP0000000000001 \
	>/dev/null 2>&1 || { fail "nvmf_create_subsystem"; exit 1; }
TRANSPORT_READY=1

${RPC} nvmf_subsystem_add_ns "${NQN}" "${LVS_NAME}/${LVOL_NAME}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (origin)"; exit 1; }
${RPC} nvmf_subsystem_add_listener "${NQN}" -t tcp -a "${LISTEN_ADDR}" \
	-s "${LISTEN_PORT}" >/dev/null 2>&1 || { fail "add_listener"; exit 1; }

BEFORE_CONNECT="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
if ! nvme connect -t tcp -a "${LISTEN_ADDR}" -s "${LISTEN_PORT}" -n "${NQN}" \
		>"${WORKDIR}/connect.log" 2>&1; then
	fail "nvme connect"
	sed 's/^/       /' "${WORKDIR}/connect.log"
	exit 1
fi
CONNECTED=1

NVME_DEV=""
for _ in $(seq 30); do
	NVME_DEV="$(comm -13 <(echo "${BEFORE_CONNECT}") \
			<(ls /dev/nvme*n* 2>/dev/null | sort || true) | head -1)"
	[ -n "${NVME_DEV}" ] && break
	sleep 0.5
done
if [ -z "${NVME_DEV}" ]; then
	fail "no new block device after connect"
	exit 1
fi
pass "origin is ${NVME_DEV}"

# ==========================================================================
# [4] pattern A on the origin
# ==========================================================================
echo
echo "[4] writing pattern A to the origin"

write_pattern "${NVME_DEV}" "${WORKDIR}/A.bin"
HASH_A="$(md5_of "${WORKDIR}/A.bin")"
if [ "$(read_md5 "${NVME_DEV}" "${WORKDIR}/A_read.bin")" = "${HASH_A}" ]; then
	pass "pattern A written and read back on the origin"
else
	fail "the origin did not read back pattern A"
	exit 1
fi
check_target "step 4" || exit 1

# Flush before snapshotting. Not required for correctness -- a snapshot freezes
# clusters, whatever layer their data currently sits in -- but it makes a failure
# here mean "the snapshot is wrong" rather than "the WAL had not drained", which
# are different bugs.
raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS_NAME}")" \
	>/dev/null 2>&1 || info "flush_lvstore failed (continuing)"

# ==========================================================================
# [5] snapshot
# ==========================================================================
echo
echo "[5] snapshotting the origin"

if ! raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${LVOL_NAME}" "${SNAP_NAME}")" \
		>"${WORKDIR}/snap.json" 2>"${WORKDIR}/snap.err"; then
	fail "rcow_create_snapshot"
	sed 's/^/       /' "${WORKDIR}/snap.err"
	check_target "step 5" || exit 1
	exit 1
fi
pass "snapshot created: $(cat "${WORKDIR}/snap.json")"
check_target "step 5, snapshotting" || exit 1

BEFORE_SNAP_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
if ! ${RPC} nvmf_subsystem_add_ns "${NQN}" "${LVS_NAME}/${SNAP_NAME}" \
		>/dev/null 2>"${WORKDIR}/add_ns_snap.err"; then
	fail "nvmf_subsystem_add_ns (snapshot)"
	sed 's/^/       /' "${WORKDIR}/add_ns_snap.err"
	exit 1
fi

# Asked for rather than inferred. add_ns does not report the id it assigned, and
# guessing "2" would silently remove the wrong namespace the day the ordering
# changes -- step 9 depends on this being the snapshot's.
SNAP_NSID="$(${RPC} nvmf_get_subsystems 2>/dev/null | python3 -c '
import json, sys
nqn, bdev = sys.argv[1], sys.argv[2]
for sub in json.load(sys.stdin):
    if sub.get("nqn") != nqn:
        continue
    for ns in sub.get("namespaces", []):
        if ns.get("bdev_name") == bdev:
            print(ns["nsid"])
            sys.exit(0)
' "${NQN}" "${LVS_NAME}/${SNAP_NAME}")"
if [ -z "${SNAP_NSID}" ]; then
	fail "could not find the namespace id for ${LVS_NAME}/${SNAP_NAME}"
	exit 1
fi
info "snapshot namespace id is ${SNAP_NSID}"

SNAP_DEV="$(wait_for_new_ns "${BEFORE_SNAP_NS}")"
if [ -z "${SNAP_DEV}" ]; then
	fail "snapshot: no new namespace appeared on the host"
	exit 1
fi
pass "snapshot exported as ${SNAP_DEV}"

# The unified response does not carry read_only; try a direct write. An I/O
# error is the definitive signal that the device is read-only.
if dd if=/dev/urandom of="${SNAP_DEV}" bs=4K count=1 oflag=direct \
		>"${WORKDIR}/snap_write.log" 2>&1; then
	fail "the snapshot device accepted a write -- it is not read-only"
else
	pass "the snapshot refused a direct write (read-only)"
fi

if [ "$(read_md5 "${SNAP_DEV}" "${WORKDIR}/snap_read.bin")" = "${HASH_A}" ]; then
	pass "the snapshot contains pattern A"
else
	fail "the snapshot does not contain what the origin held when it was taken"
	exit 1
fi
check_target "step 5, reading the snapshot" || exit 1

# ==========================================================================
# [6] overwrite the origin -- the snapshot must not follow
#
# This is the assertion the whole feature rests on. Everything up to here would
# also pass if the snapshot were simply another name for the origin.
# ==========================================================================
echo
echo "[6] overwriting the origin with pattern B"

write_pattern "${NVME_DEV}" "${WORKDIR}/B.bin"
HASH_B="$(md5_of "${WORKDIR}/B.bin")"
check_target "step 6, writing B" || exit 1

if [ "$(read_md5 "${NVME_DEV}" "${WORKDIR}/B_read.bin")" = "${HASH_B}" ]; then
	pass "the origin now reads pattern B"
else
	fail "the origin did not take pattern B"
fi

if [ "$(read_md5 "${SNAP_DEV}" "${WORKDIR}/snap_after.bin")" = "${HASH_A}" ]; then
	pass "the snapshot still reads pattern A after the origin was overwritten"
else
	fail "the snapshot changed when the origin was overwritten"
	info "copy-on-write wrote over clusters the snapshot still references"
fi
check_target "step 6" || exit 1

# Same check after a flush, since the copy-on-write above went through the WAL and
# the overlay first. A snapshot that is correct in memory and wrong in S3 is the
# more likely failure of the two, and only shows up once the data has to come back
# from the object store.
raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS_NAME}")" \
	>/dev/null 2>&1 || info "flush_lvstore failed (continuing)"

if [ "$(read_md5 "${SNAP_DEV}" "${WORKDIR}/snap_flushed.bin")" = "${HASH_A}" ]; then
	pass "the snapshot still reads pattern A after a flush to S3"
else
	fail "the snapshot changed after the copy-on-write data reached S3"
fi
check_target "step 6, after flush" || exit 1

# ==========================================================================
# [7] clone
# ==========================================================================
echo
echo "[7] cloning the snapshot"

if ! raw_rpc rcow_create_clone "$(printf '{"snapshot_name":"%s","clone_name":"%s"}' \
		"${SNAP_NAME}" "${CLONE_NAME}")" \
		>"${WORKDIR}/clone.json" 2>"${WORKDIR}/clone.err"; then
	fail "rcow_create_clone"
	sed 's/^/       /' "${WORKDIR}/clone.err"
	check_target "step 7" || exit 1
	exit 1
fi
pass "clone created: $(cat "${WORKDIR}/clone.json")"
check_target "step 7, cloning" || exit 1

BEFORE_CLONE_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${LVS_NAME}/${CLONE_NAME}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (clone)"; exit 1; }

CLONE_DEV="$(wait_for_new_ns "${BEFORE_CLONE_NS}")"
if [ -z "${CLONE_DEV}" ]; then
	fail "clone: no new namespace appeared on the host"
	exit 1
fi
pass "clone exported as ${CLONE_DEV}"

# Verify the clone is writable: a direct write does not error.
if dd if=/dev/urandom of="${CLONE_DEV}" bs=4K count=1 oflag=direct \
		>"${WORKDIR}/clone_write.log" 2>&1; then
	pass "the clone device accepted a write (writable)"
else
	fail "the clone device refused a write -- it is read-only"
fi

# Reads fall through to the parent, so a fresh clone must look exactly like the
# snapshot -- not like the origin, which has moved on to B.
if [ "$(read_md5 "${CLONE_DEV}" "${WORKDIR}/clone_read.bin")" = "${HASH_A}" ]; then
	pass "the fresh clone reads pattern A through its parent"
else
	fail "the fresh clone does not read its parent's data"
fi
check_target "step 7, reading the clone" || exit 1

echo
echo "[7b] writing pattern C to the clone -- three volumes must now disagree"

write_pattern "${CLONE_DEV}" "${WORKDIR}/C.bin"
HASH_C="$(md5_of "${WORKDIR}/C.bin")"
check_target "step 7b, writing C" || exit 1

raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS_NAME}")" \
	>/dev/null 2>&1 || info "flush_lvstore failed (continuing)"

if [ "$(read_md5 "${CLONE_DEV}" "${WORKDIR}/clone_after.bin")" = "${HASH_C}" ]; then
	pass "the clone reads pattern C"
else
	fail "the clone did not take pattern C"
fi

if [ "$(read_md5 "${SNAP_DEV}" "${WORKDIR}/snap_final.bin")" = "${HASH_A}" ]; then
	pass "the snapshot is still pattern A"
else
	fail "writing the clone corrupted the snapshot"
fi

if [ "$(read_md5 "${NVME_DEV}" "${WORKDIR}/origin_final.bin")" = "${HASH_B}" ]; then
	pass "the origin is still pattern B"
else
	fail "writing the clone corrupted the origin"
fi
check_target "step 7b" || exit 1

# ==========================================================================
# [7c] discarding the clone
#
# Two questions, both of which decide whether deleting a volume can ever reclaim
# S3 space. blobstore does not clear data clusters when a blob is deleted --
# bs_release_cluster() only returns the cluster to the allocation bitmap -- so the
# proposed fix is to issue unmap over a volume before deleting it. That is only
# viable if:
#
#   1. an unmap actually reaches our bs_dev and deletes the objects, and
#   2. unmapping a clone cannot touch what its parent references.
#
# The second one is the dangerous half. A clone's unallocated clusters read
# through to the snapshot, and if an unmap followed that indirection it would
# delete objects the snapshot still needs -- corrupting a volume that is supposed
# to be immutable, from an operation on a different volume.
#
# Reading blobstore says it cannot happen: releasing a cluster requires
# blob_backed_with_zeroes_dev(), which is false for a clone, so a clone's unmap
# only reaches the device for clusters the clone itself allocated. This step is
# what turns that reading into evidence.
#
# What the clone reads afterwards is deliberately not asserted, only reported. Two
# answers are defensible -- zeroes if the cluster stays allocated with nothing
# behind it, or the parent's data if the cluster is released -- and which one
# blobstore picks is not what this test is for.
# ==========================================================================
echo
echo "[7c] discarding the clone's written region"

count_data_objects()
{
	python3 "${TOOLS_DIR}/s3_prefix_rm.py" -e "${ENDPOINT}" -b "${BUCKET}" \
		-r "${REGION}" -p "${LVS_NAME}/data/" --list ${S3LVOL_TEST_S3FLAGS:-} \
		2>/dev/null | wc -l
}

if ! command -v blkdiscard >/dev/null 2>&1; then
	info "blkdiscard not available, skipping"
else
	OBJ_BEFORE_DISCARD="$(count_data_objects)"
	info "data objects before the discard: ${OBJ_BEFORE_DISCARD}"

	# Cluster-aligned on purpose: blobstore only takes the cluster-releasing
	# path when the length is exactly one cluster, and the split layer feeds it
	# one cluster at a time. 8 MiB at 8 MiB is aligned for any cluster size up
	# to 8 MiB.
	if blkdiscard -o $((IO_OFF_MB * 1024 * 1024)) -l $((IO_LEN_MB * 1024 * 1024)) \
			"${CLONE_DEV}" 2>"${WORKDIR}/discard.err"; then
		pass "blkdiscard on the clone was accepted"
	else
		fail "blkdiscard on the clone failed"
		sed 's/^/       /' "${WORKDIR}/discard.err"
	fi
	check_target "step 7c, discarding the clone" || exit 1

	# The object deletions are fire-and-forget (s3_unmap_chunk_removed passes no
	# callback) and a listing lags behind them, so poll rather than sample.
	OBJ_AFTER_DISCARD="${OBJ_BEFORE_DISCARD}"
	for _ in $(seq 20); do
		OBJ_AFTER_DISCARD="$(count_data_objects)"
		[ "${OBJ_AFTER_DISCARD}" -lt "${OBJ_BEFORE_DISCARD}" ] && break
		sleep 1
	done

	if [ "${OBJ_AFTER_DISCARD}" -lt "${OBJ_BEFORE_DISCARD}" ]; then
		pass "the discard reclaimed S3 objects (${OBJ_BEFORE_DISCARD} -> ${OBJ_AFTER_DISCARD})"
	else
		fail "the discard reclaimed nothing (${OBJ_BEFORE_DISCARD} objects still listed)"
		info "if unmap cannot reclaim objects either, then nothing short of a"
		info "scanning GC can, and delete-time unmap is not worth implementing"
	fi

	info "the clone now reads back md5 $(read_md5 "${CLONE_DEV}" "${WORKDIR}/clone_discarded.bin")"
	info "  (pattern A is ${HASH_A}, pattern C was ${HASH_C})"

	# The load-bearing assertions of this step.
	if [ "$(read_md5 "${SNAP_DEV}" "${WORKDIR}/snap_after_discard.bin")" = "${HASH_A}" ]; then
		pass "the snapshot is untouched by the clone's discard"
	else
		fail "discarding the clone corrupted the snapshot"
		info "unmap followed the clone-to-parent indirection and deleted objects"
		info "the snapshot still references"
	fi

	if [ "$(read_md5 "${NVME_DEV}" "${WORKDIR}/origin_after_discard.bin")" = "${HASH_B}" ]; then
		pass "the origin is untouched by the clone's discard"
	else
		fail "discarding the clone corrupted the origin"
	fi
	check_target "step 7c" || exit 1
fi

# ==========================================================================
# [8] the snapshot must refuse writes
#
# Not a formality: the snapshot's clusters are what the clone reads through, so a
# write that got in would corrupt the clone as well.
#
# Where it gets refused matters, and is asserted below. It is *not* the host: nvmf
# has no notion of a read-only namespace -- nvmf_subsystem_add_ns takes no such
# option, and lib/nvmf never sets Identify Namespace's nsattr.cwp, the Currently
# Write Protected bit. So the kernel believes the device is writable, submits the
# write, and our io_type_supported rejects it at the bdev layer. dmesg gets an I/O
# error for this step, on purpose.
#
# Both halves are checked, because the failure mode to guard against is somebody
# assuming a protection that does not exist: a snapshot handed to a guest can be
# mkfs'd and mounted rw without complaint, and only fails once it writes.
# ==========================================================================
echo
echo "[8] checking the snapshot rejects writes"

# Recorded as an assertion rather than a comment so it breaks loudly if SPDK ever
# starts reporting nsattr.cwp -- at which point the guidance above changes, and the
# note in vrcow_lvol.c needs updating with it.
SNAP_RO="$(blockdev --getro "${SNAP_DEV}" 2>/dev/null || echo "?")"
if [ "${SNAP_RO}" = "0" ]; then
	pass "the host still sees the snapshot as writable (nvmf reports no write-protect bit)"
elif [ "${SNAP_RO}" = "1" ]; then
	fail "the host now sees the snapshot as read-only -- nvmf grew nsattr.cwp support?"
	info "if so this is good news, but vrcow_lvol.c documents the opposite"
else
	fail "could not read the read-only flag of ${SNAP_DEV}"
fi

if dd if=/dev/zero of="${SNAP_DEV}" bs=4k count=1 seek=$((IO_OFF_MB * 256)) \
		oflag=direct conv=fsync status=none 2>"${WORKDIR}/snap_write.err"; then
	fail "writing to the snapshot succeeded"
else
	pass "writing to the snapshot was rejected"
fi
check_target "step 8" || exit 1

if [ "$(read_md5 "${SNAP_DEV}" "${WORKDIR}/snap_after_write.bin")" = "${HASH_A}" ]; then
	pass "the snapshot is unchanged after the rejected write"
else
	fail "the rejected write still changed the snapshot"
fi

# ==========================================================================
# [9] deletion order
#
# A snapshot with a clone depending on it must not be deletable. If it were, the
# clone would be left reading clusters that had been freed -- the same data that
# would then be handed out to the next allocation.
# ==========================================================================
echo
echo "[9] checking the snapshot cannot be deleted while the clone needs it"

# The namespace has to go first, or s3lvol_lvol_destroy() refuses with -EBUSY
# because the bdev is still claimed -- the same errno the clone dependency
# produces, which would make this step pass without testing anything.
nvme_settle
if ! ${RPC} nvmf_subsystem_remove_ns "${NQN}" "${SNAP_NSID}" \
		>/dev/null 2>"${WORKDIR}/rm_ns.err"; then
	fail "could not remove the snapshot namespace (nsid ${SNAP_NSID})"
	sed 's/^/       /' "${WORKDIR}/rm_ns.err"
	exit 1
fi
pass "snapshot namespace removed, so the bdev is no longer claimed"

# The exit status is the answer: the RPC replies with {bool_value, string_value}
# whether it worked or not, and s3lvol_rpc.py turns bool_value:false into exit 1
# with the reason on stderr.
if raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${SNAP_NAME}")" \
		>"${WORKDIR}/del_snap.json" 2>"${WORKDIR}/del_snap.err"; then
	fail "deleting a snapshot with a live clone succeeded"
else
	pass "deleting the snapshot was refused: $(cat "${WORKDIR}/del_snap.err")"
fi
check_target "step 9" || exit 1

# ==========================================================================
# [10] target log
# ==========================================================================
echo
echo "[10] inspecting the target log"

# The second filter is for the CRT's own logging, not ours. s3lvol asks "does this
# key exist?" with a GET and reads 404 as the answer -- s3_client_aws.c:1538 says
# so outright -- but aws-c-s3 logs every non-2xx at ERROR level ("Meta request
# cannot recover from error 14343... response status=404"). Those are a normal
# part of attach, not a failure.
#
# Only "response status=404" is filtered, not the "Invalid response status from
# request" text beside it. That text is error 14343's generic string and comes with
# *every* non-2xx, so filtering on it would hide a 500 or a 503 -- exactly the
# failures this check exists to catch.
#
# This surfaced when the CRT started being built from unmodified upstream sources:
# the prefix used before came from another project and carried a local change that
# excluded 404 from that log line, so the filter had been incomplete without anyone
# noticing. Worth keeping in mind about the whole check -- it greps for a word, so
# it is only ever as good as the exception list.
LOG_ERRORS="$(grep -inE 'error|failed' "${TGT_LOG}" 2>/dev/null | \
	grep -viE 'already|not found|read-only|is not a snapshot|meta/owner' | \
	grep -viE 'response status=404' || true)"
if [ -n "${LOG_ERRORS}" ]; then
	info "errors mentioned in the log (expected ones filtered out):"
	echo "${LOG_ERRORS}" | head -8 | sed 's/^/       /'
else
	pass "no unexpected errors in the target log"
fi

if grep -q "Failed to register bdev" "${TGT_LOG}"; then
	fail "a bdev registration failed (see the log)"
else
	pass "every derived volume got its bdev"
fi

echo
echo "=== all steps done ==="
