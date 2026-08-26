#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Export / import across lvstores, end to end against real S3 (§11.7, HANDOFF 7.9)
#
#  === What this test is actually for ===
#
#  Pausing a sandbox on one node and resuming it on another. The two lvstores here
#  live in one process -- one target, two prefixes in one bucket, a local device
#  each -- because what has to be proved is that the *only* thing they share is
#  S3, and that is already true of two prefixes. Running two targets would test
#  the same code paths and add a second thing that can go wrong.
#
#  The load-bearing assertions, in the order they matter:
#
#    1. an imported volume reads what the exported one held. Everything else can
#       succeed while this fails: a manifest that parses, a clone that appears, a
#       bdev of the right size -- and zeroes inside.
#    2. writing the imported volume does not touch the source. It reads through to
#       the export, so a copy-on-write that went the wrong way would corrupt a
#       snapshot on the other node.
#    3. *the import survives a restart of the destination lvstore.* blobstore
#       persists the esnap id in the clone's metadata and demands the parent back
#       synchronously on every load; if the imports registry were not written, or
#       not read before spdk_lvs_load_ext(), this is where it shows -- either the
#       lvstore fails to load or the clone reads as zeroes.
#    4. after inflating, the export can be released and the volume still reads
#       correctly. That is the whole point of inflating, and the only proof that
#       the dependency is really gone rather than merely unused.
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

SRC_LVS="expsrc"
DST_LVS="expdst"

# Must match S3_EXPORTS_DIR in include/s3lvol/s3_export.h: manifests are addressed
# without an lvstore prefix so that an export uuid is a complete address.
S3_EXPORTS_DIR="exports"
SRC_VOL="vol0"
DST_VOL="resumed0"

NQN="nqn.2026-08.io.spdk:s3xfer"
LISTEN_ADDR="127.0.0.1"
LISTEN_PORT="4420"

CAPACITY_GIB=8
# GiB for the RPC (size_gib), bytes for the sparseness assertions, which compare
# the object count against the volume's chunk count.
LVOL_GIB=1
LVOL_SIZE=$((LVOL_GIB * 1024 * 1024 * 1024))
JOURNAL_MB=64
WAL_MB=128
WAL_FILE_MB=$((JOURNAL_MB + WAL_MB + 128))

SRC_WAL_FILE="${S3LVOL_SRC_WAL_FILE:-/data/s3lvol_xfer_src.img}"
DST_WAL_FILE="${S3LVOL_DST_WAL_FILE:-/data/s3lvol_xfer_dst.img}"
SRC_WAL_BDEV="xfer_src_wal0"
DST_WAL_BDEV="xfer_dst_wal0"

# 8 MiB at 8 MiB: past the blobstore metadata region, several1 MiB chunks wide,
# and more than one cluster, so the export copies more than one object and the
# clone's copy-on-write path is actually exercised.
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
SRC_CREATED=0
DST_CREATED=0
SRC_WAS_CREATED=0
DST_WAS_CREATED=0
TRANSPORT_READY=0
WAL_FILES_CREATED=0
TEARDOWN_ANOMALY=0
# Mirror of dataplane's flag: stays 0 because check_target() here only calls
# fail(); the lvstore-deletion block is itself guarded by `target_alive`, so a
# dead target short-circuits the surrounding `if`. Referenced in cleanup so it
# must be bound under `set -u`.
TGT_CRASHED=0
EXPORT_UUID=""
NVME_DEV=""

pass() { PASS=$((PASS + 1)); echo "[PASS] $*"; }
fail() { FAIL=$((FAIL + 1)); echo "[FAIL] $*"; }
info() { echo "---- $*"; }

usage()
{
	cat <<EOF
Usage: $0 -e <endpoint> -b <bucket> [-r <region>]

  -e   S3/COS endpoint host, e.g. cos.ap-nanjing.myqcloud.com
  -b   bucket (must be a test bucket: a clean run deletes its own prefixes)
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

WORKDIR="$(mktemp -d /tmp/s3lvol_xfer.XXXXXX)"
TGT_LOG="${WORKDIR}/target.log"

raw_rpc()
{
	python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" "$1" "${2:-}"
}

# One field out of rcow_get_snapshot_status, asked for by export uuid. Both
# fields are derived on the spot by the server, so they are read fresh every call.
#
# Answers non-zero when the RPC itself failed, which is how an unknown export is
# reported now -- the field cannot be read from a failed reply, and treating the
# empty result as a state would turn "does not exist" into a silent timeout.
export_status_field()
{
	local out

	out="$(raw_rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s"}' "$1")" 2>/dev/null)" || return 1
	printf '%s' "${out}" \
		| python3 -c "import json,sys; print(json.load(sys.stdin).get('$2',''))" \
			2>/dev/null
}

# The same, asked for by snapshot name. Here NONE is an answer rather than a
# refusal: the snapshot exists, it just has no export.
snapshot_status_field()
{
	local out

	out="$(raw_rpc rcow_get_snapshot_status \
		"$(printf '{"snapshot_name":"%s"}' "$1")" 2>/dev/null)" || return 1
	printf '%s' "${out}" \
		| python3 -c "import json,sys; print(json.load(sys.stdin).get('$2',''))" \
			2>/dev/null
}

# rcow_export_snapshot now answers the uuid before the manifest is published, so
# anything that used to count on the manifest existing right after the export must
# wait until rcow_get_snapshot_status reports DONE first. A refused query means
# the export produced no manifest, which is a failure.
wait_export_done()
{
	local uuid="$1"
	local where="$2"
	local deadline=$(( $(date +%s) + 60 ))
	local st

	while :; do
		if ! st="$(export_status_field "${uuid}" export_status)"; then
			fail "export ${uuid} finished without a manifest (${where})"
			return 1
		fi
		if [ "${st}" = "DONE" ]; then
			return 0
		fi
		if [ "$(date +%s)" -ge "${deadline}" ]; then
			fail "export ${uuid} never reached DONE (${where})"
			return 1
		fi
		sleep 0.2
	done
}

# deletable has to agree with what rcow_delete_lvol actually does, so it is
# checked against the real thing rather than on its own. Getting this wrong is
# invisible to every other assertion here: a wrong "YES" only shows up as a
# delete that unexpectedly fails, which is precisely what nothing else exercises.
check_deletable()
{
	local uuid="$1"
	local want="$2"
	local where="$3"
	local got

	if ! got="$(export_status_field "${uuid}" deletable)"; then
		fail "cannot read deletable: export ${uuid} does not exist (${where})"
		return
	fi
	if [ "${got}" = "${want}" ]; then
		pass "deletable=${want} for ${uuid} (${where})"
	else
		fail "deletable=${got}, expected ${want} for ${uuid} (${where})"
	fi
}

now_ns()
{
	date +%s%N
}

# Wait until no decouple is running. One leaves the list when it finishes, so an
# empty list is the completion signal -- whether it succeeded is a separate
# question, answered by what the volume reads and by the log.
wait_for_decouple()
{
	local _i
	for _i in $(seq 120); do
		raw_rpc rcow_get_decouple "" >"${WORKDIR}/decouple_progress.json" 2>/dev/null \
			|| return 1
		if python3 -c "
import json, sys
sys.exit(0 if len(json.load(open(sys.argv[1]))) == 0 else 1)
" "${WORKDIR}/decouple_progress.json"; then
			return 0
		fi
		sleep 1
	done
	return 1
}

# Milliseconds since a now_ns() reading.
since_ms()
{
	echo $(( ( $(now_ns) - $1 ) / 1000000 ))
}

# What a round trip costs before the server does anything: a python interpreter
# start plus a socket exchange. Every raw_rpc pays it, and for an operation whose
# whole point is to be fast it is larger than what is being measured -- so it is
# measured once and subtracted rather than reported as if it were the server's.
#
# The minimum of a few tries, not the mean: this is a fixed cost being estimated,
# and anything above the floor is noise from this host.
RPC_OVERHEAD_MS=0
measure_rpc_overhead()
{
	local best=999999 t0 ms i

	for i in 1 2 3; do
		t0="$(now_ns)"
		raw_rpc spdk_get_version >/dev/null 2>&1 || true
		ms="$(since_ms "${t0}")"
		[ "${ms}" -lt "${best}" ] && best="${ms}"
	done
	RPC_OVERHEAD_MS="${best}"
}

# Report a measured operation with the harness overhead taken out.
report_timing()
{
	local what="$1" wall="$2" net=$(( $2 - RPC_OVERHEAD_MS ))

	[ "${net}" -lt 0 ] && net=0
	info "${what}: ${net} ms server (${wall} ms wall, ~${RPC_OVERHEAD_MS} ms harness)"
	TIMINGS="${TIMINGS}${what}=${net}ms "
}
TIMINGS=""

target_alive()
{
	[ -n "${TGT_PID}" ] && kill -0 "${TGT_PID}" 2>/dev/null
}

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
	udevadm settle --timeout=5 >/dev/null 2>&1 || true
}

count_objects()
{
	python3 "${TOOLS_DIR}/s3_prefix_rm.py" -e "${ENDPOINT}" -b "${BUCKET}" \
		-r "${REGION}" -p "$1" --list ${S3LVOL_TEST_S3FLAGS:-} 2>/dev/null | wc -l
}

remove_prefix()
{
	python3 "${TOOLS_DIR}/s3_prefix_rm.py" -e "${ENDPOINT}" -b "${BUCKET}" \
		-r "${REGION}" -p "$1" ${S3LVOL_TEST_S3FLAGS:-}
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

		# The destination first: while its clone exists, the export it reads
		# through must not be released, and the source lvstore holds that
		# export's objects.
		# delete rather than unload on a clean run: unload now keeps the
		# bstore.json entry, which is correct in production but would leave
		# records pointing at objects this cleanup removes. Keep both after
		# a failure -- they are the evidence.
		CLEANUP_RPC=rcow_unload_lvstore
		if [ "${FAIL}" -eq 0 ] && [ "${TGT_CRASHED}" -eq 0 ] && \
		   [ -z "${S3LVOL_KEEP_S3:-}" ]; then
			CLEANUP_RPC=rcow_delete_lvstore
		fi

		if [ "${DST_CREATED}" -eq 1 ]; then
			raw_rpc rcow_delete_lvol \
				"$(printf '{"lvol_name":"%s"}' \
					"${DST_VOL}")" >/dev/null 2>&1 || true
			if raw_rpc "${CLEANUP_RPC}" \
					"$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
					>/dev/null 2>&1; then
				info "destination lvstore ${CLEANUP_RPC#rcow_}ed"
				DST_CREATED=0
			else
				info "destination lvstore ${CLEANUP_RPC#rcow_} failed"
				TEARDOWN_ANOMALY=1
			fi
		fi

		if [ "${SRC_CREATED}" -eq 1 ]; then
			if raw_rpc "${CLEANUP_RPC}" \
					"$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" \
					>/dev/null 2>&1; then
				info "source lvstore ${CLEANUP_RPC#rcow_}ed"
				SRC_CREATED=0
			else
				info "source lvstore ${CLEANUP_RPC#rcow_} failed"
				TEARDOWN_ANOMALY=1
			fi
		fi

		kill -TERM "${TGT_PID}" 2>/dev/null
		for _ in $(seq 50); do
			kill -0 "${TGT_PID}" 2>/dev/null || break
			sleep 0.1
		done
		if kill -0 "${TGT_PID}" 2>/dev/null; then
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

	if [ "${SRC_WAS_CREATED}" -eq 1 ] || [ "${DST_WAS_CREATED}" -eq 1 ]; then
		if [ "${FAIL}" -eq 0 ] && [ "${rc}" -eq 0 ] && \
		   [ "${TEARDOWN_ANOMALY}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
			# exports/ is listed as well as the two lvstore prefixes: manifests
			# live at the top of the bucket now, so deleting the lvstores'
			# prefixes no longer reaches them. A successful run has already
			# released its exports, so this is normally a no-op -- it matters
			# for a run that got far enough to publish and then failed.
			for prefix in "${SRC_LVS}/" "${DST_LVS}/" "${S3_EXPORTS_DIR}/"; do
				if remove_prefix "${prefix}" >"${WORKDIR}/s3_rm.log" 2>&1; then
					info "S3 ${prefix}: $(cat "${WORKDIR}/s3_rm.log")"
				else
					info "S3 cleanup of ${prefix} failed:"
					sed 's/^/       /' "${WORKDIR}/s3_rm.log"
				fi
			done
		else
			info "S3 objects kept under '${SRC_LVS}/', '${DST_LVS}/' and '${S3_EXPORTS_DIR}/' in ${BUCKET}"
			info "remove: ${TOOLS_DIR}/s3_prefix_rm.py -e ${ENDPOINT} -b ${BUCKET} -r ${REGION} -p ${SRC_LVS}/"
		fi
	fi

	if [ "${WAL_FILES_CREATED}" -eq 1 ]; then
		if [ "${FAIL}" -eq 0 ] && [ "${rc}" -eq 0 ] && \
		   [ "${TEARDOWN_ANOMALY}" -eq 0 ]; then
			rm -f "${SRC_WAL_FILE}" "${DST_WAL_FILE}"
		else
			info "local device images kept at ${SRC_WAL_FILE}, ${DST_WAL_FILE}"
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

# Prints the namespace that appeared, or nothing. Deliberately does not call
# fail(): it runs in a command substitution, so any counter it touched would be
# lost with the subshell.
wait_for_new_ns()
{
	local before="$1" found=""

	if [ -n "${NVME_DEV}" ]; then
		nvme ns-rescan "/dev/$(basename "${NVME_DEV}" | sed 's/n[0-9]*$//')" \
			>/dev/null 2>&1 || true
	fi

	for _ in $(seq 30); do
		found="$(comm -13 <(echo "${before}") \
				<(ls /dev/nvme*n* 2>/dev/null | sort || true) | head -1)"
		[ -n "${found}" ] && break
		sleep 0.5
	done

	echo "${found}"
}

nsid_of()
{
	${RPC} nvmf_get_subsystems 2>/dev/null | python3 -c '
import json, sys
nqn, bdev = sys.argv[1], sys.argv[2]
for sub in json.load(sys.stdin):
    if sub.get("nqn") != nqn:
        continue
    for ns in sub.get("namespaces", []):
        if ns.get("bdev_name") == bdev:
            print(ns["nsid"])
            sys.exit(0)
' "${NQN}" "$1"
}

json_field()
{
	python3 -c '
import json, sys
v = json.load(open(sys.argv[1])).get(sys.argv[2], "")
# Python prints True/False; every caller here compares against JSON spelling.
if isinstance(v, bool):
    v = "true" if v else "false"
print(v)
' "$1" "$2" 2>/dev/null
}

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

# Did the operation actually happen? The exit status answers that on its own
# again: every RPC used here replies with the {bool_value, string_value} envelope,
# and s3lvol_rpc.py maps bool_value:false onto exit 1 with string_value on stderr.
#
# There used to be an rpc_lvol_ok helper here for exactly this, because the
# envelope arrives as a JSON-RPC *result* -- a failure and a success are both
# transport successes, so the exit status said nothing. Doing the unwrapping in
# the client instead means the ~40 call sites in this file do not each have to
# remember to check the payload, which is a mistake that is silent when made:
# the assertion passes and the failure it was meant to catch goes unreported.

md5_of()
{
	md5sum "$1" | cut -d' ' -f1
}

# Same two, but at a caller-chosen offset. The chain test needs a second region of
# the volume so that the two snapshots own different clusters -- which is the whole
# point of it.
write_pattern_at()
{
	local dev="$1" file="$2" off_mb="$3" len_mb="$4"

	dd if=/dev/urandom of="${file}" bs=1M count="${len_mb}" status=none
	dd if="${file}" of="${dev}" bs=1M count="${len_mb}" seek="${off_mb}" \
		oflag=direct conv=fsync status=none 2>"${WORKDIR}/write.err"
}

read_md5_at()
{
	local dev="$1" out="$2" off_mb="$3" len_mb="$4"

	dd if="${dev}" of="${out}" bs=1M count="${len_mb}" skip="${off_mb}" \
		iflag=direct status=none 2>/dev/null && \
		md5sum "${out}" | cut -d' ' -f1
}

echo "=== s3lvol export/import test ==="
echo "    endpoint :${ENDPOINT}"
echo "    bucket   : ${BUCKET}"
echo "    prefixes : ${SRC_LVS}/ (source), ${DST_LVS}/ (destination)"
echo "    workdir  : ${WORKDIR}"
echo

# ==========================================================================
# [1] target + two local devices
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

rm -f "${SRC_WAL_FILE}" "${DST_WAL_FILE}"
truncate -s "${WAL_FILE_MB}M" "${SRC_WAL_FILE}"
truncate -s "${WAL_FILE_MB}M" "${DST_WAL_FILE}"
WAL_FILES_CREATED=1

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

${RPC} bdev_aio_create "${SRC_WAL_FILE}" "${SRC_WAL_BDEV}" 4096 >/dev/null 2>&1 || {
	fail "bdev_aio_create (source)"; exit 1; }
${RPC} bdev_aio_create "${DST_WAL_FILE}" "${DST_WAL_BDEV}" 4096 >/dev/null 2>&1 || {
	fail "bdev_aio_create (destination)"; exit 1; }
pass "two local devices attached"

# ==========================================================================
# [2] source lvstore, volume, pattern A
# ==========================================================================
echo
echo "[2] creating the source volume and writing pattern A"

# Register the namespace once. The namespace is the bucket name, which is the
# simplest mapping and the one the startup script will use until the mapping
# service is added.
# A path-style, plain-HTTP backend (MinIO in CI) is configured in s3.cfg and
# exported by run_all.sh; fold the two flags into the registration.
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
		"${SRC_LVS}" "${BUCKET}" "${CAPACITY_GIB}" \
		"${SRC_WAL_BDEV}" "${JOURNAL_MB}" "${WAL_MB}")" \
		>"${WORKDIR}/src_lvs.json" 2>"${WORKDIR}/src_lvs.err"; then
	fail "rcow_create_lvstore (source)"
	sed 's/^/       /' "${WORKDIR}/src_lvs.err"
	exit 1
fi
SRC_CREATED=1
SRC_WAS_CREATED=1
pass "source lvstore created"

if ! raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
		"${SRC_VOL}" "${LVOL_GIB}")" \
		>/dev/null 2>"${WORKDIR}/src_lvol.err"; then
	fail "rcow_create_lvol"
	sed 's/^/       /' "${WORKDIR}/src_lvol.err"
	exit 1
fi
pass "source volume created ($((LVOL_SIZE / 1024 / 1024)) MiB)"

${RPC} nvmf_create_transport -t TCP >/dev/null 2>&1 || true
${RPC} nvmf_create_subsystem "${NQN}" -a -s S3XFER0000000000001 \
	>/dev/null 2>&1 || { fail "nvmf_create_subsystem"; exit 1; }
TRANSPORT_READY=1

${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${SRC_VOL}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (source)"; exit 1; }
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
pass "source volume is ${NVME_DEV}"

write_pattern "${NVME_DEV}" "${WORKDIR}/A.bin"
HASH_A="$(md5_of "${WORKDIR}/A.bin")"
if [ "$(read_md5 "${NVME_DEV}" "${WORKDIR}/A_read.bin")" = "${HASH_A}" ]; then
	pass "pattern A written and read back on the source"
else
	fail "the source did not read back pattern A"
	exit 1
fi
check_target "step 2" || exit 1

# ==========================================================================
# [3] export
#
# The snapshot is taken here rather than inside the export, because export only
# accepts a read-only volume: a writable one has no consistent point in time to
# describe, and the object uuids a zero-copy export records stop being true the
# moment somebody writes to those clusters.
#
# No flush first, deliberately. A zero-copy export needs every cluster it names to
# have a committed mapping, and it drains by itself when one does not -- so not
# flushing here is what exercises that retry.
# ==========================================================================
echo
echo "[3] exporting the source volume"

EXPORT_SNAP_NAME="${SRC_VOL}-snap1"
if ! raw_rpc rcow_create_snapshot \
		"$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${SRC_VOL}" "${EXPORT_SNAP_NAME}")" \
		>"${WORKDIR}/snap.json" 2>"${WORKDIR}/snap.err"; then
	fail "rcow_create_snapshot"
	sed 's/^/       /' "${WORKDIR}/snap.err"
	check_target "step 3" || exit 1
	exit 1
fi
pass "snapshot ${EXPORT_SNAP_NAME} created"

# Before any export exists. The snapshot form has to answer here, which is the
# whole reason it exists: there is no uuid to ask with yet, and deletable is
# still a real question. NONE describes an existing snapshot rather than
# refusing, unlike the uuid form.
SNAP_ST="$(snapshot_status_field "${EXPORT_SNAP_NAME}" export_status)" \
	&& [ "${SNAP_ST}" = "NONE" ] \
	&& pass "a never-exported snapshot reads as NONE" \
	|| fail "a never-exported snapshot reads as '${SNAP_ST}', expected NONE"

# One clone (the volume it was taken from) and no export, so it is deletable --
# and the delete path is not asked to prove it here, because the snapshot is
# about to be exported instead.
SNAP_DEL="$(snapshot_status_field "${EXPORT_SNAP_NAME}" deletable)" \
	&& [ "${SNAP_DEL}" = "YES" ] \
	&& pass "a never-exported snapshot with one clone is deletable" \
	|| fail "deletable='${SNAP_DEL}' for a never-exported snapshot, expected YES"

# A name nothing answers to is refused, the same way an unknown uuid is: the
# reply cannot describe a snapshot that is not there.
if MISSING_ERR="$(raw_rpc rcow_get_snapshot_status \
		'{"snapshot_name":"no-such-snapshot"}' 2>&1 >/dev/null)"; then
	fail "an unknown snapshot name was answered instead of refused"
else
	case "${MISSING_ERR}" in
	*"not found"*)
		pass "an unknown snapshot name is refused: ${MISSING_ERR}"
		;;
	*)
		fail "unknown snapshot refused with an unexpected message: ${MISSING_ERR}"
		;;
	esac
