#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_cmd docker
require_cmd rg

REMOVE_VOLUMES="${CUBE_SANDBOX_REMOVE_VOLUMES:-0}"
METRIC_PID_FILE="${RUNTIME_DIR}/seed-cubemaster-metrics.pid"

if [[ -f "${METRIC_PID_FILE}" ]]; then
  metric_pid="$(<"${METRIC_PID_FILE}")"
  if [[ -n "${metric_pid}" ]] && kill -0 "${metric_pid}" >/dev/null 2>&1; then
    kill "${metric_pid}" >/dev/null 2>&1 || true
  fi
  rm -f "${METRIC_PID_FILE}"
fi

"${SCRIPT_DIR}/down-webui.sh"
"${SCRIPT_DIR}/down-cube-proxy.sh"
"${SCRIPT_DIR}/down-dns.sh"

"${SCRIPT_DIR}/down-local.sh"

"${SCRIPT_DIR}/down-support.sh"

log "dependencies stopped"
