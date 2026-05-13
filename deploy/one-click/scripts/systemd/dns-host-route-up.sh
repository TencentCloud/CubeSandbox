#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd ip

COREDNS_DIR="${TOOLBOX_ROOT}/coredns"
DNS_MODE_FILE="${COREDNS_DIR}/host-dns-mode"
DNS_IFACE_FILE="${COREDNS_DIR}/host-dns-interface"
DEFAULT_COREDNS_BIND_ADDR="${CUBE_PROXY_COREDNS_BIND_ADDR:-127.0.0.54}"
RESOLVED_COREDNS_BIND_ADDR="${CUBE_PROXY_RESOLVED_DNS_ADDR:-169.254.254.53}"
COREDNS_BIND_ADDR="${DEFAULT_COREDNS_BIND_ADDR}"
RESOLVED_LINK_NAME="${CUBE_PROXY_RESOLVED_LINK_NAME:-cube-dns0}"
RESOLVED_LINK_ADDR="${CUBE_PROXY_RESOLVED_LINK_ADDR:-${RESOLVED_COREDNS_BIND_ADDR}/32}"
NM_CONF_DIR="/etc/NetworkManager/conf.d"
NM_DNSMASQ_DIR="/etc/NetworkManager/dnsmasq.d"
NM_MAIN_CONF="${NM_CONF_DIR}/90-cubeproxy-dns.conf"
NM_DOMAIN_CONF="${NM_DNSMASQ_DIR}/90-cubeproxy-cube-app.conf"
HOST_DNS_BACKEND="networkmanager-dnsmasq"

if command -v resolvectl >/dev/null 2>&1; then
  HOST_DNS_BACKEND="systemd-resolved"
  COREDNS_BIND_ADDR="${RESOLVED_COREDNS_BIND_ADDR}"
fi

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

ensure_resolved_link() {
  if link_exists "${RESOLVED_LINK_NAME}"; then
    link_is_dummy "${RESOLVED_LINK_NAME}" || die "existing link ${RESOLVED_LINK_NAME} is not a dummy link"
  else
    ip link add "${RESOLVED_LINK_NAME}" type dummy
  fi

  ip link set "${RESOLVED_LINK_NAME}" up
  ip addr replace "${RESOLVED_LINK_ADDR}" dev "${RESOLVED_LINK_NAME}"
}

install_dnsmasq() {
  if command -v dnsmasq >/dev/null 2>&1; then
    return 0
  fi

  if command -v dnf >/dev/null 2>&1; then
    dnf install -y dnsmasq >/dev/null
  elif command -v yum >/dev/null 2>&1; then
    yum install -y dnsmasq >/dev/null
  elif command -v apt-get >/dev/null 2>&1; then
    apt-get update >/dev/null
    DEBIAN_FRONTEND=noninteractive apt-get install -y dnsmasq >/dev/null
  else
    die "dnsmasq is required for NetworkManager fallback, and no supported package manager was found"
  fi
}

configure_with_resolved() {
  require_cmd resolvectl
  ensure_resolved_link
  resolvectl revert "${RESOLVED_LINK_NAME}" >/dev/null 2>&1 || true
  resolvectl dns "${RESOLVED_LINK_NAME}" "${COREDNS_BIND_ADDR}" >/dev/null
  resolvectl domain "${RESOLVED_LINK_NAME}" '~cube.app' >/dev/null
  resolvectl default-route "${RESOLVED_LINK_NAME}" no >/dev/null
  printf 'systemd-resolved\n' > "${DNS_MODE_FILE}"
  printf '%s\n' "${RESOLVED_LINK_NAME}" > "${DNS_IFACE_FILE}"
}

configure_with_networkmanager() {
  require_cmd systemctl
  networkmanager_available || die "NetworkManager is not available for DNS fallback"
  install_dnsmasq
  mkdir -p "${NM_CONF_DIR}" "${NM_DNSMASQ_DIR}"
  cat > "${NM_MAIN_CONF}" <<EOF
[main]
dns=dnsmasq
EOF
  cat > "${NM_DOMAIN_CONF}" <<EOF
server=/cube.app/${COREDNS_BIND_ADDR}#53
EOF
  systemctl restart NetworkManager >/dev/null
  printf 'networkmanager-dnsmasq\n' > "${DNS_MODE_FILE}"
}

ensure_dir "${COREDNS_DIR}"
rm -f "${DNS_MODE_FILE}" "${DNS_IFACE_FILE}"
if [[ "${HOST_DNS_BACKEND}" == "systemd-resolved" ]]; then
  configure_with_resolved
else
  configure_with_networkmanager
fi