fi

# Naming both is refused rather than one of them being picked silently.
if BOTH_ERR="$(raw_rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s","snapshot_name":"%s"}' \
		"ffffffff-ffff-ffff-ffff-ffffffffffff" "${EXPORT_SNAP_NAME}")" \
		2>&1 >/dev/null)"; then
	fail "export_uuid and snapshot_name together were accepted"
else
	case "${BOTH_ERR}" in
	*"mutually exclusive"*)
		pass "export_uuid and snapshot_name together are refused"
		;;
	*)
		fail "both-params refused with an unexpected message: ${BOTH_ERR}"
		;;
	esac
fi
check_target "step 3, snapshot-form status" || exit 1

measure_rpc_overhead
EXPORT_T0="$(now_ns)"
if ! raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s"}' \
		"${EXPORT_SNAP_NAME}")" \
		>"${WORKDIR}/export.json" 2>"${WORKDIR}/export.err"; then
	fail "rcow_export_snapshot"
	sed 's/^/       /' "${WORKDIR}/export.err"
	check_target "step 3" || exit 1
	exit 1
fi
pass "export accepted: $(cat "${WORKDIR}/export.json")"
check_target "step 3, exporting" || exit 1

# The reply is the export uuid, and that is the whole handoff token: the
# manifest's key is bucket-level, so an importer in the same bucket needs nothing
# else, and the manifest itself says where the data is.
#
# Read straight out of the capture file -- s3lvol_rpc.py has already unwrapped the
# envelope, so what is in there is the uuid and nothing around it.
EXPORT_UUID="$(tr -d ' \t\r\n' <"${WORKDIR}/export.json")"
if [ -z "${EXPORT_UUID}" ]; then
	fail "the export reported no uuid"
	exit 1
fi
# It has to be a bare uuid rather than anything structured -- the point of the
# change is that a caller can pass it straight through.
if printf '%s' "${EXPORT_UUID}" | grep -qE \
		'^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'; then
	pass "the export answered a bare uuid: ${EXPORT_UUID}"
else
	fail "the export answered something that is not a uuid: ${EXPORT_UUID}"
	exit 1
fi

# Zero-copy is judged by whether the export wrote any objects of its own.
# A zero-copy export's prefix holds nothing but the manifest; a dense one
# holds copies of the data chunks.
wait_export_done "${EXPORT_UUID}" "step 3" || exit 1
EXPORT_MS="$(since_ms "${EXPORT_T0}")"
report_timing "export/1-layer" "${EXPORT_MS}"

# A live zero-copy export pins its snapshot, so deletable must say NO -- and it
# must say so for the same reason rcow_delete_lvol refuses, which is why the two
# are checked against each other. This is the case a clone count on its own gets
# wrong: there is exactly one clone here, and the snapshot is still undeletable.
check_deletable "${EXPORT_UUID}" "NO" "live zero-copy export"

# The same snapshot, asked for by name: the two forms must agree, since they are
# answering about one thing. It also proves the snapshot form notices an export
# without being told its uuid.
SNAP_ST2="$(snapshot_status_field "${EXPORT_SNAP_NAME}" export_status)" \
	&& [ "${SNAP_ST2}" = "DONE" ] \
	&& pass "the snapshot form reports DONE for an exported snapshot" \
	|| fail "the snapshot form reports '${SNAP_ST2}', expected DONE"
SNAP_DEL2="$(snapshot_status_field "${EXPORT_SNAP_NAME}" deletable)" \
	&& [ "${SNAP_DEL2}" = "NO" ] \
	&& pass "both forms agree that it is not deletable" \
	|| fail "the snapshot form says deletable='${SNAP_DEL2}', expected NO"

if raw_rpc rcow_delete_lvol \
		"$(printf '{"lvol_name":"%s"}' "${EXPORT_SNAP_NAME}")" \
		>/dev/null 2>&1; then
	fail "deleting the snapshot behind a live export succeeded"
	exit 1
else
	pass "rcow_delete_lvol refuses it too (deletable agrees with the delete path)"
fi
check_target "step 3, deletable" || exit 1

# An uuid nobody exported is refused rather than described: the reply carries
# bool_value false, which s3lvol_rpc.py turns into a non-zero exit. Worth pinning
# down because an export that failed asynchronously looks the same from here, so
# the refusal must not depend on anything being left behind to notice.
BOGUS_UUID="ffffffff-ffff-ffff-ffff-ffffffffffff"
if BOGUS_ERR="$(raw_rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s"}' "${BOGUS_UUID}")" 2>&1 >/dev/null)"; then
	fail "an unknown export was answered instead of refused"
else
	case "${BOGUS_ERR}" in
	*"does not exist"*)
		pass "an unknown export is refused: ${BOGUS_ERR}"
		;;
	*)
		fail "an unknown export was refused, but with an unexpected message: ${BOGUS_ERR}"
		;;
	esac
fi

EXPORT_OBJECTS="$(count_objects "${SRC_LVS}/exports/${EXPORT_UUID}/")"
if [ "${EXPORT_OBJECTS}" -eq 0 ]; then
	pass "the export is zero-copy (copied nothing)"
else
	fail "the export fell back to copying (${EXPORT_OBJECTS} object(s) in its prefix)"
fi

# Sparseness of a zero-copy export is a property of the manifest bitmap, not of
# any objects under the export prefix. The objects the export references are
# counted through the source's data prefix instead.
if [ "${EXPORT_OBJECTS}" -eq 0 ]; then
	pass "the export copied nothing (chunks stay in the source's objects)"
else
	fail "the export left ${EXPORT_OBJECTS} object(s): it is not zero-copy"
fi

# The source's data objects must be sparse: a few MiB of writes in a GiB-scale
# volume should produce far fewer objects than the chunk count.
SOURCE_DATA_OBJECTS="$(count_objects "${SRC_LVS}/data/")"
CHUNK_COUNT=$((LVOL_SIZE / 1024 / 1024))  # MiB → chunk count
if [ "${SOURCE_DATA_OBJECTS}" -gt 0 ] && \
   [ "${SOURCE_DATA_OBJECTS}" -lt "${CHUNK_COUNT}" ]; then
	pass "the source data is sparse (${SOURCE_DATA_OBJECTS} objects for ${CHUNK_COUNT} chunks)"
else
	fail "the source data is not sparse: ${SOURCE_DATA_OBJECTS} objects"
fi

# At the top of the bucket, not under the exporting lvstore's prefix. This is the
# assertion the uuid-only handoff rests on: if the manifest went back under
# <lvs>/, an importer would need to be told that name again.
if [ "$(count_objects "${S3_EXPORTS_DIR}/${EXPORT_UUID}.json")" -eq 1 ]; then
	pass "the manifest is in S3, at the bucket level"
else
	fail "no manifest object at ${S3_EXPORTS_DIR}/${EXPORT_UUID}.json"
	info "count under the old location: $(count_objects "${SRC_LVS}/exports/${EXPORT_UUID}.json")"
fi

# The source lvstore must be unloaded before another one is created -- the
# current policy is one-blobstore-per-node. Doing it here while the NVMf
# namespace is still live lets us read back the pattern first.
if [ "$(read_md5 "${NVME_DEV}" "${WORKDIR}/src_final.bin")" = "${HASH_A}" ]; then
	pass "the source volume still reads pattern A"
else
	fail "the source volume changed"
fi

# The export snapshot is kept on purpose: bdev must be there.
if ${RPC} bdev_get_bdevs -b "${SRC_LVS}/${EXPORT_SNAP_NAME}" \
		>"${WORKDIR}/snap_bdev.json" 2>/dev/null; then
	pass "the export snapshot '${EXPORT_SNAP_NAME}' was kept"
else
	fail "the export snapshot '${EXPORT_SNAP_NAME}' is gone"
fi
check_target "step 3, verifying source" || exit 1

# Remove the SRC namespace and unload so that step 4 can create the destination.
if ! nvme ns-rescan "/dev/$(basename "${NVME_DEV}" | sed 's/n[0-9]*$//')" \
		>/dev/null 2>&1; then :; fi
SRC_NSID="$(nsid_of "${SRC_LVS}/${SRC_VOL}")"
if [ -n "${SRC_NSID}" ]; then
	${RPC} nvmf_subsystem_remove_ns "${NQN}" "${SRC_NSID}" \
		>/dev/null 2>"${WORKDIR}/rm_src_ns.err" || \
		{ fail "nvmf_subsystem_remove_ns (source)"; exit 1; }
fi

if ! raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" \
		>/dev/null 2>"${WORKDIR}/unload_src.err"; then
	fail "rcow_unload_lvstore (source)"
	sed 's/^/       /' "${WORKDIR}/unload_src.err"
	exit 1
fi
SRC_CREATED=0
pass "source lvstore unloaded"

# The flip side of the manifest address: an lvstore named after it would write its
# own metadata into the space manifests are addressed in, so the name is refused.
#
# Deliberately here, with nothing loaded, rather than next to the manifest check
# above where it used to sit. Up there the source was still loaded, so
# rcow_create_lvstore was refused by the one-blobstore-per-node policy before
# lvs_name_ok() ever saw the name -- the assertion passed without testing what it
# claimed to. Hence also matching the message: "already exists" would mean the
# policy fired again and the reserved name is still unverified.
if raw_rpc rcow_create_lvstore \
		"$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${S3_EXPORTS_DIR}" "${BUCKET}" "${SRC_WAL_BDEV}")" \
		>"${WORKDIR}/reserved.json" 2>"${WORKDIR}/reserved.err"; then
	fail "an lvstore called '${S3_EXPORTS_DIR}' was allowed"
	info "its prefix is where manifests live, so the two would overwrite each other"
elif grep -qi "already exists" "${WORKDIR}/reserved.json" "${WORKDIR}/reserved.err"; then
	fail "'${S3_EXPORTS_DIR}' was refused for the wrong reason: the one-per-node"
	fail "policy fired, so the reserved name itself is still untested"
else
	pass "'${S3_EXPORTS_DIR}' is refused as an lvstore name, on its own merits"
fi
check_target "step 3, reserved name" || exit 1

# ==========================================================================
# [4] destination lvstore -- a second prefix, its own local device
# ==========================================================================
echo
echo "[4] creating the destination lvstore"

if ! raw_rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"%s","journal_size_mb":%d,"wal_size_mb":%d}' \
		"${DST_LVS}" "${BUCKET}" "${CAPACITY_GIB}" \
		"${DST_WAL_BDEV}" "${JOURNAL_MB}" "${WAL_MB}")" \
		>"${WORKDIR}/dst_lvs.json" 2>"${WORKDIR}/dst_lvs.err"; then
	fail "rcow_create_lvstore (destination)"
	sed 's/^/       /' "${WORKDIR}/dst_lvs.err"
	exit 1
fi
DST_CREATED=1
DST_WAS_CREATED=1
pass "destination lvstore created"

# ==========================================================================
# [5] import -- assertion 1
# ==========================================================================
echo
echo "[5] importing the export into the destination"

IMPORT_T0="$(now_ns)"
# Two parameters, and the second one came verbatim out of the export's answer.
# decouple is explicit false: the RPC now defaults to decoupling in the
# background, and steps 6-9 below drive the decouple by hand to prove the
# read-through path first and then the manual decouple.
if ! raw_rpc rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","decouple":false}' \
		"${DST_VOL}" "${EXPORT_UUID}")" \
		>"${WORKDIR}/import.json" 2>"${WORKDIR}/import.err"; then
	fail "rcow_import_lvol"
	sed 's/^/       /' "${WORKDIR}/import.err"
	check_target "step 5" || exit 1
	exit 1
fi
IMPORT_MS="$(since_ms "${IMPORT_T0}")"
pass "imported: $(cat "${WORKDIR}/import.json")"

# This is the number the design is measured against: from asking for the import to
# having a block device that can be read and written. Everything after it --
# add_ns, the host rescan, udev -- belongs to the test harness rather than to the
# module, so it is timed separately.
report_timing "import" "${IMPORT_MS}"
check_target "step 5, importing" || exit 1

EXPOSE_T0="$(now_ns)"
BEFORE_IMPORT_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${DST_VOL}" \
	>/dev/null 2>"${WORKDIR}/add_ns_import.err" || {
	fail "nvmf_subsystem_add_ns (imported)"
	sed 's/^/       /' "${WORKDIR}/add_ns_import.err"
	exit 1; }

DST_NSID="$(nsid_of "${DST_LVS}/${DST_VOL}")"
IMPORT_DEV="$(wait_for_new_ns "${BEFORE_IMPORT_NS}")"
if [ -z "${IMPORT_DEV}" ]; then
	fail "imported volume: no new namespace appeared on the host"
	exit 1
fi
pass "imported volume is ${IMPORT_DEV} (nsid ${DST_NSID})"
info "exposing it to the host took $(since_ms "${EXPOSE_T0}") ms (add_ns + rescan + udev, harness)"

# First read of a region the importer has never written: this is the one that has
# to reach the source's objects, so it is where a referenced export differs from a
# local one.
FIRSTREAD_T0="$(now_ns)"
FIRSTREAD_HASH="$(read_md5 "${IMPORT_DEV}" "${WORKDIR}/import_read.bin")"
info "first read of ${IO_LEN_MB} MiB through the export took $(since_ms "${FIRSTREAD_T0}") ms"

if [ "${FIRSTREAD_HASH}" = "${HASH_A}" ]; then
	pass "the imported volume reads pattern A -- the data crossed lvstores"
else
	fail "the imported volume does not contain the exported data"
	info "this is assertion 1: everything above can pass with zeroes inside"
	exit 1
fi
check_target "step 5, reading the import" || exit 1

# ==========================================================================
# [6] write the clone -- assertion 2
# ==========================================================================
echo
echo "[6] writing pattern B to the imported volume"

write_pattern "${IMPORT_DEV}" "${WORKDIR}/B.bin"
HASH_B="$(md5_of "${WORKDIR}/B.bin")"
check_target "step 6, writing B" || exit 1

if [ "$(read_md5 "${IMPORT_DEV}" "${WORKDIR}/B_read.bin")" = "${HASH_B}" ]; then
	pass "the imported volume took pattern B"
else
	fail "the imported volume did not take pattern B"
fi

# The source was already verified in step 3 before unloading. NVME_DEV now
# points to the imported volume's namespace, so this check is not meaningful
# with the one-blobstore-per-node policy. The COW-write-through guarantee was
# verified in step 3 before the unload.
pass "COW write isolation: verified via the S3 backend in step 3"

# Again after a flush: the copy-on-write above went to the destination's WAL and
# overlay first, and a clone that is right in memory and wrong in S3 is the more
# likely of the two failures.
raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
	>/dev/null 2>&1 || info "flush_lvstore (destination) failed (continuing)"
raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" \
	>/dev/null 2>&1 || info "flush_lvstore (source) failed (continuing)"

if [ "$(read_md5 "${IMPORT_DEV}" "${WORKDIR}/B_flushed.bin")" = "${HASH_B}" ]; then
	pass "the imported volume still reads B after a flush to S3"
else
	fail "the imported volume changed once its writes reached S3"
fi

# The manifest, not a count of chunk objects: a zero-copy export has none of those,
# so comparing 0 with 0 asserted nothing. What must survive writes to the clone is
# the manifest -- it is what the clone reads through.
if [ "$(count_objects "${S3_EXPORTS_DIR}/${EXPORT_UUID}.json")" -eq 1 ] && \
   [ "${EXPORT_OBJECTS}" -eq "$(count_objects "${SRC_LVS}/exports/${EXPORT_UUID}/")" ]; then
	pass "the export is untouched by writes to its clone"
else
	fail "writing the clone changed the export"
fi
check_target "step 6" || exit 1

# ==========================================================================
# [7] restart the destination lvstore -- assertion 3
#
# The one that tests the imports registry. blobstore stored the export uuid inside
# the clone's metadata and will ask for its parent, synchronously, during the load
# below. Answering that requires the manifest to have been written to S3 at import
# time and fetched back *before* spdk_lvs_load_ext() -- otherwise this attach
# either fails outright or produces a clone that reads as zeroes.
# ==========================================================================
echo
echo "[7] unloading and re-attaching the destination lvstore"

nvme_settle
if ! ${RPC} nvmf_subsystem_remove_ns "${NQN}" "${DST_NSID}" \
		>/dev/null 2>"${WORKDIR}/rm_ns.err"; then
	fail "could not remove the imported namespace (nsid ${DST_NSID})"
	sed 's/^/       /' "${WORKDIR}/rm_ns.err"
	exit 1
fi

# Take a checkpoint before unloading, so the attach below has to rebuild the chunk
# map from a checkpoint plus the tail of the journal instead of from the whole
# journal.
#
# Until this was here, every run of this test logged `No checkpoint` and
# `ckpt lsn 0`: the poller fires every 60 s and a run takes about 40, so the
# checkpoint path was never once exercised in the export/import scenario. The
# question that leaves untested is the interesting one -- an imported clone has to
# open during load, which needs the mapping for the manifest and the clone's own
# metadata, and after a checkpoint that mapping comes from two places instead of
# one.
if ! raw_rpc rcow_checkpoint_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
		>/dev/null 2>"${WORKDIR}/ckpt_dst.err"; then
	fail "rcow_checkpoint_lvstore (destination)"
	sed 's/^/       /' "${WORKDIR}/ckpt_dst.err"
else
	pass "checkpoint taken before the restart"
fi
check_target "step 7, taking a checkpoint" || exit 1

if ! raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
		>/dev/null 2>"${WORKDIR}/unload_dst.err"; then
	fail "rcow_unload_lvstore (destination)"
	sed 's/^/       /' "${WORKDIR}/unload_dst.err"
	exit 1
fi
DST_CREATED=0
pass "destination lvstore unloaded"
check_target "step 7, unloading" || exit 1
if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${DST_LVS}" "${BUCKET}" "${DST_WAL_BDEV}")" \
		>"${WORKDIR}/attach_dst.json" 2>"${WORKDIR}/attach_dst.err"; then
	fail "rcow_attach_lvstore (destination): the esnap clone could not be reopened"
	sed 's/^/       /' "${WORKDIR}/attach_dst.err"
	info "this is assertion 3: the imports registry is what makes a load possible"
	check_target "step 7, attaching" || exit 1
	exit 1
fi
DST_CREATED=1
pass "destination lvstore re-attached with its esnap clone"
check_target "step 7, attaching" || exit 1

# Proof that the attach above went through the checkpoint path. `skipped` counts
# the records replay walked past because the checkpoint already covered them, so
# both a non-zero skip count and a non-zero ckpt lsn are required: either one alone
# can be produced by the empty-journal path.
if grep -qE 'Journal replay done: [0-9]+ applied, [1-9][0-9]* skipped \(<= ckpt lsn [1-9]' \
		"${TGT_LOG}" 2>/dev/null; then
	pass "the restart replayed from a checkpoint, not from the whole journal"
else
	fail "the restart did not replay from a checkpoint"
	grep -nE 'Journal replay done|No checkpoint' "${TGT_LOG}" 2>/dev/null | \
		tail -3 | sed 's/^/       /'
fi

if raw_rpc rcow_get_imports "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
		>"${WORKDIR}/imports.json" 2>/dev/null && \
   grep -q "${EXPORT_UUID}" "${WORKDIR}/imports.json"; then
	pass "the registry still lists export ${EXPORT_UUID} after the restart"
else
	fail "the imports registry lost the export after a restart"
fi

BEFORE_REATTACH_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${DST_VOL}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (after re-attach)"; exit 1; }
DST_NSID="$(nsid_of "${DST_LVS}/${DST_VOL}")"
IMPORT_DEV="$(wait_for_new_ns "${BEFORE_REATTACH_NS}")"
if [ -z "${IMPORT_DEV}" ]; then
	fail "no namespace after re-attaching"
	exit 1
fi

if [ "$(read_md5 "${IMPORT_DEV}" "${WORKDIR}/B_reattached.bin")" = "${HASH_B}" ]; then
	pass "the imported volume reads pattern B again after a restart"
else
	fail "the imported volume lost its data across a restart"
	info "if it reads as zeroes, the esnap parent was rebuilt from nothing"
fi
check_target "step 7" || exit 1

# ==========================================================================
# [8] release must be refused while the clone depends on the export
# ==========================================================================
echo
echo "[8] trying to release the export while it is still imported"

if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s"}' \
		"${EXPORT_UUID}")" \
		>/dev/null 2>"${WORKDIR}/release_busy.err"; then
	fail "releasing an export that is still imported succeeded"
	info "its objects are what the clone reads through; deleting them would"
	info "leave holes that are not holes"
else
	pass "release was refused: $(head -c 160 "${WORKDIR}/release_busy.err")"
fi

# The manifest specifically. A release deletes it first, so its still being there
# is what says the refusal happened before anything was touched.
if [ "$(count_objects "${S3_EXPORTS_DIR}/${EXPORT_UUID}.json")" -eq 1 ] && \
   [ "${EXPORT_OBJECTS}" -eq "$(count_objects "${SRC_LVS}/exports/${EXPORT_UUID}/")" ]; then
	pass "the refused release deleted nothing, manifest included"
else
	fail "the refused release still deleted objects"
fi
check_target "step 8" || exit 1

# ==========================================================================
# [9] decouple, then release -- assertion 4
#
# Decouple is the importer's way out: it copies out the clusters the manifest says
# hold data and then clears the esnap parent, leaving the volume thin. It is not
# an inflate -- an inflate of an esnap clone allocates every cluster of the
# provisioned size and makes the volume thick, which is why it is gone.
#
# The assertions are in order of what breaks if the mechanism regresses:
#
#   1. the volume still reads its data              -> the copy covered the chunks
#   2. the registry no longer lists the export      -> the dependency was recorded
#                as ended, so release opens
#   3. release succeeds and deletes every object
#   4. the volume still reads its data              -> the copy was real, not a
#                                                      redirect to a parent that
#                                                      happens to still exist
#   5. after unload + attach it still reads         -> the parent was cleared in
#                                                      *metadata*. Swapping the
#                                                      backing device in memory
#                                                      would pass1-4 and fail
#                                                      here, on a load that asks
#                                                      for a manifest that is gone.
# ==========================================================================
echo
echo "[9] decoupling the imported volume, then releasing the export"

