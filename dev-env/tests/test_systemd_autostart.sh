#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEV_ENV_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
AUTOSTART_SCRIPT="${DEV_ENV_DIR}/cube-autostart.sh"
PREPARE_SCRIPT="${DEV_ENV_DIR}/prepare_image.sh"
SYNC_SCRIPT="${DEV_ENV_DIR}/sync_to_vm.sh"
LEGACY_SETUP_SCRIPT="${DEV_ENV_DIR}/internal/setup_autostart.sh"

TMP_DIR="$(mktemp -d)"
FAKE_BIN="${TMP_DIR}/bin"
SSH_LOG="${TMP_DIR}/ssh.log"

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_contains() {
  local path="$1"
  local expected="$2"
  grep -Fq -- "${expected}" "${path}" \
    || fail "expected ${path} to contain: ${expected}"
}

assert_not_contains() {
  local path="$1"
  local unexpected="$2"
  if grep -Fq -- "${unexpected}" "${path}"; then
    fail "expected ${path} not to contain: ${unexpected}"
  fi
}

assert_before() {
  local path="$1"
  local first="$2"
  local second="$3"
  local first_line=""
  local second_line=""

  first_line="$(grep -nF -- "${first}" "${path}" | head -n 1 | cut -d: -f1 || true)"
  second_line="$(grep -nF -- "${second}" "${path}" | head -n 1 | cut -d: -f1 || true)"
  [[ -n "${first_line}" ]] || fail "missing first command in ${path}: ${first}"
  [[ -n "${second_line}" ]] || fail "missing second command in ${path}: ${second}"
  (( first_line < second_line )) \
    || fail "expected '${first}' before '${second}' in ${path}"
}

mkdir -p "${FAKE_BIN}"

cat >"${FAKE_BIN}/setsid" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "-w" ]]; then
  shift
fi
exec "$@"
EOF

cat >"${FAKE_BIN}/ssh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

remote_command="${!#}"
printf '%s\n' "${remote_command}" >> "${FAKE_SSH_LOG}"

case "${remote_command}" in
  *"systemctl cat 'cube-sandbox-control.target'"*)
    [[ "${FAKE_TARGET_EXISTS:-1}" == "1" ]]
    ;;
  *"systemctl cat 'cube-sandbox-oneclick.service'"*)
    [[ "${FAKE_LEGACY_EXISTS:-0}" == "1" ]]
    ;;
  *"systemctl is-enabled 'cube-sandbox-control.target'"*)
    if [[ "${FAKE_TARGET_ENABLED:-0}" == "1" ]]; then
      printf 'enabled\n'
      exit 0
    fi
    printf 'disabled\n'
    exit 1
    ;;
  *"systemctl is-enabled 'cube-sandbox-oneclick.service'"*)
    if [[ "${FAKE_LEGACY_ENABLED:-0}" == "1" ]]; then
      printf 'enabled\n'
      exit 0
    fi
    printf 'disabled\n'
    exit 1
    ;;
  *"systemctl is-active 'cube-sandbox-control.target'"*)
    if [[ "${FAKE_TARGET_ACTIVE:-0}" == "1" ]]; then
      printf 'active\n'
      exit 0
    fi
    printf 'inactive\n'
    exit 3
    ;;
  *"systemctl is-active 'cube-sandbox-oneclick.service'"*)
    if [[ "${FAKE_LEGACY_ACTIVE:-0}" == "1" ]]; then
      printf 'active\n'
      exit 0
    fi
    printf 'inactive\n'
    exit 3
    ;;
  *"systemctl is-failed 'cube-sandbox-oneclick.service'"*)
    if [[ "${FAKE_LEGACY_FAILED:-0}" == "1" ]]; then
      printf 'failed\n'
      exit 0
    fi
    printf 'inactive\n'
    exit 1
    ;;
  *"systemctl --no-pager --full status '"*)
    printf 'fake systemctl status\n'
    ;;
  *)
    ;;
esac
EOF

chmod +x "${FAKE_BIN}/setsid" "${FAKE_BIN}/ssh"

run_autostart() {
  local output_path="$1"
  local action="$2"
  shift 2

  : >"${SSH_LOG}"
  env \
    PATH="${FAKE_BIN}:${PATH}" \
    WORK_DIR="${TMP_DIR}/work" \
    FAKE_SSH_LOG="${SSH_LOG}" \
    ASSUME_YES=1 \
    "$@" \
    "${AUTOSTART_SCRIPT}" "${action}" >"${output_path}" 2>&1
}

test_enable_requires_official_target() {
  local output="${TMP_DIR}/missing-target.out"

  if run_autostart "${output}" enable \
    FAKE_TARGET_EXISTS=0 \
    FAKE_LEGACY_EXISTS=1; then
    fail "enable should fail when cube-sandbox-control.target is missing"
  fi

  assert_contains "${output}" "Unit cube-sandbox-control.target not found"
  assert_contains "${output}" "install CubeSandbox"
  assert_not_contains "${SSH_LOG}" "systemctl enable 'cube-sandbox-control.target'"
}

