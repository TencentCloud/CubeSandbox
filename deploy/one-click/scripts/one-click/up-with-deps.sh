#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_cmd docker

# Point cube-proxy / cube-lifecycle-manager at the external Redis (up-cube-proxy.sh
# and up-cube-lifecycle-manager.sh read these). common.sh has already folded any
# legacy CUBE_EXTERNAL_* onto the CUBE_SANDBOX_* names and, for Sentinel mode,
# forced redis_is_external. Not redundant with the persisted CUBE_PROXY_REDIS_* in
# .one-click.env: on an ad-hoc invocation without those persisted keys,
# up-cube-proxy.sh defaults CUBE_PROXY_REDIS_IP to 127.0.0.1 (NOT
# CUBE_SANDBOX_REDIS_HOST), so this export is what steers cube-proxy to the
# external Redis in that path.
if redis_is_external; then
  if [[ -n "${CUBE_SANDBOX_REDIS_MASTER_NAME:-}" ]]; then
    export CUBE_PROXY_REDIS_MASTER_NAME="${CUBE_SANDBOX_REDIS_MASTER_NAME}"
    export CUBE_PROXY_REDIS_SENTINEL_NODES="${CUBE_SANDBOX_REDIS_SENTINEL_NODES:-}"
    export CUBE_PROXY_REDIS_PASSWORD="${CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123}"
    export CUBE_PROXY_REDIS_SENTINEL_PASSWORD="${CUBE_SANDBOX_REDIS_SENTINEL_PASSWORD:-}"
    export CUBE_LCM_REDIS_MASTER_NAME="${CUBE_SANDBOX_REDIS_MASTER_NAME}"
    export CUBE_LCM_REDIS_SENTINEL_NODES="${CUBE_SANDBOX_REDIS_SENTINEL_NODES:-}"
    export CUBE_LCM_REDIS_PASSWORD="${CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123}"
    export CUBE_LCM_REDIS_SENTINEL_PASSWORD="${CUBE_SANDBOX_REDIS_SENTINEL_PASSWORD:-}"
  else
    export CUBE_PROXY_REDIS_IP="${CUBE_SANDBOX_REDIS_HOST}"
    export CUBE_PROXY_REDIS_PORT="${CUBE_SANDBOX_REDIS_PORT:-6379}"
    export CUBE_PROXY_REDIS_PASSWORD="${CUBE_SANDBOX_REDIS_PASSWORD:-ceuhvu123}"
  fi
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
