#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd install
require_cmd systemctl

ensure_dir "${UNIT_SOURCE_DIR}"

mapfile -t unit_files < <(list_unit_files)
[[ "${#unit_files[@]}" -gt 0 ]] || die "no cube-sandbox unit files found under ${UNIT_SOURCE_DIR}"

for unit_file in "${unit_files[@]}"; do
  install_unit_file "${unit_file}"
  log "installed $(basename "${unit_file}")"
done

systemctl daemon-reload
log "systemd unit skeleton installed to ${UNIT_INSTALL_DIR}"
