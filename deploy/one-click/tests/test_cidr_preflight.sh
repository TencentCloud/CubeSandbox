#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_contains_text() {
  local haystack="$1"
  local needle="$2"
  [[ "${haystack}" == *"${needle}"* ]] || fail "expected output to contain: ${needle}"
}

assert_occurrences() {
  local haystack="$1"
  local needle="$2"
  local expected="$3"
  local count
  count="$(grep -Fo -- "${needle}" <<<"${haystack}" | wc -l | awk '{print $1}')"
  [[ "${count}" == "${expected}" ]] || fail "expected ${expected} occurrences of '${needle}', got ${count}"
}

ip() {
  local args="$*"
  case "${args}" in
    "-4 addr show scope global")
      case "${IP_SCENARIO:-empty}" in
        empty)
          ;;
        cube_same | cube_overlap)
          printf '2: cube-dev: <BROADCAST,NOARP,UP,LOWER_UP> mtu 1500\n'
          printf '    inet 192.168.0.1/18 brd 192.168.63.255 scope global cube-dev\n'
          printf '3: z192.168.0.10: <BROADCAST,UP,LOWER_UP> mtu 1500\n'
          printf '    inet 192.168.0.10/32 scope global z192.168.0.10\n'
          ;;
        cube_router_residue)
          printf '2: cube-dev: <BROADCAST,NOARP,UP,LOWER_UP> mtu 1500\n'
          printf '    inet 192.168.0.1/18 brd 192.168.63.255 scope global cube-dev\n'
          printf '3: cube-router: <BROADCAST,NOARP,UP,LOWER_UP> mtu 1500\n'
          printf '    inet 192.168.63.253/32 scope global cube-router\n'
          ;;
        host_overlap)
          printf '2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500\n'
          printf '    inet 192.168.0.5/24 brd 192.168.0.255 scope global eth0\n'
          ;;
        *)
          fail "unknown IP_SCENARIO addr: ${IP_SCENARIO}"
          ;;
      esac
      ;;
    "-4 route show")
      case "${IP_SCENARIO:-empty}" in
        empty)
          ;;
        cube_same | cube_overlap)
          printf '192.168.0.0/18 dev cube-dev proto kernel scope link src 192.168.0.1\n'
          printf '192.168.0.10/32 dev z192.168.0.10 scope link\n'
          ;;
        cube_router_residue)
          printf '192.168.0.0/18 dev cube-dev proto kernel scope link src 192.168.0.1\n'
          printf '192.168.63.254/32 dev cube-router scope link\n'
          ;;
        host_overlap)
          printf '192.168.0.0/24 dev eth0 proto kernel scope link src 192.168.0.5\n'
          ;;
        *)
          fail "unknown IP_SCENARIO route: ${IP_SCENARIO}"
          ;;
      esac
      ;;
    *)
      fail "unexpected ip command: ${args}"
      ;;
  esac
}

resolv_conf_candidates() {
  if [[ -n "${TEST_RESOLV_CANDIDATES:-}" ]]; then
    tr ':' '\n' <<<"${TEST_RESOLV_CANDIDATES}"
  fi
}

test_rejects_nameserver_overlap() {
  local resolv="${TMP_DIR}/resolv-overlap.conf"
  printf 'nameserver 192.168.0.1\n' > "${resolv}"

  local output
  if output="$(TEST_RESOLV_CANDIDATES="${resolv}" IP_SCENARIO=empty check_cidr_preflight "192.168.0.0/18" 2>&1)"; then
    fail "expected resolver nameserver overlap to be rejected"
  fi

  assert_contains_text "${output}" "nameserver 192.168.0.1 (${resolv})"
  assert_contains_text "${output}" "DNS nameservers"
}

