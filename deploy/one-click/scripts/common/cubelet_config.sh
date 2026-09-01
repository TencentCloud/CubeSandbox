# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Shared Cubelet config.toml helpers. Sourced by callers that already define
# log() and die().

if [[ "${ONE_CLICK_CUBELET_CONFIG_LIB_LOADED:-0}" == "1" ]]; then
  return 0
fi
ONE_CLICK_CUBELET_CONFIG_LIB_LOADED=1

if ! type die >/dev/null 2>&1; then
  die() {
    echo "[cubelet-config] ERROR: $*" >&2
    exit 1
  }
fi

if ! type log >/dev/null 2>&1; then
  log() {
    echo "[cubelet-config] $*" >&2
  }
fi

# write_cubelet_cubeops_addr CONFIG_PATH ADDR
# Warehouse downloads use the same CubeOps as node register/heartbeat.
# ADDR may omit a scheme; http:// is added when missing.
write_cubelet_cubeops_addr() {
  local config_path="${1:-}"
  local addr="${2:-}"
  [[ -n "${config_path}" ]] || die "write_cubelet_cubeops_addr requires a config path"
  [[ -n "${addr}" ]] || die "write_cubelet_cubeops_addr requires an address"
  [[ -f "${config_path}" ]] || die "required file not found: ${config_path}"
  if [[ "${addr}" != http://* && "${addr}" != https://* ]]; then
    addr="http://${addr}"
  fi
  if grep -Eq '^[[:space:]]*cubeops_addr[[:space:]]*=' "${config_path}"; then
    sed -i -e "s#^[[:space:]]*cubeops_addr[[:space:]]*=.*#    cubeops_addr = \"${addr}\"#" "${config_path}"
  else
    sed -i -e "/node_status_update_frequency/a\\    cubeops_addr = \"${addr}\"" "${config_path}"
  fi
  log "updated cubelet cubeops_addr=${addr}"
}
