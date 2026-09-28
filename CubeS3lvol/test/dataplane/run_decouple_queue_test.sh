#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  Regression test for issue 2: snapshotting an esnap clone whose decouple is
#  queued must not leave a decouple that cannot detach.
#
#  The field failure it pins down (64-node): a decouple that is queued behind a
#  slow full-volume materialisation does not hold action_in_progress, so a
#  snapshot of the volume was allowed -- and snapshotting an esnap clone moves
#  the external snapshot identity onto the new snapshot (spdk_bs_create_snapshot
#  clears it on the origin). The queued decouple then materialised everything
#  and failed its detach with "blob is not a clone of an external snapshot".
#
#  This test reproduces the queue window and asserts the fix. The fix was first a
#  refusal, and is now a cancellation: refusing was correct about the hazard but
#  made "import a volume, then snapshot it" impossible, since decouple defaults to
#  true and is started before the import replies. So create_snapshot cancels the
#  decouple and proceeds, which removes the hazard by removing the decouple. What
#  is asserted either way is that no decouple ever materialises everything and then
#  fails to detach.
#
#  Scenario:
#    src: a big volume written full -> snapshot -> export  (the decouple of its
#         import takes minutes, which is the queue window)
#    src: a small sparse volume -> snapshot -> export
#    dst: import big (decouple:true)   -- starts materialising, slowly
#    dst: import small (decouple:true) -- queued behind big
#    while big is ingesting: partial-write small's first parent-backed cluster
#         -- must CoW from small's export, never from big's active ingest
#    while small is queued: snapshot small -> cancels the queued decouple, succeeds
#    big's decouple must be unaffected, and small must afterwards be undecouplable
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

ctrl_of_nqn()
{
	local c

	for c in /sys/class/nvme/nvme*; do
		if [ "$(cat "${c}/subsysnqn" 2>/dev/null)" = "${NQN}" ]; then
			printf '/dev/%s' "$(basename "${c}")"
			return 0
		fi
	done
	return 1
}

# Wait for a 1 GiB namespace on this subsystem. Linux names /dev/nvmeXnY by
# discovery order, not NVMe nsid, so matching on size is what keeps hold0
# (4 MiB) and the volumes apart. exclude, if set, is skipped.
wait_vol_dev()
{
	local exclude="${1:-}"
	local deadline=$(( $(date +%s) + 25 ))
	local ctrl dev sz

	while [ "$(date +%s)" -lt "${deadline}" ]; do
		ctrl="$(ctrl_of_nqn)" || { sleep 0.2; continue; }
		nvme ns-rescan "${ctrl}" >/dev/null 2>&1 || true
		nvme_settle
		for dev in $(ls "${ctrl}"n* 2>/dev/null || true); do
			[ -b "${dev}" ] || continue
			[ -n "${exclude}" ] && [ "${dev}" = "${exclude}" ] && continue
			sz="$(blockdev --getsize64 "${dev}" 2>/dev/null || echo 0)"
			if [ "${sz}" = "${VOL_BYTES}" ]; then
				printf '%s' "${dev}"
				return 0
			fi
		done
		sleep 0.2
	done
	return 1
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
raw_rpc rcow_add_s3_config \
	"$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"%s}' \
		"${BUCKET}" "${ENDPOINT}" "${BUCKET}" "${REGION}" "${S3LVOL_EXTRA_JSON}")" >/dev/null 2>&1 \
	|| { fail "add_s3_config"; exit 1; }
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
# Keep one namespace alive while the source lvstore is unloaded. Otherwise the
# kernel is free to tear down the controller before the destination import is
# exposed, making the cross-volume CoW window impossible to drive from /dev.
${RPC} bdev_null_create hold0 4 4096 >/dev/null 2>&1 \
	|| { fail "bdev_null_create (controller hold)"; exit 1; }
HOLD_NSID=30
BIG_NSID=1
SMALL_NSID=2
VOL_BYTES=$((SMALL_GIB * 1024 * 1024 * 1024))
${RPC} nvmf_subsystem_add_ns "${NQN}" hold0 -n "${HOLD_NSID}" >/dev/null 2>&1 \
	|| { fail "nvmf_subsystem_add_ns (controller hold)"; exit 1; }
${RPC} nvmf_subsystem_add_ns "${NQN}" "${SRC_LVS}/${BIG_VOL}" \
	-n "${BIG_NSID}" >/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (big)"; exit 1; }
${RPC} nvmf_subsystem_add_listener "${NQN}" -t tcp -a "${LISTEN_ADDR}" \
	-s "${LISTEN_PORT}" >/dev/null 2>&1 || { fail "add_listener"; exit 1; }

if ! nvme connect -t tcp -a "${LISTEN_ADDR}" -s "${LISTEN_PORT}" -n "${NQN}" \
		>"${WORKDIR}/connect.log" 2>&1; then
	fail "nvme connect"
	sed 's/^/       /' "${WORKDIR}/connect.log"
	exit 1
fi
CONNECTED=1
BIG_DEV="$(wait_vol_dev)" \
	|| { fail "no 1 GiB device for big volume"; exit 1; }
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
	-n "${SMALL_NSID}" >/dev/null 2>&1 || { fail "nvmf_subsystem_add_ns (small)"; exit 1; }
