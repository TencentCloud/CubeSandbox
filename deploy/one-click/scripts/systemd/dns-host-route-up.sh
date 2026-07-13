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
# These addresses are interpolated verbatim into dnsmasq.conf / the NM dnsmasq.d
# drop-in below. Validate before templating (matching validate_host_port's
# convention) so a value containing a newline or config metacharacter cannot
# inject extra dnsmasq/NM directives.
validate_ipv4_literal "${DEFAULT_COREDNS_BIND_ADDR}" "CUBE_PROXY_COREDNS_BIND_ADDR"
validate_ipv4_literal "${RESOLVED_COREDNS_BIND_ADDR}" "CUBE_PROXY_RESOLVED_DNS_ADDR"
COREDNS_BIND_ADDR="${DEFAULT_COREDNS_BIND_ADDR}"
RESOLVED_LINK_NAME="${CUBE_PROXY_RESOLVED_LINK_NAME:-cube-dns0}"
RESOLVED_LINK_ADDR="${CUBE_PROXY_RESOLVED_LINK_ADDR:-${RESOLVED_COREDNS_BIND_ADDR}/32}"
NM_CONF_DIR="/etc/NetworkManager/conf.d"
NM_DNSMASQ_DIR="/etc/NetworkManager/dnsmasq.d"
NM_MAIN_CONF="${NM_CONF_DIR}/90-cubeproxy-dns.conf"
NM_DOMAIN_CONF="${NM_DNSMASQ_DIR}/90-cubeproxy-cube-app.conf"
STANDALONE_DNSMASQ_CONF="${COREDNS_DIR}/dnsmasq.conf"
DNSMASQ_PID_FILE="${SYSTEMD_RUNTIME_DIR}/cube-proxy-dnsmasq.pid"

if command -v resolvectl >/dev/null 2>&1; then
  HOST_DNS_BACKEND="systemd-resolved"
  COREDNS_BIND_ADDR="${RESOLVED_COREDNS_BIND_ADDR}"
else
  # systemd-resolved is unavailable, so choose how dnsmasq is managed. This is
  # the only path where CUBE_PROXY_DNSMASQ_MODE applies; it is ignored on hosts
  # that provide resolvectl.
  #   networkmanager (default) -> NetworkManager spawns and owns the dnsmasq plugin
  #   standalone               -> this script launches and manages dnsmasq directly
  # Use standalone on hosts where NetworkManager initializes the dnsmasq plugin
  # but never spawns the child (e.g. bonded interfaces managed via ifcfg + assume).
  case "${CUBE_PROXY_DNSMASQ_MODE:-networkmanager}" in
    networkmanager) HOST_DNS_BACKEND="networkmanager-dnsmasq" ;;
    standalone) HOST_DNS_BACKEND="standalone-dnsmasq" ;;
    *) die "unsupported CUBE_PROXY_DNSMASQ_MODE: ${CUBE_PROXY_DNSMASQ_MODE:-} (expected networkmanager or standalone)" ;;
  esac
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

# Render /etc/resolv.conf so the dummy-link IP is the primary nameserver,
# while preserving search/options and keeping one upstream fallback for
# recovery. Docker daemon will then propagate this non-loopback resolver
# into every container it spawns without making host DNS single-homed.
write_host_resolv_conf() {
  local primary="$1"
  local tmp="/etc/resolv.conf.cube-proxy.tmp"
  local -a candidates=(
    "/run/NetworkManager/no-stub-resolv.conf"
    "/var/run/NetworkManager/no-stub-resolv.conf"
    "/run/systemd/resolve/resolv.conf"
    "/etc/resolv.conf"
  )
  local src
  local line
  local nameserver
  local fallback=""
  local metadata=""

  : > "${tmp}"
  printf 'nameserver %s\n' "${primary}" >> "${tmp}"

  for src in "${candidates[@]}"; do
    [[ -f "${src}" ]] || continue

    if [[ -z "${metadata}" ]]; then
      metadata="$(awk '/^(search|domain|options|sortlist) / { print }' "${src}" 2>/dev/null || true)"
    fi

    while IFS= read -r line || [[ -n "${line}" ]]; do
      case "${line}" in
        nameserver\ *)
          nameserver="${line#nameserver }"
          nameserver="${nameserver%%#*}"
          nameserver="${nameserver%%;*}"
          nameserver="$(printf '%s' "${nameserver}" | awk '{print $1}')"
          if ! is_reserved_nameserver \
            "${nameserver}" \
            "${primary}" \
            "${DEFAULT_COREDNS_BIND_ADDR}" \
            "${RESOLVED_COREDNS_BIND_ADDR}"; then
            fallback="${nameserver}"
            break
          fi
          ;;
      esac
    done < "${src}"

    [[ -n "${fallback}" ]] && break
  done

  if [[ -n "${fallback}" ]]; then
    printf 'nameserver %s\n' "${fallback}" >> "${tmp}"
  fi

  if [[ -n "${metadata}" ]]; then
    printf '%s\n' "${metadata}" >> "${tmp}"
  fi

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
    die "dnsmasq is required for the host DNS fallback, and no supported package manager was found"
  fi
}

