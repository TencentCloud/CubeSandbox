#!/bin/bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Data-plane verification for the s3lvol vbdev (HANDOFF 7.3b).
#
# === What this checks ===
#
# Everything up to now only exercised the control plane: the nvmf loopback was
# brought up and /dev/nvmeXnY appeared, but no byte was ever pushed through it
# (HANDOFF 5.16). This script closes that gap, end to end:
#
#   s3lvol_tgt -> lvstore on S3 -> lvol -> bdev -> nvmf tcp -> kernel nvme
#   -> /dev/nvmeXnY -> dd / fio
#
# The lvstore is created *with a local device* (a sparse file behind bdev_aio),
# which is what selects the WAL write path: writes are logged and acknowledged
# locally, and a background flusher turns them into S3 objects one
# read-modify-write per chunk. Without that the target falls back to writing
# straight to S3, which loses concurrent partial-chunk writes -- so step 5b
# asserts the WAL path really is active instead of assuming it.
#
# The specific things it is meant to catch:
#
#  1. Whether written data reads back byte for byte (dd + sha256, and fio's own
#     verify).
#  2. Whether max_num_segments = 1 actually takes effect. The bs_dev refuses
#     iovcnt > 1 with -ENOTSUP, so if the bdev layer is not splitting, an fio
#     run with iodepth > 1 and large blocks will surface it. The log is grepped
#     for that message afterwards -- an I/O error alone would not tell us which
#     of the two layers was at fault.
#  3. Whether reads of never-written regions return zeroes, which is what
#     blobstore assumes for unallocated chunks.
#  4. Whether concurrent writes into one chunk survive. This is the defect the
#     WAL and the flusher exist to fix (HANDOFF 7.3b-BLOCKER): the fio job in
#     step 8 uses 1 MiB blocks at iodepth 8, which nvmf splits into eight
#     concurrent commands landing in the same chunk.
#  5. Whether the shutdown ordering is right (step 10). Unloading has to drain
#     the flusher, let blobstore write its final metadata, flush that too, close
#     the log, and only then release the journal and the local device. Getting
#     that wrong shows up as an assert or a hang, and nowhere else.
#
# === Why the checks are structured this way ===
#
# Reading back through the same /dev node right after writing proves little on
# its own: the page cache would serve the data even if S3 held garbage. So every
# read that matters uses iflag=direct.
#
# On the WAL path that is no longer enough either -- reads consult the in-process
# overlay first, so disconnecting nvme does not prove the data left the process.
# Step 6b therefore flushes the lvstore, which empties the overlay, and only then
# does step 7 disconnect, reconnect and re-read. At that point the data can only
# have come from S3.
#
# Usage:
#   export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
#   sudo -E ./test/dataplane/run_dataplane_test.sh \
#            -e cos.ap-nanjing.myqcloud.com -b my-bucket -r ap-nanjing
#
# Needs root: nvme connect and /dev access.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TOOLS_DIR="${REPO_ROOT}/test/tools"
SPDK_ROOT="${SPDK_ROOT:-/data/home/cow/spdk}"

TGT_BIN="${REPO_ROOT}/app/s3lvol_tgt/s3lvol_tgt"
RPC_PY="${SPDK_ROOT}/scripts/rpc.py"
# Every SPDK RPC needs the socket named explicitly: rpc.py defaults to
# /var/tmp/spdk.sock, while the target now listens on /var/run/s3lvol.sock.
RPC="${RPC_PY} -s /var/run/s3lvol.sock"

ENDPOINT=""
BUCKET=""
REGION="ap-nanjing"
LVS_NAME="dpvs"
LVOL_NAME="vol0"
NQN="nqn.2026-08.io.spdk:s3dp"
LISTEN_ADDR="127.0.0.1"
LISTEN_PORT="4420"

# 8 GiB lvstore, 2 GiB volume grown to 4 GiB in step 8b -- all of it thin, so
# nothing here costs anything until it is written, and this test writes a few MiB.
#
# The volume is 2 GiB rather than 1 so that step 8b has a smaller *whole* GiB to
# attempt a shrink with: sizes are in GiB now (rcow_create_lvol's size_gib,
# rcow_resize_lvol's size_gib), so "half the volume" has to be expressible.
#
# LVOL_SIZE keeps the byte value: the device-size assertions compare against
# `blockdev --getsize64`, and the dd offsets past the old end are in MiB.
CAPACITY_GIB=8
LVOL_GIB=2
LVOL_SIZE=$((LVOL_GIB * 1024 * 1024 * 1024))

# Local device carrying the metadata journal and the WAL.
#
# A sparse file behind the upstream bdev_aio, not a hand-written in-memory bdev:
# a real bdev module also exercises the 4 KiB alignment and O_DIRECT constraints,
# and it is one less implementation to maintain (same reasoning as the journal
# unit test).
#
# *Passing this is what selects the WAL write path.* Without it the target falls
# back to writing straight to S3, which loses concurrent partial-chunk writes --
# the very defect this test exists to catch. Step 5b asserts the path really is
# active rather than trusting that it is.
# Deliberately not under /tmp: that is tmpfs on many hosts, and a write-ahead log
# on a RAM-backed file proves nothing about durability. Override with
# S3LVOL_WAL_FILE if /data is not writable here.
WAL_FILE="${S3LVOL_WAL_FILE:-/data/s3lvol_dp_wal.img}"
WAL_BDEV="dp_wal0"
JOURNAL_MB="${S3LVOL_JOURNAL_MB:-64}"
WAL_MB="${S3LVOL_WAL_MB:-256}"
# Journal + WAL + super, plus room for the chunk cache region that local_dev
# carves out of whatever is left.
WAL_FILE_MB=$((JOURNAL_MB + WAL_MB + 128))

# Two reactors on purpose.
#
# s3_ctx and the chunk map are single-owner-thread with no locking, but blobstore
# calls bs_dev entry points from whatever thread holds the channel, and nvmf runs
# I/O on a dedicated poll group thread -- it creates one per core with its own
# spdk_thread (spdk/module/event/subsystems/nvmf/nvmf_tgt.c:226), which is never
# the thread that created the lvstore.
#
# Note that reactor count is irrelevant to that mismatch: one reactor can host
# many spdk_threads, so even -m 0x1 hits it. s3_bs_dev therefore bounces work to
# the owner thread and bounces the completion back. Using two reactors here
# exercises that path rather than hiding it.
TGT_CPUMASK="${S3LVOL_TGT_CPUMASK:-0x3}"

# Extra target arguments. Hugepages are preferred, but a machine with
# HugePages_Total = 0 cannot start DPDK at all, so fall back to --no-huge rather
# than requiring the operator to reconfigure the system. Set S3LVOL_TGT_ARGS to
# override.
if [ -n "${S3LVOL_TGT_ARGS:-}" ]; then
	TGT_ARGS="${S3LVOL_TGT_ARGS}"
elif [ "$(cat /proc/sys/vm/nr_hugepages 2>/dev/null || echo 0)" -gt 0 ]; then
	TGT_ARGS=""
else
	TGT_ARGS="--no-huge -s 1024 -r /var/run/s3lvol.sock"
fi

WORKDIR="$(mktemp -d /tmp/s3lvol_dp.XXXXXX)"
TGT_LOG="${WORKDIR}/s3lvol_tgt.log"
TGT_PID=""
NVME_DEV=""
CONNECTED=0

# Set once the target is seen to have died unexpectedly. Kept separate from FAIL
# because it also forces the logs to be preserved.
TGT_CRASHED=0

