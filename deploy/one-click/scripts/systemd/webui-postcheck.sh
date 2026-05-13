#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"
port="${WEB_UI_HOST_PORT:-12088}"
wait_for_tcp_port "${port}" 30 2 || die "webui port not ready"
wait_for_http "http://127.0.0.1:${port}/" 30 1 || die "webui index not ready"
wait_for_http "http://127.0.0.1:${port}/cubeapi/v1/health" 30 1 || die "webui cube-api route not ready"
