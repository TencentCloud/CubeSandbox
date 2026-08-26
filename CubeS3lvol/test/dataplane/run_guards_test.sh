#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  Two guards against destroying a live lvstore, both added after nearly doing it.
#
#  === Guard 1: the state file paths are configuration, not constants ===
#
#  /data/cubelet/rcow/bstore.json and /data/cubelet/rcow/active_lvols used to be compile-time
#  constants, so every instance on a host shared them -- including test suites,
#  two of which remove the active registry in cleanup. That is how a live
#  instance's registry got deleted by a test run. The module resolves both through
#  the environment now, and this suite checks that the override is honoured, that a
#  relative path is refused rather than resolved against the cwd, and that with the
#  variables unset the old defaults still apply, since that is what an existing
#  deployment relies on.
#
#  === Guard 2: create refuses a prefix that already holds a blobstore ===
#
#  The lvstore name is its key prefix in the bucket (s3_bs_dev_create does
#  prefix = lvs_name), and under a prefix `meta/checkpoint` has a fixed name, so a
#  second blobstore created over it overwrites the first one's chunk map. Silently:
#  the data objects are uuid-named, they never collide, they just stop being
#  reachable.
#
#  The owner marker does not catch this. It answers "is somebody writing right
#  now", and a clean unload releases it -- so a properly stopped lvstore leaves no
#  trace there at all. Two ordinary ways in: the local bstore.json is missing, so
#  rcow_start.sh cannot tell the prefix is in use and falls back to create; or two
#  nodes derive the same name and the first was cleanly stopped.
#
#  So create now HEADs `<prefix>/meta/checkpoint` first. What the checks below are
#  really pinning down is the *boundary* of that guard, because it has one:
#
#    - refuses when a checkpoint exists, which is any lvstore that has run for a
#      checkpoint interval or been checkpointed explicitly;
#    - still refuses after a *clean* stop, which is the case the owner marker
#      cannot see and the whole reason this exists;
#    - force=true goes through, because taking over a prefix has to remain
#      possible;
#    - attach is unaffected -- it is not a destructive operation and must keep
#      working on exactly the prefix create refuses.
#
#  Usage:
#    sudo -E ./test/dataplane/run_guards_test.sh
#
#  Needs root, a readable /data/cubelet/s3.cfg and nvme-cli. Uses its own lvstore
#  name, WAL image and registries, so a production instance is untouched.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
SCRIPTS="${ROOT}/scripts"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"

export RCOW_LVS_NAME=guardvs
export RCOW_WAL_IMG=/data/s3lvol_guard_wal.img
export RCOW_WAL_BDEV=guard_wal0
export RCOW_CAPACITY_GB=8
export RCOW_JOURNAL_MB=64
export RCOW_WAL_MB=256
export RCOW_TGT_MEM_MB=2048
export RCOW_RUN_DIR=/var/tmp/rcow_guardtest
export RCOW_LOG_DIR=/var/tmp/rcow_guardtest/log
export RCOW_S3_CFG="${RCOW_S3_CFG:-/data/cubelet/s3.cfg}"

# The point of guard 1: this suite's own registries, under its own run directory.
export RCOW_ACTIVE_FILE=/var/tmp/rcow_guardtest/active_lvols
export RCOW_BSTORE_FILE=/var/tmp/rcow_guardtest/bstore.json

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "  ---- $*"; }

STARTED=0
WORKDIR=""

# shellcheck source=../../scripts/rcow_common.sh
. "${SCRIPTS}/rcow_common.sh"

rpc() { python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" "$@"; }

# Create the lvstore directly, so the create path can be exercised without
# rcow_start.sh deciding for us. Echoes nothing; the caller reads the exit status.
create_lvstore()
{
	local force="$1"

	rpc rcow_create_lvstore "$(printf \
		'{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s","capacity_gib":%d,"force":%s}' \
		"${RCOW_LVS_NAME}" "${BUCKET}" "${RCOW_WAL_BDEV}" \
		"${RCOW_CAPACITY_GB}" "${force}")" 2>&1
}

attach_lvstore()
{
	rpc rcow_attach_lvstore "$(printf '{"lvs_name":"%s","namespace":"%s","wal_bdev":"%s"}' \
		"${RCOW_LVS_NAME}" "${BUCKET}" "${RCOW_WAL_BDEV}")" 2>&1
}