# The pid is remembered separately: check_target() clears TGT_PID after reaping,
# and cleanup still needs the pid to look the core dump up.
CRASHED_PID=""

# Set when teardown itself goes wrong -- a failed unload, or a target that
# ignores SIGTERM. Neither shows up in FAIL, because both happen after the last
# assertion has already passed, and the old code therefore threw away the logs of
# exactly the runs that most needed them.
TEARDOWN_ANOMALY=0

# Set while the lvstore is up, so teardown knows to unload it first.
LVS_CREATED=0

# Sticky version of the above: LVS_CREATED goes back to 0 once the lvstore is
# unloaded, but its objects in S3 outlive it and still have to be cleaned up.
LVS_WAS_CREATED=0

# Set once the local device file exists, so teardown removes it.
WAL_FILE_CREATED=0

PASS=0
FAIL=0

usage()
{
	cat <<EOT
Usage: $0 -e <endpoint> -b <bucket> [-r region] [-n lvs_name]

  -e  S3/COS endpoint, e.g. cos.ap-nanjing.myqcloud.com
  -b  bucket name
  -r  region (default: ${REGION})
  -n  lvstore name (default: ${LVS_NAME})

Credentials are read from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.

Environment:
  S3LVOL_KEEP_S3    keep the S3 objects even after a clean run
  S3LVOL_KEEP_LOGS  keep the target log even after a clean run

A clean run removes its own S3 objects and local device image; a failing one keeps
both, and prints the command to remove them.
EOT
}

while getopts "e:b:r:n:h" opt; do
	case "${opt}" in
	e) ENDPOINT="${OPTARG}" ;;
	b) BUCKET="${OPTARG}" ;;
	r) REGION="${OPTARG}" ;;
	n) LVS_NAME="${OPTARG}" ;;
	h) usage; exit 0 ;;
	*) usage; exit 1 ;;
	esac
done

pass() { echo "  [PASS] $*"; PASS=$((PASS + 1)); }
fail() { echo "  [FAIL] $*"; FAIL=$((FAIL + 1)); }
info() { echo "  ---- $*"; }

# ==========================================================================
# Raw JSON-RPC helper
#
# rpc.py only knows the subcommands it has registered, and the s3lvol module's
# RPCs are not among them -- the --plugin mechanism of S12.2 does not exist yet
# (HANDOFF 7.3a). So custom methods are sent as raw JSON over the unix socket,
# exactly as HANDOFF documents.
#
# Built-in methods (nvmf_*, bdev_get_bdevs, ...) still go through rpc.py, which
# is less fragile for those.
# ==========================================================================
RPC_SOCK="/var/run/s3lvol.sock"

raw_rpc()
{
	python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" --timeout 180 \
		"$1" "${2:-}"
}

# One key out of an lvstore's write_path stats object.
#
# Deliberately not jq: it is not installed everywhere, while python is already a
# dependency of raw_rpc. Prints "missing" rather than failing, so callers can
# report a useful message instead of an empty comparison.
wal_stat()
{
	raw_rpc rcow_get_lvstores "" 2>/dev/null | python3 -c '
import json, sys
key = sys.argv[1]
try:
    stores = json.load(sys.stdin)
except ValueError:
    print("missing")
    sys.exit(0)
for st in stores:
    wp = st.get("write_path", {})
    if key in wp:
        print(wp[key])
        break
else:
    print("missing")
' "$1"
}

# ==========================================================================
# Liveness checks
#
# A dead target turns every later dd into a confusing transport error
# ("no usable path", "failed to connect socket: -111"), which says nothing about
# which I/O actually killed it. So probe after every step that pushes data and
# stop at the first sign of death, naming the step.
# ==========================================================================
target_alive()
{
	[ -n "${TGT_PID}" ] && kill -0 "${TGT_PID}" 2>/dev/null
}

check_target()
{
	local step="$1"

	if target_alive; then
		return 0
	fi

	TGT_CRASHED=1
	CRASHED_PID="${TGT_PID}"
	fail "the target died during: ${step}"

	# Reap it to recover the signal. 128+n is how the shell reports a death
	# by signal n, so 139 is SIGSEGV and 134 is SIGABRT (an assert).
	local st=0
	wait "${TGT_PID}" 2>/dev/null || st=$?
	case "${st}" in
	139) info "died on SIGSEGV (signal 11)" ;;
	134) info "died on SIGABRT (signal 6), most likely an assert" ;;
	*)   info "exit status ${st}" ;;
	esac

	TGT_PID=""
	return 1
}

# Let udev finish with the device before pulling it out from under it.
#
# A namespace rescan -- which a resize triggers, and which also happens around a
# disconnect -- makes udev re-probe the device, and those probes are buffered
# reads. Disconnecting while they are still in flight leaves this in dmesg:
#
#   block nvme0n1: no available path - failing I/O
#   Buffer I/O error on dev nvme0n1, logical block 97, async page read
#
# Harmless, but at a glance indistinguishable from a real I/O failure, and that
# distinction has already cost time once: a genuine stall was first read as an I/O
# error because the teardown artefacts around it looked like the fault. Keeping
# dmesg clean on normal runs is what makes an abnormal run legible.
nvme_settle()
{
	udevadm settle --timeout=5 >/dev/null 2>&1 || true
}

