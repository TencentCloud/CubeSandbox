#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd ip
require_cmd awk

COREDNS_DIR="${TOOLBOX_ROOT}/coredns"
DNS_MODE_FILE="${COREDNS_DIR}/host-dns-mode"
DNS_IFACE_FILE="${COREDNS_DIR}/host-dns-interface"
DEFAULT_COREDNS_BIND_ADDR="${CUBE_PROXY_COREDNS_BIND_ADDR:-127.0.0.54}"
RESOLVED_COREDNS_BIND_ADDR="${CUBE_PROXY_RESOLVED_DNS_ADDR:-169.254.254.53}"
NM_MAIN_CONF="/etc/NetworkManager/conf.d/90-cubeproxy-dns.conf"
NM_DOMAIN_CONF="/etc/NetworkManager/dnsmasq.d/90-cubeproxy-cube-app.conf"

networkmanager_available() {
  command -v systemctl >/dev/null 2>&1 || return 1
  [[ "$(systemctl show -p LoadState --value NetworkManager 2>/dev/null || true)" == "loaded" ]]
}

link_exists() {
  ip link show dev "$1" >/dev/null 2>&1
}

link_is_dummy() {
  local link_details
  link_details="$(ip -d link show dev "$1" 2>/dev/null || true)"
  [[ "${link_details}" == *" dummy "* || "${link_details}" == *"dummy "* ]]
}

is_stub_nameserver() {
  is_reserved_nameserver \
    "${1:-}" \
    "${DEFAULT_COREDNS_BIND_ADDR}" \
    "${RESOLVED_COREDNS_BIND_ADDR}"
}

copy_non_stub_resolv_conf_if_needed() {
  local src_path="$1"
  local tmp_path="/etc/resolv.conf.one-click.tmp"
  local found_nameserver=1

  [[ -f "${src_path}" ]] || return 1
  : > "${tmp_path}"
  while IFS= read -r line || [[ -n "${line}" ]]; do
    case "${line}" in
      nameserver\ *)
        local nameserver="${line#nameserver }"
        nameserver="${nameserver%%#*}"
        nameserver="${nameserver%%;*}"
        nameserver="$(printf '%s' "${nameserver}" | awk '{print $1}')"
        if ! is_stub_nameserver "${nameserver}"; then
          printf 'nameserver %s\n' "${nameserver}" >> "${tmp_path}"
          found_nameserver=0
        fi
        ;;
      search\ *|domain\ *|options\ *|sortlist\ *)
        printf '%s\n' "${line}" >> "${tmp_path}"
        ;;
      \#*|'')
        printf '%s\n' "${line}" >> "${tmp_path}"
        ;;
    esac
  done < "${src_path}"

  if [[ "${found_nameserver}" -ne 0 ]]; then
    rm -f "${tmp_path}"
    return 1
  fi

  cp -f "${tmp_path}" /etc/resolv.conf
  rm -f "${tmp_path}"
  return 0
}

restore_non_stub_resolv_conf() {
  local current_nameserver=""
  local -a candidates=(
    "/run/systemd/resolve/resolv.conf"
    "/run/NetworkManager/no-stub-resolv.conf"
    "/var/run/NetworkManager/no-stub-resolv.conf"
  )

  if [[ -f /etc/resolv.conf ]]; then
    current_nameserver="$(awk '/^nameserver[[:space:]]+/ {print $2; exit}' /etc/resolv.conf)"
  fi
  if [[ -n "${current_nameserver}" ]] && ! is_stub_nameserver "${current_nameserver}"; then
    return 0
  fi

  local src_path
  for src_path in "${candidates[@]}"; do
    if copy_non_stub_resolv_conf_if_needed "${src_path}"; then
      return 0
    fi
  done
}

mode=""
iface=""
[[ -f "${DNS_MODE_FILE}" ]] && mode="$(<"${DNS_MODE_FILE}")"
[[ -f "${DNS_IFACE_FILE}" ]] && iface="$(<"${DNS_IFACE_FILE}")"

case "${mode}" in
  systemd-resolved)
    if [[ -n "${iface}" ]] && command -v resolvectl >/dev/null 2>&1; then
      resolvectl revert "${iface}" >/dev/null 2>&1 || true
    fi
    if [[ -n "${iface}" ]] && link_exists "${iface}" && link_is_dummy "${iface}"; then
      ip link delete "${iface}" >/dev/null 2>&1 || true
    fi
    ;;
  networkmanager-dnsmasq)
    rm -f "${NM_DOMAIN_CONF}" "${NM_MAIN_CONF}"
    if networkmanager_available; then
      systemctl restart NetworkManager >/dev/null 2>&1 || true
    fi
    # Restore /etc/resolv.conf in case NM is not yet ready to repopulate it.
    # With rc-manager back to its default after the conf removal+restart,
    # NM will normally rewrite resolv.conf itself, but this is the safety net.
    restore_non_stub_resolv_conf
    # The NM path now uses the same dummy link as the systemd-resolved
    # path to host dnsmasq, so tear it down here as well.
    if [[ -n "${iface}" ]] && link_exists "${iface}" && link_is_dummy "${iface}"; then
      ip link delete "${iface}" >/dev/null 2>&1 || true
    fi
    ;;
esac

rm -f "${DNS_MODE_FILE}" "${DNS_IFACE_FILE}"