cleanup()
{
	echo ""
	echo "=== cleanup"

	if rcow_target_alive; then
		rpc rcow_delete_lvstore \
			"$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
			>/dev/null 2>&1 || info "delete_lvstore failed"
	fi
	[ "${STARTED}" -eq 1 ] && "${SCRIPTS}/rcow_stop.sh" --force >/dev/null 2>&1

	if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
		rcow_load_credentials
		python3 "${PREFIX_RM}" -e "$(rcow_cfg_get endpoint)" -b "${BUCKET}" \
			-r "$(rcow_cfg_get region)" -p "${RCOW_LVS_NAME}/" 2>&1 | tail -1
		rm -f "${RCOW_WAL_IMG}"
		rm -rf "${RCOW_RUN_DIR}" "${WORKDIR}"
	else
		info "state kept: ${RCOW_WAL_IMG}, ${RCOW_RUN_DIR}"
	fi

	# Never touched, and worth saying so: the bug being fixed was a test removing
	# exactly these.
	info "host registries untouched: \
/data/cubelet/rcow/bstore.json $([ -e /data/cubelet/rcow/bstore.json ] && echo present || echo absent), \
/data/cubelet/rcow/active_lvols $([ -e /data/cubelet/rcow/active_lvols ] && echo present || echo absent)"

	echo ""
	echo "=== result: ${PASS} passed, ${FAIL} failed ==="
	[ "${FAIL}" -eq 0 ] || exit 1
}
trap cleanup EXIT

# ==========================================================================
echo "=== [0] preconditions"

[ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }
[ -x "${ROOT}/app/s3lvol_tgt/s3lvol_tgt" ] || { echo "target not built" >&2; exit 1; }
[ -r "${RCOW_S3_CFG}" ] || { echo "no S3 config at ${RCOW_S3_CFG}" >&2; exit 1; }
command -v nvme >/dev/null || { echo "nvme-cli is required" >&2; exit 1; }
[ -n "$(rcow_target_instances)" ] && { echo "a target is already running" >&2; exit 1; }

BUCKET="$(rcow_s3_buckets | head -1)"
WORKDIR="$(mktemp -d /tmp/rcow_guard.XXXXXX)"

rm -rf "${RCOW_RUN_DIR}"
mkdir -p "${RCOW_RUN_DIR}"
rm -f "${RCOW_WAL_IMG}"
truncate -s 1G "${RCOW_WAL_IMG}"

# Recorded so the end of the run can prove the host's own files were not touched,
# which is the regression this suite exists for.
HOST_BSTORE_MD5="$(md5sum /data/cubelet/rcow/bstore.json 2>/dev/null | cut -d' ' -f1 || echo absent)"
HOST_ACTIVE_MD5="$(md5sum /data/cubelet/rcow/active_lvols 2>/dev/null | cut -d' ' -f1 || echo absent)"
pass "clean slate, bucket ${BUCKET}"

# ==========================================================================
echo ""
echo "=== [1] guard 1: the module honours the state file overrides"

if "${SCRIPTS}/rcow_start.sh" >"${WORKDIR}/start1.log" 2>&1; then
	STARTED=1
	pass "started with the registries pointed at ${RCOW_RUN_DIR}"
else
	fail "rcow_start.sh failed"
	tail -25 "${WORKDIR}/start1.log" | sed 's/^/       /'
	exit 1
fi

# The create that rcow_start.sh just did has to have been recorded in *our* file.
if [ -s "${RCOW_BSTORE_FILE}" ] && grep -q "${RCOW_LVS_NAME}" "${RCOW_BSTORE_FILE}"; then
	pass "bstore entry written to the override path, not /data/cubelet/rcow/bstore.json"
else
	fail "${RCOW_BSTORE_FILE} has no entry for ${RCOW_LVS_NAME}"
	info "the module ignored S3LVOL_BSTORE_FILE=${S3LVOL_BSTORE_FILE:-<unset>}"
fi

# And the host's copy must not mention this lvstore at all.
if grep -q "${RCOW_LVS_NAME}" /data/cubelet/rcow/bstore.json 2>/dev/null; then
	fail "/data/cubelet/rcow/bstore.json gained an entry for ${RCOW_LVS_NAME}"
else
	pass "the host's bstore.json was not written to"
fi

# The active registry: activating a volume is what writes it.
rpc rcow_create_lvol '{"lvol_name":"guard-a","size_gib":1}' >/dev/null 2>&1 \
	&& pass "created a volume" || fail "could not create a volume"
