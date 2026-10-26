#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

SUPPORT_DIR="${TOOLBOX_ROOT}/support"
SUPPORT_COMPOSE_FILE="${SUPPORT_DIR}/docker-compose.yaml"
SUPPORT_COMPOSE_DOCKER_IMAGE="${SUPPORT_COMPOSE_DOCKER_IMAGE:-docker/compose:1.29.2}"

support_compose_run() {
  ensure_dir "${SUPPORT_DIR}"
  ensure_bind_mount_file "${SUPPORT_COMPOSE_FILE}"

  if docker compose version >/dev/null 2>&1; then
    docker compose -f "${SUPPORT_COMPOSE_FILE}" "$@"
    return 0
  fi

  if command -v docker-compose >/dev/null 2>&1; then
    docker-compose -f "${SUPPORT_COMPOSE_FILE}" "$@"
    return 0
  fi

  docker run --rm \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "${TOOLBOX_ROOT}:${TOOLBOX_ROOT}" \
    -w "${SUPPORT_DIR}" \
    "${SUPPORT_COMPOSE_DOCKER_IMAGE}" \
    -f "${SUPPORT_COMPOSE_FILE}" "$@"
}
