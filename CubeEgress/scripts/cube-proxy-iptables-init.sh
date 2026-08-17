#!/usr/bin/env bash
#
# CubeSandbox transparent proxy — host-side network setup.
# Phase 1: full MITM via OpenResty + TPROXY.
#
# Selection model: traffic is matched by ingress interface and by an
# skb->mark stamped from the sandbox tap's eBPF datapath. The mvmtap
# program reads allow_out_v3 to find the (host, port) → scheme mapping
# for the outgoing SYN, then writes CUBE_L7_MARK_HTTP (0xCE010000) or
# CUBE_L7_MARK_HTTPS (0xCE020000) so this chain can steer the packet at
# 8080 (nginx HTTP listener) or 8443 (nginx HTTPS listener) regardless
# of the original destination port. This lets users configure L7
# capture on arbitrary ports (e.g. an API on tcp/3000) without teaching
# iptables about their port map.
#
# The cube-owned mark uses the CUBE_L7_MARK_MASK (0xFFFF0000) high-16
# range so the low 16 bits remain free for host-level marks users may
# set for other purposes.
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
# skb->mark values written by the mvmtap L7 proxy path (the cube_l7_mark_http /
# cube_l7_mark_https / cube_l7_mark_mask globals in CubeNet/src/cubevs.h). Only
# the high 16 bits are cube-owned; the mask lets users co-exist with other
# host-level fwmark schemes on the low 16 bits.
#
# Defaults match the shipped values; a deployment may override them via
# /etc/cubeegress/l7-marks.conf so these rules and the dataplane (which reads
# the same file into its eBPF globals) stay in lock-step.
CUBE_L7_MARK_HTTP=0xCE010000
CUBE_L7_MARK_HTTPS=0xCE020000
CUBE_L7_MARK_MASK=0xFFFF0000
if [ -f /etc/cubeegress/l7-marks.conf ]; then
    # shellcheck disable=SC1091
    . /etc/cubeegress/l7-marks.conf
fi
# Validate the (possibly overridden) marks: http must differ from https, and
# both may only set bits inside the mask. Compare arithmetically (not as
# strings) so the same value in different notations — 0xCE010000 vs
# 0xce010000 vs 3456172032 — is still rejected, matching the uint32
# comparison in cubevs.resolveL7Marks.
validate_l7_marks() {
    if (( CUBE_L7_MARK_HTTP == CUBE_L7_MARK_HTTPS )); then
        echo "cube-proxy-iptables-init: CUBE_L7_MARK_HTTP (${CUBE_L7_MARK_HTTP}) must differ from CUBE_L7_MARK_HTTPS" >&2
        exit 1
    fi
    if [ $(( CUBE_L7_MARK_HTTP & ~CUBE_L7_MARK_MASK )) -ne 0 ] || \
       [ $(( CUBE_L7_MARK_HTTPS & ~CUBE_L7_MARK_MASK )) -ne 0 ]; then
        echo "cube-proxy-iptables-init: L7 marks must set bits only within CUBE_L7_MARK_MASK (${CUBE_L7_MARK_MASK})" >&2
        exit 1
    fi
}
validate_l7_marks
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

