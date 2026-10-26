#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"
wait_for_http "http://${CUBE_API_HEALTH_ADDR:-127.0.0.1:3000}/health" 30 1 || die "cube-api health not ready"
