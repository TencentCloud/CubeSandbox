#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# sync_to_vm.sh — Copy pre-built artifacts into the CubeSandbox dev VM.
#
# This script only copies files. It does not build on the host, restart
# services, run quickcheck, or roll back automatically.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${SCRIPT_DIR}/.." && pwd)}"
WORK_DIR="${WORK_DIR:-${SCRIPT_DIR}/.workdir}"

VM_USER="${VM_USER:-opencloudos}"
VM_PASSWORD="${VM_PASSWORD:-opencloudos}"
SSH_HOST="${SSH_HOST:-127.0.0.1}"
SSH_PORT="${SSH_PORT:-10022}"

TOOLBOX_ROOT="${TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
UNIT_NAME="${UNIT_NAME:-cube-sandbox-oneclick.service}"
OUTPUT_BIN_DIR="${OUTPUT_BIN_DIR:-${REPO_ROOT}/_output/bin}"

ASKPASS_SCRIPT="${WORK_DIR}/.ssh-askpass.sh"
REMOTE_DIR_DEFAULT="/tmp"

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

usage() {
  cat <<EOF
Usage: $(basename "$0") <subcommand> [args]

Subcommands:
  bin [NAME ...]                Copy pre-built binaries from ${OUTPUT_BIN_DIR}
                                into the VM. If NAME is omitted, sync all
                                known components.
  files [--remote-dir DIR] PATH [PATH ...]
                                Copy arbitrary files or directories into the VM.
  -h, --help                    Show this help.

Known components:
  cubemaster, cubemastercli, cubelet, cubecli,
  network-agent, cube-api, cube-runtime, containerd-shim-cube-rs

Environment overrides:
  VM_USER, VM_PASSWORD, SSH_HOST, SSH_PORT
  TOOLBOX_ROOT       default: ${TOOLBOX_ROOT}
  UNIT_NAME          default: ${UNIT_NAME}
  OUTPUT_BIN_DIR     default: ${OUTPUT_BIN_DIR}

Notes:
  - This script only copies files. Build on the host first.
  - Previous binary is kept as <name>.bak on the VM.
EOF
}

usage_error() {
  log_error "$1"
  usage >&2
  exit 2
}

is_help_arg() {
  case "${1:-}" in
    -h|--help|help) return 0 ;;
    *) return 1 ;;
  esac
}

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    log_error "Missing required command: $1"
    exit 1
  fi
}

cleanup() {
  rm -f "${ASKPASS_SCRIPT}"
}

setup_ssh_support() {
  need_cmd ssh
  need_cmd scp
  need_cmd setsid

  mkdir -p "${WORK_DIR}"

  cat >"${ASKPASS_SCRIPT}" <<EOF
#!/usr/bin/env bash
printf '%s\n' '${VM_PASSWORD}'
EOF
  chmod 700 "${ASKPASS_SCRIPT}"
  trap cleanup EXIT
}

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

run_with_askpass() {
  DISPLAY="${DISPLAY:-cubesandbox-dev-env}" \
  SSH_ASKPASS="${ASKPASS_SCRIPT}" \
  SSH_ASKPASS_REQUIRE=force \
  setsid -w "$@"
}

run_ssh() {
  run_with_askpass ssh "${SSH_OPTS[@]}" "${VM_USER}@${SSH_HOST}" "$@"
}

run_scp() {
  run_with_askpass scp "${SCP_OPTS[@]}" "$@"
}

component_remote_dir() {
  case "$1" in
    cubemaster|cubemastercli) printf '%s/CubeMaster/bin\n' "${TOOLBOX_ROOT}" ;;
    cubelet|cubecli) printf '%s/Cubelet/bin\n' "${TOOLBOX_ROOT}" ;;
    network-agent) printf '%s/network-agent/bin\n' "${TOOLBOX_ROOT}" ;;
    cube-api) printf '%s/CubeAPI/bin\n' "${TOOLBOX_ROOT}" ;;
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

validate_component() {
  local name="$1"
  if ! component_remote_dir "${name}" >/dev/null; then
    log_error "Unknown component: ${name}"
    log_error "Known components: ${ALL_COMPONENTS[*]}"
    exit 1
  fi
}

print_bin_next_step() {
  printf '\nNext step - paste into your ./login.sh session (root shell in the VM):\n\n'
  printf '  systemctl restart %s\n' "${UNIT_NAME}"
}

