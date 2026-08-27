#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname "$0")" && pwd)"
REPO_ROOT="$(CDPATH= cd -- "${SCRIPT_DIR}/../../../.." && pwd)"
CLEANUP="${REPO_ROOT}/EgressProxy/configurer/cleanup.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

run_owned_cleanup_case() (
  export CUBE_ROUTER_NAT_IP=172.20.0.2
  state_dir="$(mktemp -d)"
  export CUBE_EGRESS_STATE_DIR="${state_dir}"
  action_file="$(mktemp)"
  mark_jump=present
  marked_forward=present
  snat_jump=present
  trap '/bin/rm -f "${action_file}"; /bin/rm -rf "${state_dir}"' EXIT

  iptables() {
    table="$3"
    operation="$4"
    shift 4
    case "${table}:${operation}:$*" in
      "mangle:-S:PREROUTING")
        [ "${mark_jump}" = present ] &&
          printf '%s\n' '-A PREROUTING -s 172.20.0.2/32 -i cube-router -j CUBE-EGRESS-MARK'
        ;;
      "mangle:-S:CUBE-EGRESS-MARK")
        printf '%s\n' '-N CUBE-EGRESS-MARK'
        printf '%s\n' '-A CUBE-EGRESS-MARK -m set --match-set CUBE-EGRESS-CLUSTER-CIDRS dst -j MARK --set-xmark 0x1000/0x1000'
        ;;
      "filter:-S:CUBE-EGRESS")
        printf '%s\n' '-N CUBE-EGRESS'
        printf '%s\n' '-A CUBE-EGRESS -o cube-egress-p0 -j ACCEPT'
        printf '%s\n' '-A CUBE-EGRESS -o cube-egress-p1 -j ACCEPT'
        printf '%s\n' '-A CUBE-EGRESS -j DROP'
        ;;
      "nat:-S:CUBE-EGRESS-SNAT")
        printf '%s\n' '-N CUBE-EGRESS-SNAT'
        printf '%s\n' '-A CUBE-EGRESS-SNAT -o cube-egress-p0 -j SNAT --to-source 172.20.0.2'
        printf '%s\n' '-A CUBE-EGRESS-SNAT -o cube-egress-p1 -j SNAT --to-source 172.20.0.2'
        ;;
      "mangle:-C:PREROUTING "*)
        [ "${mark_jump}" = present ]
        ;;
      "mangle:-D:PREROUTING "*)
        mark_jump=absent
        printf 'delete-mark-jump\n' >>"${action_file}"
        ;;
      "filter:-C:FORWARD -s 172.20.0.2/32 -i cube-router -m mark "*)
        [ "${marked_forward}" = present ]
        ;;
      "filter:-D:FORWARD -s 172.20.0.2/32 -i cube-router -m mark "*)
        marked_forward=absent
        printf 'delete-marked-forward\n' >>"${action_file}"
        ;;
      "nat:-C:POSTROUTING -s 172.20.0.2/32 -m mark "*)
        [ "${snat_jump}" = present ]
        ;;
      "nat:-D:POSTROUTING -s 172.20.0.2/32 -m mark "*)
        snat_jump=absent
        printf 'delete-snat-jump\n' >>"${action_file}"
        ;;
      "mangle:-F:CUBE-EGRESS-MARK"|"mangle:-X:CUBE-EGRESS-MARK"|\
      "filter:-F:CUBE-EGRESS"|"filter:-X:CUBE-EGRESS"|\
      "nat:-F:CUBE-EGRESS-SNAT"|"nat:-X:CUBE-EGRESS-SNAT")
        printf '%s:%s\n' "${table}" "${operation}" >>"${action_file}"
        ;;
      "filter:-S:FORWARD")
        ;;
      *)
        fail "unexpected iptables command: ${table} ${operation} $*"
        ;;
    esac
  }

  ip() {
    case "$*" in
      "-4 rule show priority 10900")
        printf '10900:\tfrom 172.20.0.2 fwmark 0x1000/0x1000 iif cube-router lookup 100\n'
        ;;
      "-4 rule del priority 10900 "*)
        printf 'delete-policy-rule\n' >>"${action_file}"
        ;;
      "route del table 100 "*)
        printf 'delete-route\n' >>"${action_file}"
        ;;
      *)
        fail "unexpected ip command: $*"
        ;;
    esac
  }

  ipset() {
    case "$*" in
      "save CUBE-EGRESS-CLUSTER-CIDRS")
        printf '%s\n' \
          'create CUBE-EGRESS-CLUSTER-CIDRS hash:net family inet' \
          'add CUBE-EGRESS-CLUSTER-CIDRS 10.244.0.0/16'
        ;;
      "destroy CUBE-EGRESS-CLUSTER-CIDRS-NEXT")
        ;;
      "destroy CUBE-EGRESS-CLUSTER-CIDRS")
        printf 'destroy-cidr-set\n' >>"${action_file}"
        ;;
      *)
        fail "unexpected ipset command: $*"
        ;;
    esac
  }

  rm() {
    :
  }

  # shellcheck source=/dev/null
  . "${CLEANUP}"
  grep -F 'delete-mark-jump' "${action_file}" >/dev/null
  grep -F 'delete-policy-rule' "${action_file}" >/dev/null
  grep -F 'delete-snat-jump' "${action_file}" >/dev/null
  grep -F 'destroy-cidr-set' "${action_file}" >/dev/null
  [ "$(grep -c '^delete-route$' "${action_file}")" -eq 3 ]
)

run_foreign_chain_case() (
  export CUBE_ROUTER_NAT_IP=172.20.0.2
  state_dir="$(mktemp -d)"
  export CUBE_EGRESS_STATE_DIR="${state_dir}"
  trap '/bin/rm -rf "${state_dir}"' EXIT
  iptables() {
    case "$*" in
      "-w -t mangle -S PREROUTING")
        ;;
      "-w -t mangle -S CUBE-EGRESS-MARK")
        printf '%s\n' '-N CUBE-EGRESS-MARK'
        printf '%s\n' '-A CUBE-EGRESS-MARK -j ACCEPT'
        ;;
      *)
        fail "foreign case mutated state: $*"
        ;;
    esac
  }
  log() {
    :
  }

  # shellcheck source=/dev/null
  . "${CLEANUP}"
)

run_owned_cleanup_case
if run_foreign_chain_case; then
  fail "foreign mark chain was removed"
fi

printf 'EgressProxy owned host-state cleanup tests passed\n'
