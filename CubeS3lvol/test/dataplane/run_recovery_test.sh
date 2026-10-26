#!/bin/bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Crash-recovery verification for the s3lvol vbdev: does rcow_attach_lvstore
# actually bring back what the WAL promised?
#
# === Why this is a separate script from run_dataplane_test.sh ===
#
# That one verifies a single target process end to end. This one needs *three*
# target lifetimes, because that is the only way to tell the two halves of
# durability apart:
#
#   round 1  create, write pattern A, unload cleanly, exit
#   round 2  attach, verify A survived, write pattern B, then SIGKILL
#   round 3  attach, verify *both* A and B survived
#
# Round 2's clean-attach check and round 3's crash-attach check fail for
# completely different reasons, and collapsing them would make a failure
# ambiguous:
#
#   - If round 2 fails, the metadata journal did not replay: the objects are in
#     the bucket but nothing maps an LBA to them. Nothing to do with the WAL.
#   - If round 3 fails, the WAL did not replay: writes that were acknowledged to
#     the host never reached S3 and were not recovered from the log. That is a
#     durability violation (INV1), the defect class the WAL exists to prevent.
#
# === What makes the SIGKILL meaningful ===
#
# Killing the target proves nothing unless there really was unflushed data at
# the time. So round 2 deliberately does *not* flush, and asserts
# overlay_bytes > 0 immediately before the kill. Without that assertion a
# too-eager flusher would turn this into a re-run of the clean case and it would
# still pass.
#
# SIGKILL specifically, not SIGTERM: SIGTERM gives SPDK a chance to shut down,
# which would drain the flusher and close the log. What we want is the state a
# power loss leaves behind -- an open log with a live tail.
#
# === What it cannot check ===
#
# A torn *batch* (the process dying midway through the local write) is not
# reproducible from the outside, since batches are written with a single bdev
# write. That property (W4: an unclosed batch is discarded whole) is covered by
# s3_wal_test instead.
#
# Usage:
#   export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
#   sudo -E ./test/dataplane/run_recovery_test.sh \
#            -e cos.ap-nanjing.myqcloud.com -b my-bucket -r ap-nanjing
#
# Needs root: nvme connect and /dev access.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TOOLS_DIR="${REPO_ROOT}/test/tools"
SPDK_ROOT="${SPDK_ROOT:-${REPO_ROOT}/deps/spdk}"

TGT_BIN="${REPO_ROOT}/app/s3lvol_tgt/s3lvol_tgt"
RPC_PY="${SPDK_ROOT}/scripts/rpc.py"
# Every SPDK RPC needs the socket named explicitly: rpc.py defaults to
# /var/tmp/spdk.sock, while the target now listens on /var/run/s3lvol.sock.
RPC="${RPC_PY} -s /var/run/s3lvol.sock"
RPC_SOCK="/var/run/s3lvol.sock"

ENDPOINT=""
BUCKET=""
REGION="ap-nanjing"
LVS_NAME="rcvs"
LVOL_NAME="vol0"
NQN="nqn.2026-08.io.spdk:s3rc"
LISTEN_ADDR="127.0.0.1"
LISTEN_PORT="4420"

CAPACITY_GIB=8
# GiB for the RPC (size_gib), bytes for the device-size assertion in round 2.
LVOL_GIB=1
LVOL_SIZE=$((LVOL_GIB * 1024 * 1024 * 1024))

# Two disjoint regions, so a failure says which round lost data rather than just
# "the device differs". A is written before the clean unload, B after the attach
# and before the kill.
#
# B is much larger than A on purpose. It is the region that has to be recovered
# from the log, so the test is only meaningful if a decent backlog is still
# unflushed at the moment of the kill -- and the flusher runs 8 concurrent
# uploads, so a few MiB disappear into S3 faster than the script can get from the
# dd to the kill. 32 MiB is enough that it cannot win the race.
A_OFF_MB=0
A_LEN_MB=8
B_OFF_MB=16
B_LEN_MB=32

# Interval the round-3 attach asks for, and how long to wait for that trigger to
# fire. The wait is generous relative to the interval because the poller checks
# once a second and the snapshot itself is an S3 round trip.
# Interval, in seconds, for round 3's attach. Low so the *time-based* checkpoint
# trigger is reachable at all: the default is 60 s and the usage-based trigger
# needs ~2 million chunk uploads, so without this only the manual RPC would ever
# be covered.
#
# 1 rather than 5 because step [3d] needs the interval to come due *inside* the
# teardown window. The poll period is 1 s, so at 5 s that was a one-in-five
# coincidence -- which is exactly how the use-after-free it checks for reached a
# release: the suite hit it once in a while and passed the rest of the time.
CKPT_INTERVAL_SEC="${S3LVOL_CKPT_INTERVAL_SEC:-1}"
CKPT_WAIT_SEC=30

# The local device *must survive between rounds* -- it is the whole point. So it
# is created once and never re-truncated, and the aio bdev is re-created against
# the same file in every round.
WAL_FILE="${S3LVOL_WAL_FILE:-/data/s3lvol_rc_wal.img}"
WAL_BDEV="rc_wal0"
JOURNAL_MB="${S3LVOL_JOURNAL_MB:-64}"
WAL_MB="${S3LVOL_WAL_MB:-256}"
WAL_FILE_MB=$((JOURNAL_MB + WAL_MB + 128))

TGT_CPUMASK="${S3LVOL_TGT_CPUMASK:-0x3}"

if [ -n "${S3LVOL_TGT_ARGS:-}" ]; then
	TGT_ARGS="${S3LVOL_TGT_ARGS}"
elif [ "$(cat /proc/sys/vm/nr_hugepages 2>/dev/null || echo 0)" -gt 0 ]; then
	TGT_ARGS=""
else
	TGT_ARGS="--no-huge -s 1024 -r /var/run/s3lvol.sock"
fi

