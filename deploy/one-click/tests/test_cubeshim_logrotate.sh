#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

ONE_CLICK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOGROTATE_DIR="${ONE_CLICK_DIR}/scripts/logrotate"
POLICY="${LOGROTATE_DIR}/cubesandbox"
INSTALLER="${LOGROTATE_DIR}/install-logrotate.sh"
SERVICE="${LOGROTATE_DIR}/cube-sandbox-logrotate.service"
TIMER="${LOGROTATE_DIR}/cube-sandbox-logrotate.timer"
INSTALL_SH="${ONE_CLICK_DIR}/install.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }
assert_file() { [[ -f "$1" ]] || fail "missing $1"; }
assert_contains() { grep -Fq "$2" "$1" || fail "$1 missing: $2"; }
assert_not_contains() { if grep -Fq "$2" "$1"; then fail "$1 unexpectedly contains: $2"; fi; }

assert_file "${POLICY}"
assert_contains "${POLICY}" "/data/log/CubeVmm/vmm.log /data/log/CubeShim/*.log"
assert_contains "${POLICY}" "su root root"
assert_contains "${POLICY}" "hourly"
assert_contains "${POLICY}" "rotate 24"
assert_contains "${POLICY}" "missingok"
assert_contains "${POLICY}" "notifempty"
assert_contains "${POLICY}" "compress"
assert_contains "${POLICY}" "delaycompress"
assert_contains "${POLICY}" "create 0640 root root"
grep -E '^[[:space:]]*copytruncate' "${POLICY}" >/dev/null && fail "copytruncate forbidden" || true

assert_file "${INSTALLER}"
[[ -x "${INSTALLER}" ]] || fail "installer not executable"
assert_contains "${SERVICE}" "ExecStart=@LOGROTATE@ --state /var/lib/cube-sandbox-logrotate/status @POLICY@"
assert_contains "${SERVICE}" "StateDirectory=cube-sandbox-logrotate"
assert_contains "${TIMER}" "OnCalendar=hourly"
assert_contains "${INSTALL_SH}" "install_cubeshim_logrotate"
assert_contains "${ONE_CLICK_DIR}/build-release-bundle.sh" 'scripts/logrotate'

run_installer() {
  local bin_dir="$1" policy_dst="$2" legacy_dst="$3" unit_dir="$4" output_file="$5"
  PATH="${bin_dir}:${PATH}" \
  ONE_CLICK_LOGROTATE_ALLOW_NONROOT=1 \
  CUBE_SANDBOX_LOGROTATE_POLICY_SRC="${POLICY}" \
  CUBE_SANDBOX_LOGROTATE_SERVICE_SRC="${SERVICE}" \
  CUBE_SANDBOX_LOGROTATE_TIMER_SRC="${TIMER}" \
  CUBE_SANDBOX_LOGROTATE_POLICY_DST="${policy_dst}" \
  CUBE_SANDBOX_LOGROTATE_LEGACY_DST="${legacy_dst}" \
  ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR="${unit_dir}" \
    bash "${INSTALLER}" >"${output_file}" 2>&1
}

# --- success path ---------------------------------------------------------
tmp="$(mktemp -d)"
policy_dst="${tmp}/etc/cube-sandbox/logrotate.d/cubesandbox"
legacy_dst="${tmp}/etc/logrotate.d/cubesandbox"
unit_dir="${tmp}/etc/systemd/system"
mkdir -p "$(dirname "${policy_dst}")" "$(dirname "${legacy_dst}")" "${unit_dir}" "${tmp}/bin"
echo legacy > "${legacy_dst}"
cat > "${tmp}/bin/logrotate" <<'E'
#!/bin/sh
exit 0
E
cat > "${tmp}/bin/systemctl" <<E
#!/bin/sh
printf '%s\n' "\$*" >> "${tmp}/systemctl.log"
exit 0
E
chmod +x "${tmp}/bin/logrotate" "${tmp}/bin/systemctl"

run_installer "${tmp}/bin" "${policy_dst}" "${legacy_dst}" "${unit_dir}" "${tmp}/install.log" \
  || { cat "${tmp}/install.log" >&2; fail "success-path installer failed"; }

[[ -f "${policy_dst}" ]] || fail "policy not installed"
assert_contains "${policy_dst}" "su root root"
[[ -f "${unit_dir}/cube-sandbox-logrotate.timer" ]] || fail "timer not installed"
assert_contains "${unit_dir}/cube-sandbox-logrotate.service" \
  "ExecStart=${tmp}/bin/logrotate --state /var/lib/cube-sandbox-logrotate/status ${policy_dst}"
assert_not_contains "${unit_dir}/cube-sandbox-logrotate.service" "@LOGROTATE@"
assert_not_contains "${unit_dir}/cube-sandbox-logrotate.service" "@POLICY@"
grep -Fq "enable --now cube-sandbox-logrotate.timer" "${tmp}/systemctl.log" \
  || fail "timer not enabled"
grep -Fq "enabled cube-sandbox-logrotate.timer" "${tmp}/install.log" \
  || fail "installer did not report the enabled timer"
[[ ! -e "${legacy_dst}" ]] || fail "legacy not retired"
bak_parent="$(dirname "$(dirname "${policy_dst}")")"
ls "${bak_parent}"/legacy-cubesandbox.bak.* >/dev/null \
  || fail "legacy bak missing from policy parent"
