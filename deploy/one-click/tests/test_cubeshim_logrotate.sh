#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Static + injectable dry-run checks for the CubeShim/CubeVMM logrotate policy.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
LOGROTATE_DIR="${ONE_CLICK_DIR}/scripts/logrotate"
INSTALL_SH="${ONE_CLICK_DIR}/install.sh"
POLICY="${LOGROTATE_DIR}/cubesandbox"
INSTALLER="${LOGROTATE_DIR}/install-logrotate.sh"
SERVICE="${LOGROTATE_DIR}/cube-sandbox-logrotate.service"
TIMER="${LOGROTATE_DIR}/cube-sandbox-logrotate.timer"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_file() {
  [[ -f "$1" ]] || fail "missing file: $1"
}

assert_contains() {
  local file="$1"
  local needle="$2"
  grep -Fq -- "${needle}" "${file}" || fail "${file} missing: ${needle}"
}

test_policy_content() {
  assert_file "${POLICY}"
  assert_contains "${POLICY}" "/data/log/CubeVmm/vmm.log"
  assert_contains "${POLICY}" "/data/log/CubeShim/*.log"
  assert_contains "${POLICY}" "hourly"
  assert_contains "${POLICY}" "rotate 24"
  assert_contains "${POLICY}" "delaycompress"
  assert_contains "${POLICY}" "create 0640 root root"
  if grep -E '^[[:space:]]*copytruncate([[:space:]]|$)' "${POLICY}" >/dev/null; then
    fail "${POLICY} must not enable copytruncate"
  fi
  if command -v logrotate >/dev/null 2>&1; then
    logrotate -d "${POLICY}" >/dev/null 2>&1 || fail "logrotate rejects ${POLICY}"
  fi
}

test_units_and_installer() {
  assert_file "${INSTALLER}"
  assert_file "${SERVICE}"
  assert_file "${TIMER}"
  [[ -x "${INSTALLER}" ]] || fail "install-logrotate.sh must be executable"
  assert_contains "${SERVICE}" "ExecStart=@LOGROTATE@ --state @STATE@ @POLICY@"
  assert_contains "${SERVICE}" "StateDirectory=cube-sandbox-logrotate"
  assert_contains "${SERVICE}" "ConditionPathExists=@POLICY@"
  assert_contains "${TIMER}" "OnCalendar=hourly"
  assert_contains "${TIMER}" "cube-sandbox-logrotate.service"
  assert_contains "${INSTALLER}" "cube-sandbox-logrotate.timer"
  assert_contains "${INSTALLER}" "ONE_CLICK_LOGROTATE_ALLOW_NONROOT"
  assert_contains "${INSTALLER}" "ensure_logrotate_binary"
  assert_contains "${INSTALLER}" 'WARNING: could not enable cube-sandbox-logrotate.timer'
  assert_contains "${INSTALLER}" 'CUBE_SANDBOX_LOGROTATE_LEGACY_DST'
  assert_contains "${INSTALLER}" 'escape_sed'
  assert_contains "${INSTALLER}" 'ONE_CLICK_LOGROTATE_SKIP_PKG_INSTALL'
  assert_contains "${INSTALLER}" 'retire_legacy_policy'
  assert_contains "${ONE_CLICK_DIR}/env.example" 'ONE_CLICK_LOGROTATE_SKIP_PKG_INSTALL'
  assert_contains "${ONE_CLICK_DIR}/env.example" 'ONE_CLICK_LOGROTATE_INSTALLER'
  assert_contains "${ONE_CLICK_DIR}/env.example" 'ONE_CLICK_ENABLE_LOGROTATE'
  assert_contains "${INSTALL_SH}" 'ONE_CLICK_ENABLE_LOGROTATE'
}

