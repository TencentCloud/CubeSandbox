#!/usr/bin/env bash
set -euo pipefail

ONE_CLICK_RUNTIME_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOLBOX_ROOT="${ONE_CLICK_TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
ENV_FILE="${ONE_CLICK_RUNTIME_ENV_FILE:-${TOOLBOX_ROOT}/.one-click.env}"

if [[ -f "${ENV_FILE}" ]]; then
  had_nounset=0
  [[ $- == *u* ]] && had_nounset=1
  set +u
  set -a
  # shellcheck disable=SC1090
  source "${ENV_FILE}"
  set +a
  if [[ "${had_nounset}" == "1" ]]; then
    set -u
  fi
fi

RUNTIME_DIR="${ONE_CLICK_RUNTIME_DIR:-/var/run/cube-sandbox-one-click}"
LOG_DIR="${ONE_CLICK_LOG_DIR:-/var/log/cube-sandbox-one-click}"

log() {
  echo "[one-click-runtime] $*" >&2
}

die() {
  echo "[one-click-runtime] ERROR: $*" >&2
  exit 1
}

require_cmd() {
  local cmd="$1"
  command -v "${cmd}" >/dev/null 2>&1 || die "required command not found: ${cmd}"
}

one_click_cow_required_commands() {
  printf '%s\n' \
    mkfs.ext4 \
    mount \
    umount \
    losetup
}

cubelet_storage_backend_from_config() {
  local config_path="$1"
  ensure_file "${config_path}"
  sed -nE 's/^[[:space:]]*storage_backend[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "${config_path}" | head -n 1
}

validate_cubelet_cow_startup_deps() {
  local config_path="$1"
  ensure_file "${config_path}"
  require_cmd rg
  require_cmd sed

  local storage_backend
  storage_backend="$(cubelet_storage_backend_from_config "${config_path}")"
  [[ "${storage_backend}" == "cubecow" ]] || return 0

  local cmds=()
  while IFS= read -r cmd; do
    [[ -n "${cmd}" ]] && cmds+=("${cmd}")
  done < <(one_click_cow_required_commands)

  local missing=()
  local cmd
  for cmd in "${cmds[@]}"; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      missing+=("${cmd}")
    fi
  done

  if [[ "${#missing[@]}" -gt 0 ]]; then
    die "cubelet cubecow startup dependency check failed for ${config_path}; missing commands in PATH: ${missing[*]} (required commands: ${cmds[*]})"
  fi

  log "cubelet cubecow startup dependencies OK: ${cmds[*]}"
}

require_root() {
  if [[ "${EUID}" -ne 0 ]]; then
    die "this script must run as root"
  fi
}

ensure_file() {
  local path="$1"
  [[ -f "${path}" ]] || die "required file not found: ${path}"
}

ensure_dir() {
  local path="$1"
  [[ -d "${path}" ]] || die "required directory not found: ${path}"
}

ensure_not_directory_for_file() {
  local path="$1"
  if [[ ! -d "${path}" ]]; then
    return 0
  fi

  if rmdir "${path}" 2>/dev/null; then
    log "removed empty directory at file path: ${path}"
    return 0
  fi

  die "expected file path is a non-empty directory: ${path}; move it away and retry"
}

prepare_file_output() {
  local path="$1"
  ensure_not_directory_for_file "${path}"
  mkdir -p "$(dirname "${path}")"
}

ensure_bind_mount_file() {
  local path="$1"
  ensure_not_directory_for_file "${path}"
  [[ -f "${path}" ]] || die "required bind mount source file not found: ${path}"
}

render_template_atomic() {
  local template="$1"
  local output="$2"
  shift 2

  ensure_file "${template}"
  prepare_file_output "${output}"

  local tmp="${output}.tmp.$$"
  rm -f "${tmp}"
  if ! sed "$@" "${template}" > "${tmp}"; then
    rm -f "${tmp}"
    die "failed to render template ${template} to ${output}"
  fi
  mv -f "${tmp}" "${output}"
  ensure_bind_mount_file "${output}"
}

mkdir -p "${RUNTIME_DIR}" "${LOG_DIR}"

one_click_deploy_role() {
  local role="${ONE_CLICK_DEPLOY_ROLE:-control}"
  case "${role}" in
    control|compute)
      printf '%s\n' "${role}"
      ;;
    *)
      die "unsupported ONE_CLICK_DEPLOY_ROLE: ${role}"
      ;;
  esac
}

is_compute_role() {
  [[ "$(one_click_deploy_role)" == "compute" ]]
}

resolve_control_plane_cubemaster_addr() {
  local role
  role="$(one_click_deploy_role)"
  local addr="${ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR:-}"
  local ip="${ONE_CLICK_CONTROL_PLANE_IP:-}"
  local default_addr="${CUBEMASTER_ADDR:-127.0.0.1:8089}"
  local port="${default_addr##*:}"

  if [[ "${role}" != "compute" ]]; then
    printf '%s\n' "${default_addr}"
    return 0
  fi

  if [[ -n "${addr}" ]]; then
    printf '%s\n' "${addr}"
    return 0
  fi

  if [[ -n "${ip}" ]]; then
    printf '%s:%s\n' "${ip}" "${port}"
    return 0
  fi

  die "ONE_CLICK_CONTROL_PLANE_IP or ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR is required for compute role"
}

