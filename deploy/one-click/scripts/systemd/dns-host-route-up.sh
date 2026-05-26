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
  elif ! ip link add "${RESOLVED_LINK_NAME}" type dummy; then
    # CoreDNS and host DNS routing may start concurrently under systemd.
    # If another helper created the link first, accept it after validation.
    link_exists "${RESOLVED_LINK_NAME}" || die "failed to create dummy link ${RESOLVED_LINK_NAME}"
    link_is_dummy "${RESOLVED_LINK_NAME}" || die "existing link ${RESOLVED_LINK_NAME} is not a dummy link"
  fi

  ip link set "${RESOLVED_LINK_NAME}" up
  ip addr replace "${RESOLVED_LINK_ADDR}" dev "${RESOLVED_LINK_NAME}"
}

# Wait until a UDP socket is bound on ip:port. Used after restarting
# NetworkManager to confirm dnsmasq picked up the extended listen-address.
wait_for_udp_listen() {
  local ip="$1"
  local port="$2"
  local retries="${3:-30}"
  local i
  require_cmd ss
  for ((i = 1; i <= retries; i++)); do
    if ss -lnup "( sport = :${port} )" 2>/dev/null | grep -q -- "${ip}:${port}"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# Render /etc/resolv.conf so its only nameserver is the dummy-link IP
# we just bound dnsmasq to, while preserving search/options pulled from
# NetworkManager's non-stub snapshot. Docker daemon will then propagate
# this non-loopback nameserver into every container it spawns.
write_host_resolv_conf() {
  local primary="$1"
  local tmp="/etc/resolv.conf.cube-proxy.tmp"
  local -a candidates=(
    "/run/NetworkManager/no-stub-resolv.conf"
    "/var/run/NetworkManager/no-stub-resolv.conf"
    "/run/systemd/resolve/resolv.conf"
    "/etc/resolv.conf"
  )
  : > "${tmp}"
  printf 'nameserver %s\n' "${primary}" >> "${tmp}"
  local src
  for src in "${candidates[@]}"; do
    [[ -f "${src}" ]] || continue
    awk '/^(search|domain|options|sortlist) /' "${src}" >> "${tmp}"
    break
  done
  install -m 0644 "${tmp}" /etc/resolv.conf
  rm -f "${tmp}"
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

  # Reuse the same dummy link the resolvectl path uses, so dnsmasq has a
  # stable, non-loopback IP that Docker can hand to every container.
  ensure_resolved_link

  mkdir -p "${NM_CONF_DIR}" "${NM_DNSMASQ_DIR}"

  # Keep NetworkManager driving dnsmasq, but take /etc/resolv.conf out of
  # its hands (rc-manager=unmanaged). The default NM behavior would rewrite
  # resolv.conf to "nameserver 127.0.0.1", which the Docker daemon treats as
  # unreachable from inside containers and silently replaces with 8.8.8.8.
  cat > "${NM_MAIN_CONF}" <<EOF
[main]
dns=dnsmasq
rc-manager=unmanaged
EOF

  # Make NM's dnsmasq bind both 127.0.0.1 (for host stub clients) and the
  # dummy-link IP (for containers reaching us via the docker bridge gateway).
  # bind-interfaces is required so dnsmasq honors listen-address strictly.
  cat > "${NM_DOMAIN_CONF}" <<EOF
listen-address=127.0.0.1,${RESOLVED_COREDNS_BIND_ADDR}
bind-interfaces
server=/cube.app/${COREDNS_BIND_ADDR}#53
EOF

  systemctl restart NetworkManager >/dev/null

  wait_for_udp_listen "${RESOLVED_COREDNS_BIND_ADDR}" 53 30 || \
    die "dnsmasq did not bind ${RESOLVED_COREDNS_BIND_ADDR}:53 after NetworkManager restart"
  write_host_resolv_conf "${RESOLVED_COREDNS_BIND_ADDR}"

  printf 'networkmanager-dnsmasq\n' > "${DNS_MODE_FILE}"
  printf '%s\n' "${RESOLVED_LINK_NAME}" > "${DNS_IFACE_FILE}"
}

ensure_dir "${COREDNS_DIR}"
rm -f "${DNS_MODE_FILE}" "${DNS_IFACE_FILE}"
if [[ "${HOST_DNS_BACKEND}" == "systemd-resolved" ]]; then
  configure_with_resolved
else
  configure_with_networkmanager
fi
