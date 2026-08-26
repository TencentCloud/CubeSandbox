#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  rcow_recovery.sh -- make the running data plane match the recorded layout
#
#  There are three situations this has to tell apart, and only one of them is a
#  recovery in the interesting sense.
#
#  1. The target is not running.
#
#     Then the recovery *is* a start: the journal has to be replayed, the WAL
#     drained and the namespaces re-attached, in that order, and rcow_start.sh
#     already does all of it. So this hands over rather than reimplementing a
#     second bring-up path that would drift from the first.
#
#  2. The target is running and a replay plan is left over.
#
#     A previous replay died part way through. Resuming is safe, and safe for a
#     specific reason: the plan is consulted only after the live registry has
#     been removed, so the entries still in the plan are exactly the ones this
#     process has not attached. Volumes already restored are in the live registry
#     and in this process's memory, which is what rcow_active_bdev's idempotent
#     branch is for.
#
#  3. The target is running with no plan outstanding.
#
#     Then there is nothing to replay, and the only useful thing to do is check
#     that the claim the registry makes is true -- that every volume it lists is
#     visible to the host. A verification, not a repair.
#
#  === Why case 3 does not attempt a repair ===
#
#  If a volume is recorded as active but no device exists, the fix is not another
#  rcow_active_bdev call: that RPC sees the name in the registry and reports
#  success without attaching anything, which is right for a retry and useless
#  here. The mismatch means the registry was loaded by a process that never
#  attached those namespaces, and the way out is a restart -- which case 1
#  handles properly, by moving the registry aside first. Pretending to repair it
#  from here would report a restored layout that is not there, and that is the
#  one failure this whole mechanism exists to prevent.
#
#  Usage: rcow_recovery.sh [--verify-only] [--timeout SEC]
#
#    --verify-only  never start anything and never attach anything; just report.
#                   Suitable for a health check.
#    --timeout SEC  how long to wait for the host to publish devices (default 60).

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=rcow_common.sh
. "${SELF_DIR}/rcow_common.sh"

VERIFY_ONLY=0
VERIFY_TIMEOUT=60

while [ "$#" -gt 0 ]; do
	case "$1" in
	--verify-only) VERIFY_ONLY=1 ;;
	--timeout)     shift; VERIFY_TIMEOUT="${1:-60}" ;;
	-h|--help)     sed -n '2,48p' "${BASH_SOURCE[0]}"; exit 0 ;;
	*)             rcow_die "unknown option: $1 (try --help)" ;;
	esac
	shift
done

rcow_need_root
rcow_ensure_run_dir

RECORDED=0
if [ -s "${RCOW_ACTIVE_FILE}" ]; then
	RECORDED="$(rcow_registry_tsv "${RCOW_ACTIVE_FILE}" | wc -l)" ||
		rcow_die "${RCOW_ACTIVE_FILE} does not parse; decide by hand what it \
should have said before starting anything"
fi
PLANNED=0
if [ -s "${RCOW_REPLAY_FILE}" ]; then
	PLANNED="$(rcow_registry_tsv "${RCOW_REPLAY_FILE}" | wc -l)" || PLANNED="?"
fi

rcow_log "${RECORDED} volume(s) recorded active, ${PLANNED} outstanding in a \
replay plan"

# ==========================================================================
# Case 1: nothing is running.
# ==========================================================================
if ! TGT_PID="$(rcow_target_pid)"; then
	if [ "${VERIFY_ONLY}" -eq 1 ]; then
		rcow_err "the target is not running (--verify-only, so it is not \
being started)"
		exit 1
	fi

	rcow_step "no target is running: recovery is a start"
	rcow_log "handing over to rcow_start.sh, which replays the journal, drains \
the WAL and restores the recorded namespaces"
	exec "${SELF_DIR}/rcow_start.sh"
fi

rcow_log "target is running as pid ${TGT_PID}"

# ==========================================================================
# Case 2: an interrupted replay.
# ==========================================================================
if [ -s "${RCOW_REPLAY_FILE}" ]; then
	if [ "${VERIFY_ONLY}" -eq 1 ]; then
		rcow_err "${PLANNED} volume(s) are still waiting to be restored \
(${RCOW_REPLAY_FILE}); --verify-only will not attach them"
		exit 1
	fi

	rcow_step "resuming the interrupted replay"
	REPLAY_RC=0
	rcow_replay_registry || REPLAY_RC=1
else
	REPLAY_RC=0
fi

# ==========================================================================
# Case 3: verify what the registry claims.
# ==========================================================================
rcow_step "verifying that every active volume is visible to the host"

if [ ! -s "${RCOW_ACTIVE_FILE}" ]; then
	rcow_log "no volumes are active; nothing to verify"
	exit "${REPLAY_RC}"
fi

if rcow_verify_active "${VERIFY_TIMEOUT}"; then
	# Recovery is also the "make this host usable again" entry point, and a
	# volume that was re-activated by a replay above comes up with the kernel
	# default readahead. Best effort, never fatal.
	rcow_tune_readahead
	exit "${REPLAY_RC}"
fi

# The registry lists volumes the host cannot see, and no replay is outstanding.
# Almost always: the target was restarted without a replay (rcow_start.sh
# --no-replay, or a target started by hand), so it loaded the registry and now
# reports those volumes as active while holding no namespaces for them.
rcow_err "the registry lists volumes this process is not exposing"
rcow_err "re-activating them from here would not help: rcow_active_bdev sees \
the name in the registry and reports success without attaching anything"
rcow_err "restart instead, which replays the layout from a copy: \
rcow_stop.sh && rcow_start.sh"
exit 1
