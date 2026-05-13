#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
require_cmd docker

REDIS_CONTAINER="${CUBE_SANDBOX_REDIS_CONTAINER:-cube-sandbox-redis}"
if container_exists "${REDIS_CONTAINER}"; then
  docker stop -t 10 "${REDIS_CONTAINER}" >/dev/null 2>&1 || true
fi
