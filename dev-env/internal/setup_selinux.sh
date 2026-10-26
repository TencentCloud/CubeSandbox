#!/usr/bin/env bash

set -euo pipefail

LOG_TAG="setup_selinux"

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

if [[ "${EUID}" -ne 0 ]]; then
  if command -v sudo >/dev/null 2>&1; then
    exec sudo bash "$0" "$@"
  fi

  log_error "This script must be run as root."
  exit 1
fi

# On OpenCloudOS 9 / RHEL 9, SELinux is enforcing by default.
# Once `container-selinux` is pulled in as a dependency of `docker`/`moby`,
# SELinux confines container processes to `container_file_t` labels.
# Any host directory bind-mounted into a container (e.g. Cube Sandbox mounts
# the SQL init dir into the mysql container as
# `/docker-entrypoint-initdb.d`) keeps its default `var_t` / `user_tmp_t`
# label and the container process is denied with `Permission denied`,
# which then restarts the container in a loop.
#
# Because this is a disposable development environment, the simplest and
# most robust fix is to flip SELinux to permissive mode. We do it both
# live (setenforce) and persistently (/etc/selinux/config), so that the
# fix survives a guest reboot.

if ! command -v getenforce >/dev/null 2>&1; then
  log_info "SELinux tooling not present, nothing to do."
  exit 0
fi

current_mode="$(getenforce 2>/dev/null || echo Unknown)"
log_info "Current SELinux mode: ${current_mode}"

if [[ "${current_mode}" == "Disabled" ]]; then
  log_info "SELinux is already disabled, skipping."
  exit 0
fi

if [[ "${current_mode}" != "Permissive" ]]; then
  log_info "Switching SELinux to permissive for the current boot..."
  if setenforce 0 2>/dev/null; then
    log_success "SELinux is now permissive at runtime"
  else
    log_warn "setenforce 0 failed; relying on the persistent config change below."
  fi
fi

SELINUX_CONFIG="/etc/selinux/config"
if [[ -f "${SELINUX_CONFIG}" ]]; then
  if grep -Eq '^SELINUX=enforcing' "${SELINUX_CONFIG}"; then
    log_info "Persisting SELinux=permissive in ${SELINUX_CONFIG}"
    sed -i 's/^SELINUX=enforcing/SELINUX=permissive/' "${SELINUX_CONFIG}"
    log_success "Persistent SELinux mode updated to permissive"
  elif grep -Eq '^SELINUX=permissive' "${SELINUX_CONFIG}"; then
    log_info "${SELINUX_CONFIG} already has SELINUX=permissive"
  else
    log_warn "Could not find SELINUX=enforcing line in ${SELINUX_CONFIG}; leaving file untouched."
  fi
else
  log_warn "${SELINUX_CONFIG} not found; cannot persist permissive mode across reboots."
fi

log_success "SELinux setup finished (mode: $(getenforce 2>/dev/null || echo Unknown))"