SMALL_DEV="$(wait_vol_dev "${BIG_DEV}")" \
	|| { fail "no 1 GiB device for small volume"; exit 1; }
pass "small volume is ${SMALL_DEV}"

dd if=/dev/urandom of="${WORKDIR}/small.pat" bs=1M count="${SMALL_WRITE_MB}" \
	status=none
dd if="${WORKDIR}/small.pat" of="${SMALL_DEV}" bs=1M count="${SMALL_WRITE_MB}" \
	seek=0 oflag=direct conv=fsync status=none 2>"${WORKDIR}/small_write.err"
SMALL_SRC_MD5="$(dd if="${SMALL_DEV}" bs=1M count=1 iflag=direct status=none \
	| md5sum | cut -d' ' -f1)"
SMALL_PAT_MD5="$(dd if="${WORKDIR}/small.pat" bs=1M count=1 status=none \
	| md5sum | cut -d' ' -f1)"
if [ "${SMALL_SRC_MD5}" != "${SMALL_PAT_MD5}" ]; then
	fail "small source device did not retain the pattern (wrote to the wrong ns?)"
	exit 1
fi
raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" \
	>/dev/null 2>&1 || { fail "flush after writing small"; exit 1; }
pass "small volume written (${SMALL_WRITE_MB} MiB sparse)"

# Expected first MiB after the partial write made while this import is queued
# behind the big decouple. Only the first 4 KiB changes; the rest must still
# come from the small export's parent cluster.
dd if=/dev/urandom of="${WORKDIR}/cow.patch" bs=4096 count=1 status=none
dd if="${WORKDIR}/small.pat" of="${WORKDIR}/cow.expected" bs=1M count=1 status=none
dd if="${WORKDIR}/cow.patch" of="${WORKDIR}/cow.expected" bs=4096 count=1 \
	conv=notrunc status=none
COW_EXPECTED_MD5="$(md5sum "${WORKDIR}/cow.expected" | cut -d' ' -f1)"

raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
	"${SMALL_VOL}" "${SMALL_SNAP}")" >/dev/null 2>&1 \
	|| { fail "snapshot small"; exit 1; }
SMALL_EXP_UUID="$(raw_rpc rcow_export_snapshot \
	"$(printf '{"snapshot_name":"%s"}' "${SMALL_SNAP}")" \
	2>"${WORKDIR}/small_exp.err" | tr -d ' \t\r\n')"
[ -n "${SMALL_EXP_UUID}" ] || { fail "export small snapshot"; exit 1; }
wait_export_done "${SMALL_EXP_UUID}" "small export" || exit 1
pass "small snapshot exported (${SMALL_EXP_UUID})"

# Drop the source namespaces before unload so their nsids can be reused for the
# imported volume. hold0 stays, which is what keeps the controller alive.
${RPC} nvmf_subsystem_remove_ns "${NQN}" "${BIG_NSID}" >/dev/null 2>&1 || true
${RPC} nvmf_subsystem_remove_ns "${NQN}" "${SMALL_NSID}" >/dev/null 2>&1 || true

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
if grep -qE "Imported export .* as lvol '${SMALL_IMP}': .* 0 of [0-9]+ chunk" "${TGT_LOG}"; then
	fail "small import has no parent chunks; the CoW check would be vacuous"
	exit 1
fi

# ==========================================================================
echo "[7] while big ingests: CoW small must read small's export, not big's"
# Reuse the source small nsid. hold0 (nsid 30) keeps the controller up across
# the source unload, so a rescan is enough; reconnecting would also work.
SMALL_IMP_NSID="${SMALL_NSID}"
${RPC} nvmf_subsystem_add_ns "${NQN}" "${DST_LVS}/${SMALL_IMP}" \
	-n "${SMALL_IMP_NSID}" >/dev/null 2>"${WORKDIR}/add_small_imp_ns.err" || {
	fail "expose imported small"
	sed 's/^/       /' "${WORKDIR}/add_small_imp_ns.err"
	exit 1
}
SMALL_IMP_DEV="$(wait_vol_dev)" || {
	fail "imported small 1 GiB device did not appear"
	exit 1
}
BIG_STILL_RUNNING="$(raw_rpc rcow_get_decouple 2>/dev/null | python3 -c "
import json,sys
name = sys.argv[1]
try:
    rows = json.load(sys.stdin)
except Exception:
    rows = []
print(len([r for r in rows if r.get('lvol_name') == name and not r.get('queued')]))
" "${BIG_IMP}" 2>/dev/null || echo 0)"
if [ "${BIG_STILL_RUNNING}" != "1" ]; then
	fail "big decouple finished before the CoW; the cross-volume window was missed"
	exit 1
fi
pass "imported small is ${SMALL_IMP_DEV}; big ingest still running"

