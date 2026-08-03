#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"
# External MySQL: the local container is never started (see up-support.sh), so
# there is nothing local to health-check. Skip rather than block on a missing
# container and then trigger Restart=on-failure.
if mysql_is_external; then
  exit 0
fi
# CUBE_SANDBOX_DEP_HEALTH_RETRIES/DELAY tune the health-wait bounds (default
# 40 retries x 2s). They are internal knobs (mainly for tests to fail fast); the
# defaults are what a normal install uses.
wait_for_container_health "${CUBE_SANDBOX_MYSQL_CONTAINER:-cube-sandbox-mysql}" \
  "${CUBE_SANDBOX_DEP_HEALTH_RETRIES:-40}" "${CUBE_SANDBOX_DEP_HEALTH_DELAY:-2}" \
  || die "mysql container not ready"
