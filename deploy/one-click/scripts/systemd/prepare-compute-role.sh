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

OPS_ADDR="$(resolve_control_plane_cubeops_addr)"
MASTER_HTTP_ADDR="$(resolve_control_plane_cubemaster_addr)"
grep -Eq "meta_server_endpoint:" "${CUBELET_DYNAMICCONF}" || die "meta_server_endpoint missing in ${CUBELET_DYNAMICCONF}"
grep -Eq "cubemaster_http_addr:" "${CUBELET_DYNAMICCONF}" || die "cubemaster_http_addr missing in ${CUBELET_DYNAMICCONF}"

current_ops="$(sed -nE '/^[[:space:]]*meta_server_endpoint:[[:space:]]*"/{s/^[[:space:]]*meta_server_endpoint:[[:space:]]*"([^"]+)".*/\1/p;q;}' "${CUBELET_DYNAMICCONF}" 2>/dev/null || true)"
current_master_http="$(sed -nE '/^[[:space:]]*cubemaster_http_addr:[[:space:]]*"/{s/^[[:space:]]*cubemaster_http_addr:[[:space:]]*"([^"]+)".*/\1/p;q;}' "${CUBELET_DYNAMICCONF}" 2>/dev/null || true)"
if [[ "${current_ops}" == "${OPS_ADDR}" && "${current_master_http}" == "${MASTER_HTTP_ADDR}" ]]; then
  exit 0
fi

sed -i \
  -e "s#^\([[:space:]]*meta_server_endpoint:[[:space:]]*\).*#\1\"${OPS_ADDR}\"#" \
  -e "s#^\([[:space:]]*cubemaster_http_addr:[[:space:]]*\).*#\1\"${MASTER_HTTP_ADDR}\"#" \
  "${CUBELET_DYNAMICCONF}"
log "updated cubelet dynamic meta_server_endpoint=${OPS_ADDR} cubemaster_http_addr=${MASTER_HTTP_ADDR}"
