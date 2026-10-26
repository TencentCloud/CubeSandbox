#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"
wait_for_http "http://${CUBEMASTER_ADDR:-127.0.0.1:8089}/notify/health" 30 1 || die "cubemaster health not ready"