test_install_sh_wiring() {
  assert_contains "${INSTALL_SH}" "install_cubeshim_logrotate"
  assert_contains "${INSTALL_SH}" 'scripts/logrotate/install-logrotate.sh'
  assert_contains "${INSTALL_SH}" "warn_inactive_cubeshim_logrotate"
  assert_contains "${INSTALL_SH}" 'cube-shim/vmm log rotation was not installed'
  local units_line rotate_line
  units_line="$(grep -n '^install_systemd_units$' "${INSTALL_SH}" | head -1 | cut -d: -f1)"
  rotate_line="$(grep -n '^install_cubeshim_logrotate$' "${INSTALL_SH}" | head -1 | cut -d: -f1)"
  [[ -n "${units_line}" && -n "${rotate_line}" ]] \
    || fail "install.sh must call install_systemd_units and install_cubeshim_logrotate"
  (( rotate_line > units_line )) \
    || fail "install_cubeshim_logrotate must run after install_systemd_units"
}

test_bundle_ships_logrotate() {
  local bundle_sh="${ONE_CLICK_DIR}/build-release-bundle.sh"
  assert_file "${bundle_sh}"
  assert_contains "${bundle_sh}" 'scripts/logrotate'
  assert_contains "${bundle_sh}" 'copy_dir_contents "${SCRIPT_DIR}/scripts/logrotate"'
}

# Exercise install-logrotate.sh without root by injecting destinations + stubs.
test_dry_run_install() {
  local tmp policy_dst unit_dir systemctl_log fake_logrotate
  tmp="$(mktemp -d)"
  policy_dst="${tmp}/etc/logrotate.d/cubesandbox"
  unit_dir="${tmp}/etc/systemd/system"
  systemctl_log="${tmp}/systemctl.log"
  fake_logrotate="${tmp}/bin/logrotate"
  mkdir -p "$(dirname "${policy_dst}")" "${unit_dir}" "${tmp}/bin"

  cat > "${fake_logrotate}" <<'EOF'
#!/bin/sh
exit 0
EOF
  cat > "${tmp}/bin/systemctl" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "${systemctl_log}"
exit 0
EOF
  chmod +x "${fake_logrotate}" "${tmp}/bin/systemctl"

  # Stub package install path: binary already on PATH via fake.
  PATH="${tmp}/bin:${PATH}" \
  ONE_CLICK_LOGROTATE_ALLOW_NONROOT=1 \
  CUBE_SANDBOX_LOGROTATE_POLICY_SRC="${POLICY}" \
  CUBE_SANDBOX_LOGROTATE_SERVICE_SRC="${SERVICE}" \
  CUBE_SANDBOX_LOGROTATE_TIMER_SRC="${TIMER}" \
  CUBE_SANDBOX_LOGROTATE_POLICY_DST="${policy_dst}" \
  CUBE_SANDBOX_LOGROTATE_LEGACY_DST="${tmp}/legacy/cubesandbox" \
  ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR="${unit_dir}" \
    bash "${INSTALLER}"

  [[ -f "${policy_dst}" ]] || fail "dry-run did not install policy"
  [[ -f "${unit_dir}/cube-sandbox-logrotate.service" ]] || fail "dry-run did not install service"
  [[ -f "${unit_dir}/cube-sandbox-logrotate.timer" ]] || fail "dry-run did not install timer"
  grep -Fq "ExecStart=${fake_logrotate} --state /var/lib/cube-sandbox-logrotate/status ${policy_dst}" \
    "${unit_dir}/cube-sandbox-logrotate.service" \
    || fail "service unit did not render resolved logrotate path"
  grep -Fq "ConditionPathExists=${policy_dst}" \
    "${unit_dir}/cube-sandbox-logrotate.service" \
    || fail "service unit did not render ConditionPathExists for policy"
  grep -Fq "enable --now cube-sandbox-logrotate.timer" "${systemctl_log}" \
    || fail "systemctl enable --now was not requested (got: $(cat "${systemctl_log}" 2>/dev/null || true))"
  if grep -E '^[[:space:]]*copytruncate([[:space:]]|$)' "${policy_dst}" >/dev/null; then
    fail "installed policy must not enable copytruncate"
  fi
  rm -rf "${tmp}"
}

