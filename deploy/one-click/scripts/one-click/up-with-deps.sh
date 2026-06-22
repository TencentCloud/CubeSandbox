#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_cmd docker

CUBE_SANDBOX_MYSQL_CONTAINER="${CUBE_SANDBOX_MYSQL_CONTAINER:-cube-sandbox-mysql}"
MYSQL_DB="${CUBE_SANDBOX_MYSQL_DB:-cube_mvp}"
MYSQL_ROOT_PASSWORD="${CUBE_SANDBOX_MYSQL_ROOT_PASSWORD:-cube_root}"
CUBE_SANDBOX_NODE_IP="${CUBE_SANDBOX_NODE_IP:-}"
SQL_DIR="${TOOLBOX_ROOT}/sql"

# External MySQL/Redis support: when configured, the local containers are not
# started, so the single-node seed must go to the external server and cube-proxy
# must be pointed at the external Redis.
CUBE_EXTERNAL_MYSQL_HOST="${CUBE_EXTERNAL_MYSQL_HOST:-}"
CUBE_EXTERNAL_MYSQL_PORT="${CUBE_EXTERNAL_MYSQL_PORT:-3306}"
CUBE_EXTERNAL_MYSQL_USER="${CUBE_EXTERNAL_MYSQL_USER:-cube}"
CUBE_EXTERNAL_MYSQL_PASSWORD="${CUBE_EXTERNAL_MYSQL_PASSWORD:-cube_pass}"
CUBE_EXTERNAL_MYSQL_DB="${CUBE_EXTERNAL_MYSQL_DB:-${MYSQL_DB}}"
CUBE_EXTERNAL_REDIS_HOST="${CUBE_EXTERNAL_REDIS_HOST:-}"
CUBE_EXTERNAL_REDIS_PORT="${CUBE_EXTERNAL_REDIS_PORT:-6379}"
CUBE_EXTERNAL_REDIS_PASSWORD="${CUBE_EXTERNAL_REDIS_PASSWORD:-ceuhvu123}"

# Point cube-proxy at the external Redis (up-cube-proxy.sh reads these).
if [[ -n "${CUBE_EXTERNAL_REDIS_HOST}" ]]; then
  export CUBE_PROXY_REDIS_IP="${CUBE_EXTERNAL_REDIS_HOST}"
  export CUBE_PROXY_REDIS_PORT="${CUBE_EXTERNAL_REDIS_PORT}"
  export CUBE_PROXY_REDIS_PASSWORD="${CUBE_EXTERNAL_REDIS_PASSWORD}"
fi

# CubeMaster owns its own schema via the embedded goose migrations; we
# only seed deployment-specific rows here (the single-node host_info /
# sub_host_info rows that turn a fresh database into a usable single-box
# install). The seed therefore MUST run AFTER CubeMaster has finished
# startup migrations, not before.
CUBEMASTER_HEALTH_ADDR="${CUBEMASTER_HEALTH_ADDR:-127.0.0.1:8089}"
CUBEMASTER_READY_TIMEOUT="${CUBEMASTER_READY_TIMEOUT:-120}"

test -d "${SQL_DIR}" || die "sql dir missing: ${SQL_DIR}"
[[ -n "${CUBE_SANDBOX_NODE_IP}" ]] || die "CUBE_SANDBOX_NODE_IP is required; set it to the current node private IP in .one-click.env"
CUBE_SANDBOX_NODE_IP_SED="$(escape_sed "${CUBE_SANDBOX_NODE_IP}")"

"${SCRIPT_DIR}/up-support.sh"

"${SCRIPT_DIR}/up-cube-proxy.sh"
"${SCRIPT_DIR}/up-dns.sh"

"${SCRIPT_DIR}/up.sh"

# Wait for CubeMaster to be healthy (which implies dao.Migrate completed
# and the host_info / sub_host_info tables exist) before seeding the
# single-node rows. The health endpoint flips green only after every
# business package Init has returned, which transitively guarantees the
# migration step finished.
wait_for_http "http://${CUBEMASTER_HEALTH_ADDR}/notify/health" "${CUBEMASTER_READY_TIMEOUT}" 1 \
  || die "cubemaster did not become ready before seeding, check logs under ${LOG_DIR}"

if [[ -n "${CUBE_EXTERNAL_MYSQL_HOST}" ]]; then
  # External MySQL: the local container is absent, so seed via a host-side
  # mysql client connecting to the external server.
  require_cmd mysql
  log "seeding single-node rows into external MySQL ${CUBE_EXTERNAL_MYSQL_HOST}:${CUBE_EXTERNAL_MYSQL_PORT}/${CUBE_EXTERNAL_MYSQL_DB}"
  # SECURITY: pass the password via a temporary, 0600 my.cnf instead of
  # "-p<pwd>" on the command line. Process arguments are world-readable via
  # /proc/<pid>/cmdline, which would leak the MySQL password to any local user.
  # Tighten umask before mktemp so the file is created 0600 from the start --
  # this closes the brief race window between mktemp's default (umask-derived)
  # permissions and the chmod 600 below. Mirrors install.sh's pattern.
  OLD_UMASK="$(umask)"
  umask 077
  MYSQL_CNF="$(mktemp)"
  umask "${OLD_UMASK}"
  trap 'rm -f "${MYSQL_CNF}"' EXIT
  chmod 600 "${MYSQL_CNF}"
  cat > "${MYSQL_CNF}" <<EOF
[client]
password="${CUBE_EXTERNAL_MYSQL_PASSWORD}"
EOF
  # Fail fast if the external MySQL becomes slow/unreachable between the
  # install.sh preflight and this seed step, instead of hanging indefinitely.
  # Matches the preflight's connect timeout (ONE_CLICK_EXTERNAL_DEP_TIMEOUT).
  sed "s/__CUBE_SANDBOX_NODE_IP__/${CUBE_SANDBOX_NODE_IP_SED}/g" "${SQL_DIR}/002_seed_single_node.sql" \
    | mysql \
        --defaults-extra-file="${MYSQL_CNF}" \
        -h "${CUBE_EXTERNAL_MYSQL_HOST}" \
        -P "${CUBE_EXTERNAL_MYSQL_PORT}" \
        -u "${CUBE_EXTERNAL_MYSQL_USER}" \
        --connect-timeout="${ONE_CLICK_EXTERNAL_DEP_TIMEOUT:-5}" \
        "${CUBE_EXTERNAL_MYSQL_DB}"
  rm -f "${MYSQL_CNF}"
  trap - EXIT
else
  sed "s/__CUBE_SANDBOX_NODE_IP__/${CUBE_SANDBOX_NODE_IP_SED}/g" "${SQL_DIR}/002_seed_single_node.sql" \
    | docker exec -i "${CUBE_SANDBOX_MYSQL_CONTAINER}" mysql -uroot "-p${MYSQL_ROOT_PASSWORD}" "${MYSQL_DB}"
fi

"${SCRIPT_DIR}/up-webui.sh"
