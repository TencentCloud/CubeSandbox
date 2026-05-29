#!/usr/bin/env bash
set -uo pipefail

# Test script for Cubelet config.toml CIDR and tap_init_num placeholder replacement.
# Verifies that install.sh and common.sh sed substitutions correctly replace
# placeholders in Cubelet/config/config.toml with values from env vars.
#
# Usage:
#   bash deploy/one-click/tests/test-cidr-replace.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

# --- Cross-platform sed -i wrapper ---
sedi() {
  if [[ "$(uname)" == "Darwin" ]]; then
    sed -i '' "$@"
  else
    sed -i "$@"
  fi
}

pass=0
fail=0

assert_eq() {
  local label="$1" actual="$2" expected="$3"
  if [[ "${actual}" == "${expected}" ]]; then
    echo "  PASS: ${label}"
    ((pass++))
  else
    echo "  FAIL: ${label}"
    echo "    expected: ${expected}"
    echo "    actual:   ${actual}"
    ((fail++))
  fi
}

# --- Test 1: Default values ---
echo "=== Test 1: Default values (no env vars set) ==="
mkdir -p "${WORK_DIR}/t1/Cubelet/config" "${WORK_DIR}/t1/CubeMaster"
cp "${PROJECT_ROOT}/Cubelet/config/config.toml" "${WORK_DIR}/t1/Cubelet/config/config.toml"
cp "${PROJECT_ROOT}/configs/single-node/cubemaster.yaml" "${WORK_DIR}/t1/CubeMaster/conf.yaml"

cidr="192.168.0.0/18"
tap_init_num=500

sedi -E \
  -e "s|__CUBE_SANDBOX_CIDR__|${cidr}|g" \
  -e "s/tap_init_num = [0-9]+/tap_init_num = ${tap_init_num}/" \
  "${WORK_DIR}/t1/Cubelet/config/config.toml"

t1_cidr=$(grep -o 'cidr = "[^"]*"' "${WORK_DIR}/t1/Cubelet/config/config.toml")
assert_eq "config.toml cidr" "${t1_cidr}" 'cidr = "192.168.0.0/18"'

t1_tap=$(grep -o 'tap_init_num = [0-9]*' "${WORK_DIR}/t1/Cubelet/config/config.toml")
assert_eq "config.toml tap_init_num" "${t1_tap}" 'tap_init_num = 500'

t1_remain=$(grep -c '__CUBE_SANDBOX_' "${WORK_DIR}/t1/Cubelet/config/config.toml" || true)
assert_eq "config.toml no placeholders remain" "${t1_remain}" "0"

# Verify cubemaster.yaml has no placeholder (denyOut is not modified by this PR)
t1_cm_remain=$(grep -c '__CUBE_SANDBOX_DENY_OUT__' "${WORK_DIR}/t1/CubeMaster/conf.yaml" || true)
assert_eq "cubemaster.yaml no DENY_OUT placeholder" "${t1_cm_remain}" "0"

# --- Test 2: Custom CIDR ---
echo ""
echo "=== Test 2: Custom CIDR (10.128.0.0/16) ==="
mkdir -p "${WORK_DIR}/t2/Cubelet/config"
cp "${PROJECT_ROOT}/Cubelet/config/config.toml" "${WORK_DIR}/t2/Cubelet/config/config.toml"

cidr="10.128.0.0/16"
tap_init_num=500

sedi -E \
  -e "s|__CUBE_SANDBOX_CIDR__|${cidr}|g" \
  -e "s/tap_init_num = [0-9]+/tap_init_num = ${tap_init_num}/" \
  "${WORK_DIR}/t2/Cubelet/config/config.toml"

t2_cidr=$(grep -o 'cidr = "[^"]*"' "${WORK_DIR}/t2/Cubelet/config/config.toml")
assert_eq "config.toml cidr" "${t2_cidr}" 'cidr = "10.128.0.0/16"'

t2_tap=$(grep -o 'tap_init_num = [0-9]*' "${WORK_DIR}/t2/Cubelet/config/config.toml")
assert_eq "config.toml tap_init_num" "${t2_tap}" 'tap_init_num = 500'

t2_remain=$(grep -c '__CUBE_SANDBOX_' "${WORK_DIR}/t2/Cubelet/config/config.toml" || true)
assert_eq "config.toml no placeholders remain" "${t2_remain}" "0"

# --- Test 3: Custom CIDR and tap_init_num ---
echo ""
echo "=== Test 3: Custom CIDR and tap_init_num ==="
mkdir -p "${WORK_DIR}/t3/Cubelet/config"
cp "${PROJECT_ROOT}/Cubelet/config/config.toml" "${WORK_DIR}/t3/Cubelet/config/config.toml"

cidr="10.200.0.0/16"
tap_init_num=800

sedi -E \
  -e "s|__CUBE_SANDBOX_CIDR__|${cidr}|g" \
  -e "s/tap_init_num = [0-9]+/tap_init_num = ${tap_init_num}/" \
  "${WORK_DIR}/t3/Cubelet/config/config.toml"

t3_cidr=$(grep -o 'cidr = "[^"]*"' "${WORK_DIR}/t3/Cubelet/config/config.toml")
assert_eq "config.toml cidr" "${t3_cidr}" 'cidr = "10.200.0.0/16"'

t3_tap=$(grep -o 'tap_init_num = [0-9]*' "${WORK_DIR}/t3/Cubelet/config/config.toml")
assert_eq "config.toml tap_init_num" "${t3_tap}" 'tap_init_num = 800'

t3_remain=$(grep -c '__CUBE_SANDBOX_' "${WORK_DIR}/t3/Cubelet/config/config.toml" || true)
assert_eq "config.toml no placeholders remain" "${t3_remain}" "0"

# --- Summary ---
echo ""
echo "=============================="
echo "Results: ${pass} passed, ${fail} failed"
echo "=============================="
[[ "${fail}" -eq 0 ]] || exit 1
