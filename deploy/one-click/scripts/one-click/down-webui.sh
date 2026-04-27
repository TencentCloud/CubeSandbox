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

WEB_UI_CONTAINER_NAME="${WEB_UI_CONTAINER_NAME:-cube-webui}"

if [[ -f "${WEBUI_DIR}/docker-compose.yaml" ]]; then
  webui_compose_run down --remove-orphans >/dev/null 2>&1 || true
fi

if container_exists "${WEB_UI_CONTAINER_NAME}"; then
  docker rm -f "${WEB_UI_CONTAINER_NAME}" >/dev/null
fi

log "webui stopped"
