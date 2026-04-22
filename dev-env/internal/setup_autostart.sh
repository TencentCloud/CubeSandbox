#!/usr/bin/env bash

set -euo pipefail

LOG_TAG="setup_autostart"

if [[ -t 1 && -t 2 ]]; then
  LOG_COLOR_RESET=$'\033[0m'
  LOG_COLOR_INFO=$'\033[0;36m'
  LOG_COLOR_SUCCESS=$'\033[0;32m'
  LOG_COLOR_WARN=$'\033[0;33m'
  LOG_COLOR_ERROR=$'\033[0;31m'
else
  LOG_COLOR_RESET=""
  LOG_COLOR_INFO=""
  LOG_COLOR_SUCCESS=""
  LOG_COLOR_WARN=""
  LOG_COLOR_ERROR=""
fi

_log() {
  local color="$1"
  local level="$2"
  shift 2
  printf '%s[%s][%s]%s %s\n' \
    "${color}" "${LOG_TAG}" "${level}" "${LOG_COLOR_RESET}" "$*"
}

log_info()    { _log "${LOG_COLOR_INFO}"    "INFO"  "$@"; }
log_success() { _log "${LOG_COLOR_SUCCESS}" "OK"    "$@"; }
log_warn()    { _log "${LOG_COLOR_WARN}"    "WARN"  "$@" >&2; }
log_error()   { _log "${LOG_COLOR_ERROR}"   "ERROR" "$@" >&2; }

if [[ "${EUID}" -ne 0 ]]; then
  if command -v sudo >/dev/null 2>&1; then
    exec sudo bash "$0" "$@"
  fi

  log_error "This script must be run as root."
  exit 1
fi

# Install a systemd unit that brings up the Cube Sandbox one-click stack on
# boot by invoking the existing up-with-deps.sh / down-with-deps.sh helpers
# shipped with the one-click installer. The unit file is written and
# daemon-reload is performed, but the unit is intentionally NOT enabled
# here -- enabling is a separate, explicit action handled by
# dev-env/cube-autostart.sh on the host side.
#
# ConditionPathExists ensures the unit silently no-ops when the one-click
# scripts are not yet installed (e.g. before running online-install.sh
# inside the guest), so this script is safe to run on a fresh image.

TOOLBOX_ROOT="${TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
UP_SCRIPT="${TOOLBOX_ROOT}/scripts/one-click/up-with-deps.sh"
DOWN_SCRIPT="${TOOLBOX_ROOT}/scripts/one-click/down-with-deps.sh"
ENV_FILE="${TOOLBOX_ROOT}/.one-click.env"
UNIT_NAME="cube-sandbox-oneclick.service"
UNIT_PATH="/etc/systemd/system/${UNIT_NAME}"

if ! command -v systemctl >/dev/null 2>&1; then
  log_error "systemctl not found; cannot install systemd unit."
  exit 1
fi

log_info "Writing systemd unit: ${UNIT_PATH}"
cat >"${UNIT_PATH}" <<EOF
[Unit]
Description=Cube Sandbox one-click bring-up
After=docker.service network-online.target
Wants=docker.service network-online.target
ConditionPathExists=${UP_SCRIPT}

[Service]
Type=oneshot
RemainAfterExit=yes
EnvironmentFile=-${ENV_FILE}
ExecStart=${UP_SCRIPT}
ExecStop=${DOWN_SCRIPT}
TimeoutStartSec=600
TimeoutStopSec=120

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "${UNIT_PATH}"
log_success "Unit file written"

log_info "Reloading systemd manager configuration..."
systemctl daemon-reload
log_success "systemd daemon-reload done"

if systemctl is-enabled "${UNIT_NAME}" >/dev/null 2>&1; then
  log_info "${UNIT_NAME} is already enabled."
else
  log_info "${UNIT_NAME} is installed but NOT enabled."
  log_info "Run dev-env/cube-autostart.sh from the host to turn it on."
fi

log_success "Autostart unit setup finished"
