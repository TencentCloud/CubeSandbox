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

# Ensure the bucket exists before the target attaches. s3lvol will not
# create it; a missing bucket fails create/attach with -EACCES. Failure
# here is only a warning: HEAD 403 can mean "exists but HeadBucket is
# denied", and MinIO may still be coming up (Restart=on-failure retries).
S3_BUCKET_PY="${S3LVOL_ROOT}/scripts/s3_bucket.py"
if [[ ! -f "${S3_BUCKET_PY}" ]]; then
  S3_BUCKET_PY="${S3LVOL_ROOT}/test/tools/s3_bucket.py"
fi
if [[ -f "${S3_BUCKET_PY}" ]]; then
  if rcow_load_credentials; then
    s3_host="$(rcow_cfg_get endpoint)"
    s3_region="$(rcow_cfg_get region)"
    s3_bucket="$(rcow_s3_buckets | head -1)"
    [[ -n "${s3_region}" ]] || s3_region="us-east-1"
    s3_flags=()
    rcow_s3_addr_flags s3_flags
    if [[ -n "${s3_host}" && -n "${s3_bucket}" ]]; then
      if python3 "${S3_BUCKET_PY}" ensure \
          -e "${s3_host}" -b "${s3_bucket}" -r "${s3_region}" \
          "${s3_flags[@]+"${s3_flags[@]}"}"; then
        log "CubeS3lvol: bucket ${s3_bucket} is ready"
      else
        log "WARN: could not ensure S3 bucket ${s3_bucket} at ${s3_host}; rcow_start.sh will report the precise error if the bucket is actually missing"
      fi
    else
      log "WARN: ${RCOW_S3_CFG} has no endpoint/buckets; skipping bucket ensure"
    fi
  else
    log "WARN: could not read credentials from ${RCOW_S3_CFG}; skipping bucket ensure"
  fi
else
  log "WARN: s3_bucket.py not found; skipping bucket ensure"
fi

log "CubeS3lvol: running rcow_start.sh"
# Do not use `if ! cmd; then rc=$?`: the `!` inverts success, so rc becomes 0
# and Restart=on-failure never retries (MinIO still coming up).
rc=0
"${RCOW_START}" || rc=$?
if [[ "${rc}" -ne 0 ]]; then
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
