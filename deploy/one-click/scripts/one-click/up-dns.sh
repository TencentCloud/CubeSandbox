#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"
# shellcheck source=./coredns-compose-lib.sh
source "${SCRIPT_DIR}/coredns-compose-lib.sh"

require_root
require_cmd docker
require_cmd sed
require_cmd ip

CUBE_PROXY_DNS_ENABLE="${CUBE_PROXY_DNS_ENABLE:-1}"
[[ "${CUBE_PROXY_DNS_ENABLE}" == "1" ]] || die "CUBE_PROXY_DNS_ENABLE must be 1; cube proxy DNS is required in one-click deployment"

COREDNS_DIR="${TOOLBOX_ROOT}/coredns"
COREFILE_TEMPLATE="${COREDNS_DIR}/Corefile.template"
COREFILE_PATH="${COREDNS_DIR}/Corefile"
COREDNS_COMPOSE_TEMPLATE="${COREDNS_DIR}/docker-compose.yaml.template"
COREDNS_COMPOSE_FILE="${COREDNS_DIR}/docker-compose.yaml"
RESOLV_UPSTREAM_PATH="${COREDNS_DIR}/resolv.conf.upstream"
COREDNS_CONTAINER="${CUBE_PROXY_COREDNS_CONTAINER:-cube-proxy-coredns}"
CUBE_SANDBOX_NODE_IP="${CUBE_SANDBOX_NODE_IP:-}"
DNS_ANSWER_IP="${CUBE_PROXY_DNS_ANSWER_IP:-${CUBE_SANDBOX_NODE_IP:-127.0.0.1}}"
DEFAULT_COREDNS_BIND_ADDR="${CUBE_PROXY_COREDNS_BIND_ADDR:-127.0.0.54}"
RESOLVED_COREDNS_BIND_ADDR="${CUBE_PROXY_RESOLVED_DNS_ADDR:-169.254.254.53}"
COREDNS_BIND_ADDR="${DEFAULT_COREDNS_BIND_ADDR}"
DNS_MODE_FILE="${COREDNS_DIR}/host-dns-mode"
DNS_IFACE_FILE="${COREDNS_DIR}/host-dns-interface"
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

is_stub_nameserver() {
  local nameserver="$1"
  [[ -n "${nameserver}" ]] || return 0
  [[ "${nameserver}" == "127."* ]] && return 0
  [[ "${nameserver}" == "::1" ]] && return 0
  [[ "${nameserver}" == "0:0:0:0:0:0:0:1" ]] && return 0
  [[ "${nameserver}" == "${COREDNS_BIND_ADDR}" ]] && return 0
  return 1
}

write_upstream_resolv_conf() {
  local src_path="$1"
  local dst_path="$2"
  local tmp_path="${dst_path}.tmp.$$"
  local found_nameserver=1

  [[ -f "${src_path}" ]] || return 1

  prepare_file_output "${dst_path}"
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

  mv -f "${tmp_path}" "${dst_path}"
  return 0
}

prepare_upstream_resolv_conf() {
  local src_path
  local -a candidates=(
    "/run/systemd/resolve/resolv.conf"
    "/run/NetworkManager/no-stub-resolv.conf"
    "/var/run/NetworkManager/no-stub-resolv.conf"
    "/etc/resolv.conf"
  )

  for src_path in "${candidates[@]}"; do
    if write_upstream_resolv_conf "${src_path}" "${RESOLV_UPSTREAM_PATH}"; then
      log "using dns upstream from ${src_path}"
      return 0
    fi
  done

  die "failed to determine non-stub upstream DNS servers; checked ${candidates[*]}"
}

link_exists() {
  local link_name="$1"
  ip link show dev "${link_name}" >/dev/null 2>&1
}

link_is_dummy() {
  local link_name="$1"
  local link_details
  link_details="$(ip -d link show dev "${link_name}" 2>/dev/null || true)"
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

ensure_dir "${COREDNS_DIR}"
ensure_file "${COREFILE_TEMPLATE}"
ensure_file "${COREDNS_COMPOSE_TEMPLATE}"

if [[ "${HOST_DNS_BACKEND}" == "systemd-resolved" ]]; then
  # CoreDNS binds to the dummy link address on the resolved path,
  # so the address must exist before the container starts.
  ensure_resolved_link
fi

prepare_upstream_resolv_conf

render_template_atomic \
  "${COREFILE_TEMPLATE}" \
  "${COREFILE_PATH}" \
  -e "s/__CUBE_PROXY_DNS_ANSWER_IP__/${DNS_ANSWER_IP//\//\\/}/g" \
  -e "s/__COREDNS_BIND_ADDR__/${COREDNS_BIND_ADDR//\//\\/}/g"

render_template_atomic \
  "${COREDNS_COMPOSE_TEMPLATE}" \
  "${COREDNS_COMPOSE_FILE}" \
  -e "s/__COREDNS_CONTAINER__/${COREDNS_CONTAINER//\//\\/}/g" \
  -e "s#__COREDNS_DIR__#${COREDNS_DIR//\//\\/}#g"

ensure_bind_mount_file "${RESOLV_UPSTREAM_PATH}"

coredns_compose_run down --remove-orphans >/dev/null 2>&1 || true
coredns_compose_run up -d coredns >/dev/null

for _ in {1..20}; do
  state="$(docker inspect --format '{{.State.Status}}' "${COREDNS_CONTAINER}" 2>/dev/null || true)"
  if [[ "${state}" == "running" ]]; then
    break
  fi
  sleep 1
done

[[ "${state:-}" == "running" ]] || die "coredns failed to start"

rm -f "${DNS_MODE_FILE}" "${DNS_IFACE_FILE}"

configure_with_resolved() {
  require_cmd resolvectl
  ensure_resolved_link

  # `systemd-resolved` only treats the dummy link as a DNS scope
  # when the link is up and owns a local address.
  resolvectl revert "${RESOLVED_LINK_NAME}" >/dev/null 2>&1 || true

  resolvectl dns "${RESOLVED_LINK_NAME}" "${COREDNS_BIND_ADDR}" >/dev/null
  resolvectl domain "${RESOLVED_LINK_NAME}" '~cube.app' >/dev/null

  # default-route needs systemd v240+; tolerate any failure on older releases.
  if ! default_route_err="$(resolvectl default-route "${RESOLVED_LINK_NAME}" no 2>&1 >/dev/null)"; then
    log "resolvectl default-route failed (unsupported on systemd <v240, or other error); continuing — ~cube.app routing already applies: ${default_route_err}"
  fi

  printf 'systemd-resolved\n' > "${DNS_MODE_FILE}"
  printf '%s\n' "${RESOLVED_LINK_NAME}" > "${DNS_IFACE_FILE}"
  log "cube proxy dns routed via systemd-resolved on link ${RESOLVED_LINK_NAME}"
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

  command -v dnsmasq >/dev/null 2>&1 || die "failed to install dnsmasq for NetworkManager fallback"
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
  log "cube proxy dns routed via NetworkManager dnsmasq on dummy link ${RESOLVED_LINK_NAME}"
}

configure_with_fallback() {
  if networkmanager_available; then
    configure_with_networkmanager
    return 0
  fi

  die "host DNS fallback requires either systemd-resolved/resolvectl or NetworkManager with dnsmasq support"
}

if [[ "${HOST_DNS_BACKEND}" == "systemd-resolved" ]]; then
  configure_with_resolved
else
  configure_with_fallback
fi

log "cube proxy dns ready via ${COREDNS_CONTAINER}"
