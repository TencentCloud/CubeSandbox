#!/usr/bin/env bash

set -euo pipefail

LOG_TAG="setup_banner"

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

BANNER_FILE="/etc/profile.d/cubesandbox-banner.sh"

log_info "Installing welcome banner at ${BANNER_FILE}"

cat > "${BANNER_FILE}" <<'BANNER_EOF'
# Cube Sandbox development environment welcome banner.
# Sourced by login shells via /etc/profile.d/.

case $- in
  *i*)
    if [ -t 1 ]; then
      printf '\n'
      printf '\033[0;36m'
      cat <<'ART'
  ____      _          ____                  _ _
 / ___|   _| |__   ___/ ___|  __ _ _ __   __| | |__   _____  __
| |  | | | | '_ \ / _ \___ \ / _` | '_ \ / _` | '_ \ / _ \ \/ /
| |__| |_| | |_) |  __/___) | (_| | | | | (_| | |_) | (_) >  <
 \____\__,_|_.__/ \___|____/ \__,_|_| |_|\__,_|_.__/ \___/_/\_\
ART
      printf '\033[0m\n'
      printf '\033[1;32mWelcome to the Cube Sandbox development environment!\033[0m\n'
      printf '\n'
      printf '  * Guest OS      : OpenCloudOS 9 (qcow2, 100G root disk)\n'
      printf '  * Project repo  : https://github.com/tencentcloud/CubeSandbox\n'
      printf '  * Install cmd   : curl -sL https://cnb.cool/CubeSandbox/CubeSandbox/-/git/raw/master/deploy/one-click/online-install.sh | MIRROR=cn bash\n'
      printf '  * Cube API      : http://127.0.0.1:3000 (from guest) / http://127.0.0.1:13000 (from host)\n'
      printf '\n'
      printf '\033[0;33mThis VM is a disposable dev environment; do not use it for production.\033[0m\n'
      printf '\n'
    fi
    ;;
esac
BANNER_EOF

chmod 0644 "${BANNER_FILE}"

log_success "Welcome banner installed at ${BANNER_FILE}"
