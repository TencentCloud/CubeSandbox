#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_cmd docker
require_cmd rg

CUBE_SANDBOX_MYSQL_CONTAINER="${CUBE_SANDBOX_MYSQL_CONTAINER:-cube-sandbox-mysql}"
MYSQL_DB="${CUBE_SANDBOX_MYSQL_DB:-cube_mvp}"
MYSQL_ROOT_PASSWORD="${CUBE_SANDBOX_MYSQL_ROOT_PASSWORD:-cube_root}"
CUBE_SANDBOX_NODE_IP="${CUBE_SANDBOX_NODE_IP:-}"
SQL_DIR="${TOOLBOX_ROOT}/sql"
METRIC_LOOP="${CUBEMASTER_METRIC_LOOP:-0}"
METRIC_PID_FILE="${RUNTIME_DIR}/seed-cubemaster-metrics.pid"

# CubeMaster owns its own schema via the embedded goose migrations; we
# only seed deployment-specific rows here (the single-node host_info /
# sub_host_info rows that turn a fresh database into a usable single-box
# install). The seed therefore MUST run AFTER CubeMaster has finished
# startup migrations, not before.
CUBEMASTER_HEALTH_ADDR="${CUBEMASTER_HEALTH_ADDR:-127.0.0.1:8089}"
CUBEMASTER_READY_TIMEOUT="${CUBEMASTER_READY_TIMEOUT:-120}"

test -d "${SQL_DIR}" || die "sql dir missing: ${SQL_DIR}"
[[ -n "${CUBE_SANDBOX_NODE_IP}" ]] || die "CUBE_SANDBOX_NODE_IP is required; set it to the current node private IP in .one-click.env"

"${SCRIPT_DIR}/up-support.sh"

"${SCRIPT_DIR}/up-cube-proxy.sh"
"${SCRIPT_DIR}/up-dns.sh"

"${SCRIPT_DIR}/seed-cubemaster-metrics.sh"
if [[ "${METRIC_LOOP}" == "1" ]]; then
  if [[ -f "${METRIC_PID_FILE}" ]]; then
    old_pid="$(<"${METRIC_PID_FILE}")"
    if [[ -n "${old_pid}" ]] && kill -0 "${old_pid}" >/dev/null 2>&1; then
      kill "${old_pid}" >/dev/null 2>&1 || true
    fi
  fi
  "${SCRIPT_DIR}/seed-cubemaster-metrics.sh" --loop >"${LOG_DIR}/seed-cubemaster-metrics.log" 2>&1 &
  echo "$!" > "${METRIC_PID_FILE}"
fi

"${SCRIPT_DIR}/up.sh"

# Wait for CubeMaster to be healthy (which implies dao.Migrate completed
# and the host_info / sub_host_info tables exist) before seeding the
# single-node rows. The health endpoint flips green only after every
# business package Init has returned, which transitively guarantees the
# migration step finished.
wait_for_http "http://${CUBEMASTER_HEALTH_ADDR}/notify/health" "${CUBEMASTER_READY_TIMEOUT}" 1 \
  || die "cubemaster did not become ready before seeding, check logs under ${LOG_DIR}"

sed "s/__CUBE_SANDBOX_NODE_IP__/${CUBE_SANDBOX_NODE_IP//\//\\/}/g" "${SQL_DIR}/002_seed_single_node.sql" \
  | docker exec -i "${CUBE_SANDBOX_MYSQL_CONTAINER}" mysql -uroot "-p${MYSQL_ROOT_PASSWORD}" "${MYSQL_DB}"

"${SCRIPT_DIR}/up-webui.sh"