# Missing logrotate + stub installer that fails → policy laid down, exit 0, no enable.
test_missing_logrotate_warns() {
  local tmp policy_dst unit_dir systemctl_log fail_install
  tmp="$(mktemp -d)"
  policy_dst="${tmp}/etc/logrotate.d/cubesandbox"
  unit_dir="${tmp}/etc/systemd/system"
  systemctl_log="${tmp}/systemctl.log"
  fail_install="${tmp}/bin/fail-install"
  mkdir -p "$(dirname "${policy_dst}")" "${unit_dir}" "${tmp}/bin"

  cat > "${tmp}/bin/systemctl" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "${systemctl_log}"
exit 0
EOF
  cat > "${fail_install}" <<'EOF'
#!/bin/sh
exit 1
EOF
  chmod +x "${tmp}/bin/systemctl" "${fail_install}"

  # Force resolve_logrotate to miss via ONE_CLICK_LOGROTATE_BIN; keep /usr/bin
  # for install/mktemp helpers.
  PATH="${tmp}/bin:/usr/bin:/bin" \
  ONE_CLICK_LOGROTATE_ALLOW_NONROOT=1 \
  ONE_CLICK_LOGROTATE_BIN="${tmp}/missing-logrotate" \
  ONE_CLICK_LOGROTATE_INSTALLER="${fail_install}" \
  CUBE_SANDBOX_LOGROTATE_POLICY_SRC="${POLICY}" \
  CUBE_SANDBOX_LOGROTATE_SERVICE_SRC="${SERVICE}" \
  CUBE_SANDBOX_LOGROTATE_TIMER_SRC="${TIMER}" \
  CUBE_SANDBOX_LOGROTATE_POLICY_DST="${policy_dst}" \
  CUBE_SANDBOX_LOGROTATE_LEGACY_DST="${tmp}/legacy/cubesandbox" \
  ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR="${unit_dir}" \
    bash "${INSTALLER}" || fail "missing logrotate must exit 0"

  [[ -f "${policy_dst}" ]] || fail "missing-logrotate path must still install policy"
  [[ -f "${unit_dir}/cube-sandbox-logrotate.timer" ]] || fail "units should still be staged"
  if [[ -f "${systemctl_log}" ]] && grep -q 'enable --now' "${systemctl_log}"; then
    fail "timer must not be enabled when logrotate is missing"
  fi
  grep -Fq "disable --now cube-sandbox-logrotate.timer" "${systemctl_log}" \
    || fail "missing logrotate path must disable any previously enabled timer"
  rm -rf "${tmp}"
}

# systemctl enable failure must not abort (exit 0).
test_enable_failure_is_soft() {
  local tmp policy_dst unit_dir fake_logrotate
  tmp="$(mktemp -d)"
  policy_dst="${tmp}/etc/logrotate.d/cubesandbox"
  unit_dir="${tmp}/etc/systemd/system"
  fake_logrotate="${tmp}/bin/logrotate"
  mkdir -p "$(dirname "${policy_dst}")" "${unit_dir}" "${tmp}/bin"

  cat > "${fake_logrotate}" <<'EOF'
#!/bin/sh
exit 0
EOF
  cat > "${tmp}/bin/systemctl" <<'EOF'
#!/bin/sh
# daemon-reload ok; enable fails
case "$*" in
  *enable*) exit 1 ;;
  *) exit 0 ;;
esac
EOF
  chmod +x "${fake_logrotate}" "${tmp}/bin/systemctl"

  PATH="${tmp}/bin:${PATH}" \
  ONE_CLICK_LOGROTATE_ALLOW_NONROOT=1 \
  CUBE_SANDBOX_LOGROTATE_POLICY_SRC="${POLICY}" \
  CUBE_SANDBOX_LOGROTATE_SERVICE_SRC="${SERVICE}" \
  CUBE_SANDBOX_LOGROTATE_TIMER_SRC="${TIMER}" \
  CUBE_SANDBOX_LOGROTATE_POLICY_DST="${policy_dst}" \
  CUBE_SANDBOX_LOGROTATE_LEGACY_DST="${tmp}/legacy/cubesandbox" \
  ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR="${unit_dir}" \
    bash "${INSTALLER}" || fail "enable failure must not abort installer"

  rm -rf "${tmp}"
}


