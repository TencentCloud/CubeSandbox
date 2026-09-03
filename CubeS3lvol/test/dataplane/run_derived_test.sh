#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Handing on an imported volume without copying it (manifest version 3)
#
#  === What this is for ===
#
#  A volume imported from an export can be snapshotted and exported again. Before
#  version 3 that second export had to copy: the chunks older than the import live
#  under the *exporting* node's prefix, and a manifest could name only one. So the
#  second hop paid for the whole volume even though every byte was already in the
#  bucket.
#
#  Version 3 names a prefix per chunk, so the second export references both its own
#  objects and the ones it inherited. This test is what says that actually works
#  end to end, against a real bucket, rather than in a unit test's idea of a
#  manifest.
#
#  === Why three nodes ===
#
#      A: writes head+tail, snapshots, exports         -> export A (version 2)
#  B: imports A without decoupling, rewrites tail,
#     zeroes the first MiB of the head, snapshots, exports
#                                                   -> export B (version 3)
#  C: imports B, reads both halves (zeroed head prefix + A's rest + B's tail)
#
#  Two nodes would prove too little. B's manifest can name A's prefix and still be
#  useless if nothing ever resolves it, so C exists to do the resolving -- from a
#  third prefix, with A's lvstore unloaded, which is the situation a real handoff
#  arrives in.
#
#  === Why the volume is written in two halves ===
#
#  B rewrites only the tail and zeroes the first MiB of the head. That leaves
#  most of the head resolving to A's prefix, the tail to B's, and the zeroed
#  prefix a hole -- so what C reads is the only assertion that can separate:
#
#    - the right prefix from a wrong one. A wrong prefix does not fail loudly: it
#      names an object that exists and holds a different chunk, so the read
#      succeeds and returns plausible bytes.
#    - the nearer layer winning from the older one winning. A *also* has data at
#      the tail. If inherit ran before the local walk, or overwrote instead of
#      skipping, C would read A's tail -- the volume as it was before the import,
#      with nothing anywhere to say so.
#    - a local write_zeroes from an untouched hole. inherit fills absent chunks
#      from the parent; without marking the zero resolved, C would read A's
#      first MiB instead of zeroes.
#
#  A fully overwritten volume would leave B owning every chunk and the inherit path
#  would not run at all, which is why the overwrite is half and not all.
#
#  === What else is checked, and why it is not obvious ===
#
#  That B's export is version 3 with layout still ref. A copy would also produce a
#  perfectly readable export and every data assertion below would still pass -- the
#  manifest is the only place the difference is visible.
#
#  That C renews two leases, one of them A's own export. This is what keeps the
#  handoff durable: A decides independently whether its snapshot is still needed,
#  and it consults the lease. Renewing only B's would leave A free to delete the
#  objects C's head resolves to. It is also why the lease is renewed against A
#  directly rather than through B -- B may be gone.
#
#  Usage:
#    sudo -E ./test/dataplane/run_derived_test.sh -e <endpoint> -b <bucket> [-r <region>]
#
#  Requires: root, nvme-cli, a real bucket with credentials in the environment.
#
set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TOOLS_DIR="${REPO_ROOT}/test/tools"

TGT_BIN="${REPO_ROOT}/app/s3lvol_tgt/s3lvol_tgt"
RPC_SOCK="/var/run/s3lvol_derived.sock"

A_LVS="drva"
B_LVS="drvb"
C_LVS="drvc"
S3_EXPORTS_DIR="exports"

NQN="nqn.2026-09.io.spdk:derived"
LISTEN_ADDR="127.0.0.1"
LISTEN_PORT="4473"

CAPACITY_GIB=4
LVOL_GIB=1
JOURNAL_MB=64
WAL_MB=128
WAL_FILE_MB=$((JOURNAL_MB + WAL_MB + 128))

A_WAL_FILE="${S3LVOL_A_WAL_FILE:-/data/s3lvol_derived_a.img}"
B_WAL_FILE="${S3LVOL_B_WAL_FILE:-/data/s3lvol_derived_b.img}"
C_WAL_FILE="${S3LVOL_C_WAL_FILE:-/data/s3lvol_derived_c.img}"
A_WAL_BDEV="derived_a_wal0"
B_WAL_BDEV="derived_b_wal0"
C_WAL_BDEV="derived_c_wal0"

# Two halves, each several chunks wide, starting past the blobstore metadata.
HALF_MB=8
HEAD_OFF_MB=8
TAIL_OFF_MB=$((HEAD_OFF_MB + HALF_MB))
# First cluster of the head. cluster_size == chunk_size == 1 MiB.
ZERO_OFF_MB="${HEAD_OFF_MB}"
ZERO_LEN_MB=1
HEAD_REST_OFF_MB=$((HEAD_OFF_MB + ZERO_LEN_MB))
HEAD_REST_MB=$((HALF_MB - ZERO_LEN_MB))

ENDPOINT=""
BUCKET=""
REGION="ap-nanjing"