# ==========================================================================
# Teardown
#
# Runs on every exit path. Order matters: disconnect nvme before killing the
# target, otherwise the kernel is left with a controller whose transport just
# vanished, and the next connect can hang.
# ==========================================================================
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
		# Unload first when the lvstore is still up: that is what pushes
		# the last acknowledged writes into S3 and closes the log. Killing
		# the target instead is safe -- the data is in the log -- but it
		# leaves a tail that only the attach path can recover, and the
		# attach path does not exist yet.
		if [ "${LVS_CREATED}" -eq 1 ]; then
			# delete rather than unload on a clean run: unload keeps the
			# lvstore, which is now what it means -- the bstore.json entry
			# stays behind for a recovery script to find. That is right in
			# production and wrong here, where the objects are about to be
			# removed anyway, so the entry would be left pointing at data
			# that no longer exists. After a failure keep both: the entry
			# and the objects are the evidence.
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
				# Not just noted -- counted. A failed unload means
				# either the lvstore is wedged or a bdev is still
				# claimed, and both deserve the evidence that a
				# zero-FAIL run would otherwise delete.
				info "lvstore ${CLEANUP_RPC#rcow_} failed; data stays in the WAL"
				TEARDOWN_ANOMALY=1
			fi
		fi

		kill -TERM "${TGT_PID}" 2>/dev/null
		for _ in $(seq 50); do
			kill -0 "${TGT_PID}" 2>/dev/null || break
			sleep 0.1
		done
		if kill -0 "${TGT_PID}" 2>/dev/null; then
			# A target that ignores SIGTERM is stuck, not slow: SPDK
			# handles signals on the reactor, so nothing being handled
			# means the reactor is not running. (s3_journal_destroy()
			# also asserts when appends are still in flight, which is a
			# different way to get here.)
			#
			# This is the *only* moment the process is both wedged and
			# still alive, so take the backtraces here. Five seconds
			# after this the memory is gone and all that is left is
			# kernel messages about a controller that disappeared --
			# which is exactly how one stall already got misread as an
			# I/O error.
			info "target ignored SIGTERM: it is stuck, not slow"
			TEARDOWN_ANOMALY=1
			if command -v gdb >/dev/null 2>&1; then
				gdb -p "${TGT_PID}" -batch \
					-ex "set pagination off" \
					-ex "thread apply all bt" \
					>"${WORKDIR}/gdb_teardown.txt" 2>&1 || true
				info "backtraces saved to ${WORKDIR}/gdb_teardown.txt"
				grep -E '^Thread|^#[0-9]+ ' \
					"${WORKDIR}/gdb_teardown.txt" 2>/dev/null | \
					head -20 | sed 's/^/       /'
			fi
			kill -KILL "${TGT_PID}" 2>/dev/null
		else
			info "target stopped cleanly"
		fi
	elif [ "${TGT_CRASHED}" -eq 1 ]; then
		info "target had already crashed"
	fi

	# Once the target is down, so nothing is still uploading into the prefix
	# being deleted. Only a clean run drops the objects: after a failure they
	# are the evidence, and the WAL image kept below is only half the picture
	# without them.
	if [ "${LVS_WAS_CREATED}" -eq 1 ]; then
		if [ "${FAIL}" -eq 0 ] && [ "${rc}" -eq 0 ] && \
		   [ "${TGT_CRASHED}" -eq 0 ] && [ "${TEARDOWN_ANOMALY}" -eq 0 ] && \
		   [ -z "${S3LVOL_KEEP_S3:-}" ]; then
			if python3 "${TOOLS_DIR}/s3_prefix_rm.py" \
					-e "${ENDPOINT}" -b "${BUCKET}" -r "${REGION}" \
					-p "${LVS_NAME}/" \
					>"${WORKDIR}/s3_rm.log" 2>&1; then
				info "S3: $(cat "${WORKDIR}/s3_rm.log")"
			else
				info "S3 cleanup failed, objects left under '${LVS_NAME}/':"
				sed 's/^/       /' "${WORKDIR}/s3_rm.log"
			fi
		else
			info "S3 objects kept under prefix '${LVS_NAME}/' in bucket ${BUCKET}"
			info "inspect: ${TOOLS_DIR}/s3_prefix_rm.py -e ${ENDPOINT} -b ${BUCKET} -r ${REGION} -p ${LVS_NAME}/ --list"
			info "remove:  ${TOOLS_DIR}/s3_prefix_rm.py -e ${ENDPOINT} -b ${BUCKET} -r ${REGION} -p ${LVS_NAME}/"
		fi
	fi

	# Keep the local device image when anything failed: it holds the WAL and the
	# journal, which is exactly what you want to look at after a data-integrity
	# failure. Only remove it on a clean run.
	if [ "${WAL_FILE_CREATED}" -eq 1 ]; then
		if [ "${FAIL}" -eq 0 ] && [ "${rc}" -eq 0 ] && \
		   [ "${TGT_CRASHED}" -eq 0 ] && [ "${TEARDOWN_ANOMALY}" -eq 0 ]; then
			rm -f "${WAL_FILE}"
		else
			info "local device image kept at ${WAL_FILE}"
		fi
	fi

	# Preserve the log whenever anything went wrong, and *always* when the
	# target crashed.
	#
	# Getting this wrong already cost one debugging session: the previous
	# version deleted WORKDIR on a zero exit status, and a crash that happened
	# after the last check still exited zero, so the only record of why the
	# target died was removed before it could be read. Bias towards keeping.
	if [ "${FAIL}" -gt 0 ] || [ "${rc}" -ne 0 ] || \
	   [ "${TGT_CRASHED}" -eq 1 ] || [ "${TEARDOWN_ANOMALY}" -eq 1 ] || \
	   [ -n "${S3LVOL_KEEP_LOGS:-}" ]; then
		echo "  ---- logs kept in ${WORKDIR}"
		echo "  ---- target log: ${TGT_LOG}"
		echo "  ---- last 40 lines:"
		tail -40 "${TGT_LOG}" 2>/dev/null | sed 's/^/       /'

		# Point at the core dump if there is one. On a systemd-coredump
		# host, cores do not land in the cwd, so ask coredumpctl instead of
		# globbing for core* and reporting nothing.
		local cpid="${CRASHED_PID:-${TGT_PID}}"
		if command -v coredumpctl >/dev/null 2>&1 && [ -n "${cpid}" ]; then
			echo "  ---- core dump for pid ${cpid} (if any):"
			coredumpctl info "${cpid}" 2>/dev/null | \
				sed -n '1,40p' | sed 's/^/       /' || \
				echo "       none recorded"
			echo "  ---- full stack: coredumpctl info ${cpid}"
			echo "  ---- interactive: coredumpctl gdb ${cpid}"
		else
			local core
			core="$(ls -t core* 2>/dev/null | head -1 || true)"
			if [ -n "${core}" ]; then
				echo "  ---- core file: $(pwd)/${core}"
				echo "  ---- inspect with: gdb ${TGT_BIN} ${core}"
			fi
		fi
	else
		rm -rf "${WORKDIR}"
	fi
}
trap cleanup EXIT

# ==========================================================================
# Preflight
# ==========================================================================
echo "=== s3lvol data-plane verification ==="
echo

if [ -z "${ENDPOINT}" ] || [ -z "${BUCKET}" ]; then
	usage
	exit 1
fi
if [ "$(id -u)" -ne 0 ]; then
	echo "must run as root (nvme connect)" >&2
	exit 1
fi
if [ -z "${AWS_ACCESS_KEY_ID:-}" ] || [ -z "${AWS_SECRET_ACCESS_KEY:-}" ]; then
	echo "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY must be set" >&2
	echo "(with sudo, remember -E)" >&2
	exit 1
fi
for tool in nvme dd sha256sum truncate; do
	command -v "${tool}" >/dev/null 2>&1 || {
		echo "missing required tool: ${tool}" >&2
		exit 1
	}
done
[ -x "${TGT_BIN}" ] || { echo "target not built: ${TGT_BIN}" >&2; exit 1; }
[ -x "${RPC_PY}" ] || { echo "rpc.py not found: ${RPC_PY}" >&2; exit 1; }

# A pass against a stale binary is evidence pointing the wrong way -- it has
# happened, and nothing in the output showed it. See the script for the details.
if [ -z "${S3LVOL_SKIP_FRESH_CHECK:-}" ]; then
	"${TOOLS_DIR}/check_binary_fresh.sh" "${TGT_BIN}" || exit 1
fi

HAVE_FIO=0
command -v fio >/dev/null 2>&1 && HAVE_FIO=1

modprobe nvme_tcp 2>/dev/null || true
lsmod | grep -q nvme_tcp || {
	echo "nvme_tcp module not loaded; cannot use the loopback" >&2
	exit 1
}

# ==========================================================================
# [1] Start the target
# ==========================================================================
echo "[1] starting s3lvol_tgt"

# Allow a core dump. Where it lands is decided by the system-wide
# /proc/sys/kernel/core_pattern, which is deliberately not modified here; the
# point is only that the limit does not silently suppress the core. Without this,
# a SIGSEGV leaves nothing but the log to work from.
ulimit -c unlimited 2>/dev/null || info "could not raise the core dump limit"
info "core_pattern: $(cat /proc/sys/kernel/core_pattern 2>/dev/null || echo unknown)"

info "target args: -m ${TGT_CPUMASK} ${TGT_ARGS}"
# TGT_ARGS is intentionally unquoted: it carries multiple words.
# shellcheck disable=SC2086
"${TGT_BIN}" -m "${TGT_CPUMASK}" ${TGT_ARGS} >"${TGT_LOG}" 2>&1 &
TGT_PID=$!

