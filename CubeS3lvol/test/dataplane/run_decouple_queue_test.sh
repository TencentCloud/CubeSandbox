#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  Regression test for issue 2: an esnap clone that queued a decouple must not
#  be snapshotable while it waits.
#
#  The field failure it pins down (64-node): a decouple that is queued behind a
#  slow full-volume materialisation does not hold action_in_progress, so a
#  snapshot of the volume was allowed -- and snapshotting an esnap clone moves
#  the external snapshot identity onto the new snapshot (spdk_bs_create_snapshot
#  clears it on the origin). The queued decouple then materialised everything
#  and failed its detach with "blob is not a clone of an external snapshot".
#
#  This test reproduces the queue window and asserts the fix: the snapshot is
#  refused (derive_check sees the volume in the decouple queue), and the
#  decouple afterwards completes cleanly.
#
#  Scenario:
#    src: a big volume written full -> snapshot -> export  (the decouple of its
#         import takes minutes, which is the queue window)
#    src: a small sparse volume -> snapshot -> export
#    dst: import big (decouple:true)   -- starts materialising, slowly
#    dst: import small (decouple:true) -- queued behind big
#    while small is queued: snapshot small -> must be refused
#
#  Usage:
#    sudo -E ./test/dataplane/repro_issue2.sh -e <endpoint> -b <bucket> [-r <region>]
#
#  Credentials from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.
#  Needs root, nvme-cli, a writable /data.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TOOLS_DIR="${REPO_ROOT}/test/tools"

TGT_BIN="${REPO_ROOT}/app/s3lvol_tgt/s3lvol_tgt"
RPC_PY="${SPDK_ROOT:-${REPO_ROOT}/deps/spdk}/scripts/rpc.py"
RPC="${RPC_PY} -s /var/run/s3lvol.sock"
RPC_SOCK="/var/run/s3lvol.sock"

S3_EXPORTS_DIR="exports"

SRC_LVS="r2src"
DST_LVS="r2dst"

BIG_VOL="big0"
SMALL_VOL="small0"
BIG_SNAP="big0-snap"
SMALL_SNAP="small0-snap"
BIG_IMP="big0-imp"
SMALL_IMP="small0-imp"
SMALL_IMP_SNAP="small0-imp-snap"

CAPACITY_GIB=8
BIG_GIB=1          # full volume: written end to end
SMALL_GIB=1        # sparse: only SMALL_WRITE_MB written
SMALL_WRITE_MB=16
JOURNAL_MB=64
WAL_MB=128
WAL_FILE_MB=$((JOURNAL_MB + WAL_MB + 128))

SRC_WAL_FILE="/data/r2_src.img"
DST_WAL_FILE="/data/r2_dst.img"
SRC_WAL_BDEV="r2_src_wal0"
DST_WAL_BDEV="r2_dst_wal0"

NQN="nqn.2026-08.io.spdk:r2"
LISTEN_ADDR="127.0.0.1"
LISTEN_PORT="4420"

ENDPOINT=""
BUCKET=""
REGION="ap-nanjing"

PASS=0
FAIL=0
TGT_PID=""
TGT_LOG=""
WORKDIR=""
SRC_CREATED=0
DST_CREATED=0
WAL_FILES_CREATED=0
CONNECTED=0
TRANSPORT_READY=0
TEARDOWN_ANOMALY=0
BIG_EXP_UUID=""
SMALL_EXP_UUID=""

pass() { PASS=$((PASS + 1)); echo "[PASS] $*"; }
fail() { FAIL=$((FAIL + 1)); echo "[FAIL] $*"; }
info() { echo "---- $*"; }