PASS=0
FAIL=0
TGT_PID=""
TGT_LOG=""
WORKDIR=""
CONNECTED=0
TRANSPORT_READY=0
A_CREATED=0
B_CREATED=0
C_CREATED=0
ANY_CREATED=0
TEARDOWN_ANOMALY=0
EXPORT_UUIDS=""
EXP_A=""
EXP_B=""
DID_ZERO=0
NVME_DEV=""
CUR_NSID=""

pass() { PASS=$((PASS + 1)); echo "[PASS] $*"; }
fail() { FAIL=$((FAIL + 1)); echo "[FAIL] $*"; }
info() { echo "---- $*"; }

# Every value comparison goes through here so the summary cannot disagree with the
# body, and so a failure prints what it actually got.
want()
{
	if [ "$2" = "$3" ]; then
		pass "$1 (${2})"
	else
		fail "$1: got '${2}', want '${3}'"
	fi
}

usage()
{
	cat <<EOF
Usage: $0 -e <endpoint> -b <bucket> [-r <region>]

  -e   S3/COS endpoint host, e.g. cos.ap-nanjing.myqcloud.com
  -b   bucket (must be a test bucket: a clean run deletes its own prefixes)
  -r   region (default: ${REGION})

Credentials are read from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.

Environment:
  S3LVOL_KEEP_S3    keep the S3 objects after the run
  S3LVOL_KEEP_LOGS  keep the target log after a clean run
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

WORKDIR="$(mktemp -d /tmp/s3lvol_derived.XXXXXX)"
TGT_LOG="${WORKDIR}/target.log"

raw_rpc()
{
	python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" "$1" "${2:-}"
}

nvmf_rpc()
{
	python3 "${TOOLS_DIR}/s3lvol_rpc.py" --sock "${RPC_SOCK}" --raw "$1" "${2:-}"
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

export_status_field()
{
	local out

	out="$(raw_rpc rcow_get_snapshot_status \
		"$(printf '{"export_uuid":"%s"}' "$1")" 2>/dev/null)" || return 1
	printf '%s' "${out}" \
		| python3 -c "import json,sys; print(json.load(sys.stdin).get('$2',''))" \
			2>/dev/null
}

wait_export_done()
{
	local uuid="$1" where="$2"
	local deadline=$(( $(date +%s) + 120 ))
	local st

	while :; do
		if ! st="$(export_status_field "${uuid}" export_status)"; then
			fail "export ${uuid} finished without a manifest (${where})"
			return 1
		fi
		[ "${st}" = "DONE" ] && return 0
		if [ "$(date +%s)" -ge "${deadline}" ]; then
			fail "export ${uuid} never reached DONE (${where}, last '${st}')"
			return 1
		fi
		sleep 0.3
	done
}

wait_for_decouple()
{
	local deadline=$(( $(date +%s) + 900 ))

	while [ "$(date +%s)" -lt "${deadline}" ]; do
		if raw_rpc rcow_get_decouple "" 2>/dev/null |
		   python3 -c 'import json,sys; raise SystemExit(0 if not json.load(sys.stdin) else 1)'; then
			return 0
		fi
		sleep 1
	done
	return 1
}

# Read a member out of the published manifest. This is the only way to tell a
# referenced export from a copied one, so it comes from S3 rather than from any
# RPC the writing node offers -- what the importer will see is what is asserted.
manifest_field()
{
	python3 "${TOOLS_DIR}/s3_get_manifest.py" -e "${ENDPOINT}" -b "${BUCKET}" \
		-r "${REGION}" -u "$1" --field "$2" 2>/dev/null
}

nvme_settle()
{
	udevadm settle --timeout=5 >/dev/null 2>&1 || true
}

expose()
{
	nvmf_rpc nvmf_subsystem_add_ns \
		"$(printf '{"nqn":"%s","namespace":{"bdev_name":"%s"}}' \
			"${NQN}" "$1")" 2>/dev/null | tr -d '[:space:]'
}

unexpose()
{
	local nsid="$1"
	[ -n "${nsid}" ] && nvmf_rpc nvmf_subsystem_remove_ns \
		"$(printf '{"nqn":"%s","nsid":%s}' "${NQN}" "${nsid}")" \
		>/dev/null 2>&1
	nvme_settle
	return 0
}

ctrl_of_nqn()
{
	local c
	for c in /sys/class/nvme/nvme*; do
		[ "$(cat "${c}/subsysnqn" 2>/dev/null)" = "${NQN}" ] && \
			{ basename "${c}"; return 0; }
	done
	return 1
}

wait_dev()
{
	local nsid="$1" deadline ctrl dev
	deadline=$(( $(date +%s) + 30 ))
	while [ "$(date +%s)" -lt "${deadline}" ]; do
		if ctrl="$(ctrl_of_nqn)"; then
			nvme ns-rescan "/dev/${ctrl}" >/dev/null 2>&1 || true
			dev="/dev/${ctrl}n${nsid}"
			[ -b "${dev}" ] && { printf '%s' "${dev}"; return 0; }
		fi
		sleep 0.3
	done
	return 1
}

# Expose a volume and hand back its device, or fail and print nothing. Runs in a
# command substitution, so anything it did to PASS/FAIL would be lost with the
# subshell -- the caller decides what an empty answer means.
attach()
{
	local bdev="$1" nsid dev

	nsid="$(expose "${bdev}")"
	if [ -z "${nsid}" ]; then
		echo "---- could not expose ${bdev}" >&2
		return 1
	fi
	CUR_NSID="${nsid}"
	nvme_settle
	if ! dev="$(wait_dev "${nsid}")"; then
		echo "---- ${bdev}: nsid ${nsid} never became a device" >&2
		return 1
	fi
	NVME_DEV="${dev}"
	printf '%s' "${dev}"
}

# Refuses a target that is not already a block device: writing to a path that has
# not appeared yet creates a regular file where the device belongs, which outlives
# the run and breaks every later one for a reason that looks like a discovery
# timeout.
write_at()
{
	local dev="$1" file="$2" off_mb="$3"

	if [ -z "${dev}" ] || [ ! -b "${dev}" ]; then
		fail "refusing to write to '${dev}': not a block device"
		return 1
	fi
	dd if="${file}" of="${dev}" bs=1M count="${HALF_MB}" seek="${off_mb}" \
		oflag=direct conv=fsync status=none 2>"${WORKDIR}/write.err"
}

read_md5_at()
{
	local dev="$1" off_mb="$2"

	read_md5_range "${dev}" "${off_mb}" "${HALF_MB}"
}

read_md5_range()
{
	local dev="$1" off_mb="$2" count_mb="$3"

	dd if="${dev}" bs=1M count="${count_mb}" skip="${off_mb}" \
		iflag=direct status=none 2>/dev/null | md5sum | cut -d' ' -f1
}

md5_of() { md5sum "$1" | cut -d' ' -f1; }

md5_of_range()
{
	local file="$1" skip_mb="$2" count_mb="$3"

	dd if="${file}" bs=1M skip="${skip_mb}" count="${count_mb}" \
		status=none 2>/dev/null | md5sum | cut -d' ' -f1
}

md5_of_zeroes()
{
	dd if=/dev/zero bs=1M count="$1" status=none 2>/dev/null | md5sum | cut -d' ' -f1
}

drop_caches() { sync; echo 3 >/proc/sys/vm/drop_caches 2>/dev/null || true; }

create_lvstore()
{
	local lvs="$1" wal="$2"

	raw_rpc rcow_create_lvstore \
		"$(printf '{"lvs_name":"%s","namespace":"%s","capacity_gib":%d,"wal_bdev":"%s","journal_size_mb":%d,"wal_size_mb":%d}' \
			"${lvs}" "${BUCKET}" "${CAPACITY_GIB}" "${wal}" \
			"${JOURNAL_MB}" "${WAL_MB}")" \
		>"${WORKDIR}/${lvs}_create.json" 2>"${WORKDIR}/${lvs}_create.err"
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
			nvmf_rpc nvmf_delete_subsystem \
				"$(printf '{"nqn":"%s"}' "${NQN}")" >/dev/null 2>&1 || true
		fi

		for pair in "${C_LVS}:${C_CREATED}" "${B_LVS}:${B_CREATED}" \
			    "${A_LVS}:${A_CREATED}"; do
			[ "${pair#*:}" -eq 1 ] || continue
			raw_rpc rcow_unload_lvstore \
				"$(printf '{"lvs_name":"%s"}' "${pair%%:*}")" \
				>/dev/null 2>&1 || TEARDOWN_ANOMALY=1
		done

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

	# Swept whether or not the run passed, which is deliberate and differs from
	# the older suites here.
	#
	# Those gate the sweep on FAIL being zero, to leave state behind to look at.
	# The cost is worse than the benefit: this suite's prefixes are reused by
	# name, so one failed run poisons every later one, and the second run's
	# output is then a mixture of a real defect and the leftovers of the first.
	# Measured, in run_pending_delete_test: the same single failure reads as 14
	# broken checks on a dirty bucket and 1 on a clean one, and the difference
	# cost a wrong attribution.
	#
	# S3LVOL_KEEP_S3 is the documented way to keep the objects and still works,
	# so nothing is lost -- it is just no longer the accidental default of any
	# run that happened to fail.
	if [ "${ANY_CREATED}" -eq 1 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
		for prefix in "${A_LVS}/" "${B_LVS}/" "${C_LVS}/"; do
			remove_prefix "${prefix}" >>"${WORKDIR}/s3_rm.log" 2>&1 || \
				info "sweep of ${prefix} failed"
		done
		# exports/ is bucket-level and shared with anything else running, so
		# only this run's manifests go, by uuid.
		for u in ${EXPORT_UUIDS}; do
			remove_prefix "${S3_EXPORTS_DIR}/${u}" \
				>>"${WORKDIR}/s3_rm.log" 2>&1 || true
		done
		info "S3 prefixes swept"
	elif [ -n "${S3LVOL_KEEP_S3:-}" ]; then
		info "S3 kept: ${A_LVS}/ ${B_LVS}/ ${C_LVS}/ and exports/"
	fi

	rm -f "${A_WAL_FILE}" "${B_WAL_FILE}" "${C_WAL_FILE}"

	if [ "${FAIL}" -eq 0 ] && [ "${rc}" -eq 0 ] && \
	   [ "${TEARDOWN_ANOMALY}" -eq 0 ] && [ -z "${S3LVOL_KEEP_LOGS:-}" ]; then
		rm -rf "${WORKDIR}"
	else
		info "log and workdir kept: ${WORKDIR}"
	fi

	if [ "${TEARDOWN_ANOMALY}" -eq 1 ]; then
		info "teardown was not clean"
	fi

	echo
	echo "=== result: ${PASS} passed, ${FAIL} failed ==="
	[ "${FAIL}" -eq 0 ] || exit 1
	exit "${rc}"
}
trap cleanup EXIT

# ==========================================================================
echo "=== [0] preconditions"

[ -x "${TGT_BIN}" ] || { echo "target not built: ${TGT_BIN}" >&2; exit 1; }
if pgrep -f 's3lvol_tgt' >/dev/null 2>&1; then
	echo "a target is already running" >&2
	exit 1
fi

"${TOOLS_DIR}/check_binary_fresh.sh" "${TGT_BIN}" || {
	echo "target binary is older than the source it was built from" >&2
	exit 1; }
pass "target binary is newer than the tree"

rm -f "${RPC_SOCK}"
for f in "${A_WAL_FILE}" "${B_WAL_FILE}" "${C_WAL_FILE}"; do
	rm -f "${f}"
	truncate -s "${WAL_FILE_MB}M" "${f}" || {
		echo "could not create ${f}" >&2; exit 1; }
done

# Anything this run's prefixes still hold is from an earlier run that did not get
# to sweep. Removed before starting rather than after failing, so a run always
# begins from the same state no matter how the previous one ended.
for prefix in "${A_LVS}/" "${B_LVS}/" "${C_LVS}/"; do
	remove_prefix "${prefix}" >>"${WORKDIR}/s3_pre.log" 2>&1 || true
done
pass "stale prefixes from earlier runs removed"

AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID}" \
AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY}" \
	"${TGT_BIN}" -m 0x3 --no-huge -s 2048 -r "${RPC_SOCK}" \
	>"${TGT_LOG}" 2>&1 &
TGT_PID=$!
for _ in $(seq 120); do [ -S "${RPC_SOCK}" ] && break; sleep 0.25; done
[ -S "${RPC_SOCK}" ] || { fail "target never opened its RPC socket"
	tail -20 "${TGT_LOG}" | sed 's/^/       /'; exit 1; }
sleep 1
pass "target started"

raw_rpc rcow_add_s3_config \
	"$(printf '{"namespace":"%s","endpoint":"%s","bucket":"%s","region":"%s"}' \
		"${BUCKET}" "${ENDPOINT}" "${BUCKET}" "${REGION}")" >/dev/null || {
	fail "rcow_add_s3_config"; exit 1; }
pass "S3 namespace registered"

for pair in "${A_WAL_FILE}:${A_WAL_BDEV}" "${B_WAL_FILE}:${B_WAL_BDEV}" \
	    "${C_WAL_FILE}:${C_WAL_BDEV}"; do
	nvmf_rpc bdev_aio_create \
		"$(printf '{"filename":"%s","name":"%s","block_size":4096}' \
			"${pair%%:*}" "${pair#*:}")" >/dev/null 2>&1
done

nvmf_rpc nvmf_create_transport '{"trtype":"TCP"}' >/dev/null 2>&1
nvmf_rpc nvmf_create_subsystem \
	"$(printf '{"nqn":"%s","allow_any_host":true,"serial_number":"DERIVED00000001"}' \
		"${NQN}")" >/dev/null 2>&1
nvmf_rpc nvmf_subsystem_add_listener \
	"$(printf '{"nqn":"%s","listen_address":{"trtype":"TCP","adrfam":"IPv4","traddr":"%s","trsvcid":"%s"}}' \
		"${NQN}" "${LISTEN_ADDR}" "${LISTEN_PORT}")" >/dev/null 2>&1
TRANSPORT_READY=1
pass "nvmf transport and subsystem ready"

# ==========================================================================
echo
echo "=== [1] node A: two halves, a snapshot, and a zero-copy export"

create_lvstore "${A_LVS}" "${A_WAL_BDEV}" || {
	fail "create lvstore ${A_LVS}"
	sed 's/^/       /' "${WORKDIR}/${A_LVS}_create.err"; exit 1; }
A_CREATED=1; ANY_CREATED=1
pass "lvstore ${A_LVS} created"

raw_rpc rcow_create_lvol \
	"$(printf '{"lvol_name":"va","size_gib":%d}' "${LVOL_GIB}")" >/dev/null || {
	fail "create va"; exit 1; }

nvme connect -t tcp -a "${LISTEN_ADDR}" -s "${LISTEN_PORT}" -n "${NQN}" \
	>/dev/null 2>&1
CONNECTED=1
sleep 2

DEV="$(attach "${A_LVS}/va")"
[ -b "${DEV}" ] || { fail "va did not become a device"; exit 1; }
pass "va attached at ${DEV}"

HEAD="${WORKDIR}/head.bin"
TAIL_A="${WORKDIR}/tail_a.bin"
TAIL_B="${WORKDIR}/tail_b.bin"
dd if=/dev/urandom of="${HEAD}"   bs=1M count="${HALF_MB}" status=none
dd if=/dev/urandom of="${TAIL_A}" bs=1M count="${HALF_MB}" status=none
dd if=/dev/urandom of="${TAIL_B}" bs=1M count="${HALF_MB}" status=none
HEAD_MD5="$(md5_of "${HEAD}")"
TAILA_MD5="$(md5_of "${TAIL_A}")"
TAILB_MD5="$(md5_of "${TAIL_B}")"

write_at "${DEV}" "${HEAD}"   "${HEAD_OFF_MB}" || exit 1
write_at "${DEV}" "${TAIL_A}" "${TAIL_OFF_MB}" || exit 1
sync
drop_caches
want "A reads back the head it wrote" \
	"$(read_md5_at "${DEV}" "${HEAD_OFF_MB}")" "${HEAD_MD5}"
want "A reads back the tail it wrote" \
	"$(read_md5_at "${DEV}" "${TAIL_OFF_MB}")" "${TAILA_MD5}"

raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${A_LVS}")" \
	>/dev/null 2>&1 || info "flush of ${A_LVS} failed"
raw_rpc rcow_create_snapshot '{"lvol_name":"va","snapshot_name":"sa"}' \
	>/dev/null || { fail "snapshot sa"; exit 1; }
raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${A_LVS}")" \
	>/dev/null 2>&1 || true
pass "sa taken and flushed"

EXP_A="$(raw_rpc rcow_export_snapshot '{"snapshot_name":"sa"}' 2>/dev/null \
	| tr -d '"[:space:]')"
[ -n "${EXP_A}" ] || { fail "export of sa"; exit 1; }
EXPORT_UUIDS="${EXPORT_UUIDS} ${EXP_A}"
wait_export_done "${EXP_A}" "node A" || exit 1
pass "sa exported as ${EXP_A}"

# A single-source export is written as version 2, so the version alone tells the
# two apart later without having to reason about the source table.
want "A's export is version 2" "$(manifest_field "${EXP_A}" version)" "2"

check_target "node A" || exit 1

unexpose "${CUR_NSID}"
raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${A_LVS}")" \
	>/dev/null 2>&1 || { fail "unload ${A_LVS}"; exit 1; }
A_CREATED=0
sleep 2
pass "A unloaded -- everything it holds is now only in the bucket"

# ==========================================================================
echo
echo "=== [2] node B: import A, rewrite the tail, zero the first MiB of the head"

create_lvstore "${B_LVS}" "${B_WAL_BDEV}" || {
	fail "create lvstore ${B_LVS}"
	sed 's/^/       /' "${WORKDIR}/${B_LVS}_create.err"; exit 1; }
B_CREATED=1
pass "lvstore ${B_LVS} created"

# decouple:false is the whole point: vb stays an esnap clone, so the head remains
# A's objects and only the tail becomes B's. Decoupled, B would own every chunk
# and there would be nothing to inherit.
raw_rpc rcow_import_lvol \
	"$(printf '{"lvol_name":"vb","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
		"${EXP_A}" "${B_LVS}")" >"${WORKDIR}/import_b.json" 2>&1 || {
	fail "import into B"; sed 's/^/       /' "${WORKDIR}/import_b.json"; exit 1; }
pass "A imported into B without decoupling"

DEV="$(attach "${B_LVS}/vb")"
[ -b "${DEV}" ] || { fail "vb did not become a device"; exit 1; }
drop_caches

# If this is wrong nothing after it means anything, so it is checked before the
# rewrite rather than inferred from the end result.
want "B reads A's head through the export" \
	"$(read_md5_at "${DEV}" "${HEAD_OFF_MB}")" "${HEAD_MD5}"
want "B reads A's tail through the export" \
	"$(read_md5_at "${DEV}" "${TAIL_OFF_MB}")" "${TAILA_MD5}"

write_at "${DEV}" "${TAIL_B}" "${TAIL_OFF_MB}" || exit 1
sync
drop_caches
want "B's rewritten tail reads back" \
	"$(read_md5_at "${DEV}" "${TAIL_OFF_MB}")" "${TAILB_MD5}"
want "and B's head is untouched" \
	"$(read_md5_at "${DEV}" "${HEAD_OFF_MB}")" "${HEAD_MD5}"

HEAD_REST_MD5="$(md5_of_range "${HEAD}" "${ZERO_LEN_MB}" "${HEAD_REST_MB}")"
ZERO_MD5="$(md5_of_zeroes "${ZERO_LEN_MB}")"

if command -v blkdiscard >/dev/null 2>&1; then
	if blkdiscard -z -o $((ZERO_OFF_MB * 1024 * 1024)) \
		-l $((ZERO_LEN_MB * 1024 * 1024)) "${DEV}"; then
		DID_ZERO=1
		sync
		drop_caches
		want "B's first MiB of the head is now zeroes" \
			"$(read_md5_range "${DEV}" "${ZERO_OFF_MB}" "${ZERO_LEN_MB}")" \
			"${ZERO_MD5}"
		want "and the rest of B's head is still A's" \
			"$(read_md5_range "${DEV}" "${HEAD_REST_OFF_MB}" "${HEAD_REST_MB}")" \
			"${HEAD_REST_MD5}"
	else
		fail "blkdiscard -z on the first MiB of B's head"
	fi
else
	info "blkdiscard not available, skipping the local-zero inherit check"
fi

raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${B_LVS}")" \
	>/dev/null 2>&1 || info "flush of ${B_LVS} failed"
raw_rpc rcow_create_snapshot '{"lvol_name":"vb","snapshot_name":"sb"}' \
	>/dev/null || { fail "snapshot sb on an imported volume"; exit 1; }
raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${B_LVS}")" \
	>/dev/null 2>&1 || true
pass "sb taken on the imported volume"

EXP_B="$(raw_rpc rcow_export_snapshot '{"snapshot_name":"sb"}' 2>/dev/null \
	| tr -d '"[:space:]')"
[ -n "${EXP_B}" ] || { fail "export of sb"; exit 1; }
EXPORT_UUIDS="${EXPORT_UUIDS} ${EXP_B}"
wait_export_done "${EXP_B}" "node B" || exit 1
pass "sb exported as ${EXP_B}"

check_target "node B" || exit 1

# ==========================================================================
echo
echo "=== [3] was B's export derived, or copied?"

# A copy would produce an export that reads perfectly and passes every data check
# in [4]. The manifest is the only place the difference shows.
want "B's export is version 3" "$(manifest_field "${EXP_B}" version)" "3"
want "with layout still ref, so nothing was copied" \
	"$(manifest_field "${EXP_B}" layout)" "ref"

SRCS="$(manifest_field "${EXP_B}" srcs)"
info "srcs: ${SRCS}"
if printf '%s' "${SRCS}" | grep -q "${A_LVS}"; then
	pass "its source table names A's prefix (${A_LVS})"
else
	fail "its source table does not name A's prefix: ${SRCS}"
fi
# The lease is renewed against whatever export uuid the entry names, so this is
# not cosmetic: the wrong uuid here renews a lease nobody consults.
if printf '%s' "${SRCS}" | grep -q "${EXP_A}"; then
	pass "attributed to A's own export, which is what a lease renews"
else
	fail "no reference to A's export ${EXP_A} in: ${SRCS}"
fi

unexpose "${CUR_NSID}"
raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${B_LVS}")" \
	>/dev/null 2>&1 || { fail "unload ${B_LVS}"; exit 1; }
B_CREATED=0
sleep 2
pass "B unloaded -- A and B are both gone now"

# ==========================================================================
echo
echo "=== [4] node C: import the derived export and read both halves"

create_lvstore "${C_LVS}" "${C_WAL_BDEV}" || {
	fail "create lvstore ${C_LVS}"
	sed 's/^/       /' "${WORKDIR}/${C_LVS}_create.err"; exit 1; }
C_CREATED=1
pass "lvstore ${C_LVS} created"

raw_rpc rcow_import_lvol \
	"$(printf '{"lvol_name":"vc","export_uuid":"%s","lvs_name":"%s","decouple":false}' \
		"${EXP_B}" "${C_LVS}")" >"${WORKDIR}/import_c.json" 2>&1 || {
	fail "import into C"; sed 's/^/       /' "${WORKDIR}/import_c.json"; exit 1; }
pass "B's derived export imported into C"

DEV="$(attach "${C_LVS}/vc")"
[ -b "${DEV}" ] || { fail "vc did not become a device"; exit 1; }
drop_caches

# The two halves together are the assertion. The head can only come from A's
# prefix and the tail only from B's, so this is what separates a right prefix from
# a wrong one that still returns bytes -- and, since A also has data at the tail,
# what proves the nearer layer wins. The first MiB of the head is the local-zero
# case: inherit must not put A's object back.
if [ "${DID_ZERO}" -eq 1 ]; then
	want "C reads zeroes where B zeroed the head" \
		"$(read_md5_range "${DEV}" "${ZERO_OFF_MB}" "${ZERO_LEN_MB}")" \
		"${ZERO_MD5}"
	want "C reads the rest of the head from A's prefix" \
		"$(read_md5_range "${DEV}" "${HEAD_REST_OFF_MB}" "${HEAD_REST_MB}")" \
		"${HEAD_REST_MD5}"
else
	want "C reads the head from A's prefix, with both A and B unloaded" \
		"$(read_md5_at "${DEV}" "${HEAD_OFF_MB}")" "${HEAD_MD5}"
fi
want "C reads the tail as B rewrote it, not as A had it" \
	"$(read_md5_at "${DEV}" "${TAIL_OFF_MB}")" "${TAILB_MD5}"

if [ "$(read_md5_at "${DEV}" "${TAIL_OFF_MB}")" = "${TAILA_MD5}" ]; then
	fail "C read A's tail: the inherited layer overwrote the local one"
fi

check_target "node C" || exit 1

# ==========================================================================
echo
echo "=== [5] C renews a lease per prefix, not one per import"

# A's snapshot is protected only by A's own lease. Renewing B's alone would leave
# A free to delete the objects C's head resolves to, and C would start reading
# 404s some time later -- a failure that would surface nowhere near here.
#
# The pattern is anchored on "export <uuid> lease", which is what an *importer*
# logs. An export that references other prefixes renews on its own behalf too and
# logs "export <uuid> upstream lease [i]", so a pattern starting at "lease [" also
# counts the line B printed back in [2] -- this counted three for a while, and the
# third one was neither C's nor wrong.
sleep 3
LEASES="$(grep -aoE "export [0-9a-f-]+ lease \[[0-9]+\]: [^ ]+" "${TGT_LOG}" | tail -8)"
info "leases: $(printf '%s' "${LEASES}" | tr '\n' ' ')"
want "C renews two leases" \
	"$(printf '%s\n' "${LEASES}" | grep -c 'lease \[' || true)" "2"

if printf '%s' "${LEASES}" | grep -q "${A_LVS}/meta/exports/${EXP_A}.lease"; then
	pass "one is A's own export, so A will refuse to delete its snapshot"
else
	fail "A's lease ${A_LVS}/meta/exports/${EXP_A}.lease is not renewed"
fi
if printf '%s' "${LEASES}" | grep -q "${B_LVS}/meta/exports/${EXP_B}.lease"; then
	pass "the other is B's"
else
	fail "B's lease is not renewed"
fi

check_target "leases" || exit 1

# ==========================================================================
echo
echo "=== [6] converge B's snapshot and redirect its published export"

# Keep C's old generation in its imports registry. After B rewrites the manifest
# and A's objects disappear, C must encounter a 404, refetch generation 1 and
# continue against B's local objects.
unexpose "${CUR_NSID}"
raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${C_LVS}")" \
	>/dev/null 2>&1 || { fail "unload ${C_LVS} before convergence"; exit 1; }
C_CREATED=0

raw_rpc rcow_attach_lvstore \
	"$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${B_LVS}" "${BUCKET}" "${B_WAL_BDEV}")" \
	>/dev/null 2>"${WORKDIR}/attach_b.err" || {
	fail "re-attach ${B_LVS}"; sed 's/^/       /' "${WORKDIR}/attach_b.err"; exit 1; }
