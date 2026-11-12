#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  Snapshotting an imported volume must not disturb the hidden template's
#  decouple.
#
#  An import with decouple:true materialises __rcow_tpl_<uuid>, not the user
#  volume. The user volume is a clone of that template, so snapshotting it does
#  not move the esnap and does not cancel the copy. The old hazard -- snapshot
#  steals the external parent, then the decouple fails to detach -- does not
#  arise on this path.
#
#  What must hold afterwards:
#    - the snapshot exists, and the reply does not say decouple_cancelled
#    - the template decouple runs to completion, with no detach failure
#    - the snapshot and the volume both read the source's bytes in this process
#    - a further decouple is refused: the template is no longer an esnap
#
#  Scenario:
#    src: a volume written full -> snapshot -> export  (a slow decouple, so the
#         snapshot lands while the copy is still running)
#    dst: import it with decouple:true, assert the template is materialising
#    dst: snapshot the import -> must succeed without cancelling that copy
#
#  Usage:
#    sudo -E ./test/dataplane/run_snapshot_cancel_test.sh -e <endpoint> -b <bucket> [-r <region>]
#
#  Credentials from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.
#  Needs root, nvme-cli, a writable /data.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TOOLS_DIR="${REPO_ROOT}/test/tools"

TGT_BIN="${REPO_ROOT}/app/s3lvol_tgt/s3lvol_tgt"
RPC_PY="${SPDK_ROOT:-${REPO_ROOT}/deps/spdk}/scripts/rpc.py"
RPC="${RPC_PY} -s /var/run/s3lvol_sc.sock"
RPC_SOCK="/var/run/s3lvol_sc.sock"

S3_EXPORTS_DIR="exports"

SRC_LVS="scsrc"
DST_LVS="scdst"

BIG_VOL="big0"
BIG_SNAP="big0-snap"
BIG_IMP="big0-imp"
BIG_IMP_SNAP="big0-imp-snap"

CAPACITY_GIB=8
BIG_GIB=1          # written end to end, so its decouple is slow enough to catch
JOURNAL_MB=64
WAL_MB=128
WAL_FILE_MB=$((JOURNAL_MB + WAL_MB + 128))

SRC_WAL_FILE="/data/sc_src.img"
DST_WAL_FILE="/data/sc_dst.img"
SRC_WAL_BDEV="sc_src_wal0"
DST_WAL_BDEV="sc_dst_wal0"

NQN="nqn.2026-08.io.spdk:sc"
LISTEN_ADDR="127.0.0.1"
LISTEN_PORT="4421"

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

remove_ns()
{
	python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" --raw \
		nvmf_subsystem_remove_ns \
		"$(printf '{"nqn":"%s","nsid":%s}' "${NQN}" "$1")" >/dev/null 2>&1 || true
}

# md5 of the first BIG_GIB of one volume, by name.
#
# Exposed and withdrawn around each read rather than kept: the namespaces are all
# on one subsystem, so a volume added while connected shows up as another
# /dev/nvmeXnY, and the set difference is how it is found -- the same way the
# source device is located above.
read_vol_md5()
{
	local name="$1" before dev nsid _i

	before="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"

	# --raw, because for nvmf_subsystem_add_ns the result *is* the nsid, and
	# SPDK's own rpc.py prints nothing for it -- which is fine for the callers
	# that only check the exit code, but this one has to give the namespace back
	# afterwards.
	nsid="$(python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" --raw \
		nvmf_subsystem_add_ns \
		"$(printf '{"nqn":"%s","namespace":{"bdev_name":"%s"}}' \
			"${NQN}" "${DST_LVS}/${name}")" \
		2>"${WORKDIR}/addns_${name}.err" | tr -d '[:space:]')"
	if [ -z "${nsid}" ]; then
		{
			echo "add_ns for ${DST_LVS}/${name} produced no nsid"
			echo "stderr:"; sed 's/^/  /' "${WORKDIR}/addns_${name}.err"
			echo "devices: $(ls /dev/nvme*n* 2>/dev/null | tr '\n' ' ')"
		} >>"${WORKDIR}/read_vol_diag.txt" 2>&1
		return 1
	fi

	dev=""
	for _i in $(seq 40); do
		dev="$(comm -13 <(echo "${before}") \
			<(ls /dev/nvme*n* 2>/dev/null | sort || true) | head -1)"
		[ -n "${dev}" ] && [ -b "${dev}" ] && break
		dev=""
		sleep 0.5
	done
	if [ -z "${dev}" ]; then
		{
			echo "no new device for ${name} (nsid ${nsid})"
			echo "before: ${before}"
			echo "after:  $(ls /dev/nvme*n* 2>/dev/null | tr '\n' ' ')"
			echo "controllers:"
			for c in /sys/class/nvme/nvme*; do
				[ -e "${c}/subsysnqn" ] && echo "  $(basename "${c}") $(cat "${c}/subsysnqn")"
			done
		} >>"${WORKDIR}/read_vol_diag.txt" 2>&1
		remove_ns "${nsid}"
		return 1
	fi

	dd if="${dev}" bs=1M count=$((BIG_GIB * 1024)) iflag=direct status=none \
		| md5sum | cut -d' ' -f1
	remove_ns "${nsid}"
	sleep 1
	return 0
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

