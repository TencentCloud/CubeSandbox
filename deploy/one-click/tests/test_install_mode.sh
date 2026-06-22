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
  ( assert_safe_install_prefix "${TMP_DIR}/usr/local/services/cubetoolbox" ) >/dev/null 2>&1 \
    || fail "assert_safe_install_prefix should accept a normal deep prefix"
  ( assert_safe_install_prefix "${TMP_DIR}/opt/cube/custom/" ) >/dev/null 2>&1 \
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
  # e.g. after an interrupted upgrade) when .backup is a real directory.
  local onlybak="${TMP_DIR}/onlybak"
  mkdir -p "${onlybak}/.backup"
  ( assert_safe_install_prefix "${onlybak}" ) >/dev/null 2>&1 \
    || fail "assert_safe_install_prefix should accept a prefix holding only .backup"

  local symlink_target="${TMP_DIR}/symlink-target"
  mkdir -p "${symlink_target}"
  local symlink_prefix="${TMP_DIR}/symlink-prefix"
  mkdir -p "${symlink_prefix}"
  ln -s "${symlink_target}" "${symlink_prefix}/linked"
  if ( assert_safe_install_prefix "${symlink_prefix}" ) >/dev/null 2>&1; then
    fail "assert_safe_install_prefix should reject top-level symlinks"
  fi

  local backup_link_prefix="${TMP_DIR}/backup-link-prefix"
  mkdir -p "${backup_link_prefix}" "${TMP_DIR}/backup-link-target"
  ln -s "${TMP_DIR}/backup-link-target" "${backup_link_prefix}/.backup"
  if ( assert_safe_install_prefix "${backup_link_prefix}" ) >/dev/null 2>&1; then
    fail "assert_safe_install_prefix should reject a .backup symlink"
  fi
}

test_wipe_custom_install_prefix_contents() {
  local prefix="${TMP_DIR}/wipe-prefix"
  mkdir -p "${prefix}/.backup/keep" "${prefix}/Cubelet" "${prefix}/foreign"
  : > "${prefix}/.one-click.env"
  : > "${prefix}/foreign/file.txt"

  wipe_custom_install_prefix_contents "${prefix}"
  [[ -d "${prefix}/.backup" ]] || fail "wipe should preserve .backup"
  [[ ! -e "${prefix}/Cubelet" ]] || fail "wipe should remove Cubelet"
  [[ ! -e "${prefix}/foreign" ]] || fail "wipe should remove foreign top-level entries inside a marker-bearing prefix"
  [[ ! -e "${prefix}/.one-click.env" ]] || fail "wipe should remove runtime env"

  local foreign="${TMP_DIR}/wipe-foreign"
  mkdir -p "${foreign}"
  : > "${foreign}/file.txt"
  if ( wipe_custom_install_prefix_contents "${foreign}" ) >/dev/null 2>&1; then
    fail "wipe should reject a non-empty prefix without CubeSandbox markers"
  fi
  [[ -e "${foreign}/file.txt" ]] || fail "rejected foreign prefix must remain untouched"

  local with_symlink="${TMP_DIR}/wipe-with-symlink" external="${TMP_DIR}/external-target"
  mkdir -p "${with_symlink}" "${external}"
  : > "${with_symlink}/.one-click.env"
  : > "${external}/keep.txt"
  ln -s "${external}" "${with_symlink}/linked"
  if ( wipe_custom_install_prefix_contents "${with_symlink}" ) >/dev/null 2>&1; then
    fail "wipe should reject a marker-bearing prefix with a top-level symlink"
  fi
  [[ -e "${external}/keep.txt" ]] || fail "wipe must not touch symlink target content"
}

test_control_plane_validators() {
  validate_ipv4_literal "10.0.0.11" "ONE_CLICK_CONTROL_PLANE_IP"
  validate_host_port "control.example.internal:8089" "ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR"
  validate_host_port "10.0.0.11:8089" "ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR"

  if ( validate_ipv4_literal "999.0.0.1" "ONE_CLICK_CONTROL_PLANE_IP" ) >/dev/null 2>&1; then
    fail "validate_ipv4_literal should reject out-of-range octets"
  fi
  if ( validate_host_port 'bad/host:8089' "ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR" ) >/dev/null 2>&1; then
    fail "validate_host_port should reject slash-containing hosts"
  fi
  if ( validate_host_port "host:70000" "ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR" ) >/dev/null 2>&1; then
    fail "validate_host_port should reject out-of-range ports"
  fi
}

test_patch_cubelet_config_template_refuses_symlink() {
  local cfg="${TMP_DIR}/cubelet-config.toml"
  cat > "${cfg}" <<'EOF'
eth_name = "eth0"
cidr = "192.168.0.0/18"
EOF

  patch_cubelet_config_template "${cfg}" "ens3" "10.123.0.0/16" >/dev/null 2>&1
  grep -Fq 'eth_name = "ens3"' "${cfg}" || fail "patch should update eth_name"
  grep -Fq 'cidr = "10.123.0.0/16"' "${cfg}" || fail "patch should update cidr"

  local target="${TMP_DIR}/symlink-target.toml" link="${TMP_DIR}/symlink-config.toml"
  cat > "${target}" <<'EOF'
eth_name = "eth0"
cidr = "192.168.0.0/18"
EOF
  ln -s "${target}" "${link}"
  if ( patch_cubelet_config_template "${link}" "ens4" "10.124.0.0/16" ) >/dev/null 2>&1; then
    fail "patch_cubelet_config_template should reject symlink configs"
  fi
  grep -Fq 'eth_name = "eth0"' "${target}" || fail "symlink target must not be modified"
  grep -Fq 'cidr = "192.168.0.0/18"' "${target}" || fail "symlink target CIDR must not be modified"
}

