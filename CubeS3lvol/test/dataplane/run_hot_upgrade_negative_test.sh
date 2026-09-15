#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  T2 -- faults injected into the hot-upgrade path.
#
#  === Why a separate suite from T1 ===
#
#  T1 proves the happy path keeps I/O alive. That is the least interesting half:
#  the design's real claim is that every way the upgrade can go wrong is either
#  *prevented* (the version gate, the instance guard, the timeout clamps) or at
#  least *detectable* before it becomes silent corruption. A suite that only runs
#  the happy path cannot refute that -- and the failure this feature exists to
#  prevent (a namespace removed under a live sandbox, or a journal silently
#  truncated by a newer writer) is exactly the kind that leaves a green run
#  behind.
#
#  Each scenario is a function and reports its assertions on its own. They share
#  one target because the data plane allows only one; so they are ordered to keep
#  the machine in a known state, and every scenario that shortens a kernel
#  timeout or kills the target restores it before returning. The destructive
#  scenario (past ctrl_loss_tmo) runs last among the ones that touch the machine,
#  for that reason; the static read of the stop script that follows it needs
#  nothing from the machine.
#
#  === The version gate has one home ===
#
#  These scenarios call the shipped comparison (rcow_version_gate_compare)
#  instead of restating its rule, so the rule cannot hold here and drift there.
#  The documents are the real ones -- the running target's rcow_get_build_info
#  against the candidate's `s3lvol_tgt --print-build-info` -- and each refusal is
#  provoked by mutating a copy of a document, so no second build tree is needed.
#  The last gate case runs the refusal through `rcow_upgrade.sh --candidate`,
#  the entry point that ships today, and asserts the target survived it.
#
#  === The wrong write order cannot be injected through rcow_tune_initiator_timeouts ===
#
#  The function always writes reconnect_delay before ctrl_loss_tmo, so it cannot
#  produce the wrong order itself. The wrong order is therefore written to sysfs
#  directly, on this suite's own controllers, and the assertion is that the
#  read-back *detector* (ctrl_loss_tmo shows max_reconnects * reconnect_delay)
#  sees the collapse. Then the function is used to restore, which is also the
#  positive control: the correct order reads back as expected.
#
#  Usage:
#    export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...
#    sudo -E ./test/dataplane/run_hot_upgrade_negative_test.sh \
#             -e cos.ap-nanjing.myqcloud.com -b my-bucket -r ap-nanjing
#
#  Needs root, nvme-cli, fio (for the I/O-does-not-notice assertions) and
#  exclusive use of this machine's nvme stack.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SELF_DIR}/../.." && pwd)"
SCRIPTS="${ROOT}/scripts"
RPC_PY="${ROOT}/test/tools/s3lvol_rpc.py"
PREFIX_RM="${ROOT}/test/tools/s3_prefix_rm.py"

# Own lvstore / WAL / run directory, as every dataplane suite does, so a
# production instance on the host is untouched.
export RCOW_LVS_NAME=hotnegvs
export RCOW_WAL_IMG=/data/s3lvol_hotneg_wal.img
export RCOW_WAL_BDEV=hotneg_wal0
export RCOW_CAPACITY_GB=512
export RCOW_JOURNAL_MB=64
export RCOW_WAL_MB=512
export RCOW_TGT_MEM_MB=4096
export RCOW_RUN_DIR=/var/tmp/rcow_hotneg
export RCOW_LOG_DIR=/var/tmp/rcow_hotneg/log
export RCOW_S3_CFG="${RCOW_S3_CFG:-/data/cubelet/s3.cfg}"
export RCOW_ACTIVE_FILE=/var/tmp/rcow_hotneg/active_lvols
export RCOW_BSTORE_FILE=/var/tmp/rcow_hotneg/bstore.json

TGT_BIN="${ROOT}/app/s3lvol_tgt/s3lvol_tgt"

ENDPOINT=""
BUCKET=""
REGION=""

VOL="hotneg0"
LVOL_GIB=1
FIO_RUNTIME_SEC=180
VERIFY_TIMEOUT_SEC=180

# ctrl_loss_tmo short enough that the kernel gives up and deletes the
# controller inside the test's patience. With reconnect_delay=1 the kernel
# recomputes max_reconnects = 5, so deletion lands in roughly that many seconds.
# The reference reconnect_delay / ctrl_loss_tmo are read from rcow_common.sh,
# so they are captured after sourcing (below).
SHORT_CTRL_LOSS_TMO=5
# How long to watch for the deletion. Generous against the ~5 s the kernel
# needs, because the retry timer is not the only thing that runs.
DELETE_WATCH_SEC=60

WORKDIR="$(mktemp -d /tmp/s3lvol_hotneg.XXXXXX)"

PASS=0; FAIL=0; SKIP=0
SKIP_FORCED=0
pass() { PASS=$((PASS + 1)); echo "  [PASS] $*"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $*"; }
skip() { SKIP=$((SKIP + 1)); SKIP_FORCED=1; echo "  [SKIP] $*"; }
info() { echo "  ---- $*"; }
cannot_run() { skip "$1"; exit 2; }
scenario() { echo; echo "=== $*"; }

usage()
{
	cat <<EOT
Usage: $0 -e <endpoint> -b <bucket> [-r region]

  -e/-b/-r  as run_recovery_test.sh; defaults are read from ${RCOW_S3_CFG}.
Credentials come from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY or s3.cfg.
EOT
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	-e) ENDPOINT="$2"; shift 2 ;;
	-b) BUCKET="$2"; shift 2 ;;
	-r) REGION="$2"; shift 2 ;;
	-h|--help) usage; exit 0 ;;
	*) usage >&2; exit 1 ;;
	esac
done

# shellcheck source=../../scripts/rcow_common.sh
. "${SCRIPTS}/rcow_common.sh"

rpc() { python3 "${RPC_PY}" --sock "${RCOW_RPC_SOCK}" "$@"; }

