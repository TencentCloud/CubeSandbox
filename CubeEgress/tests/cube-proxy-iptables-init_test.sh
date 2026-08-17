#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../scripts/cube-proxy-iptables-init.sh
source "${ROOT_DIR}/scripts/cube-proxy-iptables-init.sh"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT
CALLS="${TMP_DIR}/calls"

# Each legacy selector exists twice. The third deletion fails and terminates
# the loop. Record every attempted exact command.
declare -A DELETE_COUNT=([80]=0 [443]=0)
ip() {
    printf '%s\n' "$*" >> "${CALLS}"
    if [[ "$1 $2" == "rule del" ]]; then
        local port=""
        local i
        for ((i = 1; i <= $#; i++)); do
            if [[ "${!i}" == "dport" ]]; then
                local next=$((i + 1))
                port="${!next}"
                break
            fi
        done
        [[ -n "${port}" ]] || return 1
        DELETE_COUNT["${port}"]=$((DELETE_COUNT["${port}"] + 1))
        (( DELETE_COUNT["${port}"] <= 2 ))
        return
    fi
    return 1
}

remove_legacy_dport_routing
[[ "${DELETE_COUNT[80]}" -eq 3 ]]
[[ "${DELETE_COUNT[443]}" -eq 3 ]]
grep -Fxq "rule del iif cube-dev ipproto tcp dport 80 table 100" "${CALLS}"
grep -Fxq "rule del iif cube-dev ipproto tcp dport 443 table 100" "${CALLS}"
if grep -Ev '^rule del iif cube-dev ipproto tcp dport (80|443) table 100$' "${CALLS}"; then
    echo "unexpected legacy cleanup command" >&2
    exit 1
fi

# --- fwmark ip-rule idempotency is case-insensitive --------------------------
# iproute2 prints fwmark hex in lowercase (0xce010000/0xffff0000), but the
# script greps for the uppercase CUBE_L7_MARK_* constants. The grep must be
# case-insensitive, or install_routing re-adds duplicate rules on every run and
# remove_routing never matches anything to delete.
RULES_STATE="${TMP_DIR}/rules"
: > "${RULES_STATE}"
: > "${CALLS}"
ip() {
    printf '%s\n' "$*" >> "${CALLS}"
    case "$1 $2" in
        "rule show")
            cat "${RULES_STATE}"
            return 0
            ;;
        "rule add")
            # $3=fwmark $4=mark/mask $5=table $6=<id>; store iproute2-style
            # lowercase show output, as the real `ip rule show` would print.
            printf 'from all fwmark %s lookup %s\n' "$4" "$6" \
                | tr '[:upper:]' '[:lower:]' >> "${RULES_STATE}"
            return 0
            ;;
        "rule del")
            local needle
            needle="$(printf '%s' "$4" | tr '[:upper:]' '[:lower:]')"
            grep -viF "fwmark ${needle} " "${RULES_STATE}" > "${RULES_STATE}.tmp" || true
            mv "${RULES_STATE}.tmp" "${RULES_STATE}"
            return 0
            ;;
        "route "*)
            return 0
            ;;
    esac
    return 0
}

# Re-running install must not duplicate rules: the grep matches the lowercase
# `ip rule show` output, so the second run is a no-op.
install_routing
install_routing
[[ "$(grep -c 'rule add fwmark' "${CALLS}" || true)" -eq 2 ]]
[[ "$(wc -l < "${RULES_STATE}")" -eq 2 ]]

# remove must match (and so delete) every installed rule.
remove_routing
[[ "$(grep -c 'rule del fwmark' "${CALLS}" || true)" -eq 2 ]]
[[ ! -s "${RULES_STATE}" ]]

# --- install_chain builds into a scratch chain and swaps atomically ----------
# A steering-rule failure must abort BEFORE the live chain / PREROUTING jump is
# touched, so a partial install never leaves a fail-open gap. We mock iptables
# and record every call; chain contents live in IPT_STATE, the PREROUTING jump
# in JUMP_STATE.
: > "${CALLS}"
IPT_STATE="${TMP_DIR}/iptables_rules"
JUMP_STATE="${TMP_DIR}/prerouting_jump"
: > "${IPT_STATE}"
: > "${JUMP_STATE}"
FAIL_ON_MARK=""

iptables() {
    printf '%s\n' "$*" >> "${CALLS}"
    local op="$3" target="$4"
    local rest="${*:5}"
    case "${op}" in
        -N|-F|-X) return 0 ;;
        -E) # rename chain $4 -> $5, carrying its rules
            local from="$4" to="$5"
            { grep -vF "${from}|" "${IPT_STATE}" || true
              grep -F "${from}|" "${IPT_STATE}" | sed "s|^${from}|${to}|" || true
            } > "${IPT_STATE}.new"
            mv "${IPT_STATE}.new" "${IPT_STATE}"
            return 0 ;;
        -A)
            if [[ "${target}" == "PREROUTING" ]]; then
                printf '%s\n' "$6" > "${JUMP_STATE}"
                return 0
            fi
            if [[ -n "${FAIL_ON_MARK}" && "${rest}" == *"${FAIL_ON_MARK}"* ]]; then
                return 1
            fi
            printf '%s|%s\n' "${target}" "${rest}" >> "${IPT_STATE}"
            return 0 ;;
        -C)
            if [[ "${target}" == "PREROUTING" ]]; then
                [[ "$(cat "${JUMP_STATE}")" == "$6" ]] && return 0 || return 1
            fi
            grep -qF "${target}|${rest}" "${IPT_STATE}" && return 0 || return 1 ;;
        -D)
            if [[ "${target}" == "PREROUTING" ]]; then : > "${JUMP_STATE}"; fi
            return 0 ;;
    esac
    return 0
}

