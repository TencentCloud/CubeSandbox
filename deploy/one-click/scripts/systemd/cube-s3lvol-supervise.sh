#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# cube-s3lvol-supervise.sh -- foreground supervisor for the s3lvol NVMe/TCP
# target under systemd (Type=simple).
#
# Why a supervisor at all: rcow_start.sh detaches the target (setsid) and
# returns, so with Type=forking systemd would consider the unit "started"
# and never notice the target crashing later. Keeping THIS script in the
# foreground makes it the unit's main process: while the target lives we
# poll its pidfile, and the moment it dies we exit 1, which is what makes
# Restart=on-failure actually restart the unit.
#
# Crash-recovery semantics (unchanged from rcow_start.sh): on restart the
# new target rebuilds the same NQN/NSID grid, attaches the lvstore (WAL
# tail replay + owner-marker auto-force), and connect_all skips
# already-connected controllers -- so the NVMf initiator is NEVER
# disconnected and nvme_tcp reconnects transparently; in-flight business
# I/O is only paused, never failed.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root

S3LVOL_ROOT="${TOOLBOX_ROOT}/CubeS3lvol"
RCOW_COMMON="${S3LVOL_ROOT}/scripts/rcow_common.sh"
RCOW_START="${S3LVOL_ROOT}/scripts/rcow_start.sh"

ensure_file "${RCOW_COMMON}"
ensure_file "${RCOW_START}"

# shellcheck source=/dev/null
source "${RCOW_COMMON}" # provides rcow_target_alive + RCOW_PIDFILE default

# Clean shutdown path: systemctl stop sends SIGTERM to the main process
# (us). Exit 0 so the unit transitions to inactive and ExecStop
# (cube-s3lvol-stop.sh) performs the full teardown (disconnect -> unload ->
# SIGTERM). Without this trap we would exit non-zero and systemd would
# treat the deliberate stop as a failure.
trap 'exit 0' TERM INT

log "CubeS3lvol: running rcow_start.sh"
if ! "${RCOW_START}"; then
  rc=$?
  log "CubeS3lvol: rcow_start.sh failed (rc=${rc})"
  exit "${rc}"
fi

log "CubeS3lvol: target up, monitoring pidfile ${RCOW_PIDFILE}"
while :; do
  sleep 3
  if ! rcow_target_alive; then
    log "CubeS3lvol: target exited; triggering systemd restart"
    exit 1
  fi
done
