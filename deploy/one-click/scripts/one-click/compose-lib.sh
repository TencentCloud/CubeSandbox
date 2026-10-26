#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

PROXY_DIR="${TOOLBOX_ROOT}/cubeproxy"
COMPOSE_FILE="${PROXY_DIR}/docker-compose.yaml"
COMPOSE_DOCKER_IMAGE="${CUBE_PROXY_COMPOSE_DOCKER_IMAGE:-docker/compose:1.29.2}"

compose_run() {
  ensure_dir "${PROXY_DIR}"
  ensure_bind_mount_file "${COMPOSE_FILE}"

  if docker compose version >/dev/null 2>&1; then
    docker compose -f "${COMPOSE_FILE}" "$@"
    return 0
  fi

  if command -v docker-compose >/dev/null 2>&1; then
    docker-compose -f "${COMPOSE_FILE}" "$@"
    return 0
  fi

  docker run --rm \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "${TOOLBOX_ROOT}:${TOOLBOX_ROOT}" \
    -w "${PROXY_DIR}" \
    "${COMPOSE_DOCKER_IMAGE}" \
    -f "${COMPOSE_FILE}" "$@"
}
