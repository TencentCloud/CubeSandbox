#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# cube-autostart.sh — Manage the CubeSandbox one-click autostart systemd unit.
#
# The unit file (cube-sandbox-oneclick.service) is installed into the guest by
# prepare_image.sh but left disabled. This script is the user-facing entry
# point to enable / disable / inspect it, so that cube components restart
# automatically after a VM reboot.
#
# Subcommands:
#   enable     (default) Enable and start the unit; prompts for confirmation
#   disable    Stop and disable the unit
#   status     Print is-enabled / is-active and full systemctl status
#   -h|--help  Show usage
#
# Usage:
#   ./cube-autostart.sh                 # interactive enable
#   ./cube-autostart.sh disable
#   ./cube-autostart.sh status
#   ASSUME_YES=1 ./cube-autostart.sh    # skip confirmation
#
# Common environment variables:
#   VM_USER, VM_PASSWORD       Guest credentials (default: opencloudos / opencloudos)
#   SSH_HOST, SSH_PORT         Host-side forward target (default: 127.0.0.1:10022)
#   UNIT_NAME                  systemd unit name (default: cube-sandbox-oneclick.service)
#   ASSUME_YES                 Skip interactive confirmation when set to 1
#   STOP_NOW                   On disable, also stop the unit (default: 1)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="${WORK_DIR:-${SCRIPT_DIR}/.workdir}"

VM_USER="${VM_USER:-opencloudos}"
VM_PASSWORD="${VM_PASSWORD:-opencloudos}"
SSH_HOST="${SSH_HOST:-127.0.0.1}"
SSH_PORT="${SSH_PORT:-10022}"

UNIT_NAME="${UNIT_NAME:-cube-sandbox-oneclick.service}"
ASSUME_YES="${ASSUME_YES:-0}"
STOP_NOW="${STOP_NOW:-1}"

ASKPASS_SCRIPT="${WORK_DIR}/.ssh-askpass.sh"

LOG_TAG="cube-autostart"

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

usage() {
  cat <<EOF
Usage: $(basename "$0") [enable|disable|status]

Subcommands:
  enable   (default)  Enable and start ${UNIT_NAME} inside the guest, so
                      cube components come back automatically on every boot.
  disable             Disable the unit. By default also stops it now
                      (set STOP_NOW=0 to leave running services up).
  status              Show is-enabled / is-active and the latest status.

Environment overrides:
  VM_USER, VM_PASSWORD, SSH_HOST, SSH_PORT
  UNIT_NAME       default: ${UNIT_NAME}
  ASSUME_YES=1    skip the interactive confirmation
  STOP_NOW=0      disable: do not stop the unit now (leaves processes running)
EOF
}

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    log_error "Missing required command: $1"
    exit 1
  fi
}

confirm() {
  local prompt="$1"
  if [[ "${ASSUME_YES}" == "1" ]]; then
    return 0
  fi
  printf '\n%s [y/N] ' "${prompt}" >&2
  local reply=""
  read -r reply || reply=""
  case "${reply}" in
    y|Y|yes|YES) return 0 ;;
    *) return 1 ;;
  esac
}

ACTION="${1:-enable}"
case "${ACTION}" in
  enable|disable|status) ;;
  -h|--help|help) usage; exit 0 ;;
  *)
    log_error "Unknown subcommand: ${ACTION}"
    usage >&2
    exit 1
    ;;
esac

need_cmd ssh
need_cmd setsid

mkdir -p "${WORK_DIR}"

cat >"${ASKPASS_SCRIPT}" <<EOF
#!/usr/bin/env bash
printf '%s\n' '${VM_PASSWORD}'
EOF
chmod 700 "${ASKPASS_SCRIPT}"

cleanup() {
  rm -f "${ASKPASS_SCRIPT}"
}
trap cleanup EXIT

SSH_OPTS=(
  -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null
  -o PreferredAuthentications=password
  -o PubkeyAuthentication=no
  -p "${SSH_PORT}"
)

run_ssh() {
  DISPLAY="${DISPLAY:-cubesandbox-dev-env}" \
  SSH_ASKPASS="${ASKPASS_SCRIPT}" \
  SSH_ASKPASS_REQUIRE=force \
  setsid -w ssh "${SSH_OPTS[@]}" "${VM_USER}@${SSH_HOST}" "$@"
}

log_info "Target VM : ${VM_USER}@${SSH_HOST}:${SSH_PORT}"
log_info "Unit      : ${UNIT_NAME}"
log_info "Action    : ${ACTION}"

case "${ACTION}" in
  enable)
    if ! run_ssh "sudo systemctl cat '${UNIT_NAME}'" >/dev/null 2>&1; then
      log_error "Unit ${UNIT_NAME} not found inside the guest."
      log_error "Run prepare_image.sh (with SETUP_AUTOSTART=1, the default) first,"
      log_error "or manually run dev-env/internal/setup_autostart.sh inside the VM."
      exit 1
    fi

    if run_ssh "sudo systemctl is-enabled '${UNIT_NAME}'" >/dev/null 2>&1; then
      log_warn "${UNIT_NAME} is already enabled. Re-running enable --now is safe."
    fi

    if ! confirm "Enable ${UNIT_NAME} on boot? It will run up-with-deps.sh on every boot."; then
      log_warn "Aborted by user."
      exit 1
    fi

    log_info "Enabling and starting ${UNIT_NAME}..."
    run_ssh "sudo systemctl enable --now '${UNIT_NAME}'"
    log_success "${UNIT_NAME} enabled and started"

    log_info "Current status:"
    run_ssh "sudo systemctl --no-pager --full status '${UNIT_NAME}'" || true
    ;;

  disable)
    if ! run_ssh "sudo systemctl cat '${UNIT_NAME}'" >/dev/null 2>&1; then
      log_warn "Unit ${UNIT_NAME} not found inside the guest, nothing to disable."
      exit 0
    fi

    local_prompt="Disable ${UNIT_NAME}?"
    if [[ "${STOP_NOW}" == "1" ]]; then
      local_prompt+=" It will also stop the unit (running cube components will be torn down)."
    fi

    if ! confirm "${local_prompt}"; then
      log_warn "Aborted by user."
      exit 1
    fi

    if [[ "${STOP_NOW}" == "1" ]]; then
      log_info "Disabling and stopping ${UNIT_NAME}..."
      run_ssh "sudo systemctl disable --now '${UNIT_NAME}'"
    else
      log_info "Disabling ${UNIT_NAME} (leaving it running for now)..."
      run_ssh "sudo systemctl disable '${UNIT_NAME}'"
    fi
    log_success "${UNIT_NAME} disabled"
    ;;

  status)
    if ! run_ssh "sudo systemctl cat '${UNIT_NAME}'" >/dev/null 2>&1; then
      log_warn "Unit ${UNIT_NAME} not found inside the guest."
      exit 0
    fi

    enabled_state="$(run_ssh "sudo systemctl is-enabled '${UNIT_NAME}'" 2>/dev/null || true)"
    active_state="$(run_ssh "sudo systemctl is-active '${UNIT_NAME}'" 2>/dev/null || true)"
    log_info "is-enabled : ${enabled_state:-unknown}"
    log_info "is-active  : ${active_state:-unknown}"
    run_ssh "sudo systemctl --no-pager --full status '${UNIT_NAME}'" || true
    ;;
esac
