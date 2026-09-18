#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd sed

CUBELET_CONFIG="${TOOLBOX_ROOT}/Cubelet/config/config.toml"
ensure_file "${CUBELET_CONFIG}"

# Warehouse downloads need cubeops_addr on every node that runs Cubelet,
# including all-in-one control. CUBE_OPS_ADDR overrides the resolved address.
OPS_ADDR="$(resolve_control_plane_cubeops_addr)"
write_cubelet_cubeops_addr "${CUBELET_CONFIG}" "${CUBE_OPS_ADDR:-${OPS_ADDR}}"

# ops-agent: node-local agent for CubeOps-driven config. Runs wherever cubelet
# does (compute role and all-in-one control), so the config is regenerated on
# every run before the role split. A missing NODE_IP skips the rewrite (warn
# only) so control nodes without it keep cubelet startup unaffected.
if [[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]]; then
  # A missing token must not overwrite a good one with shared_token: "".
  if [[ -z "${CUBE_OPS_OPSAGENT_TOKEN:-}" ]]; then
    if is_compute_role; then
      die "CUBE_OPS_OPSAGENT_TOKEN is required for the compute role"
    fi
    log "CUBE_OPS_OPSAGENT_TOKEN empty; skipping ops-agent config rewrite"
  else
    OPS_AGENT_CONF_DIR="${TOOLBOX_ROOT}/ops-agent/conf"
    mkdir -p "${OPS_AGENT_CONF_DIR}"
    cat > "${OPS_AGENT_CONF_DIR}/config.yaml" <<EOF
node_id: "${CUBE_SANDBOX_NODE_IP}"
listen_addr: "0.0.0.0:8890"
cubeops_url: "http://${OPS_ADDR}"
dynamicconf_path: "${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"
reconcile_interval: 300s
backup_keep: 5
shared_token: "${CUBE_OPS_OPSAGENT_TOKEN}"
log_level: "info"
log_dir: "/data/log/ops-agent"
EOF
    # The file holds the shared push secret; tighten like other secret files.
    chmod 600 "${OPS_AGENT_CONF_DIR}/config.yaml"
    log "updated ops-agent config node_id=${CUBE_SANDBOX_NODE_IP} cubeops_url=http://${OPS_ADDR}"
  fi
else
  log "CUBE_SANDBOX_NODE_IP empty; skipping ops-agent config rewrite"
fi

if ! is_compute_role; then
  exit 0
fi

CUBELET_DYNAMICCONF="${TOOLBOX_ROOT}/Cubelet/dynamicconf/conf.yaml"
ensure_file "${CUBELET_DYNAMICCONF}"
[[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]] || die "CUBE_SANDBOX_NODE_IP is required for compute role"

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
