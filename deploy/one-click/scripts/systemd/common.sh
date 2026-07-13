#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SYSTEMD_HELPER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOOLBOX_ROOT="/usr/local/services/cubetoolbox"
ENV_FILE="${ONE_CLICK_RUNTIME_ENV_FILE:-${TOOLBOX_ROOT}/.one-click.env}"
UNIT_SOURCE_DIR="${ONE_CLICK_SYSTEMD_UNIT_SOURCE_DIR:-${TOOLBOX_ROOT}/systemd}"
UNIT_INSTALL_DIR="${ONE_CLICK_SYSTEMD_UNIT_INSTALL_DIR:-/etc/systemd/system}"
SYSTEMD_RUNTIME_DIR="${CUBE_SANDBOX_SYSTEMD_RUNTIME_DIR:-/run/cube-sandbox-systemd}"
SYSTEMD_LOG_DIR="${ONE_CLICK_LOG_DIR:-/var/log/cube-sandbox-one-click}"

log() {
  echo "[one-click-systemd] $*" >&2
}

die() {
  echo "[one-click-systemd] ERROR: $*" >&2
  exit 1
}

# shellcheck source=../common/validation.sh
source "${SYSTEMD_HELPER_DIR}/../common/validation.sh"

require_cmd() {
  local cmd="$1"
  command -v "${cmd}" >/dev/null 2>&1 || die "required command not found: ${cmd}"
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

ensure_executable() {
  local path="$1"
  [[ -x "${path}" ]] || die "required executable not found: ${path}"
}

is_reserved_nameserver() {
  local nameserver="${1:-}"
  shift || true

  [[ -n "${nameserver}" ]] || return 0
  [[ "${nameserver}" == 127.* ]] && return 0
  [[ "${nameserver}" == "::1" ]] && return 0
  [[ "${nameserver}" == "0:0:0:0:0:0:0:1" ]] && return 0

  local reserved
  for reserved in "$@"; do
    [[ -n "${reserved}" && "${nameserver}" == "${reserved}" ]] && return 0
  done
  return 1
}

load_runtime_env() {
  local had_nounset=0
  if [[ ! -f "${ENV_FILE}" ]]; then
    return 0
  fi

  [[ $- == *u* ]] && had_nounset=1
  set +u
  set -a
  # shellcheck disable=SC1090
  source "${ENV_FILE}"
  set +a
  if [[ "${had_nounset}" == "1" ]]; then
    set -u
  fi
}

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
  # 8089 is the cubemaster protocol port (a fixed constant), NOT derived from
  # CUBEMASTER_ADDR -- that variable is the control node's local listen address;
  # using its port here was an accidental coupling that broke when they differed.
  local cubemaster_port=8089

  if [[ "${role}" != "compute" ]]; then
    printf '%s\n' "${default_addr}"
    return 0
  fi

  if [[ -n "${addr}" ]]; then
    validate_host_port "${addr}" "ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR"
    printf '%s\n' "${addr}"
    return 0
  fi

  if [[ -n "${ip}" ]]; then
    validate_ipv4_literal "${ip}" "ONE_CLICK_CONTROL_PLANE_IP"
    validate_host_port "${ip}:${cubemaster_port}" "ONE_CLICK_CONTROL_PLANE_IP-derived cubemaster address"
    printf '%s:%s\n' "${ip}" "${cubemaster_port}"
    return 0
  fi

  die "ONE_CLICK_CONTROL_PLANE_IP or ONE_CLICK_CONTROL_PLANE_CUBEMASTER_ADDR is required for compute role"
}

list_unit_files() {
  local dir="${1:-${UNIT_SOURCE_DIR}}"
  local unit
  shopt -s nullglob
  for unit in \
    "${dir}"/cube-sandbox-*.service \
    "${dir}"/cube-sandbox-*.target \
    "${dir}"/cube-sandbox-*.timer
  do
    [[ -f "${unit}" ]] && printf '%s\n' "${unit}"
  done
}

