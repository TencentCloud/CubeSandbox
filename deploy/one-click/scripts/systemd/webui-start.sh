#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd docker

WEB_UI_ENABLE="${WEB_UI_ENABLE:-1}"
if [[ "${WEB_UI_ENABLE}" != "1" ]]; then
  log "webui disabled"
  exit 0
fi

WEBUI_DIR="${TOOLBOX_ROOT}/webui"
WEB_UI_IMAGE="${WEB_UI_IMAGE:-cube-sandbox-image.tencentcloudcr.com/opensource/openresty:1.21.4.1-6-alpine-fat}"
WEB_UI_CONTAINER_NAME="${WEB_UI_CONTAINER_NAME:-cube-webui}"
WEB_UI_HOST_PORT="${WEB_UI_HOST_PORT:-12088}"
WEB_UI_UPSTREAM="${WEB_UI_UPSTREAM:-http://host.docker.internal:3000}"
WEB_UI_DIST_DIR="${WEBUI_DIR}/dist"
NGINX_TEMPLATE="${WEBUI_DIR}/nginx.conf"
NGINX_CONF="${WEBUI_DIR}/nginx.generated.conf"

ensure_dir "${WEBUI_DIR}"
ensure_dir "${WEB_UI_DIST_DIR}"
ensure_file "${WEB_UI_DIST_DIR}/index.html"
ensure_file "${NGINX_TEMPLATE}"

render_template \
  "${NGINX_TEMPLATE}" \
  "${NGINX_CONF}" \
  -e "s#__WEB_UI_UPSTREAM__#$(escape_sed "${WEB_UI_UPSTREAM}")#g"

docker_rm_if_exists "${WEB_UI_CONTAINER_NAME}"
docker create \
  --name "${WEB_UI_CONTAINER_NAME}" \
  --add-host host.docker.internal:host-gateway \
  -p "${WEB_UI_HOST_PORT}:80" \
  -v "${WEB_UI_DIST_DIR}:/usr/share/nginx/html:ro" \
  -v "${NGINX_CONF}:/usr/local/openresty/nginx/conf/nginx.conf:ro" \
  --health-cmd "wget -q -O /dev/null http://127.0.0.1/" \
  --health-interval 10s \
  --health-timeout 3s \
  --health-retries 6 \
  "${WEB_UI_IMAGE}" >/dev/null

exec docker start -a "${WEB_UI_CONTAINER_NAME}"
