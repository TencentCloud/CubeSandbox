#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

COREDNS_DIR="${TOOLBOX_ROOT}/coredns"
COREDNS_COMPOSE_FILE="${COREDNS_DIR}/docker-compose.yaml"
COREDNS_COMPOSE_DOCKER_IMAGE="${COREDNS_COMPOSE_DOCKER_IMAGE:-docker/compose:1.29.2}"

coredns_compose_run() {
  ensure_dir "${COREDNS_DIR}"
  ensure_bind_mount_file "${COREDNS_COMPOSE_FILE}"

  if docker compose version >/dev/null 2>&1; then
    docker compose -f "${COREDNS_COMPOSE_FILE}" "$@"
    return 0
  fi

  if command -v docker-compose >/dev/null 2>&1; then
    docker-compose -f "${COREDNS_COMPOSE_FILE}" "$@"
    return 0
  fi

  docker run --rm \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "${TOOLBOX_ROOT}:${TOOLBOX_ROOT}" \
    -w "${COREDNS_DIR}" \
    "${COREDNS_COMPOSE_DOCKER_IMAGE}" \
    -f "${COREDNS_COMPOSE_FILE}" "$@"
}
