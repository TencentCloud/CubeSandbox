#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Install the CubeShim/CubeVMM logrotate policy and an hourly systemd timer.
# Without host-side rotation these shared logs grow without bound (see #1290).
#
# Failures here must not abort one-click install: log hygiene is important but
# the role target still needs to start. Missing packages are installed when a
# supported package manager is available; otherwise we warn and continue.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log() {
  echo "[one-click-logrotate] $*" >&2
}

die() {
  echo "[one-click-logrotate] ERROR: $*" >&2
  exit 1
}

# Escape for sed |…| substitutions (& and the delimiter).
escape_sed() {
  printf '%s' "$1" | sed -e 's/[|&\\]/\\&/g'
}

require_root() {
  if [[ "${EUID}" -eq 0 ]]; then
    return 0
  fi
  # Test-only: ALLOW_NONROOT is honored only when destinations are injected so
  # a bare flag cannot skip the root check and die later with Permission denied.
  if [[ "${ONE_CLICK_LOGROTATE_ALLOW_NONROOT:-0}" == "1" \
    && -n "${CUBE_SANDBOX_LOGROTATE_POLICY_DST:-}" \
    && -n "${ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR:-}" ]]; then
    return 0
  fi
  die "this script must run as root"
}

detect_pkg_manager() {
  if command -v apt-get >/dev/null 2>&1; then
    printf 'apt'
    return 0
  fi
  if command -v dnf >/dev/null 2>&1; then
    printf 'dnf'
    return 0
  fi
  if command -v yum >/dev/null 2>&1; then
    printf 'yum'
    return 0
  fi
  return 1
}

# Resolve an executable logrotate and print its absolute path, or return 1.
# ONE_CLICK_LOGROTATE_BIN overrides discovery (set to a non-executable path in
# tests to force the missing-binary branch).
resolve_logrotate() {
  local candidate
  if [[ -n "${ONE_CLICK_LOGROTATE_BIN:-}" ]]; then
    if [[ -x "${ONE_CLICK_LOGROTATE_BIN}" ]]; then
      printf '%s\n' "${ONE_CLICK_LOGROTATE_BIN}"
      return 0
    fi
    return 1
  fi
  if candidate="$(command -v logrotate 2>/dev/null)" && [[ -x "${candidate}" ]]; then
    printf '%s\n' "${candidate}"
    return 0
  fi
  if [[ -x /usr/sbin/logrotate ]]; then
    printf '%s\n' /usr/sbin/logrotate
    return 0
  fi
  if [[ -x /usr/bin/logrotate ]]; then
    printf '%s\n' /usr/bin/logrotate
    return 0
  fi
  return 1
}

ensure_logrotate_binary() {
  local pm
  if resolve_logrotate >/dev/null; then
    return 0
  fi

  # Explicit local/air-gapped hook first — SKIP_PKG_INSTALL only guards apt/dnf/yum.
  if [[ -n "${ONE_CLICK_LOGROTATE_INSTALLER:-}" ]]; then
    log "installing logrotate via ONE_CLICK_LOGROTATE_INSTALLER..."
    "${ONE_CLICK_LOGROTATE_INSTALLER}" || true
    resolve_logrotate >/dev/null && return 0
    log "WARNING: ONE_CLICK_LOGROTATE_INSTALLER ran but logrotate is still missing"
    return 1
  fi

  if [[ "${ONE_CLICK_LOGROTATE_SKIP_PKG_INSTALL:-0}" == "1" ]]; then
    log "WARNING: logrotate missing and ONE_CLICK_LOGROTATE_SKIP_PKG_INSTALL=1; skipping package install"
    return 1
  fi

  if ! pm="$(detect_pkg_manager)"; then
    log "WARNING: logrotate not found and no apt-get/dnf/yum available to install it"
    return 1
  fi

  log "installing logrotate via ${pm}..."
  case "${pm}" in
    apt)
      DEBIAN_FRONTEND=noninteractive apt-get update -qq \
        && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq logrotate \
        || { log "WARNING: apt-get could not install logrotate"; return 1; }
      ;;
    dnf)
      dnf install -y logrotate \
        || { log "WARNING: dnf could not install logrotate"; return 1; }
      ;;
    yum)
      yum install -y logrotate \
        || { log "WARNING: yum could not install logrotate"; return 1; }
      ;;
  esac

  if ! resolve_logrotate >/dev/null; then
    log "WARNING: attempted to install logrotate via ${pm} but it is still missing"
    return 1
  fi
  return 0
}