# What every later read is compared against. Taken from the device rather than the
# pattern file so that a source-side write bug fails here rather than looking like
# a decouple bug three steps later.
SRC_MD5="$(dd if="${BIG_DEV}" bs=1M count=$((BIG_GIB * 1024)) iflag=direct \
	status=none | md5sum | cut -d' ' -f1)"
info "source content ${SRC_MD5}"
if [ "${SRC_MD5}" = "$(md5sum "${WORKDIR}/big.pat" | cut -d' ' -f1)" ]; then
	pass "source reads back what was written"
else
	fail "source does not read back what was written"
fi

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
echo "[3] destination lvstore"
# One blobstore per node: the source has to be unloaded before the destination can
# be created. Its export lives in S3, so the import below still reads through it --
# and the device the source was read from is gone from here on, which is why
# SRC_MD5 was taken while it was still attached.
nvme_settle
remove_ns 1
raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${SRC_LVS}")" \
	>/dev/null 2>"${WORKDIR}/unload_src.err" \
	|| { fail "unload src lvstore"; sed 's/^/       /' "${WORKDIR}/unload_src.err"; exit 1; }
SRC_CREATED=0
pass "src lvstore unloaded (its export remains in S3)"

if ! raw_rpc rcow_create_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"%s","journal_size_mb":%d,"wal_size_mb":%d,"force":true}' \
	"${DST_LVS}" "${BUCKET}" "${CAPACITY_GIB}" "${DST_WAL_BDEV}" "${JOURNAL_MB}" "${WAL_MB}")" \
	>/dev/null; then
	fail "could not create ${DST_LVS}"
	exit 1
fi
DST_CREATED=1
pass "destination lvstore ready"

# ==========================================================================
echo "[4] import with decouple:true, and catch it while it runs"
if ! raw_rpc rcow_import_lvol "$(printf '{"lvol_name":"%s","export_uuid":"%s","lvs_name":"%s","decouple":true}' \
	"${BIG_IMP}" "${BIG_EXP_UUID}" "${DST_LVS}")" >/dev/null; then
	fail "import of ${BIG_IMP} failed"
	exit 1
fi

# The copy is on the hidden template, not on the user volume. decouple_start()
# runs before the import replies.
TPL="__rcow_tpl_${BIG_EXP_UUID}"
# import_start_shared_decouple() names the template itself, so the log is
# "Decoupling lvol '__rcow_tpl_...'" and not the user-lvol redirect line.
if ! grep -aqF "Decoupling lvol '${TPL}'" "${TGT_LOG}"; then
	fail "${TPL} is not materialising -- this test needs a running decouple"
	exit 1
fi
pass "imported ${BIG_IMP}; decouple is running on ${TPL}"

# Land the snapshot while that copy is still in progress.
sleep 5