B_CREATED=1
pass "B re-attached with its reference snapshot and export"

raw_rpc rcow_decouple_lvol "$(printf '{"lvol_name":"%s"}' "sb")" \
	>/dev/null 2>"${WORKDIR}/decouple_sb.err" || {
	fail "start decoupling sb"; sed 's/^/       /' "${WORKDIR}/decouple_sb.err"; exit 1; }
if wait_for_decouple; then
	pass "sb decoupled and its export rewrite completed"
else
	fail "sb decouple/export rewrite did not finish"
	exit 1
fi

want "B's export generation advanced after convergence" \
	"$(manifest_field "${EXP_B}" generation)" "1"
want "B's converged export remains zero-copy ref" \
	"$(manifest_field "${EXP_B}" layout)" "ref"
SRCS="$(manifest_field "${EXP_B}" srcs)"
info "converged srcs: ${SRCS}"
if printf '%s' "${SRCS}" | grep -q "${A_LVS}"; then
	fail "converged export still references A: ${SRCS}"
else
	pass "converged export no longer references A"
fi
if printf '%s' "$(manifest_field "${EXP_B}" source)" | grep -q "${B_LVS}"; then
	pass "converged export references B's local objects"
else
	fail "converged export's primary source does not name B"
fi
if grep -q "export ${EXP_B} now references local snapshot 'sb' at generation 1" \
	"${TGT_LOG}"; then
	pass "B logged the automatic manifest redirection"
