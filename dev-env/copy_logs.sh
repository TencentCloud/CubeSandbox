#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# copy_logs.sh — Collect runtime logs from the CubeSandbox dev VM.
#
# Creates a tarball of the guest's log directory (default /data/log) and
# downloads it to the host via SSH port-forward (127.0.0.1:10022 by default).
# Password authentication is automated via SSH_ASKPASS; no prior key-based
# trust is required.
#
# Usage:
#   ./copy_logs.sh
#
# Common environment variables:
#   VM_USER, VM_PASSWORD       Guest credentials (default: opencloudos / opencloudos)
#   SSH_HOST, SSH_PORT         Host-side forward target (default: 127.0.0.1:10022)
#   REMOTE_LOG_DIR             Log directory inside guest (default: /data/log)
#   OUTPUT_DIR                 Where to drop the archive on host (default: dev-env/)
#   ARCHIVE_NAME               Archive file name (default: data-log-<timestamp>.tar.gz)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="${WORK_DIR:-${SCRIPT_DIR}/.workdir}"

VM_USER="${VM_USER:-opencloudos}"
VM_PASSWORD="${VM_PASSWORD:-opencloudos}"
SSH_HOST="${SSH_HOST:-127.0.0.1}"
SSH_PORT="${SSH_PORT:-10022}"

REMOTE_LOG_DIR="${REMOTE_LOG_DIR:-/data/log}"
OUTPUT_DIR="${OUTPUT_DIR:-${SCRIPT_DIR}}"
ARCHIVE_NAME="${ARCHIVE_NAME:-data-log-$(date +%Y%m%d-%H%M%S).tar.gz}"
REMOTE_TMP_ARCHIVE="${REMOTE_TMP_ARCHIVE:-/tmp/${ARCHIVE_NAME}}"

ASKPASS_SCRIPT="${WORK_DIR}/.ssh-askpass.sh"

LOG_TAG="copy_logs"

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
need_cmd scp
need_cmd setsid

mkdir -p "${WORK_DIR}"
mkdir -p "${OUTPUT_DIR}"

cat >"${ASKPASS_SCRIPT}" <<EOF
#!/usr/bin/env bash
printf '%s\n' '${VM_PASSWORD}'
EOF
chmod 700 "${ASKPASS_SCRIPT}"

cleanup() {
  rm -f "${ASKPASS_SCRIPT}"
}
trap cleanup EXIT

SSH_COMMON_OPTS=(
  -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null
  -o PreferredAuthentications=password
  -o PubkeyAuthentication=no
)

SSH_OPTS=(
  "${SSH_COMMON_OPTS[@]}"
  -p "${SSH_PORT}"
)

SCP_OPTS=(
  "${SSH_COMMON_OPTS[@]}"
  -P "${SSH_PORT}"
)

run_ssh() {
  DISPLAY="${DISPLAY:-cubesandbox-dev-env}" \
  SSH_ASKPASS="${ASKPASS_SCRIPT}" \
  SSH_ASKPASS_REQUIRE=force \
  setsid -w ssh "${SSH_OPTS[@]}" "${VM_USER}@${SSH_HOST}" "$@"
}

run_scp() {
  DISPLAY="${DISPLAY:-cubesandbox-dev-env}" \
  SSH_ASKPASS="${ASKPASS_SCRIPT}" \
  SSH_ASKPASS_REQUIRE=force \
  setsid -w scp "${SCP_OPTS[@]}" "$@"
}

LOCAL_ARCHIVE_PATH="${OUTPUT_DIR}/${ARCHIVE_NAME}"

log_info "Target VM    : ${VM_USER}@${SSH_HOST}:${SSH_PORT}"
log_info "Remote dir   : ${REMOTE_LOG_DIR}"
log_info "Remote tmp   : ${REMOTE_TMP_ARCHIVE}"
log_info "Local output : ${LOCAL_ARCHIVE_PATH}"

log_info "Checking remote log directory exists..."
if ! run_ssh "sudo test -d '${REMOTE_LOG_DIR}'"; then
  log_error "Remote directory not found or inaccessible: ${REMOTE_LOG_DIR}"
  exit 1
fi

log_info "Packaging ${REMOTE_LOG_DIR} -> ${REMOTE_TMP_ARCHIVE} (inside VM)"
PARENT_DIR="$(dirname "${REMOTE_LOG_DIR}")"
BASE_NAME="$(basename "${REMOTE_LOG_DIR}")"
run_ssh "sudo tar -czf '${REMOTE_TMP_ARCHIVE}' -C '${PARENT_DIR}' '${BASE_NAME}' && sudo chmod 644 '${REMOTE_TMP_ARCHIVE}' && sudo chown '${VM_USER}':'${VM_USER}' '${REMOTE_TMP_ARCHIVE}'"

log_info "Downloading archive to ${LOCAL_ARCHIVE_PATH}"
run_scp "${VM_USER}@${SSH_HOST}:${REMOTE_TMP_ARCHIVE}" "${LOCAL_ARCHIVE_PATH}"

log_info "Removing remote temporary archive"
run_ssh "sudo rm -f '${REMOTE_TMP_ARCHIVE}'" || log_warn "Failed to remove remote temp archive ${REMOTE_TMP_ARCHIVE}"

if [[ -f "${LOCAL_ARCHIVE_PATH}" ]]; then
  ARCHIVE_SIZE="$(du -h "${LOCAL_ARCHIVE_PATH}" | awk '{print $1}')"
  log_success "Logs archived to ${LOCAL_ARCHIVE_PATH} (${ARCHIVE_SIZE})"
else
  log_error "Archive was not created: ${LOCAL_ARCHIVE_PATH}"
  exit 1
fi
