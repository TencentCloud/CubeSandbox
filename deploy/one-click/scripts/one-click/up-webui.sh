#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"
# shellcheck source=./webui-compose-lib.sh
source "${SCRIPT_DIR}/webui-compose-lib.sh"

require_root
require_cmd docker
require_cmd rg
require_cmd sed
require_cmd ss

WEB_UI_ENABLE="${WEB_UI_ENABLE:-1}"
if [[ "${WEB_UI_ENABLE}" != "1" ]]; then
  log "webui disabled"
  exit 0
fi

WEB_UI_IMAGE="${WEB_UI_IMAGE:-cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat}"
WEB_UI_CONTAINER_NAME="${WEB_UI_CONTAINER_NAME:-cube-webui}"
WEB_UI_HOST_PORT="${WEB_UI_HOST_PORT:-12088}"
WEB_UI_UPSTREAM="${WEB_UI_UPSTREAM:-http://host.docker.internal:3000}"

WEB_UI_DIST_DIR="${WEBUI_DIR}/dist"
NGINX_TEMPLATE="${WEBUI_DIR}/nginx.conf"
NGINX_CONF="${WEBUI_DIR}/nginx.generated.conf"
COMPOSE_TEMPLATE="${WEBUI_DIR}/docker-compose.yaml.template"
COMPOSE_FILE="${WEBUI_DIR}/docker-compose.yaml"

ensure_dir "${WEBUI_DIR}"
ensure_dir "${WEB_UI_DIST_DIR}"
for required_file in \
  "${WEB_UI_DIST_DIR}/index.html" \
  "${NGINX_TEMPLATE}" \
  "${COMPOSE_TEMPLATE}"
do
  ensure_file "${required_file}"
done

escape_sed() {
  printf '%s' "$1" | sed 's/[\/&]/\\&/g'
}

wait_for_tcp_port() {
  local port="$1"
  local retries="${2:-30}"
  local delay="${3:-2}"
  local i

  for ((i = 1; i <= retries; i++)); do
    if ss -lnt "( sport = :${port} )" | rg -q ":${port}"; then
      return 0
    fi
    sleep "${delay}"
  done

  return 1
}

WEB_UI_HOST_PORT_ESCAPED="$(escape_sed "${WEB_UI_HOST_PORT}")"
WEB_UI_UPSTREAM_ESCAPED="$(escape_sed "${WEB_UI_UPSTREAM}")"
WEB_UI_IMAGE_ESCAPED="$(escape_sed "${WEB_UI_IMAGE}")"
WEB_UI_CONTAINER_NAME_ESCAPED="$(escape_sed "${WEB_UI_CONTAINER_NAME}")"
WEB_UI_DIST_DIR_ESCAPED="$(escape_sed "${WEB_UI_DIST_DIR}")"
NGINX_CONF_ESCAPED="$(escape_sed "${NGINX_CONF}")"

sed \
  -e "s#__WEB_UI_HOST_PORT__#${WEB_UI_HOST_PORT_ESCAPED}#g" \
  -e "s#__WEB_UI_UPSTREAM__#${WEB_UI_UPSTREAM_ESCAPED}#g" \
  "${NGINX_TEMPLATE}" > "${NGINX_CONF}"

sed \
  -e "s#__WEB_UI_IMAGE__#${WEB_UI_IMAGE_ESCAPED}#g" \
  -e "s#__WEB_UI_CONTAINER_NAME__#${WEB_UI_CONTAINER_NAME_ESCAPED}#g" \
  -e "s#__WEB_UI_DIST_DIR__#${WEB_UI_DIST_DIR_ESCAPED}#g" \
  -e "s#__WEB_UI_NGINX_CONF__#${NGINX_CONF_ESCAPED}#g" \
  -e "s#__WEB_UI_HOST_PORT__#${WEB_UI_HOST_PORT_ESCAPED}#g" \
  "${COMPOSE_TEMPLATE}" > "${COMPOSE_FILE}"

webui_compose_run down --remove-orphans >/dev/null 2>&1 || true
webui_compose_run up -d webui

wait_for_tcp_port "${WEB_UI_HOST_PORT}" 30 2 \
  || die "webui port ${WEB_UI_HOST_PORT} did not become ready"
log "webui listening on ${WEB_UI_HOST_PORT}"

wait_for_http "http://127.0.0.1:${WEB_UI_HOST_PORT}/" 30 1 \
  || die "webui index did not become ready"
wait_for_http "http://127.0.0.1:${WEB_UI_HOST_PORT}/cubeapi/v1/health" 30 1 \
  || die "webui could not reach cube-api through /cubeapi"