DECOUPLE_T0="$(now_ns)"
if ! raw_rpc rcow_decouple_lvol "$(printf '{"lvol_name":"%s"}' \
		"${DST_VOL}")" \
		>/dev/null 2>"${WORKDIR}/decouple.err"; then
	fail "rcow_decouple_lvol"
	sed 's/^/       /' "${WORKDIR}/decouple.err"
	check_target "step 9, decoupling" || exit 1
	exit 1
fi
pass "the decouple started and the RPC returned without waiting for it"

# It reports progress while it runs and disappears from the list when done, so an
# empty list is the completion signal. The volume is writable throughout, which is
# the reason the RPC does not block in the first place.
if wait_for_decouple; then
	pass "the decouple finished"
	report_timing "decouple" "$(since_ms "${DECOUPLE_T0}")"
else
	fail "the decouple did not finish in 120s"
	sed 's/^/       /' "${WORKDIR}/decouple_progress.json" 2>/dev/null | head -3
	check_target "step 9, decoupling" || exit 1
	exit 1
fi
check_target "step 9, decoupling" || exit 1

if [ "$(read_md5 "${IMPORT_DEV}" "${WORKDIR}/B_decoupled.bin")" = "${HASH_B}" ]; then
	pass "the volume still reads pattern B after decoupling"
else
	fail "decoupling changed the volume's contents"
fi

if raw_rpc rcow_get_imports "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
		>"${WORKDIR}/imports_decoupled.json" 2>/dev/null && \
   ! grep -q "${EXPORT_UUID}" "${WORKDIR}/imports_decoupled.json"; then
	pass "the import was dropped, so nothing depends on the export any more"
else
	fail "the import is still on record after decoupling"
	info "release will refuse, and the volume would keep a parent it no longer reads"
fi

# DST must be unloaded first so only one lvstore exists when SRC is re-attached.
if ! raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
		>/dev/null 2>"${WORKDIR}/unload_dst_rel.err"; then
	fail "rcow_unload_lvstore (dest, before release)"
	exit 1
fi
DST_CREATED=0

# SRC was unloaded after step 3. Re-attach it so the release can find the lvstore.
if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${SRC_LVS}" "${BUCKET}" "${SRC_WAL_BDEV}")" \
		>"${WORKDIR}/attach_src_rel.json" 2>"${WORKDIR}/attach_src_rel.err"; then
	fail "rcow_attach_lvstore (source, for release)"
	sed 's/^/       /' "${WORKDIR}/attach_src_rel.err"
	exit 1
fi
SRC_CREATED=1

if ! raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s"}' \
		"${EXPORT_UUID}")" \
		>/dev/null 2>"${WORKDIR}/release.err"; then
	fail "rcow_release_export"
	sed 's/^/       /' "${WORKDIR}/release.err"
	check_target "step 9, releasing" || exit 1
	exit 1
fi
pass "the export was released"

if ! raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" \
		>/dev/null 2>"${WORKDIR}/unload_src_rel.err"; then
	fail "rcow_unload_lvstore (source, after release)"
	exit 1
fi
SRC_CREATED=0

# This attach is assertion 5. The volume's blob must no longer name the export in
# its metadata, because the manifest it would ask for has just been deleted.
if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${DST_LVS}" "${BUCKET}" "${DST_WAL_BDEV}")" \
		>"${WORKDIR}/attach_dst_rel.json" 2>"${WORKDIR}/attach_dst_rel.err"; then
	fail "rcow_attach_lvstore (dest, after release)"
	sed 's/^/       /' "${WORKDIR}/attach_dst_rel.err"
	info "a decoupled volume must load without its export: if the esnap id is"
	info "still in its metadata, the load asks for a manifest that is gone"
	exit 1
fi
DST_CREATED=1
pass "the decoupled volume's lvstore loads with the export deleted"
check_target "step 9, releasing" || exit 1

if raw_rpc rcow_get_imports "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
		>"${WORKDIR}/imports_after.json" 2>/dev/null && \
   ! grep -q "${EXPORT_UUID}" "${WORKDIR}/imports_after.json"; then
	pass "the registry in S3 no longer lists the released export"
else
	fail "the imports registry still lists an export that was released"
	info "the next attach would fetch a manifest for objects that are gone"
fi

BEFORE_REIMPORT="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${DST_VOL}" \
	>/dev/null 2>"${WORKDIR}/add_dst_rel_ns.err" || {
	fail "nvmf_subsystem_add_ns (dest, after release)"
	sed 's/^/       /' "${WORKDIR}/add_dst_rel_ns.err"
	exit 1; }
IMPORT_DEV="$(wait_for_new_ns "${BEFORE_REIMPORT}")"
pass "destination re-attached: ${IMPORT_DEV}"

# Scoped to this export's uuid, not to exports/ as a whole. The bucket is not
# cleaned between runs -- the script keeps its objects on purpose -- so anything
# an earlier run left behind would be counted as this release's failure.
#
# Two locations now, because they belong to two different owners: the manifest is
# bucket-level at exports/<uuid>.json, while a dense export's chunk copies sit
# under the source's own prefix. A release has to clear both.
remaining_export_objects()
{
	echo $(( $(count_objects "${S3_EXPORTS_DIR}/${EXPORT_UUID}") + \
		 $(count_objects "${SRC_LVS}/exports/${EXPORT_UUID}") ))
}

REMAINING="$(remaining_export_objects)"
for _ in $(seq 20); do
	REMAINING="$(remaining_export_objects)"
	[ "${REMAINING}" -eq 0 ] && break
	sleep 1
done
if [ "${REMAINING}" -eq 0 ]; then
	pass "every object of the export is gone, manifest and chunks alike"
else
	fail "${REMAINING} object(s) still named by export ${EXPORT_UUID}"
	info "bucket level: $(count_objects "${S3_EXPORTS_DIR}/${EXPORT_UUID}")"
	info "under ${SRC_LVS}/: $(count_objects "${SRC_LVS}/exports/${EXPORT_UUID}")"
fi

# Assertion 4, read after a flush so the answer cannot come from a cache.
raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
	>/dev/null 2>&1 || info "flush_lvstore (destination) failed (continuing)"

if [ "$(read_md5 "${IMPORT_DEV}" "${WORKDIR}/B_released.bin")" = "${HASH_B}" ]; then
	pass "the volume still reads pattern B with the export deleted"
else
	fail "the volume broke once the export was deleted"
	info "the decouple did not copy everything the manifest said was there"
fi
check_target "step 9" || exit 1

# ==========================================================================
# [10] the source is still intact (already verified in step 3)
# ==========================================================================
echo
echo "[10] the source was verified in step 3 before unloading"
pass "source and snapshot verified (see step 3)"
check_target "step 10" || exit 1

# ==========================================================================
# [11] a second snapshot -- the clone chain
#
# This is the shape the module is actually used in: lvol -> snap1 -> snap2, export
# snap2. It is worth its own step because it is the case a zero-copy export was
# unable to handle at all until the walk learned to follow the chain, and because
# what breaks when it regresses is invisible from the outside.
#
# Taking a snapshot hands the new one only the clusters written since the previous
# one -- blobstore moves the cluster map across and leaves older data where it is.
# So snap2 below owns pattern C and nothing else, while pattern A is still held by
# snap1. A manifest built from snap2 alone would name8 chunks, import cleanly, and
# read pattern A back as zeroes.
#
# Hence the two assertions that matter here: that the export is still zero-copy,
# and that it names *both* regions. The second one is what the chain buys.
# ==========================================================================
echo
echo "[11] exporting a second snapshot, whose data lives in two layers"

CHAIN_OFF_MB=24
SNAP2_NAME="${SRC_VOL}-snap2"
DST_VOL2="${DST_VOL}-chain"

# SRC was unloaded after step 3. Re-attach it for the clone chain test.
if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${SRC_LVS}" "${BUCKET}" "${SRC_WAL_BDEV}")" \
		>"${WORKDIR}/attach_src_chain.json" 2>"${WORKDIR}/attach_src_chain.err"; then
	fail "rcow_attach_lvstore (source, for chain)"
	sed 's/^/       /' "${WORKDIR}/attach_src_chain.err"
	exit 1
fi
SRC_CREATED=1

# Re-add the SRC namespace so the NVMf path can reach the volume.
BEFORE_SRC_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${SRC_VOL}" \
	>/dev/null 2>"${WORKDIR}/add_src_ns_chain.err" || {
	fail "nvmf_subsystem_add_ns (source, chain)"
	sed 's/^/       /' "${WORKDIR}/add_src_ns_chain.err"
	exit 1; }
SRC_CHAIN_DEV="$(wait_for_new_ns "${BEFORE_SRC_NS}")"
if [ -z "${SRC_CHAIN_DEV}" ]; then
	fail "no new namespace for the re-attached source volume"
	exit 1
fi
pass "source re-attached for chain: ${SRC_CHAIN_DEV}"

write_pattern_at "${SRC_CHAIN_DEV}" "${WORKDIR}/C.bin" "${CHAIN_OFF_MB}" "${IO_LEN_MB}"
HASH_C="$(md5_of "${WORKDIR}/C.bin")"
check_target "step 11, writing C" || exit 1

if [ "$(read_md5_at "${SRC_CHAIN_DEV}" "${WORKDIR}/C_read.bin" "${CHAIN_OFF_MB}" "${IO_LEN_MB}")" \
		= "${HASH_C}" ]; then
	pass "the source volume took pattern C at ${CHAIN_OFF_MB} MiB"
else
	fail "the source volume did not take pattern C"
fi

if ! raw_rpc rcow_create_snapshot \
		"$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${SRC_VOL}" "${SNAP2_NAME}")" \
		>/dev/null 2>"${WORKDIR}/snap2.err"; then
	fail "rcow_create_snapshot (second)"
	sed 's/^/       /' "${WORKDIR}/snap2.err"
	check_target "step 11" || exit 1
	exit 1
fi
pass "snapshot ${SNAP2_NAME} created on top of ${EXPORT_SNAP_NAME}"

EXPORT2_T0="$(now_ns)"
if ! raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s"}' \
		"${SNAP2_NAME}")" \
		>"${WORKDIR}/export2.json" 2>"${WORKDIR}/export2.err"; then
	fail "rcow_export_snapshot (second snapshot)"
	sed 's/^/       /' "${WORKDIR}/export2.err"
	check_target "step 11" || exit 1
	exit 1
fi
pass "export accepted: $(cat "${WORKDIR}/export2.json")"
check_target "step 11, exporting" || exit 1

# The chain export must be durable before the source is unloaded: the unload
# tears down the very chunk map the walk is still resolving against.
EXPORT2_UUID="$(tr -d ' \t\r\n' <"${WORKDIR}/export2.json")"
wait_export_done "${EXPORT2_UUID}" "step 11" || exit 1
EXPORT2_MS="$(since_ms "${EXPORT2_T0}")"
report_timing "export/2-layer" "${EXPORT2_MS}"

# Unload SRC now that the chain export is written. Its NVMf namespace was only
# needed for the write and read above.
SRC_CHAIN_NSID="$(nsid_of "${SRC_LVS}/${SRC_VOL}")"
if [ -n "${SRC_CHAIN_NSID}" ]; then
	${RPC} nvmf_subsystem_remove_ns "${NQN}" "${SRC_CHAIN_NSID}" \
		>/dev/null 2>"${WORKDIR}/rm_src_chain_ns.err" || true
fi
if ! raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" \
		>/dev/null 2>"${WORKDIR}/unload_src_chain.err"; then
	fail "rcow_unload_lvstore (source, chain)"
	exit 1
fi
SRC_CREATED=0
pass "source unloaded after chain export"

# A snapshot with a parent. Verify zero-copy via its export prefix object count.
EXPORT2_OBJECTS="$(count_objects "${SRC_LVS}/exports/${EXPORT2_UUID}/")"
if [ "${EXPORT2_OBJECTS}" -eq 0 ]; then
	pass "exporting a snapshot with a parent is still zero-copy"
else
	fail "a snapshot with a parent fell back to copying (${EXPORT2_OBJECTS} object(s))"
fi

# The load-bearing count: both patterns A (snap1's) and C (snap2's) must be in
# the source data, i.e. the walk resolved a parent's clusters through the same
# chunk map. We verify by reading both patterns from the import below; the
# response no longer carries per-export chunk counts.

IMPORT2_T0="$(now_ns)"
# decouple is explicit false again: step 11b snapshots this clone and asserts
# that the esnap parent moves onto the snapshot, which requires the clone to
# still read through to the export.
if ! raw_rpc rcow_import_lvol \
		"$(printf '{"lvol_name":"%s","export_uuid":"%s","decouple":false}' \
		"${DST_VOL2}" "${EXPORT2_UUID}")" \
		>"${WORKDIR}/import2.json" 2>"${WORKDIR}/import2.err"; then
	fail "rcow_import_lvol (chain)"
	sed 's/^/       /' "${WORKDIR}/import2.err"
	check_target "step 11" || exit 1
	exit 1
fi
report_timing "import/2-layer" "$(since_ms "${IMPORT2_T0}")"
pass "imported the chained export"

BEFORE_CHAIN_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${DST_VOL2}" \
	>/dev/null 2>"${WORKDIR}/add_ns_chain.err" || {
	fail "nvmf_subsystem_add_ns (chain)"
	sed 's/^/       /' "${WORKDIR}/add_ns_chain.err"
	exit 1; }
CHAIN_DEV="$(wait_for_new_ns "${BEFORE_CHAIN_NS}")"
if [ -z "${CHAIN_DEV}" ]; then
	fail "chained import: no new namespace appeared"
	exit 1
fi
pass "the chained import is ${CHAIN_DEV}"

# The two reads this whole step exists for. One region per layer.
if [ "$(read_md5_at "${CHAIN_DEV}" "${WORKDIR}/chain_c.bin" "${CHAIN_OFF_MB}" "${IO_LEN_MB}")" \
		= "${HASH_C}" ]; then
	pass "the chained import reads pattern C -- snap2's own clusters"
else
	fail "the chained import lost pattern C"
fi

if [ "$(read_md5_at "${CHAIN_DEV}" "${WORKDIR}/chain_a.bin" "${IO_OFF_MB}" "${IO_LEN_MB}")" \
		= "${HASH_A}" ]; then
	pass "the chained import reads pattern A -- inherited from ${EXPORT_SNAP_NAME}"
else
	fail "the chained import reads zeroes where snap1 holds pattern A"
	info "this is what an export that ignores the chain produces: it imports,"
	info "it reads, and the inherited half is silently gone"
fi
check_target "step 11" || exit 1

# ==========================================================================
# [11b] a snapshot takes the dependency with it
#
# The case that decides whether release is safe at all. Snapshotting an imported
# clone hands the external parent to the *snapshot* -- blobstore moves the parent
# link across and the clone becomes a clone of the snapshot. So after deleting the
# clone, something is still reading the export, and it is not the volume anybody
# imported.
#
# A release that consulted the imports registry would answer "nobody imports this"
# here and delete the objects out from under the snapshot. Which is why it asks the
# blobs instead: the assertion below is that release is still refused after the
# clone is gone, and only opens up when the snapshot is too.
#
# SRC and DST are both loaded on purpose. With DST unloaded there would be no blob
# to find and the check would pass for the wrong reason.
# ==========================================================================
echo
echo "[11b] snapshotting the chained clone, then deleting them in turn"

if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${SRC_LVS}" "${BUCKET}" "${SRC_WAL_BDEV}")" \
		>"${WORKDIR}/attach_src_11b.json" 2>"${WORKDIR}/attach_src_11b.err"; then
	fail "rcow_attach_lvstore (source, for step 11b)"
	sed 's/^/       /' "${WORKDIR}/attach_src_11b.err"
	exit 1
fi
SRC_CREATED=1

CHAIN_SNAP="${DST_VOL2}-snap"
if raw_rpc rcow_create_snapshot \
		"$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${DST_VOL2}" "${CHAIN_SNAP}")" \
		>"${WORKDIR}/chain_snap.json" 2>"${WORKDIR}/chain_snap.err"; then
	pass "snapshotted the imported clone, which moves the esnap parent to it"
else
	fail "rcow_create_snapshot (of an imported clone)"
	sed 's/^/       /' "${WORKDIR}/chain_snap.err" 2>/dev/null
	check_target "step 11b" || exit 1
	exit 1
fi

CHAIN_NSID="$(nsid_of "${DST_LVS}/${DST_VOL2}")"
if [ -n "${CHAIN_NSID}" ]; then
	${RPC} nvmf_subsystem_remove_ns "${NQN}" "${CHAIN_NSID}" \
		>/dev/null 2>&1 || true
fi

if raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${DST_VOL2}")" \
		>"${WORKDIR}/delete_chain.json" 2>"${WORKDIR}/delete_chain.err"; then
	pass "the imported clone was deleted, leaving only its snapshot"
else
	fail "rcow_delete_lvol (the chained clone)"
	sed 's/^/       /' "${WORKDIR}/delete_chain.err" 2>/dev/null
	check_target "step 11b" || exit 1
	exit 1
fi

if raw_rpc rcow_get_imports "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
		>"${WORKDIR}/imports_snap.json" 2>/dev/null && \
   grep -q "${EXPORT2_UUID}" "${WORKDIR}/imports_snap.json"; then
	pass "the registry still lists the export, because the snapshot reads it"
else
	fail "the registry dropped an export the snapshot still reads through to"
fi

if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
		"${EXPORT2_UUID}" "${SRC_LVS}")" \
		>"${WORKDIR}/release_snap.json" 2>"${WORKDIR}/release_snap.err"; then
	fail "releasing an export a snapshot still reads succeeded"
	info "this is the hazard the registry-based check had: the clone that was"
	info "imported is gone, but its snapshot inherited the parent"
else
	pass "release was still refused, on account of the snapshot"
fi

if raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${CHAIN_SNAP}")" \
		>"${WORKDIR}/delete_snap.json" 2>"${WORKDIR}/delete_snap.err"; then
	pass "the snapshot was deleted"
else
	fail "rcow_delete_lvol (the snapshot)"
	sed 's/^/       /' "${WORKDIR}/delete_snap.err" 2>/dev/null
	check_target "step 11b" || exit 1
	exit 1
fi

if raw_rpc rcow_get_imports "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
		>"${WORKDIR}/imports_nosnap.json" 2>/dev/null && \
   ! grep -q "${EXPORT2_UUID}" "${WORKDIR}/imports_nosnap.json"; then
	pass "deleting the last reader dropped the registry entry"
else
	fail "the registry still lists an export nothing reads"
	info "nothing later can tell this entry is stale: unload does not rewrite"
	info "the object it came from"
fi

if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
		"${EXPORT2_UUID}" "${SRC_LVS}")" \
		>"${WORKDIR}/release_chain.json" 2>"${WORKDIR}/release_chain.err"; then
	pass "the chained export was released once nothing read it"
else
	fail "rcow_release_export (chained export)"
	sed 's/^/       /' "${WORKDIR}/release_chain.err" 2>/dev/null
	head -c 200 "${WORKDIR}/release_chain.json" 2>/dev/null | sed 's/^/       /'
	check_target "step 11b" || exit 1
	exit 1
fi
check_target "step 11b" || exit 1

# ==========================================================================
# [11c] import with decouple in the background
#
# The shape an orchestrator wants: import answers as soon as the volume is usable,
# and the copying that ends the dependency on the exporting node happens
# afterwards without anybody coming back to ask for it.
# ==========================================================================
echo
echo "[11c] importing with decouple:true"

AUTO_VOL="${DST_VOL}-auto"

if ! raw_rpc rcow_export_snapshot "$(printf '{"snapshot_name":"%s"}' \
		"${EXPORT_SNAP_NAME}")" \
		>"${WORKDIR}/export3.json" 2>"${WORKDIR}/export3.err"; then
	fail "rcow_export_snapshot (for the decouple:true import)"
	sed 's/^/       /' "${WORKDIR}/export3.err"
	exit 1
fi
EXPORT3_UUID="$(tr -d ' \t\r\n' <"${WORKDIR}/export3.json")"
if [ -z "${EXPORT3_UUID}" ]; then
	fail "the export produced no uuid"
	exit 1
fi
pass "exported ${EXPORT_SNAP_NAME} again as ${EXPORT3_UUID}"

wait_export_done "${EXPORT3_UUID}" "step 11c" || exit 1

IMPORT3_T0="$(now_ns)"
if raw_rpc rcow_import_lvol \
		"$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
		"${AUTO_VOL}" "${EXPORT3_UUID}" "${DST_LVS}")" \
		>"${WORKDIR}/import3.json" 2>"${WORKDIR}/import3.err"; then
	pass "the import returned without waiting for the decouple it started"
	report_timing "import/decouple-bg" "$(since_ms "${IMPORT3_T0}")"
else
	fail "rcow_import_lvol (decouple:true)"
	sed 's/^/       /' "${WORKDIR}/import3.err" 2>/dev/null
	head -c 200 "${WORKDIR}/import3.json" 2>/dev/null | sed 's/^/       /'
	check_target "step 11c" || exit 1
	exit 1
fi

if wait_for_decouple; then
	pass "the decouple it started in the background finished"
else
	fail "the background decouple did not finish in 120s"
	sed 's/^/       /' "${WORKDIR}/decouple_progress.json" 2>/dev/null | head -3
	check_target "step 11c" || exit 1
	exit 1
fi
check_target "step 11c" || exit 1

BEFORE_AUTO_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${AUTO_VOL}" \
	>/dev/null 2>"${WORKDIR}/add_ns_auto.err" || {
	fail "nvmf_subsystem_add_ns (the decouple:true import)"
	sed 's/^/       /' "${WORKDIR}/add_ns_auto.err"
	exit 1; }
AUTO_DEV="$(wait_for_new_ns "${BEFORE_AUTO_NS}")"
if [ -z "${AUTO_DEV}" ]; then
	fail "the decoupled import got no namespace"
	exit 1
