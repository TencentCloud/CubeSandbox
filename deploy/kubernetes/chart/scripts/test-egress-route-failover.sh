#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
REPO_ROOT="$(CDPATH= cd -- "${SCRIPT_DIR}/../../../.." && pwd)"
RECONCILE="${REPO_ROOT}/EgressProxy/configurer/reconcile.sh"
CLUSTER_CIDRS="10.244.0.0/16,10.96.0.0/12"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

run_case() (
  case_name="$1"
  current_iface="$2"
  p0_healthy="$3"
  p1_healthy="$4"
  expected_actions="$5"
  expected_status="$6"

  export CUBE_EGRESS_RECONCILE_LIBRARY_ONLY=1
  export CUBE_EGRESS_CLUSTER_CIDRS="${CLUSTER_CIDRS}"
  export NODE_NAME=test-node
  export CUBE_ROUTER_NAT_IP=172.20.0.2
  # shellcheck source=/dev/null
  . "${RECONCILE}"

  action_file="$(mktemp)"
  trap 'rm -f "${action_file}"' EXIT

  peer_is_healthy() {
    iface="$1"
    case "${iface}" in
      cube-egress-p0) [ "${p0_healthy}" = "true" ] ;;
      cube-egress-p1) [ "${p1_healthy}" = "true" ] ;;
      *) return 1 ;;
    esac
  }

  print_routes() {
    if [ -n "${current_iface}" ]; then
      case "${current_iface}" in
        cube-egress-p0) gateway=169.254.240.2 ;;
        cube-egress-p1) gateway=169.254.240.6 ;;
      esac
      printf 'default via %s dev %s metric 100 \n' \
        "${gateway}" "${current_iface}"
    fi
    printf 'unreachable default metric 32767\n'
  }

  ip() {
    case "$*" in
      "route show table 100")
        print_routes
        ;;
      "route replace table 100 default via "*)
        printf 'replace:%s\n' "$9" >>"${action_file}"
        ;;
      "route del table 100 default metric 100")
        printf 'delete:%s\n' "${current_iface}" >>"${action_file}"
        ;;
      "route replace table 100 unreachable default metric 32767")
        ;;
      *)
        fail "${case_name}: unexpected ip command: $*"
        ;;
    esac
  }

  log() {
    :
  }

  ROUTE_STATE="$(print_routes)"
  if ensure_policy_route; then
    actual_status=0
  else
    actual_status=1
  fi
  actual_actions="$(cat "${action_file}")"
  if [ "${actual_actions}" != "${expected_actions}" ]; then
    fail "${case_name}: expected '${expected_actions}', got '${actual_actions}'"
  fi
  if [ "${actual_status}" -ne "${expected_status}" ]; then
    fail "${case_name}: expected status ${expected_status}, got ${actual_status}"
  fi
)

# Keep the current healthy standby-selected peer without rewriting routes.
run_case sticky-current cube-egress-p1 true true "" 0
# Switch the marked-traffic default route only after the current peer fails.
run_case failed-current cube-egress-p1 true false \
"replace:cube-egress-p0" 0
# Remove the active route while retaining unreachable default on total failure.
run_case all-failed cube-egress-p0 false false \
"delete:cube-egress-p0" 1

run_route_table_collision_case() (
  export CUBE_EGRESS_RECONCILE_LIBRARY_ONLY=1
  export CUBE_EGRESS_CLUSTER_CIDRS="${CLUSTER_CIDRS}"
  export NODE_NAME=test-node
  export CUBE_ROUTER_NAT_IP=172.20.0.2
  # shellcheck source=/dev/null
  . "${RECONCILE}"

  ip() {
    case "$*" in
      "route show table 100")
        printf '10.0.0.0/8 via 192.0.2.1 dev eth9\n'
        ;;
      *)
        fail "route-table collision case mutated state: $*"
        ;;
    esac
  }

  log() {
    :
  }

  ROUTE_STATE='10.0.0.0/8 via 192.0.2.1 dev eth9'
  if ensure_route_table_ownership; then
    fail "foreign route-table content was accepted"
  fi
)

run_route_table_collision_case

run_proxy_process_probe_case() (
  response="$1"
  expected_status="$2"
  export CUBE_EGRESS_RECONCILE_LIBRARY_ONLY=1
  export CUBE_EGRESS_CLUSTER_CIDRS="${CLUSTER_CIDRS}"
  export NODE_NAME=test-node
  export CUBE_ROUTER_NAT_IP=172.20.0.2
  # shellcheck source=/dev/null
  . "${RECONCILE}"

  ip() {
    case "$*" in
      "link show cube-egress-p0")
        ;;
      "-4 -o address show dev cube-egress-p0")
        printf '1: cube-egress-p0 inet 169.254.240.1/30 scope global cube-egress-p0\n'
        ;;
      *)
        fail "proxy probe case unexpected ip command: $*"
        ;;
    esac
  }
  nc() {
    [ "${response}" != "connection-failed" ] || return 1
    printf '%s\n' "${response}"
  }

  if peer_is_healthy cube-egress-p0; then
    actual_status=0
  else
    actual_status=1
  fi
  [ "${actual_status}" -eq "${expected_status}" ] ||
    fail "proxy probe response ${response}: expected ${expected_status}, got ${actual_status}"
)

run_proxy_process_probe_case ok 0
run_proxy_process_probe_case failed 1
run_proxy_process_probe_case connection-failed 1

printf 'EgressProxy local-veth sticky failover tests passed\n'
