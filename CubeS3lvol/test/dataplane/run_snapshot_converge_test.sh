#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  A reference chain must be able to stop depending on its source export.
#
#  Snapshotting an imported volume moves the external snapshot onto the snapshot
#  (measured, docs/import-reference-snapshot-design.md 9.2). That left nothing to
#  decouple: the volume no longer reads the export, and a read-only snapshot could
#  not be materialised -- so the chain referenced its source until it was deleted,
#  which ties the data's survival to the importer's uptime, since a lease going
#  stale is what licenses an unattended delete on the source.
#
#  Decoupling a snapshot is what closes that. This asserts it end to end, and the
#  assertion that matters is the last one: the source's objects are deleted from S3
#  and the lvstore is unloaded and re-attached before the final read, so a pass
#  cannot come from a cache.
#
#  Scenario:
#    src: a volume written full -> snapshot -> export
#    dst: import it, snapshot it (which cancels the import's decouple), clone the
#         snapshot, write to the clone
#    dst: decouple the snapshot, while a reader loops on the clone
#    then: erase the source prefix, reload, and read everything back
#
#  Usage:
#    sudo -E ./test/dataplane/run_snapshot_converge_test.sh -e <endpoint> -b <bucket> [-r <region>]
#
#  Credentials from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.
#  Needs root, nvme-cli, a writable /data.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TOOLS_DIR="${REPO_ROOT}/test/tools"

TGT_BIN="${REPO_ROOT}/app/s3lvol_tgt/s3lvol_tgt"
RPC_PY="${SPDK_ROOT:-${REPO_ROOT}/deps/spdk}/scripts/rpc.py"
RPC="${RPC_PY} -s /var/run/s3lvol_cv.sock"
RPC_SOCK="/var/run/s3lvol_cv.sock"

S3_EXPORTS_DIR="exports"

SRC_LVS="cvsrc"
DST_LVS="cvdst"

BIG_VOL="big0"
BIG_SNAP="big0-snap"
BIG_IMP="big0-imp"
BIG_IMP_SNAP="big0-imp-snap"
BIG_IMP_CLONE="big0-imp-clone"

CAPACITY_GIB=8
BIG_GIB=1          # written end to end, so its decouple is slow enough to catch
CLONE_WRITE_MB=32  # the clone's own clusters, so its writes are covered too
JOURNAL_MB=64
WAL_MB=128
WAL_FILE_MB=$((JOURNAL_MB + WAL_MB + 128))

SRC_WAL_FILE="/data/cv_src.img"
DST_WAL_FILE="/data/cv_dst.img"
SRC_WAL_BDEV="cv_src_wal0"
DST_WAL_BDEV="cv_dst_wal0"

NQN="nqn.2026-08.io.spdk:cv"
LISTEN_ADDR="127.0.0.1"
LISTEN_PORT="4422"

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
CLONE_NSID=""
READER_PID=""
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

	# The reader loops on a device that is about to be taken away, so it has to
	# stop before the namespace goes -- including when the run exits early.
	if [ -n "${READER_PID}" ]; then
		touch "${WORKDIR}/reader_stop" 2>/dev/null || true
		wait "${READER_PID}" 2>/dev/null || true
		READER_PID=""
	fi
	if [ -n "${CLONE_NSID}" ]; then
		remove_ns "${CLONE_NSID}"
		CLONE_NSID=""
	fi

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

# It must be *running*, not queued -- that is the branch this test exists for.
# decouple_start() is called before the import replies, so this should already
# hold; asserted rather than assumed, because a queued one would quietly turn this
# into a duplicate of run_decouple_queue_test.sh.
if ! grep -qE "Decoupling lvol '${BIG_IMP}'" "${TGT_LOG}"; then
	fail "${BIG_IMP} is not materialising -- this test needs a running decouple"
	exit 1
fi
pass "${BIG_IMP} imported, decouple running"

# Let it get somewhere, so the cancellation has clusters to keep.
sleep 5

# ==========================================================================
echo "[5] snapshot it, clone the snapshot, write to the clone"
raw_rpc rcow_create_snapshot "$(printf '{"lvol_name":"%s","snapshot_name":"%s"}' \
	"${BIG_IMP}" "${BIG_IMP_SNAP}")" >/dev/null 2>"${WORKDIR}/snap.err" \
	|| { fail "snapshot ${BIG_IMP}"; sed 's/^/    /' "${WORKDIR}/snap.err"; exit 1; }
pass "snapshot ${BIG_IMP_SNAP} taken (its decouple was cancelled)"

raw_rpc rcow_create_clone "$(printf '{"snapshot_name":"%s","clone_name":"%s"}' \
	"${BIG_IMP_SNAP}" "${BIG_IMP_CLONE}")" >/dev/null 2>"${WORKDIR}/clone.err" \
	|| { fail "clone ${BIG_IMP_SNAP}"; sed 's/^/    /' "${WORKDIR}/clone.err"; exit 1; }
pass "clone ${BIG_IMP_CLONE} created"