# The reference timeouts the connect flags use, so a restore returns the
# controllers to the state the rest of the tree expects.
RESTORE_DELAY="${RCOW_RECONNECT_DELAY}"
RESTORE_TMO="${RCOW_CTRL_LOSS_TMO}"

# ---------------------------------------------------------------- helpers

vol_dev()
{
	python3 -c 'import json,sys; print(json.loads(sys.argv[1]).get("device_path",""))' \
		"$(rpc rcow_get_bdev "$(printf '{"device_name":"%s"}' "$1")" 2>/dev/null)" \
		2>/dev/null
}

dmesg_count()
{
	dmesg 2>/dev/null | grep -c "$1" || true
}

our_controllers()
{
	local d
	for d in /sys/class/nvme/nvme*; do
		[ -r "${d}/subsysnqn" ] || continue
		case "$(cat "${d}/subsysnqn" 2>/dev/null)" in
		"${RCOW_NQN_PREFIX}"*) printf '%s\n' "${d}" ;;
		esac
	done
}

# The first of our controllers that exposes the two attributes the timeout
# rules act on, or nothing when this kernel does not export them (pre-5.7).
tunable_ctrl()
{
	local d
	while read -r d; do
		[ -e "${d}/reconnect_delay" ] && [ -e "${d}/ctrl_loss_tmo" ] &&
			{ printf '%s' "${d}"; return 0; }
	done < <(our_controllers)
	return 1
}

# True while one of our controllers is mid-deletion. That state precedes the
# controller actually leaving /sys/class/nvme, so it is the earlier of the two
# ways to observe the kernel giving up.
any_ctrl_deleting()
{
	local d
	while read -r d; do
		[ "$(cat "${d}/state" 2>/dev/null)" = "deleting" ] && return 0
	done < <(our_controllers)
	return 1
}

# Read an attribute, tolerating the file disappearing (a controller being
# deleted under us).
attr()
{
	cat "$1" 2>/dev/null || true
}

cleanup()
{
	echo
	echo "=== cleanup ==="

	[ -n "${FIO_PID:-}" ] && kill "${FIO_PID}" 2>/dev/null || true
	[ -z "${reader:-}" ] || kill "${reader}" 2>/dev/null || true

	"${SCRIPTS}/rcow_stop.sh" --force >/dev/null 2>&1 || true

	if [ "${FAIL}" -eq 0 ] && [ -z "${S3LVOL_KEEP_S3:-}" ]; then
		rcow_load_credentials 2>/dev/null || true
		python3 "${PREFIX_RM}" -e "${ENDPOINT}" -b "${BUCKET}" -r "${REGION}" \
			-p "${RCOW_LVS_NAME}/" 2>&1 | tail -1
		rm -f "${RCOW_WAL_IMG}"
		rm -rf "${RCOW_RUN_DIR}" "${WORKDIR}"
	else
		info "state kept for inspection: ${RCOW_WAL_IMG}, ${RCOW_RUN_DIR}, ${WORKDIR}"
	fi

	echo
	echo "=== result: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped ==="
	[ "${FAIL}" -eq 0 ] || exit 1
	if [ "${SKIP_FORCED}" -eq 1 ]; then
		echo "some scenarios could not run; this is not a pass" >&2
		exit 2
	fi
}
trap cleanup EXIT

# ==========================================================================
scenario "[0] preconditions"

