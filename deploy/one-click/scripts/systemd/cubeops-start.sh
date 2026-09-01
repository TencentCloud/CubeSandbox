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

# Warehouse unpack scratch. Distro /tmp is often tmpfs — keep it on disk.
CUBE_OPS_WAREHOUSE_WORK_DIR="${CUBE_OPS_WAREHOUSE_WORK_DIR:-/data/cubeops/tmp}"
mkdir -p "${CUBE_OPS_WAREHOUSE_WORK_DIR}"
export CUBE_OPS_WAREHOUSE_WORK_DIR
export TMPDIR="${TMPDIR:-${CUBE_OPS_WAREHOUSE_WORK_DIR}}"

# Derive CubeOps S3 connection from CUBE_S3_* when unset. Bucket stays
# cube-ops — never copy CUBE_S3_BUCKET (that is cube-volumes).
if [[ -z "${CUBE_OPS_S3_ENDPOINT:-}" && -n "${CUBE_S3_ENDPOINT:-}" ]]; then
  export CUBE_OPS_S3_ENDPOINT="${CUBE_S3_ENDPOINT}"
fi
if [[ -z "${CUBE_OPS_S3_NODE_ENDPOINT:-}" && -n "${CUBE_OPS_S3_ENDPOINT:-}" ]]; then
  export CUBE_OPS_S3_NODE_ENDPOINT="${CUBE_OPS_S3_ENDPOINT}"
fi
if [[ -z "${CUBE_OPS_S3_ACCESS_KEY_ID:-}" && -n "${CUBE_S3_ACCESS_KEY_ID:-}" ]]; then
  export CUBE_OPS_S3_ACCESS_KEY_ID="${CUBE_S3_ACCESS_KEY_ID}"
fi
if [[ -z "${CUBE_OPS_S3_SECRET_ACCESS_KEY:-}" && -n "${CUBE_S3_SECRET_ACCESS_KEY:-}" ]]; then
  export CUBE_OPS_S3_SECRET_ACCESS_KEY="${CUBE_S3_SECRET_ACCESS_KEY}"
fi
export CUBE_OPS_S3_BUCKET="${CUBE_OPS_S3_BUCKET:-cube-ops}"
if [[ -z "${CUBE_OPS_S3_REGION:-}" && -n "${CUBE_S3_REGION:-}" ]]; then
  export CUBE_OPS_S3_REGION="${CUBE_S3_REGION}"
fi
if [[ -z "${CUBE_OPS_S3_PATH_STYLE:-}" && -n "${CUBE_S3_S3FS_EXTRA_OPTS:-}" ]]; then
  if [[ "${CUBE_S3_S3FS_EXTRA_OPTS}" == *use_path_request_style* ]]; then
    export CUBE_OPS_S3_PATH_STYLE=true
  fi
fi
# Optional warehouse knobs from .one-click.env (defaults live in CubeOps):
#   CUBE_OPS_WAREHOUSE_UPLOAD_TIMEOUT
#   CUBE_OPS_WAREHOUSE_FETCH_TIMEOUT
#   CUBE_OPS_WAREHOUSE_GITHUB_REPOS / CUBE_OPS_WAREHOUSE_CNB_REPOS
#   CUBE_OPS_WAREHOUSE_GITHUB_TOKEN / CUBE_OPS_WAREHOUSE_CNB_TOKEN

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
