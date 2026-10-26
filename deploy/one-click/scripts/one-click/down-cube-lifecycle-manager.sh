#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"
# shellcheck source=./cube-lifecycle-manager-compose-lib.sh
source "${SCRIPT_DIR}/cube-lifecycle-manager-compose-lib.sh"

require_root
require_cmd docker

CUBE_LCM_CONTAINER_NAME="${CUBE_LCM_CONTAINER_NAME:-cube-lifecycle-manager}"

if [[ -f "${CUBE_LCM_COMPOSE_FILE}" ]]; then
  cube_lcm_compose_run down --remove-orphans >/dev/null 2>&1 || true
fi

# Fallback path if the container was created out-of-band.
docker_rm_if_exists "${CUBE_LCM_CONTAINER_NAME}"

log "cube-lifecycle-manager stopped"
