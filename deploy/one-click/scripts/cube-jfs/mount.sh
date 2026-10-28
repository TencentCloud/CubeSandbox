#!/usr/bin/env bash
# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0
# Enable and verify the local JuiceFS client. Run on every Cube node.

set -euo pipefail
PREFIX="${CUBE_INSTALL_PREFIX:-/usr/local/services/cubetoolbox}"
ENV_FILE="${PREFIX}/.one-click.env"
CUBELET_CONFIG="${PREFIX}/Cubelet/config/config.toml"

log() { printf '[cube-jfs-mount] %s\n' "$*"; }
die() { printf '[cube-jfs-mount] ERROR: %s\n' "$*" >&2; exit 1; }
[[ "$(id -u)" -eq 0 ]] || die "run as root"
[[ -f "${ENV_FILE}" ]] || die "missing ${ENV_FILE}"
set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a
if [[ -z "${CUBE_JFS_META_URL:-}" ]]; then
  if [[ "${ONE_CLICK_DEPLOY_ROLE:-control}" == "compute" && -n "${ONE_CLICK_CONTROL_PLANE_IP:-}" ]]; then
    CUBE_JFS_META_URL="redis://${ONE_CLICK_CONTROL_PLANE_IP}:6380/0"
    log "using metadata URL derived from ONE_CLICK_CONTROL_PLANE_IP"
  elif [[ "${ONE_CLICK_DEPLOY_ROLE:-control}" != "compute" && -n "${CUBE_SANDBOX_NODE_IP:-}" ]]; then
    CUBE_JFS_META_URL="redis://${CUBE_SANDBOX_NODE_IP}:6380/0"
    log "using metadata URL derived from CUBE_SANDBOX_NODE_IP"
  fi
fi
action="${1:-mount}"
case "${action}" in mount|unmount) ;; *) die "usage: $0 [mount|unmount]" ;; esac
if [[ "${action}" == "mount" ]]; then
  [[ "${ONE_CLICK_ENABLE_JFS:-0}" == "1" ]] || die "set ONE_CLICK_ENABLE_JFS=1 in ${ENV_FILE}"
  : "${CUBE_JFS_META_URL:?set CUBE_JFS_META_URL in ${ENV_FILE}}"
fi

mount_path="${CUBE_JFS_MOUNT_PATH:-/data/cubelet/jfs}"
root="${CUBE_JFS_ROOT:-cube}"
cache_dir="${CUBE_JFS_CACHE_DIR:-/data/cubelet/jfs-cache}"
mount_wait_seconds="${CUBE_JFS_MOUNT_WAIT_SECONDS:-30}"
[[ "${mount_wait_seconds}" =~ ^[0-9]+$ ]] \
  || die "CUBE_JFS_MOUNT_WAIT_SECONDS must be a non-negative integer (got '${mount_wait_seconds}')"

is_juicefs_mount() {
  local fs_type
  while IFS= read -r fs_type; do
    [[ "${fs_type}" == "fuse.juicefs" ]] && return 0
  done < <(findmnt -rn -T "${mount_path}" -o FSTYPE 2>/dev/null || true)
  return 1
}

wait_for_juicefs_mount() {
  local elapsed service_state
  for ((elapsed = 0; elapsed <= mount_wait_seconds; elapsed++)); do
    if is_juicefs_mount; then
      return 0
    fi
    if ! systemctl is-active --quiet cube-sandbox-jfs.service; then
      service_state="$(systemctl show cube-sandbox-jfs.service \
        -p ActiveState -p SubState -p Result --value 2>/dev/null | paste -sd / -)"
      die "cube-sandbox-jfs.service stopped before ${mount_path} was mounted (state=${service_state:-unknown})"
    fi
    (( elapsed == mount_wait_seconds )) && break
    sleep 1
  done

  findmnt -rn -T "${mount_path}" -o TARGET,FSTYPE,SOURCE >&2 2>/dev/null || true
  die "${mount_path} is not a JuiceFS mount after waiting ${mount_wait_seconds}s"
}

# shellcheck source=../common/cubelet_config.sh
source "${PREFIX}/scripts/common/cubelet_config.sh"
if [[ "${action}" == "unmount" ]]; then
  write_cubelet_jfs_config "${CUBELET_CONFIG}" "${mount_path}" "${root}" "${cache_dir}" 0
  systemctl restart cube-sandbox-cubelet.service
  systemctl disable --now cube-sandbox-jfs.service
  log "local JuiceFS client is disabled"
  exit 0
fi
write_cubelet_jfs_config "${CUBELET_CONFIG}" "${mount_path}" "${root}" "${cache_dir}" 1
systemctl daemon-reload
systemctl enable cube-sandbox-jfs.service
systemctl restart cube-sandbox-jfs.service
systemctl is-active --quiet cube-sandbox-jfs.service \
  || die "cube-sandbox-jfs.service did not become active"
log "waiting up to ${mount_wait_seconds}s for JuiceFS at ${mount_path}"
wait_for_juicefs_mount
systemctl restart cube-sandbox-cubelet.service
log "local JuiceFS mount is ready at ${mount_path}"
