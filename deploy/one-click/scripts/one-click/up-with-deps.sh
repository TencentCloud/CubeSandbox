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
CUBE_EXTERNAL_REDIS_MASTER_NAME="${CUBE_EXTERNAL_REDIS_MASTER_NAME:-}"
CUBE_EXTERNAL_REDIS_SENTINEL_NODES="${CUBE_EXTERNAL_REDIS_SENTINEL_NODES:-}"
CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD="${CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD:-}"

# Point cube-proxy / cube-lifecycle-manager at the external Redis.
if [[ -n "${CUBE_EXTERNAL_REDIS_MASTER_NAME}" ]]; then
  export CUBE_PROXY_REDIS_MASTER_NAME="${CUBE_EXTERNAL_REDIS_MASTER_NAME}"
  export CUBE_PROXY_REDIS_SENTINEL_NODES="${CUBE_EXTERNAL_REDIS_SENTINEL_NODES}"
  export CUBE_PROXY_REDIS_PASSWORD="${CUBE_EXTERNAL_REDIS_PASSWORD}"
  export CUBE_PROXY_REDIS_SENTINEL_PASSWORD="${CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD}"
  export CUBE_LCM_REDIS_MASTER_NAME="${CUBE_EXTERNAL_REDIS_MASTER_NAME}"
  export CUBE_LCM_REDIS_SENTINEL_NODES="${CUBE_EXTERNAL_REDIS_SENTINEL_NODES}"
  export CUBE_LCM_REDIS_PASSWORD="${CUBE_EXTERNAL_REDIS_PASSWORD}"
  export CUBE_LCM_REDIS_SENTINEL_PASSWORD="${CUBE_EXTERNAL_REDIS_SENTINEL_PASSWORD}"
elif [[ -n "${CUBE_EXTERNAL_REDIS_HOST}" ]]; then
  export CUBE_PROXY_REDIS_IP="${CUBE_EXTERNAL_REDIS_HOST}"
  export CUBE_PROXY_REDIS_PORT="${CUBE_EXTERNAL_REDIS_PORT}"
  export CUBE_PROXY_REDIS_PASSWORD="${CUBE_EXTERNAL_REDIS_PASSWORD}"
fi

"${SCRIPT_DIR}/up-support.sh"

# cube-lifecycle-manager owns paused-sandbox resume; it must be reachable
# before cube-proxy starts routing paused traffic through /_sidecar_resume,
# otherwise the first paused request would 502.
"${SCRIPT_DIR}/up-cube-lifecycle-manager.sh"
"${SCRIPT_DIR}/up-cube-proxy.sh"
"${SCRIPT_DIR}/up-dns.sh"

"${SCRIPT_DIR}/up.sh"

"${SCRIPT_DIR}/up-webui.sh"
