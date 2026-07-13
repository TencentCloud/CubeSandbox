#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_cmd docker

REMOVE_VOLUMES="${CUBE_SANDBOX_REMOVE_VOLUMES:-0}"

"${SCRIPT_DIR}/down-webui.sh"
"${SCRIPT_DIR}/down-cube-proxy.sh"
# Stop CLM after cube-proxy: cube-proxy is the last thing that might still
# be calling /_sidecar_resume; stopping CLM first would give it 502s during
# the shutdown window.
"${SCRIPT_DIR}/down-cube-lifecycle-manager.sh"
"${SCRIPT_DIR}/down-dns.sh"

"${SCRIPT_DIR}/down-local.sh"

"${SCRIPT_DIR}/down-support.sh"

log "dependencies stopped"