else
	fail "automatic manifest redirection was not logged"
fi

raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${B_LVS}")" \
	>/dev/null 2>&1 || { fail "unload ${B_LVS} after convergence"; exit 1; }
B_CREATED=0

remove_prefix "${A_LVS}/" >"${WORKDIR}/remove_a.log" 2>&1 || {
	fail "remove A's source objects"; exit 1; }
pass "A's source prefix removed after B converged"

raw_rpc rcow_attach_lvstore \
	"$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${C_LVS}" "${BUCKET}" "${C_WAL_BDEV}")" \
	>/dev/null 2>"${WORKDIR}/reattach_c.err" || {
	fail "re-attach ${C_LVS}"; sed 's/^/       /' "${WORKDIR}/reattach_c.err"; exit 1; }
C_CREATED=1
DEV="$(attach "${C_LVS}/vc")"
[ -b "${DEV}" ] || { fail "vc did not return after C re-attached"; exit 1; }
drop_caches
if [ "${DID_ZERO}" -eq 1 ]; then
	want "C refetches the rewritten manifest and still reads the zeroed prefix" \
		"$(read_md5_range "${DEV}" "${ZERO_OFF_MB}" "${ZERO_LEN_MB}")" \
		"${ZERO_MD5}"
	want "C still reads the rest of the head after refetch" \
		"$(read_md5_range "${DEV}" "${HEAD_REST_OFF_MB}" "${HEAD_REST_MB}")" \
		"${HEAD_REST_MD5}"
