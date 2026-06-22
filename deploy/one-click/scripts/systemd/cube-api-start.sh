#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
ensure_systemd_runtime_dirs

CUBE_API_BIN="${TOOLBOX_ROOT}/CubeAPI/bin/cube-api"
CUBE_API_LOG_DIR="${CUBE_API_LOG_DIR:-/data/log/CubeAPI}"

ensure_executable "${CUBE_API_BIN}"
mkdir -p "${CUBE_API_LOG_DIR}"

export LOG_DIR="${CUBE_API_LOG_DIR}"
export CUBE_API_BIND="${CUBE_API_BIND:-0.0.0.0:3000}"
export CUBE_API_SANDBOX_DOMAIN="${CUBE_API_SANDBOX_DOMAIN:-cube.app}"
if [[ -n "${CUBE_MASTER_ADDR:-}" ]]; then
  export CUBE_MASTER_ADDR
fi
if [[ -n "${AUTH_CALLBACK_URL:-}" ]]; then
  export AUTH_CALLBACK_URL
fi
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

exec "${CUBE_API_BIN}"
