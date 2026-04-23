#!/usr/bin/env bash
set -euo pipefail

ONE_CLICK_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${ONE_CLICK_LIB_DIR}/.." && pwd)"

log() {
  echo "[one-click] $*" >&2
}

die() {
  echo "[one-click] ERROR: $*" >&2
  exit 1
}

require_cmd() {
  local cmd="$1"
  command -v "${cmd}" >/dev/null 2>&1 || die "required command not found: ${cmd}"
}

require_root() {
  if [[ "${EUID}" -ne 0 ]]; then
    die "this script must run as root"
  fi
}

load_env_file() {
  local env_file="$1"
  local had_nounset=0
  [[ -n "${env_file}" ]] || return 0
  [[ -f "${env_file}" ]] || die "env file not found: ${env_file}"
  log "loading env file: ${env_file}"
  [[ $- == *u* ]] && had_nounset=1
  set +u
  set -a
  # shellcheck disable=SC1090
  source "${env_file}"
  set +a
  if [[ "${had_nounset}" == "1" ]]; then
    set -u
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

copy_file() {
  local src="$1"
  local dst="$2"
  ensure_file "${src}"
  mkdir -p "$(dirname "${dst}")"
  cp -f "${src}" "${dst}"
}

copy_dir_contents() {
  local src="$1"
  local dst="$2"
  ensure_dir "${src}"
  rm -rf "${dst}"
  mkdir -p "${dst}"
  cp -a "${src}/." "${dst}/"
}

latest_git_revision() {
  local repo_root="$1"
  if command -v git >/dev/null 2>&1 && git -C "${repo_root}" rev-parse --short HEAD >/dev/null 2>&1; then
    git -C "${repo_root}" rev-parse --short HEAD
    return 0
  fi
  date +%Y%m%d-%H%M%S
}

container_exists() {
  local name="$1"
  docker ps -a --format '{{.Names}}' | rg -x "${name}" >/dev/null 2>&1
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

wait_for_pidfile() {
  local pid_file="$1"
  local retries="${2:-20}"
  local delay="${3:-1}"
  local i
  for ((i = 1; i <= retries; i++)); do
    if [[ -f "${pid_file}" ]]; then
      local pid
      pid="$(<"${pid_file}")"
      if [[ -n "${pid}" ]] && kill -0 "${pid}" >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep "${delay}"
  done
  return 1
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

upsert_env_kv() {
  local env_file="$1"
  local key="$2"
  local value="$3"
  local tmp_file
  tmp_file="$(mktemp)"
  local replaced=false

  if [[ -f "${env_file}" ]]; then
    while IFS= read -r line || [[ -n "${line}" ]]; do
      if [[ "${line}" == "${key}="* ]]; then
        printf '%s=%s\n' "${key}" "${value}" >> "${tmp_file}"
        replaced=true
      else
        printf '%s\n' "${line}" >> "${tmp_file}"
      fi
    done < "${env_file}"
  fi

  if [[ "${replaced}" != "true" ]]; then
    printf '%s=%s\n' "${key}" "${value}" >> "${tmp_file}"
  fi

  mv -f "${tmp_file}" "${env_file}"
}

detect_pkg_manager() {
  if command -v apt-get >/dev/null 2>&1; then
    printf 'apt'
  elif command -v yum >/dev/null 2>&1; then
    printf 'yum'
  else
    die "unsupported package manager: neither apt-get nor yum found"
  fi
}

install_ripgrep() {
  if command -v rg >/dev/null 2>&1; then
    return 0
  fi
  local pm
  pm="$(detect_pkg_manager)"
  log "installing ripgrep via ${pm}..."
  case "${pm}" in
    apt)
      apt-get update -qq && apt-get install -y -qq ripgrep
      ;;
    yum)
      yum install -y epel-release 2>/dev/null || true
      yum install -y ripgrep
      ;;
  esac
  command -v rg >/dev/null 2>&1 || die "failed to install ripgrep"
}

install_docker() {
  if command -v docker >/dev/null 2>&1; then
    return 0
  fi
  local pm
  pm="$(detect_pkg_manager)"
  log "installing docker via ${pm}..."
  case "${pm}" in
    apt)
      apt-get update -qq
      apt-get install -y -qq docker.io docker-compose
      ;;
    yum)
      yum install -y docker docker-compose
      ;;
  esac
  systemctl enable docker && systemctl start docker
  command -v docker >/dev/null 2>&1 || die "failed to install docker"
}