test_dedupes_equivalent_resolver_paths() {
  local resolv="${TMP_DIR}/resolv-dedupe.conf"
  local resolv_link="${TMP_DIR}/resolv-dedupe-link.conf"
  printf 'nameserver 192.168.0.1\n' > "${resolv}"
  ln -s "${resolv}" "${resolv_link}"

  local output
  if output="$(TEST_RESOLV_CANDIDATES="${resolv}:${resolv_link}" IP_SCENARIO=empty check_cidr_preflight "192.168.0.0/18" 2>&1)"; then
    fail "expected resolver nameserver overlap to be rejected"
  fi

  assert_occurrences "${output}" "nameserver 192.168.0.1" "1"
}

test_allows_same_cidr_cube_dev_reinstall() {
  TEST_RESOLV_CANDIDATES="" IP_SCENARIO=cube_same check_cidr_preflight "192.168.0.0/18" >/dev/null 2>&1
}

test_allows_cube_router_residue_reinstall() {
  TEST_RESOLV_CANDIDATES="" IP_SCENARIO=cube_router_residue check_cidr_preflight "192.168.0.0/18" >/dev/null 2>&1
}

test_rejects_overlapping_cube_dev_cidr_change() {
  local output
  if output="$(TEST_RESOLV_CANDIDATES="" IP_SCENARIO=cube_overlap check_cidr_preflight "192.168.0.0/17" 2>&1)"; then
    fail "expected overlapping cube-dev CIDR change to be rejected"
  fi

  assert_contains_text "${output}" "overlaps an existing cube-dev network (192.168.0.0/18)"
}

test_rejects_host_interface_overlap() {
  local output
  if output="$(TEST_RESOLV_CANDIDATES="" IP_SCENARIO=host_overlap check_cidr_preflight "192.168.0.0/18" 2>&1)"; then
    fail "expected host interface overlap to be rejected"
  fi

  assert_contains_text "${output}" "interface eth0 (192.168.0.5/24)"
}

test_custom_label_is_used_for_format_errors() {
  local output
  if output="$(TEST_RESOLV_CANDIDATES="" IP_SCENARIO=empty check_cidr_preflight "not-a-cidr" 0 "default CubeSandbox network CIDR" 2>&1)"; then
    fail "expected invalid CIDR to be rejected"
  fi

  assert_contains_text "${output}" "default CubeSandbox network CIDR 'not-a-cidr'"
}

test_skip_conflict_keeps_format_validation() {
  if ( TEST_RESOLV_CANDIDATES="" IP_SCENARIO=host_overlap check_cidr_preflight "not-a-cidr" 1 "test CIDR" ) >/dev/null 2>&1; then
    fail "skip_conflict should not bypass format validation"
  fi
}

test_skip_conflict_allows_host_overlap() {
  TEST_RESOLV_CANDIDATES="" IP_SCENARIO=host_overlap check_cidr_preflight "192.168.0.0/18" 1 "test CIDR" >/dev/null 2>&1
}

test_rejects_sandbox_cidr_mask_outside_supported_range() {
  local output
  if output="$(TEST_RESOLV_CANDIDATES="" IP_SCENARIO=empty check_cidr_preflight "10.0.0.0/15" 1 "test CIDR" 2>&1)"; then
    fail "expected /15 sandbox CIDR to be rejected"
  fi
  assert_contains_text "${output}" "test CIDR mask must be between 16 and 24"

  if output="$(TEST_RESOLV_CANDIDATES="" IP_SCENARIO=empty check_cidr_preflight "10.0.0.0/25" 1 "test CIDR" 2>&1)"; then
    fail "expected /25 sandbox CIDR to be rejected"
  fi
  assert_contains_text "${output}" "test CIDR mask must be between 16 and 24"
}

test_rejects_nameserver_overlap
test_dedupes_equivalent_resolver_paths
test_allows_same_cidr_cube_dev_reinstall
test_allows_cube_router_residue_reinstall
test_rejects_overlapping_cube_dev_cidr_change
test_rejects_host_interface_overlap
test_custom_label_is_used_for_format_errors
test_skip_conflict_keeps_format_validation
test_skip_conflict_allows_host_overlap
test_rejects_sandbox_cidr_mask_outside_supported_range

echo "cidr preflight tests OK"
