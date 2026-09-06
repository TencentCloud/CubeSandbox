#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
ensure_systemd_runtime_dirs

CUBEMASTER_BIN="${TOOLBOX_ROOT}/CubeMaster/bin/cubemaster"
CUBEMASTER_CFG="${TOOLBOX_ROOT}/CubeMaster/conf.yaml"
CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR_DEFAULT="/data/CubeMaster/storage"
CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR_CONFIGURED="${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR:-}"
CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR="${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR_CONFIGURED:-${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR_DEFAULT}}"

ensure_executable "${CUBEMASTER_BIN}"
ensure_file "${CUBEMASTER_CFG}"

export CUBE_MASTER_CONFIG_PATH="${CUBEMASTER_CFG}"
if mkdir -p "${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR}" >/dev/null 2>&1; then
  export CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR="${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR}"
else
  log "cubemaster artifact store ${CUBEMASTER_ROOTFS_ARTIFACT_STORE_DIR} unavailable, fallback handled by cubemaster"
fi

# CubeTemplateCenter's address. Can be set here (ad-hoc override) or in
# conf.yaml as template_center_addr (persistent, per-deployment). Env wins
# over yaml. The default points at the TC unit the same bundle installs on
# this host, so split deployments need only edit this export or the conf.yaml
# field. Export unconditionally: the binary reads it on every request, and
# having it set keeps the later enable to a single edit.
export CUBE_TEMPLATE_CENTER_ADDR="${CUBE_TEMPLATE_CENTER_ADDR:-http://127.0.0.1:8090}"

exec "${CUBEMASTER_BIN}"
