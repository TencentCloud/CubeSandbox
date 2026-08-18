#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  rcow_stop.sh -- take the rcow data plane down without losing writes
#
#  Order, and what each step is for:
#
#    1. nvme disconnect     stop the host from issuing new I/O. Doing this after
#                           the unload instead would leave a mounted filesystem
#                           writing into a namespace whose lvstore is gone, which
#                           the host reports as I/O errors on a device that was
#                           working a second ago.
#    2. rcow_unload_lvstore drain everything acknowledged so far into S3 and close
#                           the log. This is the only clean shutdown on the WAL
#                           path; killing the process instead is safe but leaves a
#                           tail that the next attach has to replay.
#    3. SIGTERM the target  and SIGKILL only after the budget runs out.
#
#  === What is deliberately not done ===
#
#  Volumes are not deactivated. /data/cubelet/rcow/active_lvols is the record
#  of the
#  host-side layout, and it is what rcow_start.sh replays; emptying it here would
#  turn every planned restart into a node that comes back with no volumes
#  exposed. The namespaces disappear with the process either way.
#
#  bstore.json is not touched either, for the same reason from the other
#  direction: it is what tells the next start to attach rather than create.
#
#  === The budget ===
#
#  RCOW_STOP_TIMEOUT (default 120s) covers all three steps together, not each. An
#  unload that is still flushing at the deadline is interrupted, because a stop
#  that can hang indefinitely is not a stop -- and the WAL means interrupting it
#  costs a longer attach next time, not data.
#
#  Usage: rcow_stop.sh [--force] [--keep-connected] [--lvs-name NAME]
#
#    --force           skip the unload and go straight to signalling. For a
#                      target that is wedged; the next attach replays the log.
#    --keep-connected  leave the initiator connected. Only useful when the target
#                      is about to be restarted immediately.
#    --lvs-name        unload this lvstore instead of the one derived from the
#                      hostname. Needed when the target was started with
#                      rcow_start.sh --lvs-name; without it the unload would
#                      miss the attached lvstore and only the WAL replay would
#                      save the shutdown.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=rcow_common.sh
. "${SELF_DIR}/rcow_common.sh"

FORCE=0
KEEP_CONNECTED=0

while [ "$#" -gt 0 ]; do
	case "$1" in
	--force)          FORCE=1 ;;
	--keep-connected) KEEP_CONNECTED=1 ;;
	--lvs-name)       shift; RCOW_LVS_NAME="${1:-}" ;;
	-h|--help)        sed -n '2,50p' "${BASH_SOURCE[0]}"; exit 0 ;;
	*)                rcow_die "unknown option: $1 (try --help)" ;;
	esac
	shift
done

rcow_need_root
rcow_ensure_run_dir

DEADLINE=$((SECONDS + RCOW_STOP_TIMEOUT))
remaining() { local r=$((DEADLINE - SECONDS)); [ "${r}" -lt 0 ] && r=0; printf '%s' "${r}"; }

TGT_PID="$(rcow_target_pid)" || TGT_PID=""

# The pidfile is the fast path, not the only one. It can be missing or
# unreadable while a target is very much alive -- an earlier stop that gave up
# halfway, a hand-started target, a pidfile someone cleaned out -- and in that
# state the dangerous move is to report "nothing running" and carry on, because
# the next start would take over the RPC socket and leave the live process
# holding the lvstore and the WAL with nothing able to reach it.
#
# So when the pidfile turns up nothing, look for the process itself.
if [ -z "${TGT_PID}" ]; then
	ORPHANS="$(rcow_target_instances)"

	if [ -n "${ORPHANS}" ]; then
		rcow_warn "no usable pidfile, but the target binary is running as: \
$(printf '%s' "${ORPHANS}" | tr '\n' ' ')"

		if [ "$(printf '%s\n' "${ORPHANS}" | wc -l)" -gt 1 ]; then
			# Two live targets over one WAL image is not a state to
			# tidy up automatically: whichever is stopped second may
			# have been writing the whole time.
			rcow_die "more than one target instance is running. Work \
out which one owns ${RCOW_WAL_IMG} and stop it by hand; stopping them in the \
wrong order can lose acknowledged writes"
		fi

		TGT_PID="$(printf '%s' "${ORPHANS}")"
		rcow_log "adopting pid ${TGT_PID}"

		# Its RPC socket may have been unlinked by a later start, in which
		# case the unload below cannot reach it and the WAL is what makes
		# the shutdown survivable.
		if [ ! -S "${RCOW_RPC_SOCK}" ]; then
			rcow_warn "${RCOW_RPC_SOCK} is gone, so this target cannot \
be asked to unload; it will be signalled instead and the next attach will \
replay the log"
			FORCE=1
		fi
	fi