install_unit_file() {
  local unit_file="$1"
  ensure_file "${unit_file}"
  mkdir -p "${UNIT_INSTALL_DIR}"
  install -m 0644 "${unit_file}" "${UNIT_INSTALL_DIR}/$(basename "${unit_file}")"
}

remove_unit_file() {
  local unit_name="$1"
  rm -f "${UNIT_INSTALL_DIR}/${unit_name}"
}

ensure_systemd_runtime_dirs() {
  mkdir -p "${SYSTEMD_RUNTIME_DIR}" "${SYSTEMD_LOG_DIR}"
}

escape_sed() {
  printf '%s' "$1" | sed 's/[\/&]/\\&/g'
}

command_output_has_exact_line() {
  local needle="$1"
  shift

  require_cmd grep

  local output
  output="$("$@" 2>/dev/null || true)"
  [[ -n "${output}" ]] || return 1
  grep -Fxq -- "${needle}" <<<"${output}"
}

command_output_contains_fixed_string() {
  local needle="$1"
  shift

  require_cmd grep

  local output
  output="$("$@" 2>/dev/null || true)"
  [[ -n "${output}" ]] || return 1
  grep -Fq -- "${needle}" <<<"${output}"
}

container_exists() {
  local name="$1"
  command_output_has_exact_line "${name}" docker ps -a --format '{{.Names}}'
}

docker_rm_if_exists() {
  local name="$1"
  local stop_timeout="${2:-10}"
  if ! container_exists "${name}"; then
    return 0
  fi
  # Graceful path: stop with bounded timeout, then remove. We avoid
  # `docker rm -f` so stateful workloads can flush. Callers that invoke this
  # in a stop hook should pair it with an appropriate TimeoutStopSec on the
  # owning systemd unit.
  docker stop -t "${stop_timeout}" "${name}" >/dev/null 2>&1 || true
  docker rm "${name}" >/dev/null 2>&1 || true
}

docker_image_exists() {
  local image_ref="$1"
  docker image inspect "${image_ref}" >/dev/null 2>&1
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

  command_output_has_exact_line "${pid}" pgrep -f -- "${pattern}"
}

refresh_pidfile_from_pattern() {
  local pid_file="$1"
  local pattern="$2"
  local retries="${3:-20}"
  local delay="${4:-1}"
  local pid
  local i

  for ((i = 1; i <= retries; i++)); do
    if pid="$(first_pid_by_pattern "${pattern}")"; then
      printf '%s\n' "${pid}" > "${pid_file}"
      return 0
    fi
    sleep "${delay}"
  done

  return 1
}

stop_pid_with_timeout() {
  local pid="$1"
  local timeout="${2:-20}"
  local force_signal="${3:--9}"
  local i

  kill "${pid}" >/dev/null 2>&1 || true
  for ((i = 1; i <= timeout; i++)); do
    if ! kill -0 "${pid}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done

  if kill -0 "${pid}" >/dev/null 2>&1; then
    kill "${force_signal}" "${pid}" >/dev/null 2>&1 || true
  fi
}

# True if the process ${pid} was launched with exactly the argument ${want}
# (NUL-delimited match against /proc/<pid>/cmdline). Unlike a `pgrep -f` substring
# scan this is an exact, per-argument comparison, so it cannot be fooled by an
# unrelated process that merely embeds the string somewhere in its argv, and it
# does not treat the argument as a regex.
pid_cmdline_has_arg() {
  local pid="$1"
  local want="$2"
  local cmdline="/proc/${pid}/cmdline"
  local arg

  [[ -n "${pid}" && -r "${cmdline}" ]] || return 1
  while IFS= read -r -d '' arg; do
    [[ "${arg}" == "${want}" ]] && return 0
  done < "${cmdline}"
  return 1
}

