#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_cmd docker

CUBE_EXTERNAL_REDIS_HOST="${CUBE_EXTERNAL_REDIS_HOST:-}"
CUBE_EXTERNAL_REDIS_PORT="${CUBE_EXTERNAL_REDIS_PORT:-6379}"
CUBE_EXTERNAL_REDIS_PASSWORD="${CUBE_EXTERNAL_REDIS_PASSWORD:-ceuhvu123}"

# Point cube-proxy at the external Redis (up-cube-proxy.sh reads these).
if [[ -n "${CUBE_EXTERNAL_REDIS_HOST}" ]]; then
  export CUBE_PROXY_REDIS_IP="${CUBE_EXTERNAL_REDIS_HOST}"
  export CUBE_PROXY_REDIS_PORT="${CUBE_EXTERNAL_REDIS_PORT}"
  export CUBE_PROXY_REDIS_PASSWORD="${CUBE_EXTERNAL_REDIS_PASSWORD}"
fi

"${SCRIPT_DIR}/up-support.sh"

"${SCRIPT_DIR}/up-cube-proxy.sh"
"${SCRIPT_DIR}/up-dns.sh"

"${SCRIPT_DIR}/up.sh"

"${SCRIPT_DIR}/up-webui.sh"
