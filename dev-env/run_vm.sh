#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# run_vm.sh — Boot the CubeSandbox dev VM via QEMU/KVM.
#
# Launches the prepared qcow2 image with nested KVM enabled and sets up user
# mode networking with port forwards:
#   - host :10022 -> guest :22  (ssh, used by login.sh / sync_to_vm.sh / copy_logs.sh)
#   - host :13000 -> guest :3000 (cube-api HTTP endpoint)
#   - host :11080 -> guest :80   (cube-proxy HTTP endpoint)
#   - host :11443 -> guest :443  (cube-proxy HTTPS endpoint)
#
# Run prepare_image.sh first to produce the image. This script is the normal
# way to start the VM for day-to-day development.
#
# Usage:
#   ./run_vm.sh
#
# Common environment variables:
#   WORK_DIR                   Working dir (default: dev-env/.workdir)
#   IMAGE_URL                  Base qcow2 URL (used to derive IMAGE_NAME)
#   IMAGE_PATH                 Full path to VM disk image (defaults to WORK_DIR/IMAGE_NAME)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="${WORK_DIR:-${SCRIPT_DIR}/.workdir}"
IMAGE_URL="${IMAGE_URL:-https://mirrors.tencent.com/opencloudos/9/images/qcow2/x86_64/OpenCloudOS-GenericCloud-9.4-20251120.0.x86_64.qcow2}"
IMAGE_NAME="$(basename "${IMAGE_URL}")"
IMAGE_PATH="${IMAGE_PATH:-${WORK_DIR}/${IMAGE_NAME}}"

VM_NAME="${VM_NAME:-opencloudos9-cubesandbox}"
VM_MEMORY_MB="${VM_MEMORY_MB:-8192}"
VM_CPUS="${VM_CPUS:-4}"
SSH_PORT="${SSH_PORT:-10022}"
CUBE_API_PORT="${CUBE_API_PORT:-13000}"
CUBE_PROXY_HTTP_PORT="${CUBE_PROXY_HTTP_PORT:-11080}"
CUBE_PROXY_HTTPS_PORT="${CUBE_PROXY_HTTPS_PORT:-11443}"
REQUIRE_NESTED_KVM="${REQUIRE_NESTED_KVM:-1}"
VM_BACKGROUND="${VM_BACKGROUND:-0}"
QEMU_PIDFILE="${QEMU_PIDFILE:-${WORK_DIR}/qemu.pid}"
QEMU_SERIAL_LOG="${QEMU_SERIAL_LOG:-${WORK_DIR}/qemu-serial.log}"

LOG_TAG="run_vm"

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



require_nested_kvm() {
  local nested_value=""

  if [[ -f /sys/module/kvm_intel/parameters/nested ]]; then
    nested_value="$(tr '[:lower:]' '[:upper:]' </sys/module/kvm_intel/parameters/nested)"
  elif [[ -f /sys/module/kvm_amd/parameters/nested ]]; then
    nested_value="$(tr '[:lower:]' '[:upper:]' </sys/module/kvm_amd/parameters/nested)"
  else
    log_warn "Host KVM nested parameter not found; make sure nested virtualization is enabled."
    return
  fi

  if [[ "${nested_value}" != "Y" && "${nested_value}" != "1" ]]; then
    log_error "Host does not have nested KVM enabled; /dev/kvm will not be usable inside the guest."
    log_error "Cube Sandbox needs nested KVM to run MicroVMs inside the guest."
    log_error "To skip this check (boot only, no Cube Sandbox), set REQUIRE_NESTED_KVM=0."
    exit 1
  fi
}

need_cmd qemu-system-x86_64

if [[ ! -e /dev/kvm ]]; then
  log_error "Host has no /dev/kvm; KVM acceleration is unavailable."
  exit 1
fi

if [[ ! -f "${IMAGE_PATH}" ]]; then
  log_error "Image not found: ${IMAGE_PATH}"
  log_error "Please run ./prepare_image.sh first."
  exit 1
fi

if [[ "${REQUIRE_NESTED_KVM}" == "1" ]]; then
  require_nested_kvm
fi

log_info "Booting OpenCloudOS 9 VM"
log_info "  Image      : ${IMAGE_PATH}"
log_info "  Login user : opencloudos"
log_info "  Password   : opencloudos"
log_info "  SSH        : ssh -p ${SSH_PORT} opencloudos@127.0.0.1"
log_info "  Cube API   : http://127.0.0.1:${CUBE_API_PORT} -> guest:3000"
log_info "  CubeProxy  : http://127.0.0.1:${CUBE_PROXY_HTTP_PORT} -> guest:80"
log_info "  CubeProxy  : https://127.0.0.1:${CUBE_PROXY_HTTPS_PORT} -> guest:443"
if [[ "${VM_BACKGROUND}" == "1" ]]; then
  log_info "Background mode:"
  log_info "  PID file   : ${QEMU_PIDFILE}"
  log_info "  Serial log : ${QEMU_SERIAL_LOG}"
else
  log_info "Clean shutdown: in another terminal run ./login.sh, then poweroff in the guest (do not Ctrl+a x — abrupt QEMU exit)"
fi

QEMU_ARGS=(
  -enable-kvm
  -machine q35,accel=kvm
  -cpu host
  -name "${VM_NAME}"
  -m "${VM_MEMORY_MB}"
  -smp "${VM_CPUS}"
  -device virtio-rng-pci
  -drive if=virtio,format=qcow2,file="${IMAGE_PATH}"
  -nic user,model=virtio-net-pci,hostfwd=tcp::"${SSH_PORT}"-:22,hostfwd=tcp::"${CUBE_API_PORT}"-:3000,hostfwd=tcp::"${CUBE_PROXY_HTTP_PORT}"-:80,hostfwd=tcp::"${CUBE_PROXY_HTTPS_PORT}"-:443
)

if [[ "${VM_BACKGROUND}" == "1" ]]; then
  mkdir -p "${WORK_DIR}"
  exec qemu-system-x86_64 \
    "${QEMU_ARGS[@]}" \
    -daemonize \
    -pidfile "${QEMU_PIDFILE}" \
    -display none \
    -serial "file:${QEMU_SERIAL_LOG}"
fi

exec qemu-system-x86_64 \
  "${QEMU_ARGS[@]}" \
  -nographic \
  -serial mon:stdio
