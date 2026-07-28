#!/bin/sh
set -eu

ROUTE_TABLE=100
RULE_PRIORITY=10900
ROUTE_MARK=0x1000/0x1000
CLUSTER_CIDR_SET=CUBE-EGRESS-CLUSTER-CIDRS
CLUSTER_CIDR_SET_NEXT=CUBE-EGRESS-CLUSTER-CIDRS-NEXT
SNAT_CHAIN=CUBE-EGRESS-SNAT
CUBE_ROUTER_NAT_IP="${CUBE_ROUTER_NAT_IP:?CUBE_ROUTER_NAT_IP is required}"
STATE_DIR="${CUBE_EGRESS_STATE_DIR:-/run/cube-egress}"
CLEANUP_REQUEST_FILE="${STATE_DIR}/cleanup-requested"
RECONCILE_INTERVAL_SECONDS="${CUBE_EGRESS_RECONCILE_INTERVAL_SECONDS:-10}"
CLEANUP_WAIT_SECONDS="${CUBE_EGRESS_CLEANUP_WAIT_SECONDS:-$((RECONCILE_INTERVAL_SECONDS + 10))}"
RP_FILTER_PATH="${CUBE_EGRESS_RP_FILTER_PATH:-/host-sysctl/rp_filter}"
RP_FILTER_STATE="${STATE_DIR}/rp-filter.previous"

log() {
  printf '[egress-cleanup] %s\n' "$*"
}

mark_chain_is_owned() {
  chain="$1"
  [ "${chain}" = "-N CUBE-EGRESS-MARK" ] && return 0
  [ "${chain}" = "$(cat <<'EOF'
-N CUBE-EGRESS-MARK
-A CUBE-EGRESS-MARK -m set --match-set CUBE-EGRESS-CLUSTER-CIDRS dst -j MARK --set-xmark 0x1000/0x1000
EOF
)" ]
}

preflight_owned_ipset() {
  state="$(ipset save "${CLUSTER_CIDR_SET}" 2>/dev/null || true)"
  [ -z "${state}" ] && return 0
  printf '%s\n' "${state}" |
    awk -v name="${CLUSTER_CIDR_SET}" '
      NR == 1 {
        if ($1 != "create" || $2 != name || $3 != "hash:net") exit 1
        next
      }
      $1 != "add" || $2 != name { exit 1 }
    ' || {
      log "refusing to remove foreign ipset ${CLUSTER_CIDR_SET}"
      return 1
    }
}

cleanup_mark_rules() {
  jump="-A PREROUTING -s ${CUBE_ROUTER_NAT_IP}/32 -i cube-router -j CUBE-EGRESS-MARK"
  jumps="$(
    iptables -w -t mangle -S PREROUTING |
      sed -n '/^-A .* -j CUBE-EGRESS-MARK$/p'
  )"
  [ -z "${jumps}" ] || [ "${jumps}" = "${jump}" ] || {
    log "refusing to remove foreign CUBE-EGRESS-MARK jump: ${jumps}"
    return 1
  }

  if chain="$(iptables -w -t mangle -S CUBE-EGRESS-MARK 2>/dev/null)"; then
    mark_chain_is_owned "${chain}" || {
      log "refusing to remove foreign CUBE-EGRESS-MARK chain"
      return 1
    }
  else
    chain=""
  fi

  while iptables -w -t mangle -C PREROUTING \
    -s "${CUBE_ROUTER_NAT_IP}/32" -i cube-router \
    -j CUBE-EGRESS-MARK 2>/dev/null; do
    iptables -w -t mangle -D PREROUTING \
      -s "${CUBE_ROUTER_NAT_IP}/32" -i cube-router \
      -j CUBE-EGRESS-MARK
  done
  if [ -n "${chain}" ]; then
    iptables -w -t mangle -F CUBE-EGRESS-MARK
    iptables -w -t mangle -X CUBE-EGRESS-MARK
  fi
}

expected_forward_chain() {
  cat <<'EOF'
-N CUBE-EGRESS
-A CUBE-EGRESS -o cube-egress-p0 -j ACCEPT
-A CUBE-EGRESS -o cube-egress-p1 -j ACCEPT
-A CUBE-EGRESS -j DROP
EOF
}