WORKDIR="$(mktemp -d /tmp/s3lvol_rc.XXXXXX)"
TGT_PID=""
TGT_LOG=""
ROUND=0
NVME_DEV=""
CONNECTED=0
# nvmf transport lives in the target process, so this resets with every round.
TRANSPORT_READY=0
LVS_EXISTS=0
# Sticky version of the above: LVS_EXISTS goes back to 0 on a clean unload, but
# the objects in S3 outlive it and still have to be cleaned up.
LVS_WAS_CREATED=0
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
  S3LVOL_CKPT_INTERVAL_SEC  interval round 3 attaches with (default: ${CKPT_INTERVAL_SEC})
  S3LVOL_KEEP_S3            keep the S3 objects even after a clean run
  S3LVOL_KEEP_LOGS          keep the target logs even after a clean run

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
# The s3lvol methods are defined in this repo, not in SPDK, so spdk/scripts/rpc.py
# does not know them. The client lives in test/tools so both test scripts and
# manual debugging use the same one -- when it was inlined in each script, the two
# copies had already drifted to different socket timeouts.
# ==========================================================================
raw_rpc()
{
	python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" "$1" "${2:-}"
}

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

	fail "the target died during: ${step}"
	local st=0
	wait "${TGT_PID}" 2>/dev/null || st=$?
	case "${st}" in
	139) info "died on SIGSEGV (signal 11)" ;;
	134) info "died on SIGABRT (signal 6), most likely an assert" ;;
	*)   info "exit status ${st}" ;;
	esac
	info "log: ${TGT_LOG}"
	tail -30 "${TGT_LOG}" 2>/dev/null | sed 's/^/       /'
	TGT_PID=""
	return 1
}

# ==========================================================================
# Target lifecycle
#
# Each round gets its own log file. Keeping them apart matters more here than in
# the single-process test: "which round produced this error" is the first
# question asked of any failure.
# ==========================================================================
start_target()
{
	ROUND=$((ROUND + 1))
	TGT_LOG="${WORKDIR}/tgt_round${ROUND}.log"
	# New process, so nothing it held is still configured.
	TRANSPORT_READY=0

	# shellcheck disable=SC2086
	"${TGT_BIN}" -m "${TGT_CPUMASK}" ${TGT_ARGS} >"${TGT_LOG}" 2>&1 &
	TGT_PID=$!

	local i
	for i in $(seq 150); do
		${RPC} spdk_get_version >/dev/null 2>&1 && break
		kill -0 "${TGT_PID}" 2>/dev/null || break
		sleep 0.2
	done

	if ! ${RPC} spdk_get_version >/dev/null 2>&1; then
		fail "round ${ROUND}: target did not come up"
		tail -20 "${TGT_LOG}" 2>/dev/null | sed 's/^/       /'
		return 1
	fi
	info "round ${ROUND}: target up (pid ${TGT_PID}), log ${TGT_LOG}"
	return 0
}

# Re-attach the local device image. The file persists across rounds; only the
# bdev has to be recreated, since it lives in the process that just exited.
attach_local_dev()
{
	if ! ${RPC} bdev_aio_create "${WAL_FILE}" "${WAL_BDEV}" 4096 \
			>/dev/null 2>"${WORKDIR}/aio_r${ROUND}.err"; then
		fail "round ${ROUND}: bdev_aio_create"
		sed 's/^/       /' "${WORKDIR}/aio_r${ROUND}.err"
		return 1
	fi
	return 0
}

export_and_connect()
{
	# Only once per target lifetime. Calling it again is harmless but logs
	# "Transport type 'TCP' already exists" as an *ERROR*, which is noise that
	# looks like a real failure when reading a recovery log.
	if [ "${TRANSPORT_READY}" -eq 0 ]; then
		${RPC} nvmf_create_transport -t TCP >/dev/null 2>&1 || true
		TRANSPORT_READY=1
	fi
	${RPC} nvmf_create_subsystem "${NQN}" -a -s S3RC00000000000001 \
		>/dev/null 2>&1 || { fail "nvmf_create_subsystem"; return 1; }
	${RPC} nvmf_subsystem_add_ns "${NQN}" "${LVS_NAME}/${LVOL_NAME}" \
		>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns"; return 1; }
	${RPC} nvmf_subsystem_add_listener "${NQN}" \
		-t tcp -a "${LISTEN_ADDR}" -s "${LISTEN_PORT}" \
		>/dev/null 2>&1 || { fail "nvmf_subsystem_add_listener"; return 1; }

	local before after i
	before="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
	if ! nvme connect -t tcp -a "${LISTEN_ADDR}" -s "${LISTEN_PORT}" \
			-n "${NQN}" >"${WORKDIR}/connect_r${ROUND}.log" 2>&1; then
		fail "round ${ROUND}: nvme connect"
		sed 's/^/       /' "${WORKDIR}/connect_r${ROUND}.log"
		return 1
	fi
	CONNECTED=1

	for i in $(seq 50); do
		after="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
		NVME_DEV="$(comm -13 <(echo "${before}") <(echo "${after}") | head -1)"
		[ -n "${NVME_DEV}" ] && [ -b "${NVME_DEV}" ] && break
		sleep 0.2
	done
	if [ -z "${NVME_DEV}" ] || [ ! -b "${NVME_DEV}" ]; then
		fail "round ${ROUND}: no nvme block device appeared"
		return 1
	fi
	info "round ${ROUND}: block device ${NVME_DEV}"

	# The kernel probes the namespace on attach, so a bug can kill the target
	# here before any dd runs.
	sleep 2
	check_target "round ${ROUND}, right after nvme connect" || return 1
	return 0
}

disconnect_nvme()
{
	if [ "${CONNECTED}" -eq 1 ]; then
		# Let udev's re-probe finish first, or the disconnect leaves
		# "Buffer I/O error ... async page read" in dmesg: harmless
		# teardown noise that reads like a real failure.
		udevadm settle --timeout=5 >/dev/null 2>&1 || true
		nvme disconnect -n "${NQN}" >/dev/null 2>&1
		CONNECTED=0
		sleep 1
	fi
}

