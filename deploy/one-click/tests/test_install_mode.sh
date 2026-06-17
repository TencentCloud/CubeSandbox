#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Unit tests for install-mode resolution and the static wiring of install.sh's
# config-preserving upgrade flow (M3-1/M3-2). resolve_install_mode is exercised
# directly; install.sh itself is checked structurally (it requires root/KVM to
# actually run).
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

assert_contains() {
  grep -Fq -- "$2" "$1" || fail "expected $1 to contain: $2"
}

make_install_dir() {
  local d="$1"
  mkdir -p "${d}"
  : > "${d}/.one-click.env"
}

# resolve_install_mode reads stdin for the interactive prompt; run all
# resolution tests with stdin closed so they take the non-interactive paths.

test_explicit_install_mode() {
  local d="${TMP_DIR}/a"
  make_install_dir "${d}"
  local got
  got="$(resolve_install_mode install "${d}" 0 < /dev/null 2>/dev/null)"
  [[ "${got}" == "install" ]] || fail "explicit install should resolve to install (got ${got})"
}

test_explicit_upgrade_requires_existing() {
  local d="${TMP_DIR}/missing"
  mkdir -p "${d}"
  # Subshell: resolve_install_mode calls die (exit 1) on this path; isolate it.
  if ( resolve_install_mode upgrade "${d}" 0 ) < /dev/null >/dev/null 2>&1; then
    fail "upgrade without existing install should fail"
  fi
}

test_explicit_upgrade_with_existing() {
  local d="${TMP_DIR}/b"
  make_install_dir "${d}"
  local got
  got="$(resolve_install_mode upgrade "${d}" 0 < /dev/null 2>/dev/null)"
  [[ "${got}" == "upgrade" ]] || fail "upgrade with existing should resolve to upgrade (got ${got})"
}

test_auto_mode() {
  local existing="${TMP_DIR}/c" fresh="${TMP_DIR}/d"
  make_install_dir "${existing}"
  mkdir -p "${fresh}"
  local got
  got="$(resolve_install_mode auto "${existing}" 0 < /dev/null 2>/dev/null)"
  [[ "${got}" == "upgrade" ]] || fail "auto+existing should be upgrade (got ${got})"
  got="$(resolve_install_mode auto "${fresh}" 0 < /dev/null 2>/dev/null)"
  [[ "${got}" == "install" ]] || fail "auto+fresh should be install (got ${got})"
}

test_default_fresh_is_install() {
  local d="${TMP_DIR}/e"
  mkdir -p "${d}"
  local got
  got="$(resolve_install_mode "" "${d}" 0 < /dev/null 2>/dev/null)"
  [[ "${got}" == "install" ]] || fail "default+fresh should be install (got ${got})"
}

test_default_existing_non_interactive_is_install() {
  local d="${TMP_DIR}/f"
  make_install_dir "${d}"
  local got
  got="$(resolve_install_mode "" "${d}" 0 < /dev/null 2>/dev/null)"
  [[ "${got}" == "install" ]] \
    || fail "default+existing+non-interactive should default to install (got ${got})"
}

test_assume_yes_existing_is_upgrade() {
  local d="${TMP_DIR}/g"
  make_install_dir "${d}"
  local got
  got="$(resolve_install_mode "" "${d}" 1 < /dev/null 2>/dev/null)"
  [[ "${got}" == "upgrade" ]] || fail "default+existing+--yes should be upgrade (got ${got})"
}

test_parse_args_space_and_equals_forms() {
  one_click_parse_args --mode upgrade
  [[ "${CLI_MODE}" == "upgrade" ]] || fail "--mode upgrade (space) should set CLI_MODE (got '${CLI_MODE}')"
  one_click_parse_args --mode=upgrade
  [[ "${CLI_MODE}" == "upgrade" ]] || fail "--mode=upgrade should set CLI_MODE (got '${CLI_MODE}')"

  one_click_parse_args --node-ip 10.0.0.7
  [[ "${CLI_NODE_IP}" == "10.0.0.7" ]] || fail "--node-ip (space) should set CLI_NODE_IP (got '${CLI_NODE_IP}')"
  one_click_parse_args --node-ip=10.0.0.8
  [[ "${CLI_NODE_IP}" == "10.0.0.8" ]] || fail "--node-ip= should set CLI_NODE_IP (got '${CLI_NODE_IP}')"

  one_click_parse_args -y --allow-downgrade --allow-role-change
  [[ "${CLI_ASSUME_YES}" == "1" ]] || fail "-y should set CLI_ASSUME_YES"
  [[ "${CLI_ALLOW_DOWNGRADE}" == "1" ]] || fail "--allow-downgrade should set CLI_ALLOW_DOWNGRADE"
  [[ "${CLI_ALLOW_ROLE_CHANGE}" == "1" ]] || fail "--allow-role-change should set CLI_ALLOW_ROLE_CHANGE"
}