mode_bin() {
  local -a target_components=("$@")
  local name=""
  local remote_dir=""
  local local_path=""
  local remote_path=""

  if [[ "${#target_components[@]}" -eq 1 ]] && is_help_arg "${target_components[0]}"; then
    usage
    exit 0
  fi

  if [[ ! -d "${OUTPUT_BIN_DIR}" ]]; then
    log_error "Binary output directory not found: ${OUTPUT_BIN_DIR}"
    log_error "Build on the host first, for example: make all"
    exit 1
  fi

  if [[ "${#target_components[@]}" -eq 0 ]]; then
    target_components=("${ALL_COMPONENTS[@]}")
  fi

  for name in "${target_components[@]}"; do
    if [[ "${name}" == -* ]]; then
      usage_error "Unknown option for 'bin': ${name}"
    fi
    validate_component "${name}"
    local_path="${OUTPUT_BIN_DIR}/${name}"
    if [[ ! -f "${local_path}" ]]; then
      log_error "Binary not found: ${local_path}"
      log_error "Build it on the host first, for example: make all"
      exit 1
    fi
  done

  setup_ssh_support

  log_info "Target VM : ${VM_USER}@${SSH_HOST}:${SSH_PORT}"
  log_info "Subcommand: bin"

  for name in "${target_components[@]}"; do
    remote_dir="$(component_remote_dir "${name}")"
    local_path="${OUTPUT_BIN_DIR}/${name}"
    remote_path="${remote_dir}/${name}"

    log_info "Copying ${local_path} -> ${VM_USER}@${SSH_HOST}:${remote_path}"

    local stage_path="/tmp/${name}.sync_to_vm.$$"
    if ! run_scp "${local_path}" "${VM_USER}@${SSH_HOST}:${stage_path}"; then
      log_error "Failed to upload ${local_path} to the VM"
      log_error "Check that the VM is running and SSH settings are correct."
      exit 1
    fi

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
      log_error "Failed to install ${name} on the VM"
      run_ssh "sudo rm -f '${stage_path}'" || true
      exit 1
    fi
  done

  log_success "Synced ${#target_components[@]} binaries: ${target_components[*]}"
  print_bin_next_step
}

mode_files() {
  local remote_dir="${REMOTE_DIR_DEFAULT}"
  local -a paths=()
  local f=""

  while [[ "$#" -gt 0 ]]; do
    case "$1" in
      --remote-dir)
        if [[ "$#" -lt 2 ]]; then
          usage_error "Missing value for --remote-dir"
        fi
        remote_dir="$2"
        shift 2
        ;;
      -h|--help|help)
        usage
        exit 0
        ;;
      --)
        shift
        while [[ "$#" -gt 0 ]]; do
          paths+=("$1")
          shift
        done
        ;;
      -*)
        usage_error "Unknown option for 'files': $1"
        ;;
      *)
        paths+=("$1")
        shift
        ;;
    esac
  done

  if [[ "${#paths[@]}" -eq 0 ]]; then
    usage_error "'files' requires at least one PATH"
  fi

  for f in "${paths[@]}"; do
    if [[ ! -e "${f}" ]]; then
      log_error "Local path not found: ${f}"
      log_error "Check the path and try again."
      exit 1
    fi
  done

  setup_ssh_support

  log_info "Target VM : ${VM_USER}@${SSH_HOST}:${SSH_PORT}"
  log_info "Subcommand: files"
  log_info "Remote dir: ${remote_dir}"

  if ! run_ssh "sudo mkdir -p '${remote_dir}' && sudo chown ${VM_USER}:${VM_USER} '${remote_dir}' || true"; then
    log_error "Failed to prepare remote directory: ${remote_dir}"
    log_error "Check that the VM is reachable and the directory is writable."
    exit 1
  fi

  for f in "${paths[@]}"; do
    log_info "Copying ${f} -> ${VM_USER}@${SSH_HOST}:${remote_dir}/"
    if ! run_scp -r "${f}" "${VM_USER}@${SSH_HOST}:${remote_dir}/"; then
      log_error "Failed to upload ${f} to the VM"
      log_error "Check that the VM is running and SSH settings are correct."
      exit 1
    fi
  done

  log_success "Copied ${#paths[@]} path(s) into ${remote_dir}/ on the VM"
}

ACTION="${1:-}"
if is_help_arg "${ACTION}"; then
  usage
  exit 0
fi
if [[ -z "${ACTION}" ]]; then
  usage_error "Missing subcommand. Use 'bin' or 'files'."
fi
shift

case "${ACTION}" in
  bin)
    mode_bin "$@"
    ;;
  files)
    mode_files "$@"
    ;;
  *)
    usage_error "Unknown subcommand: ${ACTION}"
    ;;
esac
