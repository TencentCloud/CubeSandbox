#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="${WORK_DIR:-${SCRIPT_DIR}/.workdir}"
IMAGE_URL="${IMAGE_URL:-https://mirrors.tencent.com/opencloudos/9/images/qcow2/x86_64/OpenCloudOS-GenericCloud-9.4-20251120.0.x86_64.qcow2}"
TARGET_SIZE="${TARGET_SIZE:-100G}"
AUTO_BOOT="${AUTO_BOOT:-1}"
AUTO_RESIZE_IN_GUEST="${AUTO_RESIZE_IN_GUEST:-1}"
VM_USER="${VM_USER:-opencloudos}"
VM_PASSWORD="${VM_PASSWORD:-opencloudos}"
SSH_PORT="${SSH_PORT:-2222}"
SSH_WAIT_TIMEOUT_SECS="${SSH_WAIT_TIMEOUT_SECS:-180}"
SHUTDOWN_WAIT_TIMEOUT_SECS="${SHUTDOWN_WAIT_TIMEOUT_SECS:-120}"
FORCE_KILL_ON_EXIT="${FORCE_KILL_ON_EXIT:-0}"

IMAGE_NAME="$(basename "${IMAGE_URL}")"
IMAGE_PATH="${IMAGE_PATH:-${WORK_DIR}/${IMAGE_NAME}}"
RUN_VM_SCRIPT="${SCRIPT_DIR}/run_vm.sh"
INTERNAL_DIR="${SCRIPT_DIR}/internal"
GROW_SCRIPT="${INTERNAL_DIR}/grow_rootfs.sh"
SELINUX_SCRIPT="${INTERNAL_DIR}/setup_selinux.sh"
PATH_SCRIPT="${INTERNAL_DIR}/setup_path.sh"
BANNER_SCRIPT="${INTERNAL_DIR}/setup_banner.sh"
QEMU_PIDFILE="${WORK_DIR}/qemu.pid"
QEMU_SERIAL_LOG="${WORK_DIR}/qemu-serial.log"
ASKPASS_SCRIPT="${WORK_DIR}/.ssh-askpass.sh"
SSH_COMMON_OPTS=(
  -o StrictHostKeyChecking=no
  -o UserKnownHostsFile=/dev/null
  -o PreferredAuthentications=password
  -o PubkeyAuthentication=no
  -o ConnectTimeout=5
)

LOG_TAG="prepare_image"

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

log_info()    { _log "${LOG_COLOR_INFO}"    "INFO"    "$@"; }
log_success() { _log "${LOG_COLOR_SUCCESS}" "OK"      "$@"; }
log_warn()    { _log "${LOG_COLOR_WARN}"    "WARN"    "$@" >&2; }
log_error()   { _log "${LOG_COLOR_ERROR}"   "ERROR"   "$@" >&2; }

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    log_error "Missing required command: $1"
    exit 1
  fi
}

virtual_size_bytes() {
  python3 - "$1" <<'PY'
import json
import subprocess
import sys

info = json.loads(
    subprocess.check_output(
        ["qemu-img", "info", "--output=json", sys.argv[1]],
        text=True,
    )
)
print(info["virtual-size"])
PY
}

size_to_bytes() {
  python3 - "$1" <<'PY'
import re
import sys

value = sys.argv[1].strip().upper()
match = re.fullmatch(r"(\d+)([KMGTP]?)(I?B?)?", value)
if not match:
    raise SystemExit(f"cannot parse size: {value}")

number = int(match.group(1))
unit = match.group(2)
factor = {
    "": 1,
    "K": 1024,
    "M": 1024 ** 2,
    "G": 1024 ** 3,
    "T": 1024 ** 4,
    "P": 1024 ** 5,
}[unit]
print(number * factor)
PY
}

create_askpass_script() {
  cat >"${ASKPASS_SCRIPT}" <<EOF
#!/usr/bin/env bash
printf '%s\n' '${VM_PASSWORD}'
EOF
  chmod 700 "${ASKPASS_SCRIPT}"
}

ssh_with_password() {
  DISPLAY="${DISPLAY:-prepare-image}" \
  SSH_ASKPASS="${ASKPASS_SCRIPT}" \
  SSH_ASKPASS_REQUIRE=force \
  setsid -w "$@"
}

