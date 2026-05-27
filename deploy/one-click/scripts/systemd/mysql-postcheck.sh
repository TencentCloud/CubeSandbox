#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"
wait_for_container_health "${CUBE_SANDBOX_MYSQL_CONTAINER:-cube-sandbox-mysql}" 40 2 || die "mysql container not ready"
