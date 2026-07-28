#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# cube-autostart.sh — Manage CubeSandbox's systemd target inside the dev VM.
#
# The one-click installer owns cube-sandbox-control.target and its component
# units. This script is the host-side entry point for enabling, disabling, and
# inspecting that target. It also disables the legacy dev-env unit when found.
#
# Subcommands:
#   enable     (default) Enable and restart the target; prompts for confirmation
#   disable    Stop and disable the target
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
#   UNIT_NAME                  systemd target name (default: cube-sandbox-control.target)
#   ASSUME_YES                 Skip interactive confirmation when set to 1
#   STOP_NOW                   On disable, also stop the unit (default: 1)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="${WORK_DIR:-${SCRIPT_DIR}/.workdir}"

VM_USER="${VM_USER:-opencloudos}"
VM_PASSWORD="${VM_PASSWORD:-opencloudos}"
SSH_HOST="${SSH_HOST:-127.0.0.1}"
SSH_PORT="${SSH_PORT:-10022}"

UNIT_NAME="${UNIT_NAME:-cube-sandbox-control.target}"
LEGACY_UNIT_NAME="cube-sandbox-oneclick.service"
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
  enable   (default)  Enable and restart ${UNIT_NAME} inside the guest, so
                      cube components come back automatically on every boot.
  disable             Disable the target. By default also stops it now
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

if [[ "${UNIT_NAME}" == "${LEGACY_UNIT_NAME}" ]]; then
  log_error "${LEGACY_UNIT_NAME} is deprecated and cannot be selected with UNIT_NAME."
  log_error "Use cube-sandbox-control.target instead."
  exit 1
fi

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

unit_exists() {
  run_ssh "sudo systemctl cat '$1'" >/dev/null 2>&1
}

log_info "Target VM : ${VM_USER}@${SSH_HOST}:${SSH_PORT}"
log_info "Unit      : ${UNIT_NAME}"
log_info "Action    : ${ACTION}"

case "${ACTION}" in
  enable)
    if ! unit_exists "${UNIT_NAME}"; then
      log_error "Unit ${UNIT_NAME} not found inside the guest."
      log_error "Please install CubeSandbox inside the VM with the one-click installer first."
      exit 1
    fi

    if run_ssh "sudo systemctl is-enabled '${UNIT_NAME}'" >/dev/null 2>&1; then
      log_warn "${UNIT_NAME} is already enabled; it will still be restarted."
    fi

    if ! confirm "Enable ${UNIT_NAME} on boot and restart the CubeSandbox services now?"; then
      log_warn "Aborted by user."
      exit 1
    fi

    if unit_exists "${LEGACY_UNIT_NAME}"; then
      log_info "Disabling legacy unit ${LEGACY_UNIT_NAME}..."
      run_ssh "sudo systemctl disable '${LEGACY_UNIT_NAME}'"

      if run_ssh "sudo systemctl is-active '${LEGACY_UNIT_NAME}'" >/dev/null 2>&1; then
        log_info "Pausing CubeSandbox services for a clean handoff from the legacy unit..."
        run_ssh "sudo systemctl stop '${UNIT_NAME}'"
        run_ssh "sudo systemctl stop '${LEGACY_UNIT_NAME}'"
      fi

      run_ssh "sudo systemctl reset-failed '${LEGACY_UNIT_NAME}'"
      log_success "Legacy unit ${LEGACY_UNIT_NAME} cleaned up"
    fi

    log_info "Enabling and restarting ${UNIT_NAME}..."
    run_ssh "sudo systemctl enable '${UNIT_NAME}'"
    run_ssh "sudo systemctl restart '${UNIT_NAME}'"
    log_success "${UNIT_NAME} enabled and restarted"

    log_info "Current status:"
    run_ssh "sudo systemctl --no-pager --full status '${UNIT_NAME}'" || true
    ;;

  disable)
    target_exists=0
    legacy_exists=0
    unit_exists "${UNIT_NAME}" && target_exists=1
    unit_exists "${LEGACY_UNIT_NAME}" && legacy_exists=1

    if [[ "${target_exists}" == "0" && "${legacy_exists}" == "0" ]]; then
      log_warn "Neither ${UNIT_NAME} nor ${LEGACY_UNIT_NAME} exists inside the guest."
      exit 0
    fi

    local_prompt="Disable CubeSandbox autostart (${UNIT_NAME} and any legacy unit)?"
    if [[ "${STOP_NOW}" == "1" ]]; then
      local_prompt+=" It will also stop the running CubeSandbox services."
    fi

    if ! confirm "${local_prompt}"; then
      log_warn "Aborted by user."
      exit 1
    fi

    if [[ "${target_exists}" == "1" ]]; then
      run_ssh "sudo systemctl disable '${UNIT_NAME}'"
    fi
    if [[ "${legacy_exists}" == "1" ]]; then
      run_ssh "sudo systemctl disable '${LEGACY_UNIT_NAME}'"
    fi

    if [[ "${STOP_NOW}" == "1" ]]; then
      if [[ "${target_exists}" == "1" ]]; then
        log_info "Stopping ${UNIT_NAME}..."
        run_ssh "sudo systemctl stop '${UNIT_NAME}'"
      fi
      if [[ "${legacy_exists}" == "1" ]] \
        && run_ssh "sudo systemctl is-active '${LEGACY_UNIT_NAME}'" >/dev/null 2>&1; then
        log_info "Stopping legacy unit ${LEGACY_UNIT_NAME}..."
        run_ssh "sudo systemctl stop '${LEGACY_UNIT_NAME}'"
      fi
    else
      log_info "Units disabled; running services were left unchanged."
    fi
    log_success "CubeSandbox autostart disabled"
    ;;

  status)
    target_exists=0
    if unit_exists "${UNIT_NAME}"; then
      target_exists=1
      enabled_state="$(run_ssh "sudo systemctl is-enabled '${UNIT_NAME}'" 2>/dev/null || true)"
      active_state="$(run_ssh "sudo systemctl is-active '${UNIT_NAME}'" 2>/dev/null || true)"
      log_info "is-enabled : ${enabled_state:-unknown}"
      log_info "is-active  : ${active_state:-unknown}"
      run_ssh "sudo systemctl --no-pager --full status '${UNIT_NAME}'" || true
    else
      log_warn "Unit ${UNIT_NAME} not found inside the guest."
    fi

    if unit_exists "${LEGACY_UNIT_NAME}"; then
      legacy_enabled_state="$(run_ssh "sudo systemctl is-enabled '${LEGACY_UNIT_NAME}'" 2>/dev/null || true)"
      legacy_active_state="$(run_ssh "sudo systemctl is-active '${LEGACY_UNIT_NAME}'" 2>/dev/null || true)"
      legacy_failed_state="$(run_ssh "sudo systemctl is-failed '${LEGACY_UNIT_NAME}'" 2>/dev/null || true)"
      if [[ "${legacy_enabled_state}" == "enabled" \
        || "${legacy_active_state}" == "active" \
        || "${legacy_failed_state}" == "failed" ]]; then
        log_warn \
          "Legacy unit ${LEGACY_UNIT_NAME} still needs cleanup (enabled=${legacy_enabled_state:-unknown}, active=${legacy_active_state:-unknown}, failed=${legacy_failed_state:-unknown})."
      fi
    fi

    [[ "${target_exists}" == "1" ]] || exit 0
    ;;
esac
