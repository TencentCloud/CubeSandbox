#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# sync_to_vm.sh — Push locally-built artifacts into the CubeSandbox dev VM.
#
# This is the core inner-loop tool for development: rebuild on the host, sync
# binaries into the guest, restart services, and run quickcheck — with
# automatic rollback from .bak if the health check fails.
#
# Modes (selected via MODE env, default "binaries"):
#   binaries  Copy Go binaries from _output/bin/ to their install paths in
#             the guest; previous file is kept as <name>.bak (only 1 backup).
#             Restarts cube-sandbox-oneclick.service and runs quickcheck.sh.
#             On failure, .bak files are restored and the unit restarted again.
#   release   Run `make manual-release` then deploy the tarball via
#             deploy-manual.sh inside the guest.
#   files     Free-form scp of a list of files to REMOTE_DIR (default /tmp).
#
# Usage:
#   ./sync_to_vm.sh                                  # binaries mode, build + restart
#   MODE=binaries COMPONENTS="cubelet cubemaster" ./sync_to_vm.sh
#   MODE=binaries BUILD=0 RESTART=0 ./sync_to_vm.sh  # sync only, no build/restart
#   MODE=release ./sync_to_vm.sh
#   MODE=files FILES="foo.yaml bar.conf" REMOTE_DIR=/tmp ./sync_to_vm.sh
#
# Common environment variables:
#   MODE                       binaries | release | files (default: binaries)
#   BUILD                      Run `make all` before syncing (default: 1)
#   RESTART                    Restart unit + quickcheck after sync (default: 1; 0 or "systemd")
#   COMPONENTS                 Space-separated binary names to sync (default: all in _output/bin/)
#   FILES                      Files to send in "files" mode
#   REMOTE_DIR                 Remote dir for "files" mode (default: /tmp)
#   VM_USER, VM_PASSWORD       Guest credentials (default: opencloudos / opencloudos)
#   SSH_HOST, SSH_PORT         Host-side forward target (default: 127.0.0.1:10022)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"
WORK_DIR="${WORK_DIR:-${SCRIPT_DIR}/.workdir}"

VM_USER="${VM_USER:-opencloudos}"
VM_PASSWORD="${VM_PASSWORD:-opencloudos}"
SSH_HOST="${SSH_HOST:-127.0.0.1}"
SSH_PORT="${SSH_PORT:-10022}"

MODE="${MODE:-binaries}"
BUILD="${BUILD:-1}"
RESTART="${RESTART:-1}"
COMPONENTS="${COMPONENTS:-}"
FILES="${FILES:-}"
REMOTE_DIR="${REMOTE_DIR:-/tmp}"

TOOLBOX_ROOT="${TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
UNIT_NAME="${UNIT_NAME:-cube-sandbox-oneclick.service}"
QUICKCHECK="${TOOLBOX_ROOT}/scripts/one-click/quickcheck.sh"

OUTPUT_BIN_DIR="${OUTPUT_BIN_DIR:-${REPO_ROOT}/_output/bin}"
RELEASE_DIR="${RELEASE_DIR:-${REPO_ROOT}/_output/release}"
DEPLOY_MANUAL_SCRIPT="${REPO_ROOT}/deploy/one-click/deploy-manual.sh"

ASKPASS_SCRIPT="${WORK_DIR}/.ssh-askpass.sh"

LOG_TAG="sync_to_vm"

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

# Component layout: name -> remote install dir.
# Mirrors deploy/one-click/scripts/one-click/up.sh.
component_remote_dir() {
  case "$1" in
    cubemaster|cubemastercli)   printf '%s/CubeMaster/bin\n' "${TOOLBOX_ROOT}" ;;
    cubelet|cubecli)            printf '%s/Cubelet/bin\n' "${TOOLBOX_ROOT}" ;;
    network-agent)              printf '%s/network-agent/bin\n' "${TOOLBOX_ROOT}" ;;
    cube-api)                   printf '%s/CubeAPI/bin\n' "${TOOLBOX_ROOT}" ;;
    cube-runtime|containerd-shim-cube-rs) printf '/usr/local/bin\n' ;;
    *) return 1 ;;
  esac
}

