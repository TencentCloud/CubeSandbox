#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#
#  The control plane: scripts/rcow_start.sh, rcow_stop.sh, rcow_recovery.sh.
#
#  === What is worth asserting here ===
#
#  Every other dataplane test drives the target directly. This one drives the
#  three scripts, because their job is the sequencing, and the failures that
#  matter are the ones where a script reports success for a state that is not
#  there:
#
#    1. create vs. attach is decided from bstore.json, and a second start must
#       attach rather than create -- create formats the journal and the WAL.
#    2. a restart puts every volume back on the same subsystem and nsid, and the
#       data is still there. The host's uuid -> device lookup depends on it.
#    3. after a crash the owner marker in S3 blocks the attach. Forcing past it is
#       only allowed once the holder is proved dead, and "proved" has to mean
#       something narrower than "the attach failed".
#    4. a registry entry naming a volume that no longer exists is dropped, loudly,
#       rather than replayed onto whatever now answers to that name.
#    5. a target that loaded the registry but attached nothing must be reported as
#       a mismatch, not as healthy.
#
#  Case 5 is also the regression guard for a NULL dereference that killed the
#  target on the first rcow_get_bdev after a restart: spdk_json_next() returns
#  NULL at the end of the enclosing object, and the registry loader walked past
#  it. Nothing else in the suite reaches that line, because every other test
#  either starts with no registry file or removes it before the first activation.
#
#  === Isolation ===
#
#  The lvstore name, the WAL image and the run directory are all test-specific, so
#  a production instance is untouched. bstore.json and rcow_active_lvols are not:
#  their paths are compiled into the module. So the test refuses to run when
#  something is already recorded as active, and removes only its own entries
#  afterwards.
#
#  Usage:
#    sudo -E ./test/dataplane/run_control_test.sh
#
#  Needs root (the target, nvme connect) and a readable /data/cubelet/s3.cfg.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
SCRIPTS="${ROOT}/scripts"
RPC="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"

# Test-specific, and exported so the three scripts pick them up.
export RCOW_LVS_NAME=ctlvs
export RCOW_WAL_IMG=/data/s3lvol_ctl_wal.img
export RCOW_WAL_BDEV=ctl_wal0
export RCOW_CAPACITY_GB=1
export RCOW_JOURNAL_MB=64
export RCOW_WAL_MB=256
export RCOW_TGT_MEM_MB=2048
export RCOW_RUN_DIR=/var/tmp/rcow_ctltest
# Under RCOW_RUN_DIR on purpose. Step [0] wipes that directory so the log check at
# the end sees only this run, but the default log dir is /data/log/rcow, which is
# outside it and shared with every other instance on the host -- so the wipe missed
# and a crash from hours earlier was reported as this run's failure.
export RCOW_LOG_DIR=/var/tmp/rcow_ctltest/log
export RCOW_S3_CFG="${RCOW_S3_CFG:-/data/cubelet/s3.cfg}"

# This suite's own registries, not the host's. They used to be the compiled-in
# /var/tmp paths, shared with every instance on the machine -- and cleanup below
# removes the active one outright, which is how a live instance lost its registry.
# The module resolves both through the environment now (vbdev_s3lvol_statefile.c)
# and rcow_common.sh passes them on to the target.
#
# Under RCOW_RUN_DIR, so step [0]'s wipe covers them too: this suite asserts on
# registry contents, and a previous run's entries would be read as this run's.
export RCOW_ACTIVE_FILE=/var/tmp/rcow_ctltest/active_lvols
export RCOW_BSTORE_FILE=/var/tmp/rcow_ctltest/bstore.json
ACTIVE_FILE="${RCOW_ACTIVE_FILE}"
REPLAY_FILE="${RCOW_ACTIVE_FILE}.replay"
BSTORE_FILE="${RCOW_BSTORE_FILE}"

PATTERN=/tmp/rcow_ctl_pattern.bin
READBACK=/tmp/rcow_ctl_readback.bin

PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL+1)); echo "  [FAIL] $*"; }
info() { echo "  ---- $*"; }

