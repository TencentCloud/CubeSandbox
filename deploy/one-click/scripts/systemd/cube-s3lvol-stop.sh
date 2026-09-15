#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# cube-s3lvol-stop.sh -- ExecStop for cube-sandbox-s3lvol.service.
#
# What it does, by the state it finds:
#
#   1. Hot restart (marker for this boot naming a live target): the marker was
#      written by an upgrade orchestrator, so this stop is the one an upgrade
#      asked for. rcow_upgrade.sh, handed the candidate the marker recorded,
#      flushes and checkpoints online and then kills the target outright -- no
#      disconnect, no unload -- so the host only pauses I/O.
#
#   2. Planned stop (target alive, no marker): rcow_stop.sh does the full
#      teardown in reverse start order. bstore.json and the active registry are
#      left for the next start to attach, so planned restarts are transparent.
#
#   3. Target already gone: NEVER disconnect the NVMf initiator -- its
#      controllers are sitting in nvme_tcp reconnect, and a disconnect would
#      break the no-I/O-interruption guarantee the crash-restart design rests
#      on. Only clean target-side residue, so the next ExecStart can rebuild
#      the same NQN/NSID grid and the kernel reconnects on its own.
#
# Two further states are refused rather than served, because everything they
# could fall through to destroys a live target's namespaces:
#
#   - a marker for this boot whose pid is not a running target; it may be a live
#     intent this host cannot confirm, and the planned path deletes the gendisks
#     for one. The marker is left for the operator to resolve.
#   - a target running that the pidfile does not name.
#
# A refusal exits non-zero, which leaves the unit FAILED, and `systemctl stop`
# on a failed unit is a no-op -- so recovery is `systemctl reset-failed` and then
# the stop by hand. That is the trade, taken deliberately: refuse and leave
# something inspectable rather than succeed by breaking it.
#
# The marker is what separates 1 from 2, and it is deliberately not writable
# from here: an operator asking for a plain stop must never get the path that
# leaves the lvstore to be picked up again.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root

S3LVOL_ROOT="${TOOLBOX_ROOT}/CubeS3lvol"
RCOW_COMMON="${S3LVOL_ROOT}/scripts/rcow_common.sh"
RCOW_STOP="${S3LVOL_ROOT}/scripts/rcow_stop.sh"
RCOW_UPGRADE="${S3LVOL_ROOT}/scripts/rcow_upgrade.sh"

# Nothing installed (yet) -- nothing to stop.
if [[ ! -f "${RCOW_COMMON}" ]]; then
  exit 0
fi

# shellcheck source=/dev/null
source "${RCOW_COMMON}" # provides rcow_target_alive + RCOW_* path defaults

# Consumes the marker, so this answers once and a stale one cannot be read
# twice. The three answers are not the same thing: 0 is an upgrade asking for
# this stop, 1 is nobody asking, and 2 is a marker for this boot whose pid is not
# a running target -- which may be a live intent this host cannot confirm.
#
# `|| consume=$?` rather than a bare call: the script runs under `set -e`, and a
# bare call would exit on the very answers this has to branch on.
consume=0
rcow_hot_marker_consume || consume=$?

if [[ "${consume}" -eq 2 ]]; then
  # Falling through would run rcow_stop.sh: disconnect the initiator and unload
  # the lvstore, which deletes the gendisks of a live target's namespaces. That
  # is the outage this path exists to avoid, so refuse and leave the marker --
  # the intent is the operator's, not this script's to discard.
  log "CubeS3lvol: ${RCOW_HOT_MARKER} names a target this host cannot confirm; refusing rather than tearing the namespaces down. Resolve which process owns the WAL, then remove the marker by hand"
  exit 1
fi

if [[ "${consume}" -eq 0 ]]; then
  if [[ ! -x "${RCOW_UPGRADE}" ]]; then
    # Refusing rather than falling back: the full teardown drops the nvme
    # controllers, and the upgrade that wrote the marker is not expecting it.
    log "CubeS3lvol: hot restart was requested but ${RCOW_UPGRADE} is missing or not executable"
    exit 1
  fi
  log "CubeS3lvol: hot restart requested; stopping via rcow_upgrade.sh, initiator untouched"
  # The candidate travels with the marker because this stop runs before the
  # versioned directory is switched in: RCOW_TGT_BIN still names the outgoing
  # binary at this point, so comparing it would compare the running build with
  # itself and the gate would pass without checking anything. An empty candidate
  # is passed on rather than hidden -- rcow_upgrade.sh refuses it.
  if [[ -n "${RCOW_HOT_CANDIDATE}" ]]; then
    "${RCOW_UPGRADE}" --candidate "${RCOW_HOT_CANDIDATE}"
  else
    "${RCOW_UPGRADE}"
  fi
elif rcow_target_alive; then
  log "CubeS3lvol: target alive, full teardown via rcow_stop.sh"
  "${RCOW_STOP}"
elif [[ -n "$(rcow_target_instances)" ]]; then
  # A target is alive but its pidfile is missing or unreadable. Also not a case
  # for the full teardown, for the same reason as the marker branch above: it
  # would delete the gendisks of a live target's namespaces. Refusing leaves the
  # operator a process they can still talk to. rcow_target_alive alone must never
  # be read as "no target".
  log "CubeS3lvol: a target is running that ${RCOW_PIDFILE} does not account for; refusing to tear it down. Restore the pidfile, or stop that pid by hand"
  exit 1
else
  log "CubeS3lvol: target not running; cleaning target-side state, initiator untouched"
  rm -f \
    "${RCOW_PIDFILE}" \
    "${RCOW_RPC_SOCK}" \
    "${RCOW_RPC_SOCK}.lock" \
    /var/tmp/spdk_cpu_lock_* 2>/dev/null || true
fi
exit 0