ALL_COMPONENTS=(
  cubemaster cubemastercli
  cubelet cubecli
  network-agent
  cube-api
  cube-runtime containerd-shim-cube-rs
)

restart_remote() {
  case "${RESTART}" in
    0)
      log_info "RESTART=0, skipping remote restart"
      return 0
      ;;
    1|systemd)
      log_info "Restarting ${UNIT_NAME} on the guest..."
      if ! run_ssh "sudo systemctl restart '${UNIT_NAME}'"; then
        log_error "systemctl restart ${UNIT_NAME} failed"
        return 1
      fi
      log_success "${UNIT_NAME} restarted"
      ;;
    *)
      log_error "Unknown RESTART value: ${RESTART} (expected 0|1|systemd)"
      return 1
      ;;
  esac

  if run_ssh "sudo test -x '${QUICKCHECK}'"; then
    log_info "Running quickcheck on the guest..."
    if run_ssh "sudo '${QUICKCHECK}'"; then
      log_success "quickcheck passed"
      return 0
    fi
    log_error "quickcheck failed"
    return 1
  fi

  log_warn "quickcheck script not found at ${QUICKCHECK}, skipping verification"
}

mode_binaries() {
  if [[ "${BUILD}" == "1" ]]; then
    log_info "Running 'make all' in ${REPO_ROOT}..."
    ( cd "${REPO_ROOT}" && make all )
    log_success "Local build finished"
  else
    log_info "BUILD=0, skipping 'make all'"
  fi

  if [[ ! -d "${OUTPUT_BIN_DIR}" ]]; then
    log_error "Output dir not found: ${OUTPUT_BIN_DIR}"
    log_error "Run 'make all' first or set BUILD=1."
    exit 1
  fi

  local -a target_components
  if [[ -n "${COMPONENTS}" ]]; then
    # shellcheck disable=SC2206
    target_components=(${COMPONENTS})
  else
    target_components=("${ALL_COMPONENTS[@]}")
  fi

  local -a synced=()
  local -a synced_remote_paths=()
  local name remote_dir local_path remote_path

  for name in "${target_components[@]}"; do
    if ! remote_dir="$(component_remote_dir "${name}")"; then
      log_warn "Unknown component '${name}', skipping"
      continue
    fi
    local_path="${OUTPUT_BIN_DIR}/${name}"
    if [[ ! -f "${local_path}" ]]; then
      log_warn "Local binary missing, skipping: ${local_path}"
      continue
    fi

    remote_path="${remote_dir}/${name}"
    log_info "Syncing ${name} -> ${VM_USER}@${SSH_HOST}:${remote_path}"

    # Stage to /tmp first (we cannot scp directly as root via password auth).
    local stage_path="/tmp/${name}.sync_to_vm.$$"
    run_scp "${local_path}" "${VM_USER}@${SSH_HOST}:${stage_path}"

    # Backup existing -> .bak (overwrites previous .bak), then move into place.
    if ! run_ssh "
      set -e
      sudo mkdir -p '${remote_dir}'
      if [ -f '${remote_path}' ]; then
        sudo mv -f '${remote_path}' '${remote_path}.bak'
      fi
      sudo mv -f '${stage_path}' '${remote_path}'
      sudo chmod +x '${remote_path}'
      sudo chown root:root '${remote_path}' || true
    "; then
      log_error "Failed to install ${name} on the guest"
      run_ssh "sudo rm -f '${stage_path}'" || true
      exit 1
    fi
    synced+=("${name}")
    synced_remote_paths+=("${remote_path}")
  done

  if [[ "${#synced[@]}" -eq 0 ]]; then
    log_warn "No binaries were synced. Nothing to restart."
    return 0
  fi

  log_success "Synced binaries: ${synced[*]}"

  if ! restart_remote; then
    log_error "Remote restart/quickcheck failed; rolling back .bak files..."
    local p
    for p in "${synced_remote_paths[@]}"; do
      log_warn "Restoring ${p}.bak -> ${p}"
      run_ssh "
        if sudo test -f '${p}.bak'; then
          sudo mv -f '${p}.bak' '${p}'
          sudo chmod +x '${p}' || true
        fi
      " || log_error "Failed to restore ${p}"
    done
    log_warn "Attempting to restart with previous binaries..."
    run_ssh "sudo systemctl restart '${UNIT_NAME}'" || \
      log_error "Restart with previous binaries also failed; manual intervention required."
    exit 1
  fi
}

