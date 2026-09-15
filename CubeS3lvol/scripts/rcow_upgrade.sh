#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
#
#  rcow_upgrade.sh -- stop the target so the host keeps its namespaces
#
#  The planned shutdown (rcow_stop.sh) disconnects the initiator and then
#  unloads the lvstore, and both of those break a live sandbox's I/O. A hot
#  restart wants neither: the target is killed outright and the replacement
#  rebuilds the same NQN/NSID/UUID layout, so the kernel's nvme_tcp reconnects
#  to the same /dev/nvmeXnY and business I/O only pauses. This script does the
#  online half of that and then kills the process and clears its residue;
#  bringing the replacement up is rcow_start.sh's job.
#
#  Order, and what each step is for:
#
#    1. one live target that answers RPC   two targets over one WAL cannot be
#                                          recovered from. With none, or with one
#                                          that cannot be reached, there is
#                                          nothing to pause and only residue
#                                          left to clear
#    2. version gate, when --candidate     the new binary has to accept the
#                                          formats on disk; only here are both
#                                          descriptions known at once
#    3. rcow_flush_lvstore      push everything acknowledged to S3; online
#    4. rcow_checkpoint_lvstore snapshot the chunk map and truncate the journal,
#                               which is what keeps the next attach short
#    5. snapshot the layout     the file rcow_verify_active --expect compares to
#    6. SIGKILL the target      a crash, not a shutdown: a crash is the one exit
#                               guaranteed to leave the namespace in place and
#                               drive the host into error recovery
#    7. clear four leftovers    pidfile, RPC socket, its .lock, cpu locks
#
#  === What this must never do ===
#
#    - nvme disconnect, in any form: it deletes the controllers and their
#      gendisks, which is breakage rather than a pause.
#    - rcow_unload_lvstore: unregistering a bdev makes SPDK remove the namespace
#      and send the host a NS_ATTR_CHANGED AEN, which it answers by removing the
#      gendisk.
#    - touch active_lvols, bstore.json or any WAL image: active_lvols is what
#      the replay restores from, bstore.json is what chooses attach over create
#      (create formats the WAL), and the WAL holds acknowledged writes not yet
#      in S3.
#    - write the hot-restart marker: the orchestrator writes it and the stop
#      script consumes it. Writing it here would make an unasked-for stop look
#      like an upgrade's.
#
#  Usage: rcow_upgrade.sh [--dry-run] [--candidate <binary>]
#
#    --dry-run    run the online steps and print what would be killed and removed,
#                 without killing or removing anything.
#    --candidate  check the version gate against this binary before anything is
#                 touched, and refuse if the two builds cannot share the on-disk
#                 state. Optional: without it the caller is either the mechanism
#                 test, which upgrades a binary to itself, or an operator who has
#                 decided deliberately.

set -u

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=rcow_common.sh
. "${SELF_DIR}/rcow_common.sh"

DRY_RUN=0
CANDIDATE=""

while [ "$#" -gt 0 ]; do
	case "$1" in
	--dry-run)   DRY_RUN=1 ;;
	--candidate) shift; CANDIDATE="${1:-}"
		# An empty value would read as "no gate asked for" further down, and
		# the caller asking for a gate is exactly the caller that must not
		# get one silently skipped.
		[ -n "${CANDIDATE}" ] || rcow_die "--candidate needs a binary path" ;;
	-h|--help)   sed -n '2,57p' "${BASH_SOURCE[0]}"; exit 0 ;;
	*)           rcow_die "unknown option: $1 (try --help)" ;;
	esac
	shift
done

rcow_need_root
rcow_ensure_run_dir

TGT_PID=""

# Every failure before the kill leaves the target untouched and unsignalled, so
# the upgrade has not half-happened: the old process still holds the WAL and no
# acknowledged write is at risk. A half-killed node is worse than one that never
# started.
fail_live()
{
	rcow_err "$*"
	rcow_err "the target (pid ${TGT_PID}) is still running and was not \
signalled; no residue was removed. Nothing acknowledged is at risk: the WAL is \
still this process's"
	exit 1
}