# Remove the lvstore's objects once the target is down.
#
# Nothing in s3lvol deletes them -- unload keeps them because the next attach
# needs them -- so every run used to leave a prefix behind, and the owner marker a
# crashed round leaves makes the *next* create fail with -EBUSY. Both were manual
# steps between runs; now only a failing run keeps its objects, because that is
# when they are evidence.
clean_s3_prefix()
{
	local keep="$1"

	if [ -n "${S3LVOL_KEEP_S3:-}" ] || [ "${keep}" -eq 1 ]; then
		info "S3 objects kept under prefix '${LVS_NAME}/' in bucket ${BUCKET}"
		info "inspect: ${TOOLS_DIR}/s3_prefix_rm.py -e ${ENDPOINT} -b ${BUCKET} -r ${REGION} -p ${LVS_NAME}/ --list"
		info "remove:  ${TOOLS_DIR}/s3_prefix_rm.py -e ${ENDPOINT} -b ${BUCKET} -r ${REGION} -p ${LVS_NAME}/"
		return
	fi

	if python3 "${TOOLS_DIR}/s3_prefix_rm.py" -e "${ENDPOINT}" -b "${BUCKET}" \
			-r "${REGION}" -p "${LVS_NAME}/" \
			>"${WORKDIR}/s3_rm.log" 2>&1; then
		info "S3: $(cat "${WORKDIR}/s3_rm.log")"
	else
		info "S3 cleanup failed, objects left under '${LVS_NAME}/':"
		sed 's/^/       /' "${WORKDIR}/s3_rm.log"
	fi
}

# ==========================================================================
# Teardown
# ==========================================================================
cleanup()
{
	local rc=$?

	echo
	echo "=== cleanup ==="

	disconnect_nvme

	if target_alive; then
		if [ "${LVS_EXISTS}" -eq 1 ]; then
			# delete on a clean run so the bstore.json entry goes with the
			# objects; keep both after a failure.
			CLEANUP_RPC=rcow_unload_lvstore
			if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
				CLEANUP_RPC=rcow_delete_lvstore
			fi
			raw_rpc "${CLEANUP_RPC}" \
				"$(printf '{"lvs_name":"%s"}' "${LVS_NAME}")" \
				>/dev/null 2>&1 && info "lvstore ${CLEANUP_RPC#rcow_}ed"
		fi
		kill -TERM "${TGT_PID}" 2>/dev/null
		for _ in $(seq 50); do
			kill -0 "${TGT_PID}" 2>/dev/null || break
			sleep 0.1
		done
		kill -KILL "${TGT_PID}" 2>/dev/null
	fi

	# After the target is gone, so nothing is still uploading into the prefix
	# being deleted.
	if [ "${LVS_EXISTS}" -eq 1 ] || [ "${LVS_WAS_CREATED}" -eq 1 ]; then
		if [ "${FAIL}" -eq 0 ] && [ "${rc}" -eq 0 ]; then
			clean_s3_prefix 0
		else
			clean_s3_prefix 1
		fi
	fi

	if [ "${WAL_FILE_CREATED}" -eq 1 ]; then
		if [ "${FAIL}" -eq 0 ] && [ "${rc}" -eq 0 ]; then
			rm -f "${WAL_FILE}"
		else
			info "local device image kept at ${WAL_FILE}"
			info "it holds the WAL and the journal -- the evidence for"
			info "any recovery failure is in there"
		fi
	fi

	if [ "${FAIL}" -gt 0 ] || [ "${rc}" -ne 0 ] || \
	   [ -n "${S3LVOL_KEEP_LOGS:-}" ]; then
		echo "  ---- logs kept in ${WORKDIR}"
		ls -1 "${WORKDIR}"/tgt_round*.log 2>/dev/null | sed 's/^/       /'
	else
		rm -rf "${WORKDIR}"
	fi
}
trap cleanup EXIT

# ==========================================================================
# Preflight
# ==========================================================================
echo "=== s3lvol crash-recovery verification (attach path) ==="
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

modprobe nvme_tcp 2>/dev/null || true
lsmod | grep -q nvme_tcp || {
	echo "nvme_tcp module not loaded; cannot use the loopback" >&2
	exit 1
}

ulimit -c unlimited 2>/dev/null || true

# Reference patterns. Random rather than repeating: a pattern would still compare
# equal if the data came back from the wrong offset.
dd if=/dev/urandom of="${WORKDIR}/pat_a.bin" bs=1M count=${A_LEN_MB} status=none
dd if=/dev/urandom of="${WORKDIR}/pat_b.bin" bs=1M count=${B_LEN_MB} status=none
A_HASH="$(sha256sum "${WORKDIR}/pat_a.bin" | cut -d' ' -f1)"
B_HASH="$(sha256sum "${WORKDIR}/pat_b.bin" | cut -d' ' -f1)"

verify_region()
{
	local label="$1" off_mb="$2" len_mb="$3" want="$4"
	local out="${WORKDIR}/read_${label}_r${ROUND}.bin"

	if ! dd if="${NVME_DEV}" of="${out}" bs=1M count="${len_mb}" \
			skip="${off_mb}" iflag=direct status=none \
			2>"${WORKDIR}/dd_${label}_r${ROUND}.err"; then
		fail "round ${ROUND}: reading region ${label}"
		sed 's/^/       /' "${WORKDIR}/dd_${label}_r${ROUND}.err"
		return 1
	fi

	local got
	got="$(sha256sum "${out}" | cut -d' ' -f1)"
	if [ "${want}" = "${got}" ]; then
		pass "round ${ROUND}: region ${label} intact (${len_mb} MiB @ ${off_mb} MiB)"
		return 0
	fi

	fail "round ${ROUND}: region ${label} differs (want ${want:0:16}..., got ${got:0:16}...)"
	cmp "${WORKDIR}/pat_${label}.bin" "${out}" 2>&1 | head -3 | sed 's/^/       /'
	return 1
}

