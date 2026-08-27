#!/bin/sh
set -eu

ROUTE_TABLE=100
RULE_PRIORITY=10900
ROUTE_MARK=0x1000/0x1000
CLUSTER_CIDR_SET=CUBE-EGRESS-CLUSTER-CIDRS
CLUSTER_CIDR_SET_NEXT=CUBE-EGRESS-CLUSTER-CIDRS-NEXT
SNAT_CHAIN=CUBE-EGRESS-SNAT
RAW_CLUSTER_CIDRS="${CUBE_EGRESS_CLUSTER_CIDRS:?CUBE_EGRESS_CLUSTER_CIDRS is required}"
NODE_NAME="${NODE_NAME:?NODE_NAME is required}"
CUBE_ROUTER_NAT_IP="${CUBE_ROUTER_NAT_IP:?CUBE_ROUTER_NAT_IP is required}"
STATE_DIR="${CUBE_EGRESS_STATE_DIR:-/run/cube-egress}"
RP_FILTER_PATH="${CUBE_EGRESS_RP_FILTER_PATH:-/host-sysctl/rp_filter}"
RP_FILTER_STATE="${STATE_DIR}/rp-filter.previous"

log() {
  printf '[egress-configurer] %s\n' "$*"
}

canonical_cidr() (
  cidr="$1"
  address="${cidr%/*}"
  prefix="${cidr#*/}"
  old_ifs="${IFS}"
  IFS=.
  set -- ${address}
  IFS="${old_ifs}"
  [ "$#" -eq 4 ] || return 1

  remaining="${prefix}"
  result=""
  for octet in "$@"; do
    if [ "${remaining}" -ge 8 ]; then
      network_octet="${octet}"
      remaining=$((remaining - 8))
    elif [ "${remaining}" -le 0 ]; then
      network_octet=0
    else
      case "${remaining}" in
        1) mask=128 ;;
        2) mask=192 ;;
        3) mask=224 ;;
        4) mask=240 ;;
        5) mask=248 ;;
        6) mask=252 ;;
        7) mask=254 ;;
        *) return 1 ;;
      esac
      network_octet=$((octet & mask))
      remaining=0
    fi
    if [ -z "${result}" ]; then
      result="${network_octet}"
    else
      result="${result}.${network_octet}"
    fi
  done
  printf '%s/%s\n' "${result}" "${prefix}"
)

normalize_cluster_cidrs() {
  normalized=""
  old_ifs="${IFS}"
  IFS=,
  # shellcheck disable=SC2086 # Comma splitting is intentional and validated below.
  set -- ${RAW_CLUSTER_CIDRS}
  IFS="${old_ifs}"
  for cidr in "$@"; do
    canonical="$(canonical_cidr "${cidr}")" || return 1
    case ",${normalized}," in
      *,"${canonical}",*) continue ;;
    esac
    if [ -z "${normalized}" ]; then
      normalized="${canonical}"
    else
      normalized="${normalized},${canonical}"
    fi
  done
  printf '%s\n' "${normalized}"
}

CLUSTER_CIDRS="$(normalize_cluster_cidrs)"
[ -n "${CLUSTER_CIDRS}" ] || {
  log "no valid cluster CIDRs"
  exit 1
}

cluster_cidrs() {
  printf '%s\n' "${CLUSTER_CIDRS}" | tr ',' '\n'
}

load_state_snapshots() {
  MANGLE_RULES="$(iptables -w -t mangle -S)"
  FILTER_RULES="$(iptables -w -t filter -S)"
  NAT_RULES="$(iptables -w -t nat -S)"
  POLICY_RULES="$(ip -4 rule show)"
  # A fresh node has no policy-routing table yet. iproute2 reports that as a
  # non-zero exit, but it is an empty initial state that reconcile must create.
  ROUTE_STATE="$(ip route show table "${ROUTE_TABLE}" 2>/dev/null || true)"
  IPSET_STATE="$(ipset save "${CLUSTER_CIDR_SET}" 2>/dev/null || true)"
}

peer_gateway() {
  case "$1" in
    cube-egress-p0) printf '169.254.240.2\n' ;;
    cube-egress-p1) printf '169.254.240.6\n' ;;
    *) return 1 ;;
  esac
}