test_upgrade_preflight_and_backup() {
  local inst="${TMP_DIR}/preflight-inst" bundle="${TMP_DIR}/preflight-bundle" pkg="${TMP_DIR}/pkg.tar.gz"
  mkdir -p "${inst}/scripts" "${inst}/Cubelet/config" "${bundle}"
  cat > "${inst}/.one-click.env" <<'EOF'
ONE_CLICK_DEPLOY_ROLE=control
EOF
  cat > "${inst}/VERSION.txt" <<'EOF'
release_version=v0.4.0
EOF
  cat > "${bundle}/VERSION.txt" <<'EOF'
release_version=v0.5.0
EOF
  printf 'fake-package' > "${pkg}"
  printf 'config' > "${inst}/Cubelet/config/config.toml"

  preflight_upgrade "${inst}" "${bundle}" "${pkg}" control 0 0 >/dev/null 2>&1 \
    || fail "preflight_upgrade should pass for matching role and upgrade version"

  if ( preflight_upgrade "${inst}" "${bundle}" "${pkg}" compute 0 0 ) >/dev/null 2>&1; then
    fail "preflight_upgrade should reject role change without allow flag"
  fi

  cat > "${bundle}/VERSION.txt" <<'EOF'
release_version=v0.3.0
EOF
  if ( preflight_upgrade "${inst}" "${bundle}" "${pkg}" control 0 0 ) >/dev/null 2>&1; then
    fail "preflight_upgrade should reject downgrade without allow flag"
  fi

  local backup_dir
  backup_dir="$(backup_before_upgrade "${inst}" 2>/dev/null)"
  [[ -f "${backup_dir}/.one-click.env" ]] || fail "backup should include .one-click.env"
  [[ -f "${backup_dir}/Cubelet/config/config.toml" ]] || fail "backup should include Cubelet config"
  local mode
  mode="$(stat -c '%a' "${backup_dir}" 2>/dev/null || echo "")"
  [[ "${mode}" == "700" ]] || fail "backup dir should be 700 (got ${mode})"
  mode="$(stat -c '%a' "${backup_dir}/.one-click.env" 2>/dev/null || echo "")"
  [[ "${mode}" == "600" ]] || fail ".one-click.env backup should be 600 (got ${mode})"

  local bad_inst="${TMP_DIR}/backup-symlink-inst" outside="${TMP_DIR}/backup-outside"
  mkdir -p "${bad_inst}" "${outside}"
  : > "${bad_inst}/.one-click.env"
  ln -s "${outside}" "${bad_inst}/.backup"
  if ( backup_before_upgrade "${bad_inst}" ) >/dev/null 2>&1; then
    fail "backup_before_upgrade should reject a symlink .backup directory"
  fi
  if compgen -G "${outside}/upgrade-*" >/dev/null; then
    fail "backup_before_upgrade must not write through a .backup symlink"
  fi
}

test_validation_library_fallback_die() {
  local err="${TMP_DIR}/validation-fallback.err"
  if (
    unset -f die 2>/dev/null || true
    unset ONE_CLICK_VALIDATION_LIB_LOADED
    # shellcheck source=../scripts/common/validation.sh
    source "${ONE_CLICK_DIR}/scripts/common/validation.sh"
    validate_host_port "bad/host:8089" "TEST_ADDR"
  ) >/dev/null 2>"${err}"; then
    fail "validation.sh fallback die should fail invalid input"
  fi
  assert_contains "${err}" "[validation] ERROR:"
  if grep -Fq "command not found" "${err}"; then
    fail "validation.sh fallback should not produce command-not-found"
  fi
}

test_install_sh_wires_upgrade_flow() {
  local f="${ONE_CLICK_DIR}/install.sh"
  assert_contains "${f}" "resolve_install_mode"
  assert_contains "${f}" "preflight_upgrade"
  assert_contains "${f}" "backup_before_upgrade"
  assert_contains "${f}" "merge_env_three_way"
  assert_contains "${f}" "patch_cubelet_config_template"
  # CLI parsing is delegated to one_click_parse_args (supports --mode/--node-ip
  # in both = and space forms) and CLI values are re-applied after .env load.
  assert_contains "${f}" 'one_click_parse_args "$@"'
  assert_contains "${f}" "apply_cli_overrides"
  # custom-prefix wipe is guarded against unsafe install prefixes
  assert_contains "${f}" 'wipe_custom_install_prefix_contents "${INSTALL_PREFIX}"'
  # env.example baseline is installed for future three-way merges
  assert_contains "${f}" 'cp -f "${SCRIPT_DIR}/env.example" "${INSTALL_PREFIX}/env.example"'
  # upgrade writes the merged env as the runtime env
  assert_contains "${f}" 'cp -f "${MERGED_ENV}" "${RUNTIME_ENV_FILE}"'
  # full-wipe branch delegates to the helper that preserves the upgrade backup.
  assert_contains "${ONE_CLICK_DIR}/lib/common.sh" "! -name '.backup'"
  # on upgrade, CIDR host-conflict detection is skipped (M2)
  assert_contains "${f}" 'check_cidr_preflight "${CUBE_SANDBOX_NETWORK_CIDR}" "${cidr_skip_conflict}" "CUBE_SANDBOX_NETWORK_CIDR"'
  assert_contains "${f}" 'check_cidr_preflight "192.168.0.0/18" "${cidr_skip_conflict}" "default CubeSandbox network CIDR"'
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
test_wipe_custom_install_prefix_contents
test_control_plane_validators
test_patch_cubelet_config_template_refuses_symlink
test_upgrade_preflight_and_backup
test_validation_library_fallback_die
test_install_sh_wires_upgrade_flow

echo "install mode tests OK"
