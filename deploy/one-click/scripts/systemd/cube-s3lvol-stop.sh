#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# cube-s3lvol-stop.sh -- ExecStop for cube-sandbox-s3lvol.service.
#
# Two distinct paths:
#
#   1. Planned stop (target still running): hand over to rcow_stop.sh which
#      does the full teardown in reverse start order -- nvme disconnect x32,
#      rcow_unload_lvstore (drain to S3, close journal; WAL is the safety
#      net), SIGTERM the target. bstore.json / active registry are left for
#      the next start to attach/replay, so planned restarts are transparent.
#
#   2. Crash recovery (target already dead): NEVER disconnect the NVMf
#      initiator. The controllers are sitting in nvme_tcp reconnect, and a
#      disconnect here would break the no-I/O-interruption guarantee that
#      the crash-restart design depends on. Only clean target-side residue
#      (pidfile / RPC socket / cpu lock) so the next ExecStart can rebuild
#      the same NQN/NSID grid and the kernel reconnects on its own.
#
# TimeoutStopSec=180s on the unit covers rcow_stop.sh's RCOW_STOP_TIMEOUT
# budget; the crash path here is seconds.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root

S3LVOL_ROOT="${TOOLBOX_ROOT}/CubeS3lvol"
RCOW_COMMON="${S3LVOL_ROOT}/scripts/rcow_common.sh"
RCOW_STOP="${S3LVOL_ROOT}/scripts/rcow_stop.sh"

# Nothing installed (yet) -- nothing to stop.
if [[ ! -f "${RCOW_COMMON}" ]]; then
  exit 0
fi

# shellcheck source=/dev/null
source "${RCOW_COMMON}" # provides rcow_target_alive + RCOW_* path defaults

if rcow_target_alive; then
  log "CubeS3lvol: target alive, full teardown via rcow_stop.sh"
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
