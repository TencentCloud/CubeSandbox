#!/usr/bin/env bash
#
# CubeSandbox transparent proxy — host-side network setup.
# Phase 1: full MITM via OpenResty + TPROXY.
#
# Selection model: traffic is matched purely by ingress interface and
# destination port — `iif cube-dev` + tcp dport 80/443. There is NO
# fwmark involved in either the forward (sandbox→OpenResty) or the
# return (OpenResty→sandbox) direction. The cube-egress worker's
# replies are routed naturally by the kernel and re-injected into the
# sandbox tap by the from_envoy BPF program on cube-dev egress.
#
# Idempotent: safe to re-run. Rules live in a dedicated TRANSPROXY
# sub-chain so 'down' tears down our config without touching anything
# else in mangle/PREROUTING.
#
# Usage:
#   sudo cube-proxy-iptables-init.sh up      # install rules
#   sudo cube-proxy-iptables-init.sh down    # remove rules
#   sudo cube-proxy-iptables-init.sh status  # show installed rules
#
# Required before this runs:
#   - cube-dev interface exists (host-side gateway iface for sandbox VMs)
#   - the cube-egress container is reachable on TPROXY_ON_IP:TPROXY_PORT_*
#     (it shares the host network namespace, so `--on-ip <cube-dev gateway>`
#     hits OpenResty's matching transparent listeners).
set -euo pipefail

log()   { printf '[iptables-init] %s\n' "$*" >&2; }
fatal() { log "FATAL: $*"; exit 1; }