# ==========================================================================
echo "[5] snapshot the user volume; that must not cancel the template"
raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
	"${BIG_IMP}" "${BIG_IMP_SNAP}")" \
	>"${WORKDIR}/snap.json" 2>"${WORKDIR}/snap.err"
SNAP_RC=$?
if [ "${SNAP_RC}" -eq 0 ]; then
	pass "snapshot succeeded while the template decouple was running (rc=0)"
else
	fail "snapshot refused (rc=${SNAP_RC})"
	sed 's/^/    /' "${WORKDIR}/snap.err" 2>/dev/null | tail -5
fi

if grep -q 'decouple_cancelled' "${WORKDIR}/snap.json" 2>/dev/null; then
	fail "snapshot reported decouple_cancelled; the template copy must keep going"
	tr -d '\n' < "${WORKDIR}/snap.json" 2>/dev/null | head -c 200 | sed 's/^/    /'
else
	pass "snapshot did not cancel the template decouple"
fi

if grep -qE "Cancelling the decouple of lvol '${TPL}'" "${TGT_LOG}"; then
	fail "snapshot cancelled the template decouple"
else
	pass "the template decouple was not cancelled"
fi

# ==========================================================================
echo "[6] the template decouple finishes without a detach failure"
if wait_for_decouple; then
	pass "template decouple finished"
else
	fail "template decouple still running"
fi

if grep -qF "'${TPL}' no longer reads export" "${TGT_LOG}"; then
	pass "template reports it no longer reads the export"
else
	fail "no 'no longer reads export' line for ${TPL}"
fi

if grep -qE "Decoupling lvol '${TPL}' from export .* was cancelled after" "${TGT_LOG}"; then
	fail "template decouple reported itself cancelled"
else
	pass "template decouple was not cancelled"
fi

if grep -qE 'blob is not a clone of an external snapshot' "${TGT_LOG}"; then
	fail "detach failure present"
else
	pass "no detach failure"
fi

# ==========================================================================
echo "[7] the snapshot exists, and the template stays hidden"
if raw_rpc rcow_get_lvol "$(printf '{"lvol_name":"%s"}' "${BIG_IMP_SNAP}")" \
	>/dev/null 2>"${WORKDIR}/get_snap.err"; then
	pass "snapshot ${BIG_IMP_SNAP} is visible"
else
	fail "snapshot ${BIG_IMP_SNAP} is not visible"
	sed 's/^/    /' "${WORKDIR}/get_snap.err" 2>/dev/null | tail -3
fi
if raw_rpc rcow_get_lvol "$(printf '{"lvol_name":"%s"}' "${TPL}")" \
	>/dev/null 2>"${WORKDIR}/get_tpl.err"; then
	fail "hidden template ${TPL} was listed"
else
	pass "hidden template is not visible"
fi

# ==========================================================================
echo "[8] a further decouple is refused: the template is no longer an esnap"
if raw_rpc rcow_decouple_lvol "$(printf '{"lvol_name":"%s"}' "${BIG_IMP}")" \
	>/dev/null 2>"${WORKDIR}/redecouple.err"; then
	fail "${BIG_IMP} accepted another decouple after the template finished"
else
	pass "${BIG_IMP} refuses a further decouple"
fi

# ==========================================================================
echo "[9] data: the snapshot and the volume both read the source's bytes"
SNAP_MD5="$(read_vol_md5 "${BIG_IMP_SNAP}")" || SNAP_MD5="unreadable"
VOL_MD5="$(read_vol_md5 "${BIG_IMP}")" || VOL_MD5="unreadable"
info "source   ${SRC_MD5}"
info "snapshot ${SNAP_MD5}"
info "volume   ${VOL_MD5}"
if [ "${SNAP_MD5}" = "${SRC_MD5}" ]; then
	pass "snapshot reads the source's bytes"
else
	fail "snapshot content differs from the source"
fi
if [ "${VOL_MD5}" = "${SRC_MD5}" ]; then
	pass "volume still reads the source's bytes"
else
	fail "volume content differs from the source"
fi

check_target "step 9" || exit 1

echo "--- log kept at: ${TGT_LOG}"