install_docker_compose() {
  if docker compose version >/dev/null 2>&1; then
    return 0
  fi
  if command -v docker-compose >/dev/null 2>&1; then
    return 0
  fi
  local pm
  pm="$(detect_pkg_manager)"
  log "installing docker-compose via ${pm}..."
  case "${pm}" in
    apt)
      apt-get update -qq && apt-get install -y -qq docker-compose
      ;;
    yum)
      yum install -y docker-compose
      ;;
  esac
  if ! docker compose version >/dev/null 2>&1 && ! command -v docker-compose >/dev/null 2>&1; then
    die "failed to install docker-compose"
  fi
}

install_dependencies() {
  log "checking and installing dependencies..."
  install_ripgrep
  install_docker
  install_docker_compose
}

detect_node_ip() {
  if [[ -n "${CUBE_SANDBOX_NODE_IP:-}" ]]; then
    printf '%s\n' "${CUBE_SANDBOX_NODE_IP}"
    return 0
  fi

  local detected_ip=""
  if command -v ip >/dev/null 2>&1; then
    local detected_iface
    detected_iface="$(detect_primary_interface || true)"
    if [[ -n "${detected_iface}" ]]; then
      detected_ip="$(ip -4 addr show dev "${detected_iface}" 2>/dev/null \
        | grep -oP 'inet \K[0-9.]+' | head -1 || true)"
      if [[ -n "${detected_ip}" ]]; then
        log "auto-detected node IP from ${detected_iface}: ${detected_ip}"
        printf '%s\n' "${detected_ip}"
        return 0
      fi
    fi

    detected_ip="$(ip -4 addr show scope global 2>/dev/null \
      | grep -oP 'inet \K[0-9.]+' | head -1 || true)"
  fi

  if [[ -n "${detected_ip}" ]]; then
    log "auto-detected node IP from first global IPv4 address: ${detected_ip}"
    printf '%s\n' "${detected_ip}"
    return 0
  fi

  die "cannot auto-detect node IP. Please set CUBE_SANDBOX_NODE_IP or pass --node-ip=<ip>"
}

detect_primary_interface() {
  # Honor explicit override first.
  if [[ -n "${CUBE_SANDBOX_ETH_NAME:-}" ]]; then
    printf '%s\n' "${CUBE_SANDBOX_ETH_NAME}"
    return 0
  fi

  # `ip` is required for auto-detection.
  command -v ip >/dev/null 2>&1 || return 1

  local iface
  # Preferred path: resolve interface from default IPv4 route.
  iface="$(ip -o -4 route show to default 2>/dev/null | awk '{print $5; exit}')"
  if [[ -n "${iface}" ]]; then
    printf '%s\n' "${iface}"
    return 0
  fi

  # Fallback: first non-loopback interface that is currently up.
  iface="$(ip -o link show up 2>/dev/null \
    | awk -F': ' '$2 != "lo" {print $2; exit}' \
    | cut -d@ -f1)"
  [[ -n "${iface}" ]] || return 1
  printf '%s\n' "${iface}"
}

ensure_kernel_vmlinux() {
  local vmlinux_path="$1"
  local default_dir="$2"

  if [[ -f "${vmlinux_path}" ]]; then
    return 0
  fi

  cat >&2 <<EOF

============================================================
  ERROR: Kernel vmlinux file not found!
============================================================

  Missing: ${vmlinux_path}

  The vmlinux file is a required Linux kernel image used to
  boot guest VMs. You must provide it before building.

  How to fix:

    Option A — Place it in the default location:

      cp /path/to/your/vmlinux ${default_dir}/vmlinux

    Option B — Set a custom path via environment variable:

      export ONE_CLICK_CUBE_KERNEL_VMLINUX=/path/to/vmlinux

  Then re-run the build script.

  For more details, see: docs/guide/one-click-deploy.md
============================================================

EOF
  exit 1
}