# Read the pattern back from a device and compare it, as two separate findings.
#
# "the device could not be read" and "the data is different" are not the same
# thing, and the second is the most alarming sentence this test can produce.
# Written as `dd && cmp` they collapse into the second one -- and that is measured
# rather than hypothetical: running the whole suite back to back left the machine
# busy enough that udev had not yet created a device node the target had already
# published in sysfs, and the test reported it as data corruption.
read_back_pattern()
{
	local dev="$1" what="$2"
	local err=/tmp/rcow_ctl_readback.err

	if [ ! -b "${dev}" ]; then
		fail "${what}: '${dev}' is not a block device"
		return 1
	fi
	if ! dd if="${dev}" of="${READBACK}" bs=1M count=4 iflag=direct \
		status=none 2>"${err}"; then
		fail "${what}: ${dev} could not be read: $(tr -d '\n' <"${err}")"
		return 1
	fi
	pass "${what}: ${dev} is readable"

	if cmp -s "${PATTERN}" "${READBACK}"; then
		pass "${what}: the 4 MiB pattern is byte-for-byte unchanged"
		return 0
	fi
	fail "${what}: the data differs from what was written"
	return 1
}

# shellcheck source=../../scripts/rcow_common.sh
. "${SCRIPTS}/rcow_common.sh"

jget() { python3 -c 'import json,sys; print(json.loads(sys.argv[1])[sys.argv[2]])' "$1" "$2"; }

rpc() { python3 "${RPC}" "$@"; }

registry_names() {
	python3 - "${ACTIVE_FILE}" <<'PY'
import json, sys
try:
    print(" ".join(sorted(json.load(open(sys.argv[1])))))
except Exception:
    print("")
PY
}

