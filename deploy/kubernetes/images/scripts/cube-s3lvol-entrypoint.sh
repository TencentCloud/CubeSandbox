#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Foreground supervisor for the s3lvol NVMe/TCP target in the cube-node Big Pod:
# rcow_start.sh detaches the target (setsid) and returns, so this script stays
# in the foreground, polls the pidfile, and on SIGTERM runs rcow_stop.sh.
#
# The Pod hostname changes on recreate, so pin the UTS hostname to
# spec.nodeName before sourcing rcow_common.sh (owner recovery matches on
# hostname), and derive the lvstore name from the full node name rather than
# hostname -s, which truncates an address to its first octet.
set -euo pipefail

S3LVOL_ROOT="${S3LVOL_ROOT:-/opt/s3lvol}"
RCOW_COMMON="${S3LVOL_ROOT}/scripts/rcow_common.sh"
RCOW_START="${S3LVOL_ROOT}/scripts/rcow_start.sh"
RCOW_STOP="${S3LVOL_ROOT}/scripts/rcow_stop.sh"

log() { printf '[cube-s3lvol] %s\n' "$*"; }
fail() { printf '[cube-s3lvol] ERROR: %s\n' "$*" >&2; exit 1; }

s3lvol_ensure_wal() {
  local img="${RCOW_WAL_IMG}"
  local total_mb bytes

  [[ -n "${img}" ]] || fail "RCOW_WAL_IMG is empty"
  if [[ -e "${img}" ]]; then
    bytes="$(stat -c %s "${img}" 2>/dev/null || echo unknown)"
    log "WAL image already exists: ${img} (${bytes} bytes); layout is frozen, not resized"
    return 0
  fi
  [[ "${RCOW_JOURNAL_MB}" =~ ^[0-9]+$ && "${RCOW_WAL_MB}" =~ ^[0-9]+$ && "${RCOW_CACHE_MB}" =~ ^[0-9]+$ ]] \
    || fail "RCOW_JOURNAL_MB / RCOW_WAL_MB / RCOW_CACHE_MB must be integers"
  total_mb=$((RCOW_JOURNAL_MB + RCOW_WAL_MB + RCOW_CACHE_MB))
  mkdir -p "$(dirname "${img}")"
  log "creating sparse WAL image ${img} (${total_mb} MiB = journal+wal+cache)"
  truncate -s "${total_mb}M" "${img}"
}

s3lvol_ensure_bucket() {
  local s3_bucket_py s3_host s3_region s3_bucket
  local -a s3_flags=()

  s3_bucket_py="${S3LVOL_ROOT}/scripts/s3_bucket.py"
  if [[ ! -f "${s3_bucket_py}" ]]; then
    log "WARN: s3_bucket.py not found; skipping bucket ensure"
    return 0
  fi
  if ! rcow_load_credentials; then
    log "WARN: could not read credentials from ${RCOW_S3_CFG}; skipping bucket ensure"
    return 0
  fi
  s3_host="$(rcow_cfg_get endpoint)"
  s3_region="$(rcow_cfg_get region)"
  s3_bucket="$(rcow_s3_buckets | head -1)"
  [[ -n "${s3_region}" ]] || s3_region="us-east-1"
  rcow_s3_addr_flags s3_flags
  if [[ -z "${s3_host}" || -z "${s3_bucket}" ]]; then
    log "WARN: ${RCOW_S3_CFG} has no endpoint/buckets; skipping bucket ensure"
    return 0
  fi
  if python3 "${s3_bucket_py}" ensure \
      -e "${s3_host}" -b "${s3_bucket}" -r "${s3_region}" \
      "${s3_flags[@]+"${s3_flags[@]}"}"; then
    log "bucket ${s3_bucket} is ready"
  else
    log "WARN: could not ensure bucket ${s3_bucket} at ${s3_host}; rcow_start.sh will fail with the precise error if it is missing"
  fi
}

s3lvol_shutdown() {
  if type rcow_target_alive >/dev/null 2>&1 && rcow_target_alive; then
    log "SIGTERM: target alive, full teardown via rcow_stop.sh"
    "${RCOW_STOP}" || true
  else
    log "SIGTERM: target not running; cleaning target-side state"
    rm -f \
      "${RCOW_PIDFILE:-}" \
      "${RCOW_RPC_SOCK:-}" \
      "${RCOW_RPC_SOCK:-}.lock" \
      /var/tmp/spdk_cpu_lock_* 2>/dev/null || true
  fi
}

