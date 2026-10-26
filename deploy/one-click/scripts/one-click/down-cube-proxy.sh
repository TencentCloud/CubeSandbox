#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"
# shellcheck source=./compose-lib.sh
source "${SCRIPT_DIR}/compose-lib.sh"

require_root
require_cmd docker

PROXY_DIR="${TOOLBOX_ROOT}/cubeproxy"
CUBE_PROXY_CONTAINER_NAME="${CUBE_PROXY_CONTAINER_NAME:-cube-proxy}"

if [[ -f "${PROXY_DIR}/docker-compose.yaml" ]]; then
  # compose down does graceful SIGTERM + grace period internally; no -f needed.
  compose_run down --remove-orphans >/dev/null 2>&1 || true
fi

# Fallback for the rare case where the container was not created by compose
# (e.g. upgrade from a legacy bundle). Stop gracefully, then remove.
docker_rm_if_exists "${CUBE_PROXY_CONTAINER_NAME}"

log "cube proxy stopped"
