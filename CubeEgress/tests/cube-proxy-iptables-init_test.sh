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
