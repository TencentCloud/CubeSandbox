#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"
wait_for_tcp_port "${CUBE_PROXY_HOST_PORT:-443}" 30 2 || die "cube-proxy tcp port not ready"
