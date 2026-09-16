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
#                                          recovered from. With none, there is
#                                          nothing to pause and only residue left
#                                          to clear; with one that cannot be
#                                          reached, nothing at all is touched --
#                                          see the note at that branch
#    2. version gate, when --candidate     the new binary has to accept the
#                                          formats on disk; only here are both
#                                          descriptions known at once
#    3. pin the rollback budget             the connect flags only reach a fresh
#                                          connection, so the controllers an
#                                          upgrade inherits are written to here
#    4. rcow_flush_lvstore      push everything acknowledged to S3; online
#    5. rcow_checkpoint_lvstore snapshot the chunk map and truncate the journal,
#                               which is what keeps the next attach short
#    6. snapshot the layout     the file rcow_verify_active --expect compares to
#    7. SIGKILL the target      a crash, not a shutdown: a crash is the one exit
#                               guaranteed to leave the namespace in place and
#                               drive the host into error recovery
#    8. clear four leftovers    pidfile, RPC socket, its .lock, cpu locks
#    9. drop the marker         the intent is spent once the target it names is
#                               gone; until then it stays, so a refused attempt
#                               still reads as a hot one to the next stop
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
#      like an upgrade's. Removing it, once the target it names is gone, is the
#      other half of that rule: it is the upgrade's own record of intent, and
#      this is the script that carries that intent out.
#
#  Usage: rcow_upgrade.sh --candidate <binary> [--dry-run]
#
#    --candidate  the binary the upgrade intends to start. Required on a real
#                 run: the version gate is the only moment both builds can
#                 describe themselves, and it is the only guard against a new
#                 binary that cannot read the formats already on disk. It is
#                 deliberately not defaulted to RCOW_TGT_BIN -- an upgrade
#                 switches the versioned directory in only after the old process
#                 is confirmed dead, so that path still names the outgoing
#                 binary here. The stop path takes it from the hot-restart
#                 marker, which records it when the orchestrator writes one.
#    --dry-run    run the online steps and print what would be killed and removed,
#                 without killing or removing anything. The one thing that may be
#                 left out here, because nothing is risked either way.

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
	-h|--help)   sed -n '2,66p' "${BASH_SOURCE[0]}"; exit 0 ;;
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
	# The intent is spent: the target it names is not there to be restarted.
	rm -f "${RCOW_HOT_MARKER}"
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

# Not a kill, and now not a cleanup either. The socket is this process's only way
# back in: unlinking it makes a live target permanently unreachable, so no flush,
# no checkpoint and no clean shutdown through rcow_stop.sh remain and SIGKILL
# would be the only exit left. Refusing leaves the operator a process they can
# still talk to, and rcow_start.sh's instance guard keeps a second target from
# starting over the same WAL meanwhile.
#
# The cost is worth naming: a non-zero ExecStop leaves the unit FAILED, and
# `systemctl stop` on a failed unit is a no-op, so recovery is
# `systemctl reset-failed` and then the stop by hand.
if ! rcow_wait_rpc 10 "${TGT_PID}"; then
	rcow_err "the target (pid ${TGT_PID}) does not answer RPC on \
${RCOW_RPC_SOCK}; leaving it and its residue alone. Nothing was signalled and \
nothing was removed"
	exit 1
fi

# ==========================================================================
# Before the target is touched. The running side describes itself only while it
# is alive, and the two descriptions can only be compared while both exist, so
# this is the last moment the decision can be made at all.
#
# A real run without a candidate is refused rather than let through ungated: the
# journal carries no version field, so a new binary that does not recognise an op
# reads it as the end of the log and silently truncates acknowledged writes, and
# nothing else in this script can catch that.
if [ "${DRY_RUN}" -eq 0 ] && [ -z "${CANDIDATE}" ]; then
	fail_live "no --candidate, so the version gate has nothing to compare. \
Pass the binary the upgrade will start, or let the orchestrator record it in \
${RCOW_HOT_MARKER} before asking for the stop"
fi

if [ -n "${CANDIDATE}" ]; then
	rcow_step "version gate against ${CANDIDATE}"
	if ! rcow_version_gate_check "${CANDIDATE}"; then
		fail_live "the version gate refused the hot upgrade"
	fi
fi

# ==========================================================================
# The connect flags pin these for a fresh connection, but a connection keeps
# whatever it was made with -- and an upgrade runs against controllers that
# already exist, which need not have been connected by rcow_start.sh at all.
# Writing them here is what makes the unit's TimeoutStopSec mean anything.
rcow_step "initiator: pin the rollback budget"
rcow_tune_initiator_timeouts
case "$?" in
0)
	rcow_log "initiator: ${RCOW_NUM_SUBSYS} controller(s) at \
reconnect_delay=${RCOW_RECONNECT_DELAY}s, ctrl_loss_tmo=${RCOW_CTRL_LOSS_TMO}s" ;;
1)
	# Refusing rather than warning: with fast_io_fail_tmo set, every pause is a
	# fast failure -- the opposite of what this path is for -- and writing the
	# other two attributes does not undo it.
	fail_live "the initiator is set to fail I/O on the first pause; clear \
fast_io_fail_tmo before upgrading" ;;
2)
	# pre-5.7: the host runs on its own default whatever this is configured to.
	# Worth saying, not worth refusing over.
	rcow_warn "this kernel exports no reconnect_delay / ctrl_loss_tmo; the \
pause budget is the kernel's default, not ${RCOW_CTRL_LOSS_TMO}s" ;;
esac

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
TGT_EXE="$(rcow_pid_exe "${TGT_PID}")" || :
if [ -z "${TGT_EXE}" ]; then
	# An empty capture is the one answer the poll below cannot tell apart from a
	# failed read, and reading a live target as gone is the mistake just named.
	# Refuse rather than clean up after a process that may still hold the WAL --
	# the same posture as the unreachable-target branch above. Not fail_live,
	# whose second line asserts the target is running: that is the very thing
	# this branch cannot confirm.
	rcow_err "cannot read the identity of pid ${TGT_PID}; nothing was signalled \
and no residue was removed"
	exit 1
fi

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

# The intent is spent: this is the moment it was recorded for. Clearing it here
# rather than where the stop script read it is what leaves a refused attempt
# looking like a hot one to the next stop, instead of a planned teardown.
rm -f "${RCOW_HOT_MARKER}"

# ==========================================================================
rcow_step "cleaning up"

hot_clear_residue

rcow_log "hot stop complete. The layout is in ${RCOW_HOT_SNAPSHOT}; \
${RCOW_ACTIVE_FILE}, ${RCOW_BSTORE_FILE} and the WAL image were left untouched"
exit 0