# The clone is kept exposed: it is the reader that has to stay correct while its
# parent is materialised, and giving it clusters of its own means the test also
# covers "the clone's own writes are not disturbed".
CLONE_BEFORE="$(ls /dev/nvme*n* 2>/dev/null | sort || true)"
CLONE_NSID="$(python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" --raw \
	nvmf_subsystem_add_ns \
	"$(printf '{"nqn":"%s","namespace":{"bdev_name":"%s"}}' \
		"${NQN}" "${DST_LVS}/${BIG_IMP_CLONE}")" 2>/dev/null | tr -d '[:space:]')"
CLONE_DEV=""
for _i in $(seq 40); do
	CLONE_DEV="$(comm -13 <(echo "${CLONE_BEFORE}") \
		<(ls /dev/nvme*n* 2>/dev/null | sort || true) | head -1)"
	[ -n "${CLONE_DEV}" ] && [ -b "${CLONE_DEV}" ] && break
	CLONE_DEV=""
	sleep 0.5
done
[ -n "${CLONE_DEV}" ] || { fail "no device for the clone"; exit 1; }

dd if=/dev/urandom of="${WORKDIR}/clone.pat" bs=1M count="${CLONE_WRITE_MB}" status=none
dd if="${WORKDIR}/clone.pat" of="${CLONE_DEV}" bs=1M count="${CLONE_WRITE_MB}" \
	oflag=direct conv=fsync status=none
# What the clone must read from here on: its own write over the inherited content.
cp "${WORKDIR}/big.pat" "${WORKDIR}/clone_expect.bin"
dd if="${WORKDIR}/clone.pat" of="${WORKDIR}/clone_expect.bin" bs=1M conv=notrunc status=none
CLONE_MD5="$(md5sum "${WORKDIR}/clone_expect.bin" | cut -d' ' -f1)"
ACTUAL="$(dd if="${CLONE_DEV}" bs=1M count=$((BIG_GIB * 1024)) iflag=direct \
	status=none | md5sum | cut -d' ' -f1)"
if [ "${ACTUAL}" = "${CLONE_MD5}" ]; then
	pass "clone reads its own write over the inherited content"
else
	fail "clone content wrong before the decouple even started"
fi

# ==========================================================================
echo "[6] decouple the snapshot, with a reader looping on the clone"
: >"${WORKDIR}/clone_reads.txt"
rm -f "${WORKDIR}/reader_stop"
# Small reads at scattered offsets rather than whole-volume ones: a full 1 GiB
# md5 takes long enough that a materialisation of the same size only fits two of
# them, which is too few to call the window covered. Each read is checked against
# the same offset of the expected image, so a cluster caught mid-migration shows
# up as a mismatch on that offset.
(
	off=0
	while [ ! -f "${WORKDIR}/reader_stop" ]; do
		got="$(dd if="${CLONE_DEV}" bs=1M skip="${off}" count=4 iflag=direct \
			status=none 2>/dev/null | md5sum | cut -d' ' -f1)"
		want="$(dd if="${WORKDIR}/clone_expect.bin" bs=1M skip="${off}" count=4 \
			status=none 2>/dev/null | md5sum | cut -d' ' -f1)"
		if [ "${got}" = "${want}" ]; then
			echo "ok ${off}" >>"${WORKDIR}/clone_reads.txt"
		else
			echo "BAD ${off} got=${got} want=${want}" >>"${WORKDIR}/clone_reads.txt"
		fi
		off=$(( (off + 4) % (BIG_GIB * 1024) ))
	done
) &
READER_PID=$!

if raw_rpc rcow_decouple_lvol "$(printf '{"lvol_name":"%s"}' "${BIG_IMP_SNAP}")" \
	>/dev/null 2>"${WORKDIR}/dec.err"; then
	pass "decouple of the read-only snapshot was accepted"
else
	fail "decouple of ${BIG_IMP_SNAP} refused"
	sed 's/^/    /' "${WORKDIR}/dec.err" 2>/dev/null | tail -3
fi

if wait_for_decouple; then
	pass "decouple finished"
else
	fail "decouple did not finish in time"
fi

touch "${WORKDIR}/reader_stop"
wait "${READER_PID}" 2>/dev/null
# wc, not `grep -c ... || echo 0`: grep exits non-zero when it matches nothing,
# and the fallback then appends to an already-empty value, producing a two-line
# "number" that fails the comparison with a baffling message.
READS="$(wc -l < "${WORKDIR}/clone_reads.txt" 2>/dev/null | tr -d ' ')"
READS="${READS:-0}"
BAD="$(grep -c '^BAD ' "${WORKDIR}/clone_reads.txt" 2>/dev/null | head -1)"
BAD="${BAD:-0}"
if [ "${READS}" -gt 0 ] && [ "${BAD}" -eq 0 ]; then
	pass "clone read correctly ${READS} time(s) while its parent materialised"