render_service_unit() {
  local src="$1"
  local dst="$2"
  local logrotate_bin="$3"
  local policy_path="$4"
  local state_path="${CUBE_SANDBOX_LOGROTATE_STATE:-/var/lib/cube-sandbox-logrotate/status}"
  local tmp esc_bin esc_policy esc_state
  tmp="$(mktemp)"
  esc_bin="$(escape_sed "${logrotate_bin}")"
  esc_policy="$(escape_sed "${policy_path}")"
  esc_state="$(escape_sed "${state_path}")"
  # Template is authoritative: substitute @TOKEN@ placeholders only.
  sed \
    -e "s|@LOGROTATE@|${esc_bin}|g" \
    -e "s|@STATE@|${esc_state}|g" \
    -e "s|@POLICY@|${esc_policy}|g" \
    "${src}" >"${tmp}"
  if grep -E '^[^#[:space:]].*@[A-Z0-9_]+@' "${tmp}" >/dev/null; then
    rm -f "${tmp}"
    die "unresolved @TOKEN@ left in rendered unit ${dst}"
  fi
  install -m 0644 "${tmp}" "${dst}"
  rm -f "${tmp}"
}


# After the new hourly timer is confirmed enabled, retire any pre-#1895 policy
# under /etc/logrotate.d so the distro daily job does not also rotate these
# paths. Back up rather than delete: that path was documented for hand installs.
retire_legacy_policy() {
  if [[ -z "${LEGACY_POLICY_DST}" || "${POLICY_DST}" == "${LEGACY_POLICY_DST}" ]]; then
    return 0
  fi
  if [[ ! -e "${LEGACY_POLICY_DST}" ]]; then
    return 0
  fi
  # Keep the backup outside the distro include: dotted .bak.* names are NOT in
  # logrotate's taboo suffix list, so a same-dir rename under /etc/logrotate.d
  # would keep rotating (and POLICY_DST is injectable, so dirname alone is not
  # enough when an operator points it into that include).
  local bak_dir
  bak_dir="$(dirname "${POLICY_DST}")"
  if [[ "${bak_dir}" == "/etc/logrotate.d" || "${bak_dir}" == */etc/logrotate.d ]]; then
    bak_dir="$(dirname "${bak_dir}")/cube-sandbox/logrotate.d"
  fi
  mkdir -p "${bak_dir}"
  local bak="${bak_dir}/legacy-cubesandbox.bak.$(date +%s)"
  log "WARNING: moving legacy logrotate policy ${LEGACY_POLICY_DST} -> ${bak}"
  if ! mv "${LEGACY_POLICY_DST}" "${bak}"; then
    log "WARNING: could not retire legacy policy ${LEGACY_POLICY_DST}"
    return 0
  fi
}

POLICY_SRC="${CUBE_SANDBOX_LOGROTATE_POLICY_SRC:-${SCRIPT_DIR}/cubesandbox}"
SERVICE_SRC="${CUBE_SANDBOX_LOGROTATE_SERVICE_SRC:-${SCRIPT_DIR}/cube-sandbox-logrotate.service}"
TIMER_SRC="${CUBE_SANDBOX_LOGROTATE_TIMER_SRC:-${SCRIPT_DIR}/cube-sandbox-logrotate.timer}"