# A 4 KiB overwrite of a parent-backed 1 MiB chunk forces blobstore to preserve
# the other 1020 KiB through CoW. While the big CopyObject ingest is installed
# lvstore-wide, the historical bug resolved that read against the big manifest:
# the write either hung behind ingest slots or the untouched bytes silently
# came from the wrong export. Bound the write so a regression cannot stall CI.
if timeout 60 dd if="${WORKDIR}/cow.patch" of="${SMALL_IMP_DEV}" bs=4096 count=1 \
		oflag=direct conv=fsync status=none 2>"${WORKDIR}/cow_write.err"; then
	COW_GOT_MD5="$(dd if="${SMALL_IMP_DEV}" bs=1M count=1 iflag=direct \
		status=none 2>"${WORKDIR}/cow_read.err" | md5sum | cut -d' ' -f1)"
	if [ "${COW_GOT_MD5}" = "${COW_EXPECTED_MD5}" ]; then
		pass "queued small CoW preserved bytes from the small export"
	else
		fail "queued small CoW used wrong parent bytes (got ${COW_GOT_MD5}, expected ${COW_EXPECTED_MD5})"
	fi
else
	fail "queued small CoW failed or timed out while big ingest was active"
	sed 's/^/       /' "${WORKDIR}/cow_write.err"
fi

# ==========================================================================
echo "[8] while small is queued: snapshot it (must cancel the decouple and succeed)"
# This used to assert a refusal, and the refusal was correct as far as it went --
# letting the snapshot through while the decouple stood is what produced the
# detach failure this test is named for. But refusing makes "import a volume,
# then snapshot it" impossible, because decouple defaults to true and starts
# before the import replies. So the decouple is cancelled instead, and the
# snapshot proceeds; the hazard is gone because there is no longer a decouple to
# fail. See docs/import-reference-snapshot-design.md §3.1 and §9.2.
raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
	"${SMALL_IMP}" "${SMALL_IMP_SNAP}")" \
	>"${WORKDIR}/snap_small.json" 2>"${WORKDIR}/snap_small.err"
SNAP_RC=$?
if [ "${SNAP_RC}" -eq 0 ]; then
	pass "snapshot of queued small succeeded (rc=0)"
else
	fail "snapshot of queued small refused (rc=${SNAP_RC}) -- it must cancel the queued decouple instead"
	sed 's/^/    /' "${WORKDIR}/snap_small.err" 2>/dev/null | tail -5
fi

# The reply has to say the decouple was dropped: the caller asked for one, by
# default, and is not getting it.
if grep -q 'decouple_cancelled' "${WORKDIR}/snap_small.json" 2>/dev/null; then
	pass "reply reports decouple_cancelled"
else
	fail "reply does not report decouple_cancelled: $(tr -d '\n' < "${WORKDIR}/snap_small.json" 2>/dev/null | head -c 200)"
fi

# Cancelled from the queue, so it must say so -- and must not have started.
if grep -qE "'${SMALL_IMP}' was waiting to be decoupled .* is being snapshotted" "${TGT_LOG}"; then
	pass "queued decouple of ${SMALL_IMP} was dequeued for the snapshot"
else
	fail "no dequeue-for-snapshot line for ${SMALL_IMP} in the log"
fi
if grep -qE "Decoupling lvol '${SMALL_IMP}'" "${TGT_LOG}"; then
	fail "${SMALL_IMP} started materialising -- a queued cancel must stop it before that"
else
	pass "${SMALL_IMP} never started materialising"
fi

# ==========================================================================
echo "[9] wait for all decouples to finish"
if wait_for_decouple; then
	pass "all decouples finished"
else
	fail "decouple did not finish in time"
fi

# ==========================================================================
echo "[10] verdict"
echo "--- relevant log lines:"
grep -nE 'queued to be decoupled|decouple_start|decouple_finish|Decoupling|blob is not a clone' \
	"${TGT_LOG}" | tail -40 | sed 's/^/    /' || true

# The point of the whole test. Whether the snapshot is refused or the decouple is
# cancelled, what must never appear is a decouple that materialised everything and
# then could not detach.
if grep -qE 'blob is not a clone of an external snapshot' "${TGT_LOG}"; then
	fail "decouple detach failed -- the fix did not hold"
else
	pass "no detach failure: every decouple detached cleanly"
fi

# big was never snapshotted, so its decouple must still have run to completion --
# cancelling one volume's decouple must not disturb another's.
if grep -qE "'${BIG_IMP}' no longer reads export" "${TGT_LOG}"; then
	pass "${BIG_IMP} decoupled cleanly, unaffected by the cancellation"
else
	fail "${BIG_IMP} did not finish its decouple"
fi

# small's decouple was cancelled, so it must still read the export -- and asking
# again must be refused, because the snapshot now owns the external parent.
raw_rpc rcow_decouple_lvol "$(printf '{"lvol_name":"%s"}' "${SMALL_IMP}")" \
	>/dev/null 2>"${WORKDIR}/redecouple.err"
if [ $? -ne 0 ]; then
	pass "${SMALL_IMP} can no longer be decoupled (its snapshot holds the parent)"
else
	fail "${SMALL_IMP} accepted a decouple after being snapshotted -- it has no external parent to clear"
fi

check_target "step 9" || exit 1

echo "--- log kept at: ${TGT_LOG}"