# ==========================================================================
# Round 1: create, write A, unload cleanly
#
# The clean unload is what makes round 2 a test of the *journal* alone: the
# flusher is drained and the log is closed, so nothing is left for the WAL replay
# to do and a failure can only be a chunk map that did not come back.
# ==========================================================================
echo "[round 1] create, write pattern A, unload cleanly"

rm -f "${WAL_FILE}"
if ! truncate -s "${WAL_FILE_MB}M" "${WAL_FILE}"; then
	fail "could not create ${WAL_FILE}"
	exit 1
fi
WAL_FILE_CREATED=1

start_target || exit 1
attach_local_dev || exit 1

# A deliberately long checkpoint interval for rounds 1 and 2, so the only
# checkpoint they take is the explicit one below. Left at the 60 s default, a slow
# S3 endpoint could push these rounds past it and produce a second, automatic
# checkpoint -- which is correct behaviour but would make the counts asserted here
# depend on wall-clock timing. Round 3 is where the interval is exercised.
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

if ! raw_rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"%s","journal_size_mb":%d,"wal_size_mb":%d,"checkpoint_interval_sec":3600}' \
		"${LVS_NAME}" "${BUCKET}" "${CAPACITY_GIB}" \
		"${WAL_BDEV}" "${JOURNAL_MB}" "${WAL_MB}")" \
		>"${WORKDIR}/lvs.json" 2>"${WORKDIR}/lvs.err"; then
	fail "rcow_create_lvstore"
	sed 's/^/       /' "${WORKDIR}/lvs.err"
	exit 1
fi
LVS_EXISTS=1
LVS_WAS_CREATED=1
pass "round 1: lvstore created"

if ! raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
		"${LVOL_NAME}" "${LVOL_GIB}")" \
		>"${WORKDIR}/lvol.json" 2>"${WORKDIR}/lvol.err"; then
	fail "rcow_create_lvol"
	sed 's/^/       /' "${WORKDIR}/lvol.err"
	exit 1
fi
pass "round 1: lvol created"

export_and_connect || exit 1

if ! dd if="${WORKDIR}/pat_a.bin" of="${NVME_DEV}" bs=1M count=${A_LEN_MB} \
		seek=${A_OFF_MB} oflag=direct conv=fsync status=none \
		2>"${WORKDIR}/dd_wa.err"; then
	fail "round 1: writing pattern A"
	sed 's/^/       /' "${WORKDIR}/dd_wa.err"
	check_target "round 1, writing pattern A" || exit 1
	exit 1
fi
pass "round 1: wrote pattern A (${A_LEN_MB} MiB @ ${A_OFF_MB} MiB)"
check_target "round 1, writing pattern A" || exit 1

disconnect_nvme
${RPC} nvmf_delete_subsystem "${NQN}" >/dev/null 2>&1 || true

# Take a checkpoint before unloading, so round 2's attach has to restore the chunk
# map from the snapshot rather than from the journal.
#
# Explicit because neither automatic trigger can fire in this round: the usage one
# needs 50% of the journal region, which is ~2 million chunk uploads for the
# default 256 MiB, and the interval one was set to an hour when the lvstore was
# created. That is on purpose -- this round wants a checkpoint at a *known* point,
# not whenever a timer happens to expire. The interval trigger has its own section
# in round 3.
if ! raw_rpc rcow_checkpoint_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS_NAME}")" \
		>/dev/null 2>"${WORKDIR}/ckpt1.err"; then
	fail "round 1: rcow_checkpoint_lvstore"
	sed 's/^/       /' "${WORKDIR}/ckpt1.err"
else
	pass "round 1: checkpoint taken"
fi
check_target "round 1, taking a checkpoint" || exit 1

CKPT_DONE="$(wal_stat ckpt_done)"
CKPT_LSN="$(wal_stat ckpt_lsn)"
if [ "${CKPT_DONE}" = "1" ]; then
	pass "round 1: one checkpoint completed, covering journal LSN ${CKPT_LSN}"
else
	fail "round 1: ckpt_done is ${CKPT_DONE}, expected 1"
fi
# A checkpoint that covers LSN 0 covers nothing, which would make round 2's
# restore vacuous while still looking like it worked.
if [ "${CKPT_LSN}" != "missing" ] && [ "${CKPT_LSN}" -gt 0 ] 2>/dev/null; then
	pass "round 1: the checkpoint covers a non-zero LSN, so it has mappings in it"
else
	fail "round 1: the checkpoint covers LSN ${CKPT_LSN}; nothing was snapshotted"
fi

if ! raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS_NAME}")" \
		>/dev/null 2>"${WORKDIR}/unload1.err"; then
	fail "round 1: rcow_unload_lvstore"
	sed 's/^/       /' "${WORKDIR}/unload1.err"
	exit 1
fi
pass "round 1: lvstore unloaded cleanly"
check_target "round 1, unloading" || exit 1

kill -TERM "${TGT_PID}" 2>/dev/null
for _ in $(seq 100); do
	kill -0 "${TGT_PID}" 2>/dev/null || break
	sleep 0.1
done
if kill -0 "${TGT_PID}" 2>/dev/null; then
	fail "round 1: target ignored SIGTERM (stuck in shutdown?)"
	kill -KILL "${TGT_PID}" 2>/dev/null
else
	pass "round 1: target exited on SIGTERM"
fi
TGT_PID=""

# ==========================================================================
# Round 2: attach, verify A, write B, then kill without flushing
# ==========================================================================
echo
echo "[round 2] attach, verify A, write pattern B, then SIGKILL"

start_target || exit 1
attach_local_dev || exit 1

if ! raw_rpc rcow_add_s3_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"%s}' \
		"${BUCKET}" "${ENDPOINT}" "${BUCKET}" "${REGION}" "${S3LVOL_EXTRA_JSON}")" \
		>/dev/null 2>"${WORKDIR}/cos_config_r2.err"; then
	fail "rcow_add_s3_config (round 2)"
	exit 1