# rcow_flush_lvstore and rcow_checkpoint_lvstore answer -EBUSY while another of
# their kind is running -- they do not queue. That is "come back later", not a
# failure, so back off and retry within the stop budget. Anything else is fatal,
# except an outcome the caller names in $4: that one is survivable by
# construction, and retrying it would only spend the budget to arrive here again.
hot_online_op()
{
	local label="$1" method="$2" params="$3" tolerate="${4:-}"
	local out delay=2 deadline=$((SECONDS + RCOW_STOP_TIMEOUT))

	while :; do
		# rcow_rpc defaults to RCOW_RPC_TIMEOUT, which is longer than this
		# script's whole budget, so a call that hangs would outlive the deadline
		# the retry loop is watching. What is left of that budget is also this
		# call's ceiling.
		local left=$((deadline - SECONDS))
		[ "${left}" -lt 1 ] && left=1
		if out="$(RCOW_RPC_TIMEOUT="${left}" rcow_rpc "${method}" "${params}" 2>&1)"; then
			rcow_log "${label}: done"
			return 0
		fi
		if [ -n "${tolerate}" ] && [[ "${out}" == *"${tolerate}"* ]]; then
			rcow_warn "${label}: ${out}"
			return 0
		fi
		case "${out}" in
		*[Bb][Uu][Ss][Yy]*)
			if [ "${SECONDS}" -ge "${deadline}" ]; then
				rcow_err "${label}: still busy after \
$((RCOW_STOP_TIMEOUT))s: ${out}"
				return 1
			fi
			rcow_warn "${label}: busy, retrying in ${delay}s"
			sleep "${delay}"
			delay=$((delay * 2))
			[ "${delay}" -gt 30 ] && delay=30
			;;
		*)
			rcow_err "${label} failed: ${out}"
			return 1
			;;
		esac
	done
}

# The whole of the residue: the pidfile, the RPC socket and its lock, and the cpu
# locks. Nothing else is removed here -- see the prohibitions above.
hot_clear_residue()
{
	rm -f "${RCOW_PIDFILE}" "${RCOW_RPC_SOCK}" "${RCOW_RPC_SOCK}.lock" \
		/var/tmp/spdk_cpu_lock_*
}

# ==========================================================================
rcow_step "preflight"

INSTANCES="$(rcow_target_instances)"

# Degrading rather than refusing, for the case a stop script gets handed: the
# upgrade it is part of has already been decided on, and a stop that fails
# outright leaves the unit red. A stop that finds nothing to stop still has one
# job, and it is the same one the crash path in cube-s3lvol-stop.sh does.
if [ -z "${INSTANCES}" ]; then
	rcow_log "no target is running; clearing its residue. The initiator and the \
lvstore are left as they are"
	hot_clear_residue
	exit 0
fi

COUNT="$(printf '%s\n' "${INSTANCES}" | wc -l)"
if [ "${COUNT}" -ne 1 ]; then
	rcow_die "${COUNT} target instances are running ($(printf '%s ' \
${INSTANCES})). Work out which one owns ${RCOW_WAL_IMG} and stop it by hand; two \
targets over one WAL is not a state anything recovers from"
fi

TGT_PID="${INSTANCES}"
rcow_log "target pid ${TGT_PID}"

# Also a degrade, and deliberately not a kill: a target that cannot be asked to
# flush is not killed on this path. The residue still goes, because the stop has
# to succeed; the live process is left to refuse the restart, which rcow_start.sh
# does by finding it by binary and not starting a second one over the same WAL.
if ! rcow_wait_rpc 10 "${TGT_PID}"; then
	rcow_log "the target (pid ${TGT_PID}) does not answer RPC on \
${RCOW_RPC_SOCK}; clearing its residue and leaving the process alone"
	hot_clear_residue
	exit 0
fi

# ==========================================================================
# Before the target is touched. The running side describes itself only while it
# is alive, and the two descriptions can only be compared while both exist, so
# this is the last moment the decision can be made at all.
if [ -n "${CANDIDATE}" ]; then
	rcow_step "version gate against ${CANDIDATE}"
	if ! rcow_version_gate_check "${CANDIDATE}"; then
		fail_live "the version gate refused the hot upgrade"
	fi
fi

# ==========================================================================
rcow_step "flush: everything acknowledged into S3"
LVS_JSON="$(printf '{"lvs_name":"%s"}' "${RCOW_LVS_NAME}")"

# The flush is a lever on the length of the paused window, not a precondition for
# the restart: what it cannot push is in the WAL and gets replayed. -ETIMEDOUT
# (-110) is what a sandbox that keeps writing produces -- its overlay never goes
# clean, so the drain runs out of time -- and refusing the upgrade there would
# make every busy sandbox un-upgradable. The same reading is taken on the destroy
# path, in s3_bs_dev_flusher_drained(). The checkpoint below still runs, so what
# the pause pays for is a longer replay, and that is reported, not hidden.
hot_online_op "flush" rcow_flush_lvstore "${LVS_JSON}" '"code": -110' ||
	fail_live "could not flush the lvstore"

