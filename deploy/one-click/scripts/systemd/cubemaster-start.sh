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

# CubeTemplateCenter's address, read only when templatecenter_enabled=true in
# conf.yaml. It is an environment variable (not a conf key) because an address
# is a deployment fact; this is the exact counterpart of CUBE_MASTER_ADDR on
# the TC side. The default points at the TC unit the same bundle installs on
# this host, so enabling TC needs only the conf flip, no addressing. Export
# unconditionally: when the conf switch is off the binary never reads it, and
# having it set keeps the later enable to a single edit.
export CUBE_TEMPLATE_CENTER_ADDR="${CUBE_TEMPLATE_CENTER_ADDR:-http://127.0.0.1:8090}"

exec "${CUBEMASTER_BIN}"
