#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd docker

CUBE_PROXY_CONTAINER_NAME="${CUBE_PROXY_CONTAINER_NAME:-cube-proxy}"
if container_exists "${CUBE_PROXY_CONTAINER_NAME}"; then
  docker stop -t 10 "${CUBE_PROXY_CONTAINER_NAME}" >/dev/null 2>&1 || true
fi