usage()
{
	cat <<EOF
Usage: $0 -e <endpoint> -b <bucket> [-r <region>]

  -e   S3/COS endpoint host
  -b   bucket (a test bucket: a clean run deletes its own prefixes)
  -r   region (default: ${REGION})
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
[ -n "${ENDPOINT}" ] && [ -n "${BUCKET}" ] || { usage; exit 1; }
[ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] || {
	echo "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY must be set" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }
for tool in nvme dd md5sum python3; do
	command -v "${tool}" >/dev/null 2>&1 || {
		echo "${tool} is required" >&2; exit 1; }
done

WORKDIR="$(mktemp -d /tmp/r2.XXXXXX)"
TGT_LOG="${WORKDIR}/target.log"

raw_rpc()
{
	python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" "$1" "${2:-}"
}

wait_export_done()
{
	local uuid="$1" where="$2"
	local deadline=$(( $(date +%s) + 120 ))
	local out st

	while :; do
		if ! out="$(raw_rpc rcow_get_snapshot_status \
				"$(printf '{"export_uuid":"%s"}' "${uuid}")" 2>/dev/null)"; then
			fail "export ${uuid} finished without a manifest (${where})"
			return 1
		fi
		st="$(printf '%s' "${out}" \
			| python3 -c 'import json,sys; print(json.load(sys.stdin).get("export_status",""))' \
				2>/dev/null)"
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
	return 0
}

nvme_settle()
{
	udevadm settle --timeout=5 >/dev/null 2>&1 || true
}

remove_prefix()
{
	python3 "${TOOLS_DIR}/s3_prefix_rm.py" -e "${ENDPOINT}" -b "${BUCKET}" \
		-r "${REGION}" -p "$1"
}

# Decouple is considered done when the decouple list is empty.
wait_for_decouple()
{
	local _i
	for _i in $(seq 600); do
		if ! raw_rpc rcow_get_decouple "" >"${WORKDIR}/decouple.json" 2>/dev/null; then
			return 1
		fi
		if python3 -c "
import json, sys
sys.exit(0 if len(json.load(open(sys.argv[1]))) == 0 else 1)
" "${WORKDIR}/decouple.json"; then
			return 0
		fi
		sleep 1
	done
	return 1
}

cleanup()
{
	local rc=$?

	echo
	echo "=== cleanup ==="

	if [ "${CONNECTED}" -eq 1 ]; then
		nvme_settle
		nvme disconnect -n "${NQN}" >/dev/null 2>&1 || true
		CONNECTED=0
	fi

	if target_alive; then
		if [ "${TRANSPORT_READY}" -eq 1 ]; then
			${RPC} nvmf_delete_subsystem "${NQN}" >/dev/null 2>&1 || true
		fi
		for lvs in "${DST_LVS}" "${SRC_LVS}"; do
			raw_rpc rcow_delete_lvstore \
				"$(printf '{"lvs_name":"%s"}' "${lvs}")" >/dev/null 2>&1 || true
		done
		kill -TERM "${TGT_PID}" 2>/dev/null
		for _ in $(seq 50); do
			kill -0 "${TGT_PID}" 2>/dev/null || break
			sleep 0.1
		done
		kill -KILL "${TGT_PID}" 2>/dev/null
	fi

	if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
		for prefix in "${SRC_LVS}/" "${DST_LVS}/" "${S3_EXPORTS_DIR}/"; do
			remove_prefix "${prefix}" >"${WORKDIR}/s3_rm.log" 2>&1 || true
		done
	fi
	if [ "${WAL_FILES_CREATED}" -eq 1 ]; then
		rm -f "${SRC_WAL_FILE}" "${DST_WAL_FILE}"
	fi

	echo "--- target log: ${TGT_LOG}"
	echo
	echo "=== result: ${PASS} passed, ${FAIL} failed ==="
	[ "${FAIL}" -eq 0 ] || exit 1
}
trap cleanup EXIT

# ==========================================================================
echo "[0] preconditions"
[ -x "${TGT_BIN}" ] || { echo "target not built" >&2; exit 1; }
if pgrep -x s3lvol_tgt >/dev/null 2>&1; then
	echo "another s3lvol_tgt is running; stop it first" >&2
	exit 1
fi
pass "preconditions"

# ==========================================================================
echo "[1] starting the target"
rm -f "${SRC_WAL_FILE}" "${DST_WAL_FILE}"
truncate -s "${WAL_FILE_MB}M" "${SRC_WAL_FILE}"
truncate -s "${WAL_FILE_MB}M" "${DST_WAL_FILE}"
WAL_FILES_CREATED=1

"${TGT_BIN}" -m "${S3LVOL_TGT_CPUMASK:-0x3}" --no-huge -s 2048 -r "${RPC_SOCK}" \
	>"${TGT_LOG}" 2>&1 &
TGT_PID=$!

for _ in $(seq 80); do
	[ -e "${RPC_SOCK}" ] && break
	kill -0 "${TGT_PID}" 2>/dev/null || { echo "target died on start" >&2; exit 1; }
	sleep 0.1
done
[ -e "${RPC_SOCK}" ] || { echo "target never opened ${RPC_SOCK}" >&2; exit 1; }
sleep 1
pass "target is up (pid ${TGT_PID})"

${RPC} bdev_aio_create "${SRC_WAL_FILE}" "${SRC_WAL_BDEV}" 4096 >/dev/null 2>&1 || {
	fail "bdev_aio_create (source)"; exit 1; }
${RPC} bdev_aio_create "${DST_WAL_FILE}" "${DST_WAL_BDEV}" 4096 >/dev/null 2>&1 || {
	fail "bdev_aio_create (destination)"; exit 1; }
pass "two local devices attached"

# ==========================================================================
echo "[2] source lvstore + big volume, written full"
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
raw_rpc rcow_add_cos_config \
	"$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"%s}' \
		"${BUCKET}" "${ENDPOINT}" "${BUCKET}" "${REGION}" "${S3LVOL_EXTRA_JSON}")" >/dev/null 2>&1 \
	|| { fail "add_cos_config"; exit 1; }