# Build the steering rules into a scratch chain and only swap it into the live
# chain once every rule is verified present. Flushing and rebuilding the live
# chain in place would be non-atomic: if the second (HTTPS) rule failed after
# the first succeeded, the script would abort before install_routing() while
# PREROUTING still jumped at a half-built chain — a silent fail-open that
# bypasses L7 interception/deny for that scheme. Building in an unreferenced
# scratch chain keeps the currently-live config intact until the swap.
install_chain() {
    local scratch="${CHAIN}.new"

    # Start from a clean scratch chain. It is not referenced by PREROUTING yet,
    # so a failure below never disturbs the currently-live chain.
    iptables -t mangle -F "${scratch}" 2>/dev/null || true
    iptables -t mangle -X "${scratch}" 2>/dev/null || true
    iptables -t mangle -N "${scratch}"

    # HTTP: mvmtap stamps CUBE_L7_MARK_HTTP → steer to nginx HTTP listener.
    iptables -t mangle -A "${scratch}" \
        -i "${INGRESS_IFACE}" -p tcp \
        -m mark --mark "${CUBE_L7_MARK_HTTP}/${CUBE_L7_MARK_MASK}" \
        -j TPROXY --on-ip "${TPROXY_ON_IP}" --on-port "${TPROXY_PORT_HTTP}"

    # HTTPS: mvmtap stamps CUBE_L7_MARK_HTTPS → steer to nginx HTTPS listener.
    iptables -t mangle -A "${scratch}" \
        -i "${INGRESS_IFACE}" -p tcp \
        -m mark --mark "${CUBE_L7_MARK_HTTPS}/${CUBE_L7_MARK_MASK}" \
        -j TPROXY --on-ip "${TPROXY_ON_IP}" --on-port "${TPROXY_PORT_HTTPS}"

    iptables -t mangle -A "${scratch}" -j RETURN

    # Post-condition: both steering rules must exist before the swap, else a
    # silently dropped rule would fail open for that scheme.
    iptables -t mangle -C "${scratch}" \
        -i "${INGRESS_IFACE}" -p tcp \
        -m mark --mark "${CUBE_L7_MARK_HTTP}/${CUBE_L7_MARK_MASK}" \
        -j TPROXY --on-ip "${TPROXY_ON_IP}" --on-port "${TPROXY_PORT_HTTP}" \
        || fatal "HTTP TPROXY rule missing from ${scratch} after build"
    iptables -t mangle -C "${scratch}" \
        -i "${INGRESS_IFACE}" -p tcp \
        -m mark --mark "${CUBE_L7_MARK_HTTPS}/${CUBE_L7_MARK_MASK}" \
        -j TPROXY --on-ip "${TPROXY_ON_IP}" --on-port "${TPROXY_PORT_HTTPS}" \
        || fatal "HTTPS TPROXY rule missing from ${scratch} after build"

    # Swap: detach the previously-live chain from PREROUTING, drop it, rename
    # the fully-built scratch chain into the live name, then point PREROUTING
    # at it. The old config keeps serving until it is detached here, so there
    # is no window with a half-configured chain.
    while iptables -t mangle -C PREROUTING -j "${CHAIN}" 2>/dev/null; do
        iptables -t mangle -D PREROUTING -j "${CHAIN}" || break
    done
    iptables -t mangle -F "${CHAIN}" 2>/dev/null || true
    iptables -t mangle -X "${CHAIN}" 2>/dev/null || true
    iptables -t mangle -E "${scratch}" "${CHAIN}"
    iptables -t mangle -C PREROUTING -j "${CHAIN}" 2>/dev/null \
        || iptables -t mangle -A PREROUTING -j "${CHAIN}"
}

install_routing() {
    # Two ip rules: cube-owned mark bits → table 100. Match on fwmark so the
    # rule set stays independent of the original destination port; user rules
    # may attach L7 handling to arbitrary ports (e.g. tcp/3000) and mvmtap
    # writes the same mark for all of them.
    local mark
    for mark in "${CUBE_L7_MARK_HTTP}" "${CUBE_L7_MARK_HTTPS}"; do
        if ! ip rule show \
             | grep -qiE "fwmark ${mark}/${CUBE_L7_MARK_MASK} lookup ${ROUTE_TABLE}[[:space:]]*$"; then
            ip rule add fwmark "${mark}/${CUBE_L7_MARK_MASK}" \
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
    # Drop any scratch chain left over from an aborted install.
    iptables -t mangle -F "${CHAIN}.new" 2>/dev/null || true
    iptables -t mangle -X "${CHAIN}.new" 2>/dev/null || true
}

remove_routing() {
    local mark
    for mark in "${CUBE_L7_MARK_HTTP}" "${CUBE_L7_MARK_HTTPS}"; do
        while ip rule show \
              | grep -qiE "fwmark ${mark}/${CUBE_L7_MARK_MASK} lookup ${ROUTE_TABLE}[[:space:]]*$"; do
            ip rule del fwmark "${mark}/${CUBE_L7_MARK_MASK}" \
                       table "${ROUTE_TABLE}" || break
        done
    done
    ip route flush table "${ROUTE_TABLE}" 2>/dev/null || true
}

# Remove policy-routing selectors installed by the pre-fwmark implementation.
# Delete only the exact cube-dev TCP/80 and TCP/443 selectors; unrelated rules
# using table 100 are outside this script's ownership.
remove_legacy_dport_routing() {
    local port
    for port in 80 443; do
        while ip rule del iif "${INGRESS_IFACE}" ipproto tcp dport "${port}" \
                      table "${ROUTE_TABLE}" 2>/dev/null; do
            :
        done
    done
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
            remove_legacy_dport_routing
            log "cube-proxy iptables/route rules installed"
            show_status
            ;;
        down)
            require_root
            remove_chain
            remove_routing
            remove_legacy_dport_routing
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

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