else
	want "C refetches the rewritten manifest and still reads the head" \
		"$(read_md5_at "${DEV}" "${HEAD_OFF_MB}")" "${HEAD_MD5}"
fi
want "C still reads B's tail with A gone" \
	"$(read_md5_at "${DEV}" "${TAIL_OFF_MB}")" "${TAILB_MD5}"
if grep -q "export ${EXP_B}: now reading generation 1" "${TGT_LOG}"; then
	pass "C switched from its cached manifest to generation 1"
else
	fail "C did not log a manifest refetch to generation 1"
fi

check_target "automatic export convergence" || exit 1

# ==========================================================================
echo
echo "=== [7] persist the refetched generation, then derive and decouple"

unexpose "${CUR_NSID}"
raw_rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${C_LVS}")" \
	>/dev/null 2>&1 || { fail "unload ${C_LVS} after refetch persist"; exit 1; }
C_CREATED=0
raw_rpc rcow_attach_lvstore \
	"$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${C_LVS}" "${BUCKET}" "${C_WAL_BDEV}")" \
	>/dev/null 2>"${WORKDIR}/restart_c.err" || {
	fail "re-attach ${C_LVS} after refetch persist"
	sed 's/^/       /' "${WORKDIR}/restart_c.err"; exit 1; }
C_CREATED=1
DEV="$(attach "${C_LVS}/vc")"
[ -b "${DEV}" ] || { fail "vc did not return after refetch persist"; exit 1; }
drop_caches
if [ "${DID_ZERO}" -eq 1 ]; then
	want "C still reads zeroes after restart on the persisted generation" \
		"$(read_md5_range "${DEV}" "${ZERO_OFF_MB}" "${ZERO_LEN_MB}")" \
		"${ZERO_MD5}"
	want "C still reads the rest of the head after restart" \
		"$(read_md5_range "${DEV}" "${HEAD_REST_OFF_MB}" "${HEAD_REST_MB}")" \
		"${HEAD_REST_MD5}"
