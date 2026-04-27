#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Build and deploy the WebUI into the CubeSandbox dev VM.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEV_ENV_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="${REPO_ROOT:-$(cd "${DEV_ENV_DIR}/.." && pwd)}"
WORK_DIR="${WORK_DIR:-${DEV_ENV_DIR}/.workdir}"

VM_USER="${VM_USER:-opencloudos}"
VM_PASSWORD="${VM_PASSWORD:-opencloudos}"
SSH_HOST="${SSH_HOST:-127.0.0.1}"
SSH_PORT="${SSH_PORT:-10022}"

TOOLBOX_ROOT="${TOOLBOX_ROOT:-/usr/local/services/cubetoolbox}"
REMOTE_WEBUI_DIR="${REMOTE_WEBUI_DIR:-${TOOLBOX_ROOT}/webui}"
WEB_DIR="${WEB_DIR:-${REPO_ROOT}/web}"

WEB_SYNC_BUILD="${WEB_SYNC_BUILD:-1}"
WEB_UI_IMAGE="${WEB_UI_IMAGE:-cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat}"
WEB_UI_CONTAINER_NAME="${WEB_UI_CONTAINER_NAME:-cube-webui}"
WEB_UI_HOST_PORT="${WEB_UI_HOST_PORT:-12088}"
WEB_UI_UPSTREAM="${WEB_UI_UPSTREAM:-http://host.docker.internal:3000}"

ASKPASS_SCRIPT="${WORK_DIR}/.ssh-askpass.sh"
TMP_DIR="${WORK_DIR}/webui-sync"
REMOTE_STAGE_DIR="/tmp/cube-webui-sync"

LOG_TAG="sync_web_to_vm"

if [[ -t 1 && -t 2 ]]; then
  LOG_COLOR_RESET=$'\033[0m'
  LOG_COLOR_INFO=$'\033[0;36m'
  LOG_COLOR_SUCCESS=$'\033[0;32m'
  LOG_COLOR_ERROR=$'\033[0;31m'
else
  LOG_COLOR_RESET=""
  LOG_COLOR_INFO=""
  LOG_COLOR_SUCCESS=""
  LOG_COLOR_ERROR=""
fi

_log() {
  local color="$1"
  local level="$2"
  shift 2
  printf '%s[%s][%s]%s %s\n' \
    "${color}" "${LOG_TAG}" "${level}" "${LOG_COLOR_RESET}" "$*"
}

log_info()    { _log "${LOG_COLOR_INFO}" "INFO" "$@"; }
log_success() { _log "${LOG_COLOR_SUCCESS}" "OK" "$@"; }
log_error()   { _log "${LOG_COLOR_ERROR}" "ERROR" "$@" >&2; }

usage() {
  cat <<EOF
Usage: $(basename "$0")

Build and deploy WebUI into the dev VM.

Environment overrides:
  WEB_SYNC_BUILD        default: ${WEB_SYNC_BUILD} (set 0 to skip npm build)
  WEB_UI_HOST_PORT      default: ${WEB_UI_HOST_PORT}
  WEB_UI_UPSTREAM       default: ${WEB_UI_UPSTREAM}
  WEB_UI_IMAGE          default: ${WEB_UI_IMAGE}
  WEB_UI_CONTAINER_NAME default: ${WEB_UI_CONTAINER_NAME}
  SSH_HOST, SSH_PORT    default: ${SSH_HOST}:${SSH_PORT}
  VM_USER, VM_PASSWORD  default: ${VM_USER} / ${VM_PASSWORD}
  TOOLBOX_ROOT          default: ${TOOLBOX_ROOT}
EOF
}

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    log_error "Missing required command: $1"
    exit 1
  fi
}

escape_sed() {
  printf '%s' "$1" | sed 's/[\/&]/\\&/g'
}

cleanup() {
  rm -f "${ASKPASS_SCRIPT}"
}

