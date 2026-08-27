#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
ensure_systemd_runtime_dirs

CUBE_OPS_BIN="${TOOLBOX_ROOT}/CubeOps/bin/cubeops"
CUBE_OPS_LOG_DIR="${CUBE_OPS_LOG_DIR:-/data/log/CubeOps}"

ensure_executable "${CUBE_OPS_BIN}"
mkdir -p "${CUBE_OPS_LOG_DIR}"

# Bind address — must be 0.0.0.0 in All-in-One mode so the WebUI nginx
# container can reach CubeOps via host.docker.internal:3010.
export CUBE_OPS_BIND="${CUBE_OPS_BIND:-0.0.0.0:3010}"
export CUBE_OPS_LOG_LEVEL="${CUBE_OPS_LOG_LEVEL:-info}"

# CubeMaster address (same host in All-in-One mode).
export CUBE_MASTER_ADDR="${CUBE_MASTER_ADDR:-http://127.0.0.1:8089}"

# JWT configuration. JWT_SECRET left unset → CubeOps auto-generates and
# persists it to t_system_setting on first boot (single-instance default).
export JWT_ACCESS_TTL="${JWT_ACCESS_TTL:-15m}"
export JWT_REFRESH_TTL="${JWT_REFRESH_TTL:-168h}"

# Shared MySQL (same instance as CubeMaster, database cube_mvp).
if [[ -n "${DATABASE_URL:-}" ]]; then
  export DATABASE_URL
else
  mysql_host="${CUBE_SANDBOX_MYSQL_HOST:-127.0.0.1}"
  mysql_port="${CUBE_SANDBOX_MYSQL_PORT:-3306}"
  mysql_user="${CUBE_SANDBOX_MYSQL_USER:-cube}"
  mysql_password="${CUBE_SANDBOX_MYSQL_PASSWORD:-cube_pass}"
  mysql_db="${CUBE_SANDBOX_MYSQL_DB:-cube_mvp}"
  export DATABASE_URL="mysql://${mysql_user}:${mysql_password}@${mysql_host}:${mysql_port}/${mysql_db}"
fi

# Skip migration fingerprint check (dev environment compat).
if [[ -n "${CUBEMASTER_MIGRATION_SKIP_FINGERPRINT_CHECK:-}" ]]; then
  export CUBEMASTER_MIGRATION_SKIP_FINGERPRINT_CHECK
fi

# Redis (optional). CubeProxy/LCM use the same derivation (CUBE_EXTERNAL → CUBE_SANDBOX).
# CubeOps splits host/port/password (no REDIS_URL) and assembles the connection internally.
export REDIS_HOST="${CUBE_EXTERNAL_REDIS_HOST:-${CUBE_SANDBOX_REDIS_HOST:-127.0.0.1}}"
export REDIS_PORT="${CUBE_EXTERNAL_REDIS_PORT:-${CUBE_SANDBOX_REDIS_PORT:-6379}}"
export REDIS_PASSWORD="${CUBE_EXTERNAL_REDIS_PASSWORD:-${CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123}}"
export REDIS_MASTER_NAME="${CUBE_EXTERNAL_REDIS_MASTER_NAME:-}"
export REDIS_SENTINEL_NODES="${CUBE_EXTERNAL_REDIS_SENTINEL_NODES:-}"
export REDIS_SENTINEL_PASSWORD="${CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD:-}"

exec "${CUBE_OPS_BIN}"