sandbox_gateway_ip_from_cidr() {
    local cidr="$1"
    local ip="${cidr%/*}"
    local mask="${cidr#*/}"
    local a b c d ip_int host_bits mask_int network_int

    [[ "${cidr}" == */* && "${ip}" != "${cidr}" && "${mask}" =~ ^[0-9]+$ ]] \
        || fatal "invalid CUBE_SANDBOX_NETWORK_CIDR: ${cidr}"
    [[ "${ip}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] \
        || fatal "invalid CUBE_SANDBOX_NETWORK_CIDR address: ${cidr}"
    IFS=. read -r a b c d <<< "${ip}"
    local octet
    for octet in "${a}" "${b}" "${c}" "${d}"; do
        [[ "${octet}" =~ ^[0-9]{1,3}$ ]] || fatal "invalid CUBE_SANDBOX_NETWORK_CIDR address: ${cidr}"
        (( 10#${octet} >= 0 && 10#${octet} <= 255 )) || fatal "invalid CUBE_SANDBOX_NETWORK_CIDR address: ${cidr}"
    done

    host_bits=$(( 32 - 10#${mask} ))
    (( host_bits >= 1 && host_bits <= 31 )) || fatal "invalid CUBE_SANDBOX_NETWORK_CIDR mask: ${cidr}"
    ip_int=$(( (10#${a} << 24) + (10#${b} << 16) + (10#${c} << 8) + 10#${d} ))
    mask_int=$(( (0xFFFFFFFF << host_bits) & 0xFFFFFFFF ))
    network_int=$(( ip_int & mask_int ))

    printf '%s.%s.%s.%s\n' \
        "$(( ((network_int + 1) >> 24) & 255 ))" \
        "$(( ((network_int + 1) >> 16) & 255 ))" \
        "$(( ((network_int + 1) >> 8) & 255 ))" \
        "$(( (network_int + 1) & 255 ))"
}

# -------- Tunables (must match nginx.conf) --------
SANDBOX_NETWORK_CIDR="${CUBE_SANDBOX_NETWORK_CIDR:-192.168.0.0/18}"
TPROXY_ON_IP="$(sandbox_gateway_ip_from_cidr "${SANDBOX_NETWORK_CIDR}")"  # cube-dev IP
TPROXY_PORT_HTTP=8080
TPROXY_PORT_HTTPS=8443
ROUTE_TABLE=100
INGRESS_IFACE="${CUBE_INGRESS_IFACE:-cube-dev}"
CHAIN="TRANSPROXY"

require_root()  { [[ "$(id -u)" -eq 0 ]] || fatal "must run as root"; }
require_iface() { ip link show "${INGRESS_IFACE}" &>/dev/null \
                       || fatal "interface ${INGRESS_IFACE} not found"; }

require_modules() {
    local m
    for m in xt_TPROXY xt_socket nf_tproxy_ipv4; do
        if ! modprobe "${m}" 2>/dev/null; then
            log "WARN: modprobe ${m} failed (may be built-in)"
        fi
    done
}

# Create-or-flush our sub-chain, then ensure PREROUTING jumps to it once.
install_chain() {
    iptables -t mangle -N "${CHAIN}" 2>/dev/null || true
    iptables -t mangle -F "${CHAIN}"

    iptables -t mangle -C PREROUTING -j "${CHAIN}" 2>/dev/null \
        || iptables -t mangle -A PREROUTING -j "${CHAIN}"

    iptables -t mangle -A "${CHAIN}" \
        -i "${INGRESS_IFACE}" -p tcp --dport 80 \
        -j TPROXY --on-ip "${TPROXY_ON_IP}" --on-port "${TPROXY_PORT_HTTP}"

    iptables -t mangle -A "${CHAIN}" \
        -i "${INGRESS_IFACE}" -p tcp --dport 443 \
        -j TPROXY --on-ip "${TPROXY_ON_IP}" --on-port "${TPROXY_PORT_HTTPS}"

    iptables -t mangle -A "${CHAIN}" -j RETURN
}

install_routing() {
    # Two ip rules: tcp/80 and tcp/443 from cube-dev → table 100.
    # Match by selectors (iif/ipproto/dport), not fwmark.
    local proto port
    for port in 80 443; do
        if ! ip rule show \
             | grep -q "iif ${INGRESS_IFACE} ipproto tcp dport ${port} lookup ${ROUTE_TABLE}"; then
            ip rule add iif "${INGRESS_IFACE}" ipproto tcp dport "${port}" \
                       table "${ROUTE_TABLE}"
        fi
    done

    if ! ip route show table "${ROUTE_TABLE}" | grep -q "local 0.0.0.0/0 dev lo"; then
        ip route add local 0.0.0.0/0 dev lo table "${ROUTE_TABLE}"
    fi
}

remove_chain() {
    # Remove jump from PREROUTING, then flush+delete the sub-chain.
    while iptables -t mangle -C PREROUTING -j "${CHAIN}" 2>/dev/null; do
        iptables -t mangle -D PREROUTING -j "${CHAIN}" || break
    done
    iptables -t mangle -F "${CHAIN}" 2>/dev/null || true
    iptables -t mangle -X "${CHAIN}" 2>/dev/null || true
}

remove_routing() {
    local port
    for port in 80 443; do
        while ip rule show \
              | grep -q "iif ${INGRESS_IFACE} ipproto tcp dport ${port} lookup ${ROUTE_TABLE}"; do
            ip rule del iif "${INGRESS_IFACE}" ipproto tcp dport "${port}" \
                       table "${ROUTE_TABLE}" || break
        done
    done
    ip route flush table "${ROUTE_TABLE}" 2>/dev/null || true
}

show_status() {
    log "=== mangle/${CHAIN} ==="
    iptables -t mangle -L "${CHAIN}" -n -v --line-numbers 2>/dev/null \
        || log "(chain absent)"

    log "=== mangle/PREROUTING jump ==="
    iptables -t mangle -L PREROUTING -n -v --line-numbers \
        | grep -E "(${CHAIN}|^Chain|^num)" || true

    log "=== ip rule (table ${ROUTE_TABLE}) ==="
    ip rule show | grep "lookup ${ROUTE_TABLE}" || log "(no rule)"

    log "=== ip route table ${ROUTE_TABLE} ==="
    ip route show table "${ROUTE_TABLE}" || log "(empty)"
}

main() {
    local action="${1:-up}"
    case "${action}" in
        up)
            require_root
            require_iface
            require_modules
            install_chain
            install_routing
            log "cube-proxy iptables/route rules installed"
            show_status
            ;;
        down)
            require_root
            remove_chain
            remove_routing
            log "cube-proxy iptables/route rules removed"
            ;;
        status)
            show_status
            ;;
        *)
            echo "usage: $0 {up|down|status}" >&2
            exit 2
            ;;
    esac
}

main "$@"
