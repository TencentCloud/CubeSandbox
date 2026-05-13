#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "${SCRIPT_DIR}/common.sh"

require_root
ensure_systemd_runtime_dirs

CUBELET_BIN="${TOOLBOX_ROOT}/Cubelet/bin/cubelet"
PID_FILE="${SYSTEMD_RUNTIME_DIR}/cubelet.pid"
PATTERN="^${CUBELET_BIN} --config"
pid=""

if [[ -f "${PID_FILE}" ]]; then
  pid="$(<"${PID_FILE}")"
  if [[ -n "${pid}" ]] && ! kill -0 "${pid}" >/dev/null 2>&1; then
    pid=""
  fi
  if [[ -n "${pid}" ]] && ! pid_matches_pattern "${pid}" "${PATTERN}"; then
    pid=""
  fi
fi

if [[ -z "${pid}" ]]; then
  pid="$(first_pid_by_pattern "${PATTERN}" || true)"
fi

if [[ -z "${pid}" ]]; then
  rm -f "${PID_FILE}"
  exit 0
fi

stop_pid_with_timeout "${pid}" 20
rm -f "${PID_FILE}"
log "cubelet stopped pid=${pid}"