# --------------------------------------------------------------------------
cleanup()
{
	echo ""
	echo "=== cleanup"

	if rcow_target_alive; then
		rpc rcow_deactive_bdev '{"device_name":"ctl-a"}'    >/dev/null 2>&1
		rpc rcow_deactive_bdev '{"device_name":"ctl-b"}'    >/dev/null 2>&1
		rpc rcow_deactive_bdev '{"device_name":"ctl-snap"}' >/dev/null 2>&1
		# delete rather than unload: this removes the objects and the
		# bstore.json entry together. After a failure keep both -- they are
		# the evidence.
		if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
			rpc rcow_delete_lvstore "$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" \
				>/dev/null 2>&1
		fi
	fi

	"${SCRIPTS}/rcow_stop.sh" --force >/dev/null 2>&1

	# Whatever the target did not remove.
	if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
		ENDPOINT="$(rcow_cfg_get endpoint)"
		REGION="$(rcow_cfg_get region)"
		BUCKET="$(rcow_s3_buckets | head -1)"
		rcow_load_credentials
		python3 "${PREFIX_RM}" -e "${ENDPOINT}" -b "${BUCKET}" -r "${REGION}" \
			-p "${RCOW_LVS_NAME}/" 2>&1 | tail -1
		rm -f "${RCOW_WAL_IMG}" "${PATTERN}" "${READBACK}" /tmp/rcow_ctl_readback.err
		rm -f "${ACTIVE_FILE}" "${REPLAY_FILE}"
		python3 - "${BSTORE_FILE}" "${RCOW_LVS_NAME}" <<'PY'
import json, sys
p = sys.argv[1]
try:
    d = json.load(open(p))
except Exception:
    raise SystemExit
if d.pop(sys.argv[2], None) is not None:
    json.dump(d, open(p, "w"), indent=2)
PY
	else
		info "state kept for inspection: ${RCOW_WAL_IMG}, ${ACTIVE_FILE}, ${RCOW_LOG}"
	fi

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

# The registry is this suite's own now, so a stale one is leftover state rather
# than someone else's live data; step [0] wipes RCOW_RUN_DIR, which covers it.
# What still has to be exclusive is the target: it binds a fixed RPC socket and
# the host's nvme stack.
if [ -n "$(rcow_target_instances)" ]; then
	echo "an s3lvol_tgt is already running; stop it first" >&2
	exit 1
fi

rm -f "${REPLAY_FILE}"

# A fresh log directory, because the target log is appended to across runs and
# the check at the end greps it. Keeping it cost both halves of a real failure:
# an assert from one run was reported against the next one, and the run that
# actually caused it was reported green.
rm -rf "${RCOW_RUN_DIR}"
# Recreated immediately: the registries live in here, and the module's atomic write
# needs the directory to exist before it can rename a temp file into it.
mkdir -p "${RCOW_RUN_DIR}"

truncate -s 1G "${RCOW_WAL_IMG}"
pass "starting from a clean slate"

# ==========================================================================
echo ""
echo "=== [1] the start script refuses what it should"

if RCOW_WAL_IMG=/nonexistent/wal.img "${SCRIPTS}/rcow_start.sh" >/dev/null 2>&1; then
	fail "started with a WAL image that does not exist"
	"${SCRIPTS}/rcow_stop.sh" --force >/dev/null 2>&1
else
	pass "a missing WAL image is refused (it is never created here: an empty \
file looks like an lvstore with nothing to replay)"
fi

# ==========================================================================
echo ""
echo "=== [2] first start: creates the lvstore and the whole grid"

if "${SCRIPTS}/rcow_start.sh" >/tmp/rcow_ctl_start1.log 2>&1; then
	pass "rcow_start.sh succeeded"
else
	fail "rcow_start.sh failed"
	tail -20 /tmp/rcow_ctl_start1.log | sed 's/^/       /'
	exit 1
fi

grep -q "creating a new lvstore" /tmp/rcow_ctl_start1.log &&
	pass "it created rather than attached (nothing was recorded yet)" ||
	fail "it did not take the create path on an empty bstore.json"

[ "$(rcow_subsys_count)" = "${RCOW_NUM_SUBSYS}" ] &&
	pass "all ${RCOW_NUM_SUBSYS} subsystems exist" ||
	fail "only $(rcow_subsys_count) subsystems exist"

CONNECTED="$(rcow_connected_nqns | grep -cF "${RCOW_NQN_PREFIX}")"
[ "${CONNECTED}" = "${RCOW_NUM_SUBSYS}" ] &&
	pass "the initiator connected all ${RCOW_NUM_SUBSYS} of them at startup" ||
	fail "only ${CONNECTED} subsystems are connected"

# Every namespace slot must be reachable, which is what -m 64 on
# nvmf_create_subsystem is for. Without it the subsystem silently caps at 32 and
# any nsid above that is refused with a message that only appears in the log.
MAXNS="$(rcow_srpc nvmf_get_subsystems 2>/dev/null | python3 -c '
import json, sys
subs = json.load(sys.stdin)
vals = {s.get("max_namespaces") for s in subs if s.get("nqn","").startswith(sys.argv[1])}
print(",".join(str(v) for v in sorted(vals)))
' "${RCOW_NQN_PREFIX}")"
[ "${MAXNS}" = "${RCOW_NS_PER_SUBSYS}" ] &&
	pass "each subsystem allows ${RCOW_NS_PER_SUBSYS} namespaces" ||
	fail "max_namespaces is '${MAXNS}', wanted ${RCOW_NS_PER_SUBSYS}"

# max_io_size is set to the chunk size so that the host issues one command per
# chunk instead of eight, and so that an aligned 1 MiB request cannot straddle
# two S3 objects. It is a transport option, and the host learns it as MDTS.
TR_MIS="$(rcow_srpc nvmf_get_transports 2>/dev/null | python3 -c '
import json, sys
for t in json.load(sys.stdin):
    if t.get("trtype", "").upper() == "TCP":
        print(t.get("max_io_size", 0))
        break
' 2>/dev/null)"
[ "${TR_MIS}" = "${RCOW_MAX_IO_SIZE}" ] &&
	pass "the TCP transport has max_io_size ${RCOW_MAX_IO_SIZE}" ||
	fail "max_io_size is '${TR_MIS}', wanted ${RCOW_MAX_IO_SIZE}"

if "${SCRIPTS}/rcow_start.sh" >/tmp/rcow_ctl_start_again.log 2>&1; then
	fail "a second start was allowed while one was running"
else
	pass "a second start is refused while one is running"
fi

# ==========================================================================
echo ""
echo "=== [3] volumes appear on the host after the connect, not before"

rpc rcow_create_lvol '{"lvol_name":"ctl-a","size_gib":1}' >/dev/null ||
	fail "could not create ctl-a"
rpc rcow_create_lvol '{"lvol_name":"ctl-b","size_gib":1}' >/dev/null ||
	fail "could not create ctl-b"
rpc rcow_create_snapshot '{"lvol_name":"ctl-a","snapshot_name":"ctl-snap"}' \
	>/dev/null || fail "could not snapshot ctl-a"

A_JSON="$(rpc rcow_active_bdev '{"device_name":"ctl-a"}')" || fail "activate ctl-a"
rpc rcow_active_bdev '{"device_name":"ctl-b"}'    >/dev/null || fail "activate ctl-b"
rpc rcow_active_bdev '{"device_name":"ctl-snap"}' >/dev/null || fail "activate ctl-snap"

A_SUB="$(jget "${A_JSON}" subsys)"
A_NSID="$(jget "${A_JSON}" nsid)"
A_UUID="$(jget "${A_JSON}" uuid)"
info "ctl-a -> subsys ${A_SUB} nsid ${A_NSID}"

# The controllers were connected before these namespaces existed, so this is the
# hot-plug path: the target sends a namespace-changed AEN and the host adds the
# device on its own. If it did not work, every volume would need a fresh fabric
# login in the latency path of its attach.
if rcow_verify_active 30 >/dev/null 2>&1; then
	pass "namespaces added after the connect became block devices (AEN hot-plug)"
else
	fail "namespaces added after the connect did not appear on the host"
fi

A_DEV="$(jget "$(rpc rcow_get_bdev '{"device_name":"ctl-a"}')" device_path)"
info "ctl-a device ${A_DEV}"

# What max_io_size buys, from the host's side: MDTS follows it (ctrlr.c:3264), so
# the kernel stops cutting every 1 MiB request into eight 128 KiB commands.
WANT_KB=$((RCOW_MAX_IO_SIZE / 1024))
HW_KB="$(cat "/sys/block/$(basename "${A_DEV}")/queue/max_hw_sectors_kb" 2>/dev/null)"
[ "${HW_KB}" = "${WANT_KB}" ] &&
	pass "the host will issue up to ${WANT_KB} KiB per command (MDTS follows \
max_io_size)" ||
	fail "max_hw_sectors_kb is '${HW_KB}', wanted ${WANT_KB}"

# And what the iobuf setting buys on the target side, which is the half that
# actually reaches S3. The transport cuts a request into
# ceil(length / large_bufsize) buffers (transport.c:931), and the bdev splits per
# segment because s3_bs_dev takes iovcnt == 1 -- so with the 132 KiB default a
# 1 MiB read became eight bdev I/Os and eight object operations. The invariant
# worth holding on to is max_io_size <= large_bufsize, and this is what it looks
# like from outside: one host I/O, one bdev I/O.
#
# Asserted behaviourally rather than by reading the config back, because there is
# no iobuf_get_options RPC, and because the number that matters is this one.
iostat_read_ops()
{
	rcow_srpc bdev_get_iostat --names "$1" 2>/dev/null | python3 -c \
		'import json,sys; print(json.load(sys.stdin)["bdevs"][0]["num_read_ops"])' \
		2>/dev/null || echo -1
}

OPS_BEFORE="$(iostat_read_ops "${RCOW_LVS_NAME}/ctl-a")"
dd if="${A_DEV}" of=/dev/null bs=1M count=1 iflag=direct status=none 2>/dev/null
OPS_AFTER="$(iostat_read_ops "${RCOW_LVS_NAME}/ctl-a")"
OPS_DELTA=$((OPS_AFTER - OPS_BEFORE))

EXPECT_OPS=$(( (RCOW_MAX_IO_SIZE + RCOW_IOBUF_LARGE_BUFSIZE - 1) / RCOW_IOBUF_LARGE_BUFSIZE ))
[ "${OPS_DELTA}" = "${EXPECT_OPS}" ] &&
	pass "a 1 MiB host read reaches the bdev as ${OPS_DELTA} I/O, so as one \
chunk and one S3 object" ||
	fail "a 1 MiB host read became ${OPS_DELTA} bdev I/O(s), expected \
${EXPECT_OPS} for large_bufsize ${RCOW_IOBUF_LARGE_BUFSIZE}"

dd if=/dev/urandom of="${PATTERN}" bs=1M count=4 status=none
if dd if="${PATTERN}" of="${A_DEV}" bs=1M count=4 oflag=direct status=none &&
   dd if="${A_DEV}" of="${READBACK}" bs=1M count=4 iflag=direct status=none &&
   cmp -s "${PATTERN}" "${READBACK}"; then
	pass "4 MiB written and read back through ${A_DEV}"
else
	fail "the write/read round trip through ${A_DEV} failed"
fi

# ==========================================================================
echo ""
echo "=== [4] clean stop keeps the record of what was active"

if "${SCRIPTS}/rcow_stop.sh" >/tmp/rcow_ctl_stop1.log 2>&1; then
	pass "rcow_stop.sh succeeded"
else
	fail "rcow_stop.sh failed"
	tail -20 /tmp/rcow_ctl_stop1.log | sed 's/^/       /'
fi

grep -q "unloaded: everything acknowledged" /tmp/rcow_ctl_stop1.log &&
	pass "the lvstore was unloaded, so the log is closed" ||
	fail "the stop did not unload the lvstore"

[ -z "$(rcow_target_instances)" ] && pass "no target process is left" ||
	fail "a target is still running after the stop"

[ "$(rcow_connected_nqns | grep -cF "${RCOW_NQN_PREFIX}")" = "0" ] &&
	pass "the initiator was disconnected" ||
	fail "controllers are still connected"

# The registry has to survive: it is what the next start replays. Emptying it on
# the way down would turn every planned restart into a node that comes back with
# no volumes exposed.
[ "$(registry_names)" = "ctl-a ctl-b ctl-snap" ] &&
	pass "all three volumes are still recorded in ${ACTIVE_FILE}" ||
	fail "the registry says '$(registry_names)' after the stop"

# ==========================================================================
echo ""
echo "=== [5] second start: attaches, and puts the layout back unchanged"

if "${SCRIPTS}/rcow_start.sh" >/tmp/rcow_ctl_start2.log 2>&1; then
	pass "rcow_start.sh succeeded"
else
	fail "the second rcow_start.sh failed"
	tail -25 /tmp/rcow_ctl_start2.log | sed 's/^/       /'
fi

if grep -q "creating a new lvstore" /tmp/rcow_ctl_start2.log; then
	fail "it created a second lvstore instead of attaching the recorded one"
else
	pass "it attached rather than created"
fi

grep -q "replay: 3 restored, 0 refused, 0 failed" /tmp/rcow_ctl_start2.log &&
	pass "all three volumes were restored" ||
	{ fail "the replay did not restore all three"; \
	  grep -i replay /tmp/rcow_ctl_start2.log | sed 's/^/       /'; }

RE_JSON="$(rpc rcow_get_bdev '{"device_name":"ctl-a"}')"
[ "$(jget "${RE_JSON}" subsys)" = "${A_SUB}" ] &&
[ "$(jget "${RE_JSON}" nsid)" = "${A_NSID}" ] &&
	pass "ctl-a came back at subsys ${A_SUB} nsid ${A_NSID}" ||
	fail "ctl-a came back at subsys $(jget "${RE_JSON}" subsys) nsid $(jget "${RE_JSON}" nsid)"

[ "$(jget "${RE_JSON}" uuid)" = "${A_UUID}" ] &&
	pass "and with the same uuid, so the host's lookup still resolves it" ||
	fail "the uuid changed across the restart"

RE_DEV="$(jget "${RE_JSON}" device_path)"
read_back_pattern "${RE_DEV}" "after the clean restart"

# ==========================================================================
echo ""
echo "=== [5b] removing a namespace while the host is reading it"

# The deactivate path pauses the namespace, removes it, and resumes; the resume
# releases the namespace's bdev channel. If the pause does not actually quiesce
# the namespace, that release happens with reads still outstanding, and the bdev
# layer aborts the process:
#
#   bdev_channel_destroy_resource: Assertion `TAILQ_EMPTY(&ch->io_submitted)'
#
# Which is exactly what happened: spdk_nvmf_subsystem_pause() takes the nsid to
# quiesce and treats 0 as "no namespaces", and this module passed 0. An idle
# volume never shows it -- every other test here deactivates a volume nobody is
# reading -- so what found it was the udev probe of a device that had just
# appeared, arriving in the same millisecond as a deactivate.
#
# The read has to be one that takes real time, hence a cold 32 MiB from S3
# rather than something the overlay can answer.
TGT_BEFORE="$(rcow_target_pid)"
dd if="${RE_DEV}" of=/dev/null bs=1M count=32 iflag=direct status=none 2>/dev/null &
DD_PID=$!
sleep 0.4

if rpc rcow_deactive_bdev '{"device_name":"ctl-a"}' >/dev/null 2>&1; then
	pass "the deactivate completed with reads in flight"
else
	fail "the deactivate failed with reads in flight"
fi
wait "${DD_PID}" 2>/dev/null || true   # the read is expected to fail: its device left

TGT_AFTER="$(rcow_target_pid)" || TGT_AFTER=""
if [ -n "${TGT_AFTER}" ] && [ "${TGT_AFTER}" = "${TGT_BEFORE}" ]; then
	pass "the target survived it (pid ${TGT_AFTER} unchanged)"
else
	fail "the target died removing a namespace under host reads"
fi

# Put it back, so the rest of the test sees the layout it expects.
rpc rcow_active_bdev '{"device_name":"ctl-a"}' >/dev/null 2>&1 ||
	fail "could not re-activate ctl-a"
if rcow_verify_active 30 >/dev/null 2>&1; then
	pass "and the volume can be activated again afterwards"
else
	fail "ctl-a did not come back after being deactivated"
fi

RE_JSON="$(rpc rcow_get_bdev '{"device_name":"ctl-a"}')"
RE_DEV="$(jget "${RE_JSON}" device_path)"
[ "$(jget "${RE_JSON}" subsys)" = "${A_SUB}" ] &&
[ "$(jget "${RE_JSON}" nsid)" = "${A_NSID}" ] &&
	pass "at the same placement it had before (subsys ${A_SUB} nsid ${A_NSID})" ||
	fail "it came back at subsys $(jget "${RE_JSON}" subsys) nsid $(jget "${RE_JSON}" nsid)"

read_back_pattern "${RE_DEV}" "after deactivating under load and re-activating"

# ==========================================================================
echo ""
echo "=== [6] crash: recovery takes the lvstore back and restores the layout"

CRASH_PID="$(rcow_target_pid)"
kill -9 "${CRASH_PID}" 2>/dev/null
sleep 1
[ -z "$(rcow_target_instances)" ] && pass "target ${CRASH_PID} was killed outright" ||
	fail "the target survived SIGKILL"

# An entry naming a volume that no longer exists. Replaying it would either fail
# or, worse, land on whatever has taken that name since.
python3 - "${ACTIVE_FILE}" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d["ctl-ghost"] = {"uuid": "deadbeef-0000-0000-0000-000000000000",
                  "subsys": 5, "nsid": 7}
json.dump(d, open(sys.argv[1], "w"), indent=2)
PY

if "${SCRIPTS}/rcow_recovery.sh" >/tmp/rcow_ctl_recover.log 2>&1; then
	pass "rcow_recovery.sh succeeded"
else
	fail "rcow_recovery.sh failed"
	tail -25 /tmp/rcow_ctl_recover.log | sed 's/^/       /'
fi

grep -q "no target is running: recovery is a start" /tmp/rcow_ctl_recover.log &&
	pass "with nothing running it handed over to the start path" ||
	fail "it did not recognise that a recovery is a start"

# The owner marker in S3 still names the process that was killed. Forcing past it
# is only correct once the holder is known to be gone, and the only thing that
# knows is this node.
grep -q "which is not running. Retrying with force=true" /tmp/rcow_ctl_recover.log &&
	pass "the stale owner marker was confirmed dead before being overwritten" ||
	fail "the owner marker was not handled the way it should be"

grep -q "'ctl-ghost' was active .* no longer exists" /tmp/rcow_ctl_recover.log &&
	pass "the entry for a volume that no longer exists was refused, loudly" ||
	fail "the ghost entry was not refused"

grep -q "replay: 3 restored, 1 refused, 0 failed" /tmp/rcow_ctl_recover.log &&
	pass "the other three were restored" ||
	{ fail "unexpected replay outcome"; \
	  grep -i "replay:" /tmp/rcow_ctl_recover.log | sed 's/^/       /'; }

[ "$(registry_names)" = "ctl-a ctl-b ctl-snap" ] &&
	pass "the ghost is gone from the registry and the rest is intact" ||
	fail "the registry says '$(registry_names)'"

[ ! -e "${REPLAY_FILE}" ] &&
	pass "the replay plan was removed once nothing was left to do" ||
	fail "${REPLAY_FILE} is still there"

RE_DEV="$(jget "$(rpc rcow_get_bdev '{"device_name":"ctl-a"}')" device_path)"
read_back_pattern "${RE_DEV}" "after the SIGKILL (WAL replay)"

# ==========================================================================
echo ""
echo "=== [7] a target that inherited the registry without replaying it"

"${SCRIPTS}/rcow_stop.sh" >/dev/null 2>&1
if ! "${SCRIPTS}/rcow_start.sh" --no-replay >/tmp/rcow_ctl_start3.log 2>&1; then
	fail "rcow_start.sh --no-replay failed"
	tail -20 /tmp/rcow_ctl_start3.log | sed 's/^/       /'
fi

# This is where the target used to die. The registry is read lazily, on the first
# activation RPC, so this call is the first time the parser runs against a file
# with entries in it -- and spdk_json_next() hands back NULL at the end of the
# object, which the loop walked straight into.
if LIST="$(rpc rcow_get_bdev '{}' 2>&1)"; then
	COUNT="$(printf '%s' "${LIST}" | python3 -c \
		'import json,sys; print(len(json.load(sys.stdin)))' 2>/dev/null || echo 0)"
	[ "${COUNT}" = "3" ] &&
		pass "the registry loaded and listed all 3 inherited entries" ||
		fail "the loader returned ${COUNT} entries, wanted 3"
else
	fail "rcow_get_bdev against an inherited registry failed: ${LIST}"
fi

rcow_target_alive &&
	pass "the target survived reading a non-empty registry" ||
	fail "the target died while loading the registry"

EMPTY_PATHS="$(printf '%s' "${LIST}" | python3 -c '
import json, sys
try:
    print(sum(1 for e in json.load(sys.stdin) if not e.get("device_path")))
except Exception:
    print(-1)
' 2>/dev/null)"
[ "${EMPTY_PATHS}" = "3" ] &&
	pass "and reported them with no device path, which is the truth here" ||
	fail "${EMPTY_PATHS} of the inherited entries had no device path, wanted 3"

if "${SCRIPTS}/rcow_recovery.sh" --verify-only --timeout 5 \
		>/tmp/rcow_ctl_verify.log 2>&1; then
	fail "--verify-only called this healthy"
else
	pass "--verify-only reported the mismatch instead of calling it healthy"
fi
grep -q "restart instead" /tmp/rcow_ctl_verify.log &&
	pass "and said what actually fixes it, rather than re-activating" ||
	fail "the advice on a mismatch is missing"

# ==========================================================================
echo ""
echo "=== [8] the restart really does fix it"

"${SCRIPTS}/rcow_stop.sh" >/dev/null 2>&1
if "${SCRIPTS}/rcow_start.sh" >/tmp/rcow_ctl_start4.log 2>&1 &&
   "${SCRIPTS}/rcow_recovery.sh" --verify-only >/dev/null 2>&1; then
	pass "stop + start restored the layout and --verify-only agrees"
else
	fail "the restart did not restore the layout"
	tail -20 /tmp/rcow_ctl_start4.log | sed 's/^/       /'
fi

# ==========================================================================
echo ""
echo "=== target log check"
if grep -qE "Assertion|SIGSEGV|stays paused" "${RCOW_LOG}"; then
	fail "the target log has asserts or a subsystem left paused"
	grep -nE "Assertion|SIGSEGV|stays paused" "${RCOW_LOG}" | head -5 | sed 's/^/       /'
else
	pass "no asserts, and no subsystem left paused"
fi
