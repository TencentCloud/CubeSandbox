#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"
# External database: the local container is never started (see up-support.sh),
# so there is nothing local to health-check. Skip rather than block ~80s on a
# missing container and then trigger Restart=on-failure. install.sh persists
# the canonical CUBE_EXTERNAL_DB_HOST (the legacy CUBE_EXTERNAL_MYSQL_HOST is
# mapped onto it), so honour both to match up-support.sh / quickcheck.sh.
if [[ -n "${CUBE_EXTERNAL_DB_HOST:-${CUBE_EXTERNAL_MYSQL_HOST:-}}" ]]; then
  exit 0
fi
wait_for_container_health "${CUBE_SANDBOX_MYSQL_CONTAINER:-cube-sandbox-mysql}" 40 2 || die "mysql container not ready"