test_parse_args_missing_value_fails() {
  if ( one_click_parse_args --mode ) >/dev/null 2>&1; then
    fail "bare --mode should fail (missing value)"
  fi
  if ( one_click_parse_args --node-ip ) >/dev/null 2>&1; then
    fail "bare --node-ip should fail (missing value)"
  fi
}

test_parse_args_unknown_is_ignored() {
  # Unknown tokens warn but do not fail, and do not set any CLI_* value.
  one_click_parse_args --not-a-flag positional 2>/dev/null
  [[ -z "${CLI_MODE}" ]] || fail "unknown args should not set CLI_MODE"
}

test_assert_safe_install_prefix() {
  for bad in "/" "/usr" "/etc" "/home" "relative/path" "/toplevel"; do
    if ( assert_safe_install_prefix "${bad}" ) >/dev/null 2>&1; then
      fail "assert_safe_install_prefix should reject: ${bad}"
    fi
  done
  ( assert_safe_install_prefix "/usr/local/services/cubetoolbox" ) >/dev/null 2>&1 \
    || fail "assert_safe_install_prefix should accept a normal deep prefix"
  ( assert_safe_install_prefix "/opt/cube/custom/" ) >/dev/null 2>&1 \
    || fail "assert_safe_install_prefix should accept a deep prefix with trailing slash"

  # Content sanity check: a non-empty prefix with no CubeSandbox marker is
  # foreign (e.g. a mis-set ONE_CLICK_INSTALL_PREFIX=/usr/local) and must be
  # refused so the wipe does not rm -rf unrelated content.
  local foreign="${TMP_DIR}/foreign"
  mkdir -p "${foreign}/somedir"
  : > "${foreign}/notes.txt"
  if ( assert_safe_install_prefix "${foreign}" ) >/dev/null 2>&1; then
    fail "assert_safe_install_prefix should reject a non-empty foreign prefix (no CubeSandbox marker)"
  fi

  # A real CubeSandbox install (marker present) is accepted even when non-empty.
  local cube="${TMP_DIR}/cube"
  mkdir -p "${cube}/cubeproxy"
  : > "${cube}/.one-click.env"
  : > "${cube}/CubeMaster"
  ( assert_safe_install_prefix "${cube}" ) >/dev/null 2>&1 \
    || fail "assert_safe_install_prefix should accept a prefix with a CubeSandbox marker"

  # An empty prefix is accepted (fresh install, nothing to destroy).
  local empty="${TMP_DIR}/empty"
  mkdir -p "${empty}"
  ( assert_safe_install_prefix "${empty}" ) >/dev/null 2>&1 \
    || fail "assert_safe_install_prefix should accept an empty prefix"

  # A prefix holding only '.backup' is accepted (the wipe preserves .backup,
  # e.g. after an interrupted upgrade).
  local onlybak="${TMP_DIR}/onlybak"
  mkdir -p "${onlybak}/.backup"
  ( assert_safe_install_prefix "${onlybak}" ) >/dev/null 2>&1 \
    || fail "assert_safe_install_prefix should accept a prefix holding only .backup"
}

test_install_sh_wires_upgrade_flow() {
  local f="${ONE_CLICK_DIR}/install.sh"
  assert_contains "${f}" "resolve_install_mode"
  assert_contains "${f}" "preflight_upgrade"
  assert_contains "${f}" "backup_before_upgrade"
  assert_contains "${f}" "merge_env_three_way"
  # CLI parsing is delegated to one_click_parse_args (supports --mode/--node-ip
  # in both = and space forms) and CLI values are re-applied after .env load.
  assert_contains "${f}" 'one_click_parse_args "$@"'
  assert_contains "${f}" "apply_cli_overrides"
  # custom-prefix wipe is guarded against unsafe install prefixes
  assert_contains "${f}" 'assert_safe_install_prefix "${INSTALL_PREFIX}"'
  # env.example baseline is installed for future three-way merges
  assert_contains "${f}" 'cp -f "${SCRIPT_DIR}/env.example" "${INSTALL_PREFIX}/env.example"'
  # upgrade writes the merged env as the runtime env
  assert_contains "${f}" 'cp -f "${MERGED_ENV}" "${RUNTIME_ENV_FILE}"'
  # full-wipe branch preserves the upgrade backup directory (M1)
  assert_contains "${f}" "! -name '.backup'"
  # on upgrade, CIDR host-conflict detection is skipped (M2)
  assert_contains "${f}" 'check_cidr_preflight "${CUBE_SANDBOX_NETWORK_CIDR}" "${cidr_skip_conflict}"'
}

test_explicit_install_mode
test_explicit_upgrade_requires_existing
test_explicit_upgrade_with_existing
test_auto_mode
test_default_fresh_is_install
test_default_existing_non_interactive_is_install
test_assume_yes_existing_is_upgrade
test_parse_args_space_and_equals_forms
test_parse_args_missing_value_fails
test_parse_args_unknown_is_ignored
test_assert_safe_install_prefix
test_install_sh_wires_upgrade_flow

echo "install mode tests OK"
