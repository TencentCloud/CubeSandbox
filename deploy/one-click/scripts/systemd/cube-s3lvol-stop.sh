#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# cube-s3lvol-stop.sh -- ExecStop for cube-sandbox-s3lvol.service.
#
# Three distinct paths:
#
#   1. Hot restart (target alive, intent marker present): the marker was written
#      by an upgrade orchestrator, so this stop is the one an upgrade asked for.
#      rcow_upgrade.sh flushes and checkpoints online, then kills the target
#      outright -- no disconnect, no unload -- so the host only pauses I/O.
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
# twice. A failure here means either no marker or one that does not name the
# live target, and both mean the same thing: nobody asked for a hot restart.
if rcow_hot_marker_consume; then
  if [[ ! -x "${RCOW_UPGRADE}" ]]; then
    # Refusing rather than falling back: the full teardown drops the nvme
    # controllers, and the upgrade that wrote the marker is not expecting it.
    log "CubeS3lvol: hot restart was requested but ${RCOW_UPGRADE} is missing or not executable"
    exit 1
  fi
  log "CubeS3lvol: hot restart requested; stopping via rcow_upgrade.sh, initiator untouched"
  "${RCOW_UPGRADE}"
elif rcow_target_alive; then
  log "CubeS3lvol: target alive, full teardown via rcow_stop.sh"
  "${RCOW_STOP}"
elif [[ -n "$(rcow_target_instances)" ]]; then
  # The pidfile is the fast path, not the only one, and it can be missing or
  # unreadable while a target is very much alive. Removing the residue here
  # would unlink that live target's own socket and leave it unreachable, so
  # hand over instead: rcow_stop.sh adopts it by pid and warns about the
  # socket. rcow_target_alive alone must never be read as "no target".
  log "CubeS3lvol: target running without a usable pidfile; full teardown via rcow_stop.sh"
  "${RCOW_STOP}"
else
  log "CubeS3lvol: target not running; cleaning target-side state, initiator untouched"
  rm -f \
    "${RCOW_PIDFILE}" \
    "${RCOW_RPC_SOCK}" \
    "${RCOW_RPC_SOCK}.lock" \
    /var/tmp/spdk_cpu_lock_* 2>/dev/null || true
fi
exit 0
