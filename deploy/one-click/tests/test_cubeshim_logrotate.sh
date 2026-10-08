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

assert_file "${POLICY}"
assert_contains "${POLICY}" "/data/log/CubeVmm/vmm.log /data/log/CubeShim/*.log"
assert_contains "${POLICY}" "hourly"
assert_contains "${POLICY}" "missingok"
grep -E '^[[:space:]]*copytruncate' "${POLICY}" >/dev/null && fail "copytruncate forbidden" || true

assert_file "${INSTALLER}"
[[ -x "${INSTALLER}" ]] || fail "installer not executable"
assert_contains "${SERVICE}" "ExecStart=@LOGROTATE@ --state /var/lib/cube-sandbox-logrotate/status @POLICY@"
assert_contains "${SERVICE}" "StateDirectory=cube-sandbox-logrotate"
assert_contains "${TIMER}" "OnCalendar=hourly"
assert_contains "${INSTALL_SH}" "install_cubeshim_logrotate"
assert_contains "${ONE_CLICK_DIR}/build-release-bundle.sh" 'scripts/logrotate'

# Dry-run install with stubs
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

PATH="${tmp}/bin:${PATH}" \
ONE_CLICK_LOGROTATE_ALLOW_NONROOT=1 \
CUBE_SANDBOX_LOGROTATE_POLICY_SRC="${POLICY}" \
CUBE_SANDBOX_LOGROTATE_SERVICE_SRC="${SERVICE}" \
CUBE_SANDBOX_LOGROTATE_TIMER_SRC="${TIMER}" \
CUBE_SANDBOX_LOGROTATE_POLICY_DST="${policy_dst}" \
CUBE_SANDBOX_LOGROTATE_LEGACY_DST="${legacy_dst}" \
ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR="${unit_dir}" \
  bash "${INSTALLER}"

[[ -f "${policy_dst}" ]] || fail "policy not installed"
[[ -f "${unit_dir}/cube-sandbox-logrotate.timer" ]] || fail "timer not installed"
grep -Fq "enable --now cube-sandbox-logrotate.timer" "${tmp}/systemctl.log" \
  || fail "timer not enabled"
[[ ! -e "${legacy_dst}" ]] || fail "legacy not retired"
ls "$(dirname "${policy_dst}")"/legacy-cubesandbox.bak.* >/dev/null \
  || fail "legacy bak missing"
rm -rf "${tmp}"

echo "cubeshim logrotate tests OK"
