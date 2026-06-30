#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

SYSTEMD_SCRIPT_DIR="${TOOLBOX_ROOT}/scripts/systemd"

require_root

if command -v docker >/dev/null 2>&1 && [[ -x "${SYSTEMD_SCRIPT_DIR}/cube-egress-stop.sh" ]]; then
  "${SYSTEMD_SCRIPT_DIR}/cube-egress-stop.sh" || true
else
  log "cube-egress container stop skipped; docker or stop script is unavailable"
fi

stop_by_pidfile "cube-egress"

if [[ -x "${SYSTEMD_SCRIPT_DIR}/cube-egress-net-stop.sh" ]]; then
  "${SYSTEMD_SCRIPT_DIR}/cube-egress-net-stop.sh" || true
else
  log "cube-egress network stop skipped; script is unavailable"
fi

log "cube-egress stopped"