fi

# Again an hour-long interval: an automatic checkpoint here would truncate the
# journal, and round 3 asserts on how much there was to replay.
if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s","checkpoint_interval_sec":3600}' \
		"${LVS_NAME}" "${BUCKET}" "${WAL_BDEV}")" \
		>"${WORKDIR}/attach1.json" 2>"${WORKDIR}/attach1.err"; then
	fail "round 2: rcow_attach_lvstore"
	sed 's/^/       /' "${WORKDIR}/attach1.err"
	check_target "round 2, attaching" || exit 1
	exit 1
fi
# No force here, deliberately: round 1 unloaded cleanly, which deletes the owner
# marker. If that release were broken, this attach would be refused -- so this
# line doubles as the check that a clean unload gives the claim back.
pass "round 2: lvstore attached without force (round 1's unload released the owner marker)"

# The chunk map for pattern A now lives *only* in the snapshot -- round 1's
# checkpoint truncated the journal past those records. So this is the check that
# the checkpoint is actually being read back; without it, a broken restore would
# show up much later as pattern A reading as zeroes.
if grep -q 'Loaded checkpoint gen=' "${TGT_LOG}"; then
	pass "round 2: $(grep -o 'Loaded checkpoint gen=[0-9]*.*covers LSN [0-9]*' "${TGT_LOG}" | tail -1)"
else
	fail "round 2: the attach did not load a checkpoint"
	info "round 1 took one, so the chunk map should have been restored from it"
	grep -iE 'checkpoint' "${TGT_LOG}" | tail -5 | sed 's/^/       /'
fi
check_target "round 2, attaching" || exit 1

# The attach has to bring the lvol back by itself. Nothing recreates it, so if
# the bdev is missing the load did not walk the blobstore's lvol list.
if grep -q "\"bdev_name\": \"${LVS_NAME}/${LVOL_NAME}\"" "${WORKDIR}/attach1.json"; then
	pass "round 2: lvol '${LVOL_NAME}' came back and was re-registered as a bdev"
else
	fail "round 2: attach did not report a bdev for '${LVOL_NAME}'"
	sed 's/^/       /' "${WORKDIR}/attach1.json"
	exit 1
fi

export_and_connect || exit 1

DEV_SIZE="$(blockdev --getsize64 "${NVME_DEV}")"
if [ "${DEV_SIZE}" -eq "${LVOL_SIZE}" ]; then
	pass "round 2: device size matches the lvol (${DEV_SIZE} bytes)"
else
	fail "round 2: device size ${DEV_SIZE} != ${LVOL_SIZE}"
fi

# The core check of the clean case: the chunk map came back from the journal.
verify_region a "${A_OFF_MB}" "${A_LEN_MB}" "${A_HASH}" || true
check_target "round 2, verifying pattern A" || exit 1

if ! dd if="${WORKDIR}/pat_b.bin" of="${NVME_DEV}" bs=1M count=${B_LEN_MB} \
		seek=${B_OFF_MB} oflag=direct conv=fsync status=none \
		2>"${WORKDIR}/dd_wb.err"; then
	fail "round 2: writing pattern B"
	sed 's/^/       /' "${WORKDIR}/dd_wb.err"
	check_target "round 2, writing pattern B" || exit 1
	exit 1
fi
pass "round 2: wrote pattern B (${B_LEN_MB} MiB @ ${B_OFF_MB} MiB), acknowledged from the log"
check_target "round 2, writing pattern B" || exit 1

# Kill the target *immediately* after sampling what is still pending.
#
# The first live run passed while barely testing anything: it sampled
# overlay_bytes right after the dd, then made another stats RPC, then
# disconnected nvme (which sleeps a second), and only then killed. In those~6
# seconds the flusher uploaded almost everything, so round 3 replayed 3 entries
# instead of the backlog -- a green run that proved nearly nothing.
#
# So: one stats call, then kill, and nothing in between. nvme is disconnected
# *after* the kill, which is also the more faithful order -- a host does not
# politely detach before the power goes out.
STATS="$(raw_rpc rcow_get_lvstores "" 2>/dev/null)"
kill -KILL "${TGT_PID}" 2>/dev/null
KILLED_PID="${TGT_PID}"
wait "${TGT_PID}" 2>/dev/null || true
TGT_PID=""
info "round 2: SIGKILLed pid ${KILLED_PID} with no drain and no log close"

disconnect_nvme

OVERLAY_BYTES="$(printf '%s' "${STATS}" | python3 -c '
import json, sys
try:
    stores = json.load(sys.stdin)
except ValueError:
    print("missing"); sys.exit(0)
for st in stores:
    wp = st.get("write_path", {})
    if "overlay_bytes" in wp:
        print(wp["overlay_bytes"]); break
else:
    print("missing")
' 2>/dev/null)"

# *These assertions are what give the kill its meaning.* A bare "> 0" is not
# enough: it passes when the flusher has drained 99% of the backlog, which is
# exactly the failure mode described above. Require a real backlog instead, so
# that "the flusher won the race" is reported as a broken test rather than
# quietly accepted as a pass.
#
# The floor is two chunks, not a fraction of what was written. It used to be a
# quarter of pattern B, which stopped holding the moment the flusher got better
# at its job: it now takes fully dirty chunks first, and those upload without
# reading the old object back, so it moves roughly twice the data per round trip
# and legitimately keeps far closer to the writer. Measured after that change,
# 32 MiB written leaves 6-7 MiB pending rather than the 8+ the ratio demanded --
# so the old floor had turned into a requirement that the flusher be inefficient.
#
# Two chunks is what the assertion is actually there to catch: the original bug
# was a run that drained everything and replayed three entries. It is far enough
# below what is observed to not be a coin flip, and far enough above zero that a
# drained backlog still fails. The direct measure of recovery work is the replay
# count in round 3, which is asserted separately and is not a proxy for anything.
# Two chunks is the floor on a remote store. A local one (MinIO in CI) keeps
# far closer to the writer, so the floor is tunable.
MIN_PENDING="${S3LVOL_TEST_MIN_PENDING:-$((2 * 1024 * 1024))}"
if [ "${S3LVOL_TEST_SKIP_RECOVERY_QUANTITY:-0}" -eq 1 ]; then
	# A fast local backend (MinIO in CI) keeps up with the writer, so a
	# multi-MiB backlog cannot be built reliably. The round 3 data checks
	# still verify recovery; only the "was there enough to recover" floors
	# are skipped.
	pass "round 2: pending-size floor skipped (fast local backend)"