# Standalone copy of rcow_node_hash's digest (sha256, first 8 hex): this runs
# before rcow_common.sh is sourced.
s3lvol_derived_lvs_name() {
  local id="${1:-}"
  [[ -n "${id}" ]] || return 1
  command -v sha256sum >/dev/null 2>&1 || return 1
  printf 'rcow-%s' "$(printf '%s' "${id}" | sha256sum | cut -c1-8)"
}

# An explicit RCOW_LVS_NAME (cubeS3lvol.lvsName) wins; otherwise derive
# rcow-<hash of NODE_NAME> now, before rcow_common.sh resolves the name at
# source time. RCOW_TGT_CPUMASK is exported only when set (rcow default 0x3).
s3lvol_export_optional_knobs() {
  if [[ -n "${RCOW_LVS_NAME:-}" ]]; then
    export RCOW_LVS_NAME
    log "RCOW_LVS_NAME=${RCOW_LVS_NAME} (explicit)"
  else
    local node="${NODE_NAME:-}"
    [[ -n "${node}" ]] || fail "NODE_NAME is required to derive RCOW_LVS_NAME"
    RCOW_LVS_NAME="$(s3lvol_derived_lvs_name "${node}")" \
      || fail "could not derive RCOW_LVS_NAME from NODE_NAME=${node}"
    export RCOW_LVS_NAME
    log "RCOW_LVS_NAME=${RCOW_LVS_NAME} (from NODE_NAME=${node})"
  fi
  if [[ -n "${RCOW_TGT_CPUMASK:-}" ]]; then
    export RCOW_TGT_CPUMASK
    log "RCOW_TGT_CPUMASK=${RCOW_TGT_CPUMASK} (explicit)"
  else
    log "RCOW_TGT_CPUMASK unset; rcow_common default is 0x3"
  fi
}

s3lvol_prepare_identity() {
  local node="${NODE_NAME:-}"
  [[ -n "${node}" ]] || fail "NODE_NAME is required (spec.nodeName)"
  if ! hostname "${node}"; then
    fail "failed to set UTS hostname to ${node}; cube-s3lvol must run privileged"
  fi
  log "hostname pinned to ${node}"
  s3lvol_export_optional_knobs
}

s3lvol_main() {
  [[ -f "${RCOW_COMMON}" ]] || fail "missing ${RCOW_COMMON}"
  [[ -x "${RCOW_START}" ]] || fail "missing ${RCOW_START}"
  [[ -x "${RCOW_STOP}" ]] || fail "missing ${RCOW_STOP}"

  export RCOW_REPO_ROOT="${S3LVOL_ROOT}"
  export RCOW_RPC_SOCK="${RCOW_RPC_SOCK:-/var/run/s3lvol/s3lvol.sock}"
  export RCOW_S3_CFG="${RCOW_S3_CFG:-/etc/s3lvol/s3.cfg}"
  export RCOW_WAL_IMG="${RCOW_WAL_IMG:-/data/cubelet/rcow/wal_bdev.img}"
  export RCOW_RUN_DIR="${RCOW_RUN_DIR:-/var/run/s3lvol}"
  export RCOW_PIDFILE="${RCOW_PIDFILE:-${RCOW_RUN_DIR}/s3lvol_tgt.pid}"
  export RCOW_LOG_DIR="${RCOW_LOG_DIR:-/data/log/rcow}"
  export RCOW_JOURNAL_MB="${RCOW_JOURNAL_MB:-1024}"
  export RCOW_WAL_MB="${RCOW_WAL_MB:-32768}"
  export RCOW_CACHE_MB="${RCOW_CACHE_MB:-490496}"

  mkdir -p "${RCOW_RUN_DIR}" "${RCOW_LOG_DIR}" "$(dirname "${RCOW_WAL_IMG}")"

  s3lvol_prepare_identity
  s3lvol_ensure_wal

  # shellcheck source=/dev/null
  source "${RCOW_COMMON}"

  trap 's3lvol_shutdown; exit 0' TERM INT

  s3lvol_ensure_bucket

  log "running rcow_start.sh"
  local rc=0
  "${RCOW_START}" || rc=$?
  if [[ "${rc}" -ne 0 ]]; then
    fail "rcow_start.sh failed (rc=${rc})"
  fi

  log "target up, monitoring pidfile ${RCOW_PIDFILE}"
  while :; do
    sleep 3
    if ! rcow_target_alive; then
      log "target exited; exiting so kubelet restarts the container"
      exit 1
    fi
  done
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  s3lvol_main "$@"
fi