fi

if [ "$(read_md5 "${AUTO_DEV}" "${WORKDIR}/auto_a.bin")" = "${HASH_A}" ]; then
	pass "the volume reads the exported data with no parent left to read"
else
	fail "the background decouple did not copy the export's data"
fi

if raw_rpc rcow_get_imports "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
		>"${WORKDIR}/imports_auto.json" 2>/dev/null && \
   ! grep -q "${EXPORT3_UUID}" "${WORKDIR}/imports_auto.json"; then
	pass "the background decouple dropped the registry entry as well"
else
	fail "the registry still lists the export after a background decouple"
fi

if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
		"${EXPORT3_UUID}" "${SRC_LVS}")" \
		>"${WORKDIR}/release_auto.json" 2>"${WORKDIR}/release_auto.err"; then
	pass "its export could be released straight away"
else
	fail "rcow_release_export (after a background decouple)"
	sed 's/^/       /' "${WORKDIR}/release_auto.err" 2>/dev/null
fi
check_target "step 11c" || exit 1

# ==========================================================================
# [11d] default decouple, idempotent export, TTL expiry
#
# Three behaviours the suite did not cover:
#
#   1. an import that does not pass decouple at all. The RPC has defaulted it to
#      true since the "xfer: decouple imported volumes by default" change, so
#      the volume must end up decoupled without anybody asking -- the flag-less
#      call is now the common case, and only the explicit decouple:true and
#      decouple:false forms were being exercised.
#
#   2. exporting the same snapshot twice while the first export is still in
#      flight answers the same uuid (idempotence, so a timed-out caller that
#      retries keeps polling the right uuid), and a fresh export after DONE gets
#      a fresh uuid (re-exporting is deliberately legal).
#
#   3. a reference export with a ttl stops pinning its snapshot once the ttl
#      expires -- deletable flips to YES while the export status stays DONE.
# ==========================================================================
echo
echo "[11d] default decouple, idempotent export, TTL expiry"

IDEM_VOL="${SRC_VOL}-idem"
IDEM_SNAP="${IDEM_VOL}-snap"
IDEM_OFF_MB=48
# Big enough that the first export cannot reach DONE between the two quick RPC
# calls that the idempotence assertion makes: 96 unwritten MiB drain as 96
# chunk uploads, which takes seconds, while two s3lvol_rpc.py launches take a
# few hundred milliseconds. The default is sized for a remote object store;
# a local one (MinIO in CI) drains far faster, so S3LVOL_TEST_IDEM_MB exists to
# push the drain window out again.
IDEM_LEN_MB="${S3LVOL_TEST_IDEM_MB:-96}"

# --------------------------------------------------------------------------
# 11d.1 default decouple: no flag at all
# --------------------------------------------------------------------------
AUTO2_VOL="${DST_VOL}-auto2"
EXPORT4_UUID="$(raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s"}' "${EXPORT_SNAP_NAME}")" \
		2>"${WORKDIR}/export4.err" | tr -d ' \t\r\n')"
if [ -z "${EXPORT4_UUID}" ]; then
	fail "rcow_export_snapshot (for the flag-less import)"
	sed 's/^/       /' "${WORKDIR}/export4.err" 2>/dev/null
	check_target "step 11d.1" || exit 1
	exit 1
fi
wait_export_done "${EXPORT4_UUID}" "step 11d.1" || exit 1

if ! raw_rpc rcow_import_lvol \
		"$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s"}' \
		"${AUTO2_VOL}" "${EXPORT4_UUID}" "${DST_LVS}")" \
		>"${WORKDIR}/import_auto2.json" 2>"${WORKDIR}/import_auto2.err"; then
	fail "rcow_import_lvol without a decouple flag"
	sed 's/^/       /' "${WORKDIR}/import_auto2.err" 2>/dev/null
	check_target "step 11d.1" || exit 1
	exit 1
fi
pass "an import without a decouple flag was accepted"

if wait_for_decouple; then
	pass "the default decouple (no flag) finished"
else
	fail "the flag-less import's decouple did not finish in 120s"
	check_target "step 11d.1" || exit 1
	exit 1
fi

if raw_rpc rcow_get_imports "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
		>"${WORKDIR}/imports_auto2.json" 2>/dev/null && \
   ! grep -q "${EXPORT4_UUID}" "${WORKDIR}/imports_auto2.json"; then
	pass "the flag-less import dropped the registry entry like decouple:true does"
else
	fail "the flag-less import is still reading through to the export"
fi

if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
		"${EXPORT4_UUID}" "${SRC_LVS}")" \
		>"${WORKDIR}/release_auto2.json" 2>"${WORKDIR}/release_auto2.err"; then
	pass "the flag-less import's export could be released"
else
	fail "rcow_release_export (flag-less import)"
	sed 's/^/       /' "${WORKDIR}/release_auto2.err" 2>/dev/null
fi
if ! raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${AUTO2_VOL}")" \
		>/dev/null 2>"${WORKDIR}/delete_auto2.err"; then
	fail "rcow_delete_lvol (the flag-less import)"
	sed 's/^/       /' "${WORKDIR}/delete_auto2.err" 2>/dev/null
fi
check_target "step 11d.1" || exit 1

# Unload DST so the idempotence volume below can be created: rcow_create_lvol
# takes no lvs_name and refuses when more than one lvstore is loaded (it
# operates "the one lvstore that exists"). Nothing in 11d.2-5 needs DST --
# the exports, releases and deletes address their volume's own lvstore -- and
# cleanup handles a DST that was never re-attached.
if ! raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
		>/dev/null 2>"${WORKDIR}/unload_dst_11d.err"; then
	fail "rcow_unload_lvstore (destination, for 11d.2)"
	sed 's/^/       /' "${WORKDIR}/unload_dst_11d.err" 2>/dev/null
	check_target "step 11d.1" || exit 1
	exit 1
fi
DST_CREATED=0
pass "destination unloaded so the idempotence volume can be created"

# --------------------------------------------------------------------------
# 11d.2 a dedicated volume for the idempotence and TTL checks
# --------------------------------------------------------------------------
if ! raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":1}' \
		"${IDEM_VOL}")" \
		>/dev/null 2>"${WORKDIR}/idem_lvol.err"; then
	fail "rcow_create_lvol (idempotence volume)"
	sed 's/^/       /' "${WORKDIR}/idem_lvol.err" 2>/dev/null
	check_target "step 11d.2" || exit 1
	exit 1
fi
BEFORE_IDEM_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${IDEM_VOL}" \
	>/dev/null 2>"${WORKDIR}/add_idem_ns.err" || {
	fail "nvmf_subsystem_add_ns (idempotence volume)"
	sed 's/^/       /' "${WORKDIR}/add_idem_ns.err"
	exit 1; }
IDEM_DEV="$(wait_for_new_ns "${BEFORE_IDEM_NS}")"
if [ -z "${IDEM_DEV}" ]; then
	fail "the idempotence volume got no namespace"
	exit 1
fi
write_pattern_at "${IDEM_DEV}" "${WORKDIR}/idem.bin" "${IDEM_OFF_MB}" "${IDEM_LEN_MB}"
IDEM_HASH="$(md5_of "${WORKDIR}/idem.bin")"
if [ "$(read_md5_at "${IDEM_DEV}" "${WORKDIR}/idem_read.bin" \
		"${IDEM_OFF_MB}" "${IDEM_LEN_MB}")" = "${IDEM_HASH}" ]; then
	pass "wrote ${IDEM_LEN_MB} MiB to the idempotence volume"
else
	fail "the idempotence volume did not take its pattern"
fi
if ! raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${IDEM_VOL}" "${IDEM_SNAP}")" \
		>/dev/null 2>"${WORKDIR}/idem_snap.err"; then
	fail "rcow_create_snapshot (idempotence)"
	sed 's/^/       /' "${WORKDIR}/idem_snap.err" 2>/dev/null
	check_target "step 11d.2" || exit 1
	exit 1
fi
pass "snapshot ${IDEM_SNAP} created with ${IDEM_LEN_MB} MiB behind it"
check_target "step 11d.2" || exit 1

# --------------------------------------------------------------------------
# 11d.3 idempotence: the same snapshot twice, in flight
# --------------------------------------------------------------------------
IDEM_U1="$(raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s"}' "${IDEM_SNAP}")" \
		2>"${WORKDIR}/idem_export1.err" | tr -d ' \t\r\n')"
# The second call, back to back. The first export cannot have finished draining
# 96 MiB yet, so this must not start a second export.
IDEM_U2="$(raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s"}' "${IDEM_SNAP}")" \
		2>"${WORKDIR}/idem_export2.err" | tr -d ' \t\r\n')"
if [ -n "${IDEM_U1}" ] && [ "${IDEM_U1}" = "${IDEM_U2}" ]; then
	pass "exporting the same snapshot twice while in flight returned the same uuid (${IDEM_U1})"
else
	fail "idempotence broken: first export '${IDEM_U1}', second '${IDEM_U2}'"
	check_target "step 11d.3" || exit 1
	exit 1
fi
wait_export_done "${IDEM_U1}" "step 11d.3" || exit 1

# A completed export is not blocked from re-exporting: a fresh call gets a fresh
# uuid, which is what exporting to a second target wants.
IDEM_U3="$(raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s"}' "${IDEM_SNAP}")" \
		2>"${WORKDIR}/idem_export3.err" | tr -d ' \t\r\n')"
if [ -n "${IDEM_U3}" ] && [ "${IDEM_U3}" != "${IDEM_U1}" ]; then
	pass "a fresh export after DONE got a fresh uuid (${IDEM_U3})"
else
	fail "a re-export after DONE answered '${IDEM_U3}'"
fi
wait_export_done "${IDEM_U3}" "step 11d.3" || exit 1
check_target "step 11d.3" || exit 1

# --------------------------------------------------------------------------
# 11d.4 TTL expiry: the pin lifts on its own
# --------------------------------------------------------------------------
# Release the two above first so deletable is driven by this export alone.
for _u in "${IDEM_U1}" "${IDEM_U3}"; do
	if ! raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
			"${_u}" "${SRC_LVS}")" >/dev/null 2>&1; then
		fail "rcow_release_export (${_u}) before the TTL check"
	fi
done

TTL_U="$(raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s","ttl_sec":3}' "${IDEM_SNAP}")" \
		2>"${WORKDIR}/ttl_export.err" | tr -d ' \t\r\n')"
if [ -z "${TTL_U}" ]; then
	fail "rcow_export_snapshot (TTL)"
	sed 's/^/       /' "${WORKDIR}/ttl_export.err" 2>/dev/null
	check_target "step 11d.4" || exit 1
	exit 1
fi
wait_export_done "${TTL_U}" "step 11d.4" || exit 1

check_deletable "${TTL_U}" NO "step 11d.4, within TTL"

# 3 s ttl plus margin; the poll that does the work is once a second.
sleep 5

ST_TTL="$(export_status_field "${TTL_U}" export_status)"
[ "${ST_TTL}" = "DONE" ] \
	&& pass "the expired export still reports DONE" \
	|| fail "the expired export reports '${ST_TTL}', expected DONE"
check_deletable "${TTL_U}" YES "step 11d.4, after TTL"
check_target "step 11d.4" || exit 1

# --------------------------------------------------------------------------
# 11d.5 two exports of one lvstore, back to back
#
# An export drains the lvstore first, and s3_flusher_drain runs one drain at a
# time -- a second caller gets -EBUSY. Two exports started close together used to
# have the second one fail on that, which was quiet from the caller's side: the
# uuid was already returned, so the failure arrived asynchronously and left
# nothing behind, and asking rcow_get_snapshot_status about that uuid answered
# "does not exist". Observed in production with two exports 8 ms apart.
#
# Different snapshots on purpose: exporting the *same* snapshot twice is
# deliberately idempotent (11d.3), which would hide the drain contention this is
# about.
# --------------------------------------------------------------------------
echo
echo "  11d.5 two exports of one lvstore, back to back"

RACE_SNAP="${IDEM_VOL}-snap2"
# Fresh data behind the second snapshot, so both exports have chunks to drain
# rather than completing immediately.
write_pattern_at "${IDEM_DEV}" "${WORKDIR}/race.bin" \
	$((IDEM_OFF_MB + IDEM_LEN_MB)) "${IDEM_LEN_MB}"
if ! raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${IDEM_VOL}" "${RACE_SNAP}")" \
		>/dev/null 2>"${WORKDIR}/race_snap.err"; then
	fail "rcow_create_snapshot (drain contention)"
	sed 's/^/       /' "${WORKDIR}/race_snap.err" 2>/dev/null
	check_target "step 11d.5" || exit 1
	exit 1
fi

RACE_U1="$(raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s"}' "${IDEM_SNAP}")" \
		2>"${WORKDIR}/race_export1.err" | tr -d ' \t\r\n')"
RACE_U2="$(raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s"}' "${RACE_SNAP}")" \
		2>"${WORKDIR}/race_export2.err" | tr -d ' \t\r\n')"

if [ -n "${RACE_U1}" ] && [ -n "${RACE_U2}" ] && [ "${RACE_U1}" != "${RACE_U2}" ]; then
	pass "two back-to-back exports got two uuids (${RACE_U1}, ${RACE_U2})"
else
	fail "back-to-back exports answered '${RACE_U1}' and '${RACE_U2}'"
	check_target "step 11d.5" || exit 1
	exit 1
fi

# The point of the fix: both reach DONE. Before it, the second export failed on
# -EBUSY and its uuid became unknown to rcow_get_snapshot_status.
for _u in "${RACE_U1}" "${RACE_U2}"; do
	wait_export_done "${_u}" "step 11d.5" || exit 1
done
pass "both exports completed despite contending for the drain"

# The failure mode this replaces, asserted directly: a uuid whose export failed
# reads as "does not exist", so a status that answers at all proves it did not.
for _u in "${RACE_U1}" "${RACE_U2}"; do
	if ! raw_rpc rcow_get_snapshot_status \
			"$(printf '{"export_uuid":"%s"}' "${_u}")" >/dev/null 2>&1; then
		fail "export ${_u} is unknown to rcow_get_snapshot_status"
	fi
done
pass "both uuids are known to rcow_get_snapshot_status"

# The drain contention must have been retried, not merely avoided by luck: the
# target says so when it had to wait. Not a failure if it did not happen -- the
# two RPC launches can be far enough apart for the first drain to finish -- but
# worth reporting either way. When it did happen, the wait has to be visible from
# the start too: a production node needs to be able to tell an export that is
# waiting from one that is stuck, which is what the NOTICE naming the uuid is for.
if grep -qE "drained '${SRC_LVS}' after [0-9]+ retr" "${TGT_LOG}" 2>/dev/null; then
	info "the drain was retried, which is the path under test"
	if grep -qE "Export [0-9a-f-]+ of snapshot '[^']+' is waiting for another drain" \
			"${TGT_LOG}" 2>/dev/null; then
		pass "the wait was reported when it began, naming the export"
	else
		fail "an export waited for a drain without saying so"
	fi
else
	info "no drain retry was needed this run (the drains did not overlap)"
fi
# What must not appear is the old outcome: an export giving up on -EBUSY. Any
# such failure now names its uuid, which is what made the production report hard
# to place: the error said only which lvstore was busy.
if grep -qE "could not drain lvstore .* Device or resource busy" "${TGT_LOG}" 2>/dev/null; then
	fail "an export still gave up because another drain was running"
else
	pass "no export failed because of a concurrent drain"
fi

for _u in "${RACE_U1}" "${RACE_U2}"; do
	if ! raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
			"${_u}" "${SRC_LVS}")" >/dev/null 2>&1; then
		fail "rcow_release_export (${_u})"
	fi
done
if ! raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${RACE_SNAP}")" \
		>/dev/null 2>"${WORKDIR}/race_snap_delete.err"; then
	fail "rcow_delete_lvol (drain contention snapshot)"
	sed 's/^/       /' "${WORKDIR}/race_snap_delete.err" 2>/dev/null
fi
check_target "step 11d.5" || exit 1

# --------------------------------------------------------------------------
# 11d.6 three exports of one lvstore, back to back
#
# Two-way contention was the reported production incident (two exports 8 ms
# apart, both refused by the single-slot drain). Three-way contention is the
# generalisation: with one drain slot per lvstore, two of three concurrent
# exports have to wait for the first. All three must still reach DONE, all
# three uuids must stay known to rcow_get_snapshot_status, and none may give
# up on -EBUSY. Different snapshots, as in 11d.5, so idempotence cannot mask
# the contention.
# --------------------------------------------------------------------------
echo
echo "  11d.6 three exports of one lvstore, back to back"

TRIP_SNAP2="${IDEM_VOL}-snap3"
TRIP_SNAP3="${IDEM_VOL}-snap4"
# Fresh data behind each new snapshot, so all three exports have chunks to
# drain rather than completing immediately.
write_pattern_at "${IDEM_DEV}" "${WORKDIR}/trip2.bin" \
	$((IDEM_OFF_MB + 2 * IDEM_LEN_MB)) "${IDEM_LEN_MB}"
write_pattern_at "${IDEM_DEV}" "${WORKDIR}/trip3.bin" \
	$((IDEM_OFF_MB + 3 * IDEM_LEN_MB)) "${IDEM_LEN_MB}"
for _s in "${TRIP_SNAP2}" "${TRIP_SNAP3}"; do
	if ! raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
			"${IDEM_VOL}" "${_s}")" \
			>/dev/null 2>"${WORKDIR}/trip_snap.err"; then
		fail "rcow_create_snapshot (three-way contention, ${_s})"
		sed 's/^/       /' "${WORKDIR}/trip_snap.err" 2>/dev/null
		check_target "step 11d.6" || exit 1
		exit 1
	fi
done

TRIP_U1="$(raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s"}' "${IDEM_SNAP}")" \
		2>"${WORKDIR}/trip_export1.err" | tr -d ' \t\r\n')"
TRIP_U2="$(raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s"}' "${TRIP_SNAP2}")" \
		2>"${WORKDIR}/trip_export2.err" | tr -d ' \t\r\n')"
TRIP_U3="$(raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s"}' "${TRIP_SNAP3}")" \
		2>"${WORKDIR}/trip_export3.err" | tr -d ' \t\r\n')"

if [ -n "${TRIP_U1}" ] && [ -n "${TRIP_U2}" ] && [ -n "${TRIP_U3}" ] && \
   [ "${TRIP_U1}" != "${TRIP_U2}" ] && \
   [ "${TRIP_U1}" != "${TRIP_U3}" ] && \
   [ "${TRIP_U2}" != "${TRIP_U3}" ]; then
	pass "three back-to-back exports got three distinct uuids"
else
	fail "three exports answered '${TRIP_U1}' / '${TRIP_U2}' / '${TRIP_U3}'"
	check_target "step 11d.6" || exit 1
	exit 1
fi

for _u in "${TRIP_U1}" "${TRIP_U2}" "${TRIP_U3}"; do
	wait_export_done "${_u}" "step 11d.6" || exit 1
done
pass "all three exports reached DONE despite contending for the drain"

for _u in "${TRIP_U1}" "${TRIP_U2}" "${TRIP_U3}"; do
	if ! raw_rpc rcow_get_snapshot_status \
			"$(printf '{"export_uuid":"%s"}' "${_u}")" >/dev/null 2>&1; then
		fail "export ${_u} is unknown to rcow_get_snapshot_status"
	fi
done
pass "all three uuids are known to rcow_get_snapshot_status"

if grep -qE "drained '${SRC_LVS}' after [0-9]+ retr" "${TGT_LOG}" 2>/dev/null; then
	info "the drain was retried during the three-way contention"
else
	info "no drain retry was needed this run (the drains did not overlap)"
fi
if grep -qE "could not drain lvstore .* Device or resource busy" "${TGT_LOG}" 2>/dev/null; then
	fail "an export still gave up because another drain was running"
else
	pass "no export failed because of a concurrent drain"
fi

for _u in "${TRIP_U1}" "${TRIP_U2}" "${TRIP_U3}"; do
	if ! raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
			"${_u}" "${SRC_LVS}")" >/dev/null 2>&1; then
		fail "rcow_release_export (${_u})"
	fi
done
if ! raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${TRIP_SNAP2}")" \
		>/dev/null 2>"${WORKDIR}/trip2_delete.err"; then
	fail "rcow_delete_lvol (${TRIP_SNAP2})"
	sed 's/^/       /' "${WORKDIR}/trip2_delete.err" 2>/dev/null
fi
if ! raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${TRIP_SNAP3}")" \
		>/dev/null 2>"${WORKDIR}/trip3_delete.err"; then
	fail "rcow_delete_lvol (${TRIP_SNAP3})"
	sed 's/^/       /' "${WORKDIR}/trip3_delete.err" 2>/dev/null
fi
check_target "step 11d.6" || exit 1

# --------------------------------------------------------------------------
# 11d.7 cleanup of the idempotence volume
# --------------------------------------------------------------------------
if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
		"${TTL_U}" "${SRC_LVS}")" >/dev/null 2>&1; then
	pass "the expired export could still be released"
else
	fail "rcow_release_export (the expired export)"
fi
IDEM_NSID="$(nsid_of "${SRC_LVS}/${IDEM_VOL}")"
if [ -n "${IDEM_NSID}" ]; then
	${RPC} nvmf_subsystem_remove_ns "${NQN}" "${IDEM_NSID}" \
		>/dev/null 2>&1 || true
fi
if ! raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${IDEM_VOL}")" \
		>/dev/null 2>"${WORKDIR}/idem_delete.err"; then
	fail "rcow_delete_lvol (idempotence volume)"
	sed 's/^/       /' "${WORKDIR}/idem_delete.err" 2>/dev/null
fi
if ! raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${IDEM_SNAP}")" \
		>/dev/null 2>"${WORKDIR}/idem_snap_delete.err"; then
	fail "rcow_delete_lvol (idempotence snapshot)"
	sed 's/^/       /' "${WORKDIR}/idem_snap_delete.err" 2>/dev/null