[ -x "${TGT_BIN}" ] || { echo "target not built: ${TGT_BIN}" >&2; exit 1; }
[ -x "${SCRIPTS}/rcow_start.sh" ] || { echo "rcow_start.sh missing" >&2; exit 1; }
[ -x "${SCRIPTS}/rcow_upgrade.sh" ] || {
	echo "rcow_upgrade.sh missing: the hot path is not in this tree" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || cannot_run "must run as root to connect nvme controllers"
command -v nvme >/dev/null || cannot_run "nvme-cli is required"
command -v python3 >/dev/null || cannot_run "python3 is required"
command -v truncate >/dev/null || cannot_run "truncate is required to size the WAL image"

if [ -z "${S3LVOL_SKIP_FRESH_CHECK:-}" ]; then
	"${ROOT}/test/tools/check_binary_fresh.sh" "${TGT_BIN}" || exit 1
fi

modprobe nvme_tcp 2>/dev/null || true
lsmod | grep -q nvme_tcp || cannot_run "nvme_tcp is not available; there is no loopback transport"

if [ -n "$(rcow_target_instances)" ]; then
	cannot_run "an s3lvol_tgt is already running; stop it first"
fi

[ -n "${ENDPOINT}" ] || ENDPOINT="$(rcow_cfg_get endpoint)"
[ -n "${BUCKET}" ]   || BUCKET="$(rcow_s3_buckets | head -1)"
[ -n "${REGION}" ]   || REGION="$(rcow_cfg_get region)"
[ -n "${REGION}" ] || REGION="us-east-1"

if [ -z "${AWS_ACCESS_KEY_ID:-}" ] || [ -z "${AWS_SECRET_ACCESS_KEY:-}" ]; then
	rcow_load_credentials 2>/dev/null ||
		cannot_run "no credentials: export AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY (with sudo, remember -E)"
fi
if [ -z "${ENDPOINT}" ] || [ -z "${BUCKET}" ]; then
	cannot_run "endpoint/bucket unknown: pass -e/-b or provide ${RCOW_S3_CFG}"
fi

HAVE_FIO=0
command -v fio >/dev/null 2>&1 && HAVE_FIO=1

rm -rf "${RCOW_RUN_DIR}"
mkdir -p "${RCOW_RUN_DIR}"
rm -f "${RCOW_WAL_IMG}"
truncate -s 2G "${RCOW_WAL_IMG}" ||
	cannot_run "could not create the WAL image at ${RCOW_WAL_IMG}"

info "endpoint ${ENDPOINT}, bucket ${BUCKET}, region ${REGION}"
info "fio: $([ "${HAVE_FIO}" -eq 1 ] && echo present || echo MISSING)"

# ==========================================================================
scenario "[1] bring the data plane up and activate one volume"

if "${SCRIPTS}/rcow_start.sh" >"${WORKDIR}/start.log" 2>&1; then
	pass "rcow_start.sh brought the grid up"
else
	fail "rcow_start.sh failed"
	tail -25 "${WORKDIR}/start.log" | sed 's/^/       /'
	exit 1
fi

if rpc rcow_create_lvol "$(printf '{"lvol_name":"%s","size_gib":%d}' "${VOL}" "${LVOL_GIB}")" \
		>/dev/null 2>"${WORKDIR}/create.err" &&
   rpc rcow_active_bdev "$(printf '{"device_name":"%s"}' "${VOL}")" \
		>/dev/null 2>"${WORKDIR}/active.err"; then
	pass "volume ${VOL} created and activated"
else
	fail "could not create/activate ${VOL}"
	cat "${WORKDIR}/create.err" "${WORKDIR}/active.err" 2>/dev/null | sed 's/^/       /'
	exit 1
fi

if rcow_verify_active 60; then
	pass "the volume resolved to an openable block device"
else
	fail "the volume never became a block device"
fi

VOL_DEV="$(vol_dev "${VOL}")"
[ -b "${VOL_DEV}" ] && pass "${VOL} -> ${VOL_DEV}" ||
	fail "${VOL} has no block device (got '${VOL_DEV}')"

# The layout every pre-upgrade scenario must leave untouched.
rpc rcow_get_bdev '{}' >"${WORKDIR}/layout-baseline.json" 2>/dev/null &&
	pass "captured the pre-upgrade layout" ||
	fail "could not capture the layout"

# fio runs across the two scenarios the spec wants to hear "I/O never noticed"
# from: the version gate's refusal and the failed-new-binary rollback.
FIO_PID=""
if [ "${HAVE_FIO}" -eq 1 ] && [ -b "${VOL_DEV}" ]; then
	mkdir -p "${WORKDIR}/fio"
	fio --name="${VOL}" --filename="${VOL_DEV}" \
		--ioengine=libaio --direct=1 --rw=randrw --bs=4k --iodepth=32 \
		--time_based --runtime="${FIO_RUNTIME_SEC}" \
		--continue_on_error=none --error_dump=1 \
		--output-format=json --output="${WORKDIR}/fio/${VOL}.json" \
		>"${WORKDIR}/fio/${VOL}.stdout" 2>&1 &
	FIO_PID=$!
	pass "fio started on ${VOL} (pid ${FIO_PID})"
	sleep 5
else
	skip "no fio (or no device): the 'I/O never noticed' assertions cannot run"
fi

# ==========================================================================
# The version gate, exercised through the shipped function. rcow_common.sh is
# sourced above, so rcow_version_gate_compare *is* the rule under test -- there
# is deliberately no copy of it here.
# ==========================================================================

# mutate_json <src> <dst> <key> <value> [<key> <value> ...]
#
# value is a JSON literal, DELETE, or +N/-N as a delta on the current value.
# Enough to reach every branch of the gate without a second build.
mutate_json()
{
	python3 - "$@" <<'PY'
import json, sys
src, dst, pairs = sys.argv[1], sys.argv[2], sys.argv[3:]
if len(pairs) % 2:
    sys.exit("mutate_json: key/value pairs must come in twos")
d = json.load(open(src))
for i in range(0, len(pairs), 2):
    key, val = pairs[i], pairs[i + 1]
    if val == "DELETE":
        d.pop(key, None)
    elif val[:1] in "+-":
        d[key] = d.get(key, 0) + int(val)
    else:
        d[key] = json.loads(val)
json.dump(d, open(dst, "w"))
PY
}

# rcow_version_gate_compare <running-doc> <candidate-doc>; refusal reasons land
# in ${WORKDIR}/gate.out so a caller can assert on them.
gate_refuses()
{
	if rcow_version_gate_compare "$1" "$2" >"${WORKDIR}/gate.out" 2>&1; then
		fail "the gate accepted $4"
		return 1
	fi
	if ! grep -q "$3" "${WORKDIR}/gate.out"; then
		fail "the gate refused $4 without naming $3"
		return 1
	fi
	pass "the gate refuses $4"
}

gate_accepts()
{
	if rcow_version_gate_compare "$1" "$2" >"${WORKDIR}/gate.out" 2>&1; then
		pass "the gate accepts $3"
	else
		fail "the gate refused $3: $(tr '\n' ' ' <"${WORKDIR}/gate.out")"
		return 1
	fi
}

scenario "[2] version gate: refusals, and the target surviving them"

# No params argument: rcow_get_build_info takes none, and s3lvol_rpc.py sends no
# "params" key at all when the argument is empty.
rpc rcow_get_build_info >"${WORKDIR}/build-running.json" 2>"${WORKDIR}/bi.err" &&
	pass "rcow_get_build_info answered" ||
	fail "rcow_get_build_info failed: $(cat "${WORKDIR}/bi.err")"

"${TGT_BIN}" --print-build-info >"${WORKDIR}/build-new.json" 2>"${WORKDIR}/bi2.err" &&
	pass "s3lvol_tgt --print-build-info answered" ||
	fail "--print-build-info failed: $(cat "${WORKDIR}/bi2.err")"

if [ -s "${WORKDIR}/build-running.json" ] && [ -s "${WORKDIR}/build-new.json" ]; then
	RUNNING="${WORKDIR}/build-running.json"
	NEW="${WORKDIR}/build-new.json"

	gate_accepts "${RUNNING}" "${NEW}" "the running binary against itself"

	# A checkpoint format change across the window: the reason the gate exists.
	mutate_json "${NEW}" "${WORKDIR}/mut-ckpt.json" ckpt_version +1
	gate_refuses "${RUNNING}" "${WORKDIR}/mut-ckpt.json" ckpt_version \
		"a bumped ckpt_version"

	# The journal is the exception that makes the gate necessary: it carries no
	# version field, so journal_op_max is the only thing that distinguishes a
	# newer writer's op values from a torn tail.
	mutate_json "${NEW}" "${WORKDIR}/mut-op.json" journal_op_max +1
	gate_refuses "${RUNNING}" "${WORKDIR}/mut-op.json" journal_op_max \
		"a bumped journal_op_max"

	# The export-version window. Both edges, because an off-by-one here reads
	# as a format change that is not one.
	CUR="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["export_version_max"])' \
		"${RUNNING}")"

	mutate_json "${NEW}" "${WORKDIR}/mut-lo.json" export_version_min "$((CUR + 1))"
	gate_refuses "${RUNNING}" "${WORKDIR}/mut-lo.json" export_version \
		"a candidate whose minimum export version is above the running one"

	mutate_json "${NEW}" "${WORKDIR}/mut-hi.json" export_version_max "$((CUR - 1))"
	gate_refuses "${RUNNING}" "${WORKDIR}/mut-hi.json" export_version \
		"a candidate whose maximum export version is below the running one"

	# An interval that is exactly the running version: the edges are inclusive.
	mutate_json "${NEW}" "${WORKDIR}/mut-edge.json" \
		export_version_min "${CUR}" export_version_max "${CUR}"
	gate_accepts "${RUNNING}" "${WORKDIR}/mut-edge.json" \
		"a candidate whose interval is exactly the running export version"

	# A field the comparison needs, described by only one side. Reported rather
	# than ignored: an absent field that compared equal to another absent one
	# would be a pass for something never compared.
	mutate_json "${NEW}" "${WORKDIR}/mut-one.json" nqn_prefix DELETE
	gate_refuses "${RUNNING}" "${WORKDIR}/mut-one.json" nqn_prefix \
		"a candidate that omits nqn_prefix"

	# The same field absent from both sides is a refusal for the same reason.
	mutate_json "${RUNNING}" "${WORKDIR}/mut-none-run.json" num_subsys DELETE
	mutate_json "${NEW}" "${WORKDIR}/mut-none-new.json" num_subsys DELETE
	gate_refuses "${WORKDIR}/mut-none-run.json" "${WORKDIR}/mut-none-new.json" \
		num_subsys "a field missing from both build-info documents"

	# An SPDK bump is expected and must not refuse: the host keys a disk on its
	# namespace UUID, so the firmware revision and the default model number can
	# both move underneath it.
	mutate_json "${NEW}" "${WORKDIR}/mut-spdk.json" spdk_version '"spdk-0.0.0-test"'
	if rcow_version_gate_compare "${RUNNING}" "${WORKDIR}/mut-spdk.json" \
			>"${WORKDIR}/gate.out" 2>&1; then
		grep -q "spdk-0.0.0-test" "${WORKDIR}/gate.out" &&
			pass "the gate accepts a differing spdk_version and reports both" ||
			fail "the gate accepted a differing spdk_version without reporting it"
	else
		fail "the gate refused a differing spdk_version"
	fi

	# End to end, through the entry point that ships. The candidate is a stub
	# whose only job is to describe itself -- --print-build-info is the one
	# argument the gate passes it -- and it describes a bumped checkpoint format.
	# Named so it can never be mistaken for a target by the instance guard.
	printf '#!/usr/bin/env bash\ncat %s\n' "${WORKDIR}/mut-ckpt.json" \
		>"${WORKDIR}/candidate-gate-stub"
	chmod +x "${WORKDIR}/candidate-gate-stub"

	if "${SCRIPTS}/rcow_upgrade.sh" --candidate "${WORKDIR}/candidate-gate-stub" \
			>"${WORKDIR}/gate-e2e.log" 2>&1; then
		fail "rcow_upgrade.sh proceeded while the version gate refused"
	else
		pass "rcow_upgrade.sh exits non-zero when the gate refuses"
	fi
	grep -q ckpt_version "${WORKDIR}/gate-e2e.log" &&
		pass "the refusal names the field that differed" ||
		fail "the refusal does not say which field it refused on"

	# The point of gating before the kill: a refusal costs nothing. If this ever
	# fails, the suite is looking at a stopped target from here on.
	if rcow_target_alive; then
		pass "the target is still running after the refusal"
	else
		fail "the target is gone after a gate refusal"
	fi
	if rcow_wait_rpc 10 "$(rcow_target_pid)"; then
		pass "the target still answers RPC after the refusal"
	else
		fail "the target stopped answering RPC after a gate refusal"
	fi
	if rcow_verify_active 30 --expect "${WORKDIR}/layout-baseline.json"; then
		pass "the layout is unchanged by the refusal"
	else
		fail "the layout changed during a gate refusal"
	fi
else
	skip "build-info documents unavailable: the version gate cannot be evaluated"
fi

# ==========================================================================
scenario "[3] a new binary that crashes on start rolls back"

# A "new binary" that dies before it can serve RPC. It must not be the real
# binary: the scenario is a replacement that fails, and the rollback is starting
# the old one again. Freshness is skipped for the stub only -- it is not built
# from the sources check_binary_fresh.sh inspects.
mkdir -p "${WORKDIR}/crashbin"
printf '#!/usr/bin/env bash\nexit 1\n' >"${WORKDIR}/crashbin/s3lvol_tgt"
chmod +x "${WORKDIR}/crashbin/s3lvol_tgt"

if "${SCRIPTS}/rcow_upgrade.sh" --candidate "${TGT_BIN}" \
		>"${WORKDIR}/hot_stop.log" 2>&1; then
	pass "rcow_upgrade.sh stopped the target without unloading"
else
	fail "rcow_upgrade.sh failed"
	tail -20 "${WORKDIR}/hot_stop.log" | sed 's/^/       /'
fi

if S3LVOL_SKIP_FRESH_CHECK=1 RCOW_TGT_BIN="${WORKDIR}/crashbin/s3lvol_tgt" \
		"${SCRIPTS}/rcow_start.sh" >"${WORKDIR}/crash_start.log" 2>&1; then
	fail "rcow_start.sh accepted a binary that crashes on start"
else
	pass "rcow_start.sh refused the crashing new binary"
fi

if RCOW_TGT_BIN="${TGT_BIN}" "${SCRIPTS}/rcow_start.sh" \
		>"${WORKDIR}/rollback_start.log" 2>&1; then
	pass "the rollback start brought the old binary back up"
else
	fail "the rollback start failed"
	tail -20 "${WORKDIR}/rollback_start.log" | sed 's/^/       /'
fi

if rcow_verify_active "${VERIFY_TIMEOUT_SEC}" --expect "${WORKDIR}/layout-baseline.json"; then
	pass "the layout survived the failed upgrade and the rollback"
else
	fail "the layout did not survive the rollback"
fi

if rcow_target_alive; then
	pass "the target is running after the rollback"
else
	fail "the target is not running after the rollback"
fi

# ==========================================================================
scenario "[4] fio across the gate refusal and the rollback"

if [ -z "${FIO_PID}" ]; then
	skip "no fio was started: err / io_errors not available"
else
	WAIT_DEADLINE=$(( $(date +%s) + FIO_RUNTIME_SEC ))
	while kill -0 "${FIO_PID}" 2>/dev/null &&
	      [ "$(date +%s)" -lt "${WAIT_DEADLINE}" ]; do
		sleep 2
	done
	json="${WORKDIR}/fio/${VOL}.json"
	if [ ! -s "${json}" ]; then
		fail "fio produced no report (it may have exited on I/O error)"
	elif python3 - "${json}" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
jobs = d.get("jobs", [])
if not jobs:
    print("fio report has no jobs"); sys.exit(1)
err = sum(j.get("error", 0) for j in jobs)
ioerr = sum(j.get("read", {}).get("io_errors", 0) +
            j.get("write", {}).get("io_errors", 0) for j in jobs)
if err or ioerr:
    print("fio error=%d io_errors=%d" % (err, ioerr)); sys.exit(1)
PY
	then
		pass "fio reports 0 errors and 0 io_errors across both scenarios"
	else
		fail "fio saw an error across the gate refusal / rollback"
	fi
fi

# ==========================================================================
scenario "[5] a stale intent marker is not honoured"

# The marker is "<boot_id> <pid> [<candidate>]"; it answers only "did the last
# stop happen because an upgrade asked for it", and carries the binary that
# upgrade intends to start. A stale one must read as absent, because
# RCOW_RUN_DIR (/var/tmp/rcow by default) survives a reboot.
if rcow_hot_marker_write; then
	pass "rcow_hot_marker_write recorded the live target"
	if rcow_hot_marker_consume; then
		pass "a marker for this boot and this target is honoured"
	else
		fail "a marker for this boot and this target was not honoured"
	fi
else
	fail "rcow_hot_marker_write failed with a live target"
fi

# The candidate travels with the marker. The stop path cannot discover it any
# other way: it runs before the versioned directory is switched in, so
# RCOW_TGT_BIN still names the outgoing binary there, and a gate handed that
# would compare the running build with itself.
if rcow_hot_marker_write "${TGT_BIN}"; then
	if rcow_hot_marker_consume; then
		[ "${RCOW_HOT_CANDIDATE}" = "${TGT_BIN}" ] &&
			pass "consume hands back the candidate the writer recorded" ||
			fail "the candidate came back as '${RCOW_HOT_CANDIDATE}', not '${TGT_BIN}'"
	else
		fail "a marker carrying a candidate was not honoured"
	fi
fi

# And a hot stop with no candidate is refused rather than run ungated: the gate
# is the only guard against a new binary that cannot read the formats already on
# disk, and the journal has no version field to catch it later.
if rcow_hot_marker_write; then
	if "${SCRIPTS}/rcow_upgrade.sh" >"${WORKDIR}/nocand.log" 2>&1; then
		fail "a hot stop with no candidate was allowed to proceed"
	else
		pass "a hot stop with no candidate is refused"
	fi
	[ -e "${RCOW_HOT_MARKER}" ] &&
		pass "the refusal left the marker alone" ||
		fail "the refusal removed the marker"
	rcow_target_alive &&
		pass "the refused stop left the target running" ||
		fail "the refused stop killed the target"
fi

# Fake a boot_id change: the pid is still the live target, only the boot differs.
# Consume removes the file first, so each case writes a fresh one.
if rcow_hot_marker_write; then
	real_pid="$(rcow_target_pid)"
	printf '00000000-0000-0000-0000-000000000000 %s\n' "${real_pid}" >"${RCOW_HOT_MARKER}"
	if rcow_hot_marker_consume; then
		fail "a marker naming a different boot was honoured"
	else
		pass "a marker naming a different boot is refused"
	fi
fi

# A boot_id that matches but a pid that is not the live target. Two readings are
# possible -- a reboot whose pid was reused, or a live intent this host cannot
# confirm -- and they want opposite handling, so consume answers 2 rather than 1
# and the caller refuses instead of falling through to the planned stop. The
# marker has to survive: the intent is the operator's, not this script's.
if rcow_hot_marker_write; then
	boot="$(cat /proc/sys/kernel/random/boot_id)"
	printf '%s %s\n' "${boot}" "999999" >"${RCOW_HOT_MARKER}"
	rcow_hot_marker_consume
	rc=$?
	[ "${rc}" -eq 2 ] &&
		pass "a marker naming a non-target pid answers 2, not 1" ||
		fail "a non-target pid answered ${rc}, not 2"
	[ -e "${RCOW_HOT_MARKER}" ] &&
		pass "the marker survives a pid the host cannot confirm" ||
		fail "the marker was discarded although the intent may be live"
	rcow_target_alive &&
		pass "the target is untouched by the refusal" ||
		fail "the refusal killed the target"
fi

# Consume is one-shot: the file must be gone after it runs.
rcow_hot_marker_write >/dev/null 2>&1
rcow_hot_marker_consume >/dev/null 2>&1
[ ! -e "${RCOW_HOT_MARKER}" ] &&
	pass "consume removed the marker it answered" ||
	fail "the marker survived consume"

# The startup path removes it unconditionally, so a process that never intended
# a hot restart does not inherit one. Prove it by writing a marker and starting.
rcow_hot_marker_write >/dev/null 2>&1
"${SCRIPTS}/rcow_stop.sh" --force >/dev/null 2>&1
"${SCRIPTS}/rcow_start.sh" >"${WORKDIR}/marker_start.log" 2>&1
[ ! -e "${RCOW_HOT_MARKER}" ] &&
	pass "rcow_start.sh cleared the marker at startup" ||
	fail "the marker survived a cold start"
rcow_verify_active "${VERIFY_TIMEOUT_SEC}" >/dev/null 2>&1 &&
	pass "the target is back with the volume active" ||
	fail "the target did not come back after the cold start"

# ==========================================================================
scenario "[6] reconnect_delay of 0 or negative is refused before sysfs"

CTRL="$(tunable_ctrl)" || CTRL=""
BEFORE_RD=""
[ -n "${CTRL}" ] && BEFORE_RD="$(attr "${CTRL}/reconnect_delay")"

for bad in 0 -1 ''; do
	if rcow_tune_initiator_timeouts --reconnect-delay "${bad}" >/dev/null 2>&1; then
		fail "reconnect_delay='${bad}' was accepted"
	else
		pass "reconnect_delay='${bad}' is refused (rc 1)"
	fi
done

if [ -n "${CTRL}" ]; then
	AFTER_RD="$(attr "${CTRL}/reconnect_delay")"
	[ "${AFTER_RD}" = "${BEFORE_RD}" ] &&
		pass "the refused values never reached sysfs (${CTRL}/reconnect_delay unchanged)" ||
		fail "sysfs reconnect_delay changed to '${AFTER_RD}' despite the refusal"
else
	skip "no tunable controller: cannot confirm the refused values missed sysfs"
fi

# ==========================================================================
scenario "[7] wrong write order collapses the retry budget, and the read-back sees it"

if [ -z "${CTRL}" ]; then
	skip "no tunable controller on this kernel: the wrong write order cannot be injected"
else
	RD="${CTRL}/reconnect_delay"
	CLT="${CTRL}/ctrl_loss_tmo"

	# The wrong order: ctrl_loss_tmo written first (recomputing max_reconnects
	# against the old delay), then reconnect_delay, which the kernel does not
	# recompute against. Deliberately writes what the function must never do.
	printf '10\n' >"${RD}"
	printf '%s\n' "${RESTORE_TMO}" >"${CLT}"
	printf '1\n' >"${RD}"

	got="$(attr "${CLT}")"
	# The count the kernel is left holding: it was computed against the original
	# delay (10) and never recomputed, so ctrl_loss_tmo now shows that count
	# scaled by the new delay (1) -- the order-of-magnitude collapse this injects.
	old_delay=10
	want=$(( (RESTORE_TMO + old_delay - 1) / old_delay ))
	if [ "${got}" = "${RESTORE_TMO}" ]; then
		fail "the read-back saw the full budget ${RESTORE_TMO}: the wrong-order injection did not take"
	elif [ "${got}" = "${want}" ]; then
		pass "the read-back detects the collapse: ctrl_loss_tmo reads ${got} (${want} expected when collapsed), not ${RESTORE_TMO}"
	else
		pass "the read-back detects a mismatch: ctrl_loss_tmo reads ${got}, not ${RESTORE_TMO}"
	fi

	# Restore through the function -- the positive control that the correct
	# order reads back as expected. It only is one with the delay somewhere
	# other than RESTORE_DELAY: at its target value already, a swapped order
	# writes the same values and reads back the same.
	printf '10\n' >"${RD}"

	if rcow_tune_initiator_timeouts --reconnect-delay "${RESTORE_DELAY}" \
			--ctrl-loss-tmo "${RESTORE_TMO}"; then
		expect=$(( (RESTORE_TMO + RESTORE_DELAY - 1) / RESTORE_DELAY * RESTORE_DELAY ))
		[ "$(attr "${CLT}")" = "${expect}" ] &&
			pass "the correct order restores ctrl_loss_tmo to ${expect}" ||
			fail "restore read back $(attr "${CLT}"), expected ${expect}"
	else
		fail "rcow_tune_initiator_timeouts could not restore the timeouts"
	fi
fi

# ==========================================================================
scenario "[8] writing only reconnect_delay collapses the budget too"

if [ -z "${CTRL}" ]; then
	skip "no tunable controller on this kernel: the wrong write order cannot be injected"
else
	RD="${CTRL}/reconnect_delay"
	CLT="${CTRL}/ctrl_loss_tmo"

	# A full budget first, then a delay-only write: no ctrl_loss_tmo write
	# follows, so max_reconnects stays at its old count while the delay shrinks.
	printf '10\n' >"${RD}"
	printf '%s\n' "${RESTORE_TMO}" >"${CLT}"
	got_full="$(attr "${CLT}")"
	printf '1\n' >"${RD}"
	got_only="$(attr "${CLT}")"

	if [ "${got_full}" = "${RESTORE_TMO}" ] && [ "${got_only}" != "${RESTORE_TMO}" ]; then
		pass "a delay-only write collapses ctrl_loss_tmo from ${got_full} to ${got_only}"
	elif [ "${got_only}" = "${RESTORE_TMO}" ]; then
		fail "a delay-only write left ctrl_loss_tmo at ${RESTORE_TMO}: the collapse was not observed"
	else
		info "ctrl_loss_tmo now reads ${got_only} (full ${got_full}, wanted ${RESTORE_TMO})"
		pass "a delay-only write moved ctrl_loss_tmo off the wanted value"
	fi

	if rcow_tune_initiator_timeouts --reconnect-delay "${RESTORE_DELAY}" \
			--ctrl-loss-tmo "${RESTORE_TMO}"; then
		pass "the timeouts are restored after the delay-only injection"
	else
		fail "could not restore the timeouts after the delay-only injection"
	fi
fi

# ==========================================================================
scenario "[9] a second target is refused, and the guard finds an unlinked one"

# (a) With the target from [5] alive, a start must refuse rather than overlap.
if "${SCRIPTS}/rcow_start.sh" >"${WORKDIR}/second_start.log" 2>&1; then
	fail "rcow_start.sh started a second target over a live one"
else
	pass "rcow_start.sh refuses a second target over a live one"
fi
grep -qiE 'already running|does not account for' "${WORKDIR}/second_start.log" &&
	pass "the refusal says a target is already running" ||
	fail "the refusal did not name the running target"

# (b) The basename fallback: point at a different path with the same
# basename. The realpath sweep misses the live process; the basename sweep must
# not. A copy is used so the shared build artifact is never touched.
mkdir -p "${WORKDIR}/other"
cp "${TGT_BIN}" "${WORKDIR}/other/s3lvol_tgt" 2>/dev/null ||
	fail "could not stage a second-path binary for the basename check"
live_pid="$(rcow_target_pid)"
found="$(RCOW_TGT_BIN="${WORKDIR}/other/s3lvol_tgt" bash -c '
	. "$1/scripts/rcow_common.sh"
	rcow_target_instances
' _ "${ROOT}" 2>/dev/null)"
case " ${found} " in
*" ${live_pid} "*) pass "the basename fallback still finds the live target at a swapped path" ;;
*) fail "the basename fallback missed the live target (found '${found}', live ${live_pid})" ;;
esac

