#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Deleting the source volume after an export / import must not corrupt the
#  imported volume (§11.7, HANDOFF 7.9)
#
#  === What this test is for ===
#
#  The source volume, its snapshot, and the import form one chain:
#
#      src lvol (writable, parent)
#        └── snapshot (read-only clone, the export target)
#              └── esnap clone on the destination (imported, reads through)
#
#  Deleting the source lvol merges it into its snapshot -- the snapshot owns the
#  clusters the export references, so blobstore must hand them to it rather than
#  free them. The load-bearing assertion is therefore what the destination reads
#  *after* the source volume is gone: the import was decoupled and materialised,
#  and if the merge had un-mapped or deleted anything the snapshot references,
#  that read would come back wrong (or as a 404 -- see the TTL hazard documented
#  in vbdev_s3lvol_lvstore.c).
#
#  === Flow ===
#
#  One blobstore per node: SRC and DST cannot be loaded at once. They never have
#  to be, because the export lives in S3 -- the manifest and the data objects
#  are in the bucket, and nothing on the source side needs its lvstore loaded
#  once the manifest is published. The flow is therefore:
#
#    1. SRC up: create volume, write pattern A, snapshot, export (DONE)
#    2. SRC up, still: delete the source volume -- the merge happens here,
#       while the source lvstore is loaded and the export holds the snapshot
#    3. unload SRC          -- the export is durable, S3 holds everything
#    4. DST up: import (decouple=true), wait for the decouple, read pattern A
#
#  Deleting the source lvol after the export is the point: the export pins the
#  *snapshot*, not the volume, so the delete is allowed and blobstore merges the
#  volume into its one clone (the snapshot). The destination's import is then
#  verified against the same pattern A -- if the merge had freed or re-mapped
#  anything the snapshot references, the read would differ (or 404, see the TTL
#  hazard documented in vbdev_s3lvol_lvstore.c).
#
#  decouple is explicit true (the RPC's default, stated explicitly): the import
#  materialises the export's data in the background and the test waits for it.
#  That is the configuration a caller actually uses when it wants the volume to
#  outlive the source, and the deletion that happened on the source in step 2 is
#  what it has to survive.
#
#  A second, cheaper assertion backs it up: the data objects under the source
#  prefix must not shrink when the lvol is deleted. The snapshot references
#  every cluster the lvol ever wrote, so deleting the lvol should delete
#  nothing.
#
#  Requires: root, nvme-cli, a real bucket with credentials in the environment.
#
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TOOLS_DIR="${REPO_ROOT}/test/tools"

TGT_BIN="${REPO_ROOT}/app/s3lvol_tgt/s3lvol_tgt"
RPC_PY="${SPDK_ROOT:-${REPO_ROOT}/deps/spdk}/scripts/rpc.py"
RPC="${RPC_PY} -s /var/run/s3lvol.sock"
RPC_SOCK="/var/run/s3lvol.sock"

SRC_LVS="sdsrc"
DST_LVS="sddst"
S3_EXPORTS_DIR="exports"
SRC_VOL="svol"
DST_VOL="dvol"
SNAP_NAME="svol-snap"

NQN="nqn.2026-08.io.spdk:srcdel"
LISTEN_ADDR="127.0.0.1"
LISTEN_PORT="4420"

CAPACITY_GIB=4
LVOL_GIB=1
JOURNAL_MB=64
WAL_MB=128
WAL_FILE_MB=$((JOURNAL_MB + WAL_MB + 128))

SRC_WAL_FILE="${S3LVOL_SRC_WAL_FILE:-/data/s3lvol_srcdel_src.img}"
DST_WAL_FILE="${S3LVOL_DST_WAL_FILE:-/data/s3lvol_srcdel_dst.img}"
SRC_WAL_BDEV="srcdel_src_wal0"
DST_WAL_BDEV="srcdel_dst_wal0"

# One MiB past the blobstore metadata region, several chunks wide.
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
TGT_CRASHED=0
EXPORT_UUID=""
NVME_DEV=""
SRC_NSID=""

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

WORKDIR="$(mktemp -d /tmp/s3lvol_srcdel.XXXXXX)"
TGT_LOG="${WORKDIR}/target.log"

raw_rpc()
{
	python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" "$1" "${2:-}"
}

export_status_field()
{
	local out

	out="$(raw_rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s"}' "$1")" 2>/dev/null)" || return 1
	printf '%s' "${out}" \
		| python3 -c "import json,sys; print(json.load(sys.stdin).get('$2',''))" \
			2>/dev/null
}

snapshot_status_field()
{
	local out

	out="$(raw_rpc rcow_get_snapshot_status \
		"$(printf '{"snapshot_name":"%s"}' "$1")" 2>/dev/null)" || return 1
	printf '%s' "${out}" \
		| python3 -c "import json,sys; print(json.load(sys.stdin).get('$2',''))" \
			2>/dev/null
}

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