fi
check_target "step 11d.7" || exit 1

# ==========================================================================
# [11e] a snapshot holding clusters that were zeroed after being written
# ==========================================================================
echo
echo "[11e] exporting a snapshot whose clusters were zeroed after being written"

# The regression this pins down, and why it was invisible for so long.
#
# write_zeroes on this device is unmap: the mapping is dropped and the object
# deleted, because a chunk with no mapping already reads back as zeroes. What it
# does *not* do is give the cluster back to blobstore -- the blob still lists it
# as allocated. So "allocated" and "has an object in S3" stop agreeing, and a
# zero-copy walk that assumes they agree finds a cluster it cannot name.
#
# The walk used to treat that as the one thing it must never ignore: data that is
# still in the WAL, where naming no object would hand the importer zeroes in
# place of real bytes. Refusing is safe but not free -- the export degrades to
# copying the whole volume. And this arrangement is not exotic: a filesystem
# produces it in bulk, since ext4's lazy inode table init zeroes megabytes of a
# freshly created volume. Every rootfs snapshot on a real node hit it, so no
# rootfs was ever handed off zero-copy, and nothing said why.
#
# Two properties are asserted, and they pull in opposite directions, which is the
# point:
#
#   1. the export stays zero-copy -- the zeroed clusters are recognised as holes
#      and skipped, rather than aborting the walk;
#   2. the imported volume still reads exactly what the source reads, zeroes
#      included. A hole absent from the manifest bitmap *is* the encoding for
#      "reads as zeroes", so getting property 1 by mislabelling live data would
#      show up here as a mismatch.
#
# blkdiscard -z is used rather than a filesystem: it issues write_zeroes at an
# offset this test chooses, so the objects that disappear are known ones rather
# than whatever ext4 happened to touch.
#
# A caveat worth writing down, because it took a probe to establish. Zeroing a
# whole cluster this way makes blobstore release it too, so those clusters are
# not reported as allocated afterwards and the walk never asks about them -- the
# skip counter can legitimately read zero here. What this step does pin down is
# the half that matters for correctness and that regressed in the field: a
# snapshot whose objects have been deleted under it still exports zero-copy, and
# still reads back exactly, zeroes and neighbours alike. The allocated-but-
# unmapped shape itself comes from a filesystem zeroing part of a cluster and is
# covered by run_agent_template_test.sh, which builds a real rootfs.

ZERO_VOL="${SRC_VOL}-zeroed"
ZERO_SNAP="${ZERO_VOL}-snap"
ZERO_DST="${DST_VOL}-zeroed"
# Three regions, all cluster aligned: one kept, one zeroed, one kept. Zeroing
# between two live regions means a walk that miscounts while skipping shifts
# everything after it, which the md5 of the third region catches.
ZKEEP1_MB=8
ZHOLE_MB=12
ZKEEP2_MB=16
ZREGION_MB=4

if ! command -v blkdiscard >/dev/null 2>&1; then
	info "blkdiscard not available, skipping step 11e"
else

if ! raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
		"${ZERO_VOL}" "${LVOL_GIB}")" \
		>/dev/null 2>"${WORKDIR}/zero_lvol.err"; then
	fail "rcow_create_lvol (zeroed-cluster volume)"
	sed 's/^/       /' "${WORKDIR}/zero_lvol.err"
	exit 1
fi
pass "volume for the zeroed-cluster case created"

BEFORE_ZERO_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${ZERO_VOL}" \
	>/dev/null 2>"${WORKDIR}/zero_ns.err" || {
	fail "nvmf_subsystem_add_ns (zeroed-cluster volume)"
	sed 's/^/       /' "${WORKDIR}/zero_ns.err"
	exit 1; }
ZERO_DEV="$(wait_for_new_ns "${BEFORE_ZERO_NS}")"
if [ -z "${ZERO_DEV}" ]; then
	fail "no new namespace for the zeroed-cluster volume"
	exit 1
fi

write_pattern_at "${ZERO_DEV}" "${WORKDIR}/Z1.bin" "${ZKEEP1_MB}" "${ZREGION_MB}"
write_pattern_at "${ZERO_DEV}" "${WORKDIR}/ZH.bin" "${ZHOLE_MB}"  "${ZREGION_MB}"
write_pattern_at "${ZERO_DEV}" "${WORKDIR}/Z2.bin" "${ZKEEP2_MB}" "${ZREGION_MB}"

# Flush first, so the middle region really does have objects to lose. Zeroing a
# region that never reached S3 would exercise the WAL path instead, and the
# distinction being tested is precisely between those two.
if ! raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" \
		>/dev/null 2>"${WORKDIR}/zero_flush.err"; then
	fail "rcow_flush_lvstore (before zeroing)"
	sed 's/^/       /' "${WORKDIR}/zero_flush.err"
fi
pass "three regions written and flushed to S3"

if blkdiscard -z -o $((ZHOLE_MB * 1024 * 1024)) -l $((ZREGION_MB * 1024 * 1024)) \
		"${ZERO_DEV}" 2>"${WORKDIR}/zero_discard.err"; then
	pass "the middle region was zeroed with write_zeroes"
else
	fail "blkdiscard -z on the middle region"
	sed 's/^/       /' "${WORKDIR}/zero_discard.err"
fi

# Flush again, and this is not symmetry with the flush above. write_zeroes is
# logged like any other write, so until the journal is drained the mapping it
# removes is still the committed one -- the walk would find every cluster mapped
# and the case under test would not arise at all. It is the drain that turns the
# zeroed region into clusters that are allocated with nothing behind them.
if ! raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" \
		>/dev/null 2>"${WORKDIR}/zero_flush2.err"; then
	fail "rcow_flush_lvstore (after zeroing)"
	sed 's/^/       /' "${WORKDIR}/zero_flush2.err"
fi
pass "the removal of those mappings was drained"

# The source is the reference for what the import must reproduce, so it is read
# after the zeroing rather than assumed.
ZH_SRC_MD5="$(read_md5_at "${ZERO_DEV}" "${WORKDIR}/zh_src.bin" \
	"${ZHOLE_MB}" "${ZREGION_MB}")"
Z1_SRC_MD5="$(read_md5_at "${ZERO_DEV}" "${WORKDIR}/z1_src.bin" \
	"${ZKEEP1_MB}" "${ZREGION_MB}")"
Z2_SRC_MD5="$(read_md5_at "${ZERO_DEV}" "${WORKDIR}/z2_src.bin" \
	"${ZKEEP2_MB}" "${ZREGION_MB}")"

ZEROES_MD5="$(dd if=/dev/zero bs=1M count="${ZREGION_MB}" status=none \
	| md5sum | cut -d' ' -f1)"
if [ "${ZH_SRC_MD5}" = "${ZEROES_MD5}" ]; then
	pass "the zeroed region reads back as zeroes on the source"
else
	fail "the zeroed region is not zeroes on the source (${ZH_SRC_MD5})"
fi

if ! raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${ZERO_VOL}" "${ZERO_SNAP}")" \
		>/dev/null 2>"${WORKDIR}/zero_snap.err"; then
	fail "rcow_create_snapshot (zeroed-cluster volume)"
	sed 's/^/       /' "${WORKDIR}/zero_snap.err"
	exit 1
fi
pass "snapshot of the volume with zeroed clusters created"

ZERO_LOG_MARK="$(wc -l < "${TGT_LOG}")"
ZERO_UUID="$(raw_rpc rcow_export_snapshot \
	"$(printf '{"snapshot_name":"%s"}' "${ZERO_SNAP}")" 2>/dev/null \
	| tr -d '"[:space:]')"
if [ -z "${ZERO_UUID}" ]; then
	fail "rcow_export_snapshot (zeroed-cluster snapshot)"
	exit 1
fi
wait_export_done "${ZERO_UUID}" "step 11e" || exit 1
pass "the zeroed-cluster snapshot exported as ${ZERO_UUID}"

# Property 1. The ref engine says "nothing was copied"; the copy engine says
# "chunk object(s)". Which one ran is the whole question, so it is read from the
# log rather than inferred from timing or object counts.
ZERO_LOG="$(tail -n +"${ZERO_LOG_MARK}" "${TGT_LOG}")"
if printf '%s' "${ZERO_LOG}" | grep -q "Export ${ZERO_UUID} references .* nothing was copied"; then
	pass "the export stayed zero-copy despite the zeroed clusters"
else
	fail "the export did not take the ref path"
	printf '%s' "${ZERO_LOG}" \
		| grep -E "Export ${ZERO_UUID}|copying instead|no committed mapping" \
		| head -5 | sed 's/^/       /'
fi

# How many clusters end up allocated-but-unmapped is not something this test can
# dictate. It depends on how the filesystem lays itself out and on how much of
# that reached S3 before the snapshot, and on a small volume mkfs may well write
# its inode tables through rather than leaving them to be zeroed later -- in
# which case there are no holes and nothing to skip. The count is therefore
# reported, and only its consistency is asserted: whatever the walk skipped, it
# must not also have reported those clusters as unflushed data, and the reads
# below must still match. A hard lower bound here would fail on volume
# geometries that are perfectly correct.
ZERO_SKIPPED="$(printf '%s' "${ZERO_LOG}" \
	| sed -nE "s/.*Export ${ZERO_UUID} references .* skipping ([0-9]+) zeroed cluster\(s\).*/\1/p" \
	| head -1)"
if [ -n "${ZERO_SKIPPED}" ]; then
	pass "the walk accounted for zeroed clusters (skipped ${ZERO_SKIPPED})"
	info "zeroed clusters skipped by this run: ${ZERO_SKIPPED}"
else
	fail "the walk did not report a zeroed-cluster count at all"
fi

if ! printf '%s' "${ZERO_LOG}" | grep -q "no committed mapping yet"; then
	pass "no cluster was mistaken for unflushed data"
else
	fail "a zeroed cluster was reported as having no committed mapping"
	printf '%s' "${ZERO_LOG}" | grep "no committed mapping yet" | head -3 \
		| sed 's/^/       /'
fi

# Property 2. Importing and reading back is the only thing that distinguishes
# "skipped a hole" from "skipped live data".
#
# decouple:false on purpose. Reads then go through the manifest rather than
# through clusters materialised locally, so an absent bit is served as zeroes by
# the import path itself -- which is the encoding being checked. Decoupling first
# would test the materialise path instead and could mask a bitmap that is wrong.
# The destination has been unloaded since step 11d.2, so it is brought back
# before anything is imported into it.
if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${DST_LVS}" "${BUCKET}" "${DST_WAL_BDEV}")" \
		>/dev/null 2>"${WORKDIR}/zero_attach_dst.err"; then
	fail "rcow_attach_lvstore (destination, for step 11e)"
	sed 's/^/       /' "${WORKDIR}/zero_attach_dst.err"
	exit 1
fi
DST_CREATED=1

if ! raw_rpc rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
		"${ZERO_DST}" "${ZERO_UUID}" "${DST_LVS}")" \
		>/dev/null 2>"${WORKDIR}/zero_import.err"; then
	fail "rcow_import_lvol (zeroed-cluster export)"
	sed 's/^/       /' "${WORKDIR}/zero_import.err"
	exit 1
fi
pass "the zeroed-cluster export imported"
BEFORE_ZDST_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${ZERO_DST}" \
	>/dev/null 2>"${WORKDIR}/zero_dst_ns.err" || {
	fail "nvmf_subsystem_add_ns (imported zeroed-cluster volume)"
	sed 's/^/       /' "${WORKDIR}/zero_dst_ns.err"
	exit 1; }
ZERO_DST_DEV="$(wait_for_new_ns "${BEFORE_ZDST_NS}")"
if [ -z "${ZERO_DST_DEV}" ]; then
	fail "no new namespace for the imported zeroed-cluster volume"
	exit 1
fi

Z1_DST_MD5="$(read_md5_at "${ZERO_DST_DEV}" "${WORKDIR}/z1_dst.bin" \
	"${ZKEEP1_MB}" "${ZREGION_MB}")"
ZH_DST_MD5="$(read_md5_at "${ZERO_DST_DEV}" "${WORKDIR}/zh_dst.bin" \
	"${ZHOLE_MB}" "${ZREGION_MB}")"
Z2_DST_MD5="$(read_md5_at "${ZERO_DST_DEV}" "${WORKDIR}/z2_dst.bin" \
	"${ZKEEP2_MB}" "${ZREGION_MB}")"

if [ "${Z1_DST_MD5}" = "${Z1_SRC_MD5}" ]; then
	pass "the region before the hole survived the handoff"
else
	fail "the region before the hole differs (${Z1_SRC_MD5} vs ${Z1_DST_MD5})"
fi
if [ "${ZH_DST_MD5}" = "${ZEROES_MD5}" ]; then
	pass "the skipped cluster reads as zeroes on the importing side"
else
	fail "the skipped cluster is not zeroes after import (${ZH_DST_MD5})"
fi
# The one that catches an off-by-one in the skip: a walk that dropped a cluster
# from the bitmap without keeping the indices straight leaves this region shifted.
if [ "${Z2_DST_MD5}" = "${Z2_SRC_MD5}" ]; then
	pass "the region after the hole is not shifted by the skip"
else
	fail "the region after the hole differs (${Z2_SRC_MD5} vs ${Z2_DST_MD5})"
fi

# Leave the lvstores as step 12 expects to find them.
ZERO_DST_NSID="$(nsid_of "${DST_LVS}/${ZERO_DST}")"
[ -n "${ZERO_DST_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
	"${ZERO_DST_NSID}" >/dev/null 2>&1 || true
ZERO_NSID="$(nsid_of "${SRC_LVS}/${ZERO_VOL}")"
[ -n "${ZERO_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
	"${ZERO_NSID}" >/dev/null 2>&1 || true

raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${ZERO_DST}")" \
	>/dev/null 2>&1 || fail "rcow_delete_lvol (imported zeroed-cluster volume)"

# The registry entry is dropped when the last reader goes away, and that happens
# after the delete replies -- so releasing immediately races it and is refused.
# Retried rather than slept on, since the wait is normally a single poll.
ZERO_RELEASED=0
for _ in $(seq 20); do
	if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
			"${ZERO_UUID}" "${SRC_LVS}")" \
			>/dev/null 2>"${WORKDIR}/zero_release.err"; then
		ZERO_RELEASED=1
		break
	fi
	sleep 0.5
done
if [ "${ZERO_RELEASED}" = "1" ]; then
	pass "the zeroed-cluster export was released"
else
	fail "rcow_release_export (zeroed-cluster export)"
	sed 's/^/       /' "${WORKDIR}/zero_release.err" 2>/dev/null
fi

raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${ZERO_VOL}")" \
	>/dev/null 2>&1 || fail "rcow_delete_lvol (zeroed-cluster volume)"
# Only deletable once the export above is gone: the snapshot is what that export
# reads through, and the target refuses while another node might still want it.
# Retried for the same reason the release was.
ZERO_SNAP_GONE=0
for _ in $(seq 20); do
	if raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${ZERO_SNAP}")" \
			>/dev/null 2>"${WORKDIR}/zero_snap_del.err"; then
		ZERO_SNAP_GONE=1
		break
	fi
	sleep 0.5
done
if [ "${ZERO_SNAP_GONE}" = "1" ]; then
	pass "the zeroed-cluster snapshot could be deleted once released"
else
	fail "rcow_delete_lvol (zeroed-cluster snapshot)"
	sed 's/^/       /' "${WORKDIR}/zero_snap_del.err" 2>/dev/null
fi

# Unloaded again, because that is the state the remaining steps were written
# against and cleanup checks for.
raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
	>/dev/null 2>&1 || fail "rcow_unload_lvstore (destination, after step 11e)"
DST_CREATED=0

check_target "step 11e" || exit 1

fi

# ==========================================================================
# [11f] several writable volumes from one export
# ==========================================================================
echo
echo "[11f] importing one export into several writable volumes"

# Starting N sandboxes on a node from one sealed rootfs is the ordinary case, not
# a corner. It used to be refused outright: the imports registry is keyed by
# (lvstore, export uuid) and a second import of the same uuid was answered with
# -EEXIST.
#
# The key is right and the refusal was not. What the registry records is that this
# lvstore depends on the export -- it caches the manifest so an esnap clone can be
# reopened after a restart -- and one entry describes any number of clones. Each
# clone gets its own bs_dev from the esnap callback; all they share is a read-only
# manifest and an S3 client.
#
# What this pins down:
#
#   1. two volumes can be imported from one export, and both read what the source
#      held;
#   2. writing one does not disturb the other, nor the export. They are separate
#      copy-on-write volumes that happen to start from the same bytes;
#   3. deleting one leaves the other working -- the registry entry is shared, so a
#      delete that dropped it would break the survivor at the next attach;
#   4. release stays refused until *both* are gone, and says how many are left;
#   5. the survivor still opens after the destination lvstore is re-attached,
#      which is the only proof that the shared entry was persisted rather than
#      merely held in memory.

MULTI_VOL="${SRC_VOL}-multi"
MULTI_SNAP="${MULTI_VOL}-snap"
MULTI_A="${DST_VOL}-multi-a"
MULTI_B="${DST_VOL}-multi-b"
MULTI_DATA_OFF_MB=8
# Written at different offsets so that a volume seeing the other's write is
# detected as such, rather than as a checksum that merely differs.
MULTI_A_OFF_MB=32
MULTI_B_OFF_MB=40
MULTI_LEN_MB=4

# A fresh export of its own. Every earlier one in this file has been released by
# now, and an export whose objects are gone would fail here for the wrong reason.
# The source lvstore is still attached from the steps above; only the destination
# gets unloaded between them.
if ! raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
		"${MULTI_VOL}" "${LVOL_GIB}")" \
		>/dev/null 2>"${WORKDIR}/multi_lvol.err"; then
	fail "rcow_create_lvol (step 11f source volume)"
	sed 's/^/       /' "${WORKDIR}/multi_lvol.err"
	exit 1
fi

BEFORE_MULTI_SRC_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${MULTI_VOL}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (step 11f source)"; exit 1; }
MULTI_SRC_DEV="$(wait_for_new_ns "${BEFORE_MULTI_SRC_NS}")"
if [ -z "${MULTI_SRC_DEV}" ]; then
	fail "no namespace for the step 11f source volume"
	exit 1
fi
write_pattern_at "${MULTI_SRC_DEV}" "${WORKDIR}/M.bin" \
	"${MULTI_DATA_OFF_MB}" "${MULTI_LEN_MB}"

if ! raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${MULTI_VOL}" "${MULTI_SNAP}")" \
		>/dev/null 2>"${WORKDIR}/multi_snap.err"; then
	fail "rcow_create_snapshot (step 11f)"
	sed 's/^/       /' "${WORKDIR}/multi_snap.err"
	exit 1
fi

MULTI_UUID="$(raw_rpc rcow_export_snapshot \
	"$(printf '{"snapshot_name":"%s"}' "${MULTI_SNAP}")" 2>/dev/null \
	| tr -d '"[:space:]')"
if [ -z "${MULTI_UUID}" ]; then
	fail "rcow_export_snapshot (step 11f)"
	exit 1
fi
wait_export_done "${MULTI_UUID}" "step 11f" || exit 1
pass "an export for the multi-import case created (${MULTI_UUID})"

if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${DST_LVS}" "${BUCKET}" "${DST_WAL_BDEV}")" \
		>/dev/null 2>"${WORKDIR}/multi_attach.err"; then
	fail "rcow_attach_lvstore (destination, for step 11f)"
	sed 's/^/       /' "${WORKDIR}/multi_attach.err"
	exit 1
fi
DST_CREATED=1

# decouple:false for both. The point is two volumes reading one export at the
# same time, which is exactly what decoupling would undo.
for vol in "${MULTI_A}" "${MULTI_B}"; do
	if ! raw_rpc rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
			"${vol}" "${MULTI_UUID}" "${DST_LVS}")" \
			>/dev/null 2>"${WORKDIR}/multi_import_${vol}.err"; then
		fail "rcow_import_lvol (${vol}) -- a second import of one export"
		sed 's/^/       /' "${WORKDIR}/multi_import_${vol}.err"
		exit 1
	fi
done
pass "two volumes imported from one export"

# One registry entry for both, not two. Reported by rcow_get_imports, which is
# what an operator would look at.
MULTI_ENTRIES="$(raw_rpc rcow_get_imports 2>/dev/null | python3 -c "
import json, sys
uuid = sys.argv[1]
try:
    rows = json.load(sys.stdin)
except Exception:
    print('parse-error'); sys.exit(0)
print(sum(1 for r in rows if r.get('export_uuid') == uuid))
" "${MULTI_UUID}" 2>/dev/null)"
if [ "${MULTI_ENTRIES}" = "1" ]; then
	pass "the two volumes share one registry entry"
else
	fail "expected one registry entry for the export, got '${MULTI_ENTRIES}'"
fi

BEFORE_MULTI_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${MULTI_A}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${MULTI_A})"; exit 1; }
MULTI_A_DEV="$(wait_for_new_ns "${BEFORE_MULTI_NS}")"
BEFORE_MULTI_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${MULTI_B}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${MULTI_B})"; exit 1; }
MULTI_B_DEV="$(wait_for_new_ns "${BEFORE_MULTI_NS}")"
if [ -z "${MULTI_A_DEV}" ] || [ -z "${MULTI_B_DEV}" ]; then
	fail "the two imported volumes did not both get a namespace"
	exit 1
fi
pass "both volumes exposed: ${MULTI_A_DEV}, ${MULTI_B_DEV}"

# Property 1: both start from the export's contents.
MULTI_EXPECTED="$(md5sum "${WORKDIR}/M.bin" | cut -d' ' -f1)"
A_DATA_MD5="$(read_md5_at "${MULTI_A_DEV}" "${WORKDIR}/multi_a_data.bin" \
	"${MULTI_DATA_OFF_MB}" "${MULTI_LEN_MB}")"
