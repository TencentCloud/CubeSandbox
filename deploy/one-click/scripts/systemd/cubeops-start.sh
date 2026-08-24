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

# Redis (optional, for JWT blacklist / logout invalidation).
# When REDIS_URL is unset but Redis container is running, build it from
# the one-click Redis variables.
if [[ -z "${REDIS_URL:-}" && -n "${CUBE_SANDBOX_REDIS_HOST:-}" ]]; then
  redis_pass="${CUBE_SANDBOX_REDIS_PASSWORD:-}"
  redis_auth=""
  if [[ -n "${redis_pass}" ]]; then
    redis_auth=":${redis_pass}@"
  fi
  export REDIS_URL="redis://${redis_auth}${CUBE_SANDBOX_REDIS_HOST}:${CUBE_SANDBOX_REDIS_PORT:-6379}"
fi

# Webhook delivery worker (issue #642). Disabled by default so existing
# deployments are unaffected; enable only when the lifecycle Redis stream is
# reachable and webhook subscriptions have been registered.
export CUBE_OPS_WEBHOOK_ENABLED="${CUBE_OPS_WEBHOOK_ENABLED:-false}"
# Pass-through knobs (all optional; defaults live in CubeOps config).
export CUBE_OPS_WEBHOOK_CONSUMER_GROUP="${CUBE_OPS_WEBHOOK_CONSUMER_GROUP:-cube-webhook}"
export CUBE_OPS_WEBHOOK_HTTP_TIMEOUT="${CUBE_OPS_WEBHOOK_HTTP_TIMEOUT:-10s}"
export CUBE_OPS_WEBHOOK_SHUTDOWN_TIMEOUT="${CUBE_OPS_WEBHOOK_SHUTDOWN_TIMEOUT:-30s}"
export CUBE_OPS_WEBHOOK_WORKER_CONCURRENCY="${CUBE_OPS_WEBHOOK_WORKER_CONCURRENCY:-8}"
export CUBE_OPS_WEBHOOK_PER_SUBSCRIPTION_CONCURRENCY="${CUBE_OPS_WEBHOOK_PER_SUBSCRIPTION_CONCURRENCY:-2}"
export CUBE_OPS_WEBHOOK_KEEP_PENDING_MAX_RETRY_WINDOW="${CUBE_OPS_WEBHOOK_KEEP_PENDING_MAX_RETRY_WINDOW:-168h}"
export CUBE_OPS_WEBHOOK_ALLOW_PRIVATE_NETWORKS="${CUBE_OPS_WEBHOOK_ALLOW_PRIVATE_NETWORKS:-false}"

exec "${CUBE_OPS_BIN}"