# ==========================================================================
scenario "[10] a window past ctrl_loss_tmo makes controller deletion observable"

# This asserts the *failure mode is detectable*, not that it is prevented --
# when the rollback budget really runs out, the kernel deletes the controller
# and the host's I/O turns to EIO. Destructive, so it runs last; the target is
# brought back afterwards so cleanup has something to stop.
if [ -z "${CTRL}" ]; then
	skip "no tunable controller on this kernel: the ctrl_loss_tmo window cannot be shortened"
elif ! rcow_tune_initiator_timeouts --reconnect-delay 1 \
		--ctrl-loss-tmo "${SHORT_CTRL_LOSS_TMO}"; then
	skip "could not shorten ctrl_loss_tmo (rc nonzero); nothing to stretch"
else
	pass "ctrl_loss_tmo shortened to ${SHORT_CTRL_LOSS_TMO}s on this suite's controllers"

	# I/O in flight across the window. With nothing outstanding, deleting the
	# controller has no request to fail and the scenario proves nothing; the
	# read's own failure is asserted below. Resolved once -- the gendisk
	# survives the kill, that being the whole point, so the path is good for the
	# life of the window. Bounded, so it stops by itself if the deletion never
	# comes.
	reader_dev="$(vol_dev "${VOL}")"
	if [ ! -b "${reader_dev}" ]; then
		fail "no block device for ${VOL} (got '${reader_dev}'): the window \
cannot be watched under I/O"
	else
		(
			reader_end=$(( $(date +%s) + DELETE_WATCH_SEC ))
			while [ "$(date +%s)" -lt "${reader_end}" ]; do
				# iflag=direct, as everywhere else this tree reads a device.
				# Buffered, every pass after the first re-reads the same
				# ceiling from the page cache and issues no request at all,
				# so the deletion would have nothing to fail.
				dd if="${reader_dev}" of=/dev/null bs=1M count=64 \
					iflag=direct 2>"${WORKDIR}/reader.err"
				rc=$?
				printf '%s\n' "${rc}" >"${WORKDIR}/reader.rc"
				[ "${rc}" -eq 0 ] || break
			done
		) &
		reader=$!
	fi

	before_ctrls="$(our_controllers | wc -l)"
	before_del="$(dmesg_count 'I/O error')"
	before_rm="$(dmesg_count "Removing ctrl: NQN \"${RCOW_NQN_PREFIX}")"

	"${SCRIPTS}/rcow_upgrade.sh" --candidate "${TGT_BIN}" \
		>"${WORKDIR}/tmo_stop.log" 2>&1 ||
		fail "rcow_upgrade.sh failed before the window test"

	deadline=$(( $(date +%s) + DELETE_WATCH_SEC ))
	deleted=0
	while [ "$(date +%s)" -lt "${deadline}" ]; do
		if [ "$(our_controllers | wc -l)" -lt "${before_ctrls}" ] || any_ctrl_deleting; then
			deleted=1
			break
		fi
		sleep 1
	done

	# The queued requests are failed as the queues are torn down, so the dmesg
	# line trails the controller leaving sysfs. Settle before reading, or the
	# fast detection path reads a count that has not moved yet.
	sleep 3
	after_del="$(dmesg_count 'I/O error')"
	if [ "${deleted}" -eq 1 ]; then
		pass "a controller was observed going away after the budget expired"
	elif [ "${after_del}" -gt "${before_del}" ]; then
		pass "dmesg gained an I/O error after the budget expired (deletion logged)"
	else
		fail "no controller deletion and no I/O error observed within ${DELETE_WATCH_SEC}s"
	fi

	# The host's own record. What an operator watching the node sees when the
	# budget runs out is the kernel naming the controller it is deleting -- not
	# an "I/O error" line. A *direct* read that fails on a removed controller
	# returns EIO to its caller and never reaches the buffer cache, so only the
	# buffered path (page-cache writeback) emits "Buffer I/O error". Measured on
	# 6.6.69: a dd blocked for the whole window and came back short, and dmesg
	# gained nothing but the "Removing ctrl" lines counted here.
	after_rm="$(dmesg_count "Removing ctrl: NQN \"${RCOW_NQN_PREFIX}")"
	if [ "${after_rm}" -gt "${before_rm}" ]; then
		pass "dmesg names one of this suite's controllers being removed: the host logged the deletion"
	else
		fail "a controller went away but dmesg never named it"
	fi

	# The workload's half of "not silent": the read that spanned the window
	# failed rather than hanging forever. Either the read itself failed short or
	# the next open found the device gone; both are non-zero.
	reader_rc="$(cat "${WORKDIR}/reader.rc" 2>/dev/null || true)"
	if [ -n "${reader_rc}" ] && [ "${reader_rc}" -ne 0 ]; then
		pass "the read spanning the window failed (dd rc=${reader_rc}) instead of hanging"
	else
		fail "no read failed across the window: the deletion had nothing outstanding to fail"
	fi

	# Served its purpose either way. A dd blocked on the deleted controller fails
	# and exits on its own; this covers the loop's own next iteration.
	[ -z "${reader:-}" ] || kill "${reader}" 2>/dev/null

	# Bring the target and the host back so the suite leaves a clean machine.
	if RCOW_TGT_BIN="${TGT_BIN}" "${SCRIPTS}/rcow_start.sh" \
			>"${WORKDIR}/tmo_restart.log" 2>&1; then
		pass "the target and the host reconnected after the destructive scenario"
	else
		fail "could not bring the target back after the destructive scenario"
		tail -15 "${WORKDIR}/tmo_restart.log" | sed 's/^/       /'
	fi