fi

if [ -z "${TGT_PID}" ]; then
	rcow_log "no target is running"
	if [ "${KEEP_CONNECTED}" -eq 0 ]; then
		# The controllers outlive the target: without a disconnect they sit
		# there in a reconnect loop, and the next start hands them namespaces
		# from a process they were not connected to.
		rcow_step "initiator: disconnecting leftover controllers"
		rcow_disconnect_all
	fi
	# Safe to remove only because the scan above found no live instance.
	rm -f "${RCOW_PIDFILE}"
	exit 0
fi

rcow_log "target pid ${TGT_PID}, budget ${RCOW_STOP_TIMEOUT}s"

# ==========================================================================
if [ "${KEEP_CONNECTED}" -eq 0 ]; then
	rcow_step "initiator: disconnecting"
	rcow_disconnect_all
else
	rcow_step "initiator: left connected (--keep-connected)"
fi

# ==========================================================================
if [ "${FORCE}" -eq 1 ]; then
	rcow_step "unload: skipped (--force)"
	rcow_warn "the log will not be closed; the next attach replays its tail"
else
	rcow_step "unloading lvstore '${RCOW_LVS_NAME}'"

	# Leave a share of the budget for the process shutdown itself, and at least
	# ten seconds for the unload to be worth attempting at all. 60s is reserved
	# because that is the order of magnitude measured: a target sent SIGTERM while an lvstore was still attached took ~30s to tear down the S3
	# client and the thread spawner. After a successful unload it is far
	# quicker, but the budget has to cover the case where the unload failed.
	UNLOAD_BUDGET=$(( $(remaining) - 60 ))
	[ "${UNLOAD_BUDGET}" -lt 10 ] && UNLOAD_BUDGET=10

	UNLOAD_OUT="$(RCOW_RPC_TIMEOUT="${UNLOAD_BUDGET}" \
		rcow_rpc rcow_unload_lvstore \
		"$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")" 2>&1)" && UNLOAD_RC=0 ||
		UNLOAD_RC=1

	if [ "${UNLOAD_RC}" -eq 0 ]; then
		rcow_log "unloaded: everything acknowledged is in S3 and the log is closed"
	else
		# Not fatal. The lvstore may simply not have been attached (a start
		# that failed at that step), and when it was, the WAL is exactly the
		# mechanism that makes an interrupted shutdown survivable.
		rcow_warn "rcow_unload_lvstore did not complete: ${UNLOAD_OUT}"
		rcow_warn "continuing to shut down; the next attach will replay the log"
	fi
fi

# ==========================================================================
rcow_step "stopping the target"

TERM_BUDGET="$(remaining)"
[ "${TERM_BUDGET}" -lt 5 ] && TERM_BUDGET=5

if rcow_kill_target "${TGT_PID}" "${TERM_BUDGET}"; then
	rcow_log "target stopped"
else
	rcow_die "pid ${TGT_PID} survived SIGKILL; something outside this script \
is holding it"
fi

rm -f "${RCOW_PIDFILE}" "${RCOW_RPC_SOCK}" "${RCOW_RPC_SOCK}.lock" \
	/var/tmp/spdk_cpu_lock_*

# ==========================================================================
rcow_step "down"

if [ -s "${RCOW_ACTIVE_FILE}" ]; then
	rcow_log "$(rcow_registry_tsv "${RCOW_ACTIVE_FILE}" | wc -l) volume(s) are \
still recorded in ${RCOW_ACTIVE_FILE}; rcow_start.sh will put them back on the \
same subsystem and nsid"
else
	rcow_log "no volumes were active"
fi

exit 0