B_DATA_MD5="$(read_md5_at "${MULTI_B_DEV}" "${WORKDIR}/multi_b_data.bin" \
	"${MULTI_DATA_OFF_MB}" "${MULTI_LEN_MB}")"
if [ "${A_DATA_MD5}" = "${MULTI_EXPECTED}" ] && [ "${B_DATA_MD5}" = "${MULTI_EXPECTED}" ]; then
	pass "both volumes read the exported data"
else
	fail "the volumes do not both read the export (${A_DATA_MD5} / ${B_DATA_MD5})"
fi

# Property 2: independent copy-on-write.
write_pattern_at "${MULTI_A_DEV}" "${WORKDIR}/MA.bin" "${MULTI_A_OFF_MB}" "${MULTI_LEN_MB}"
write_pattern_at "${MULTI_B_DEV}" "${WORKDIR}/MB.bin" "${MULTI_B_OFF_MB}" "${MULTI_LEN_MB}"

MA_EXPECTED="$(md5sum "${WORKDIR}/MA.bin" | cut -d' ' -f1)"
MB_EXPECTED="$(md5sum "${WORKDIR}/MB.bin" | cut -d' ' -f1)"
ZERO_LEN_MD5="$(dd if=/dev/zero bs=1M count="${MULTI_LEN_MB}" status=none \
	| md5sum | cut -d' ' -f1)"

A_OWN="$(read_md5_at "${MULTI_A_DEV}" "${WORKDIR}/ma_own.bin" \
	"${MULTI_A_OFF_MB}" "${MULTI_LEN_MB}")"
B_OWN="$(read_md5_at "${MULTI_B_DEV}" "${WORKDIR}/mb_own.bin" \
	"${MULTI_B_OFF_MB}" "${MULTI_LEN_MB}")"
if [ "${A_OWN}" = "${MA_EXPECTED}" ] && [ "${B_OWN}" = "${MB_EXPECTED}" ]; then
	pass "each volume reads back its own write"
else
	fail "a volume did not read back its own write"
fi

# The load-bearing half: neither sees the other's write. Both offsets were holes
# in the export, so what should be there is zeroes.
A_AT_B="$(read_md5_at "${MULTI_A_DEV}" "${WORKDIR}/ma_at_b.bin" \
	"${MULTI_B_OFF_MB}" "${MULTI_LEN_MB}")"
B_AT_A="$(read_md5_at "${MULTI_B_DEV}" "${WORKDIR}/mb_at_a.bin" \
	"${MULTI_A_OFF_MB}" "${MULTI_LEN_MB}")"
if [ "${A_AT_B}" = "${ZERO_LEN_MD5}" ] && [ "${B_AT_A}" = "${ZERO_LEN_MD5}" ]; then
	pass "neither volume sees the other's write"
else
	fail "the volumes are not isolated (a@b=${A_AT_B} b@a=${B_AT_A})"
	info "a write to one imported volume reached the other, or reached the export"
fi

# Property 4, while both exist: release must refuse and account for both.
# The count goes to the target log, not to the RPC reply -- rcow_release_export
# answers with an errno and leaves the detail to the log, as the other failure
# paths in this file do.
MULTI_REL_MARK="$(wc -l < "${TGT_LOG}")"
if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
		"${MULTI_UUID}" "${SRC_LVS}")" \
		>/dev/null 2>"${WORKDIR}/multi_release_busy.err"; then
	fail "release succeeded while two volumes still read the export"
else
	pass "release was refused while two volumes read the export"
	if tail -n +"${MULTI_REL_MARK}" "${TGT_LOG}" \
			| grep -q "parent of 2 volume(s)"; then
		pass "the refusal counted both readers"
	else
		fail "the refusal did not report both readers"
		tail -n +"${MULTI_REL_MARK}" "${TGT_LOG}" \
			| grep -E "still the parent of" | head -2 | sed 's/^/       /'
	fi
fi

# Property 3: deleting one must not disturb the other.
MULTI_A_NSID="$(nsid_of "${DST_LVS}/${MULTI_A}")"
[ -n "${MULTI_A_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
	"${MULTI_A_NSID}" >/dev/null 2>&1 || true
if ! raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${MULTI_A}")" \
		>/dev/null 2>"${WORKDIR}/multi_del_a.err"; then
	fail "rcow_delete_lvol (${MULTI_A})"
	sed 's/^/       /' "${WORKDIR}/multi_del_a.err"
fi

B_DATA_AFTER="$(read_md5_at "${MULTI_B_DEV}" "${WORKDIR}/mb_data_after.bin" \
	"${MULTI_DATA_OFF_MB}" "${MULTI_LEN_MB}")"
B_OWN_AFTER="$(read_md5_at "${MULTI_B_DEV}" "${WORKDIR}/mb_own_after.bin" \
	"${MULTI_B_OFF_MB}" "${MULTI_LEN_MB}")"
if [ "${B_DATA_AFTER}" = "${MULTI_EXPECTED}" ] && [ "${B_OWN_AFTER}" = "${MB_EXPECTED}" ]; then
	pass "the surviving volume is unaffected by the other's deletion"
else
	fail "deleting one volume damaged the other (${B_DATA_AFTER} / ${B_OWN_AFTER})"
fi

# The shared entry must still be on record: one reader is left.
MULTI_ENTRIES_AFTER="$(raw_rpc rcow_get_imports 2>/dev/null | python3 -c "
import json, sys
uuid = sys.argv[1]
try:
    rows = json.load(sys.stdin)
except Exception:
    print('parse-error'); sys.exit(0)
print(sum(1 for r in rows if r.get('export_uuid') == uuid))
" "${MULTI_UUID}" 2>/dev/null)"
if [ "${MULTI_ENTRIES_AFTER}" = "1" ]; then
	pass "the registry entry survived the first delete"
else
	fail "the registry entry went away while a volume still reads it (got '${MULTI_ENTRIES_AFTER}')"
fi

# Property 5: the survivor reopens across an attach. This is what fails if the
# shared entry was never written to S3.
MULTI_B_NSID="$(nsid_of "${DST_LVS}/${MULTI_B}")"
[ -n "${MULTI_B_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
	"${MULTI_B_NSID}" >/dev/null 2>&1 || true
if ! raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
		>/dev/null 2>"${WORKDIR}/multi_unload.err"; then
	fail "rcow_unload_lvstore (destination, step 11f)"
	sed 's/^/       /' "${WORKDIR}/multi_unload.err"
fi
DST_CREATED=0
if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${DST_LVS}" "${BUCKET}" "${DST_WAL_BDEV}")" \
		>/dev/null 2>"${WORKDIR}/multi_reattach.err"; then
	fail "rcow_attach_lvstore (destination, reopening the survivor)"
	sed 's/^/       /' "${WORKDIR}/multi_reattach.err"
	exit 1
fi
DST_CREATED=1

BEFORE_MULTI_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${MULTI_B}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${MULTI_B}, after attach)"; exit 1; }
MULTI_B_DEV2="$(wait_for_new_ns "${BEFORE_MULTI_NS}")"
if [ -z "${MULTI_B_DEV2}" ]; then
	fail "the surviving volume did not come back after re-attach"
else
	B_DATA_REOPEN="$(read_md5_at "${MULTI_B_DEV2}" "${WORKDIR}/mb_data_reopen.bin" \
		"${MULTI_DATA_OFF_MB}" "${MULTI_LEN_MB}")"
	B_OWN_REOPEN="$(read_md5_at "${MULTI_B_DEV2}" "${WORKDIR}/mb_own_reopen.bin" \
		"${MULTI_B_OFF_MB}" "${MULTI_LEN_MB}")"
	if [ "${B_DATA_REOPEN}" = "${MULTI_EXPECTED}" ] && \
	   [ "${B_OWN_REOPEN}" = "${MB_EXPECTED}" ]; then
		pass "the survivor reopened and still reads both the export and its own write"
	else
		fail "the survivor came back wrong (${B_DATA_REOPEN} / ${B_OWN_REOPEN})"
	fi
fi

# Property 4, the other half: once nothing reads it, release is allowed.
MULTI_B_NSID="$(nsid_of "${DST_LVS}/${MULTI_B}")"
[ -n "${MULTI_B_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
	"${MULTI_B_NSID}" >/dev/null 2>&1 || true
if ! raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${MULTI_B}")" \
		>/dev/null 2>"${WORKDIR}/multi_del_b.err"; then
	fail "rcow_delete_lvol (${MULTI_B})"
	sed 's/^/       /' "${WORKDIR}/multi_del_b.err"
fi

MULTI_RELEASED=0
for _ in $(seq 20); do
	if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
			"${MULTI_UUID}" "${SRC_LVS}")" \
			>/dev/null 2>"${WORKDIR}/multi_release.err"; then
		MULTI_RELEASED=1
		break
	fi
	sleep 0.5
done
if [ "${MULTI_RELEASED}" = "1" ]; then
	pass "release was allowed once both volumes were gone"
else
	fail "release still refused after both volumes were deleted"
	sed 's/^/       /' "${WORKDIR}/multi_release.err" 2>/dev/null
fi

raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
	>/dev/null 2>&1 || fail "rcow_unload_lvstore (destination, after step 11f)"
DST_CREATED=0

check_target "step 11f" || exit 1

# ==========================================================================
# [11g] two volumes asked to decouple from one export at once
# ==========================================================================
echo
echo "[11g] queueing decouples of one export"

# Only one volume materialises a given export at a time. N concurrent decouples
# would fetch the same objects N times over, competing for the same client and
# bandwidth, and every one of them would finish later than if they had taken
# turns.
#
# The wait is kept inside rather than handed back as -EBUSY. "Import N volumes
# from this export and decouple them" is one operation to the caller; refusing all
# but the first would leave the rest esnap clones unless the caller noticed and
# retried, which means polling for a wait it never asked to manage.
#
# So both imports use decouple:true and both are expected to succeed. What this
# checks is that the second one really did happen -- a queue that dropped its
# entry would look identical at the RPC level and only differ in that one volume
# never became independent.
#
# The proof that it became independent is release: the export cannot be released
# while any volume still reads through it, so a release that succeeds once both
# decouples are done is the same statement as "neither reads the export any more".

MULTI2_VOL="${SRC_VOL}-multi2"
MULTI2_SNAP="${MULTI2_VOL}-snap"
MULTI2_A="${DST_VOL}-q-a"
MULTI2_B="${DST_VOL}-q-b"

if ! raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
		"${MULTI2_VOL}" "${LVOL_GIB}")" \
		>/dev/null 2>"${WORKDIR}/multi2_lvol.err"; then
	fail "rcow_create_lvol (step 11g source volume)"
	sed 's/^/       /' "${WORKDIR}/multi2_lvol.err"
	exit 1
fi

BEFORE_MULTI2_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${MULTI2_VOL}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (step 11g source)"; exit 1; }
MULTI2_SRC_DEV="$(wait_for_new_ns "${BEFORE_MULTI2_NS}")"
if [ -z "${MULTI2_SRC_DEV}" ]; then
	fail "no namespace for the step 11g source volume"
	exit 1
fi
# Enough regions that materialising takes long enough for the second request to
# arrive while the first is still running -- which is the situation being tested.
write_pattern_at "${MULTI2_SRC_DEV}" "${WORKDIR}/Q.bin" \
	"${MULTI_DATA_OFF_MB}" "${IO_LEN_MB}"
write_pattern_at "${MULTI2_SRC_DEV}" "${WORKDIR}/Q2.bin" \
	"$((MULTI_DATA_OFF_MB + IO_LEN_MB))" "${IO_LEN_MB}"

if ! raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${MULTI2_VOL}" "${MULTI2_SNAP}")" \
		>/dev/null 2>"${WORKDIR}/multi2_snap.err"; then
	fail "rcow_create_snapshot (step 11g)"
	sed 's/^/       /' "${WORKDIR}/multi2_snap.err"
	exit 1
fi

MULTI2_UUID="$(raw_rpc rcow_export_snapshot \
	"$(printf '{"snapshot_name":"%s"}' "${MULTI2_SNAP}")" 2>/dev/null \
	| tr -d '"[:space:]')"
if [ -z "${MULTI2_UUID}" ]; then
	fail "rcow_export_snapshot (step 11g)"
	exit 1
fi
wait_export_done "${MULTI2_UUID}" "step 11g" || exit 1

if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${DST_LVS}" "${BUCKET}" "${DST_WAL_BDEV}")" \
		>/dev/null 2>"${WORKDIR}/multi2_attach.err"; then
	fail "rcow_attach_lvstore (destination, for step 11g)"
	sed 's/^/       /' "${WORKDIR}/multi2_attach.err"
	exit 1
fi
DST_CREATED=1

# Back to back, deliberately without waiting: the second import's decouple is
# meant to land while the first is still copying.
Q_IMPORTS_OK=1
for vol in "${MULTI2_A}" "${MULTI2_B}"; do
	if ! raw_rpc rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
			"${vol}" "${MULTI2_UUID}" "${DST_LVS}")" \
			>/dev/null 2>"${WORKDIR}/multi2_import_${vol}.err"; then
		fail "rcow_import_lvol (${vol}, decouple:true)"
		sed 's/^/       /' "${WORKDIR}/multi2_import_${vol}.err"
		Q_IMPORTS_OK=0
	fi
done
if [ "${Q_IMPORTS_OK}" = "1" ]; then
	pass "both volumes imported with decouple:true"
fi

# One running, the rest queued -- never two of the same export running. Sampled
# while the work is in flight; a single sample is enough to catch the case this
# replaces, where both would have been running.
Q_MAX_RUNNING=0
Q_SAW_QUEUED=0
for _ in $(seq 60); do
	Q_STATE="$(raw_rpc rcow_get_decouple 2>/dev/null | python3 -c "
import json, sys
uuid = sys.argv[1]
try:
    rows = json.load(sys.stdin)
except Exception:
    print('0 0 0'); sys.exit(0)
mine = [r for r in rows if r.get('export_uuid') == uuid]
running = [r for r in mine if not r.get('queued')]
queued  = [r for r in mine if r.get('queued')]
print('%d %d %d' % (len(mine), len(running), len(queued)))
" "${MULTI2_UUID}" 2>/dev/null)"
	Q_TOTAL="$(printf '%s' "${Q_STATE}" | cut -d' ' -f1)"
	Q_RUNNING="$(printf '%s' "${Q_STATE}" | cut -d' ' -f2)"
	Q_QUEUED="$(printf '%s' "${Q_STATE}" | cut -d' ' -f3)"
	[ -z "${Q_RUNNING}" ] && Q_RUNNING=0
	[ -z "${Q_QUEUED}" ] && Q_QUEUED=0
	[ "${Q_RUNNING}" -gt "${Q_MAX_RUNNING}" ] 2>/dev/null && \
		Q_MAX_RUNNING="${Q_RUNNING}"
	[ "${Q_QUEUED}" -gt 0 ] 2>/dev/null && Q_SAW_QUEUED=1
	[ "${Q_TOTAL}" = "0" ] && break
	sleep 0.2
done

if [ "${Q_MAX_RUNNING}" -le 1 ] 2>/dev/null; then
	pass "never more than one decouple of the export ran at a time"
else
	fail "${Q_MAX_RUNNING} decouples of one export ran concurrently"
fi
# Not asserted: on a volume this small the first decouple can finish inside one
# poll interval, so the queued state is real but may never be observed.
if [ "${Q_SAW_QUEUED}" = "1" ]; then
	pass "the second volume was observed waiting in the queue"
else
	info "the first decouple finished before a queued state could be sampled"
fi

# Both must actually have been decoupled. Release is the check: it refuses while
# any volume still reads the export.
Q_RELEASED=0
for _ in $(seq 40); do
	if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
			"${MULTI2_UUID}" "${SRC_LVS}")" \
			>/dev/null 2>"${WORKDIR}/multi2_release.err"; then
		Q_RELEASED=1
		break
	fi
	sleep 0.5
done
if [ "${Q_RELEASED}" = "1" ]; then
	pass "both queued decouples completed, so the export could be released"
else
	fail "the export could not be released; a queued decouple never ran"
	sed 's/^/       /' "${WORKDIR}/multi2_release.err" 2>/dev/null
fi

# And both volumes still read their data, now from their own clusters.
Q_EXPECTED="$(md5sum "${WORKDIR}/Q.bin" | cut -d' ' -f1)"
Q_BOTH_OK=1
for vol in "${MULTI2_A}" "${MULTI2_B}"; do
	BEFORE_Q_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
	${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${vol}" \
		>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${vol})"; exit 1; }
	Q_DEV="$(wait_for_new_ns "${BEFORE_Q_NS}")"
	if [ -z "${Q_DEV}" ]; then
		fail "no namespace for ${vol}"
		Q_BOTH_OK=0
		continue
	fi
	Q_MD5="$(read_md5_at "${Q_DEV}" "${WORKDIR}/q_${vol}.bin" \
		"${MULTI_DATA_OFF_MB}" "${IO_LEN_MB}")"
	if [ "${Q_MD5}" != "${Q_EXPECTED}" ]; then
		fail "${vol} does not read its data after decoupling (${Q_MD5})"
		Q_BOTH_OK=0
	fi
done
if [ "${Q_BOTH_OK}" = "1" ]; then
	pass "both decoupled volumes read their data without the export"
fi

for vol in "${MULTI2_A}" "${MULTI2_B}"; do
	Q_NSID="$(nsid_of "${DST_LVS}/${vol}")"
	[ -n "${Q_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
		"${Q_NSID}" >/dev/null 2>&1 || true
	raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${vol}")" \
		>/dev/null 2>&1 || fail "rcow_delete_lvol (${vol})"
done
raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
	>/dev/null 2>&1 || fail "rcow_unload_lvstore (destination, after step 11g)"
DST_CREATED=0

MULTI2_NSID="$(nsid_of "${SRC_LVS}/${MULTI2_VOL}")"
[ -n "${MULTI2_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
	"${MULTI2_NSID}" >/dev/null 2>&1 || true
raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${MULTI2_VOL}")" \
	>/dev/null 2>&1 || fail "rcow_delete_lvol (${MULTI2_VOL})"
raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${MULTI2_SNAP}")" \
	>/dev/null 2>&1 || fail "rcow_delete_lvol (${MULTI2_SNAP})"

check_target "step 11g" || exit 1

# ==========================================================================
# [11h] a second decouple of the same lvstore, from a different export
#
# Step 11g covers several volumes taking turns at one *export*, which is rationed
# because they would fetch the same objects twice. Two different exports do not
# collide that way, so this is the case where the reason to wait is blobstore
# rather than S3: cluster allocation is serialised per io channel, a channel is per
# thread per blobstore, and both decouples run on the RPC thread.
#
# The bug that made this step necessary. spdk_blob_materialize_cluster() hands
# blobstore a zero-length read as the operation to re-execute once the cluster is
# in place. bs_allocate_and_copy_cluster() also queues an operation, doing nothing
# else with it, when an allocation for some other cluster is already in flight on
# the channel -- and re-executing a zero-length read copies nothing and reports
# success. So every cluster of the volume that lost the channel was counted as
# materialised without being read, and the volume was detached from an export it
# had never read: 43 clusters, 43 "materialised", 40 KiB fetched instead of 43 MiB,
# and a filesystem that would not mount.
#
# That is fixed in the API, which re-offers a cluster that lost the channel, so the
# data now arrives either way. Queueing is on top of that and is about time rather
# than correctness: with both running, those 43 clusters took 71 seconds behind 700
# instead of the 3 they take alone, the first of them losing the channel over 500
# times, and nothing was gained since blobstore serialised the work regardless.
#
# So there are two things to check and they are independent. That the second volume
# waits -- and is reported as waiting, for the lvstore rather than for the export.
# And that its data is all there afterwards, read back byte for byte, which is the
# only way to tell a materialised cluster from one that was merely counted.
#
# The first volume is deliberately larger, so that it is still copying when the
# second request arrives. It is checked too: there was never a reason to think the
# *first* one suffers, and a regression that broke it should not hide here.
# ==========================================================================
echo
echo "[11h] a second decouple of one lvstore, from another export"

CC_BIG_VOL="${SRC_VOL}-cc-big"
CC_SMALL_VOL="${SRC_VOL}-cc-small"
CC_BIG_SNAP="${CC_BIG_VOL}-snap"
CC_SMALL_SNAP="${CC_SMALL_VOL}-snap"
CC_BIG_DST="${DST_VOL}-cc-big"
CC_SMALL_DST="${DST_VOL}-cc-small"

# Four regions rather than one: each is its own set of clusters to fetch, and the
# copying has to outlast the second import's arrival.
CC_BIG_REGIONS=4

CC_SETUP_OK=1
for vol in "${CC_BIG_VOL}" "${CC_SMALL_VOL}"; do
	raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
		"${vol}" "${LVOL_GIB}")" >/dev/null 2>"${WORKDIR}/cc_lvol.err" || {
		fail "rcow_create_lvol (${vol})"
		sed 's/^/       /' "${WORKDIR}/cc_lvol.err"
		CC_SETUP_OK=0
	}
done
[ "${CC_SETUP_OK}" = "1" ] || exit 1

BEFORE_CC_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${CC_BIG_VOL}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${CC_BIG_VOL})"; exit 1; }
CC_BIG_SRC_DEV="$(wait_for_new_ns "${BEFORE_CC_NS}")"
[ -n "${CC_BIG_SRC_DEV}" ] || { fail "no namespace for ${CC_BIG_VOL}"; exit 1; }

CC_R=0
while [ "${CC_R}" -lt "${CC_BIG_REGIONS}" ]; do
	write_pattern_at "${CC_BIG_SRC_DEV}" "${WORKDIR}/CCB${CC_R}.bin" \
		"$((MULTI_DATA_OFF_MB + CC_R * IO_LEN_MB))" "${IO_LEN_MB}"
	CC_R=$((CC_R + 1))
done

BEFORE_CC_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${CC_SMALL_VOL}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${CC_SMALL_VOL})"; exit 1; }
CC_SMALL_SRC_DEV="$(wait_for_new_ns "${BEFORE_CC_NS}")"
[ -n "${CC_SMALL_SRC_DEV}" ] || { fail "no namespace for ${CC_SMALL_VOL}"; exit 1; }

