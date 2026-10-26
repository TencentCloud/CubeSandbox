#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"
# shellcheck source=./support-compose-lib.sh
source "${SCRIPT_DIR}/support-compose-lib.sh"

require_root
require_cmd docker

REMOVE_VOLUMES="${CUBE_SANDBOX_REMOVE_VOLUMES:-0}"
SUPPORT_DIR="${TOOLBOX_ROOT}/support"
SUPPORT_SERVICES="${ONE_CLICK_SUPPORT_SERVICES:-}"

if [[ -f "${SUPPORT_DIR}/docker-compose.yaml" ]]; then
  if [[ -n "${SUPPORT_SERVICES}" ]]; then
    support_compose_run stop ${SUPPORT_SERVICES} >/dev/null 2>&1 || true
    if [[ "${REMOVE_VOLUMES}" == "1" ]]; then
      support_compose_run rm -f -v ${SUPPORT_SERVICES} >/dev/null 2>&1 || true
    else
      support_compose_run rm -f ${SUPPORT_SERVICES} >/dev/null 2>&1 || true
    fi
  else
    if [[ "${REMOVE_VOLUMES}" == "1" ]]; then
      support_compose_run down --remove-orphans -v >/dev/null 2>&1 || true
    else
      support_compose_run down --remove-orphans >/dev/null 2>&1 || true
    fi
  fi
fi

log "support services stopped"
