#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd docker

MYSQL_IMAGE="${CUBE_SANDBOX_MYSQL_IMAGE:-cube-sandbox-image.tencentcloudcr.com/opensource/mysql:8.0}"
MYSQL_CONTAINER="${CUBE_SANDBOX_MYSQL_CONTAINER:-cube-sandbox-mysql}"
MYSQL_VOLUME="${CUBE_SANDBOX_MYSQL_VOLUME:-cube-sandbox-mysql-data}"
MYSQL_PORT="${CUBE_SANDBOX_MYSQL_PORT:-3306}"
MYSQL_DB="${CUBE_SANDBOX_MYSQL_DB:-cube_mvp}"
MYSQL_USER="${CUBE_SANDBOX_MYSQL_USER:-cube}"
MYSQL_PASSWORD="${CUBE_SANDBOX_MYSQL_PASSWORD:-cube_pass}"
MYSQL_ROOT_PASSWORD="${CUBE_SANDBOX_MYSQL_ROOT_PASSWORD:-cube_root}"
SQL_DIR="${TOOLBOX_ROOT}/sql"

ensure_dir "${SQL_DIR}"
docker_rm_if_exists "${MYSQL_CONTAINER}"

docker create \
  --name "${MYSQL_CONTAINER}" \
  -e MYSQL_ROOT_PASSWORD="${MYSQL_ROOT_PASSWORD}" \
  -e MYSQL_DATABASE="${MYSQL_DB}" \
  -e MYSQL_USER="${MYSQL_USER}" \
  -e MYSQL_PASSWORD="${MYSQL_PASSWORD}" \
  -p "${MYSQL_PORT}:3306" \
  -v "${MYSQL_VOLUME}:/var/lib/mysql" \
  -v "${SQL_DIR}:/docker-entrypoint-initdb.d:ro" \
  --health-cmd "mysqladmin ping -h 127.0.0.1 -u${MYSQL_USER} -p${MYSQL_PASSWORD} --silent" \
  --health-interval 3s \
  --health-timeout 3s \
  --health-retries 20 \
  "${MYSQL_IMAGE}" \
  --default-authentication-plugin=mysql_native_password \
  --skip-name-resolve >/dev/null

exec docker start -a "${MYSQL_CONTAINER}"