mode_release() {
  if [[ "${BUILD}" == "1" ]]; then
    log_info "Running 'make manual-release' in ${REPO_ROOT}..."
    ( cd "${REPO_ROOT}" && make manual-release )
    log_success "Manual release built"
  else
    log_info "BUILD=0, skipping 'make manual-release'"
  fi

  if [[ ! -d "${RELEASE_DIR}" ]]; then
    log_error "Release dir not found: ${RELEASE_DIR}"
    exit 1
  fi

  local pkg
  pkg="$(ls -t "${RELEASE_DIR}"/cube-manual-update-*.tar.gz 2>/dev/null | head -n 1 || true)"
  if [[ -z "${pkg}" ]]; then
    log_error "No cube-manual-update-*.tar.gz found in ${RELEASE_DIR}"
    exit 1
  fi

  if [[ ! -f "${DEPLOY_MANUAL_SCRIPT}" ]]; then
    log_error "deploy-manual.sh not found: ${DEPLOY_MANUAL_SCRIPT}"
    exit 1
  fi

  local remote_pkg="/tmp/$(basename "${pkg}")"
  local remote_script="/tmp/deploy-manual.sh"

  log_info "Uploading $(basename "${pkg}") to ${remote_pkg}"
  run_scp "${pkg}" "${VM_USER}@${SSH_HOST}:${remote_pkg}"
  log_info "Uploading deploy-manual.sh to ${remote_script}"
  run_scp "${DEPLOY_MANUAL_SCRIPT}" "${VM_USER}@${SSH_HOST}:${remote_script}"

  log_info "Running deploy-manual.sh on the guest..."
  run_ssh "sudo bash '${remote_script}' '${remote_pkg}'"
  log_success "Manual deploy finished"

  if [[ "${RESTART}" != "0" ]]; then
    # deploy-manual.sh already restarts services, but if the user enabled the
    # systemd unit we additionally re-sync its state so that the unit reflects
    # the new processes.
    log_info "Refreshing ${UNIT_NAME} state..."
    run_ssh "sudo systemctl restart '${UNIT_NAME}'" || \
      log_warn "systemctl restart ${UNIT_NAME} failed (unit may not be enabled)"
  fi
}

mode_files() {
  if [[ -z "${FILES}" ]]; then
    log_error "MODE=files requires FILES=\"path1 path2 ...\""
    exit 1
  fi

  log_info "Remote dir: ${REMOTE_DIR}"
  run_ssh "sudo mkdir -p '${REMOTE_DIR}' && sudo chown ${VM_USER}:${VM_USER} '${REMOTE_DIR}' || true"

  local f
  # shellcheck disable=SC2206
  local -a paths=(${FILES})
  for f in "${paths[@]}"; do
    if [[ ! -e "${f}" ]]; then
      log_error "Local path not found: ${f}"
      exit 1
    fi
    log_info "Copying ${f} -> ${VM_USER}@${SSH_HOST}:${REMOTE_DIR}/"
    run_scp -r "${f}" "${VM_USER}@${SSH_HOST}:${REMOTE_DIR}/"
  done
  log_success "Files synced"
}

log_info "Target VM : ${VM_USER}@${SSH_HOST}:${SSH_PORT}"
log_info "Mode      : ${MODE}"

case "${MODE}" in
  binaries) mode_binaries ;;
  release)  mode_release  ;;
  files)    mode_files    ;;
  *)
    log_error "Unknown MODE: ${MODE} (expected binaries|release|files)"
    exit 1
    ;;
esac

log_success "sync_to_vm finished"
