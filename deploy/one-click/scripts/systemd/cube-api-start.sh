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

exec "${CUBE_API_BIN}"
