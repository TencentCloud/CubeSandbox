#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

"${SCRIPT_DIR}/down-cube-egress.sh"

stop_by_pidfile "cubelet" "^${TOOLBOX_ROOT}/Cubelet/bin/cubelet --config"
stop_by_pidfile "cube-api" "^${TOOLBOX_ROOT}/CubeAPI/bin/cube-api"
stop_by_pidfile "cubemaster"
stop_by_pidfile "network-agent"

log "local services stopped"
