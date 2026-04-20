#!/usr/bin/env bash

set -euo pipefail

LOG_TAG="setup_path"

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

# 1. Make sure every interactive login shell has /usr/local/{sbin,bin}
#    on PATH. The RHEL 9 / OpenCloudOS 9 /etc/profile usually does this
#    already, but we drop a dedicated profile.d file to make it
#    tamper-resistant and explicit.
PROFILE_FILE="/etc/profile.d/cubesandbox-path.sh"
log_info "Installing login shell PATH override at ${PROFILE_FILE}"
cat > "${PROFILE_FILE}" <<'PROFILE_EOF'
# Ensure /usr/local/{sbin,bin} are on PATH for every login shell.
# Installed by dev-env/internal/setup_path.sh (Cube Sandbox dev env).
for _cube_dir in /usr/local/sbin /usr/local/bin; do
  case ":${PATH}:" in
    *":${_cube_dir}:"*) ;;
    *) PATH="${_cube_dir}:${PATH}" ;;
  esac
done
unset _cube_dir
export PATH
PROFILE_EOF
chmod 0644 "${PROFILE_FILE}"
log_success "Login shell PATH override installed"

# 2. Make sudo honor /usr/local/bin too. On RHEL 9 / OpenCloudOS 9 the
#    default secure_path is `/sbin:/bin:/usr/sbin:/usr/bin` which does
#    not include /usr/local/bin. That breaks things like
#      sudo cubemastercli ...
#    when the user has not switched to root. We drop a sudoers.d file
#    that widens secure_path, validated with `visudo -cf` before it is
#    installed so a syntax error cannot lock us out of sudo.
SUDOERS_TARGET="/etc/sudoers.d/00-cubesandbox-path"
SUDOERS_TMP="$(mktemp)"
cat > "${SUDOERS_TMP}" <<'SUDOERS_EOF'
# Extend sudo's secure_path so tools installed under /usr/local/{sbin,bin}
# (e.g. cubemastercli) work via `sudo` without specifying an absolute path.
# Installed by dev-env/internal/setup_path.sh (Cube Sandbox dev env).
Defaults secure_path="/usr/local/sbin:/usr/local/bin:/sbin:/bin:/usr/sbin:/usr/bin"
SUDOERS_EOF

log_info "Validating ${SUDOERS_TARGET} with visudo -cf"
if visudo -cf "${SUDOERS_TMP}" >/dev/null; then
  install -m 0440 "${SUDOERS_TMP}" "${SUDOERS_TARGET}"
  rm -f "${SUDOERS_TMP}"
  log_success "sudo secure_path updated at ${SUDOERS_TARGET}"
else
  rm -f "${SUDOERS_TMP}"
  log_error "Generated sudoers snippet failed visudo validation; aborting."
  exit 1
fi