fi

# ==========================================================================
scenario "[11] target log"

if rcow_target_alive; then
	pass "the target is running at the end of the run"
else
	fail "the target is not running at the end of the run"
fi

if grep -qE 'Assertion|SIGSEGV|panic:' "${RCOW_LOG}" 2>/dev/null; then
	fail "assertion or fault in the target log"
	grep -nE 'Assertion|SIGSEGV|panic:' "${RCOW_LOG}" | head -5 | sed 's/^/       /'
else
	pass "no asserts and no faults in the recovered target's log"
fi

# ==========================================================================
scenario "[12] the hot stop's own structure: what it must not contain"

# A tripwire over the shipped script, not a proof of its behaviour -- behaviour
# is what scenarios [2], [3] and [10] exercise. It earns its place because the
# four prohibitions are the ones whose violation is unrecoverable: a disconnect
# or an unload removes the namespace under a live sandbox, and a write to the
# registry, to bstore.json or to the WAL image destroys the state the next
# target attaches to. Each is one careless line away, and each would leave the
# other scenarios green.
#
# The script's own header names every one of these calls and files while
# forbidding them, so comments are stripped before looking: a check that read
# the prose would fire on its own documentation.
if [ ! -s "${SCRIPTS}/rcow_upgrade.sh" ]; then
	fail "rcow_upgrade.sh is missing: the prohibitions cannot be checked"