test_legacy_policy_retired_after_enable() {
  local tmp policy_dst unit_dir legacy_dst fake_logrotate systemctl_log
  tmp="$(mktemp -d)"
  policy_dst="${tmp}/etc/cube-sandbox/logrotate.d/cubesandbox"
  legacy_dst="${tmp}/etc/logrotate.d/cubesandbox"
  unit_dir="${tmp}/etc/systemd/system"
  systemctl_log="${tmp}/systemctl.log"
  fake_logrotate="${tmp}/bin/logrotate"
  mkdir -p "$(dirname "${policy_dst}")" "$(dirname "${legacy_dst}")" "${unit_dir}" "${tmp}/bin"
  echo 'legacy' > "${legacy_dst}"

  cat > "${fake_logrotate}" <<'EOF'
#!/bin/sh
exit 0
EOF
  cat > "${tmp}/bin/systemctl" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "${systemctl_log}"
exit 0
EOF
  chmod +x "${fake_logrotate}" "${tmp}/bin/systemctl"

  PATH="${tmp}/bin:${PATH}" \
  ONE_CLICK_LOGROTATE_ALLOW_NONROOT=1 \
  CUBE_SANDBOX_LOGROTATE_POLICY_SRC="${POLICY}" \
  CUBE_SANDBOX_LOGROTATE_SERVICE_SRC="${SERVICE}" \
  CUBE_SANDBOX_LOGROTATE_TIMER_SRC="${TIMER}" \
  CUBE_SANDBOX_LOGROTATE_POLICY_DST="${policy_dst}" \
  CUBE_SANDBOX_LOGROTATE_LEGACY_DST="${legacy_dst}" \
  ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR="${unit_dir}" \
    bash "${INSTALLER}"

  [[ ! -e "${legacy_dst}" ]] || fail "legacy policy should be retired after enable"
  ls "$(dirname "${policy_dst}")"/legacy-cubesandbox.bak.* >/dev/null 2>&1 \
    || fail "legacy policy should be moved beside the new policy as legacy-cubesandbox.bak.*"
  rm -rf "${tmp}"
}

test_legacy_same_as_policy_is_kept() {
  local tmp policy_dst unit_dir fake_logrotate
  tmp="$(mktemp -d)"
  policy_dst="${tmp}/etc/cube-sandbox/logrotate.d/cubesandbox"
  unit_dir="${tmp}/etc/systemd/system"
  fake_logrotate="${tmp}/bin/logrotate"
  mkdir -p "$(dirname "${policy_dst}")" "${unit_dir}" "${tmp}/bin"

  cat > "${fake_logrotate}" <<'EOF'
#!/bin/sh
exit 0
EOF
  cat > "${tmp}/bin/systemctl" <<'EOF'
#!/bin/sh
exit 0
EOF
  chmod +x "${fake_logrotate}" "${tmp}/bin/systemctl"

  PATH="${tmp}/bin:${PATH}" \
  ONE_CLICK_LOGROTATE_ALLOW_NONROOT=1 \
  CUBE_SANDBOX_LOGROTATE_POLICY_SRC="${POLICY}" \
  CUBE_SANDBOX_LOGROTATE_SERVICE_SRC="${SERVICE}" \
  CUBE_SANDBOX_LOGROTATE_TIMER_SRC="${TIMER}" \
  CUBE_SANDBOX_LOGROTATE_POLICY_DST="${policy_dst}" \
  CUBE_SANDBOX_LOGROTATE_LEGACY_DST="${policy_dst}" \
  ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR="${unit_dir}" \
    bash "${INSTALLER}"

  [[ -f "${policy_dst}" ]] || fail "policy must remain when LEGACY_DST equals POLICY_DST"
  rm -rf "${tmp}"
}


