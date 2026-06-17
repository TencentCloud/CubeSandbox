#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"
# External MySQL: the local container is never started (see up-support.sh), so
# there is nothing local to health-check. Skip rather than block ~80s on a
# missing container and then trigger Restart=on-failure.
if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST:-}" ]]; then
  exit 0
fi
wait_for_container_health "${CUBE_SANDBOX_MYSQL_CONTAINER:-cube-sandbox-mysql}" 40 2 || die "mysql container not ready"
