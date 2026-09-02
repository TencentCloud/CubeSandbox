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

# Live [cow.s3] header: optional indent, then the table name. Comments and
# in-line mentions do not count.
cubelet_s3_header_re() {
  printf '%s' '^[[:space:]]*\[plugins\."io\.cubelet\.internal\.v1\.storage"\.cow\.s3\]'
}

count_live_cubelet_s3_headers() {
  grep -Ec "$(cubelet_s3_header_re)" "${1}" || true
}

# write_cubelet_s3lvol_enable CONFIG_PATH 0|1
# Flip [cow.s3] enable from ONE_CLICK_ENABLE_S3LVOL. socket_path is left
# alone when already set. If the section is missing, insert it after
# cow.log / before images; if neither anchor exists, append at EOF.
# Pre-scan live (uncommented) headers only: never insert when a real
# [cow.s3] table already exists anywhere, so TOML does not gain a second table.
write_cubelet_s3lvol_enable() {
  local config_path="${1:-}"
  local flag="${2:-}"
  local enable_toml
  local header='[plugins."io.cubelet.internal.v1.storage".cow.s3]'
  local cow_log='[plugins."io.cubelet.internal.v1.storage".cow.log]'
  local images='[plugins."io.cubelet.internal.v1.images"]'
  local sock="/var/run/s3lvol.sock"
  local tmp
  local header_count
  local enable_in_section=0
  [[ -n "${config_path}" ]] || die "write_cubelet_s3lvol_enable requires a config path"
  [[ -f "${config_path}" ]] || die "required file not found: ${config_path}"
  case "${flag}" in
    0) enable_toml="false" ;;
    1) enable_toml="true" ;;
    *) die "write_cubelet_s3lvol_enable requires 0 or 1 (got: '${flag}')" ;;
  esac
  tmp="$(mktemp "${config_path}.XXXXXX")"
  if grep -Eq "$(cubelet_s3_header_re)" "${config_path}"; then
    awk -v enable_toml="${enable_toml}" -v header="${header}" -v sock="${sock}" '
      function is_table(line, name,    t) {
        t = line
        sub(/^[[:space:]]+/, "", t)
        return index(t, name) == 1
      }
      function flush_s3() {
        if (in_s3) {
          if (!found_enable) {
            print "    enable = " enable_toml
          }
          if (!found_socket) {
            print "    socket_path = \"" sock "\""
          }
          in_s3 = 0
        }
      }
      is_table($0, header) {
        in_s3 = 1
        print
        next
      }
      in_s3 && /^[[:space:]]*\[/ {
        flush_s3()
      }
      in_s3 && /^[[:space:]]*enable[[:space:]]*=/ {
        print "    enable = " enable_toml
        found_enable = 1
        next
      }
      in_s3 && /^[[:space:]]*socket_path[[:space:]]*=/ {
        found_socket = 1
        print
        next
      }
      { print }
      END { flush_s3() }
    ' "${config_path}" > "${tmp}"
  else
    awk -v enable_toml="${enable_toml}" -v header="${header}" \
        -v cow_log="${cow_log}" -v images="${images}" -v sock="${sock}" '
      function is_table(line, name,    t) {
        t = line
        sub(/^[[:space:]]+/, "", t)
        return index(t, name) == 1
      }
      function emit_section() {
        print "    " header
        print "    enable = " enable_toml
        print "    socket_path = \"" sock "\""
        print ""
        inserted = 1
      }
      is_table($0, cow_log) {
        in_log = 1
        print
        next
      }
      in_log && /^[[:space:]]*\[/ {
        in_log = 0
        if (!inserted) {
          emit_section()
        }
      }
      !inserted && is_table($0, images) {
        emit_section()
      }
      { print }
      END {
        if (!inserted) {
          emit_section()
        }
      }
    ' "${config_path}" > "${tmp}"
  fi
  header_count="$(count_live_cubelet_s3_headers "${tmp}")"
  if [[ "${header_count}" -ne 1 ]]; then
    rm -f "${tmp}"
    die "expected exactly one live ${header} (got ${header_count})"
  fi
  enable_in_section="$(awk -v header="${header}" -v enable_toml="${enable_toml}" '
    function is_table(line, name,    t) {
      t = line
      sub(/^[[:space:]]+/, "", t)
      return index(t, name) == 1
    }
    is_table($0, header) { in_s3 = 1; next }
    in_s3 && /^[[:space:]]*\[/ { in_s3 = 0 }
    in_s3 && $0 ~ ("^[[:space:]]*enable[[:space:]]*=[[:space:]]*" enable_toml "[[:space:]]*$") {
      found = 1
    }
    END { print found + 0 }
  ' "${tmp}")"
  if [[ "${enable_in_section}" -ne 1 ]]; then
    rm -f "${tmp}"
    die "failed to set ${header} enable = ${enable_toml}"
  fi
  mv -f "${tmp}" "${config_path}"
  log "updated cubelet cow.s3 enable=${enable_toml}"
}
