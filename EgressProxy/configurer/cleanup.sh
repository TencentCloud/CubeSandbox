#!/bin/sh
set -eu

ROUTE_TABLE="${CUBE_EGRESS_ROUTE_TABLE:-10901}"
RULE_PRIORITY=10900
ROUTE_MARK=0x1000/0x1000
SNAT_CHAIN=CUBE-EGRESS-SNAT
CUBE_ROUTER_NAT_IP="${CUBE_ROUTER_NAT_IP:?CUBE_ROUTER_NAT_IP is required}"
STATE_DIR="${CUBE_EGRESS_STATE_DIR:-/run/cube-egress}"
CLEANUP_REQUEST_FILE="${STATE_DIR}/cleanup-requested"
RECONCILE_INTERVAL_SECONDS="${CUBE_EGRESS_RECONCILE_INTERVAL_SECONDS:-10}"
CLEANUP_WAIT_SECONDS="${CUBE_EGRESS_CLEANUP_WAIT_SECONDS:-$((RECONCILE_INTERVAL_SECONDS + 10))}"
RP_FILTER_SYSCTL_ROOT="${CUBE_EGRESS_RP_FILTER_SYSCTL_ROOT:-/host-sysctl/conf}"
EGRESS_HOST_IFACES="cube-egress-p0 cube-egress-p1"

log() {
  printf '[egress-cleanup] %s\n' "$*"
}

# A chain we created is either an empty shell, the per-CIDR shape emitted by
# reconcile (one "-d <cidr> -j MARK" rule per cluster CIDR), or the legacy
# single ipset rule from an earlier release. Anything else is foreign state and
# must not be deleted.
mark_chain_is_owned() {
  printf '%s\n' "$1" | awk '
    NR == 1 {
      if ($0 != "-N CUBE-EGRESS-MARK") exit 1
      next
    }
    $1 == "-A" && $2 == "CUBE-EGRESS-MARK" {
      if ($0 ~ /^-A CUBE-EGRESS-MARK -d [0-9.]+\/[0-9]+ -j MARK --set-xmark 0x1000\/0x1000$/) {
        rules++
        next
      }
      if ($0 == "-A CUBE-EGRESS-MARK -m set --match-set CUBE-EGRESS-CLUSTER-CIDRS dst -j MARK --set-xmark 0x1000/0x1000") {
        legacy++
        next
      }
      exit 1
    }
    { exit 1 }
    END { exit !((rules > 0 && legacy == 0) || (legacy == 1 && rules == 0)) }
  '
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

# rp_filter is set per cube-egress-pN interface, and those interfaces are
# destroyed with the Proxy Pod. There is no global baseline to restore, so
# cleanup only has to report a residual value if the interface somehow outlives
# the Pod.
report_rp_filter() {
  for iface in ${EGRESS_HOST_IFACES}; do
    path="${RP_FILTER_SYSCTL_ROOT}/${iface}/rp_filter"
    [ -e "${path}" ] || continue
    log "interface ${iface} still present with rp_filter=$(cat "${path}")"
  done
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
  cleanup_mark_rules
  cleanup_forward_rules
  cleanup_snat_rules
  cleanup_policy_routing
  report_rp_filter
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
