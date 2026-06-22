#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"

failures=0

fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

assert_lt() {
  local left="$1"
  local right="$2"
  version_lt "${left}" "${right}" || fail "expected ${left} < ${right}"
}

assert_not_lt() {
  local left="$1"
  local right="$2"
  if version_lt "${left}" "${right}"; then
    fail "expected ${left} !< ${right}"
  fi
}

assert_cmp() {
  local left="$1"
  local right="$2"
  local expected="$3"
  local actual
  actual="$(semver_compare "${left}" "${right}")"
  [[ "${actual}" == "${expected}" ]] || fail "expected compare(${left}, ${right})=${expected}, got ${actual}"
}

assert_cmp_without_stderr() {
  local left="$1"
  local right="$2"
  local expected="$3"
  local actual err_file err
  err_file="$(mktemp)"
  actual="$(semver_compare "${left}" "${right}" 2>"${err_file}")"
  err="$(<"${err_file}")"
  rm -f "${err_file}"

  [[ "${actual}" == "${expected}" ]] || fail "expected compare(${left}, ${right})=${expected}, got ${actual}"
  [[ -z "${err}" ]] || fail "expected compare(${left}, ${right}) to keep stderr empty, got: ${err}"
}

assert_debug_fallback_logs() {
  local err_file err
  err_file="$(mktemp)"
  ONE_CLICK_VERSION_COMPARE_DEBUG=1 semver_compare "release-b" "release-a" > /dev/null 2>"${err_file}"
  err="$(<"${err_file}")"
  rm -f "${err_file}"

  [[ "${err}" == *"DEBUG: falling back to lexical version comparison"* ]] || fail "expected debug fallback log, got: ${err}"
}

test_rc_to_stable_is_upgrade() {
  assert_lt "v0.4.0-rc1" "v0.4.0"
  assert_not_lt "v0.4.0" "v0.4.0-rc1"
}

test_stable_versions_and_true_downgrade() {
  assert_lt "v0.4.0" "v0.5.0"
  assert_cmp "v0.5.0" "v0.4.0" "1"
  assert_not_lt "v0.5.0" "v0.4.0"
  assert_not_lt "v0.4.0" "v0.4.0"
}

test_rc_identifier_ordering() {
  assert_lt "v0.4.0-rc1" "v0.4.0-rc2"
  assert_not_lt "v0.4.0-rc2" "v0.4.0-rc1"
  assert_lt "v0.4.0-rc2" "v0.4.0-rc10"
}

test_optional_v_prefix() {
  assert_lt "0.4.0" "v0.4.1"
  assert_cmp "v0.4.0" "0.4.0" "0"
}

test_build_metadata_is_ignored() {
  assert_cmp "v0.4.0+build.1" "v0.4.0+build.2" "0"
  assert_not_lt "v0.4.0+build.1" "v0.4.0+build.2"
  assert_lt "v0.4.0-rc1+build.1" "v0.4.0+build.2"
}

test_semver_prerelease_segments() {
  assert_lt "v0.4.0-alpha" "v0.4.0-alpha.1"
  assert_lt "v0.4.0-alpha.1" "v0.4.0-alpha.beta"
  assert_lt "v0.4.0-alpha.9" "v0.4.0-alpha.10"
}

test_invalid_semver_falls_back_to_lexical() {
  assert_cmp_without_stderr "release-b" "release-a" "1"
  assert_cmp_without_stderr "release-a" "release-b" "-1"
  assert_cmp_without_stderr "v0.4.0" "release-a" "1"
  assert_debug_fallback_logs
}

test_rc_to_stable_is_upgrade
test_stable_versions_and_true_downgrade
test_rc_identifier_ordering
test_optional_v_prefix
test_build_metadata_is_ignored
test_semver_prerelease_segments
test_invalid_semver_falls_back_to_lexical

if [[ "${failures}" -gt 0 ]]; then
  echo "${failures} version compare test(s) failed" >&2
  exit 1
fi

echo "version compare tests OK"