peer_is_healthy() {
  iface="$1"
  gateway="$(peer_gateway "${iface}")" || return 1
  ip link show "${iface}" >/dev/null 2>&1 &&
    response="$(
      printf 'health\n' |
        nc -w 2 -s "$(ip -4 -o address show dev "${iface}" |
          awk 'NR == 1 { sub(/\/.*/, "", $4); print $4 }')" \
          "${gateway}" 19091 2>/dev/null || true
    )" &&
    [ "${response}" = "ok" ]
}

ensure_route_mark_ownership() {
  foreign="$(
    printf '%s\n' "${MANGLE_RULES}" |
      grep -F -- "MARK --set-xmark ${ROUTE_MARK}" |
      grep -v '^-A CUBE-EGRESS-MARK ' || true
  )"
  if [ -n "${foreign}" ]; then
    log "route mark ${ROUTE_MARK} is occupied: ${foreign}"
    return 1
  fi
}

expected_mark_chain() {
  printf -- '-N CUBE-EGRESS-MARK\n'
  printf -- '-A CUBE-EGRESS-MARK -m set --match-set %s dst -j MARK --set-xmark %s\n' \
    "${CLUSTER_CIDR_SET}" "${ROUTE_MARK}"
}

expected_mark_jump() {
  printf -- '-A PREROUTING -s %s/32 -i cube-router -j CUBE-EGRESS-MARK\n' \
    "${CUBE_ROUTER_NAT_IP}"
}

expected_forward_chain() {
  cat <<'EOF'
-N CUBE-EGRESS
-A CUBE-EGRESS -o cube-egress-p0 -j ACCEPT
-A CUBE-EGRESS -o cube-egress-p1 -j ACCEPT
-A CUBE-EGRESS -j DROP
EOF
}

expected_forward_rules() {
  printf -- '-A FORWARD -s %s/32 -i cube-router -m mark --mark %s -j CUBE-EGRESS\n' \
    "${CUBE_ROUTER_NAT_IP}" "${ROUTE_MARK}"
}

expected_snat_chain() {
  printf -- '-N %s\n' "${SNAT_CHAIN}"
  printf -- '-A %s -o cube-egress-p0 -j SNAT --to-source %s\n' \
    "${SNAT_CHAIN}" "${CUBE_ROUTER_NAT_IP}"
  printf -- '-A %s -o cube-egress-p1 -j SNAT --to-source %s\n' \
    "${SNAT_CHAIN}" "${CUBE_ROUTER_NAT_IP}"
}

expected_snat_jump() {
  printf -- '-A POSTROUTING -s %s/32 -m mark --mark %s -j %s\n' \
    "${CUBE_ROUTER_NAT_IP}" "${ROUTE_MARK}" "${SNAT_CHAIN}"
}

preflight_iptables_ownership() {
  expected="$(expected_mark_chain)"
  current="$(
    printf '%s\n' "${MANGLE_RULES}" |
      awk '$1 == "-N" && $2 == "CUBE-EGRESS-MARK" ||
           $1 == "-A" && $2 == "CUBE-EGRESS-MARK"'
  )"
  if [ -n "${current}" ]; then
    [ "${current}" = "${expected}" ] ||
      [ "${current}" = "-N CUBE-EGRESS-MARK" ] || {
        log "iptables chain CUBE-EGRESS-MARK is occupied"
        return 1
      }
  fi

  expected="$(expected_mark_jump)"
  current="$(
    printf '%s\n' "${MANGLE_RULES}" |
      sed -n '/^-A .* -j CUBE-EGRESS-MARK$/p'
  )"
  [ -z "${current}" ] || [ "${current}" = "${expected}" ] || {
    log "iptables jump to CUBE-EGRESS-MARK is occupied: ${current}"
    return 1
  }

  expected="$(expected_forward_chain)"
  current="$(
    printf '%s\n' "${FILTER_RULES}" |
      awk '$1 == "-N" && $2 == "CUBE-EGRESS" ||
           $1 == "-A" && $2 == "CUBE-EGRESS"'
  )"
  if [ -n "${current}" ]; then
    [ "${current}" = "${expected}" ] ||
      [ "${current}" = "-N CUBE-EGRESS" ] || {
        log "iptables chain CUBE-EGRESS is occupied"
        return 1
      }
  fi

  expected="$(expected_forward_rules)"
  current="$(
    printf '%s\n' "${FILTER_RULES}" |
      awk -v nat="${CUBE_ROUTER_NAT_IP}/32" '
        $1 == "-A" && $2 == "FORWARD" && $NF == "CUBE-EGRESS" { print }
      '
  )"
  [ -z "${current}" ] || [ "${current}" = "${expected}" ] || {
    log "EgressProxy FORWARD rules are occupied: ${current}"
    return 1
  }

  expected="$(expected_snat_chain)"
  current="$(
    printf '%s\n' "${NAT_RULES}" |
      awk -v chain="${SNAT_CHAIN}" \
        '$1 == "-N" && $2 == chain || $1 == "-A" && $2 == chain'
  )"
  if [ -n "${current}" ]; then
    [ "${current}" = "${expected}" ] ||
      [ "${current}" = "-N ${SNAT_CHAIN}" ] || {
        log "iptables chain ${SNAT_CHAIN} is occupied"
        return 1
      }
  fi

  expected="$(expected_snat_jump)"
  current="$(
    printf '%s\n' "${NAT_RULES}" |
      sed -n "/^-A .* -j ${SNAT_CHAIN}$/p"
  )"
  [ -z "${current}" ] || [ "${current}" = "${expected}" ] || {
    log "EgressProxy SNAT jump is occupied: ${current}"
    return 1
  }
}