for _ in $(seq 100); do
	${RPC} spdk_get_version >/dev/null 2>&1 && break
	kill -0 "${TGT_PID}" 2>/dev/null || break
	sleep 0.2
done

if ! ${RPC} spdk_get_version >/dev/null 2>&1; then
	fail "target did not come up"
	exit 1
fi
pass "target is up (pid ${TGT_PID})"

# ==========================================================================
# [1b] Local device for the journal and the WAL
# ==========================================================================
echo
echo "[1b] attaching the local device (aio on a sparse file)"

rm -f "${WAL_FILE}"
if ! truncate -s "${WAL_FILE_MB}M" "${WAL_FILE}" 2>"${WORKDIR}/truncate.err"; then
	fail "could not create ${WAL_FILE}"
	sed 's/^/       /' "${WORKDIR}/truncate.err"
	exit 1
fi
WAL_FILE_CREATED=1

# 4096, matching S3LVOL_BLOCK_SIZE. A 512-byte aio bdev would work too, but then
# every WAL write would be a partial block from the bdev's point of view.
if ! ${RPC} bdev_aio_create "${WAL_FILE}" "${WAL_BDEV}" 4096 \
		>/dev/null 2>"${WORKDIR}/aio.err"; then
	fail "bdev_aio_create"
	sed 's/^/       /' "${WORKDIR}/aio.err"
	exit 1
fi
pass "local device '${WAL_BDEV}': ${WAL_FILE_MB} MiB (journal ${JOURNAL_MB}, WAL ${WAL_MB})"

# ==========================================================================
# [2] lvstore + lvol
# ==========================================================================
echo
echo "[2] creating the lvstore and lvol on S3"

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
pass "lvstore created (${CAPACITY_GIB} GiB)"
LVS_CREATED=1
LVS_WAS_CREATED=1

if ! raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
		"${LVOL_NAME}" "${LVOL_GIB}")" \
		>"${WORKDIR}/lvol.json" 2>"${WORKDIR}/lvol.err"; then
	fail "rcow_create_lvol"
	sed 's/^/       /' "${WORKDIR}/lvol.err"
	exit 1
fi
pass "lvol created ($((LVOL_SIZE / 1024 / 1024)) MiB)"

BDEV_NAME="${LVS_NAME}/${LVOL_NAME}"

# Confirm the geometry the bdev advertises. max_num_segments = 1 is the whole
# reason the bs_dev can get away with an iovcnt == 1 restriction.
${RPC} bdev_get_bdevs -b "${BDEV_NAME}" >"${WORKDIR}/bdev.json" 2>&1 || true
if grep -q '"num_blocks"' "${WORKDIR}/bdev.json"; then
	info "$(grep -o '"num_blocks": [0-9]*' "${WORKDIR}/bdev.json" | head -1)"
	info "$(grep -o '"block_size": [0-9]*' "${WORKDIR}/bdev.json" | head -1)"
	pass "bdev '${BDEV_NAME}' is registered"
else
	fail "bdev '${BDEV_NAME}' not found"
	exit 1
fi

# ==========================================================================
# [3] nvmf + loopback
# ==========================================================================
echo
echo "[3] exporting over nvmf-tcp and connecting the loopback"

${RPC} nvmf_create_transport -t TCP >/dev/null 2>&1 || \
	info "TCP transport already present"

${RPC} nvmf_create_subsystem "${NQN}" -a -s S3DP00000000000001 \
	>/dev/null 2>&1 || { fail "nvmf_create_subsystem"; exit 1; }

${RPC} nvmf_subsystem_add_ns "${NQN}" "${BDEV_NAME}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns"; exit 1; }

${RPC} nvmf_subsystem_add_listener "${NQN}" \
	-t tcp -a "${LISTEN_ADDR}" -s "${LISTEN_PORT}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_listener"; exit 1; }
pass "namespace exported"

# Note which nvme devices exist beforehand so the new one can be identified;
# the host may already have unrelated nvme controllers.
BEFORE="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"

if ! nvme connect -t tcp -a "${LISTEN_ADDR}" -s "${LISTEN_PORT}" -n "${NQN}" \
		>"${WORKDIR}/connect.log" 2>&1; then
	fail "nvme connect"
	sed 's/^/       /' "${WORKDIR}/connect.log"
	exit 1
fi
CONNECTED=1

for _ in $(seq 50); do
	AFTER="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
	NVME_DEV="$(comm -13 <(echo "${BEFORE}") <(echo "${AFTER}") | head -1)"
	[ -n "${NVME_DEV}" ] && [ -b "${NVME_DEV}" ] && break
	sleep 0.2
done

if [ -z "${NVME_DEV}" ] || [ ! -b "${NVME_DEV}" ]; then
	fail "no new nvme block device appeared"
	exit 1
fi
pass "block device is ${NVME_DEV}"

# Record the kernel-side timeouts, because they set the clock on every stall this
# test can observe, and they are invisible in the test output otherwise.
#
# Worth having on record: when the target stops answering, what the host does next
# is entirely decided by these three numbers. keep-alive expiring is what turns
# "the target is wedged" into "starting error recovery" in dmesg; ctrl_loss_tmo is
# how long I/O gets requeued rather than failed, which is why a stalled fio sits
# there instead of exiting with EIO. A stall analysed without them reads like a
# random pile of kernel messages -- one already did.
NVME_CTRL="$(basename "${NVME_DEV}" | sed 's/n[0-9]*$//')"
NVME_TMO=""
for attr in kato ctrl_loss_tmo reconnect_delay; do
	val="$(cat "/sys/class/nvme/${NVME_CTRL}/${attr}" 2>/dev/null || true)"
	[ -n "${val}" ] && NVME_TMO="${NVME_TMO}${attr}=${val} "
done
# Not all kernels export them -- 5.4 exports none of the three -- so the whole
# attribute list is dumped instead of printing three "n/a"s. Next time a stall
# needs explaining, that file says what this host was actually willing to tell us.
ls "/sys/class/nvme/${NVME_CTRL}/" >"${WORKDIR}/nvme_attrs.txt" 2>&1 || true
if [ -n "${NVME_TMO}" ]; then
	info "host nvme timeouts: ${NVME_TMO}(controller ${NVME_CTRL})"
else
	info "host nvme timeouts are not exported by this kernel ($(uname -r)); attribute list in nvme_attrs.txt"
fi

# The kernel starts probing the namespace as soon as it attaches (identify, and
# on some setups a partition scan), so the target can die here before any dd has
# run. A previous session lost two seconds' worth of evidence exactly this way.
sleep 2
check_target "step 3, right after nvme connect" || exit 1

DEV_SIZE="$(blockdev --getsize64 "${NVME_DEV}")"
if [ "${DEV_SIZE}" -eq "${LVOL_SIZE}" ]; then
	pass "device size matches the lvol (${DEV_SIZE} bytes)"
else
	fail "device size ${DEV_SIZE} != lvol size ${LVOL_SIZE}"
fi

# ==========================================================================
# [4] Zero-read of an untouched region
#
# Do this before writing anything: blobstore assumes unallocated chunks read
# back as zeroes, and the bs_dev must return zeroes rather than error out on a
# chunk that has no S3 object yet.
# ==========================================================================
echo
echo "[4] reading a never-written region (must be all zeroes)"

dd if="${NVME_DEV}" of="${WORKDIR}/zeros.bin" bs=1M count=4 \
	iflag=direct status=none 2>"${WORKDIR}/dd_zero.log"