setup_ssh_support() {
  need_cmd ssh
  need_cmd scp
  need_cmd setsid

  mkdir -p "${WORK_DIR}" "${TMP_DIR}"

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

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" || "${1:-}" == "help" ]]; then
  usage
  exit 0
fi

need_cmd tar
need_cmd sed

if [[ "${WEB_SYNC_BUILD}" != "0" ]]; then
  log_info "Building WebUI"
  make -C "${REPO_ROOT}" web-build
else
  log_info "Skipping WebUI build because WEB_SYNC_BUILD=0"
fi

for required_path in \
  "${WEB_DIR}/dist/index.html" \
  "${REPO_ROOT}/deploy/one-click/webui/nginx.conf" \
  "${REPO_ROOT}/deploy/one-click/webui/docker-compose.yaml.template"
do
  if [[ ! -e "${required_path}" ]]; then
    log_error "Required path not found: ${required_path}"
    exit 1
  fi
done

rm -rf "${TMP_DIR}"
mkdir -p "${TMP_DIR}"

tar -C "${WEB_DIR}/dist" -czf "${TMP_DIR}/dist.tar.gz" .
cp "${REPO_ROOT}/deploy/one-click/webui/nginx.conf" "${TMP_DIR}/nginx.conf"
cp "${REPO_ROOT}/deploy/one-click/webui/docker-compose.yaml.template" "${TMP_DIR}/docker-compose.yaml.template"

sed \
  -e "s#__WEB_UI_UPSTREAM__#$(escape_sed "${WEB_UI_UPSTREAM}")#g" \
  "${TMP_DIR}/nginx.conf" > "${TMP_DIR}/nginx.generated.conf"

sed \
  -e "s#__WEB_UI_IMAGE__#$(escape_sed "${WEB_UI_IMAGE}")#g" \
  -e "s#__WEB_UI_CONTAINER_NAME__#$(escape_sed "${WEB_UI_CONTAINER_NAME}")#g" \
  -e "s#__WEB_UI_HOST_PORT__#$(escape_sed "${WEB_UI_HOST_PORT}")#g" \
  -e "s#__WEB_UI_DIST_DIR__#$(escape_sed "${REMOTE_WEBUI_DIR}/dist")#g" \
  -e "s#__WEB_UI_NGINX_CONF__#$(escape_sed "${REMOTE_WEBUI_DIR}/nginx.generated.conf")#g" \
  "${TMP_DIR}/docker-compose.yaml.template" > "${TMP_DIR}/docker-compose.yaml"

setup_ssh_support

log_info "Uploading WebUI artifacts to ${VM_USER}@${SSH_HOST}:${SSH_PORT}"
run_ssh "sudo rm -rf '${REMOTE_STAGE_DIR}' && mkdir -p '${REMOTE_STAGE_DIR}'"
run_scp \
  "${TMP_DIR}/dist.tar.gz" \
  "${TMP_DIR}/nginx.conf" \
  "${TMP_DIR}/nginx.generated.conf" \
  "${TMP_DIR}/docker-compose.yaml.template" \
  "${TMP_DIR}/docker-compose.yaml" \
  "${VM_USER}@${SSH_HOST}:${REMOTE_STAGE_DIR}/"

log_info "Installing and restarting WebUI in VM"
run_ssh "
  set -e
  sudo mkdir -p '${REMOTE_WEBUI_DIR}/dist'
  sudo rm -rf '${REMOTE_WEBUI_DIR}/dist'
  sudo mkdir -p '${REMOTE_WEBUI_DIR}/dist'
  sudo tar -xzf '${REMOTE_STAGE_DIR}/dist.tar.gz' -C '${REMOTE_WEBUI_DIR}/dist'
  sudo install -m 0644 '${REMOTE_STAGE_DIR}/nginx.conf' '${REMOTE_WEBUI_DIR}/nginx.conf'
  sudo install -m 0644 '${REMOTE_STAGE_DIR}/nginx.generated.conf' '${REMOTE_WEBUI_DIR}/nginx.generated.conf'
  sudo install -m 0644 '${REMOTE_STAGE_DIR}/docker-compose.yaml.template' '${REMOTE_WEBUI_DIR}/docker-compose.yaml.template'
  sudo install -m 0644 '${REMOTE_STAGE_DIR}/docker-compose.yaml' '${REMOTE_WEBUI_DIR}/docker-compose.yaml'
  cd '${REMOTE_WEBUI_DIR}'
  sudo docker compose down --remove-orphans >/dev/null 2>&1 || true
  sudo docker compose up -d
  sudo rm -rf '${REMOTE_STAGE_DIR}'
"

log_info "Checking WebUI from inside VM"
run_ssh "
  set -e
  curl -fsS 'http://127.0.0.1:${WEB_UI_HOST_PORT}/' >/dev/null
  curl -fsS 'http://127.0.0.1:${WEB_UI_HOST_PORT}/cubeapi/v1/health' >/dev/null
"

log_success "WebUI synced. Open: http://127.0.0.1:${WEB_UI_HOST_PORT}"
