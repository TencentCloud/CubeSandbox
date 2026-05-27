#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd pgrep
ensure_systemd_runtime_dirs
"${SCRIPT_DIR}/prepare-compute-role.sh"

CUBELET_BIN="${TOOLBOX_ROOT}/Cubelet/bin/cubelet"
CUBELET_CONFIG="${TOOLBOX_ROOT}/Cubelet/config/config.toml"
CUBELET_DYNAMICCONF="${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"
PID_FILE="${SYSTEMD_RUNTIME_DIR}/cubelet.pid"
PATTERN="^${CUBELET_BIN} --config"

ensure_executable "${CUBELET_BIN}"
ensure_file "${CUBELET_CONFIG}"
ensure_file "${CUBELET_DYNAMICCONF}"
mkdir -p \
  "${TOOLBOX_ROOT}/cube-vs/network" \
  "${TOOLBOX_ROOT}/cube-snapshot" \
  /tmp/cube \
  /data/log/Cubelet \
  /data/log/CubeShim \
  /data/log/CubeVmm \
  /data/cube-shim/disks \
  /data/snapshot_pack/disks

if [[ -f "${PID_FILE}" ]]; then
  existing_pid="$(<"${PID_FILE}")"
  if [[ -n "${existing_pid}" ]] && kill -0 "${existing_pid}" >/dev/null 2>&1 && pid_matches_pattern "${existing_pid}" "${PATTERN}"; then
    log "cubelet already running pid=${existing_pid}"
    exit 0
  fi
  rm -f "${PID_FILE}"
fi

if [[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]]; then
  (
    export CUBE_SANDBOX_NODE_IP="${CUBE_SANDBOX_NODE_IP}"
    exec "${CUBELET_BIN}" --config "${CUBELET_CONFIG}" --dynamic-conf-path "${CUBELET_DYNAMICCONF}"
  ) &
else
  "${CUBELET_BIN}" --config "${CUBELET_CONFIG}" --dynamic-conf-path "${CUBELET_DYNAMICCONF}" &
fi
initial_pid=$!
log "cubelet initial pid=${initial_pid}"

for _ in {1..20}; do
  if ! kill -0 "${initial_pid}" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

refresh_pidfile_from_pattern "${PID_FILE}" "${PATTERN}" 20 1 || die "failed to determine cubelet child pid"
log "cubelet stable pid=$(<"${PID_FILE}")"