raw_rpc rcow_create_lvstore \
	"$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"%s","journal_size_mb":%d,"wal_size_mb":%d}' \
		"${SRC_LVS}" "${BUCKET}" "${CAPACITY_GIB}" "${SRC_WAL_BDEV}" \
		"${JOURNAL_MB}" "${WAL_MB}")" \
	>"${WORKDIR}/src_lvs.json" 2>"${WORKDIR}/src_lvs.err" \
	|| { fail "src create_lvstore"; sed 's/^/       /' "${WORKDIR}/src_lvs.err"; exit 1; }
SRC_CREATED=1
pass "src lvstore ${SRC_LVS} created"

raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
	"${BIG_VOL}" "${BIG_GIB}")" >/dev/null 2>&1 \
	|| { fail "create big lvol"; exit 1; }

${RPC} nvmf_create_transport -t TCP >/dev/null 2>&1 || true
${RPC} nvmf_create_subsystem "${NQN}" -a -s R2X0000000000000001 \
	>/dev/null 2>&1 || { fail "nvmf_create_subsystem"; exit 1; }
TRANSPORT_READY=1
${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${BIG_VOL}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (big)"; exit 1; }
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
BIG_DEV=""
for _ in $(seq 30); do
	BIG_DEV="$(comm -13 <(echo "${BEFORE_CONNECT}") \
			<(ls /dev/nvme*n* 2>/dev/null | sort || true) | head -1)"
	[ -n "${BIG_DEV}" ] && break
	sleep 0.5
done
[ -n "${BIG_DEV}" ] || { fail "no device for big volume"; exit 1; }
pass "big volume is ${BIG_DEV}"

info "writing ${BIG_GIB}GiB to ${BIG_VOL} (this is the slow part)"
dd if=/dev/urandom of="${WORKDIR}/big.pat" bs=1M count=$((BIG_GIB * 1024)) \
	status=none
dd if="${WORKDIR}/big.pat" of="${BIG_DEV}" bs=1M count=$((BIG_GIB * 1024)) \
	oflag=direct conv=fsync status=none 2>"${WORKDIR}/big_write.err"
pass "big volume written full"

raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
	"${BIG_VOL}" "${BIG_SNAP}")" >/dev/null 2>&1 \
	|| { fail "snapshot big"; exit 1; }
BIG_EXP_UUID="$(raw_rpc rcow_export_snapshot \
	"$(printf '{"snapshot_name":"%s"}' "${BIG_SNAP}")" \
	2>"${WORKDIR}/big_exp.err" | tr -d ' \t\r\n')"
[ -n "${BIG_EXP_UUID}" ] || { fail "export big snapshot"; exit 1; }
wait_export_done "${BIG_EXP_UUID}" "big export" || exit 1
pass "big snapshot exported (${BIG_EXP_UUID})"

# ==========================================================================
echo "[3] source small volume, sparse"
raw_rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' \
	"${SMALL_VOL}" "${SMALL_GIB}")" >/dev/null 2>&1 \
	|| { fail "create small lvol"; exit 1; }
${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${SMALL_VOL}" \
	>/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (small)"; exit 1; }
nvme ns-rescan "/dev/$(basename "${BIG_DEV}" | sed 's/n[0-9]*$//')" \
	>/dev/null 2>&1 || true
SMALL_DEV=""
for _ in $(seq 30); do
	SMALL_DEV="$(comm -13 <(echo "${BEFORE_CONNECT}") \
			<(ls /dev/nvme*n* 2>/dev/null | sort || true) | grep -v "${BIG_DEV}" \
			| head -1)"
	[ -n "${SMALL_DEV}" ] && break
	sleep 0.5
done
[ -n "${SMALL_DEV}" ] || { fail "no device for small volume"; exit 1; }
pass "small volume is ${SMALL_DEV}"

dd if=/dev/urandom of="${WORKDIR}/small.pat" bs=1M count="${SMALL_WRITE_MB}" \
	status=none
dd if="${WORKDIR}/small.pat" of="${SMALL_DEV}" bs=1M count="${SMALL_WRITE_MB}" \
	seek=0 oflag=direct conv=fsync status=none 2>"${WORKDIR}/small_write.err"
pass "small volume written (${SMALL_WRITE_MB} MiB sparse)"

raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
	"${SMALL_VOL}" "${SMALL_SNAP}")" >/dev/null 2>&1 \
	|| { fail "snapshot small"; exit 1; }