md5_of()
{
	md5sum "$1" | cut -d' ' -f1
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

count_objects()
{
	python3 "${TOOLS_DIR}/s3_prefix_rm.py" -e "${ENDPOINT}" -b "${BUCKET}" \
		-r "${REGION}" -p "$1" --list 2>/dev/null | wc -l
}

remove_prefix()
{
	python3 "${TOOLS_DIR}/s3_prefix_rm.py" -e "${ENDPOINT}" -b "${BUCKET}" \
		-r "${REGION}" -p "$1"
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
			kill -KILL "${TGT_PID}" 2>/dev/null
		else
			info "target stopped cleanly"
		fi
	fi

	if [ "${SRC_WAS_CREATED}" -eq 1 ] || [ "${DST_WAS_CREATED}" -eq 1 ]; then
		if [ "${FAIL}" -eq 0 ] && [ "${rc}" -eq 0 ] && \
		   [ "${TEARDOWN_ANOMALY}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
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

echo "=== s3lvol source-delete test ==="
echo "    endpoint :${ENDPOINT}"
echo "    bucket   : ${BUCKET}"
echo "    prefixes : ${SRC_LVS}/ (source), ${DST_LVS}/ (destination)"
echo "    workdir  : ${WORKDIR}"
echo

# ==========================================================================
# [1] target + two local devices
# ==========================================================================
echo "[1] starting the target"

if pgrep -x s3lvol_tgt >/dev/null 2>&1; then
	fail "another s3lvol_tgt is running; stop it first (pkill -f s3lvol_tgt)"
	exit 1
fi

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
# [2] source: lvstore, volume, pattern A, snapshot, export
# ==========================================================================
echo
echo "[2] creating the source volume, writing pattern A"

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
pass "source volume created"

${RPC} nvmf_create_transport -t TCP >/dev/null 2>&1 || true
${RPC} nvmf_create_subsystem "${NQN}" -a -s S3SRCDEL00000000001 \
	>/dev/null 2>&1 || { fail "nvmf_create_subsystem"; exit 1; }
TRANSPORT_READY=1

${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${SRC_VOL}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (source)"; exit 1; }
${RPC} nvmf_subsystem_add_listener "${NQN}" -t tcp -a "${LISTEN_ADDR}" \
	-s "${LISTEN_PORT}" >/dev/null 2>&1 || { fail "add_listener"; exit 1; }
SRC_NSID="$(nsid_of "${SRC_LVS}/${SRC_VOL}")"

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

write_pattern_at "${NVME_DEV}" "${WORKDIR}/A.bin" "${IO_OFF_MB}" "${IO_LEN_MB}"
HASH_A="$(md5_of "${WORKDIR}/A.bin")"
if [ "$(read_md5_at "${NVME_DEV}" "${WORKDIR}/A_read.bin" \
		"${IO_OFF_MB}" "${IO_LEN_MB}")" = "${HASH_A}" ]; then
	pass "pattern A written and read back on the source"
else
	fail "the source did not read back pattern A"
	exit 1
fi
check_target "step 2" || exit 1

echo
echo "[2b] snapshot and export"

if ! raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
		"${SRC_VOL}" "${SNAP_NAME}")" \
		>"${WORKDIR}/snap.json" 2>"${WORKDIR}/snap.err"; then
	fail "rcow_create_snapshot"
	sed 's/^/       /' "${WORKDIR}/snap.err"
	check_target "step 2b" || exit 1
	exit 1
fi
pass "snapshot ${SNAP_NAME} created"

# The source volume's namespace has to go before the volume can be deleted:
# the delete is refused while the volume is active (rcow_active_bdev registry)
# or attached (bdev open descriptor). The export pins the *snapshot*, not the
# volume, so removing the namespace is the only precondition on this side.
${RPC} nvmf_subsystem_remove_ns "${NQN}" "${SRC_NSID}" \
	>/dev/null 2>"${WORKDIR}/rm_src_ns.err" || {
	fail "nvmf_subsystem_remove_ns (source volume)"
	sed 's/^/       /' "${WORKDIR}/rm_src_ns.err"
	exit 1; }
pass "source volume namespace removed (the export keeps the snapshot pinned)"

if ! raw_rpc rcow_export_snapshot \
		"$(printf '{"snapshot_name":"%s"}' "${SNAP_NAME}")" \
		>"${WORKDIR}/export.json" 2>"${WORKDIR}/export.err"; then
	fail "rcow_export_snapshot"
	sed 's/^/       /' "${WORKDIR}/export.err"
	check_target "step 2b" || exit 1
	exit 1
fi
EXPORT_UUID="$(tr -d ' \t\r\n' <"${WORKDIR}/export.json")"
if [ -z "${EXPORT_UUID}" ]; then
	fail "the export reported no uuid"
	exit 1
fi
wait_export_done "${EXPORT_UUID}" "step 2b" || exit 1
pass "export ${EXPORT_UUID} reached DONE"

# Snapshot data objects before the source volume is deleted: the assertion
# after the delete is that it removes none of them.
OBJ_BEFORE="$(count_objects "${SRC_LVS}/data/")"
info "source data objects before the delete: ${OBJ_BEFORE}"
check_target "step 2b" || exit 1

# ==========================================================================
# [3] delete the source volume -- the point of this test
#
# Done now, with the source lvstore still loaded and the export holding the
# snapshot: the export pins the *snapshot*, not the volume, so the delete is
# allowed, and blobstore merges the volume into its one clone (the snapshot).
# The destination's import below is verified against the same pattern A; if the
# merge had freed or re-mapped anything the snapshot references, the read would
# differ (or 404, see the TTL hazard documented in vbdev_s3lvol_lvstore.c).
# ==========================================================================
echo
echo "[3] deleting the source volume"

if ! raw_rpc rcow_delete_lvol "$(printf '{"lvol_name":"%s"}' "${SRC_VOL}")" \
		>"${WORKDIR}/del_src.json" 2>"${WORKDIR}/del_src.err"; then
	fail "rcow_delete_lvol (source volume)"
	sed 's/^/       /' "${WORKDIR}/del_src.err"
	check_target "step 3" || exit 1
	exit 1
fi
pass "source volume deleted"

# The snapshot it was deleted into must still be there, still exported.
SNAP_ST="$(snapshot_status_field "${SNAP_NAME}" export_status)"
if [ "${SNAP_ST}" = "DONE" ]; then
	pass "the snapshot is still exported (DONE) after the source volume is gone"
else
	fail "the snapshot reads '${SNAP_ST}' after the delete, expected DONE"
fi
check_target "step 3, after the delete" || exit 1

# The source's data objects survived -- assertion 2.
OBJ_AFTER="$(count_objects "${SRC_LVS}/data/")"
info "source data objects after the delete: ${OBJ_AFTER}"
if [ "${OBJ_AFTER}" -lt "${OBJ_BEFORE}" ]; then
	fail "the source delete removed data objects (${OBJ_BEFORE} -> ${OBJ_AFTER})"
	info "the snapshot references every cluster the volume wrote, so the"
	info "merge must have handed them to the snapshot, not freed them"
elif [ "${OBJ_AFTER}" -eq "${OBJ_BEFORE}" ]; then
	pass "no source data object was deleted with the volume (${OBJ_BEFORE} -> ${OBJ_AFTER})"
else
	info "object count grew (${OBJ_BEFORE} -> ${OBJ_AFTER}); the merge rewrote some, which is fine"
	pass "no source data object was lost with the volume"
fi
check_target "step 3, object count" || exit 1

# ==========================================================================
# [4] unload the source; the export is durable in S3, nothing needs the
#     lvstore loaded from here on.
# ==========================================================================
echo
echo "[4] unloading the source lvstore"

if ! raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" \
		>/dev/null 2>"${WORKDIR}/unload_src.err"; then
	fail "rcow_unload_lvstore (source)"
	sed 's/^/       /' "${WORKDIR}/unload_src.err"
	exit 1
fi
SRC_CREATED=0
pass "source lvstore unloaded"

# ==========================================================================
# [5] destination: lvstore + import + decouple + verify -- assertion 1
# ==========================================================================
echo
echo "[5] importing into the destination and verifying"

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

# decouple=true (the RPC's default, stated explicitly): the destination
# materialises the export's data in the background and the test waits for it.
# This is the configuration a caller uses when it wants the volume to outlive
# the source -- and the source volume is already gone at this point.
if ! raw_rpc rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","decouple":true}' \
		"${DST_VOL}" "${EXPORT_UUID}")" \
		>"${WORKDIR}/import.json" 2>"${WORKDIR}/import.err"; then
	fail "rcow_import_lvol"
	sed 's/^/       /' "${WORKDIR}/import.err"
	check_target "step 5" || exit 1
	exit 1
fi
pass "imported: $(cat "${WORKDIR}/import.json")"
check_target "step 5, importing" || exit 1

if ! wait_for_decouple; then
	fail "the decouple did not finish in 120s"
	check_target "step 5, decoupling" || exit 1
	exit 1
fi
pass "the imported volume decoupled"

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

# The decouple has materialised the data, so this read is against the
# destination's own objects. It has to match pattern A, written on the source
# and carried through export -> delete -> import: if the source-volume merge
# freed or re-mapped anything the snapshot referenced, this is where it shows.
if [ "$(read_md5_at "${IMPORT_DEV}" "${WORKDIR}/import_read.bin" \
		"${IO_OFF_MB}" "${IO_LEN_MB}")" = "${HASH_A}" ]; then
	pass "the decoupled imported volume reads pattern A -- the data survived the source delete"
else
	fail "the imported volume does not contain the exported data"
	info "the delete merged the source lvol into its snapshot; the snapshot's"
	info "objects must survive, and the import reads exactly those"
	info "everything above can pass with zeroes inside"
	exit 1
fi
check_target "step 5, reading the import" || exit 1