if [ $? -ne 0 ]; then
	fail "reading the fresh device"
	sed 's/^/       /' "${WORKDIR}/dd_zero.log"
else
	# tr -d '\0' leaves nothing behind if every byte was zero.
	if [ -s "${WORKDIR}/zeros.bin" ] && \
	   [ "$(tr -d '\0' < "${WORKDIR}/zeros.bin" | wc -c)" -eq 0 ]; then
		pass "unwritten region reads back as 4 MiB of zeroes"
	else
		fail "unwritten region is not zero-filled"
	fi
fi

check_target "step 4, reading an unwritten region" || exit 1

# ==========================================================================
# [5] dd write/read round trip, verified by hash
#
# Uses urandom rather than a repeating pattern: a pattern can hide offset
# errors, since a misplaced read of the same pattern still compares equal.
# oflag/iflag=direct keeps the page cache out of it.
# ==========================================================================
echo
echo "[5] dd round trip, 16 MiB of random data (O_DIRECT)"

dd if=/dev/urandom of="${WORKDIR}/src.bin" bs=1M count=16 status=none
SRC_HASH="$(sha256sum "${WORKDIR}/src.bin" | cut -d' ' -f1)"

if ! dd if="${WORKDIR}/src.bin" of="${NVME_DEV}" bs=1M count=16 \
		oflag=direct conv=fsync status=none 2>"${WORKDIR}/dd_w.log"; then
	fail "dd write"
	sed 's/^/       /' "${WORKDIR}/dd_w.log"
	# A failed write is exactly where the target is most likely to have died,
	# and the reason lives in its log rather than in dd's errno.
	check_target "step 5, the 16 MiB write" || exit 1
else
	pass "wrote 16 MiB"
	check_target "step 5, the 16 MiB write" || exit 1

	if ! dd if="${NVME_DEV}" of="${WORKDIR}/dst.bin" bs=1M count=16 \
			iflag=direct status=none 2>"${WORKDIR}/dd_r.log"; then
		fail "dd read"
		sed 's/^/       /' "${WORKDIR}/dd_r.log"
	else
		DST_HASH="$(sha256sum "${WORKDIR}/dst.bin" | cut -d' ' -f1)"
		if [ "${SRC_HASH}" = "${DST_HASH}" ]; then
			pass "read back identical (sha256 ${SRC_HASH:0:16}...)"
		else
			fail "data mismatch: ${SRC_HASH:0:16}... != ${DST_HASH:0:16}..."
			cmp "${WORKDIR}/src.bin" "${WORKDIR}/dst.bin" \
				2>&1 | head -3 | sed 's/^/       /'
		fi
	fi
fi

# ==========================================================================
# [5b] Is the WAL write path actually in use?
#
# Everything below would also pass on the direct-to-S3 path, right up to the
# point where it silently drops concurrent writes. So check explicitly rather
# than assume: a typo in the wal_bdev parameter would otherwise turn this whole
# script into a test of the wrong code path.
# ==========================================================================
echo
echo "[5b] confirming the WAL write path is active"

WAL_ATTACHED="$(wal_stat wal_attached)"
WAL_WRITES="$(wal_stat wal_writes)"

if [ "${WAL_ATTACHED}" = "True" ]; then
	pass "the lvstore reports the WAL path attached"
else
	fail "the WAL is NOT attached (wal_attached=${WAL_ATTACHED})"
	info "every result below would describe the direct-to-S3 path instead"
fi

if [ "${WAL_WRITES}" != "missing" ] && [ "${WAL_WRITES}" -gt 0 ] 2>/dev/null; then
	pass "${WAL_WRITES} writes were acknowledged from the log"
else
	fail "no write went through the log (wal_writes=${WAL_WRITES})"
fi

info "chunks flushed to S3 so far: $(wal_stat chunks_flushed)"
info "overlay: $(wal_stat overlay_bytes) bytes across $(wal_stat overlay_chunks) chunks"

check_target "step 5b, reading the write-path stats" || exit 1

# ==========================================================================
# [6] Offset write, to catch LBA-to-chunk arithmetic errors
#
# Writes at 33 MiB, deliberately not chunk aligned, so it lands in the middle of
# a chunk and forces the read-modify-write path.
# ==========================================================================
echo
echo "[6] unaligned write at offset 33 MiB (exercises RMW)"

dd if=/dev/urandom of="${WORKDIR}/off_src.bin" bs=4k count=7 status=none
OFF_HASH="$(sha256sum "${WORKDIR}/off_src.bin" | cut -d' ' -f1)"
OFF_BYTES=$((33 * 1024 * 1024))

if ! dd if="${WORKDIR}/off_src.bin" of="${NVME_DEV}" bs=4k count=7 \
		seek=$((OFF_BYTES / 4096)) oflag=direct conv=fsync status=none \
		2>"${WORKDIR}/dd_ow.log"; then
	fail "offset write"
	sed 's/^/       /' "${WORKDIR}/dd_ow.log"
	check_target "step 6, the unaligned RMW write" || exit 1
else
	check_target "step 6, the unaligned RMW write" || exit 1
	if ! dd if="${NVME_DEV}" of="${WORKDIR}/off_dst.bin" bs=4k count=7 \
			skip=$((OFF_BYTES / 4096)) iflag=direct status=none \
			2>"${WORKDIR}/dd_or.log"; then
		fail "offset read"
	else
		if [ "${OFF_HASH}" = "$(sha256sum "${WORKDIR}/off_dst.bin" | cut -d' ' -f1)" ]; then
			pass "unaligned 28 KiB at 33 MiB round-tripped"
		else
			fail "unaligned write/read mismatch"
		fi
	fi
fi

# Re-read the first 16 MiB: the offset write must not have disturbed it. This is
# what catches a chunk index computed from the wrong base.
if dd if="${NVME_DEV}" of="${WORKDIR}/recheck.bin" bs=1M count=16 \
		iflag=direct status=none 2>/dev/null; then
	if [ "${SRC_HASH}" = "$(sha256sum "${WORKDIR}/recheck.bin" | cut -d' ' -f1)" ]; then
		pass "earlier data is still intact after the offset write"
	else
		fail "the offset write corrupted earlier data"
	fi
fi

check_target "step 6, re-reading after the offset write" || exit 1

# ==========================================================================
# [6b] Force everything into S3
#
# This is what makes the next step mean something. Reads consult the overlay
# first, and the overlay lives in the target process -- so disconnecting nvme
# does *not* prove the data is in S3 the way it used to on the direct-to-S3 path.
# After a flush the overlay is empty, so any later read has to come from S3.
# ==========================================================================
echo
echo "[6b] flushing the lvstore into S3"

if ! raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS_NAME}")" \
		>/dev/null 2>"${WORKDIR}/flush.err"; then
	fail "rcow_flush_lvstore"
	sed 's/^/       /' "${WORKDIR}/flush.err"
else
	pass "flush completed"
fi

check_target "step 6b, flushing to S3" || exit 1

OVERLAY_LEFT="$(wal_stat overlay_bytes)"
FLUSHED="$(wal_stat chunks_flushed)"
FLUSH_FAILS="$(wal_stat flush_failures)"

if [ "${OVERLAY_LEFT}" = "0" ]; then
	pass "the overlay is empty, so reads can no longer be served from RAM"
else
	fail "the overlay still holds ${OVERLAY_LEFT} bytes after a flush"