else
	want "C still reads the head after restart on the persisted generation" \
		"$(read_md5_at "${DEV}" "${HEAD_OFF_MB}")" "${HEAD_MD5}"
fi
want "C still reads B's tail after restart" \
	"$(read_md5_at "${DEV}" "${TAIL_OFF_MB}")" "${TAILB_MD5}"

# C's data plane has generation 1. Derive and same-bucket decouple used to
# read imp->m, which stayed on generation 0 and named A's objects -- already
# deleted. Restart used the persisted registry copy of the same stale
# generation.
raw_rpc rcow_flush_lvstore "$(printf '{"lvs_name":"%s"}' "${C_LVS}")" \
	>/dev/null 2>&1 || true
raw_rpc rcow_create_snapshot '{"lvol_name":"vc","snapshot_name":"sc"}' \
	>/dev/null || { fail "snapshot sc after refetch"; exit 1; }
EXP_C="$(raw_rpc rcow_export_snapshot '{"snapshot_name":"sc"}' 2>/dev/null \
	| tr -d '"[:space:]')"
[ -n "${EXP_C}" ] || { fail "export of sc"; exit 1; }
EXPORT_UUIDS="${EXPORT_UUIDS} ${EXP_C}"
wait_export_done "${EXP_C}" "node C after refetch" || exit 1
pass "sc exported as ${EXP_C}"