test_legacy_bak_escapes_logrotate_d() {
  local tmp policy_dst unit_dir legacy_dst fake_logrotate
  tmp="$(mktemp -d)"
  # Operator override: policy still under the distro include path.
  policy_dst="${tmp}/etc/logrotate.d/cubesandbox"
  legacy_dst="${tmp}/etc/logrotate.d/old-cubesandbox"
  unit_dir="${tmp}/etc/systemd/system"
  fake_logrotate="${tmp}/bin/logrotate"
  mkdir -p "$(dirname "${policy_dst}")" "${unit_dir}" "${tmp}/bin"
  echo 'legacy' > "${legacy_dst}"

  cat > "${fake_logrotate}" <<'EOF'
#!/bin/sh
exit 0
EOF
  cat > "${tmp}/bin/systemctl" <<'EOF'
#!/bin/sh
exit 0
EOF
  chmod +x "${fake_logrotate}" "${tmp}/bin/systemctl"

  PATH="${tmp}/bin:${PATH}" \
  ONE_CLICK_LOGROTATE_ALLOW_NONROOT=1 \
  CUBE_SANDBOX_LOGROTATE_POLICY_SRC="${POLICY}" \
  CUBE_SANDBOX_LOGROTATE_SERVICE_SRC="${SERVICE}" \
  CUBE_SANDBOX_LOGROTATE_TIMER_SRC="${TIMER}" \
  CUBE_SANDBOX_LOGROTATE_POLICY_DST="${policy_dst}" \
  CUBE_SANDBOX_LOGROTATE_LEGACY_DST="${legacy_dst}" \
  ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR="${unit_dir}" \
    bash "${INSTALLER}"

  [[ ! -e "${legacy_dst}" ]] || fail "legacy policy should be retired"
  ls "${tmp}/etc/logrotate.d"/legacy-cubesandbox.bak.* >/dev/null 2>&1 \
    && fail "legacy bak must not remain under the distro logrotate.d include"
  ls "${tmp}/etc/cube-sandbox/logrotate.d"/legacy-cubesandbox.bak.* >/dev/null 2>&1 \
    || fail "legacy bak should land under etc/cube-sandbox/logrotate.d"
  rm -rf "${tmp}"
}


test_rejected_policy_keeps_legacy() {
  local tmp policy_dst unit_dir legacy_dst fake_logrotate systemctl_log
  tmp="$(mktemp -d)"
  policy_dst="${tmp}/etc/cube-sandbox/logrotate.d/cubesandbox"
  legacy_dst="${tmp}/etc/logrotate.d/cubesandbox"
  unit_dir="${tmp}/etc/systemd/system"
  systemctl_log="${tmp}/systemctl.log"
  fake_logrotate="${tmp}/bin/logrotate"
  mkdir -p "$(dirname "${policy_dst}")" "$(dirname "${legacy_dst}")" "${unit_dir}" "${tmp}/bin"
  echo 'legacy' > "${legacy_dst}"

  cat > "${fake_logrotate}" <<'EOF'
#!/bin/sh
# Pretend parse failure for -d and any other invocation.
exit 1
EOF
  cat > "${tmp}/bin/systemctl" <<EOF
#!/bin/sh
printf '%s\n' "\$*" >> "${systemctl_log}"
exit 0
EOF
  chmod +x "${fake_logrotate}" "${tmp}/bin/systemctl"

  PATH="${tmp}/bin:${PATH}" \
  ONE_CLICK_LOGROTATE_ALLOW_NONROOT=1 \
  CUBE_SANDBOX_LOGROTATE_POLICY_SRC="${POLICY}" \
  CUBE_SANDBOX_LOGROTATE_SERVICE_SRC="${SERVICE}" \
  CUBE_SANDBOX_LOGROTATE_TIMER_SRC="${TIMER}" \
  CUBE_SANDBOX_LOGROTATE_POLICY_DST="${policy_dst}" \
  CUBE_SANDBOX_LOGROTATE_LEGACY_DST="${legacy_dst}" \
  ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR="${unit_dir}" \
    bash "${INSTALLER}" || fail "rejected policy must exit 0"

  [[ -e "${legacy_dst}" ]] || fail "legacy policy must remain when logrotate -d rejects"
  if grep -Fq "enable --now cube-sandbox-logrotate.timer" "${systemctl_log}" 2>/dev/null; then
    fail "timer must not be enabled when policy is rejected"
  fi
  grep -Fq "disable --now cube-sandbox-logrotate.timer" "${systemctl_log}" \
    || fail "rejected policy path must disable timer"
  rm -rf "${tmp}"
}

test_policy_content
test_units_and_installer
test_install_sh_wiring
test_bundle_ships_logrotate
test_dry_run_install
test_missing_logrotate_warns
test_enable_failure_is_soft
test_legacy_policy_retired_after_enable
test_legacy_same_as_policy_is_kept
test_legacy_bak_escapes_logrotate_d
test_rejected_policy_keeps_legacy

echo "cubeshim logrotate tests OK"