fi
if [ "${FLUSHED}" != "missing" ] && [ "${FLUSHED}" -gt 0 ] 2>/dev/null; then
	pass "${FLUSHED} chunks were uploaded to S3"
else
	fail "no chunk reached S3 (chunks_flushed=${FLUSHED})"
fi
if [ "${FLUSH_FAILS}" = "0" ]; then
	pass "no upload failed"
else
	info "uploads that failed and were retried: ${FLUSH_FAILS}"
fi

# ==========================================================================
# [7] Durability across a reconnect
#
# The strongest check in this script. Disconnect nvme, reconnect, then read: no
# page cache and no SPDK-side channel survives that, and step 6b emptied the
# overlay, so the data can only come back from S3.
# ==========================================================================
echo
echo "[7] disconnect / reconnect, then re-read (proves it came from S3)"

nvme_settle
nvme disconnect -n "${NQN}" >/dev/null 2>&1
CONNECTED=0
sleep 1

BEFORE="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
if ! nvme connect -t tcp -a "${LISTEN_ADDR}" -s "${LISTEN_PORT}" -n "${NQN}" \
		>"${WORKDIR}/reconnect.log" 2>&1; then
	fail "nvme reconnect"
	sed 's/^/       /' "${WORKDIR}/reconnect.log"
else
	CONNECTED=1
	for _ in $(seq 50); do
		AFTER="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
		NVME_DEV="$(comm -13 <(echo "${BEFORE}") <(echo "${AFTER}") | head -1)"
		[ -n "${NVME_DEV}" ] && [ -b "${NVME_DEV}" ] && break
		sleep 0.2
	done

	if [ -z "${NVME_DEV}" ] || [ ! -b "${NVME_DEV}" ]; then
		fail "device did not reappear after reconnect"
	else
		info "reconnected as ${NVME_DEV}"
		if dd if="${NVME_DEV}" of="${WORKDIR}/after.bin" bs=1M count=16 \
				iflag=direct status=none 2>/dev/null; then
			if [ "${SRC_HASH}" = "$(sha256sum "${WORKDIR}/after.bin" | cut -d' ' -f1)" ]; then
				pass "data survived the reconnect: it really is in S3"
			else
				fail "data differs after reconnect"
			fi
		else
			fail "read after reconnect"
		fi
	fi
fi

check_target "step 7, re-reading after the reconnect" || exit 1

# ==========================================================================
# [8] fio with verify, and the max_num_segments question
#
# iodepth > 1 with 1 MiB blocks is what would produce multi-segment I/O if the
# bdev layer were not honouring max_num_segments = 1. fio's own verify catches
# corruption; the log grep afterwards tells us whether the bs_dev ever had to
# refuse an iovcnt > 1, which an I/O error alone would not distinguish.
# ==========================================================================
echo
echo "[8] fio with verify"

# Every fio run is wrapped in a timeout.
#
# Without one, a stalled I/O hangs the whole script indefinitely, and the trap
# then tears the target down -- destroying the only state that could explain the
# stall. That happened: a 4k randrw run stopped making progress, the teardown
# SIGKILLed the target, and the kernel logged a screenful of "no available path -
# failing I/O" that had nothing to do with the cause. The log directory was then
# removed because FAIL was still zero.
#
# So: bound it, and when the bound is hit, collect evidence *before* anything is
# torn down. Which of these three answers comes back narrows it down a lot:
#
#   - fio's own status line -> is it 0 IOPS, or merely slow?
#   - does the target still answer RPC -> is the owner thread stuck, or just I/O?
#   - thread backtraces -> where exactly.
FIO_TIMEOUT="${S3LVOL_FIO_TIMEOUT:-240}"

# fio appends a status line at this interval, so a timed-out run still shows
# whether it was progressing.
FIO_COMMON="--aux-path=${WORKDIR} --status-interval=15"

diagnose_fio_stall()
{
	local log="$1"

	info "fio's last reported status:"
	grep -E 'IOPS|iops|bw=|read:|write:' "${log}" 2>/dev/null | tail -6 | \
		sed 's/^/       /'

	# A short timeout of its own: if the target is wedged, this must not hang
	# too. Answering at all means the poller and the RPC thread are alive, which
	# points at the I/O path rather than at a stuck reactor.
	if timeout 20 python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" \
			--timeout 15 rcow_get_lvstores \
			>"${WORKDIR}/stall_stats.json" 2>&1; then
		info "the target still answers RPC, so the reactor is alive:"
		python3 -c '
import json, sys
try:
    s = json.load(open(sys.argv[1]))[0]["write_path"]
except Exception:
    sys.exit(0)
for k in ("wal_writes", "overlay_bytes", "chunks_flushed", "upload_failures",
          "journal_used", "ckpt_done"):
    if k in s:
        print("%-16s %s" % (k, s[k]))
' "${WORKDIR}/stall_stats.json"
	else
		info "the target did NOT answer RPC within 20s: the reactor itself is stuck"
	fi

	if command -v gdb >/dev/null 2>&1 && target_alive; then
		# This stops the target for as long as gdb is attached, which the host
		# will notice. Acceptable: the run has already failed, and a backtrace
		# is worth more than a clean teardown here.
		gdb -p "${TGT_PID}" -batch -ex "set pagination off" \
			-ex "thread apply all bt" >"${WORKDIR}/gdb_stall.txt" 2>&1 || true
		info "thread backtraces saved to ${WORKDIR}/gdb_stall.txt"
		grep -E '^Thread|^#[0-9]+ ' "${WORKDIR}/gdb_stall.txt" 2>/dev/null | \
			head -25 | sed 's/^/       /'
	fi
}

# Runs fio, and distinguishes "stalled" from "failed". Returns non-zero either
# way; the caller has already reported it.
run_fio()
{
	local label="$1" log="$2"
	shift 2
	local rc=0

	timeout --foreground "${FIO_TIMEOUT}" fio "$@" >/dev/null 2>&1 || rc=$?

	if [ "${rc}" -eq 0 ]; then
		pass "${label}"
		return 0
	fi

	if [ "${rc}" -eq 124 ]; then
		fail "${label}: no progress within ${FIO_TIMEOUT}s"
		diagnose_fio_stall "${log}"
	else
		fail "${label} (fio exited ${rc})"
		grep -iE 'error|verify|bad' "${log}" 2>/dev/null | \
			head -10 | sed 's/^/       /'
	fi
	return 1
}

if [ "${HAVE_FIO}" -eq 0 ]; then
	info "fio not installed, skipping"
else
	run_fio "fio randwrite 1M iodepth=8 with crc32c verify" \
		"${WORKDIR}/fio.log" \
		--name=s3lvol_verify ${FIO_COMMON} \
		--filename="${NVME_DEV}" \
		--ioengine=libaio --direct=1 \
		--rw=randwrite --bs=1M --iodepth=8 \
		--size=32M --offset=0 \
		--verify=crc32c --verify_fatal=1 --verify_backlog=1 \
		--output="${WORKDIR}/fio.log" || true

	check_target "step 8, fio 1M iodepth=8" || exit 1

	# Small blocks at high depth, to stress the chunk boundary logic.
	#
	# This is the expensive one by design: with a 1 MiB chunk and no local cache
	# (s3_cache.c is still a stub), a 4k write that misses the overlay is a
	# read-modify-write of a whole chunk. 16 MiB of 4k I/O touches 16 chunks, so
	# the overlay should absorb almost all of it -- if this run ever takes
	# minutes, that assumption is what to check first.
	run_fio "fio randrw 4k iodepth=16 with crc32c verify" \
		"${WORKDIR}/fio_small.log" \
		--name=s3lvol_small ${FIO_COMMON} \
		--filename="${NVME_DEV}" \
		--ioengine=libaio --direct=1 \
		--rw=randrw --bs=4k --iodepth=16 \
		--size=16M --offset=32M \
		--verify=crc32c --verify_fatal=1 \
		--output="${WORKDIR}/fio_small.log" || true

	check_target "step 8, fio 4k iodepth=16" || exit 1