SRCS="$(manifest_field "${EXP_C}" srcs)"
info "derived-after-refetch srcs: ${SRCS}"
if printf '%s' "${SRCS}" | grep -q "${A_LVS}"; then
	fail "derived export after refetch still names A's prefix: ${SRCS}"
else
	pass "derived export after refetch does not name A's deleted prefix"
fi

raw_rpc rcow_decouple_lvol '{"lvol_name":"sc"}' \
	>/dev/null 2>"${WORKDIR}/decouple_sc.err" || {
	fail "decouple sc after refetch"
	sed 's/^/       /' "${WORKDIR}/decouple_sc.err"; exit 1; }
if wait_for_decouple; then
	pass "sc decoupled against the refetched generation"
else
	fail "sc decouple after refetch did not finish"
	exit 1
fi
drop_caches
if [ "${DID_ZERO}" -eq 1 ]; then
	want "C still reads zeroes after decouple" \
		"$(read_md5_range "${DEV}" "${ZERO_OFF_MB}" "${ZERO_LEN_MB}")" \
		"${ZERO_MD5}"
	want "C still reads the rest of the head after decouple" \
		"$(read_md5_range "${DEV}" "${HEAD_REST_OFF_MB}" "${HEAD_REST_MB}")" \
		"${HEAD_REST_MD5}"
else
	want "C still reads the head after decouple" \
		"$(read_md5_at "${DEV}" "${HEAD_OFF_MB}")" "${HEAD_MD5}"
fi
want "C still reads B's tail after decouple" \
	"$(read_md5_at "${DEV}" "${TAIL_OFF_MB}")" "${TAILB_MD5}"

check_target "derive/decouple after refetch" || exit 1

echo
echo "=== all steps done"
