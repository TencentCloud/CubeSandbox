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
"${SCRIPT_DIR}/down-dns.sh"

"${SCRIPT_DIR}/down-local.sh"

"${SCRIPT_DIR}/down-support.sh"

log "dependencies stopped"