preflight_ipset_ownership() {
  [ -z "${IPSET_STATE}" ] && return 0
  first_line="$(printf '%s\n' "${IPSET_STATE}" | sed -n '1p')"
  case "${first_line}" in
    "create ${CLUSTER_CIDR_SET} hash:net "* | \
    "create ${CLUSTER_CIDR_SET} hash:net")
      ;;
    *)
      log "ipset ${CLUSTER_CIDR_SET} is occupied: ${first_line}"
      return 1
      ;;
  esac
}

expected_ipset_members() {
  while IFS= read -r cidr; do
    printf 'add %s %s\n' "${CLUSTER_CIDR_SET}" "${cidr}"
  done <<EOF
$(cluster_cidrs)
EOF
}

ensure_cluster_cidr_set() {
  current_members="$(
    printf '%s\n' "${IPSET_STATE}" |
      awk -v name="${CLUSTER_CIDR_SET}" '$1 == "add" && $2 == name' |
      sort
  )"
  expected_members="$(expected_ipset_members | sort)"
  [ "${current_members}" = "${expected_members}" ] && return 0

  ipset create "${CLUSTER_CIDR_SET}" hash:net family inet -exist
  ipset create "${CLUSTER_CIDR_SET_NEXT}" hash:net family inet -exist
  ipset flush "${CLUSTER_CIDR_SET_NEXT}"
  while IFS= read -r cidr; do
    ipset add "${CLUSTER_CIDR_SET_NEXT}" "${cidr}"
  done <<EOF
$(cluster_cidrs)
EOF
  ipset swap "${CLUSTER_CIDR_SET_NEXT}" "${CLUSTER_CIDR_SET}"
  ipset destroy "${CLUSTER_CIDR_SET_NEXT}"
}

ensure_mark_chain() {
  iptables -w -t mangle -N CUBE-EGRESS-MARK 2>/dev/null || true

  expected_chain="$(expected_mark_chain)"
  current_chain="$(
    printf '%s\n' "${MANGLE_RULES}" |
      awk '$1 == "-N" && $2 == "CUBE-EGRESS-MARK" ||
           $1 == "-A" && $2 == "CUBE-EGRESS-MARK"'
  )"
  if [ "${current_chain}" != "${expected_chain}" ]; then
    {
      printf '*mangle\n'
      printf -- '-F CUBE-EGRESS-MARK\n'
      printf '%s\n' "${expected_chain}" | sed -n '/^-A /p'
      printf 'COMMIT\n'
    } | iptables-restore --noflush --wait
  fi

  expected_jump="$(expected_mark_jump)"
  current_jumps="$(
    printf '%s\n' "${MANGLE_RULES}" |
      sed -n '/^-A .* -j CUBE-EGRESS-MARK$/p'
  )"
  first_rule="$(
    printf '%s\n' "${MANGLE_RULES}" |
      sed -n '/^-A /{p;q;}'
  )"
  if [ "${current_jumps}" != "${expected_jump}" ] ||
     [ "${first_rule}" != "${expected_jump}" ]; then
    {
      printf '*mangle\n'
      printf '%s\n' "${current_jumps}" | sed 's/^-A /-D /'
      printf -- '-I PREROUTING 1 -s %s/32 -i cube-router -j CUBE-EGRESS-MARK\n' \
        "${CUBE_ROUTER_NAT_IP}"
      printf 'COMMIT\n'
    } | iptables-restore --noflush --wait
  fi
}

