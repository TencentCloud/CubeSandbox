#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
if ! is_compute_role; then
  exit 0
fi

require_cmd sed

CUBELET_DYNAMICCONF="${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"
ensure_file "${CUBELET_DYNAMICCONF}"
[[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]] || die "CUBE_SANDBOX_NODE_IP is required for compute role"

CONTROL_PLANE_ADDR="$(resolve_control_plane_cubemaster_addr)"
grep -Eq "meta_server_endpoint:" "${CUBELET_DYNAMICCONF}" || die "meta_server_endpoint missing in ${CUBELET_DYNAMICCONF}"

current_endpoint="$(sed -nE '/^[[:space:]]*meta_server_endpoint:[[:space:]]*"/{s/^[[:space:]]*meta_server_endpoint:[[:space:]]*"([^"]+)".*/\1/p;q;}' "${CUBELET_DYNAMICCONF}" 2>/dev/null || true)"
if [[ "${current_endpoint}" == "${CONTROL_PLANE_ADDR}" ]]; then
  exit 0
fi

sed -i \
  -e "s#^\([[:space:]]*meta_server_endpoint:[[:space:]]*\).*#\1\"${CONTROL_PLANE_ADDR}\"#" \
  "${CUBELET_DYNAMICCONF}"
log "updated cubelet dynamic meta_server_endpoint=${CONTROL_PLANE_ADDR}"