else
	while IFS='|' read -r verdict what detail; do
		case "${verdict}" in
		ok)  pass "${what}" ;;
		bad) fail "${what}: ${detail}" ;;
		esac
	done < <(python3 - "${SCRIPTS}/rcow_upgrade.sh" <<'PY'
import re, sys

# Continuations are joined before comments are dropped: the residue list is
# split across two lines by a backslash, and a check that saw only the first
# half would report a removal the script does not perform.
raw = open(sys.argv[1], encoding="utf-8").read().replace("\\\n", " ")
lines = [re.sub(r"#.*$", "", line) for line in raw.split("\n")]
code = "\n".join(lines)

# A mutator, anchored so rm/mv/cp match as commands and not as substrings of a
# path; and a redirect into a variable-named file.
CMD = re.compile(r"(?:^|[;&|(])\s*(?:rm|mv|cp|install|truncate|dd|tee)\s")
REDIR = re.compile(r">>?\s*\"?\$\{?[A-Z_]+")

FORBIDDEN = {
    "RCOW_ACTIVE_FILE": "the registry the next target replays from",
    "RCOW_BSTORE_FILE": "what chooses attach over create",
    "RCOW_WAL_IMG": "acknowledged writes not yet in S3",
    "RCOW_HOT_MARKER": "the marker only the orchestrator writes",
}
RESIDUE = ("RCOW_PIDFILE", "RCOW_RPC_SOCK", "RCOW_RPC_SOCK}.lock",
           "spdk_cpu_lock_")

def emit(ok, what, detail=""):
    print("%s|%s|%s" % ("ok" if ok else "bad", what, detail))

# Both spellings of a disconnect: the raw command, and the helper this tree
# actually disconnects through. Not anchored to the start of a line, because
# the helper is called as `rcow_disconnect_all` at the start of a statement.
m = re.search(r"(nvme\s+disconnect[\w-]*|rcow_disconnect\w*)", code)
if m:
    emit(False, "the hot stop never disconnects the initiator",
         "it calls %s" % m.group(1))
else:
    emit(True, "the hot stop never disconnects the initiator")

m = re.search(r"\brcow_unload_lvstore\b", code)
if m:
    emit(False, "the hot stop never unloads the lvstore",
         "it calls rcow_unload_lvstore")
else:
    emit(True, "the hot stop never unloads the lvstore")

hits = []
for i, line in enumerate(lines, 1):
    if not (CMD.search(line) or REDIR.search(line)):
        continue
    for name in FORBIDDEN:
        if name in line:
            hits.append("line %d mutates %s (%s): %s"
                        % (i, name, FORBIDDEN[name], line.strip()))
if hits:
    for h in hits:
        emit(False, "the hot stop never touches the state the next target needs", h)
else:
    emit(True, "the hot stop never touches the state the next target needs")

# The other half, which "mutates nothing" would also satisfy: what it does
# remove is the residue and only the residue.
rm_lines = [line for line in lines if CMD.search(line)]
missing = [n for n in RESIDUE if not any(n in line for line in rm_lines)]
if missing:
    emit(False, "the hot stop clears exactly the four residues",
         "not removed: %s" % ", ".join(missing))
else:
    emit(True, "the hot stop clears exactly the four residues")
PY
	)
fi