ensure_forward_chain() {
  iptables -w -t filter -N CUBE-EGRESS 2>/dev/null || true

  expected_chain="$(expected_forward_chain)"
  current_chain="$(
    printf '%s\n' "${FILTER_RULES}" |
      awk '$1 == "-N" && $2 == "CUBE-EGRESS" ||
           $1 == "-A" && $2 == "CUBE-EGRESS"'
  )"
  if [ "${current_chain}" != "${expected_chain}" ]; then
    {
      printf '*filter\n'
      printf -- '-F CUBE-EGRESS\n'
      printf '%s\n' "${expected_chain}" | sed -n '/^-A /p'
      printf 'COMMIT\n'
    } | iptables-restore --noflush --wait
  fi

  expected_rules="$(expected_forward_rules)"
  current_rules="$(
    printf '%s\n' "${FILTER_RULES}" |
      awk '$1 == "-A" && $2 == "FORWARD" && $NF == "CUBE-EGRESS" { print }'
  )"
  leading_rules="$(
    printf '%s\n' "${FILTER_RULES}" |
      sed -n '/^-A /p' |
      sed -n '1p'
  )"
  if [ "${current_rules}" != "${expected_rules}" ] ||
     [ "${leading_rules}" != "${expected_rules}" ]; then
    {
      printf '*filter\n'
      printf '%s\n' "${current_rules}" | sed 's/^-A /-D /'
      printf '%s\n' "${expected_rules}" |
        awk '{ line[NR] = $0 } END { for (i = NR; i >= 1; i--) print line[i] }' |
        sed 's/^-A FORWARD /-I FORWARD 1 /'
      printf 'COMMIT\n'
    } | iptables-restore --noflush --wait
  fi
}

ensure_snat_chain() {
  iptables -w -t nat -N "${SNAT_CHAIN}" 2>/dev/null || true

  expected_chain="$(expected_snat_chain)"
  current_chain="$(
    printf '%s\n' "${NAT_RULES}" |
      awk -v chain="${SNAT_CHAIN}" \
        '$1 == "-N" && $2 == chain || $1 == "-A" && $2 == chain'
  )"
  if [ "${current_chain}" != "${expected_chain}" ]; then
    {
      printf '*nat\n'
      printf -- '-F %s\n' "${SNAT_CHAIN}"
      printf '%s\n' "${expected_chain}" | sed -n '/^-A /p'
      printf 'COMMIT\n'
    } | iptables-restore --noflush --wait
  fi

  expected_jump="$(expected_snat_jump)"
  current_jumps="$(
    printf '%s\n' "${NAT_RULES}" |
      sed -n "/^-A .* -j ${SNAT_CHAIN}$/p"
  )"
  first_rule="$(
    printf '%s\n' "${NAT_RULES}" |
      awk '$1 == "-A" && $2 == "POSTROUTING" { print; exit }'
  )"
  if [ "${current_jumps}" != "${expected_jump}" ] ||
     [ "${first_rule}" != "${expected_jump}" ]; then
    {
      printf '*nat\n'
      printf '%s\n' "${current_jumps}" | sed 's/^-A /-D /'
      printf -- '-I POSTROUTING 1 -s %s/32 -m mark --mark %s -j %s\n' \
        "${CUBE_ROUTER_NAT_IP}" "${ROUTE_MARK}" "${SNAT_CHAIN}"
      printf 'COMMIT\n'
    } | iptables-restore --noflush --wait
  fi
}

ensure_rp_filter() {
  current="$(cat "${RP_FILTER_PATH}")"
  case "${current}" in
    0 | 1 | 2) ;;
    *) log "invalid rp_filter value: ${current}"; return 1 ;;
  esac
  if [ ! -f "${RP_FILTER_STATE}" ]; then
    printf '%s\n' "${current}" > "${RP_FILTER_STATE}.tmp"
    mv "${RP_FILTER_STATE}.tmp" "${RP_FILTER_STATE}"
  fi
  [ "${current}" = 0 ] || printf '0\n' > "${RP_FILTER_PATH}"
}

expected_policy_rule() {
  printf '%s:\tfrom %s fwmark %s iif cube-router lookup %s\n' \
    "${RULE_PRIORITY}" "${CUBE_ROUTER_NAT_IP}" "${ROUTE_MARK}" "${ROUTE_TABLE}"
}