start_with_pidfile() {
  local name="$1"
  local cmd="$2"
  local pid_file="${RUNTIME_DIR}/${name}.pid"
  local log_file="${LOG_DIR}/${name}.log"
  local clean_path="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
  local clean_home="${HOME:-/root}"
  local clean_lang="${LANG:-C.UTF-8}"

  if [[ -f "${pid_file}" ]]; then
    local pid
    pid="$(<"${pid_file}")"
    if [[ -n "${pid}" ]] && kill -0 "${pid}" >/dev/null 2>&1; then
      log "${name} already running pid=${pid}"
      return 0
    fi
    rm -f "${pid_file}"
  fi

  nohup env -i \
    PATH="${clean_path}" \
    HOME="${clean_home}" \
    LANG="${clean_lang}" \
    SHELL="/bin/bash" \
    bash -c "${cmd}" >"${log_file}" 2>&1 &
  local new_pid=$!
  echo "${new_pid}" >"${pid_file}"
  log "started ${name} pid=${new_pid} log=${log_file}"
}

first_pid_by_pattern() {
  local pattern="$1"
  local pids=()

  mapfile -t pids < <(pgrep -f -- "${pattern}" || true)
  if [[ "${#pids[@]}" -eq 0 ]]; then
    return 1
  fi

  printf '%s\n' "${pids[0]}"
}

pid_matches_pattern() {
  local pid="$1"
  local pattern="${2:-}"

  if [[ -z "${pattern}" ]]; then
    return 0
  fi

  pgrep -f -- "${pattern}" | rg -x -- "${pid}" >/dev/null 2>&1
}

refresh_pidfile_from_pattern() {
  local name="$1"
  local pattern="$2"
  local retries="${3:-20}"
  local delay="${4:-1}"
  local pid_file="${RUNTIME_DIR}/${name}.pid"
  local pid
  local i

  for ((i = 1; i <= retries; i++)); do
    if pid="$(first_pid_by_pattern "${pattern}")"; then
      printf '%s\n' "${pid}" > "${pid_file}"
      log "refreshed ${name} pid=${pid}"
      return 0
    fi
    sleep "${delay}"
  done

  return 1
}

stop_by_pidfile() {
  local name="$1"
  local pattern="${2:-}"
  local pid_file="${RUNTIME_DIR}/${name}.pid"
  local pid=""

  if [[ -f "${pid_file}" ]]; then
    pid="$(<"${pid_file}")"
    if [[ -n "${pid}" ]] && ! kill -0 "${pid}" >/dev/null 2>&1; then
      pid=""
    fi
    if [[ -n "${pid}" ]] && ! pid_matches_pattern "${pid}" "${pattern}"; then
      pid=""
    fi
  fi

  if [[ -z "${pid}" ]] && [[ -n "${pattern}" ]]; then
    pid="$(first_pid_by_pattern "${pattern}" || true)"
    if [[ -n "${pid}" ]]; then
      printf '%s\n' "${pid}" > "${pid_file}"
    fi
  fi

  if [[ -z "${pid}" ]]; then
    rm -f "${pid_file}"
    return 0
  fi

  kill "${pid}" >/dev/null 2>&1 || true
  for _ in {1..20}; do
    if ! kill -0 "${pid}" >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  if kill -0 "${pid}" >/dev/null 2>&1; then
    kill -9 "${pid}" >/dev/null 2>&1 || true
  fi

  rm -f "${pid_file}"
}

container_exists() {
  local name="$1"
  docker ps -a --format '{{.Names}}' | rg -x "${name}" >/dev/null 2>&1
}

docker_rm_if_exists() {
  local name="$1"
  local stop_timeout="${2:-10}"
  if ! container_exists "${name}"; then
    return 0
  fi
  # Graceful path: ask the container to stop, then remove it. We deliberately
  # avoid `docker rm -f` (SIGKILL) so workloads like MySQL/Redis get to flush
  # state. The systemd units that own these containers set TimeoutStopSec to
  # cover this graceful stop.
  docker stop -t "${stop_timeout}" "${name}" >/dev/null 2>&1 || true
  docker rm "${name}" >/dev/null 2>&1 || true
}

wait_for_http() {
  local url="$1"
  local retries="${2:-30}"
  local delay="${3:-2}"
  local i
  for ((i = 1; i <= retries; i++)); do
    if curl -fsS "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep "${delay}"
  done
  return 1
}

wait_for_health() {
  local container="$1"
  local retries="${2:-40}"
  local delay="${3:-2}"
  local status
  local i
  for ((i = 1; i <= retries; i++)); do
    status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${container}" 2>/dev/null || true)"
    if [[ "${status}" == "healthy" || "${status}" == "running" ]]; then
      log "${container} is ${status}"
      return 0
    fi
    sleep "${delay}"
  done
  return 1
}
