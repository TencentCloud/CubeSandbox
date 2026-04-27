#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

WEBUI_DIR="${TOOLBOX_ROOT}/webui"
WEBUI_COMPOSE_FILE="${WEBUI_DIR}/docker-compose.yaml"
WEBUI_COMPOSE_DOCKER_IMAGE="${WEB_UI_COMPOSE_DOCKER_IMAGE:-docker/compose:1.29.2}"

webui_compose_run() {
  ensure_dir "${WEBUI_DIR}"
  ensure_file "${WEBUI_COMPOSE_FILE}"

  if docker compose version >/dev/null 2>&1; then
    docker compose -f "${WEBUI_COMPOSE_FILE}" "$@"
    return 0
  fi

  if command -v docker-compose >/dev/null 2>&1; then
    docker-compose -f "${WEBUI_COMPOSE_FILE}" "$@"
    return 0
  fi

  docker run --rm \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "${TOOLBOX_ROOT}:${TOOLBOX_ROOT}" \
    -w "${WEBUI_DIR}" \
    "${WEBUI_COMPOSE_DOCKER_IMAGE}" \
    -f "${WEBUI_COMPOSE_FILE}" "$@"
}