# Success path: both steering rules are verified in the scratch chain BEFORE it
# is renamed into the live chain, and PREROUTING only jumps at the live chain
# after the swap.
install_chain
http_v="$(grep -n -- "-t mangle -C ${CHAIN}.new .*${CUBE_L7_MARK_HTTP}" "${CALLS}" | head -1 | cut -d: -f1)"
https_v="$(grep -n -- "-t mangle -C ${CHAIN}.new .*${CUBE_L7_MARK_HTTPS}" "${CALLS}" | head -1 | cut -d: -f1)"
swap="$(grep -n -- "-t mangle -E ${CHAIN}.new ${CHAIN}" "${CALLS}" | head -1 | cut -d: -f1)"
jump="$(grep -n -- "-t mangle -A PREROUTING -j ${CHAIN}" "${CALLS}" | head -1 | cut -d: -f1)"
[[ -n "${http_v}" && -n "${https_v}" && -n "${swap}" && -n "${jump}" ]]
[[ "${http_v}" -lt "${swap}" && "${https_v}" -lt "${swap}" && "${swap}" -lt "${jump}" ]]
# Post-swap, the live chain holds both steering rules.
grep -qF "${CHAIN}|-i ${INGRESS_IFACE} -p tcp -m mark --mark ${CUBE_L7_MARK_HTTP}/${CUBE_L7_MARK_MASK} -j TPROXY --on-ip ${TPROXY_ON_IP} --on-port ${TPROXY_PORT_HTTP}" "${IPT_STATE}"
grep -qF "${CHAIN}|-i ${INGRESS_IFACE} -p tcp -m mark --mark ${CUBE_L7_MARK_HTTPS}/${CUBE_L7_MARK_MASK} -j TPROXY --on-ip ${TPROXY_ON_IP} --on-port ${TPROXY_PORT_HTTPS}" "${IPT_STATE}"

# Failure path: the HTTPS rule fails to add. The subshell aborts before the
# swap and before any PREROUTING jump, so the live path is never half-built.
: > "${CALLS}"; : > "${IPT_STATE}"; : > "${JUMP_STATE}"
if ( FAIL_ON_MARK="${CUBE_L7_MARK_HTTPS}"; install_chain ); then
    echo "install_chain succeeded despite HTTPS rule failure" >&2
    exit 1
fi
if grep -q -- "-t mangle -E ${CHAIN}.new ${CHAIN}" "${CALLS}"; then
    echo "live chain swapped in despite failed HTTPS rule (fail-open)" >&2
    exit 1
fi
if grep -q -- "-t mangle -A PREROUTING -j ${CHAIN}" "${CALLS}"; then
    echo "PREROUTING jump installed despite failed HTTPS rule (fail-open)" >&2
    exit 1
fi
unset -f iptables

# Verify migration ordering without touching the host network.
: > "${CALLS}"
require_root() { :; }
require_iface() { :; }
require_modules() { :; }
install_chain() { echo install_chain >> "${CALLS}"; }
install_routing() { echo install_routing >> "${CALLS}"; }
remove_legacy_dport_routing() { echo remove_legacy >> "${CALLS}"; }
show_status() { echo show_status >> "${CALLS}"; }
main up
EXPECTED=$'install_chain\ninstall_routing\nremove_legacy\nshow_status'
[[ "$(<"${CALLS}")" == "${EXPECTED}" ]]

# --- marks validation compares arithmetically, not as strings ----------------
# Go resolveL7Marks compares uint32 values; the shell check must reject the
# same mark written in a different notation (hex case variant or decimal),
# otherwise iptables and the dataplane diverge on identical marks.
# validate_l7_marks exits the (sub)shell on rejection, so a subshell that
# survives means the value was (wrongly) accepted.
if (
    CUBE_L7_MARK_HTTP=0xCE010000
    CUBE_L7_MARK_HTTPS=0xce010000   # same value, different case
    validate_l7_marks
); then
    echo "case-variant identical marks were not rejected" >&2
    exit 1
fi
if (
    CUBE_L7_MARK_HTTP=0xCE010000
    CUBE_L7_MARK_HTTPS=3456172032   # decimal equivalent of 0xCE010000
    validate_l7_marks
); then
    echo "decimal-equivalent identical marks were not rejected" >&2
    exit 1
fi
# distinct values within the mask still pass
(
    CUBE_L7_MARK_HTTP=0xCE010000
    CUBE_L7_MARK_HTTPS=0xCE020000
    CUBE_L7_MARK_MASK=0xFFFF0000
    validate_l7_marks
)
# bits outside the mask are still rejected (0x0000CE00 sets low-16 bits)
if (
    CUBE_L7_MARK_HTTP=0x0000CE00
    CUBE_L7_MARK_HTTPS=0xCE020000
    CUBE_L7_MARK_MASK=0xFFFF0000
    validate_l7_marks
); then
    echo "out-of-mask mark was not rejected" >&2
    exit 1
fi

printf 'cube-proxy-iptables-init_test: PASS\n'