else
	fail "clone misread ${BAD} of ${READS} read(s) during the materialisation"
	grep '^BAD ' "${WORKDIR}/clone_reads.txt" 2>/dev/null | head -3 | sed 's/^/    /'
fi

if grep -qE "'${BIG_IMP_SNAP}' no longer reads export" "${TGT_LOG}"; then
	pass "snapshot reports it no longer reads the export"
else
	fail "no 'no longer reads export' line for ${BIG_IMP_SNAP}"
fi

# ==========================================================================
echo "[7] the parent is really gone, not merely unrefused"
# A second decouple must now fail: EINVAL means the blob is no longer an esnap
# clone, which is the only direct evidence that clear_external_parent ran.
if raw_rpc rcow_decouple_lvol "$(printf '{"lvol_name":"%s"}' "${BIG_IMP_SNAP}")" \
	>/dev/null 2>&1; then
	fail "a second decouple was accepted -- the external parent was not cleared"
else
	pass "second decouple refused: the snapshot is no longer an esnap clone"
fi

raw_rpc rcow_get_lvstores "" >"${WORKDIR}/lvs.json" 2>/dev/null || true
SNAP_ALLOC="$(python3 -c "
import json, sys
try:
    rows = json.load(open(sys.argv[1]))
except Exception:
    print(-1); raise SystemExit
for lvs in (rows if isinstance(rows, list) else rows.get('lvstores', [])):
    for l in lvs.get('lvols', []):
        if l.get('name') == sys.argv[2]:
            print(l.get('allocated_clusters', -1)); raise SystemExit
print(-1)
" "${WORKDIR}/lvs.json" "${BIG_IMP_SNAP}" 2>/dev/null || echo -1)"
if [ "${SNAP_ALLOC}" -gt 0 ] 2>/dev/null; then
	pass "snapshot now owns ${SNAP_ALLOC} cluster(s) of its own"
else
	fail "snapshot owns no clusters after being decoupled (reported '${SNAP_ALLOC}')"
fi

# ==========================================================================
echo "[8] erase the source and reload: the bytes must be local now"
# The whole point. release_export cannot be used -- it runs on the source
# lvstore, which was unloaded so this one could exist -- and deleting the prefix
# is the stronger statement anyway: if the objects are gone from S3 and the reads
# still work, the data is really here. Unload and re-attach so that nothing can
# be answered out of memory.
nvme_settle
remove_ns "${CLONE_NSID}"
CLONE_NSID=""

python3 "${TOOLS_DIR}/s3_prefix_rm.py" -e "${ENDPOINT}" -b "${BUCKET}" \
	-r "${REGION}" -p "${SRC_LVS}/" >"${WORKDIR}/prefix_rm.log" 2>&1
info "$(tail -1 "${WORKDIR}/prefix_rm.log")"
if grep -qE '[1-9][0-9]* deleted' "${WORKDIR}/prefix_rm.log"; then
	pass "source prefix erased from S3"
else
	fail "source prefix was not erased -- the check below would prove nothing"
fi

raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" >/dev/null 2>&1
raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${DST_LVS}")" \
	>/dev/null 2>"${WORKDIR}/unload_dst.err" \
	|| { fail "unload ${DST_LVS}"; sed 's/^/    /' "${WORKDIR}/unload_dst.err"; exit 1; }
DST_CREATED=0
sleep 2
raw_rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
	"${DST_LVS}" "${BUCKET}" "${DST_WAL_BDEV}")" \
	>/dev/null 2>"${WORKDIR}/attach_dst.err" \
	|| { fail "re-attach ${DST_LVS}"; sed 's/^/    /' "${WORKDIR}/attach_dst.err"; exit 1; }
DST_CREATED=1
sleep 2
pass "${DST_LVS} unloaded and re-attached with the source gone"

SNAP_MD5="$(read_vol_md5 "${BIG_IMP_SNAP}")" || SNAP_MD5="unreadable"
VOL_MD5="$(read_vol_md5 "${BIG_IMP}")" || VOL_MD5="unreadable"
CLONE_NOW="$(read_vol_md5 "${BIG_IMP_CLONE}")" || CLONE_NOW="unreadable"
info "source was      ${SRC_MD5}"
info "snapshot        ${SNAP_MD5}"
info "volume          ${VOL_MD5}"
info "clone (expects ${CLONE_MD5})"
info "clone           ${CLONE_NOW}"

if [ "${SNAP_MD5}" = "${SRC_MD5}" ]; then
	pass "snapshot still reads correctly with its source erased"
else
	fail "snapshot content wrong after the source was erased"
fi
if [ "${VOL_MD5}" = "${SRC_MD5}" ]; then
	pass "volume still reads correctly with its source erased"
else
	fail "volume content wrong after the source was erased"
fi
if [ "${CLONE_NOW}" = "${CLONE_MD5}" ]; then
	pass "clone still reads its own write with its source erased"
else
	fail "clone content wrong after the source was erased"
fi

check_target "step 8" || exit 1

echo "--- log kept at: ${TGT_LOG}"