cleanup_forward_rules() {
  if chain="$(iptables -w -t filter -S CUBE-EGRESS 2>/dev/null)"; then
    [ "${chain}" = "$(expected_forward_chain)" ] ||
      [ "${chain}" = "-N CUBE-EGRESS" ] || {
        log "refusing to remove foreign CUBE-EGRESS chain"
        return 1
      }
  else
    chain=""
  fi

  delete_rule() {
    while iptables -w -t filter -C FORWARD "$@" 2>/dev/null; do
      iptables -w -t filter -D FORWARD "$@"
    done
  }
  delete_rule -s "${CUBE_ROUTER_NAT_IP}/32" -i cube-router \
    -m mark --mark "${ROUTE_MARK}" -j CUBE-EGRESS

  if [ -n "${chain}" ]; then
    iptables -w -t filter -F CUBE-EGRESS
    iptables -w -t filter -X CUBE-EGRESS
  fi
}

expected_snat_chain() {
  cat <<EOF
-N ${SNAT_CHAIN}
-A ${SNAT_CHAIN} -o cube-egress-p0 -j SNAT --to-source ${CUBE_ROUTER_NAT_IP}
-A ${SNAT_CHAIN} -o cube-egress-p1 -j SNAT --to-source ${CUBE_ROUTER_NAT_IP}
EOF
}

cleanup_snat_rules() {
  if chain="$(iptables -w -t nat -S "${SNAT_CHAIN}" 2>/dev/null)"; then
    [ "${chain}" = "$(expected_snat_chain)" ] ||
      [ "${chain}" = "-N ${SNAT_CHAIN}" ] || {
        log "refusing to remove foreign ${SNAT_CHAIN} chain"
        return 1
      }
  else
    chain=""
  fi

  while iptables -w -t nat -C POSTROUTING \
    -s "${CUBE_ROUTER_NAT_IP}/32" -m mark --mark "${ROUTE_MARK}" \
    -j "${SNAT_CHAIN}" 2>/dev/null; do
    iptables -w -t nat -D POSTROUTING \
      -s "${CUBE_ROUTER_NAT_IP}/32" -m mark --mark "${ROUTE_MARK}" \
      -j "${SNAT_CHAIN}"
  done
  if [ -n "${chain}" ]; then
    iptables -w -t nat -F "${SNAT_CHAIN}"
    iptables -w -t nat -X "${SNAT_CHAIN}"
  fi
}

restore_rp_filter() {
  [ -f "${RP_FILTER_STATE}" ] || return 0
  previous="$(cat "${RP_FILTER_STATE}")"
  case "${previous}" in
    0 | 1 | 2) ;;
    *) log "invalid saved rp_filter value: ${previous}"; return 1 ;;
  esac
  current="$(cat "${RP_FILTER_PATH}")"
  if [ "${current}" = 0 ]; then
    printf '%s\n' "${previous}" > "${RP_FILTER_PATH}"
  else
    log "rp_filter changed externally to ${current}; leaving it unchanged"
  fi
  rm -f "${RP_FILTER_STATE}"
}

cleanup_policy_routing() {
  expected="${RULE_PRIORITY}:	from ${CUBE_ROUTER_NAT_IP} fwmark ${ROUTE_MARK} iif cube-router lookup ${ROUTE_TABLE}"
  current="$(ip -4 rule show priority "${RULE_PRIORITY}" | sed -n '1p')"
  if [ -n "${current}" ] && [ "${current}" != "${expected}" ]; then
    log "refusing to remove foreign priority ${RULE_PRIORITY}: ${current}"
    return 1
  fi
  if [ -n "${current}" ]; then
    ip -4 rule del priority "${RULE_PRIORITY}" \
      iif cube-router from "${CUBE_ROUTER_NAT_IP}/32" \
      fwmark "${ROUTE_MARK}" lookup "${ROUTE_TABLE}"
  fi

  ip route del table "${ROUTE_TABLE}" \
    default via 169.254.240.2 dev cube-egress-p0 metric 100 2>/dev/null || true
  ip route del table "${ROUTE_TABLE}" \
    default via 169.254.240.6 dev cube-egress-p1 metric 100 2>/dev/null || true
  ip route del table "${ROUTE_TABLE}" \
    unreachable default metric 32767 2>/dev/null || true
}

cleanup_owned_state() {
  preflight_owned_ipset
  cleanup_mark_rules
  ipset destroy "${CLUSTER_CIDR_SET_NEXT}" 2>/dev/null || true
  ipset destroy "${CLUSTER_CIDR_SET}" 2>/dev/null || true
  cleanup_forward_rules
  cleanup_snat_rules
  cleanup_policy_routing
  restore_rp_filter
}

touch "${CLEANUP_REQUEST_FILE}"
attempt=0
while [ -f "${STATE_DIR}/ready" ] &&
  [ "${attempt}" -lt "${CLEANUP_WAIT_SECONDS}" ]; do
  attempt=$((attempt + 1))
  sleep 1
done

cleanup_owned_state || {
  return 1 2>/dev/null || exit 1
}
rm -f "${STATE_DIR}/ready"
log "owned host rules removed"
