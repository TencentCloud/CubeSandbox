#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

CUBE_OPS_BIND="${CUBE_OPS_BIND:-0.0.0.0:3010}"
# Strip host prefix, take port.
port="${CUBE_OPS_BIND##*:}"
health_url="http://127.0.0.1:${port}/health"

wait_for_http "${health_url}" 30 1 || die "cubeops health not ready at ${health_url}"