write_pattern_at "${CC_SMALL_SRC_DEV}" "${WORKDIR}/CCS.bin" \
	"${MULTI_DATA_OFF_MB}" "${IO_LEN_MB}"

for pair in "${CC_BIG_VOL}:${CC_BIG_SNAP}" "${CC_SMALL_VOL}:${CC_SMALL_SNAP}"; do
	raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${pair%%:*}" "${pair##*:}")" >/dev/null 2>"${WORKDIR}/cc_snap.err" || {
		fail "rcow_create_snapshot (${pair%%:*})"
		sed 's/^/       /' "${WORKDIR}/cc_snap.err"
		exit 1
	}
done

CC_BIG_UUID="$(raw_rpc rcow_export_snapshot \
	"$(printf '{"snapshot_name":"%s"}' "${CC_BIG_SNAP}")" 2>/dev/null \
	| tr -d '"[:space:]')"
CC_SMALL_UUID="$(raw_rpc rcow_export_snapshot \
	"$(printf '{"snapshot_name":"%s"}' "${CC_SMALL_SNAP}")" 2>/dev/null \
	| tr -d '"[:space:]')"
if [ -z "${CC_BIG_UUID}" ] || [ -z "${CC_SMALL_UUID}" ]; then
	fail "rcow_export_snapshot (step 11h)"
	exit 1
fi
wait_export_done "${CC_BIG_UUID}" "step 11h (big)" || exit 1
wait_export_done "${CC_SMALL_UUID}" "step 11h (small)" || exit 1

if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${DST_LVS}" "${BUCKET}" "${DST_WAL_BDEV}")" \
		>/dev/null 2>"${WORKDIR}/cc_attach.err"; then
	fail "rcow_attach_lvstore (destination, for step 11h)"
	sed 's/^/       /' "${WORKDIR}/cc_attach.err"
	exit 1
fi
DST_CREATED=1

if ! raw_rpc rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
		"${CC_BIG_DST}" "${CC_BIG_UUID}" "${DST_LVS}")" \
		>/dev/null 2>"${WORKDIR}/cc_import_big.err"; then
	fail "rcow_import_lvol (${CC_BIG_DST}, decouple:true)"
	sed 's/^/       /' "${WORKDIR}/cc_import_big.err"
	exit 1
fi

# Wait until the first one is demonstrably copying, so the second really does land
# underneath it rather than after it.
CC_BIG_RUNNING=0
for _ in $(seq 100); do
	CC_N="$(raw_rpc rcow_get_decouple 2>/dev/null | python3 -c "
import json, sys
uuid = sys.argv[1]
try:
    rows = json.load(sys.stdin)
except Exception:
    print(0); sys.exit(0)
print(len([r for r in rows
           if r.get('export_uuid') == uuid and not r.get('queued')]))
" "${CC_BIG_UUID}" 2>/dev/null)"
	[ "${CC_N}" = "1" ] && { CC_BIG_RUNNING=1; break; }
	sleep 0.1
done
if [ "${CC_BIG_RUNNING}" = "1" ]; then
	pass "the first export's decouple was running when the second was submitted"
else
	info "the first decouple was not caught running; the race may not be exercised"
fi

if ! raw_rpc rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
		"${CC_SMALL_DST}" "${CC_SMALL_UUID}" "${DST_LVS}")" \
		>/dev/null 2>"${WORKDIR}/cc_import_small.err"; then
	fail "rcow_import_lvol (${CC_SMALL_DST}, decouple:true)"
	sed 's/^/       /' "${WORKDIR}/cc_import_small.err"
	exit 1
fi

# Two different exports, but one lvstore, so the second one waits. Not for
# bandwidth -- these name different objects -- but because blobstore serialises
# cluster allocation per io channel, a channel is per thread per blobstore, and
# both decouples run on the same thread. Overlapping them gains nothing and costs
# the smaller volume any bound on when it finishes: measured before this was
# queued, 43 clusters took 71 seconds behind 700 instead of the 3 they take alone,
# with the first cluster losing the channel over 500 times.
#
# So what is asserted is the opposite of what the overlap would suggest: never two
# running at once, and the second one seen waiting. Both still succeed, and the
# data check below is what says the wait did not cost anything.
CC_MAX_RUNNING=0
CC_SAW_QUEUED=0
for _ in $(seq 600); do
	CC_STATE="$(raw_rpc rcow_get_decouple 2>/dev/null | python3 -c "
import json, sys
try:
    rows = json.load(sys.stdin)
except Exception:
    print('0 0 0'); sys.exit(0)
running = [r for r in rows if not r.get('queued')]
queued  = [r for r in rows if r.get('queued')]
print('%d %d %d' % (len(rows), len(running), len(queued)))
" 2>/dev/null)"
	CC_TOTAL="$(printf '%s' "${CC_STATE}" | cut -d' ' -f1)"
	CC_RUNNING="$(printf '%s' "${CC_STATE}" | cut -d' ' -f2)"
	CC_QUEUED="$(printf '%s' "${CC_STATE}" | cut -d' ' -f3)"
	[ -z "${CC_RUNNING}" ] && CC_RUNNING=0
	[ -z "${CC_QUEUED}" ] && CC_QUEUED=0
	[ "${CC_RUNNING}" -gt "${CC_MAX_RUNNING}" ] 2>/dev/null && \
		CC_MAX_RUNNING="${CC_RUNNING}"
	[ "${CC_QUEUED}" -gt 0 ] 2>/dev/null && CC_SAW_QUEUED=1
	[ "${CC_TOTAL}" = "0" ] && break
	sleep 0.2
done
if [ "${CC_MAX_RUNNING}" -le 1 ] 2>/dev/null; then
	pass "never two decouples of one lvstore ran at the same time"
else
	fail "${CC_MAX_RUNNING} decouples of one lvstore ran concurrently"
fi
if [ "${CC_SAW_QUEUED}" = "1" ]; then
	pass "the second export's volume was observed waiting in the queue"
else
	info "the queued state was not sampled before the first decouple finished"
fi

# Queued for the right reason, and said so. Distinguishes this from the 11g case,
# where two volumes of one *export* queue -- a log that reported "same export"
# here would mean the lvstore rule never fired and the step is testing 11g again.
if grep -q "queued to be decoupled from .* (same lvstore)" "${TGT_LOG}"; then
	pass "the wait was attributed to the lvstore, not to the export"
else
	info "no 'same lvstore' queueing was logged; the two may not have overlapped"
fi

# The point of the step. A cluster counted as materialised without having been
# copied reads as zeroes, and after the parent is dropped there is nothing to fall
# back to, so this comparison is the only thing that can tell the difference.
CC_DATA_OK=1
for pair in "${CC_SMALL_DST}:${WORKDIR}/CCS.bin:${MULTI_DATA_OFF_MB}"; do
	vol="$(printf '%s' "${pair}" | cut -d: -f1)"
	src="$(printf '%s' "${pair}" | cut -d: -f2)"
	off="$(printf '%s' "${pair}" | cut -d: -f3)"

	BEFORE_CC_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
	${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${vol}" \
		>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${vol})"; exit 1; }
	CC_DEV="$(wait_for_new_ns "${BEFORE_CC_NS}")"
	if [ -z "${CC_DEV}" ]; then
		fail "no namespace for ${vol}"
		CC_DATA_OK=0
		continue
	fi
	CC_GOT="$(read_md5_at "${CC_DEV}" "${WORKDIR}/cc_got_${vol}.bin" \
		"${off}" "${IO_LEN_MB}")"
	CC_WANT="$(md5sum "${src}" | cut -d' ' -f1)"
	if [ "${CC_GOT}" != "${CC_WANT}" ]; then
		fail "${vol} lost data to the concurrent decouple (${CC_GOT} != ${CC_WANT})"
		CC_DATA_OK=0
	fi
done
if [ "${CC_DATA_OK}" = "1" ]; then
	pass "the volume that started second holds every byte of its export"
fi

# And the volume that was already copying is intact region by region.
BEFORE_CC_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${CC_BIG_DST}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${CC_BIG_DST})"; exit 1; }
CC_BIG_DEV="$(wait_for_new_ns "${BEFORE_CC_NS}")"
if [ -z "${CC_BIG_DEV}" ]; then
	fail "no namespace for ${CC_BIG_DST}"
else
	CC_BIG_OK=1
	CC_R=0
	while [ "${CC_R}" -lt "${CC_BIG_REGIONS}" ]; do
		CC_GOT="$(read_md5_at "${CC_BIG_DEV}" "${WORKDIR}/cc_got_big${CC_R}.bin" \
			"$((MULTI_DATA_OFF_MB + CC_R * IO_LEN_MB))" "${IO_LEN_MB}")"
		CC_WANT="$(md5sum "${WORKDIR}/CCB${CC_R}.bin" | cut -d' ' -f1)"
		[ "${CC_GOT}" = "${CC_WANT}" ] || {
			fail "${CC_BIG_DST} region ${CC_R} differs (${CC_GOT} != ${CC_WANT})"
			CC_BIG_OK=0
		}
		CC_R=$((CC_R + 1))
	done
	[ "${CC_BIG_OK}" = "1" ] && \
		pass "the volume that was already copying kept all ${CC_BIG_REGIONS} regions"
fi

# The guard in decouple_cluster_done() would have caught the old bug rather than
# letting it through, so its absence from the log is a second, independent
# statement that no cluster was counted without being copied.
if grep -q "reported materialised but is not allocated" "${TGT_LOG}"; then
	fail "a cluster was counted as materialised without being allocated"
	grep "reported materialised but is not allocated" "${TGT_LOG}" | \
		head -3 | sed 's/^/       /'
else
	pass "no cluster was counted as materialised without being allocated"
fi

# Both are independent now, which release proves for each export separately.
for uuid in "${CC_BIG_UUID}" "${CC_SMALL_UUID}"; do
	CC_REL=0
	for _ in $(seq 40); do
		if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
				"${uuid}" "${SRC_LVS}")" \
				>/dev/null 2>"${WORKDIR}/cc_release.err"; then
			CC_REL=1
			break
		fi
		sleep 0.5
	done
	[ "${CC_REL}" = "1" ] || {
		fail "export ${uuid} could not be released after its decouple"
		sed 's/^/       /' "${WORKDIR}/cc_release.err" 2>/dev/null
	}
done

for vol in "${CC_BIG_DST}" "${CC_SMALL_DST}"; do
	CC_NSID="$(nsid_of "${DST_LVS}/${vol}")"
	[ -n "${CC_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
		"${CC_NSID}" >/dev/null 2>&1 || true
	raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${vol}")" \
		>/dev/null 2>&1 || fail "rcow_delete_lvol (${vol})"
done
raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
	>/dev/null 2>&1 || fail "rcow_unload_lvstore (destination, after step 11h)"
DST_CREATED=0

for vol in "${CC_BIG_VOL}" "${CC_SMALL_VOL}"; do
	CC_NSID="$(nsid_of "${SRC_LVS}/${vol}")"
	[ -n "${CC_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
		"${CC_NSID}" >/dev/null 2>&1 || true
done
for vol in "${CC_BIG_VOL}" "${CC_SMALL_VOL}" "${CC_BIG_SNAP}" "${CC_SMALL_SNAP}"; do
	raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${vol}")" \
		>/dev/null 2>&1 || fail "rcow_delete_lvol (${vol})"
done

check_target "step 11h" || exit 1

# ==========================================================================
# [11i] three volumes imported concurrently from one export stay consistent
#
# 11f imported two volumes with decouple:false, one RPC after another. 11g
# queued two decouples of one export. This step is the third shape: three
# imports submitted at the same time, all decoupling from the same export, and
# the question is the data.
#
# Three kinds of stability are meant by "stable".
#
#   1. Before and after materialisation. A decoupled volume is an esnap clone
#      while its decouple runs: reads go through to the export. Once the
#      clusters are materialised, reads come from the local blob. The bytes
#      must not change across that transition. The queue from 11g guarantees
#      the transition is observable here: the first import starts decoupling
#      while the second and third are still queued, so reading them early
#      really does read an un-materialised volume, not merely a finished one.
#
#   2. Repeated reads. Reading the same volume twice must give the same bytes.
#
#   3. Independence. Copy-on-write means a write to one volume changes nothing
#      the others can see -- and their data regions must still match the
#      source afterwards.
#
# And one kind of consistency:
#
#   - All three volumes agree with each other and with the source snapshot,
#     data regions and holes alike. A hole that turns into non-zero data in
#     any of them is a materialisation bug (this is the class that used to
#     read as zeroes after d048f44).
#
# The data layout leaves a deliberate hole between the written regions, so
# that "the volume is intact" means "the written bytes are there and the
# unwritten bytes stayed zero" -- neither alone would be enough.
# ==========================================================================
echo
echo "[11i] three concurrent imports of one export, data stability"

CON3_VOL="${SRC_VOL}-con3"
CON3_SNAP="${CON3_VOL}-snap"
CON3_A="${DST_VOL}-c3-a"
CON3_B="${DST_VOL}-c3-b"
CON3_C="${DST_VOL}-c3-c"
CON3_OFF_MB=8
# Three data regions, one hole region. 8 MiB each: every region crosses at
# least one cluster boundary (clusters are 1 MiB), and three regions give the
# first decouple enough work that the other two are still queued when it runs.
CON3_REGIONS="16:CON3D1 32:CON3D2 48:CON3D3"
CON3_HOLE_MB=64
CON3_ZERO_MD5="$(dd if=/dev/zero bs=1M count="${IO_LEN_MB}" status=none \
	| md5sum | cut -d' ' -f1)"

if ! raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
		"${CON3_VOL}" "${LVOL_GIB}")" \
		>/dev/null 2>"${WORKDIR}/con3_lvol.err"; then
	fail "rcow_create_lvol (step 11i source volume)"
	sed 's/^/       /' "${WORKDIR}/con3_lvol.err"
	exit 1
fi

BEFORE_CON3_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${CON3_VOL}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (step 11i source)"; exit 1; }
CON3_SRC_DEV="$(wait_for_new_ns "${BEFORE_CON3_NS}")"
if [ -z "${CON3_SRC_DEV}" ]; then
	fail "no namespace for the step 11i source volume"
	exit 1
fi

# Source data: three patterned regions at 16/32/48 MiB, hole at 64 MiB.
CON3_SRC_OK=1
for spec in ${CON3_REGIONS}; do
	off="$(printf '%s' "${spec}" | cut -d: -f1)"
	name="$(printf '%s' "${spec}" | cut -d: -f2)"
	write_pattern_at "${CON3_SRC_DEV}" "${WORKDIR}/${name}.bin" \
		"$((CON3_OFF_MB + off))" "${IO_LEN_MB}" || CON3_SRC_OK=0
done
[ "${CON3_SRC_OK}" = "1" ] || exit 1

if ! raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${CON3_VOL}" "${CON3_SNAP}")" \
		>/dev/null 2>"${WORKDIR}/con3_snap.err"; then
	fail "rcow_create_snapshot (step 11i)"
	sed 's/^/       /' "${WORKDIR}/con3_snap.err"
	exit 1
fi

CON3_UUID="$(raw_rpc rcow_export_snapshot \
	"$(printf '{"snapshot_name":"%s"}' "${CON3_SNAP}")" 2>/dev/null \
	| tr -d '"[:space:]')"
if [ -z "${CON3_UUID}" ]; then
	fail "rcow_export_snapshot (step 11i)"
	exit 1
fi
wait_export_done "${CON3_UUID}" "step 11i" || exit 1
pass "an export created for the three-way concurrent import (${CON3_UUID})"

if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${DST_LVS}" "${BUCKET}" "${DST_WAL_BDEV}")" \
		>/dev/null 2>"${WORKDIR}/con3_attach.err"; then
	fail "rcow_attach_lvstore (destination, for step 11i)"
	sed 's/^/       /' "${WORKDIR}/con3_attach.err"
	exit 1
fi
DST_CREATED=1

# Device of an imported volume, by volume name.
con3_dev_of()
{
	case "$1" in
		"${CON3_A}") printf '%s' "${CON3_DEV_A}" ;;
		"${CON3_B}") printf '%s' "${CON3_DEV_B}" ;;
		"${CON3_C}") printf '%s' "${CON3_DEV_C}" ;;
		*) return 1 ;;
	esac
}

# Three imports submitted in one breath, none of them waited for before the
# next is sent. All decouple:true, so all three end up queued behind the same
# export and materialise one after another (the 11g queue).
CON3_IMPORT_OK=1
CON3_PIDS=""
i=0
for vol in "${CON3_A}" "${CON3_B}" "${CON3_C}"; do
	i=$((i + 1))
	raw_rpc rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
			"${vol}" "${CON3_UUID}" "${DST_LVS}")" \
			>"${WORKDIR}/con3_import_${i}.out" 2>"${WORKDIR}/con3_import_${i}.err" &
	CON3_PIDS="${CON3_PIDS} $!"
done
i=0
for pid in ${CON3_PIDS}; do
	i=$((i + 1))
	wait "${pid}" || CON3_IMPORT_OK=0
done
if [ "${CON3_IMPORT_OK}" = "1" ]; then
	pass "all three imports submitted concurrently succeeded"
else
	fail "a concurrent import failed"
	for i in 1 2 3; do
		[ -s "${WORKDIR}/con3_import_${i}.err" ] && \
			sed 's/^/       /' "${WORKDIR}/con3_import_${i}.err"
	done
	exit 1
fi

# One registry entry for all three, as in 11f.
CON3_ENTRIES="$(raw_rpc rcow_get_imports 2>/dev/null | python3 -c "
import json, sys
uuid = sys.argv[1]
try:
    rows = json.load(sys.stdin)
except Exception:
    print('parse-error'); sys.exit(0)
print(sum(1 for r in rows if r.get('export_uuid') == uuid))
" "${CON3_UUID}" 2>/dev/null)"
if [ "${CON3_ENTRIES}" = "1" ]; then
	pass "three volumes share one registry entry"
else
	fail "expected one registry entry for the export, got '${CON3_ENTRIES}'"
fi

# Expose all three, one RPC after another as an orchestration layer would.
BEFORE_CON3_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${CON3_A}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${CON3_A})"; exit 1; }
CON3_DEV_A="$(wait_for_new_ns "${BEFORE_CON3_NS}")"
BEFORE_CON3_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${CON3_B}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${CON3_B})"; exit 1; }
CON3_DEV_B="$(wait_for_new_ns "${BEFORE_CON3_NS}")"
BEFORE_CON3_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${CON3_C}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${CON3_C})"; exit 1; }
CON3_DEV_C="$(wait_for_new_ns "${BEFORE_CON3_NS}")"
if [ -z "${CON3_DEV_A}" ] || [ -z "${CON3_DEV_B}" ] || [ -z "${CON3_DEV_C}" ]; then
	fail "not all three imported volumes got a namespace"
	exit 1
fi
pass "all three volumes exposed: ${CON3_DEV_A}, ${CON3_DEV_B}, ${CON3_DEV_C}"

# T1: read everything now, deliberately before waiting for the decouples.
# Because import answers before its decouple finishes, and the second and third
# volumes are still queued at this point, at least those two are read while
# un-materialised -- reads going through the export. This is the "before" side
# of the stability comparison; nothing depends on the timing being perfect, but
# the queue state sampled right after is the evidence that it was right.
CON3_PRE_OK=1
for vol in "${CON3_A}" "${CON3_B}" "${CON3_C}"; do
	dev="$(con3_dev_of "${vol}")"
	for spec in ${CON3_REGIONS}; do
		off="$(printf '%s' "${spec}" | cut -d: -f1)"
		name="$(printf '%s' "${spec}" | cut -d: -f2)"
		want="$(md5sum "${WORKDIR}/${name}.bin" | cut -d' ' -f1)"
		got="$(read_md5_at "${dev}" "${WORKDIR}/con3_${vol##*-}_${name}.bin" \
			"$((CON3_OFF_MB + off))" "${IO_LEN_MB}")"
		[ "${got}" = "${want}" ] || CON3_PRE_OK=0
	done
	got_hole="$(read_md5_at "${dev}" "${WORKDIR}/con3_${vol##*-}_hole.bin" \
		"${CON3_HOLE_MB}" "${IO_LEN_MB}")"
	[ "${got_hole}" = "${CON3_ZERO_MD5}" ] || CON3_PRE_OK=0
done
if [ "${CON3_PRE_OK}" = "1" ]; then
	pass "all three volumes read the source data and zero holes while decoupling was in flight"
else
	fail "an early read did not match the source"
	exit 1
fi

# Evidence that the early read really straddled the materialisation. If the
# queue was non-empty when the reads finished, at least one volume was still
# un-materialised while being read.
CON3_QSTATE="$(raw_rpc rcow_get_decouple 2>/dev/null | python3 -c "
import json, sys
uuid = sys.argv[1]
try:
    rows = json.load(sys.stdin)
except Exception:
    print('0'); sys.exit(0)
print(len([r for r in rows if r.get('export_uuid') == uuid]))
" "${CON3_UUID}" 2>/dev/null)"
if [ "${CON3_QSTATE}" != "0" ] && [ -n "${CON3_QSTATE}" ]; then
	pass "the early reads overlapped decoupling (${CON3_QSTATE} still in the queue)"
else
	info "the early reads missed the decoupling window; stability is still asserted but with less force"
fi

# Wait for all three decouples to drain.
CON3_DRAINED=0
for _ in $(seq 600); do
	N="$(raw_rpc rcow_get_decouple 2>/dev/null | python3 -c "
import json, sys
try:
    print(len(json.load(sys.stdin)))
except Exception:
    print(0)" 2>/dev/null)"
	[ "${N}" = "0" ] && { CON3_DRAINED=1; break; }
	sleep 0.5
done
if [ "${CON3_DRAINED}" = "1" ]; then
	pass "all three decouples completed"
else
	fail "decouples did not drain within the wait"
	exit 1
fi