fi

# ==========================================================================
# [8b] Growing the lvol
#
# Three things have to line up for a resize to be usable, and each of them is a
# separate failure: the blob grows, the bdev's blockcnt follows, and the NVMe host
# notices. The first two are in this process; the third is the kernel, and the
# whole point of exporting over nvmf is that the volume behaves like a disk.
#
# The new region is then written and read back, because a bdev that merely
# *reports* a larger size is worse than one that refuses to grow: I/O past the old
# end would fail somewhere in blobstore, or worse, land on a cluster nobody
# allocated.
# ==========================================================================
echo
echo "[8b] growing the lvol from ${LVOL_GIB} to $((2 * LVOL_GIB)) GiB"

SIZE_BEFORE="$(blockdev --getsize64 "${NVME_DEV}")"
NEW_GIB=$((2 * LVOL_GIB))
# The reply and the host device are both in bytes, so the expected value is too.
NEW_SIZE=$((NEW_GIB * 1024 * 1024 * 1024))

# Shrinking first, while the volume is still the original size: it must be
# refused. If it silently succeeded, the data above the new end would be
# unreachable and its S3 objects orphaned -- there is no GC to collect them.
#
# Half the volume, which is why LVOL_GIB is 2: size_gib takes whole GiB, so this
# needs a volume with a smaller whole GiB below it.
if raw_rpc rcow_resize_lvol \
		"$(printf '{"lvol_name":"%s","size_gib":%d}' \
			"${LVOL_NAME}" $((LVOL_GIB / 2)))" \
		>/dev/null 2>"${WORKDIR}/shrink.err"; then
	fail "shrinking the lvol was accepted; it should be refused"
else
	if grep -qiE 'not supported|ENOTSUP|-95' "${WORKDIR}/shrink.err"; then
		pass "shrinking was refused with -ENOTSUP"
	else
		fail "shrinking failed, but not with -ENOTSUP"
		sed 's/^/       /' "${WORKDIR}/shrink.err"
	fi
fi
check_target "step 8b, refusing to shrink" || exit 1

if ! raw_rpc rcow_resize_lvol \
		"$(printf '{"lvol_name":"%s","size_gib":%d}' \
			"${LVOL_NAME}" "${NEW_GIB}")" \
		>"${WORKDIR}/resize.json" 2>"${WORKDIR}/resize.err"; then
	fail "rcow_resize_lvol"
	sed 's/^/       /' "${WORKDIR}/resize.err"
	check_target "step 8b, resizing" || exit 1
else
	pass "resize RPC accepted"
	info "resize reported: $(cat "${WORKDIR}/resize.json")"
fi
check_target "step 8b, resizing" || exit 1

# The RPC no longer reports a byte count -- it answers the unified
# {"bool_value":true,"string_value":"<lvol name>"}, which s3lvol_rpc.py unwraps to
# the bare name. The host device is the single source of truth for the post-resize
# size, so compare directly.
# Give the kernel a moment to rescan the namespace through the AER.
sleep 1
SIZE_AFTER="$(blockdev --getsize64 "${NVME_DEV}" 2>/dev/null || echo 0)"
if [ "${SIZE_AFTER}" = "${NEW_SIZE}" ]; then
	pass "the host device reports ${NEW_SIZE} bytes"
fi

# The kernel learns about it through an AER from the target. Give that a moment,
# and fall back to an explicit rescan -- which of the two paths worked is worth
# knowing, so they are reported separately rather than silently retried.
HOST_SAW=0
for _ in $(seq 20); do
	SIZE_AFTER="$(blockdev --getsize64 "${NVME_DEV}")"
	[ "${SIZE_AFTER}" != "${SIZE_BEFORE}" ] && { HOST_SAW=1; break; }
	sleep 0.5
done
if [ "${HOST_SAW}" -eq 0 ]; then
	nvme ns-rescan "$(echo "${NVME_DEV}" | sed 's/n[0-9]*$//')" >/dev/null 2>&1 || true
	sleep 1
	SIZE_AFTER="$(blockdev --getsize64 "${NVME_DEV}")"
	[ "${SIZE_AFTER}" != "${SIZE_BEFORE}" ] && HOST_SAW=2
fi

case "${HOST_SAW}" in
1) pass "the NVMe host picked up the new size on its own (${SIZE_AFTER} bytes)" ;;
2) pass "the NVMe host picked up the new size after an explicit ns-rescan (${SIZE_AFTER} bytes)" ;;
*) fail "the host still reports ${SIZE_BEFORE} bytes after the resize"
   info "the bdev grew but the namespace did not; nvmf did not report the change" ;;
esac

if [ "${SIZE_AFTER}" = "${NEW_SIZE}" ]; then
	pass "host-visible size matches the new lvol size"
else
	info "host reports ${SIZE_AFTER}, expected ${NEW_SIZE} (a stale namespace geometry)"
fi

# Write into the region that only exists because of the resize.4 MiB at the old
# end: below it is pre-existing data, above it is the new tail.
if [ "${HOST_SAW}" -ne 0 ] && [ "${SIZE_AFTER}" = "${NEW_SIZE}" ]; then
	dd if=/dev/urandom of="${WORKDIR}/grown.bin" bs=1M count=4 status=none
	GROWN_HASH="$(md5sum "${WORKDIR}/grown.bin" | cut -d' ' -f1)"

	if dd if="${WORKDIR}/grown.bin" of="${NVME_DEV}" bs=1M count=4 \
			seek=$((LVOL_SIZE / 1024 / 1024)) oflag=direct conv=fsync \
			status=none 2>"${WORKDIR}/grown_w.err"; then
		pass "wrote 4 MiB into the newly added region"
	else
		fail "writing into the new region"
		sed 's/^/       /' "${WORKDIR}/grown_w.err"
	fi
	check_target "step 8b, writing into the new region" || exit 1

	if dd if="${NVME_DEV}" of="${WORKDIR}/grown_read.bin" bs=1M count=4 \
			skip=$((LVOL_SIZE / 1024 / 1024)) iflag=direct \
			status=none 2>/dev/null && \
	   [ "$(md5sum "${WORKDIR}/grown_read.bin" | cut -d' ' -f1)" = "${GROWN_HASH}" ]; then
		pass "read it back from the new region intact"
	else
		fail "the new region did not read back what was written"
	fi
	check_target "step 8b, reading the new region" || exit 1
else
	info "skipping the new-region I/O: the host does not see the new size"
fi

# ==========================================================================
# [9] Inspect the target log
#
# The point of this step: an I/O failure above would not tell us *which* layer
# refused. These two messages are specific.
# ==========================================================================
echo
echo "[9] checking the target log for layer-specific refusals"

if grep -q 'not supported yet' "${TGT_LOG}"; then
	fail "bs_dev refused a multi-segment I/O -- max_num_segments is not being honoured"
	grep 'not supported yet' "${TGT_LOG}" | head -5 | sed 's/^/       /'
else
	pass "no multi-segment refusal: max_num_segments = 1 is effective"