elif [ "${OVERLAY_BYTES}" = "missing" ]; then
	fail "round 2: could not read overlay_bytes before the kill"
elif [ "${OVERLAY_BYTES}" -ge "${MIN_PENDING}" ] 2>/dev/null; then
	pass "round 2: $((OVERLAY_BYTES / 1048576)) MiB acknowledged but not yet in S3 (>= $((MIN_PENDING / 1048576)) MiB needed to make this a real crash)"
else
	fail "round 2: only ${OVERLAY_BYTES} bytes were pending, need >= ${MIN_PENDING}"
	info "the flusher kept up with the writer, so there is almost nothing for"
	info "round 3 to recover and a green result there would be meaningless"
fi
pass "round 2: target killed with data still in the log"

# ==========================================================================
# Round 3: attach again and check that *both* patterns are there
#
# A must survive because it was in S3 already; B can only come back from the WAL.
#
# This round also checks the owner marker, and the three rounds happen to cover
# its whole contract:
#
#   - round 2 attached with no force at all and succeeded, which is only possible
#     because round 1's clean unload deleted the marker.
#   - round 3 was preceded by SIGKILL, so the marker is still there. An attach
#     without force has to be *refused*, and one with force has to work.
#
# The refusal is the more important of the two: it is the check that stops two
# processes from running a flusher each against the same key prefix, which
# corrupts data silently rather than failing.
# ==========================================================================
echo
echo "[round 3] attach after the crash and verify both patterns"

start_target || exit 1
attach_local_dev || exit 1

if ! raw_rpc rcow_add_s3_config "$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"%s}' \
		"${BUCKET}" "${ENDPOINT}" "${BUCKET}" "${REGION}" "${S3LVOL_EXTRA_JSON}")" \
		>/dev/null 2>"${WORKDIR}/cos_config_r3.err"; then
	fail "rcow_add_s3_config (round 3)"
	exit 1
fi

# The crash left the marker behind on purpose -- it does not self-expire, because
# guessing whether the other owner is alive from a timestamp is worse than making
# a human confirm it.
if raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${LVS_NAME}" "${BUCKET}" "${WAL_BDEV}")" \
		>"${WORKDIR}/attach_noforce.json" 2>"${WORKDIR}/attach_noforce.err"; then
	fail "round 3: attach succeeded without force, so the owner marker left by the crash was ignored"
	info "two processes could then run a flusher each against the same objects"
else
	if grep -qi 'device or resource busy\|EBUSY\|-16' "${WORKDIR}/attach_noforce.err"; then
		pass "round 3: attach without force was refused, the crashed owner's marker is still honoured"
	else
		fail "round 3: attach failed, but not with -EBUSY as the owner check should"
		sed 's/^/       /' "${WORKDIR}/attach_noforce.err"
	fi
fi
check_target "round 3, attach without force" || exit 1

# checkpoint_interval_sec is set low here so the *time-based* trigger can be
# observed. The default is 60 s, and the usage-based one needs ~2 million chunk
# uploads, so without this neither automatic path is reachable in a test and
# only the manual RPC would ever be covered. Round 3 is the right place for it:
# nothing after this point depends on the journal still holding its records.
if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s","force":true,"checkpoint_interval_sec":%d}' \
		"${LVS_NAME}" "${BUCKET}" "${WAL_BDEV}" "${CKPT_INTERVAL_SEC}")" \
		>"${WORKDIR}/attach2.json" 2>"${WORKDIR}/attach2.err"; then
	fail "round 3: rcow_attach_lvstore --force after a crash"
	sed 's/^/       /' "${WORKDIR}/attach2.err"
	check_target "round 3, attaching after the crash" || exit 1
	exit 1
fi
pass "round 3: lvstore attached with force after an unclean shutdown"
check_target "round 3, attaching after the crash" || exit 1

REPORTED_INTERVAL="$(wal_stat ckpt_interval_sec)"
if [ "${REPORTED_INTERVAL}" = "${CKPT_INTERVAL_SEC}" ]; then
	pass "round 3: the checkpoint interval was taken from the RPC (${REPORTED_INTERVAL}s)"
else
	fail "round 3: asked for a ${CKPT_INTERVAL_SEC}s checkpoint interval, target reports ${REPORTED_INTERVAL}"
	info "the rest of the interval checks below will be meaningless"
fi

# Replaying nothing would be a silent failure: the attach would succeed and the
# data would simply be missing. And replaying just a handful of entries means the
# flusher had drained the backlog before the kill, so the round is not actually
# exercising recovery -- which is why there is a floor here and not just "> 0".
# A 1 MiB write becomes about two WAL entries (a full chunk does not fit in one
# batch buffer), so the >= 8 MiB backlog asserted in round 2 has to show up as
# well over 8 entries.
REPLAYED="$(grep -o 'replayed [0-9]* WAL entries' "${TGT_LOG}" | \
	grep -o '[0-9]*' | tail -1 || true)"
if [ "${S3LVOL_TEST_SKIP_RECOVERY_QUANTITY:-0}" -eq 1 ]; then
	pass "round 3: replay-count floor skipped (fast local backend); replayed ${REPLAYED:-0} entries"
