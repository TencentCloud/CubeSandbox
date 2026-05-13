#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd docker

REDIS_IMAGE="${CUBE_SANDBOX_REDIS_IMAGE:-cube-sandbox-image.tencentcloudcr.com/opensource/redis:7-alpine}"
REDIS_CONTAINER="${CUBE_SANDBOX_REDIS_CONTAINER:-cube-sandbox-redis}"
REDIS_VOLUME="${CUBE_SANDBOX_REDIS_VOLUME:-cube-sandbox-redis-data}"
REDIS_PORT="${CUBE_SANDBOX_REDIS_PORT:-6379}"
REDIS_PASSWORD="${CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123}"

docker_rm_if_exists "${REDIS_CONTAINER}"

docker create \
  --name "${REDIS_CONTAINER}" \
  -e REDIS_PASSWORD="${REDIS_PASSWORD}" \
  -p "${REDIS_PORT}:6379" \
  -v "${REDIS_VOLUME}:/data" \
  --health-cmd "sh -ec 'redis-cli -a \"$REDIS_PASSWORD\" ping | grep -x PONG'" \
  --health-interval 3s \
  --health-timeout 3s \
  --health-retries 20 \
  "${REDIS_IMAGE}" \
  redis-server \
  --requirepass \
  "${REDIS_PASSWORD}" >/dev/null

exec docker start -a "${REDIS_CONTAINER}"