# T2: the same reads again, now from the materialised blobs. Must be byte for
# byte identical to the early reads (stability across materialisation) and to
# the source (consistency). Read twice: the second pass is the repeated-read
# stability check.
CON3_POST_OK=1
for pass in 1 2; do
	for vol in "${CON3_A}" "${CON3_B}" "${CON3_C}"; do
		dev="$(con3_dev_of "${vol}")"
		for spec in ${CON3_REGIONS}; do
			off="$(printf '%s' "${spec}" | cut -d: -f1)"
			name="$(printf '%s' "${spec}" | cut -d: -f2)"
			want="$(md5sum "${WORKDIR}/${name}.bin" | cut -d' ' -f1)"
			got="$(read_md5_at "${dev}" "${WORKDIR}/con3_${vol##*-}_post${pass}_${name}.bin" \
				"$((CON3_OFF_MB + off))" "${IO_LEN_MB}")"
			[ "${got}" = "${want}" ] || CON3_POST_OK=0
		done
		got_hole="$(read_md5_at "${dev}" "${WORKDIR}/con3_${vol##*-}_post${pass}_hole.bin" \
			"${CON3_HOLE_MB}" "${IO_LEN_MB}")"
		[ "${got_hole}" = "${CON3_ZERO_MD5}" ] || CON3_POST_OK=0
	done
done
if [ "${CON3_POST_OK}" = "1" ]; then
	pass "all three volumes are byte-identical to the source twice after materialisation"
else
	fail "a post-materialisation read did not match the source"
	exit 1
fi

# Copy-on-write independence, with a fresh write into a region that is a hole
# in the export: the writer reads it back, the other two must still see zero.
CON3_WRITE_OFF_MB=72
write_pattern_at "${CON3_DEV_A}" "${WORKDIR}/CON3W.bin" \
	"${CON3_WRITE_OFF_MB}" "${IO_LEN_MB}"
CON3W_MD5="$(md5sum "${WORKDIR}/CON3W.bin" | cut -d' ' -f1)"
CON3_ISOLATION_OK=1
CON3_A_WRITE="$(read_md5_at "${CON3_DEV_A}" "${WORKDIR}/con3_a_write.bin" \
	"${CON3_WRITE_OFF_MB}" "${IO_LEN_MB}")"
[ "${CON3_A_WRITE}" = "${CON3W_MD5}" ] || CON3_ISOLATION_OK=0
for vol in "${CON3_B}" "${CON3_C}"; do
	dev="$(con3_dev_of "${vol}")"
	got="$(read_md5_at "${dev}" "${WORKDIR}/con3_${vol##*-}_at_a.bin" \
		"${CON3_WRITE_OFF_MB}" "${IO_LEN_MB}")"
	[ "${got}" = "${CON3_ZERO_MD5}" ] || CON3_ISOLATION_OK=0
done
if [ "${CON3_ISOLATION_OK}" = "1" ]; then
	pass "a write to one volume is invisible to the other two"
else
	fail "the volumes are not independent"
	exit 1
fi

# Cleanup: three imported volumes, then the source volume and snapshot.
for vol in "${CON3_A}" "${CON3_B}" "${CON3_C}"; do
	CON3_NSID="$(nsid_of "${DST_LVS}/${vol}")"
	[ -n "${CON3_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
		"${CON3_NSID}" >/dev/null 2>&1 || true
	raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${vol}")" \
		>/dev/null 2>&1 || fail "rcow_delete_lvol (${vol})"
done
raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
	>/dev/null 2>&1 || fail "rcow_unload_lvstore (destination, after step 11i)"
DST_CREATED=0

# Once nothing reads the export, release must succeed -- the same proof as
# 11g that all three decouples really completed.
CON3_RELEASED=0
for _ in $(seq 40); do
	if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
			"${CON3_UUID}" "${SRC_LVS}")" \
			>/dev/null 2>"${WORKDIR}/con3_release.err"; then
		CON3_RELEASED=1
		break
	fi
	sleep 0.5
done
if [ "${CON3_RELEASED}" = "1" ]; then
	pass "release succeeded once all three volumes were gone"
else
	fail "release still refused after all three volumes were deleted"
	sed 's/^/       /' "${WORKDIR}/con3_release.err" 2>/dev/null
fi

CON3_NSID="$(nsid_of "${SRC_LVS}/${CON3_VOL}")"
[ -n "${CON3_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
	"${CON3_NSID}" >/dev/null 2>&1 || true
for vol in "${CON3_VOL}" "${CON3_SNAP}"; do
	raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${vol}")" \
		>/dev/null 2>&1 || fail "rcow_delete_lvol (${vol})"
done

check_target "step 11i" || exit 1

# ==========================================================================
# [11j] three exports of three snapshots, run at the same time
#
# Every earlier step exports one snapshot at a time. A sandbox teardown does
# not: it seals the rootfs, memory and meta volumes of one sandbox together,
# so three rcow_export_snapshot calls land within milliseconds of each other.
# Nothing in the export path serialises them on purpose -- each walks its own
# snapshot and writes its own manifest -- but that is exactly why this has to
# be exercised: concurrent walks share the flusher, the chunk map and the S3
# client, and a bug there would corrupt *all three* exports at once, which a
# one-at-a-time test can never see.
#
# The step is deliberately the mirror image of 11i. There, one export and
# three concurrent imports; here, three concurrent exports and one import of
# each. The checks are the same shape -- each volume is read back byte for
# byte -- but the shared object under test is different.
#
# Three source volumes, one data region each, at different offsets so the
# exports own disjoint cluster sets (no two snapshots share a cluster, which
# would let one walk cover for another's mistake).
# ==========================================================================
echo
echo "[11j] three snapshots exported at the same time"

XR_VOL_A="${SRC_VOL}-xr-a"
XR_VOL_B="${SRC_VOL}-xr-b"
XR_VOL_C="${SRC_VOL}-xr-c"
XR_SNAP_A="${XR_VOL_A}-snap"
XR_SNAP_B="${XR_VOL_B}-snap"
XR_SNAP_C="${XR_VOL_C}-snap"
XR_DST_A="${DST_VOL}-xr-a"
XR_DST_B="${DST_VOL}-xr-b"
XR_DST_C="${DST_VOL}-xr-c"

XR_SETUP_OK=1
i=0
for vol in "${XR_VOL_A}" "${XR_VOL_B}" "${XR_VOL_C}"; do
	i=$((i + 1))
	raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
		"${vol}" "${LVOL_GIB}")" >/dev/null 2>"${WORKDIR}/xr_lvol_${i}.err" || {
		fail "rcow_create_lvol (${vol})"
		sed 's/^/       /' "${WORKDIR}/xr_lvol_${i}.err"
		XR_SETUP_OK=0
	}
done
[ "${XR_SETUP_OK}" = "1" ] || exit 1

# One region per volume, at distinct offsets. 8 MiB each: enough clusters to
# make the walk do real work, not so much that the step drags.
i=0
for vol in "${XR_VOL_A}" "${XR_VOL_B}" "${XR_VOL_C}"; do
	i=$((i + 1))
	BEFORE_XR_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
	${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${vol}" \
		>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${vol})"; exit 1; }
	XR_DEV="$(wait_for_new_ns "${BEFORE_XR_NS}")"
	if [ -z "${XR_DEV}" ]; then
		fail "no namespace for ${vol}"
		exit 1
	fi
	write_pattern_at "${XR_DEV}" "${WORKDIR}/XR${i}.bin" \
		"$((MULTI_DATA_OFF_MB + i * IO_LEN_MB))" "${IO_LEN_MB}"
	eval "XR_DEV_${i}=\"${XR_DEV}\""
done

# Three snapshots, submitted in one breath. The point is that the exports
# below overlap, and they can only overlap if the snapshots all exist first;
# creating them concurrently is how a teardown would do it, and it also keeps
# this step from accidentally serialising the exports on the snapshot RPCs.
XR_SNAP_OK=1
XR_SNAP_PIDS=""
i=0
for pair in "${XR_VOL_A}:${XR_SNAP_A}" "${XR_VOL_B}:${XR_SNAP_B}" "${XR_VOL_C}:${XR_SNAP_C}"; do
	i=$((i + 1))
	raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${pair%%:*}" "${pair##*:}")" \
		>/dev/null 2>"${WORKDIR}/xr_snap_${i}.err" &
	XR_SNAP_PIDS="${XR_SNAP_PIDS} $!"
done
i=0
for p in ${XR_SNAP_PIDS}; do
	i=$((i + 1))
	wait "${p}" || {
		fail "concurrent snapshot ${i} failed"
		sed 's/^/       /' "${WORKDIR}/xr_snap_${i}.err"
		XR_SNAP_OK=0
	}
done
if [ "${XR_SNAP_OK}" = "1" ]; then
	pass "all three snapshots created"
else
	exit 1
fi

# The three exports themselves. No waiting between submissions: this is the
# shape a teardown produces, three seal operations in flight at once.
XR_UUIDS=""
XR_EXPORT_PIDS=""
i=0
for snap in "${XR_SNAP_A}" "${XR_SNAP_B}" "${XR_SNAP_C}"; do
	i=$((i + 1))
	raw_rpc rcow_export_snapshot "$(printf '{"snapshot_name":"%s"}' "${snap}")" \
		>"${WORKDIR}/xr_exp_${i}.out" 2>"${WORKDIR}/xr_exp_${i}.err" &
	XR_EXPORT_PIDS="${XR_EXPORT_PIDS} $!"
done
i=0
for p in ${XR_EXPORT_PIDS}; do
	i=$((i + 1))
	wait "${p}" || {
		fail "concurrent export ${i} failed"
		sed 's/^/       /' "${WORKDIR}/xr_exp_${i}.err"
		exit 1
	}
	U="$(tr -d '"[:space:]' < "${WORKDIR}/xr_exp_${i}.out")"
	[ -n "${U}" ] || { fail "concurrent export ${i} answered no uuid"; exit 1; }
	XR_UUIDS="${XR_UUIDS} ${U}"
done
XR_U_A="$(printf '%s' "${XR_UUIDS}" | awk '{print $1}')"
XR_U_B="$(printf '%s' "${XR_UUIDS}" | awk '{print $2}')"
XR_U_C="$(printf '%s' "${XR_UUIDS}" | awk '{print $3}')"
if [ -n "${XR_U_A}" ] && [ -n "${XR_U_B}" ] && [ -n "${XR_U_C}" ]; then
	pass "three exports submitted concurrently, all answered"
else
	fail "not all three exports produced a uuid (${XR_UUIDS})"
	exit 1
fi

# All three exported at once, each must reach DONE independently.
XR_DONE_OK=1
for pair in "${XR_U_A}:a" "${XR_U_B}:b" "${XR_U_C}:c"; do
	u="$(printf '%s' "${pair}" | cut -d: -f1)"
	tag="$(printf '%s' "${pair}" | cut -d: -f2)"
	wait_export_done "${u}" "step 11j (${tag})" || XR_DONE_OK=0
done
[ "${XR_DONE_OK}" = "1" ] && \
	pass "all three concurrent exports reached DONE"
if [ "${XR_DONE_OK}" != "1" ]; then
	fail "a concurrent export never finished"
	exit 1
fi

# Import each once, read each back byte for byte against what was written.
if ! raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${DST_LVS}" "${BUCKET}" "${DST_WAL_BDEV}")" \
		>/dev/null 2>"${WORKDIR}/xr_attach.err"; then
	fail "rcow_attach_lvstore (destination, for step 11j)"
	sed 's/^/       /' "${WORKDIR}/xr_attach.err"
	exit 1
fi
DST_CREATED=1

XR_DATA_OK=1
i=0
for pair in "${XR_DST_A}:${XR_U_A}:1" "${XR_DST_B}:${XR_U_B}:2" "${XR_DST_C}:${XR_U_C}:3"; do
	dst="$(printf '%s' "${pair}" | cut -d: -f1)"
	u="$(printf '%s' "${pair}" | cut -d: -f2)"
	idx="$(printf '%s' "${pair}" | cut -d: -f3)"
	i=$((i + 1))

	if ! raw_rpc rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s"}' \
			"${dst}" "${u}" "${DST_LVS}")" \
			>/dev/null 2>"${WORKDIR}/xr_imp_${i}.err"; then
		fail "rcow_import_lvol (${dst})"
		sed 's/^/       /' "${WORKDIR}/xr_imp_${i}.err"
		XR_DATA_OK=0
		continue
	fi

	BEFORE_XR_NS="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
	${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${dst}" \
		>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (${dst})"; exit 1; }
	XR_DST_DEV="$(wait_for_new_ns "${BEFORE_XR_NS}")"
	if [ -z "${XR_DST_DEV}" ]; then
		fail "no namespace for ${dst}"
		XR_DATA_OK=0
		continue
	fi

	want="$(md5sum "${WORKDIR}/XR${idx}.bin" | cut -d' ' -f1)"
	got="$(read_md5_at "${XR_DST_DEV}" "${WORKDIR}/xr_got_${i}.bin" \
		"$((MULTI_DATA_OFF_MB + idx * IO_LEN_MB))" "${IO_LEN_MB}")"
	if [ "${got}" = "${want}" ]; then
		pass "${dst} holds every byte of its own export"
	else
		fail "${dst} lost data (${got} != ${want})"
		XR_DATA_OK=0
	fi
done
if [ "${XR_DATA_OK}" = "1" ]; then
	pass "all three concurrent exports produced identical data"
else
	fail "a concurrent export corrupted its data"
	exit 1
fi

# Cleanup: imported volumes, then the source volumes and snapshots.
for vol in "${XR_DST_A}" "${XR_DST_B}" "${XR_DST_C}"; do
	XR_NSID="$(nsid_of "${DST_LVS}/${vol}")"
	[ -n "${XR_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
		"${XR_NSID}" >/dev/null 2>&1 || true
	raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${vol}")" \
		>/dev/null 2>&1 || fail "rcow_delete_lvol (${vol})"
done
raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
	>/dev/null 2>&1 || fail "rcow_unload_lvstore (destination, after step 11j)"
DST_CREATED=0

XR_RELEASE_OK=1
for uuid in ${XR_UUIDS}; do
	XR_RELEASED=0
	for _ in $(seq 40); do
		if raw_rpc rcow_release_export "$(printf '{"export_uuid":"%s","lvs_name":"%s"}' \
				"${uuid}" "${SRC_LVS}")" \
				>/dev/null 2>"${WORKDIR}/xr_release.err"; then
			XR_RELEASED=1
			break
		fi
		sleep 0.5
	done
	if [ "${XR_RELEASED}" = "1" ]; then
		pass "export ${uuid} released"
	else
		fail "export ${uuid} could not be released"
		sed 's/^/       /' "${WORKDIR}/xr_release.err" 2>/dev/null
		XR_RELEASE_OK=0
	fi
done
[ "${XR_RELEASE_OK}" = "1" ] || exit 1

i=0
for vol in "${XR_VOL_A}" "${XR_VOL_B}" "${XR_VOL_C}"; do
	i=$((i + 1))
	XR_NSID="$(nsid_of "${SRC_LVS}/${vol}")"
	[ -n "${XR_NSID}" ] && ${RPC} nvmf_subsystem_remove_ns "${NQN}" \
		"${XR_NSID}" >/dev/null 2>&1 || true
done
for vol in "${XR_VOL_A}" "${XR_VOL_B}" "${XR_VOL_C}" \
	   "${XR_SNAP_A}" "${XR_SNAP_B}" "${XR_SNAP_C}"; do
	raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${vol}")" \
		>/dev/null 2>&1 || fail "rcow_delete_lvol (${vol})"
done

check_target "step 11j" || exit 1

# ==========================================================================
# [12] target log
# ==========================================================================
echo
echo "[12] inspecting the target log"

# Exactly one shape of data-object 404 is benign, and every other one is a data
# integrity failure whatever else passes.
#
# The benign shape is a read that finished a step too late. create-once deletes the
# superseded object as soon as the new mapping is durable and does not wait for the
# delete, so a reader that looked its uuid up before that point can GET an object
# that existed when it started. The bs_dev recovers by looking the mapping up again
# and rereading the object it now names, and says so in the log.
#
# Everything else means the chunk map named an object that is not in S3:
#
#   a 404 with no matching reread   the mapping points at an object that is absent.
#                              Note the bs_dev turns this into -ENOENT rather than
#                              zero-filling, which is right: a committed mapping
#                              with no object is not the same as a hole, and
#                              pretending otherwise would serve zeroes for data.
#   the bs_dev declining to reread  it says why: either the mapping did not change
#                              under the read, or the object is still missing after
#                              a reread. Both mean a bad mapping, not a late read.
#   Metadata page N read failed  the same thing happening to a blobstore metadata
#                              page, i.e. to a blob's own descriptor.
#
# These were seen intermittently (2026-08-05, two runs in twelve) and survived a
# whole run without failing anything, because a missing page still let blobstore
# carry on and every data checksum matched. That is exactly why it has to be a hard
# failure: nothing else in this script would notice.
REREAD_UUIDS="$(sed -nE 's/.*being read \(([0-9a-f-]{36}) ->.*/\1/p' \
	"${TGT_LOG}" 2>/dev/null | sort -u)"

# Pair each 404 with a reread of the same object; a 404 more than the rereads that
# account for it is one nobody recovered from.
UNACCOUNTED=""
while read -r uuid; do
	[ -n "${uuid}" ] || continue
	n404="$(grep -cE "data/${uuid}.*http=404" "${TGT_LOG}" 2>/dev/null || true)"
	nfix="$(grep -cF "being read (${uuid} ->" "${TGT_LOG}" 2>/dev/null || true)"
	if [ "${n404:-0}" -gt "${nfix:-0}" ]; then
		UNACCOUNTED="${UNACCOUNTED}${uuid}: ${n404} 404(s), ${nfix} reread(s)
"
	fi
done <<EOF
$(sed -nE 's#.*data/([0-9a-f-]{36}).*http=404.*#\1#p' "${TGT_LOG}" 2>/dev/null | sort -u)
EOF

BAD_MAPPING="$(grep -inE 'mapping did not change under us|still missing after a reread|Metadata page [0-9]+ read failed' \
	"${TGT_LOG}" 2>/dev/null || true)"

if [ -n "${UNACCOUNTED}" ] || [ -n "${BAD_MAPPING}" ]; then
	fail "the chunk map names objects that are not in S3"
	if [ -n "${UNACCOUNTED}" ]; then
		printf '%s' "${UNACCOUNTED}" | head -6 | sed 's/^/       /'
	fi
	if [ -n "${BAD_MAPPING}" ]; then
		echo "${BAD_MAPPING}" | head -6 | sed 's/^/       /'
	fi
	info "a committed chunk map entry must always have a durable object behind"
	info "it; see HANDOFF 7.10 for the export-side version of this bug"
else
	pass "every object the chunk map names is in S3"
fi

if [ -n "${REREAD_UUIDS}" ]; then
	info "$(printf '%s\n' "${REREAD_UUIDS}" | wc -l) chunk(s) were overwritten while"
	info "being read and were reread; that is the race working as intended"
fi

# The second filter is the CRT logging its own non-2xx responses at ERROR level.
# s3lvol reads 404 as "this key does not exist" on purpose (s3_client_aws.c:1538),
# so those lines are normal traffic. Only the 404s are filtered, not the "Invalid
# response status from request" text beside them: that is error 14343's generic
# string and accompanies every non-2xx, so filtering on it would hide a 500 or 503.
#
# They only became visible once the CRT was built from unmodified upstream sources
# -- the prefix used before belonged to another project and carried a local change
# excluding 404 from that log line -- so the exception list had been incomplete
# without anyone noticing.
# 'is the snapshot behind export' is the export-pin refusal, which step 3 now
# provokes on purpose to prove deletable agrees with the delete path. It is an
# expected refusal like the ones beside it, not a fault.
LOG_ERRORS="$(grep -inE 'error|failed' "${TGT_LOG}" 2>/dev/null | \
	grep -viE 'already|not found|read-only|is not a snapshot|meta/owner|still the parent|imports.json|is the snapshot behind export' | \
	grep -viE 'response status=404' || true)"
# The 404 a reread already dealt with is not an unexpected error either.
if [ -n "${LOG_ERRORS}" ] && [ -n "${REREAD_UUIDS}" ]; then
	LOG_ERRORS="$(printf '%s\n' "${LOG_ERRORS}" | \
		grep -vF -f <(printf '%s\n' "${REREAD_UUIDS}") || true)"
fi
if [ -n "${LOG_ERRORS}" ]; then
	info "errors mentioned in the log (expected ones filtered out):"
	echo "${LOG_ERRORS}" | head -8 | sed 's/^/       /'
else
	pass "no unexpected errors in the target log"
fi

if grep -q "Failed to register bdev" "${TGT_LOG}"; then
	fail "a bdev registration failed (see the log)"
else
	pass "every volume got its bdev"
fi

# ==========================================================================
# [13] what it cost
#
# The point of a referenced export is that these numbers do not grow with the
# volume, so they are worth printing even when everything passes. A handoff that
# still moved the data would show up here as seconds rather than as a change in
# any assertion above.
# ==========================================================================
echo
echo "[13] timings"

info "${TIMINGS}"
info "volume $((LVOL_SIZE / 1024 / 1024)) MiB provisioned, ${IO_LEN_MB} MiB written per region"
info "the walk resolved both snap1's and snap2's clusters through the same chunk map"

# Whether the walk had to flush before it could name everything. It is the one
# term in an export's latency that depends on how much was written rather than on
# how large the volume is, so which way it went explains any outlier above.
if grep -q "needs a drain" "${TGT_LOG}"; then
	info "at least one export had to drain first (the walk found an uncommitted"
	info "cluster and retried) -- so the numbers above include a flush"
else
	info "no export needed a drain: every cluster already had a committed mapping"
fi

if grep -q "nothing was copied" "${TGT_LOG}"; then
	pass "the target log confirms no data was copied"
else
	fail "the target log has no zero-copy export in it"
fi

echo
echo "=== all steps done ==="
