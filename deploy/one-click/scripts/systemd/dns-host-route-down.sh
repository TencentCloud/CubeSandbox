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
RESOLV_UPSTREAM_PATH="${COREDNS_DIR}/resolv.conf.upstream"
DEFAULT_COREDNS_BIND_ADDR="${CUBE_PROXY_COREDNS_BIND_ADDR:-127.0.0.54}"
RESOLVED_COREDNS_BIND_ADDR="${CUBE_PROXY_RESOLVED_DNS_ADDR:-169.254.254.53}"
NM_MAIN_CONF="/etc/NetworkManager/conf.d/90-cubeproxy-dns.conf"
NM_DOMAIN_CONF="/etc/NetworkManager/dnsmasq.d/90-cubeproxy-cube-app.conf"
STANDALONE_DNSMASQ_CONF="${COREDNS_DIR}/dnsmasq.conf"
DNSMASQ_PID_FILE="${SYSTEMD_RUNTIME_DIR}/cube-proxy-dnsmasq.pid"

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
    # Last resort for the standalone-dnsmasq path on hosts with no resolver
    # manager: the upstream snapshot coredns-start.sh froze still holds the
    # real non-stub nameservers, so recover /etc/resolv.conf from it instead
    # of leaving it pointed at the dnsmasq stub we just stopped.
    "${RESOLV_UPSTREAM_PATH}"
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

  # No candidate yielded a non-stub nameserver. /etc/resolv.conf is still
  # pointed at the dnsmasq stub we just stopped, so host DNS is now broken.
  # Log loudly so this degraded state is detectable rather than silent.
  log "ERROR restore_non_stub_resolv_conf: no non-stub resolver found in candidates (${candidates[*]}); /etc/resolv.conf may still point at the stopped stub ${current_nameserver:-<unknown>}"
  return 1
}

# Shared teardown for both dnsmasq backends: remove the NM drop-ins, restart NM
# so it reclaims /etc/resolv.conf, restore a non-stub resolver as a safety net,
# then delete the dummy link. The only per-backend difference (stopping a
# dnsmasq we launched ourselves) is handled by the caller before this runs.
teardown_dnsmasq_backend() {
  local iface="$1"

  rm -f "${NM_DOMAIN_CONF}" "${NM_MAIN_CONF}"
  if networkmanager_available; then
    systemctl restart NetworkManager >/dev/null 2>&1 || true
  fi
  # Restore /etc/resolv.conf in case NM is not yet ready to repopulate it.
  # With rc-manager back to its default after the conf removal+restart, NM will
  # normally rewrite resolv.conf itself, but this is the safety net. On the
  # standalone path (no NM), this recovers the resolver from the upstream
  # snapshot instead of leaving it pointed at the stopped dnsmasq stub.
  restore_non_stub_resolv_conf || true
  if [[ -n "${iface}" ]] && link_exists "${iface}" && link_is_dummy "${iface}"; then
    ip link delete "${iface}" >/dev/null 2>&1 || true
  fi
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
  standalone-dnsmasq)
    # Stop the dnsmasq we launched ourselves before the shared teardown; the NM
    # path leaves its dnsmasq plugin child to NM's own restart.
    stop_dnsmasq_by_conf "${DNSMASQ_PID_FILE}" "${STANDALONE_DNSMASQ_CONF}" 10
    rm -f "${STANDALONE_DNSMASQ_CONF}" "${DNSMASQ_PID_FILE}"
    teardown_dnsmasq_backend "${iface}"
    ;;
  networkmanager-dnsmasq)
    teardown_dnsmasq_backend "${iface}"
    ;;
esac

rm -f "${DNS_MODE_FILE}" "${DNS_IFACE_FILE}"
