#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

CUBE_LCM_DIR="${TOOLBOX_ROOT}/cube-lifecycle-manager"
CUBE_LCM_COMPOSE_FILE="${CUBE_LCM_DIR}/docker-compose.yaml"
CUBE_LCM_COMPOSE_DOCKER_IMAGE="${CUBE_LCM_COMPOSE_DOCKER_IMAGE:-docker/compose:1.29.2}"

cube_lcm_compose_run() {
  ensure_dir "${CUBE_LCM_DIR}"
  ensure_bind_mount_file "${CUBE_LCM_COMPOSE_FILE}"

  if docker compose version >/dev/null 2>&1; then
    docker compose -f "${CUBE_LCM_COMPOSE_FILE}" "$@"
    return 0
  fi

  if command -v docker-compose >/dev/null 2>&1; then
    docker-compose -f "${CUBE_LCM_COMPOSE_FILE}" "$@"
    return 0
  fi

  docker run --rm \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "${TOOLBOX_ROOT}:${TOOLBOX_ROOT}" \
    -w "${CUBE_LCM_DIR}" \
    "${CUBE_LCM_COMPOSE_DOCKER_IMAGE}" \
    -f "${CUBE_LCM_COMPOSE_FILE}" "$@"
}