test_enable_migrates_active_legacy_unit() {
  local output="${TMP_DIR}/enable-migration.out"

  run_autostart "${output}" enable \
    FAKE_TARGET_EXISTS=1 \
    FAKE_TARGET_ENABLED=1 \
    FAKE_TARGET_ACTIVE=1 \
    FAKE_LEGACY_EXISTS=1 \
    FAKE_LEGACY_ENABLED=1 \
    FAKE_LEGACY_ACTIVE=1 \
    FAKE_LEGACY_FAILED=0

  assert_contains "${SSH_LOG}" "sudo systemctl disable 'cube-sandbox-oneclick.service'"
  assert_contains "${SSH_LOG}" "sudo systemctl stop 'cube-sandbox-control.target'"
  assert_contains "${SSH_LOG}" "sudo systemctl stop 'cube-sandbox-oneclick.service'"
  assert_contains "${SSH_LOG}" "sudo systemctl reset-failed 'cube-sandbox-oneclick.service'"
  assert_contains "${SSH_LOG}" "sudo systemctl enable 'cube-sandbox-control.target'"
  assert_contains "${SSH_LOG}" "sudo systemctl restart 'cube-sandbox-control.target'"
  assert_before "${SSH_LOG}" \
    "sudo systemctl stop 'cube-sandbox-control.target'" \
    "sudo systemctl stop 'cube-sandbox-oneclick.service'"
  assert_before "${SSH_LOG}" \
    "sudo systemctl reset-failed 'cube-sandbox-oneclick.service'" \
    "sudo systemctl restart 'cube-sandbox-control.target'"
}

test_enable_rejects_legacy_unit_override() {
  local output="${TMP_DIR}/legacy-override.out"

  if run_autostart "${output}" enable \
    UNIT_NAME=cube-sandbox-oneclick.service \
    FAKE_LEGACY_EXISTS=1; then
    fail "legacy UNIT_NAME override should be rejected"
  fi

  assert_contains "${output}" "deprecated"
  [[ ! -s "${SSH_LOG}" ]] || fail "legacy UNIT_NAME rejection should happen before SSH"
}

test_disable_without_stop_disables_both_units() {
  local output="${TMP_DIR}/disable-later.out"

  run_autostart "${output}" disable \
    STOP_NOW=0 \
    FAKE_TARGET_EXISTS=1 \
    FAKE_LEGACY_EXISTS=1 \
    FAKE_LEGACY_ACTIVE=1

  assert_contains "${SSH_LOG}" "sudo systemctl disable 'cube-sandbox-control.target'"
  assert_contains "${SSH_LOG}" "sudo systemctl disable 'cube-sandbox-oneclick.service'"
  assert_not_contains "${SSH_LOG}" "sudo systemctl stop 'cube-sandbox-control.target'"
  assert_not_contains "${SSH_LOG}" "sudo systemctl stop 'cube-sandbox-oneclick.service'"
}

test_disable_stops_target_before_active_legacy_unit() {
  local output="${TMP_DIR}/disable-now.out"

  run_autostart "${output}" disable \
    STOP_NOW=1 \
    FAKE_TARGET_EXISTS=1 \
    FAKE_TARGET_ACTIVE=1 \
    FAKE_LEGACY_EXISTS=1 \
    FAKE_LEGACY_ACTIVE=1

  assert_before "${SSH_LOG}" \
    "sudo systemctl stop 'cube-sandbox-control.target'" \
    "sudo systemctl stop 'cube-sandbox-oneclick.service'"
}

test_status_warns_about_unhealthy_legacy_unit() {
  local output="${TMP_DIR}/status.out"

  run_autostart "${output}" status \
    FAKE_TARGET_EXISTS=1 \
    FAKE_TARGET_ENABLED=1 \
    FAKE_TARGET_ACTIVE=1 \
    FAKE_LEGACY_EXISTS=1 \
    FAKE_LEGACY_ENABLED=1 \
    FAKE_LEGACY_ACTIVE=0 \
    FAKE_LEGACY_FAILED=1

  assert_contains "${output}" "is-enabled : enabled"
  assert_contains "${output}" "is-active  : active"
  assert_contains "${output}" "Legacy unit cube-sandbox-oneclick.service still needs cleanup"
  assert_contains "${output}" "enabled=enabled, active=inactive, failed=failed"
}

test_static_wiring_uses_official_target() {
  assert_not_contains "${PREPARE_SCRIPT}" "SETUP_AUTOSTART"
  assert_not_contains "${PREPARE_SCRIPT}" "setup_autostart.sh"
  [[ -f "${LEGACY_SETUP_SCRIPT}" ]] || fail "deprecated setup_autostart.sh should be retained"
  assert_contains "${SYNC_SCRIPT}" 'UNIT_NAME="${UNIT_NAME:-cube-sandbox-control.target}"'
}

test_enable_requires_official_target
echo "PASS: enable requires official target"
test_enable_migrates_active_legacy_unit
echo "PASS: enable migrates active legacy unit"
test_enable_rejects_legacy_unit_override
echo "PASS: enable rejects legacy UNIT_NAME override"
test_disable_without_stop_disables_both_units
echo "PASS: disable without stop disables both units"
test_disable_stops_target_before_active_legacy_unit
echo "PASS: disable stops target before legacy unit"
test_status_warns_about_unhealthy_legacy_unit
echo "PASS: status warns about unhealthy legacy unit"
test_static_wiring_uses_official_target
echo "PASS: static wiring uses official target"
