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

test_install_sh_wires_upgrade_flow() {
  local f="${ONE_CLICK_DIR}/install.sh"
  assert_contains "${f}" "resolve_install_mode"
  assert_contains "${f}" "preflight_upgrade"
  assert_contains "${f}" "backup_before_upgrade"
  assert_contains "${f}" "merge_env_three_way"
  assert_contains "${f}" '--mode=*'
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
test_install_sh_wires_upgrade_flow

echo "install mode tests OK"