if compgen -G "$(dirname "${policy_dst}")/legacy-cubesandbox.bak.*" >/dev/null; then
  fail "legacy bak must not live inside the policy directory"
fi

# --- rejected policy: hard failure ---------------------------------------
tmp2="$(mktemp -d)"
policy_dst2="${tmp2}/etc/cube-sandbox/logrotate.d/cubesandbox"
legacy_dst2="${tmp2}/etc/logrotate.d/cubesandbox"
unit_dir2="${tmp2}/etc/systemd/system"
mkdir -p "$(dirname "${policy_dst2}")" "$(dirname "${legacy_dst2}")" "${unit_dir2}" "${tmp2}/bin"
echo legacy > "${legacy_dst2}"
cat > "${tmp2}/bin/logrotate" <<'E'
#!/bin/sh
echo "error: simulated parse failure" >&2
exit 1
E
cat > "${tmp2}/bin/systemctl" <<E
#!/bin/sh
printf '%s\n' "\$*" >> "${tmp2}/systemctl.log"
exit 0
E
chmod +x "${tmp2}/bin/logrotate" "${tmp2}/bin/systemctl"

if ! run_installer "${tmp2}/bin" "${policy_dst2}" "${legacy_dst2}" "${unit_dir2}" "${tmp2}/install.log"; then
  cat "${tmp2}/install.log" >&2
  fail "installer must soft-fail (exit 0) when logrotate rejects the policy"
fi
grep -Fq "WARNING: logrotate rejected" "${tmp2}/install.log" \
  || fail "missing rejection warning (hard failure)"
[[ -f "${policy_dst2}" ]] || fail "rejected policy should remain staged for diagnosis"
[[ ! -e "${unit_dir2}/cube-sandbox-logrotate.timer" ]] || fail "timer must not be installed on rejection"
[[ -e "${legacy_dst2}" ]] || fail "legacy must not be retired on rejection"
[[ ! -e "${tmp2}/systemctl.log" ]] || fail "systemctl must not be invoked on rejection"

# --- rejected policy: unknown directive with exit 0 ----------------------
tmp3="$(mktemp -d)"
policy_dst3="${tmp3}/etc/cube-sandbox/logrotate.d/cubesandbox"
legacy_dst3="${tmp3}/etc/logrotate.d/cubesandbox"
unit_dir3="${tmp3}/etc/systemd/system"
mkdir -p "$(dirname "${policy_dst3}")" "$(dirname "${legacy_dst3}")" "${unit_dir3}" "${tmp3}/bin"
echo legacy > "${legacy_dst3}"
cat > "${tmp3}/bin/logrotate" <<'E'
#!/bin/sh
echo "error: /tmp/policy:3 unknown option 'bogus' -- ignoring line" >&2
exit 0
E
cat > "${tmp3}/bin/systemctl" <<E
#!/bin/sh
printf '%s\n' "\$*" >> "${tmp3}/systemctl.log"
exit 0
E
chmod +x "${tmp3}/bin/logrotate" "${tmp3}/bin/systemctl"

if ! run_installer "${tmp3}/bin" "${policy_dst3}" "${legacy_dst3}" "${unit_dir3}" "${tmp3}/install.log"; then
  cat "${tmp3}/install.log" >&2
  fail "installer must soft-fail (exit 0) on an unknown directive"
fi
grep -Fq "WARNING: logrotate rejected" "${tmp3}/install.log" \
  || fail "unknown directive was not rejected"
[[ ! -e "${unit_dir3}/cube-sandbox-logrotate.timer" ]] || fail "timer must not be installed for an unknown directive"
[[ -e "${legacy_dst3}" ]] || fail "legacy must not be retired for an unknown directive"
[[ ! -e "${tmp3}/systemctl.log" ]] || fail "systemctl must not be invoked for an unknown directive"

# --- real logrotate parse (when available) ------------------------------
if command -v logrotate >/dev/null 2>&1; then
  parse_dir="$(mktemp -d)"
  parse_out="${parse_dir}/out"
  if [[ "${EUID}" -eq 0 ]]; then
    logrotate --debug --state "${parse_dir}/status" "${policy_dst}" >"${parse_out}" 2>&1 \
      || { cat "${parse_out}" >&2; fail "real logrotate rejected the installed policy"; }
  elif sudo -n true >/dev/null 2>&1; then
    sudo -n "$(command -v logrotate)" --debug --state "${parse_dir}/status" "${policy_dst}" >"${parse_out}" 2>&1 \
      || { cat "${parse_out}" >&2; fail "real logrotate rejected the installed policy (sudo)"; }
  else
    sed '/^[[:space:]]*su[[:space:]]/d' "${policy_dst}" >"${parse_dir}/policy-nosu"
    logrotate --debug --state "${parse_dir}/status" "${parse_dir}/policy-nosu" >"${parse_out}" 2>&1 \
      || { cat "${parse_out}" >&2; fail "real logrotate rejected the installed policy (without su)"; }
    echo "SKIP: su root root privilege-switch check requires root; run as root to validate it" >&2
  fi
  if grep -qE 'unknown option|error:' "${parse_out}"; then
    cat "${parse_out}" >&2
    fail "real logrotate reported errors for the installed policy"
  fi
  rm -rf "${parse_dir}"
fi

rm -rf "${tmp}" "${tmp2}" "${tmp3}"
echo "cubeshim logrotate tests OK"
