#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Install CubeShim/VMM logrotate policy + hourly timer (#1290 / #1292).
# Failures must not abort one-click install.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

log() { echo "[one-click-logrotate] $*" >&2; }
die() { echo "[one-click-logrotate] ERROR: $*" >&2; exit 1; }

escape_sed() { printf '%s' "$1" | sed -e 's/[|&\\]/\\&/g'; }

require_root() {
  [[ "${EUID}" -eq 0 || "${ONE_CLICK_LOGROTATE_ALLOW_NONROOT:-0}" == "1" ]] \
    || die "this script must run as root"
}

resolve_logrotate() {
  local c
  if [[ -n "${ONE_CLICK_LOGROTATE_BIN:-}" ]]; then
    [[ -x "${ONE_CLICK_LOGROTATE_BIN}" ]] || return 1
    printf '%s\n' "${ONE_CLICK_LOGROTATE_BIN}"
    return 0
  fi
  if c="$(command -v logrotate 2>/dev/null)" && [[ -x "${c}" ]]; then
    printf '%s\n' "${c}"; return 0
  fi
  [[ -x /usr/sbin/logrotate ]] && { printf '%s\n' /usr/sbin/logrotate; return 0; }
  [[ -x /usr/bin/logrotate ]] && { printf '%s\n' /usr/bin/logrotate; return 0; }
  return 1
}

ensure_logrotate() {
  resolve_logrotate >/dev/null && return 0
  [[ "${ONE_CLICK_LOGROTATE_SKIP_PKG_INSTALL:-0}" == "1" ]] && return 1
  if command -v apt-get >/dev/null 2>&1; then
    log "installing logrotate via apt-get..."
    DEBIAN_FRONTEND=noninteractive apt-get update -qq \
      && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq logrotate \
      || return 1
  elif command -v dnf >/dev/null 2>&1; then
    log "installing logrotate via dnf..."
    dnf install -y logrotate || return 1
  elif command -v yum >/dev/null 2>&1; then
    log "installing logrotate via yum..."
    yum install -y logrotate || return 1
  else
    return 1
  fi
  resolve_logrotate >/dev/null
}

render_service() {
  local src="$1" dst="$2" bin="$3" policy="$4" tmp
  tmp="$(mktemp)"
  sed \
    -e "s|@LOGROTATE@|$(escape_sed "${bin}")|g" \
    -e "s|@POLICY@|$(escape_sed "${policy}")|g" \
    "${src}" >"${tmp}"
  install -m 0644 "${tmp}" "${dst}"
  rm -f "${tmp}"
}

POLICY_SRC="${CUBE_SANDBOX_LOGROTATE_POLICY_SRC:-${SCRIPT_DIR}/cubesandbox}"
SERVICE_SRC="${CUBE_SANDBOX_LOGROTATE_SERVICE_SRC:-${SCRIPT_DIR}/cube-sandbox-logrotate.service}"
TIMER_SRC="${CUBE_SANDBOX_LOGROTATE_TIMER_SRC:-${SCRIPT_DIR}/cube-sandbox-logrotate.timer}"
POLICY_DST="${CUBE_SANDBOX_LOGROTATE_POLICY_DST:-/etc/cube-sandbox/logrotate.d/cubesandbox}"
LEGACY_DST="${CUBE_SANDBOX_LOGROTATE_LEGACY_DST:-/etc/logrotate.d/cubesandbox}"
UNIT_DIR="${ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR:-/etc/systemd/system}"

require_root
[[ -f "${POLICY_SRC}" && -f "${SERVICE_SRC}" && -f "${TIMER_SRC}" ]] \
  || die "logrotate sources missing under ${SCRIPT_DIR}"

install -d -m 0755 "$(dirname "${POLICY_DST}")"
install -m 0644 "${POLICY_SRC}" "${POLICY_DST}"
log "installed ${POLICY_DST}"

if ! ensure_logrotate; then
  log "WARNING: logrotate missing; policy staged but hourly rotation inactive"
  log "         install logrotate, then re-run: bash ${SCRIPT_DIR}/install-logrotate.sh"
  exit 0
fi

LOGROTATE_BIN="$(resolve_logrotate)"
install -d -m 0755 "${UNIT_DIR}"
render_service "${SERVICE_SRC}" "${UNIT_DIR}/cube-sandbox-logrotate.service" \
  "${LOGROTATE_BIN}" "${POLICY_DST}"
install -m 0644 "${TIMER_SRC}" "${UNIT_DIR}/cube-sandbox-logrotate.timer"
log "installed cube-sandbox-logrotate.service/timer (ExecStart=${LOGROTATE_BIN} ${POLICY_DST})"

if ! command -v systemctl >/dev/null 2>&1; then
  log "WARNING: systemctl not found; invoke logrotate on ${POLICY_DST} hourly by hand"
  exit 0
fi

systemctl daemon-reload || log "WARNING: daemon-reload failed"
if ! systemctl enable --now cube-sandbox-logrotate.timer; then
  log "WARNING: could not enable cube-sandbox-logrotate.timer"
  exit 0
fi
log "enabled cube-sandbox-logrotate.timer (hourly)"

# Retire pre-#1895 hand-install under /etc/logrotate.d (avoid double rotation).
if [[ -n "${LEGACY_DST}" && "${LEGACY_DST}" != "${POLICY_DST}" && -e "${LEGACY_DST}" ]]; then
  bak="$(dirname "${POLICY_DST}")/legacy-cubesandbox.bak.$(date +%s)"
  log "WARNING: moving legacy ${LEGACY_DST} -> ${bak}"
  mv "${LEGACY_DST}" "${bak}" || log "WARNING: could not retire ${LEGACY_DST}"
fi