configure_with_resolved() {
  require_cmd resolvectl
  ensure_resolved_link
  resolvectl revert "${RESOLVED_LINK_NAME}" >/dev/null 2>&1 || true
  resolvectl dns "${RESOLVED_LINK_NAME}" "${COREDNS_BIND_ADDR}" >/dev/null
  resolvectl domain "${RESOLVED_LINK_NAME}" '~cube.app' >/dev/null
  # default-route needs systemd v240+; tolerate any failure on older releases.
  if ! default_route_err="$(resolvectl default-route "${RESOLVED_LINK_NAME}" no 2>&1 >/dev/null)"; then
    log "resolvectl default-route failed (unsupported on systemd <v240, or other error); continuing — ~cube.app routing already applies: ${default_route_err}"
  fi
  printf 'systemd-resolved\n' > "${DNS_MODE_FILE}"
  printf '%s\n' "${RESOLVED_LINK_NAME}" > "${DNS_IFACE_FILE}"
}

# Launch a dnsmasq instance we own directly, instead of relying on
# NetworkManager's dnsmasq plugin. On hosts where every real interface is
# externally managed (ifcfg + assume), NM initializes the plugin but never
# spawns the dnsmasq child, so the bind on the dummy link never happens.
start_standalone_dnsmasq() {
  # The upstream snapshot is normally already present: coredns-start.sh writes it
  # via prepare_upstream_resolv_conf, run as the ExecStartPre of
  # cube-sandbox-coredns.service (coredns-prepare.sh just execs coredns-start.sh
  # with PREPARE_ONLY=1), and ExecStartPre completes before that unit is active --
  # which is what our After=cube-sandbox-coredns.service ordering waits on. Keep a
  # bounded wait purely as a safety net (e.g. if the prepare step is ever skipped,
  # or the snapshot is removed out of band) instead of hard-failing on the first
  # missing-file check.
  local i
  local retries=20
  for ((i = 1; i <= retries; i++)); do
    [[ -f "${RESOLV_UPSTREAM_PATH}" ]] && break
    sleep 1
  done
  [[ -f "${RESOLV_UPSTREAM_PATH}" ]] || \
    die "${RESOLV_UPSTREAM_PATH} not found after ${retries}s; coredns-start.sh (in cube-sandbox-coredns.service) may have failed to write the upstream snapshot"
  ensure_systemd_runtime_dirs

  # Explicitly point dnsmasq at the non-stub upstream snapshot coredns-start.sh
  # already produced. Without resolv-file, dnsmasq would read /etc/resolv.conf,
  # which we rewrite below to point at dnsmasq itself -- a resolution loop.
  # No dhcp-range is configured, so this instance is DNS-only; bind-interfaces
  # makes it honor listen-address strictly.
  cat > "${STANDALONE_DNSMASQ_CONF}" <<EOF
listen-address=127.0.0.1,${RESOLVED_COREDNS_BIND_ADDR}
bind-interfaces
server=/cube.app/${COREDNS_BIND_ADDR}#53
resolv-file=${RESOLV_UPSTREAM_PATH}
pid-file=${DNSMASQ_PID_FILE}
EOF
  # This config exposes internal DNS topology (dummy-link IP, CoreDNS address,
  # upstream snapshot path), so keep it non-world-readable. Do NOT restrict the
  # upstream snapshot: coredns-start.sh bind-mounts it into the CoreDNS container
  # as /etc/resolv.conf:ro, so tightening its mode would break a non-root CoreDNS
  # image, and it only holds the upstream nameserver IPs already present in the
  # world-readable /etc/resolv.conf.
  chmod 0640 "${STANDALONE_DNSMASQ_CONF}"

  # Clean up any dnsmasq we previously launched from this same config before
  # starting a fresh one, so the new instance can bind ${RESOLVED_COREDNS_BIND_ADDR}:53.
  # stop_dnsmasq_by_conf falls back to a process-table scan when the pid-file is
  # stale/missing, so a leftover instance is still reaped after a crash-restart.
  stop_dnsmasq_by_conf "${DNSMASQ_PID_FILE}" "${STANDALONE_DNSMASQ_CONF}" 10
  rm -f "${DNSMASQ_PID_FILE}"

  # NetworkManager's own dnsmasq plugin child may still hold
  # ${RESOLVED_COREDNS_BIND_ADDR}:53 for a moment after we restart NM (unclean
  # exit, child reparenting, or a slow stop). Launching immediately would then
  # fail to bind with "Address already in use" and abort under set -e. Retry a
  # bounded number of times, waiting for the port to free between attempts.
  local attempts=5
  for ((i = 1; i <= attempts; i++)); do
    if command_output_contains_fixed_string "${RESOLVED_COREDNS_BIND_ADDR}:53" \
      ss -lnu "( sport = :53 )"; then
      log "dnsmasq launch: ${RESOLVED_COREDNS_BIND_ADDR}:53 still in use (attempt ${i}/${attempts}); waiting for it to free"
      sleep 1
      continue
    fi
    if dnsmasq --conf-file="${STANDALONE_DNSMASQ_CONF}"; then
      return 0
    fi
    log "dnsmasq launch failed (attempt ${i}/${attempts}); retrying"
    sleep 1
  done
  die "dnsmasq failed to launch after ${attempts} attempts; ${RESOLVED_COREDNS_BIND_ADDR}:53 may still be held by another process"
}