fi

if grep -qE 'Failed to (journal|update) chunk map' "${TGT_LOG}"; then
	fail "chunk map update failures in the log"
	grep -E 'Failed to (journal|update) chunk map' "${TGT_LOG}" | \
		head -5 | sed 's/^/       /'
else
	pass "no chunk map update failures"
fi

ASSERTS="$(grep -ciE 'assert|segfault|Segmentation' "${TGT_LOG}" || true)"
if [ "${ASSERTS}" -eq 0 ]; then
	pass "no asserts or crashes in the target log"
else
	fail "${ASSERTS} assert/crash lines in the target log"
fi

# ==========================================================================
# [10] Clean unload
#
# Not just tidiness. Unloading is the one path that has to get the shutdown
# ordering right: drain the flusher, let blobstore write its final metadata,
# flush *that* to S3, close the log, and only then release the journal and the
# local device. Getting it wrong shows up here as an assert or a hang, and
# nowhere else.
# ==========================================================================
echo
echo "[10] unloading the lvstore"

nvme_settle
nvme disconnect -n "${NQN}" >/dev/null 2>&1 && CONNECTED=0
${RPC} nvmf_delete_subsystem "${NQN}" >/dev/null 2>&1 || \
	info "nvmf_delete_subsystem failed (continuing)"

# Deleting the lvol before the lvstore, deliberately in that order: it is the
# only way to find out whether unload still works afterwards. A delete that left
# the lvstore holding a reference would wedge the unload below, and the two would
# be indistinguishable from a broken unload.
#
# The object count is sampled either side of it, because whether a delete reclaims
# S3 space was carried in HANDOFF as an unverified assumption for months. The chain
# is: deleting a blob truncates it, blob_persist_clear_clusters() clears the
# dropped clusters according to clear_method (we create lvols with
# LVOL_CLEAR_WITH_UNMAP), that arrives as unmap on our bs_dev, and
# s3_unmap_chunk_removed() drops the mapping and deletes the object.
#
# The first run of this assertion measured 351 objects before and 351 after, and I
# blamed blobstore for not clearing on delete. Wrong: every link above was working
# except the last one, where s3_delete() rejected the NULL callback that the
# fire-and-forget callers passed. The chain is fine; the leak was ours.
count_data_objects()
{
	python3 "${TOOLS_DIR}/s3_prefix_rm.py" -e "${ENDPOINT}" -b "${BUCKET}" \
		-r "${REGION}" -p "${LVS_NAME}/data/" --list 2>/dev/null | wc -l
}

OBJECTS_BEFORE_DELETE="$(count_data_objects)"
info "data objects before deleting the lvol: ${OBJECTS_BEFORE_DELETE}"
if ! raw_rpc rcow_delete_lvol \
		"$(printf '{"lvol_name":"%s"}' \
			"${LVOL_NAME}")" \
		>/dev/null 2>"${WORKDIR}/delete_lvol.err"; then
	fail "rcow_delete_lvol"
	sed 's/^/       /' "${WORKDIR}/delete_lvol.err"
else
	pass "lvol deleted"
fi
check_target "step 10, deleting the lvol" || exit 1

# Polled rather than sampled once: the deletes are best-effort and not waited on
# (s3_unmap_chunk_removed passes no callback), and a listing lags behind them
# anyway -- we watched a run report "362 deleted" while a listing seconds later
# still showed 177 objects. So give it time to drop, and report what it settles at.
OBJECTS_AFTER_DELETE="${OBJECTS_BEFORE_DELETE}"
for _ in $(seq 20); do
	OBJECTS_AFTER_DELETE="$(count_data_objects)"
	[ "${OBJECTS_AFTER_DELETE}" -lt "${OBJECTS_BEFORE_DELETE}" ] && break
	sleep 1
done

if [ "${OBJECTS_BEFORE_DELETE}" -eq 0 ]; then
	info "no data objects existed before the delete; nothing to reclaim"
elif [ "${OBJECTS_AFTER_DELETE}" -lt "${OBJECTS_BEFORE_DELETE}" ]; then
	pass "deleting the lvol reclaimed S3 objects (${OBJECTS_BEFORE_DELETE} -> ${OBJECTS_AFTER_DELETE})"
else
	fail "deleting the lvol reclaimed nothing (${OBJECTS_BEFORE_DELETE} objects still listed)"
	info "check the delete path: unmap reaching s3_bs_dev, the mapping being"
	info "dropped, and s3_delete() actually submitting -- it silently refused a"
	info "NULL callback once, which disabled every fire-and-forget delete"
fi

# The bdev has to go with it. A leftover bdev pointing at a destroyed lvol is a
# use-after-free waiting for the next I/O.
if ${RPC} bdev_get_bdevs 2>/dev/null | \
		grep -q "\"${LVS_NAME}/${LVOL_NAME}\""; then
	fail "bdev '${LVS_NAME}/${LVOL_NAME}' is still registered after the delete"
else
	pass "its bdev was unregistered too"
fi

# The successful delete has to be findable in the log. Nothing else prints it:
# the unregister is silent on success, the object deletes are fire-and-forget
# and log only their failures, and the RPC answer never reaches the log. Three
# lines cover the lifecycle: the entry logged the request, the module-level
# NOTICE confirms the blob is gone, the RPC completion confirms the caller got
# its reply (and the active record, if any, was dropped).
if grep -q "rcow_delete_lvol '${LVOL_NAME}' requested" "${TGT_LOG}" &&
   grep -q "Deleted lvol '${LVS_NAME}/${LVOL_NAME}'" "${TGT_LOG}" &&
   grep -q "rcow_delete_lvol '${LVOL_NAME}' completed" "${TGT_LOG}"; then
	pass "the delete left its requested / Deleted lvol / completed lines in the log"
else
	fail "the delete did not leave all three log lines for '${LVOL_NAME}'"
	info "expected: rcow_delete_lvol '${LVOL_NAME}' requested"
	info "          Deleted lvol '${LVS_NAME}/${LVOL_NAME}'"
	info "          rcow_delete_lvol '${LVOL_NAME}' completed"
fi

# And the lvol name is free again -- which also checks that the lookup helper
# reads the live list rather than a cached one.
if raw_rpc rcow_delete_lvol \
		"$(printf '{"lvol_name":"%s"}' \
			"${LVOL_NAME}")" \
		>/dev/null 2>"${WORKDIR}/delete_again.err"; then
	fail "deleting the same lvol twice succeeded"
else
	pass "deleting it again reports it does not exist"
fi
check_target "step 10, deleting a missing lvol" || exit 1

if ! raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS_NAME}")" \
		>/dev/null 2>"${WORKDIR}/unload.err"; then
	fail "rcow_unload_lvstore"
	sed 's/^/       /' "${WORKDIR}/unload.err"
else
	pass "lvstore unloaded"
	LVS_CREATED=0
fi

check_target "step 10, unloading the lvstore" || exit 1

# The WAL should have been truncated as the flusher consumed it. A log that is
# still full means truncation never advanced, which would eventually wedge the
# write path with -ENOSPC.
if grep -q 'WAL is full' "${TGT_LOG}"; then
	fail "the WAL filled up: truncation is not keeping up"
	grep 'WAL is full' "${TGT_LOG}" | head -3 | sed 's/^/       /'
else
	pass "the WAL never filled up"
fi

echo
echo "=== result: ${PASS} passed, ${FAIL} failed ==="
[ "${FAIL}" -eq 0 ] || exit 1
exit 0