POLICY_DST="${CUBE_SANDBOX_LOGROTATE_POLICY_DST:-/etc/cube-sandbox/logrotate.d/cubesandbox}"
# Pre-#1895 / docs path; injectable so tests never touch the real host file.
LEGACY_POLICY_DST="${CUBE_SANDBOX_LOGROTATE_LEGACY_DST:-/etc/logrotate.d/cubesandbox}"
UNIT_INSTALL_DIR="${ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR:-/etc/systemd/system}"
SERVICE_DST="${UNIT_INSTALL_DIR}/cube-sandbox-logrotate.service"
TIMER_DST="${UNIT_INSTALL_DIR}/cube-sandbox-logrotate.timer"

require_root

[[ -f "${POLICY_SRC}" ]] || die "logrotate policy not found: ${POLICY_SRC}"
[[ -f "${SERVICE_SRC}" ]] || die "logrotate service unit not found: ${SERVICE_SRC}"
[[ -f "${TIMER_SRC}" ]] || die "logrotate timer unit not found: ${TIMER_SRC}"

install -d -m 0755 "$(dirname "${POLICY_DST}")"
install -m 0644 "${POLICY_SRC}" "${POLICY_DST}"
log "installed ${POLICY_DST}"
if ! ensure_logrotate_binary; then
  log "WARNING: policy installed at ${POLICY_DST} but hourly rotation is inactive"
  log "         install the logrotate package, then re-run this installer so ExecStart"
  log "         is re-rendered with the resolved path (do not only enable the timer):"
  log "         bash ${SCRIPT_DIR}/install-logrotate.sh"
  # Still drop unit templates so a later re-run can enable the timer.
  install -d -m 0755 "${UNIT_INSTALL_DIR}"
  # Placeholder path only; operators must re-run after installing the package.
  render_service_unit "${SERVICE_SRC}" "${SERVICE_DST}" "/usr/sbin/logrotate" "${POLICY_DST}"
  install -m 0644 "${TIMER_SRC}" "${TIMER_DST}"
  # Keep reported state aligned: do not leave a previously-enabled timer firing
  # at a missing binary (or a stale ExecStart) while we claim rotation is inactive.
  if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now cube-sandbox-logrotate.timer >/dev/null 2>&1 || true
    systemctl reset-failed cube-sandbox-logrotate.service >/dev/null 2>&1 || true
    systemctl daemon-reload >/dev/null 2>&1 || true
  fi
  exit 0
fi

LOGROTATE_BIN="$(resolve_logrotate)"
install -d -m 0755 "${UNIT_INSTALL_DIR}"
render_service_unit "${SERVICE_SRC}" "${SERVICE_DST}" "${LOGROTATE_BIN}" "${POLICY_DST}"
install -m 0644 "${TIMER_SRC}" "${TIMER_DST}"
log "installed ${SERVICE_DST} (ExecStart=${LOGROTATE_BIN} --state /var/lib/cube-sandbox-logrotate/status ${POLICY_DST}) and ${TIMER_DST}"

# Debug-parse before enabling or retiring legacy: a rejected policy would leave
# the host with a failing hourly unit and no daily fallback (#1290).
if ! "${LOGROTATE_BIN}" -d "${POLICY_DST}" >/dev/null 2>&1; then
  log "WARNING: logrotate rejects ${POLICY_DST}; leaving timer disabled and legacy policy in place"
  log "         fix the policy (or CUBE_SANDBOX_LOGROTATE_POLICY_SRC), then re-run:"
  log "         bash ${SCRIPT_DIR}/install-logrotate.sh"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now cube-sandbox-logrotate.timer >/dev/null 2>&1 || true
    systemctl reset-failed cube-sandbox-logrotate.service >/dev/null 2>&1 || true
    systemctl daemon-reload >/dev/null 2>&1 || true
  fi
  exit 0
fi

if ! command -v systemctl >/dev/null 2>&1; then
  log "WARNING: systemctl not found; install ${POLICY_DST} manually on an hourly schedule"
  exit 0
fi

systemctl daemon-reload || log "WARNING: systemctl daemon-reload failed"
if ! systemctl enable --now cube-sandbox-logrotate.timer; then
  log "WARNING: could not enable cube-sandbox-logrotate.timer; run it by hand after fixing systemd"
  exit 0
fi
log "enabled cube-sandbox-logrotate.timer (hourly)"
retire_legacy_policy
