#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"
wait_for_container_health "${CUBE_SANDBOX_REDIS_CONTAINER:-cube-sandbox-redis}" 40 2 || die "redis container not ready"