elif [ -z "${REPLAYED}" ]; then
	fail "round 3: the log does not say anything was replayed"
	grep -iE 'replay|wal' "${TGT_LOG}" | tail -10 | sed 's/^/       /'
elif [ "${REPLAYED}" -ge 8 ] 2>/dev/null; then
	pass "round 3: replayed ${REPLAYED} WAL entries from the crashed log"
else
	fail "round 3: only ${REPLAYED} WAL entries were replayed"
	info "too little was left in the log for this to be a real recovery test"
fi

export_and_connect || exit 1

# A: was already in S3 before the crash. Failing here means the journal replay
# broke, not the WAL.
verify_region a "${A_OFF_MB}" "${A_LEN_MB}" "${A_HASH}" || true
# B: acknowledged to the host but never uploaded. This can only come back from
# the WAL, so it is the durability check (INV1).
verify_region b "${B_OFF_MB}" "${B_LEN_MB}" "${B_HASH}" || true
check_target "round 3, verifying both patterns" || exit 1

# Now push the recovered data out and read it back through a fresh connection:
# without this, B could still be coming from the overlay that the replay filled,
# which would not prove it ever reached S3.
echo
echo "[round 3b] flush the recovered data into S3 and re-read it"

if ! raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS_NAME}")" \
		>/dev/null 2>"${WORKDIR}/flush3.err"; then
	fail "round 3: rcow_flush_lvstore"
	sed 's/^/       /' "${WORKDIR}/flush3.err"
else
	pass "round 3: flush completed"
fi
check_target "round 3, flushing" || exit 1

LEFT="$(wal_stat overlay_bytes)"
if [ "${LEFT}" = "0" ]; then
	pass "round 3: the overlay is empty, so reads now come from S3"
else
	fail "round 3: the overlay still holds ${LEFT} bytes after a flush"
fi

disconnect_nvme
${RPC} nvmf_delete_subsystem "${NQN}" >/dev/null 2>&1 || true
export_and_connect || exit 1

verify_region a "${A_OFF_MB}" "${A_LEN_MB}" "${A_HASH}" || true
verify_region b "${B_OFF_MB}" "${B_LEN_MB}" "${B_HASH}" || true
check_target "round 3b, re-reading from S3" || exit 1

# ==========================================================================
# [3c] The time-based checkpoint trigger
#
# Everything above reaches the checkpoint code through the manual RPC, which says
# nothing about whether a checkpoint ever happens on its own. That matters more
# than the manual path: the usage trigger only fires when the journal region is
# half full, so on a lightly loaded lvstore it can be days between checkpoints and
# a crash then has to replay all of it. The interval is what bounds that.
#
# No writes are needed to provoke one. The replay this round performed advanced
# the chunk map's applied_lsn past the LSN round 1's checkpoint covered, so there
# is genuinely something new to snapshot -- and if there were not, the trigger is
# supposed to do nothing at all, which is what the "nothing changed" branch in
# ckpt_start() is for.
# ==========================================================================
echo
echo "[3c] waiting up to ${CKPT_WAIT_SEC}s for the ${CKPT_INTERVAL_SEC}s interval to trigger a checkpoint"

CKPT_AUTO_DONE=0
for _ in $(seq "${CKPT_WAIT_SEC}"); do
	CKPT_AUTO_DONE="$(wal_stat ckpt_done)"
	case "${CKPT_AUTO_DONE}" in
	''|*[!0-9]*) CKPT_AUTO_DONE=0 ;;
	esac
	[ "${CKPT_AUTO_DONE}" -ge 1 ] && break
	sleep 1
done

# ckpt_done counts what *this* process did, so any non-zero value here was
# unprompted -- round 3 never called the checkpoint RPC.
if [ "${CKPT_AUTO_DONE}" -ge 1 ]; then
	pass "round 3: ${CKPT_AUTO_DONE} checkpoint(s) happened without anyone asking"
else
	fail "round 3: no checkpoint in ${CKPT_WAIT_SEC}s with a ${CKPT_INTERVAL_SEC}s interval"
	grep -iE 'checkpoint' "${TGT_LOG}" | tail -5 | sed 's/^/       /'
fi

# Which trigger fired is the actual claim being tested. Without this the check
# above would also pass if the journal had happened to cross 50%, which is a
# different code path and not the one this section is about.
if grep -qF '(interval)' "${TGT_LOG}"; then
	pass "round 3: $(grep -oE "Checkpoint gen=[0-9]+ for '[^']*' \(interval\).*" "${TGT_LOG}" | tail -1)"
else
	fail "round 3: a checkpoint ran, but not because of the interval"
	grep -oE "Checkpoint gen=[0-9]+ .*\(.*\)" "${TGT_LOG}" | tail -3 | sed 's/^/       /'
fi

# gen is restored from the super block, so it has to have moved past round 1's --
# a checkpoint that did not update the super block leaves the journal untruncated,
# which is the failure mode that matters.
CKPT_GEN_NOW="$(wal_stat ckpt_gen)"
if [ "${CKPT_GEN_NOW}" != "missing" ] && [ "${CKPT_GEN_NOW}" -ge 2 ] 2>/dev/null; then
	pass "round 3: checkpoint generation advanced to ${CKPT_GEN_NOW} (round 1 left it at 1)"
else
	fail "round 3: checkpoint generation is ${CKPT_GEN_NOW}, expected at least 2"
fi

info "round 3: journal now $(wal_stat journal_used) of $(wal_stat journal_capacity) bytes used, covering LSN $(wal_stat ckpt_lsn)"

# ==========================================================================
# [4] Log inspection
# ==========================================================================
echo
echo "[4] checking the recovery round's log"

if grep -qiE 'assert|Segmentation|CRC mismatch' "${TGT_LOG}"; then
	fail "round 3: asserts, crashes or CRC failures in the log"
	grep -iE 'assert|Segmentation|CRC mismatch' "${TGT_LOG}" | \
		head -5 | sed 's/^/       /'
else
	pass "round 3: no asserts, crashes or CRC failures"
fi