# ==========================================================================
rcow_step "checkpoint: chunk map to S3, journal truncated"
hot_online_op "checkpoint" rcow_checkpoint_lvstore "${LVS_JSON}" ||
	fail_live "could not checkpoint the lvstore"

# ==========================================================================
rcow_step "layout snapshot to ${RCOW_HOT_SNAPSHOT}"

# Clamped like the online ops above, and for the same reason: rcow_rpc defaults to
# RCOW_RPC_TIMEOUT, which outlives this script's per-step budget. This one does not
# retry, so the budget is its ceiling outright.
if ! RCOW_RPC_TIMEOUT="${RCOW_STOP_TIMEOUT}" \
	rcow_rpc rcow_get_bdev '{}' >"${RCOW_HOT_SNAPSHOT}.tmp" 2>/dev/null; then
	rm -f "${RCOW_HOT_SNAPSHOT}.tmp"
	fail_live "could not capture the active layout"
fi
mv -f "${RCOW_HOT_SNAPSHOT}.tmp" "${RCOW_HOT_SNAPSHOT}" ||
	fail_live "could not write ${RCOW_HOT_SNAPSHOT}"
# The snapshot has to outlive the kill, and that is the only thing here that
# does; see rcow_fsync_file().
rcow_fsync_file "${RCOW_HOT_SNAPSHOT}" ||
	fail_live "could not make ${RCOW_HOT_SNAPSHOT} durable"
rcow_log "$(grep -o '"device_name"' "${RCOW_HOT_SNAPSHOT}" | wc -l) volume(s) \
recorded for the post-upgrade comparison"

# ==========================================================================
if [ "${DRY_RUN}" -eq 1 ]; then
	rcow_step "dry run: would stop the target and clear its residue"
	rcow_log "would SIGKILL pid ${TGT_PID}"
	rcow_log "would remove ${RCOW_PIDFILE}"
	rcow_log "would remove ${RCOW_RPC_SOCK}"
	rcow_log "would remove ${RCOW_RPC_SOCK}.lock"
	rcow_log "would remove /var/tmp/spdk_cpu_lock_*"
	rcow_log "nothing was killed and no residue was removed"
	exit 0
fi

# ==========================================================================
rcow_step "killing the target"

# SIGKILL, not SIGTERM: the kernel closes the socket with an RST, which the host
# can only read as a link failure, and a link failure is what makes it keep the
# namespace and block I/O until the peer returns. A graceful stop's ordering is
# SPDK's to decide and may announce the removal.
#
# The identity is captured here, while the process is known to be the target, and
# the poll compares against this copy rather than asking rcow_pid_is_target, which
# resolves the path RCOW_TGT_BIN names. An upgrade is exactly when that path stops
# resolving -- the old binary lives in a versioned directory that gets renamed --
# and a surviving target read as gone is the one mistake this script must not
# make: the caller would start a replacement over a WAL the old process still
# holds.
TGT_EXE="$(rcow_pid_exe "${TGT_PID}")"

if ! kill -KILL "${TGT_PID}" 2>/dev/null; then
	rcow_warn "pid ${TGT_PID} could not be signalled; it may already have \
exited, which is confirmed below"
fi

GONE=0
DEADLINE=$((SECONDS + RCOW_STOP_TIMEOUT))
while [ "${SECONDS}" -lt "${DEADLINE}" ]; do
	# No exe at all, or a different one: the process has exited, or the pid has
	# been handed to something else. Either way nothing holds the lvstore now.
	EXE_NOW="$(rcow_pid_exe "${TGT_PID}")"
	if [ -z "${EXE_NOW}" ] || [ "${EXE_NOW}" != "${TGT_EXE}" ]; then
		GONE=1
		break
	fi
	sleep 0.2
done

if [ "${GONE}" -ne 1 ]; then
	rcow_die "pid ${TGT_PID} survived SIGKILL for ${RCOW_STOP_TIMEOUT}s; \
something outside this script is holding it, and the target is still alive"
fi
rcow_log "target pid ${TGT_PID} is gone"

# ==========================================================================
rcow_step "cleaning up"

hot_clear_residue

rcow_log "hot stop complete. The layout is in ${RCOW_HOT_SNAPSHOT}; \
${RCOW_ACTIVE_FILE}, ${RCOW_BSTORE_FILE} and the WAL image were left untouched"
exit 0