# Shared tail for both dnsmasq backends. By this point dnsmasq has been started
# (directly, or by NetworkManager). Wait for it to bind the dummy-link IP, wait
# for CoreDNS, then switch host resolv.conf.
finalize_dnsmasq_backend() {
  local mode="$1"
  local bind_err="$2"

  wait_for_udp_port "${RESOLVED_COREDNS_BIND_ADDR}" 53 30 1 || die "${bind_err}"

  # Persist the teardown markers now that dnsmasq is bound, BEFORE the CoreDNS
  # wait below can die. On the standalone path dns-host-route-down.sh keys its
  # cleanup (stopping the dnsmasq we own) off DNS_MODE_FILE; writing it only
  # after a successful CoreDNS wait would orphan that dnsmasq if CoreDNS never
  # comes up. The NM path is harmless here too -- NM owns its own child.
  printf '%s\n' "${mode}" > "${DNS_MODE_FILE}"
  printf '%s\n' "${RESOLVED_LINK_NAME}" > "${DNS_IFACE_FILE}"

  # Keep the host on its original upstream DNS until CoreDNS is actually
  # listening. Otherwise a failed/slow CoreDNS start can strand docker pulls.
  wait_for_udp_port "${COREDNS_BIND_ADDR}" 53 20 1 || \
    die "coredns did not become ready on ${COREDNS_BIND_ADDR}:53; refusing to switch host resolv.conf"
  write_host_resolv_conf "${RESOLVED_COREDNS_BIND_ADDR}"
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

  finalize_dnsmasq_backend "networkmanager-dnsmasq" \
    "dnsmasq did not bind ${RESOLVED_COREDNS_BIND_ADDR}:53 after NetworkManager restart"
}

configure_with_standalone_dnsmasq() {
  install_dnsmasq

  # Reuse the same dummy link the resolvectl path uses, so dnsmasq has a
  # stable, non-loopback IP that Docker can hand to every container.
  ensure_resolved_link

  # Take /etc/resolv.conf out of NetworkManager's hands (rc-manager=unmanaged),
  # so it does not overwrite the nameserver we set below with an interface's DNS.
  # We no longer use NM's own dnsmasq, so drop dns=dnsmasq and the NM dnsmasq.d
  # drop-in that a prior version installed.
  if networkmanager_available; then
    mkdir -p "${NM_CONF_DIR}"
    cat > "${NM_MAIN_CONF}" <<EOF
[main]
rc-manager=unmanaged
EOF
    rm -f "${NM_DOMAIN_CONF}"
    # Best-effort, matching the teardown path (dns-host-route-down.sh). NM only
    # needs to stop owning /etc/resolv.conf here; we manage dnsmasq ourselves,
    # so a restart failure (e.g. NM loaded but not active) must not abort setup.
    systemctl restart NetworkManager >/dev/null 2>&1 || true
  fi

  start_standalone_dnsmasq

  finalize_dnsmasq_backend "standalone-dnsmasq" \
    "standalone dnsmasq did not bind ${RESOLVED_COREDNS_BIND_ADDR}:53"
}

ensure_dir "${COREDNS_DIR}"
rm -f "${DNS_MODE_FILE}" "${DNS_IFACE_FILE}"
case "${HOST_DNS_BACKEND}" in
  systemd-resolved) configure_with_resolved ;;
  standalone-dnsmasq) configure_with_standalone_dnsmasq ;;
  networkmanager-dnsmasq) configure_with_networkmanager ;;
  *) die "unsupported host DNS backend: ${HOST_DNS_BACKEND}" ;;
esac
