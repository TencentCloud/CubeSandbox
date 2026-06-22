#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"
# External Redis: the local container is never started (see up-support.sh), so
# there is nothing local to health-check. Skip rather than block on a missing
# container and then trigger Restart=on-failure.
if [[ -n "${CUBE_EXTERNAL_REDIS_HOST:-}" ]]; then
  exit 0
fi
wait_for_container_health "${CUBE_SANDBOX_REDIS_CONTAINER:-cube-sandbox-redis}" 40 2 || die "redis container not ready"
