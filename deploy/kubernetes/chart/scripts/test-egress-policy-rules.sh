#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
REPO_ROOT="$(CDPATH= cd -- "${SCRIPT_DIR}/../../../.." && pwd)"
RECONCILE="${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

run_create_case() (
  export CUBE_EGRESS_RECONCILE_LIBRARY_ONLY=1
  export CUBE_EGRESS_CLUSTER_CIDRS=10.0.0.1/24,10.0.0.2/24
  export NODE_NAME=test-node
  export CUBE_ROUTER_NAT_IP=172.20.0.2
  # shellcheck source=/dev/null
  . "${RECONCILE}"
  POLICY_RULES=""

  [ "${CLUSTER_CIDRS}" = "10.0.0.0/24" ] ||
    fail "CIDRs were not normalized and deduplicated: ${CLUSTER_CIDRS}"

  action_file="$(mktemp)"
  trap 'rm -f "${action_file}"' EXIT
  ip() {
    case "$*" in
      "-4 rule show priority 10900")
        ;;
      "-4 rule add priority 10900 "*)
        printf '%s\n' "$*" >"${action_file}"
        ;;
      *)
        fail "create case unexpected ip command: $*"
        ;;
    esac
  }

  preflight_policy_rule_ownership
  ensure_policy_rule
  grep -F 'fwmark 0x1000/0x1000 lookup 10901' "${action_file}" >/dev/null ||
    fail "mark-scoped policy rule was not created"
)

run_idempotent_case() (
  export CUBE_EGRESS_RECONCILE_LIBRARY_ONLY=1
  export CUBE_EGRESS_CLUSTER_CIDRS=10.244.0.0/16
  export NODE_NAME=test-node
  export CUBE_ROUTER_NAT_IP=172.20.0.2
  # shellcheck source=/dev/null
  . "${RECONCILE}"
  POLICY_RULES="$(expected_policy_rule)"

  ip() {
    fail "idempotent case mutated rules: $*"
  }

  preflight_policy_rule_ownership
  ensure_policy_rule
)

run_collision_case() (
  export CUBE_EGRESS_RECONCILE_LIBRARY_ONLY=1
  export CUBE_EGRESS_CLUSTER_CIDRS=10.244.0.0/16
  export NODE_NAME=test-node
  export CUBE_ROUTER_NAT_IP=172.20.0.2
  # shellcheck source=/dev/null
  . "${RECONCILE}"
  POLICY_RULES="$(printf '10900:\tfrom all lookup 200\n')"

  ip() {
    fail "collision case mutated rules: $*"
  }
  log() {
    :
  }

  if preflight_policy_rule_ownership; then
    fail "foreign primary-priority collision was accepted"
  fi
)

run_iptables_collision_case() (
  export CUBE_EGRESS_RECONCILE_LIBRARY_ONLY=1
  export CUBE_EGRESS_CLUSTER_CIDRS=10.244.0.0/16
  export NODE_NAME=test-node
  export CUBE_ROUTER_NAT_IP=172.20.0.2
  # shellcheck source=/dev/null
  . "${RECONCILE}"
  MANGLE_RULES="$(printf '%s\n' \
    '-N CUBE-EGRESS-MARK' \
    '-A CUBE-EGRESS-MARK -j ACCEPT')"
  FILTER_RULES=""
  log() {
    :
  }

  if preflight_iptables_ownership; then
    fail "foreign iptables chain was accepted"
  fi
)

run_mark_chain_cidr_case() (
  export CUBE_EGRESS_RECONCILE_LIBRARY_ONLY=1
  export CUBE_EGRESS_CLUSTER_CIDRS=10.0.0.1/24,10.1.0.0/16
  export NODE_NAME=test-node
  export CUBE_ROUTER_NAT_IP=172.20.0.2
  # shellcheck source=/dev/null
  . "${RECONCILE}"

  expected_chain="$(expected_mark_chain)"
  expected_chain="$(cat <<'EOF'
-N CUBE-EGRESS-MARK
-A CUBE-EGRESS-MARK -d 10.0.0.0/24 -j MARK --set-xmark 0x1000/0x1000
-A CUBE-EGRESS-MARK -d 10.1.0.0/16 -j MARK --set-xmark 0x1000/0x1000
EOF
)"
  [ "$(expected_mark_chain)" = "${expected_chain}" ] ||
    fail "mark chain did not expand one rule per normalized cluster CIDR"

  # The chain must not depend on ipset any more.
  printf '%s\n' "${expected_chain}" | grep -q -- '--match-set' &&
    fail "mark chain still references an ipset"
  true
)

run_missing_route_table_snapshot_case() (
  export CUBE_EGRESS_RECONCILE_LIBRARY_ONLY=1
  export CUBE_EGRESS_CLUSTER_CIDRS=10.244.0.0/16
  export NODE_NAME=test-node
  export CUBE_ROUTER_NAT_IP=172.20.0.2
  # shellcheck source=/dev/null
  . "${RECONCILE}"

  iptables() {
    :
  }
  ip() {
    case "$*" in
      "-4 rule show") : ;;
      "route show table 10901") return 2 ;;
      *) fail "missing route-table case unexpected ip command: $*" ;;
    esac
  }

  load_state_snapshots
  [ -z "${ROUTE_STATE}" ] ||
    fail "missing route table was not treated as an empty snapshot"
)

run_create_case
run_idempotent_case
run_collision_case
run_iptables_collision_case
run_mark_chain_cidr_case
run_missing_route_table_snapshot_case

preflight_line="$(
  grep -n '^preflight_policy_rule_ownership$' "${RECONCILE}" |
    cut -d: -f1
)"
write_line="$(
  grep -n '^ensure_mark_chain$' "${RECONCILE}" |
    cut -d: -f1
)"
[ "${preflight_line}" -lt "${write_line}" ] ||
  fail "ownership preflight must complete before the first state write"

printf 'EgressProxy policy-rule ownership and CIDR normalization tests passed\n'