SMALL_EXP_UUID="$(raw_rpc rcow_export_snapshot \
	"$(printf '{"snapshot_name":"%s"}' "${SMALL_SNAP}")" \
	2>"${WORKDIR}/small_exp.err" | tr -d ' \t\r\n')"
[ -n "${SMALL_EXP_UUID}" ] || { fail "export small snapshot"; exit 1; }
wait_export_done "${SMALL_EXP_UUID}" "small export" || exit 1
pass "small snapshot exported (${SMALL_EXP_UUID})"

# One blobstore per node: the source lvstore has to be unloaded before the
# destination can be created. Its export lives in S3, so the import below still
# reads through it.
raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" \
	>/dev/null 2>"${WORKDIR}/unload_src.err" \
	|| { fail "unload src lvstore"; sed 's/^/       /' "${WORKDIR}/unload_src.err"; exit 1; }
SRC_CREATED=0
pass "src lvstore unloaded (exports remain in S3)"

# ==========================================================================
echo "[4] destination lvstore"
# The namespace was registered for the source; one registration per bucket.
raw_rpc rcow_create_lvstore \
	"$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"%s","journal_size_mb":%d,"wal_size_mb":%d}' \
		"${DST_LVS}" "${BUCKET}" "${CAPACITY_GIB}" "${DST_WAL_BDEV}" \
		"${JOURNAL_MB}" "${WAL_MB}")" \
	>"${WORKDIR}/dst_lvs.json" 2>"${WORKDIR}/dst_lvs.err" \
	|| { fail "dst create_lvstore"; sed 's/^/       /' "${WORKDIR}/dst_lvs.err"; exit 1; }
DST_CREATED=1
pass "dst lvstore ${DST_LVS} created"

# ==========================================================================
echo "[5] import big with decouple:true (starts materialising)"
if ! raw_rpc rcow_import_lvol \
		"$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
			"${BIG_IMP}" "${BIG_EXP_UUID}" "${DST_LVS}")" \
		>"${WORKDIR}/import_big.json" 2>"${WORKDIR}/import_big.err"; then
	fail "import big"
	sed 's/^/       /' "${WORKDIR}/import_big.err"
	exit 1
fi
pass "big imported, decoupling in background"

# Give the big decouple a moment to grab the queue.
sleep 3

# ==========================================================================
echo "[6] import small with decouple:true (queued behind big)"
if ! raw_rpc rcow_import_lvol \
		"$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
			"${SMALL_IMP}" "${SMALL_EXP_UUID}" "${DST_LVS}")" \
		>"${WORKDIR}/import_small.json" 2>"${WORKDIR}/import_small.err"; then
	fail "import small"
	sed 's/^/       /' "${WORKDIR}/import_small.err"
	exit 1
fi
pass "small imported, decouple queued behind big"

# ==========================================================================
echo "[7] while small is queued: snapshot it (must be refused)"
raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
	"${SMALL_IMP}" "${SMALL_IMP_SNAP}")" \
	>"${WORKDIR}/snap_small.json" 2>"${WORKDIR}/snap_small.err"
SNAP_RC=$?
if [ "${SNAP_RC}" -ne 0 ]; then
	pass "snapshot of queued small refused (rc=${SNAP_RC})"
	if ! grep -qE "is queued to be decoupled" "${TGT_LOG}"; then
		fail "no 'queued to be decoupled' refusal reason in the log"
	fi
else
	fail "snapshot of queued small succeeded -- a queued esnap clone must not be snapshotable"
fi

# ==========================================================================
echo "[8] wait for all decouples to finish"
if wait_for_decouple; then
	pass "all decouples finished"
else
	fail "decouple did not finish in time"
fi

# ==========================================================================
echo "[9] verdict"
echo "--- relevant log lines:"
grep -nE 'queued to be decoupled|decouple_start|decouple_finish|Decoupling|blob is not a clone' \
	"${TGT_LOG}" | tail -40 | sed 's/^/    /' || true

if grep -qE 'blob is not a clone of an external snapshot' "${TGT_LOG}"; then
	fail "decouple detach failed -- the fix did not hold"
else
	pass "no detach failure: every decouple detached cleanly"
fi
if grep -qE "Decoupling lvol '${SMALL_IMP}'" "${TGT_LOG}" &&
   grep -qE "'${SMALL_IMP}' no longer reads export" "${TGT_LOG}"; then
	pass "${SMALL_IMP} decoupled cleanly after the queued snapshot was refused"
else
	fail "${SMALL_IMP} did not finish its decouple"
fi

check_target "step 9" || exit 1

echo "--- log kept at: ${TGT_LOG}"