# Stop a dnsmasq instance we launched directly, identified by its config-file
# path. Only signal a PID we can confirm belongs to that instance: prefer the
# pid-file when it still points at our process, otherwise scan the process table
# for candidates. In both cases the candidate is confirmed by an exact
# "--conf-file=<path>" argv match (pid_cmdline_has_arg), not a substring/regex
# scan, so we never SIGTERM/SIGKILL an unrelated process the kernel recycled the
# PID onto, or one that merely has "dnsmasq" (or the path) somewhere in its argv.
stop_dnsmasq_by_conf() {
  local pid_file="$1"
  local conf_path="$2"
  local timeout="${3:-10}"
  local want_arg="--conf-file=${conf_path}"
  local pid=""

  if [[ -f "${pid_file}" ]]; then
    pid="$(<"${pid_file}")"
    if [[ -z "${pid}" ]] || ! pid_cmdline_has_arg "${pid}" "${want_arg}"; then
      pid=""
    fi
  fi
  if [[ -z "${pid}" ]]; then
    # pgrep -f only narrows the candidate set (its argument is a regex and can
    # over-match); pid_cmdline_has_arg then confirms each candidate exactly.
    local cand
    for cand in $(pgrep -f -- "conf-file=${conf_path}" 2>/dev/null || true); do
      if pid_cmdline_has_arg "${cand}" "${want_arg}"; then
        pid="${cand}"
        break
      fi
    done
  fi
  if [[ -n "${pid}" ]]; then
    stop_pid_with_timeout "${pid}" "${timeout}" || true
  fi
}

wait_for_http() {
  local url="$1"
  local retries="${2:-30}"
  local delay="${3:-2}"
  local curl_args="${4:-}"
  local i
  local -a extra_args=()
  local last_err=""

  if [[ -n "${curl_args}" ]]; then
    # shellcheck disable=SC2206
    extra_args=(${curl_args})
  fi

  for ((i = 1; i <= retries; i++)); do
    if last_err="$(curl -fsS "${extra_args[@]}" "${url}" 2>&1 >/dev/null)"; then
      return 0
    fi
    sleep "${delay}"
  done
  log "ERROR wait_for_http timeout: url=${url} waited=$((retries * delay))s last_curl_error=${last_err:-<empty>}"
  return 1
}

wait_for_tcp_port() {
  local port="$1"
  local retries="${2:-30}"
  local delay="${3:-2}"
  local i

  require_cmd ss
  for ((i = 1; i <= retries; i++)); do
    if command_output_contains_fixed_string ":${port}" ss -lnt "( sport = :${port} )"; then
      return 0
    fi
    sleep "${delay}"
  done
  log "ERROR wait_for_tcp_port timeout: port=${port} waited=$((retries * delay))s no_listener_observed"
  return 1
}

wait_for_udp_port() {
  local address="$1"
  local port="$2"
  local retries="${3:-30}"
  local delay="${4:-2}"
  local i

  require_cmd ss
  for ((i = 1; i <= retries; i++)); do
    if command_output_contains_fixed_string "${address}:${port}" ss -lnu "( sport = :${port} )"; then
      return 0
    fi
    sleep "${delay}"
  done
  log "ERROR wait_for_udp_port timeout: address=${address} port=${port} waited=$((retries * delay))s no_listener_observed"
  return 1
}

wait_for_container_health() {
  local container="$1"
  local retries="${2:-40}"
  local delay="${3:-2}"
  local status=""
  local i

  require_cmd docker
  for ((i = 1; i <= retries; i++)); do
    status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "${container}" 2>/dev/null || true)"
    if [[ "${status}" == "healthy" || "${status}" == "running" ]]; then
      return 0
    fi
    sleep "${delay}"
  done
  log "ERROR wait_for_container_health timeout: container=${container} waited=$((retries * delay))s last_status=${status:-<unknown>}"
  return 1
}

render_template() {
  local template="$1"
  local output="$2"
  shift 2
  ensure_file "${template}"
  prepare_file_output "${output}"
  sed "$@" "${template}" > "${output}"
  ensure_bind_mount_file "${output}"
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

systemd_target_for_role() {
  local role="${1:-$(one_click_deploy_role)}"
  case "${role}" in
    control)
      printf 'cube-sandbox-control.target\n'
      ;;
    compute)
      printf 'cube-sandbox-compute.target\n'
      ;;
    *)
      die "unsupported role for target resolution: ${role}"
      ;;
  esac
}

load_runtime_env
