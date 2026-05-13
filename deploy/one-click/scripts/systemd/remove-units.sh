#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd systemctl

shopt -s nullglob
unit_paths=(
  "${UNIT_INSTALL_DIR}"/cube-sandbox-*.service
  "${UNIT_INSTALL_DIR}"/cube-sandbox-*.target
  "${UNIT_INSTALL_DIR}"/cube-sandbox-*.timer
)

for unit_path in "${unit_paths[@]}"; do
  [[ -f "${unit_path}" ]] || continue
  unit_name="$(basename "${unit_path}")"
  systemctl disable --now "${unit_name}" >/dev/null 2>&1 || true
  remove_unit_file "${unit_name}"
  log "removed ${unit_name}"
done

systemctl daemon-reload
systemctl reset-failed >/dev/null 2>&1 || true
log "systemd unit skeleton removed from ${UNIT_INSTALL_DIR}"