wait_for_ssh() {
  local deadline
  deadline=$((SECONDS + SSH_WAIT_TIMEOUT_SECS))

  while (( SECONDS < deadline )); do
    if [[ -f "${QEMU_SERIAL_LOG}" ]] \
      && rg -q "Failed to start .*sshd\\.service|FAILED.*sshd\\.service" \
        "${QEMU_SERIAL_LOG}"; then
      log_error "Guest sshd.service failed to start, SSH will never be ready."
      log_error "Please inspect serial log: ${QEMU_SERIAL_LOG}"
      return 1
    fi

    if ssh_with_password ssh "${SSH_COMMON_OPTS[@]}" -p "${SSH_PORT}" \
      "${VM_USER}@127.0.0.1" 'true' >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done

  log_error "Timed out waiting for guest SSH to become ready (${SSH_WAIT_TIMEOUT_SECS}s)."
  log_error "Please inspect serial log: ${QEMU_SERIAL_LOG}"
  return 1
}

wait_for_shutdown() {
  local deadline pid=""
  deadline=$((SECONDS + SHUTDOWN_WAIT_TIMEOUT_SECS))

  if [[ -f "${QEMU_PIDFILE}" ]]; then
    pid="$(tr -d '[:space:]' <"${QEMU_PIDFILE}")"
  fi

  while (( SECONDS < deadline )); do
    if [[ -n "${pid}" ]]; then
      if ! kill -0 "${pid}" >/dev/null 2>&1; then
        rm -f "${QEMU_PIDFILE}"
        return 0
      fi
    elif [[ ! -f "${QEMU_PIDFILE}" ]]; then
      return 0
    fi
    sleep 2
  done

  log_error "Timed out waiting for guest to shut down (${SHUTDOWN_WAIT_TIMEOUT_SECS}s)."
  return 1
}

cleanup_vm() {
  local pid=""

  rm -f "${ASKPASS_SCRIPT}"

  if [[ -f "${QEMU_PIDFILE}" ]]; then
    pid="$(tr -d '[:space:]' <"${QEMU_PIDFILE}")"
  fi

  if [[ -n "${pid}" ]] && kill -0 "${pid}" >/dev/null 2>&1; then
    if [[ "${FORCE_KILL_ON_EXIT}" == "1" ]]; then
      log_warn "QEMU (pid ${pid}) still running, force killing as requested."
      kill "${pid}" >/dev/null 2>&1 || true
    else
      log_warn "QEMU (pid ${pid}) still running; leaving it alive for inspection."
      log_warn "  PID file   : ${QEMU_PIDFILE}"
      log_warn "  Serial log : ${QEMU_SERIAL_LOG}"
      log_warn "To force kill on next run, set FORCE_KILL_ON_EXIT=1."
    fi
  fi
}

need_cmd curl
need_cmd qemu-img
need_cmd qemu-system-x86_64
need_cmd rg
need_cmd python3
need_cmd ssh
need_cmd scp
need_cmd setsid

mkdir -p "${WORK_DIR}"

if [[ ! -f "${IMAGE_PATH}" ]]; then
  log_info "Downloading image to ${IMAGE_PATH}"
  curl -fL --progress-bar -o "${IMAGE_PATH}" "${IMAGE_URL}"
else
  log_info "Reusing existing image: ${IMAGE_PATH}"
fi

current_size="$(virtual_size_bytes "${IMAGE_PATH}")"
target_size="$(size_to_bytes "${TARGET_SIZE}")"

if (( current_size < target_size )); then
  log_info "Resizing qcow2 in place to ${TARGET_SIZE}"
  qemu-img resize "${IMAGE_PATH}" "${TARGET_SIZE}"
else
  log_info "Image virtual size already >= ${TARGET_SIZE}, skipping resize"
fi

log_success "Image preparation finished"
log_info "  Image path: ${IMAGE_PATH}"

if [[ "${AUTO_BOOT}" != "1" || "${AUTO_RESIZE_IN_GUEST}" != "1" ]]; then
  log_info "Next steps:"
  log_info "  1. ./run_vm.sh"
  log_info "  2. scp -P ${SSH_PORT} internal/grow_rootfs.sh ${VM_USER}@127.0.0.1:~/ && ssh -p ${SSH_PORT} ${VM_USER}@127.0.0.1 'bash ~/grow_rootfs.sh'"
  exit 0
fi

if [[ ! -x "${RUN_VM_SCRIPT}" ]]; then
  log_error "Boot helper script is missing or not executable: ${RUN_VM_SCRIPT}"
  exit 1
fi
if [[ ! -f "${GROW_SCRIPT}" ]]; then
  log_error "Guest grow script is missing: ${GROW_SCRIPT}"
  exit 1
fi
if [[ ! -f "${SELINUX_SCRIPT}" ]]; then
  log_error "Guest SELinux script is missing: ${SELINUX_SCRIPT}"
  exit 1
fi
if [[ ! -f "${PATH_SCRIPT}" ]]; then
  log_error "Guest PATH script is missing: ${PATH_SCRIPT}"
  exit 1
