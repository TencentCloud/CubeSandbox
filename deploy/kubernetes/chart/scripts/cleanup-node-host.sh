#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Cleanup host leftovers after `helm uninstall cube -n cube-system` before a
# fresh cube-node install. Run on each compute node (or via a DaemonSet Job).
#
# Usage:
#   sudo ./cleanup-node-host.sh
#   TOOLBOX_ROOT=/usr/local/services/cubetoolbox DATA_CUBELET=/data/cubelet sudo ./cleanup-node-host.sh
set -euo pipefail

TOOLBOX_ROOT="${TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
DATA_CUBELET="${DATA_CUBELET:-/data/cubelet}"
DATA_CUBE_SHIM="${DATA_CUBE_SHIM:-/data/cube-shim}"
TMP_CUBE="${TMP_CUBE:-/tmp/cube}"
BOOTSTRAP_STATE="${BOOTSTRAP_STATE:-/var/lib/cube-node-bootstrap}"
DRY_RUN="${DRY_RUN:-0}"

log() { printf '[cleanup-node-host] %s\n' "$*"; }
run() {
  if [[ "${DRY_RUN}" == "1" ]]; then
    log "DRY_RUN: $*"
  else
    log "+ $*"
    eval "$@"
  fi
}

[[ "$(id -u)" -eq 0 ]] || { echo "must run as root" >&2; exit 1; }

log "removing toolbox hostPath ${TOOLBOX_ROOT}"
run "rm -rf '${TOOLBOX_ROOT}'"

log "removing bootstrap state ${BOOTSTRAP_STATE}"
run "rm -rf '${BOOTSTRAP_STATE}'"

log "removing data dirs ${DATA_CUBELET} ${DATA_CUBE_SHIM} ${TMP_CUBE}"
run "rm -rf '${DATA_CUBELET}' '${DATA_CUBE_SHIM}' '${TMP_CUBE}'"
run "rm -rf /data/cubelet-xfs.img /data/log/Cubelet /data/log/CubeShim /data/log/CubeVmm /data/snapshot_pack || true"

# Best-effort residual dataplane cleanup (safe if devices/rules absent).
if command -v ip >/dev/null 2>&1; then
  for dev in cube-dev; do
    if ip link show "${dev}" >/dev/null 2>&1; then
      run "ip link delete '${dev}' || true"
    fi
  done
  # z* TAP leftovers from CubeVS naming
  while read -r ifc; do
    [[ -n "${ifc}" ]] || continue
    run "ip link delete '${ifc}' || true"
  done < <(ip -o link show | awk -F': ' '$2 ~ /^z/ {print $2}' | cut -d'@' -f1 || true)
fi

if command -v iptables >/dev/null 2>&1; then
  run "iptables -t mangle -F CUBE_TPROXY 2>/dev/null || true"
  run "iptables -t nat -F CUBE_TPROXY 2>/dev/null || true"
fi

log "done. Reinstall with:"
log "  helm upgrade --install cube ./deploy/kubernetes/chart -n cube-system -f values-tke.yaml -f runtime-values.yaml"