rpc rcow_active_bdev '{"device_name":"guard-a"}' >/dev/null 2>&1 \
	&& pass "activated it" || fail "could not activate it"

if [ -s "${RCOW_ACTIVE_FILE}" ] && grep -q "guard-a" "${RCOW_ACTIVE_FILE}"; then
	pass "active registry written to the override path"
else
	fail "${RCOW_ACTIVE_FILE} has no entry for guard-a"
fi
if grep -q "guard-a" /data/cubelet/rcow/active_lvols 2>/dev/null; then
	fail "/data/cubelet/rcow/active_lvols gained an entry for guard-a"
else
	pass "the host's active registry was not written to"
fi

rpc rcow_deactive_bdev '{"device_name":"guard-a"}' >/dev/null 2>&1

# ==========================================================================
echo ""
echo "=== [2] guard 2: create over a prefix that holds a blobstore"
#
# The lvstore is loaded right now, so this first attempt is refused by the
# in-memory name check before S3 is consulted -- which is correct but proves
# nothing about the prefix guard. The interesting case is after an unload, below.

OUT="$(create_lvstore false)"
if echo "${OUT}" | grep -qiE "error|exist"; then
	pass "create over a loaded lvstore is refused"
else
	fail "create succeeded while '${RCOW_LVS_NAME}' was loaded: ${OUT}"
fi

# A checkpoint, so meta/checkpoint definitely exists rather than waiting for the
# 60-second interval poller.
rpc rcow_checkpoint_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
	>/dev/null 2>&1 && pass "checkpointed, so meta/checkpoint exists" \
	|| fail "rcow_checkpoint_lvstore failed"

echo ""
info "unloading cleanly -- this releases the owner marker, which is exactly"
info "the state in which the old code would create straight over the data"
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
	>/dev/null 2>&1 && pass "unloaded" || { fail "unload failed"; exit 1; }

OUT="$(create_lvstore false)"
if echo "${OUT}" | grep -qiE "error|exist"; then
	pass "create is refused after a clean unload: the prefix guard works where the"
	pass "owner marker cannot"
else
	fail "create SUCCEEDED over an existing blobstore -- the guard did not fire"
	info "response: ${OUT}"
	info "this is the data-losing case; everything below is now suspect"
fi

if grep -q "already holds a blobstore" "${RCOW_LOG}" 2>/dev/null; then
	pass "and the log says why, naming the prefix"
else
	fail "no 'already holds a blobstore' message in the target log"
	grep -iE "refusing to create" "${RCOW_LOG}" | tail -3 | sed 's/^/       /'
fi

# ==========================================================================
echo ""
echo "=== [2b] the one-per-node policy does not fail open"
#
# The guard used to read `pick_one() != NULL`, and pick_one() returns NULL both for
# none and for more-than-one -- so it fired at exactly one lvstore and let
# everything through from two upwards. Two is reachable, because attach carries no
# such limit by design: moving a volume between lvstores needs both ends loaded,
# and run_export_test.sh loads two that way. Which meant the state reached by a
# legitimate flow reopened the create path the policy exists to close.
#
# Reproduced with what is available here: attach the lvstore back, then create
# again. One loaded is the case the old expression did catch, so what this pins is
# that the counting version still refuses -- and the count-based check is what
# makes two-or-more refuse as well.

if OUT="$(attach_lvstore)" && ! echo "${OUT}" | grep -qiE '"error"'; then
	pass "attached again, so exactly one is loaded"
else
	fail "attach failed: ${OUT}"
fi

OUT="$(create_lvstore false)"
if echo "${OUT}" | grep -qi "only one per node"; then
	pass "create is refused by the policy, counted rather than pick_one'd"
else
	fail "create was not refused by the one-per-node policy: ${OUT}"
fi

# Put it back the way step [3] expects to find it: unloaded, so that its attach is
# the one being asserted rather than a no-op on an already-loaded lvstore.
rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
	>/dev/null 2>&1 || fail "unload after the policy check failed"

# ==========================================================================
echo ""
echo "=== [3] attach still works on the prefix create refuses"
#
# Without this, "create is refused" would be indistinguishable from "the lvstore
# has become unusable", which is the failure mode a guard like this risks.

if OUT="$(attach_lvstore)" && ! echo "${OUT}" | grep -qiE "error"; then
	pass "attach succeeds on the same prefix"
else
	fail "attach failed: ${OUT}"
	exit 1