fi
if [[ ! -f "${BANNER_SCRIPT}" ]]; then
  log_error "Guest banner script is missing: ${BANNER_SCRIPT}"
  exit 1
fi

trap cleanup_vm EXIT
create_askpass_script

rm -f "${QEMU_PIDFILE}" "${QEMU_SERIAL_LOG}"

log_info "Starting guest root disk auto-grow workflow"
log_info "  User     : ${VM_USER}"
log_info "  SSH port : ${SSH_PORT}"
log_info "  Image    : ${IMAGE_PATH}"

IMAGE_PATH="${IMAGE_PATH}" \
IMAGE_URL="${IMAGE_URL}" \
VM_BACKGROUND=1 \
SSH_PORT="${SSH_PORT}" \
"${RUN_VM_SCRIPT}"

log_info "Waiting for guest SSH to become ready..."
wait_for_ssh
log_success "Guest SSH is ready"

log_info "Uploading grow_rootfs.sh to the guest..."
ssh_with_password scp "${SSH_COMMON_OPTS[@]}" -P "${SSH_PORT}" \
  "${GROW_SCRIPT}" "${VM_USER}@127.0.0.1:~/grow_rootfs.sh"
log_success "grow_rootfs.sh uploaded"

log_info "Running grow_rootfs.sh inside the guest..."
ssh_with_password ssh "${SSH_COMMON_OPTS[@]}" -p "${SSH_PORT}" \
  "${VM_USER}@127.0.0.1" \
  'chmod +x ~/grow_rootfs.sh && ~/grow_rootfs.sh'
log_success "Guest root filesystem expansion finished"

log_info "Uploading setup_selinux.sh to the guest..."
ssh_with_password scp "${SSH_COMMON_OPTS[@]}" -P "${SSH_PORT}" \
  "${SELINUX_SCRIPT}" "${VM_USER}@127.0.0.1:~/setup_selinux.sh"
log_success "setup_selinux.sh uploaded"

log_info "Switching guest SELinux to permissive..."
ssh_with_password ssh "${SSH_COMMON_OPTS[@]}" -p "${SSH_PORT}" \
  "${VM_USER}@127.0.0.1" \
  'chmod +x ~/setup_selinux.sh && ~/setup_selinux.sh'
log_success "Guest SELinux is now permissive (persistent)"

log_info "Uploading setup_path.sh to the guest..."
ssh_with_password scp "${SSH_COMMON_OPTS[@]}" -P "${SSH_PORT}" \
  "${PATH_SCRIPT}" "${VM_USER}@127.0.0.1:~/setup_path.sh"
log_success "setup_path.sh uploaded"

log_info "Adding /usr/local/{sbin,bin} to login PATH and sudo secure_path..."
ssh_with_password ssh "${SSH_COMMON_OPTS[@]}" -p "${SSH_PORT}" \
  "${VM_USER}@127.0.0.1" \
  'chmod +x ~/setup_path.sh && ~/setup_path.sh'
log_success "Guest PATH setup finished"

log_info "Uploading setup_banner.sh to the guest..."
ssh_with_password scp "${SSH_COMMON_OPTS[@]}" -P "${SSH_PORT}" \
  "${BANNER_SCRIPT}" "${VM_USER}@127.0.0.1:~/setup_banner.sh"
log_success "setup_banner.sh uploaded"

log_info "Installing welcome banner inside the guest..."
ssh_with_password ssh "${SSH_COMMON_OPTS[@]}" -p "${SSH_PORT}" \
  "${VM_USER}@127.0.0.1" \
  'chmod +x ~/setup_banner.sh && ~/setup_banner.sh'
log_success "Welcome banner installed inside the guest"

log_info "Requesting graceful shutdown from the guest..."
ssh_with_password ssh "${SSH_COMMON_OPTS[@]}" -p "${SSH_PORT}" \
  "${VM_USER}@127.0.0.1" \
  'sudo shutdown -h now' >/dev/null 2>&1 || true

wait_for_shutdown
log_success "Guest has shut down cleanly"

rm -f "${ASKPASS_SCRIPT}"
trap - EXIT

log_success "All done:"
log_success "  1. Image downloaded"
log_success "  2. qcow2 resized to ${TARGET_SIZE}"
log_success "  3. VM booted and guest root filesystem expanded"
log_success "  4. Guest SELinux set to permissive"
log_success "  5. /usr/local/{sbin,bin} added to login PATH and sudo secure_path"
log_success "  6. Welcome banner installed inside the guest"
log_success "  7. VM powered off cleanly"
log_info "You can now run ./run_vm.sh to start the dev VM"
