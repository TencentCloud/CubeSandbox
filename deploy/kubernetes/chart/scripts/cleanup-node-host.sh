#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Cleanup host leftovers after `helm uninstall cube -n cube-system` before a
# fresh cube-node install. Run on each compute node (or via a DaemonSet Job).
#
# Usage:
#   sudo ./cleanup-node-host.sh
#   sudo env DRY_RUN=1 DATA_CUBELET=/var/lib/cube/cubelet \
#     DATA_LOG=/var/log/cube DATA_SNAPSHOT_PACK=/var/lib/cube/snapshots \
#     LOOPBACK_IMAGE_PATH=/var/lib/cube/cubelet-xfs.img ./cleanup-node-host.sh
# Review the dry run before removing DRY_RUN=1. Pass the host directories from
# your values file explicitly; this script does not read Helm values. It keeps
# Host Mount user data (hostPaths.dataShared); it is not a user-data purge tool.
set -euo pipefail

TOOLBOX_ROOT="${TOOLBOX_ROOT-/usr/local/services/cubetoolbox}"
DATA_CUBELET="${DATA_CUBELET-/data/cubelet}"
DATA_CUBE_SHIM="${DATA_CUBE_SHIM-/data/cube-shim}"
DATA_CUBE_SHARED="${DATA_CUBE_SHARED-/data/cube-shared}"
DATA_LOG="${DATA_LOG-/data/log}"
DATA_SNAPSHOT_PACK="${DATA_SNAPSHOT_PACK-/data/snapshot_pack}"
LOOPBACK_IMAGE_PATH="${LOOPBACK_IMAGE_PATH-/data/cubelet-xfs.img}"
TMP_CUBE="${TMP_CUBE-/tmp/cube}"
BOOTSTRAP_STATE="${BOOTSTRAP_STATE-/var/lib/cube-node-bootstrap}"
DRY_RUN="${DRY_RUN:-0}"

log() { printf '[cleanup-node-host] %s\n' "$*"; }
run() {
  local command
  printf -v command '%q ' "$@"
  if [[ "${DRY_RUN}" == "1" ]]; then
    log "DRY_RUN: ${command}"
  else
    log "+ ${command}"
    "$@"
  fi
}

[[ "$(id -u)" -eq 0 ]] || { echo "must run as root" >&2; exit 1; }

# Validate every target before performing any cleanup. Explicitly empty values
# must not silently fall back to deleting the default tree. Strip trailing
# slashes so rm removes a symlink itself rather than traversing its target.
for name in TOOLBOX_ROOT DATA_CUBELET DATA_CUBE_SHIM DATA_CUBE_SHARED DATA_LOG \
  DATA_SNAPSHOT_PACK LOOPBACK_IMAGE_PATH TMP_CUBE BOOTSTRAP_STATE; do
  path="${!name}"
  while [[ "${path}" == */ ]]; do path="${path%/}"; done
  if [[ "${path}" != /* || "/${path#/}/" == *'/../'* || "/${path#/}/" == *'/./'* ]]; then
    echo "unsafe cleanup path for ${name}: ${!name}" >&2
    exit 1
  fi
  printf -v "${name}" '%s' "${path}"
done

log "removing toolbox hostPath ${TOOLBOX_ROOT}"
run rm -rf -- "${TOOLBOX_ROOT}"

log "removing bootstrap state ${BOOTSTRAP_STATE}"
run rm -rf -- "${BOOTSTRAP_STATE}"

log "removing data dirs ${DATA_CUBELET} ${DATA_CUBE_SHIM} ${DATA_CUBE_SHARED} ${TMP_CUBE}"
run rm -rf -- "${DATA_CUBELET}" "${DATA_CUBE_SHIM}" "${DATA_CUBE_SHARED}" "${TMP_CUBE}"
run rm -rf -- "${LOOPBACK_IMAGE_PATH}" "${DATA_LOG}/Cubelet" \
  "${DATA_LOG}/CubeShim" "${DATA_LOG}/CubeVmm" "${DATA_SNAPSHOT_PACK}" || true

# Best-effort residual dataplane cleanup (safe if devices/rules absent).
if command -v ip >/dev/null 2>&1; then
  for dev in cube-dev; do
    if ip link show "${dev}" >/dev/null 2>&1; then
      run ip link delete "${dev}" || true
    fi
  done
  # z* TAP leftovers from CubeVS naming
  while read -r ifc; do
    [[ -n "${ifc}" ]] || continue
    run ip link delete "${ifc}" || true
  done < <(ip -o link show | awk -F': ' '$2 ~ /^z/ {print $2}' | cut -d'@' -f1 || true)
fi

if command -v iptables >/dev/null 2>&1; then
  run iptables -t mangle -F CUBE_TPROXY 2>/dev/null || true
  run iptables -t nat -F CUBE_TPROXY 2>/dev/null || true
fi

log "done. Reinstall with:"
log "  helm upgrade --install cube ./deploy/kubernetes/chart -n cube-system -f values-tke.yaml -f runtime-values.yaml"
