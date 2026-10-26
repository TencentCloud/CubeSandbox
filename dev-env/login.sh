#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# login.sh — Open an interactive SSH session to the CubeSandbox dev VM.
#
# Connects to 127.0.0.1:10022 (the host-side port forwarded by run_vm.sh) and
# authenticates with the default guest password via SSH_ASKPASS, so that no
# ssh-key setup is needed.
#
# Usage:
#   ./login.sh
#
# Common environment variables:
#   VM_USER, VM_PASSWORD       Guest credentials (default: opencloudos / opencloudos)
#   SSH_HOST, SSH_PORT         Host-side forward target (default: 127.0.0.1:10022)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="${WORK_DIR:-${SCRIPT_DIR}/.workdir}"

VM_USER="${VM_USER:-opencloudos}"
VM_PASSWORD="${VM_PASSWORD:-opencloudos}"
SSH_HOST="${SSH_HOST:-127.0.0.1}"
SSH_PORT="${SSH_PORT:-10022}"
LOGIN_AS_ROOT="${LOGIN_AS_ROOT:-1}"

ASKPASS_SCRIPT="${WORK_DIR}/.ssh-askpass.sh"

LOG_TAG="login"

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

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    log_error "Missing required command: $1"
    exit 1
  fi
}

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
  -t
  -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null
  -o PreferredAuthentications=password
  -o PubkeyAuthentication=no
  -p "${SSH_PORT}"
)

if [[ "${LOGIN_AS_ROOT}" == "1" ]]; then
  REMOTE_CMD="sudo -i"
  log_info "Logging into ${VM_USER}@${SSH_HOST}:${SSH_PORT} and switching to root (sudo -i)"
else
  REMOTE_CMD=""
  log_info "Logging into ${VM_USER}@${SSH_HOST}:${SSH_PORT} as ${VM_USER}"
fi

DISPLAY="${DISPLAY:-cubesandbox-dev-env}" \
SSH_ASKPASS="${ASKPASS_SCRIPT}" \
SSH_ASKPASS_REQUIRE=force \
setsid -w ssh "${SSH_OPTS[@]}" "${VM_USER}@${SSH_HOST}" ${REMOTE_CMD}