preflight_policy_rule_ownership() {
  expected="$(expected_policy_rule)"
  current="$(
    printf '%s\n' "${POLICY_RULES}" |
      awk -v priority="${RULE_PRIORITY}:" '$1 == priority { print; exit }'
  )"
  if [ -n "${current}" ] && [ "${current}" != "${expected}" ]; then
    log "priority ${RULE_PRIORITY} is occupied: ${current}"
    return 1
  fi
}

ensure_policy_rule() {
  current="$(
    printf '%s\n' "${POLICY_RULES}" |
      awk -v priority="${RULE_PRIORITY}:" '$1 == priority { print; exit }'
  )"
  [ "${current}" = "$(expected_policy_rule)" ] && return 0
  ip -4 rule add priority "${RULE_PRIORITY}" \
    iif cube-router from "${CUBE_ROUTER_NAT_IP}/32" \
    fwmark "${ROUTE_MARK}" lookup "${ROUTE_TABLE}"
}

ensure_unreachable_route() {
  printf '%s\n' "${ROUTE_STATE}" |
    grep -Fx 'unreachable default metric 32767' >/dev/null && return 0
  ip route replace table "${ROUTE_TABLE}" \
    unreachable default metric 32767
}

ensure_route_table_ownership() {
  while IFS= read -r route; do
    [ -n "${route}" ] || continue
    route="$(printf '%s\n' "${route}" | sed 's/[[:space:]]*$//')"
    case "${route}" in
      "unreachable default metric 32767" | \
      "default via 169.254.240.2 dev cube-egress-p0 metric 100" | \
      "default via 169.254.240.6 dev cube-egress-p1 metric 100")
        ;;
      *)
        log "route table ${ROUTE_TABLE} is occupied: ${route}"
        return 1
        ;;
    esac
  done <<EOF
${ROUTE_STATE}
EOF
}

current_active_iface() {
  printf '%s\n' "${ROUTE_STATE}" |
    awk '
      $1 == "default" {
        for (i = 2; i <= NF; i++) {
          if ($i == "dev") dev = $(i + 1)
          if ($i == "metric" && $(i + 1) == "100") metric = 1
        }
        if (metric) { print dev; exit }
      }
    '
}

replace_active_route() {
  iface="$1"
  gateway="$(peer_gateway "${iface}")"
  if printf '%s\n' "${ROUTE_STATE}" |
    grep -F "default via ${gateway} dev ${iface}" |
    grep -F "metric 100" >/dev/null; then
    return 0
  fi
  ip route replace table "${ROUTE_TABLE}" \
    default via "${gateway}" dev "${iface}" metric 100
}

remove_active_route() {
  printf '%s\n' "${ROUTE_STATE}" |
    awk '$1 == "default" {
      for (i = 2; i <= NF; i++) {
        if ($i == "metric" && $(i + 1) == "100") found = 1
      }
    } END { exit !found }' || return 0
  ip route del table "${ROUTE_TABLE}" default metric 100 2>/dev/null || true
}

ensure_policy_route() {
  ensure_route_table_ownership
  ensure_unreachable_route
  current_iface="$(current_active_iface)"
  active_iface=""
  if peer_is_healthy "${current_iface}"; then
    active_iface="${current_iface}"
  elif peer_is_healthy cube-egress-p0; then
    active_iface=cube-egress-p0
  elif peer_is_healthy cube-egress-p1; then
    active_iface=cube-egress-p1
  fi

  if [ -z "${active_iface}" ]; then
    remove_active_route
    log "no healthy EgressProxy; cluster CIDRs are fail-closed"
    return 1
  fi

  replace_active_route "${active_iface}"
  if [ "${active_iface}" != "${current_iface}" ]; then
    log "active peer changed: ${current_iface:-none} -> ${active_iface}"
  fi
}

if [ "${CUBE_EGRESS_RECONCILE_LIBRARY_ONLY:-0}" = "1" ]; then
  return 0 2>/dev/null || exit 0
fi

mkdir -p "${STATE_DIR}"
ip link show cube-router >/dev/null
load_state_snapshots
ensure_route_mark_ownership
preflight_iptables_ownership
preflight_ipset_ownership
preflight_policy_rule_ownership
ensure_route_table_ownership
ensure_cluster_cidr_set
ensure_mark_chain
ensure_forward_chain
ensure_snat_chain
ensure_rp_filter
ensure_policy_rule
route_failed=0
ensure_policy_route || route_failed=1
[ "${route_failed}" -eq 0 ] || exit 1
log "ready: node=${NODE_NAME} nat=${CUBE_ROUTER_NAT_IP} table=${ROUTE_TABLE}"
