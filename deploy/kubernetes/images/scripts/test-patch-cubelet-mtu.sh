#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Unit checks for patch_cubelet_mtu in component-entrypoint.sh
# (no container required). Mirrors test-stage-kernel-select.sh: source the
# entrypoint helpers without executing main, then drive the function.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRY="${SCRIPT_DIR}/component-entrypoint.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# Load entrypoint helpers without executing main.
# shellcheck disable=SC1090
source <(sed '/^main "$@"/d' "${ENTRY}")

assert_file_contains() {
  local file="$1" pattern="$2" msg="$3"
  if ! grep -qE "${pattern}" "${file}"; then
    printf 'FAIL: %s\n  %q does not match %q\n' "${msg}" "$(cat "${file}")" "${pattern}" >&2
    exit 1
  fi
  printf 'ok: %s\n' "${msg}"
}

assert_file_absent() {
  local file="$1" pattern="$2" msg="$3"
  if grep -qE "${pattern}" "${file}"; then
    printf 'FAIL: %s\n  %q unexpectedly matches %q\n' "${msg}" "$(cat "${file}")" "${pattern}" >&2
    exit 1
  fi
  printf 'ok: %s\n' "${msg}"
}

# expect_fail <value> <msg>: value must be rejected with a non-zero exit.
expect_fail() {
  local value="$1" msg="$2"
  local cfg="${TMP}/fail-$$.toml"
  printf 'mvm_mtu = 1500\n' >"${cfg}"
  # Subshell: patch_cubelet_mtu calls fail() which `exit 1`s — inside a
  # subshell that only kills the subshell, letting the `if` catch it.
  if (CUBE_SANDBOX_NETWORK_MTU="${value}" patch_cubelet_mtu "${cfg}") >/dev/null 2>&1; then
    printf 'FAIL: %s (value %q was accepted)\n' "${msg}" "${value}" >&2
    exit 1
  fi
  printf 'ok: %s\n' "${msg}"
}

# --- 1. default / 0 / empty => no-op ---
cfg="${TMP}/noop.toml"
printf 'mvm_mtu = 1500\nother = 1\n' >"${cfg}"
CUBE_SANDBOX_NETWORK_MTU=0 patch_cubelet_mtu "${cfg}"
assert_file_contains "${cfg}" '^mvm_mtu = 1500' "0 keeps packaged default (no-op)"
unset CUBE_SANDBOX_NETWORK_MTU || true
patch_cubelet_mtu "${cfg}"
assert_file_contains "${cfg}" '^mvm_mtu = 1500' "unset keeps packaged default (no-op)"

# --- 2. in-range patch ---
cfg="${TMP}/patch.toml"
printf 'mvm_mtu = 1500\n' >"${cfg}"
CUBE_SANDBOX_NETWORK_MTU=1450 patch_cubelet_mtu "${cfg}"
assert_file_contains "${cfg}" '^mvm_mtu = 1450$' "in-range value patches mvm_mtu"

# --- 3. out-of-range hard fail ---
expect_fail 1279 "below MIN_MTU rejected"
expect_fail 65536 "above u16 max rejected"
expect_fail 68 "sub-1280 rejected (virtio MIN_MTU)"

# --- 4. non-numeric hard fail ---
expect_fail "14O0" "non-numeric rejected (operator typo)"
expect_fail "1450;rm -rf /" "shell metacharacters rejected"

# --- 5. leading zeros normalized, zero-padded in-range accepted ---
cfg="${TMP}/zp.toml"
printf 'mvm_mtu = 1500\n' >"${cfg}"
CUBE_SANDBOX_NETWORK_MTU=001450 patch_cubelet_mtu "${cfg}"
assert_file_contains "${cfg}" '^mvm_mtu = 1450$' "zero-padded 001450 normalized to 1450"

# --- 6. absurdly long digit strings rejected ---
expect_fail "123456789012345678901234567890" "25+ digit string rejected (int64 wrap guard)"
expect_fail "18446744073709552896" "20-digit string that wraps to 1280 in int64 rejected"
expect_fail "18446744073709553066" "20-digit string that wraps to 1450 in int64 rejected"

# --- 7. missing mvm_mtu key: warn + skip, not a hard fail ---
cfg="${TMP}/missing.toml"
printf 'other = 1\n' >"${cfg}"
out="$(CUBE_SANDBOX_NETWORK_MTU=1450 patch_cubelet_mtu "${cfg}" 2>&1)"
[[ "${out}" == *"WARN"* ]] || { printf 'FAIL: missing key should warn\n  got: %s\n' "${out}" >&2; exit 1; }
printf 'ok: missing mvm_mtu key warns and skips\n'

# --- 8. 1450 vs 14500 boundary (post-check must not log false success) ---
cfg="${TMP}/partial.toml"
printf 'mvm_mtu = 14500\n' >"${cfg}"
out="$(CUBE_SANDBOX_NETWORK_MTU=1450 patch_cubelet_mtu "${cfg}" 2>&1)"
assert_file_contains "${cfg}" '^mvm_mtu = 1450$' "14500 rewritten wholesale to 1450 (not partial)"

# --- 9. trailing garbage after the value: post-check must fail ---
cfg="${TMP}/garbage.toml"
printf 'mvm_mtu = 1450abc\n' >"${cfg}"
out="$(CUBE_SANDBOX_NETWORK_MTU=1450 patch_cubelet_mtu "${cfg}" 2>&1)"
[[ "${out}" == *"WARN"* ]] || { printf 'FAIL: trailing garbage should warn\n  got: %s\n' "${out}" >&2; exit 1; }
printf 'ok: value with trailing garbage warns (no false success)\n'

# --- 10. comment line never rewritten ---
cfg="${TMP}/comment.toml"
printf '# mvm_mtu = 1500\nmvm_mtu = 1500\n' >"${cfg}"
CUBE_SANDBOX_NETWORK_MTU=1450 patch_cubelet_mtu "${cfg}"
assert_file_contains "${cfg}" '^# mvm_mtu = 1500$' "commented example untouched"
assert_file_contains "${cfg}" '^mvm_mtu = 1450$' "real setting patched"

# --- 11. no-space form `mvm_mtu=1500` (valid TOML) rewritten ---
cfg="${TMP}/nospace.toml"
printf 'mvm_mtu=1500\n' >"${cfg}"
CUBE_SANDBOX_NETWORK_MTU=1450 patch_cubelet_mtu "${cfg}"
assert_file_contains "${cfg}" '^mvm_mtu = 1450$' "no-space form rewritten"

# --- 12. underscore integer literal `mvm_mtu = 1_500` rewritten wholesale ---
cfg="${TMP}/underscore.toml"
printf 'mvm_mtu = 1_500\n' >"${cfg}"
CUBE_SANDBOX_NETWORK_MTU=1450 patch_cubelet_mtu "${cfg}"
assert_file_contains "${cfg}" '^mvm_mtu = 1450$' "underscore literal rewritten wholesale"
assert_file_absent "${cfg}" '1450_500' "no mangle into 1450_500"

printf 'ALL PASS\n'