# An unclosed batch at the tail of the log is expected and correct after a kill
# (W4), so this is reported rather than judged -- but a large number would mean
# batches were being dropped for some other reason.
DROPPED="$(grep -o 'dropped [0-9]* unclosed batch' "${TGT_LOG}" | \
	   grep -o '[0-9]*' | tail -1 || true)"
info "round 3: unclosed batches discarded at the tail: ${DROPPED:-0} (0 or 1 is normal after a kill)"

# ==========================================================================
# [3d] unload while the checkpoint interval is live
#
# This is where a use-after-free lived. Teardown is asynchronous -- a flusher
# drain and a WAL close, easily hundreds of milliseconds -- and the checkpoint
# poller used to keep running through all of it, because it was only
# unregistered once the drain had finished. An interval that came due inside
# that window started a checkpoint against a device being freed, and its last
# step writes: s3_journal_truncate() stores into the journal. The crash was a
# segfault with the journal pointer reading as "cos.ap-n", i.e. freed memory
# already reused for an endpoint string.
#
# Provoked rather than waited for: a write just before the unload advances
# applied_lsn (so a checkpoint has something to do, which ckpt_start() requires)
# and leaves the flusher with work (so the teardown window is wide). The
# assertion is on ordering in the log, which is deterministic whether or not the
# interval actually elapses in the window -- if the poller is alive after
# destroy, its checkpoint line appears after the destroy line.
# ==========================================================================
echo
echo "[3d] unloading with a ${CKPT_INTERVAL_SEC}s checkpoint interval running"

# Provoked rather than waited for, and the size of the write is the point. Three
# conditions have to line up inside the teardown window, and a small write only
# gets two of them:
#
#   - applied_lsn past what the last checkpoint covered. blobstore's own unload
#     metadata write supplies this for free -- that is what made the original
#     crash's checkpoint cover LSN 89 when the previous one covered 88;
#   - the interval due, which needs the window to be longer than the 1s poll
#     period, and a drain of a few hundred KiB is over in a fraction of that;
#   - the poller still running, which is the bug.
#
# So enough data is written to leave the flusher seconds of uploading to do. The
# writes are acknowledged from the WAL, so this returns long before S3 has any of
# it, and the drain then holds the window open across several polls.
PRE_UNLOAD_MB=0
for off in 64 72 80 88; do
	if dd if="${WORKDIR}/pat_a.bin" of="${NVME_DEV}" bs=1M count="${A_LEN_MB}" \
			seek="${off}" oflag=direct conv=fsync status=none \
			2>"${WORKDIR}/dd_pre_unload.err"; then
		PRE_UNLOAD_MB=$((PRE_UNLOAD_MB + A_LEN_MB))
	else
		fail "round 3: writing at ${off} MiB before the unload"
		sed 's/^/       /' "${WORKDIR}/dd_pre_unload.err"
		break
	fi
done
if [ "${PRE_UNLOAD_MB}" -gt 0 ]; then
	pass "round 3: wrote ${PRE_UNLOAD_MB} MiB just before the unload"
fi

# Disconnect first, the same way every other round does: it settles udev before
# pulling the device out, so the teardown does not leave read errors in dmesg that
# look like real failures.
disconnect_nvme
NVME_DEV=""

DESTROY_LINE_BEFORE="$(grep -c 'Destroying s3_bs_dev' "${TGT_LOG}" || true)"

if ! raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${LVS_NAME}")" \
		>/dev/null 2>"${WORKDIR}/unload3.err"; then
	fail "round 3: final unload"
	sed 's/^/       /' "${WORKDIR}/unload3.err"
else
	pass "round 3: recovered lvstore unloaded cleanly"
	LVS_EXISTS=0
fi

# The target must still be alive. A segfault here is the bug this step is about,
# and it would otherwise show up only as the unload's reply never arriving.
check_target "round 3, final unload" || exit 1

# From the *last* destroy, not the first: an attach that failed earlier in this
# round also builds and tears down a bs_dev, and counting from there would sweep
# up the checkpoints that legitimately ran in between.
DESTROY_AT="$(grep -n 'Destroying s3_bs_dev' "${TGT_LOG}" | tail -1 | cut -d: -f1)"
if [ -z "${DESTROY_AT}" ] || \
   [ "$(grep -c 'Destroying s3_bs_dev' "${TGT_LOG}")" -le "${DESTROY_LINE_BEFORE}" ]; then
	fail "round 3: the unload did not destroy the bs_dev"
	info "without that line the ordering check below proves nothing"
else
	# A checkpoint beginning after that line means the poller outlived destroy.
	# ckpt_start specifically: the completion lines carry the same prefix and
	# belong to a checkpoint that started legitimately before the unload.
	AFTER_DESTROY="$(tail -n +"${DESTROY_AT}" "${TGT_LOG}" | \
		grep -c 'ckpt_start.*Checkpoint gen=' || true)"
	if [ "${AFTER_DESTROY}" -eq 0 ]; then
		pass "round 3: no checkpoint was started after the destroy began"
	else
		fail "round 3: ${AFTER_DESTROY} checkpoint(s) started after the destroy"
		tail -n +"${DESTROY_AT}" "${TGT_LOG}" | \
			grep 'Checkpoint gen=' | head -3 | sed 's/^/       /'
		info "the poller must be unregistered by destroy() itself, not by the"
		info "drain completion -- everything a checkpoint touches is being freed"
	fi
fi

# A checkpoint that was already running when the unload arrived is the other half:
# teardown waits for it instead of freeing around it. Reported rather than judged,
# because whether the window was hit is a matter of timing -- but if it was hit,
# the wait has to be what the log shows.
if grep -q 'waiting for the checkpoint' "${TGT_LOG}"; then
	info "round 3: the unload caught a checkpoint in flight and waited for it"
fi

echo
echo "=== result: ${PASS} passed, ${FAIL} failed ==="
[ "${FAIL}" -eq 0 ] || exit 1
exit 0