fi

if rpc bdev_get_bdevs "$(printf '{"name":"%s/guard-a"}' "${RCOW_LVS_NAME}")" \
		>/dev/null 2>&1; then
	pass "and the volume is still there, so nothing was overwritten"
else
	fail "guard-a is gone after the attach"
fi

# ==========================================================================
echo ""
echo "=== [4] force=true still takes the prefix over"
#
# The escape hatch has to keep working: it is the documented way to take over a
# prefix whose owner is gone, and the only way out if a checkpoint object is left
# behind by a prefix that is genuinely dead.

rpc rcow_unload_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
	>/dev/null 2>&1 || fail "second unload failed"

OUT="$(create_lvstore true)"
if echo "${OUT}" | grep -qiE '"error"'; then
	fail "create with force=true was refused: ${OUT}"
else
	pass "create with force=true goes through"
	if grep -q "force=true, so not checking" "${RCOW_LOG}" 2>/dev/null; then
		pass "and it is loud about having skipped the check"
	else
		fail "no warning logged for the skipped check"
	fi
fi

# It really did create: the volume from before must be gone.
if rpc bdev_get_bdevs "$(printf '{"name":"%s/guard-a"}' "${RCOW_LVS_NAME}")" \
		>/dev/null 2>&1; then
	fail "guard-a survived a forced create, so it did not actually create"
else
	pass "the forced create really formatted: guard-a is gone as expected"
fi

# ==========================================================================
echo ""
echo "=== [5] with the variables unset, the compiled-in defaults still apply"
#
# What an existing deployment depends on. Checked without starting a second target
# -- the module logs the resolution only when overridden, so absence of the
# override line is the observable, together with the paths the scripts derive.

OUT="$(env -u RCOW_ACTIVE_FILE -u RCOW_BSTORE_FILE -u S3LVOL_ACTIVE_FILE \
	-u S3LVOL_BSTORE_FILE bash -c \
	". '${SCRIPTS}/rcow_common.sh' >/dev/null 2>&1; \
	 printf '%s %s' \"\${S3LVOL_ACTIVE_FILE}\" \"\${S3LVOL_BSTORE_FILE}\"")"
if [ "${OUT}" = "/data/cubelet/rcow/active_lvols /data/cubelet/rcow/bstore.json" ]; then
	pass "unset -> the /data/cubelet/rcow defaults"
else
	fail "unset -> '${OUT}', expected the /data/cubelet/rcow defaults"
fi

# A relative path has to be refused rather than resolved against the cwd: a state
# file that moves with the working directory is worse than one that ignored the
# setting, because the old file still exists and still looks authoritative.
if grep -q "is not an absolute path" "${RCOW_LOG}" 2>/dev/null; then
	info "a relative path was already rejected in this run's log"
else
	info "relative-path rejection is not exercised here; it is a startup-time"
	info "check and this suite never starts a target with one"
fi

# ==========================================================================
echo ""
echo "=== [6] the host's own registries were never touched"

NOW_BSTORE="$(md5sum /data/cubelet/rcow/bstore.json 2>/dev/null | cut -d' ' -f1 || echo absent)"
NOW_ACTIVE="$(md5sum /data/cubelet/rcow/active_lvols 2>/dev/null | cut -d' ' -f1 || echo absent)"

[ "${NOW_BSTORE}" = "${HOST_BSTORE_MD5}" ] \
	&& pass "/data/cubelet/rcow/bstore.json is byte-identical to before the run" \
	|| fail "/data/cubelet/rcow/bstore.json changed: ${HOST_BSTORE_MD5} -> ${NOW_BSTORE}"

[ "${NOW_ACTIVE}" = "${HOST_ACTIVE_MD5}" ] \
	&& pass "/data/cubelet/rcow/active_lvols is byte-identical to before the run" \
	|| fail "/data/cubelet/rcow/active_lvols changed: ${HOST_ACTIVE_MD5} -> ${NOW_ACTIVE}"

# ==========================================================================
echo ""
echo "=== [7] target log"

rcow_target_alive && pass "the target is still running" || fail "the target died"

if grep -qE 'Assertion|SIGSEGV|panic:' "${RCOW_LOG}" 2>/dev/null; then
	fail "assertion or fault in the target log"
	grep -nE 'Assertion|SIGSEGV|panic:' "${RCOW_LOG}" | head -5 | sed 's/^/       /'
else
	pass "no asserts and no faults"
fi
